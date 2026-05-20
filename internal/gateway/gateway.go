package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/agent"
	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/browser"
	"github.com/rasimio/blueship/internal/openai"
	"github.com/rasimio/blueship/internal/telegram"
	"github.com/rasimio/blueship/internal/user"
	"github.com/rasimio/blueship/session"
	"github.com/rasimio/blueship/tool"
	"github.com/rasimio/blueship/version"
)

// Gateway receives transport updates and routes them through the AgentLoop.
type Gateway struct {
	deps      *bs.Deps
	modules   ModuleRegistry
	poller    *telegram.Poller
	tg        *telegram.Client
	store     *session.Store
	provider  bs.CompletionProvider
	compactor *agent.Compactor
	whisper   *openai.TranscriptionProvider
	tz        *time.Location
	logger    *slog.Logger

	// botID and botUsername are populated at startup via telegram getMe so
	// command handlers can recognise `/reset@<bot>` targeted commands AND
	// the addressing logic can tell a reply-to-us from a reply-to-someone-else
	// in group chats with multiple participants. Zero/empty values mean
	// getMe failed and the group-chat routing degrades gracefully
	// (address check becomes nickname-only, reply-to-us check always false).
	botID       int64
	botUsername string

	systemPrompt string

	// Reflex pipeline prompts. Loaded from <Config.Prompts>/<key>.md when
	// the agent ships those files; missing files leave the default empty.
	reflexSystemPrompt   string // system prompt for reflex LLM call
	reflexPlanTemplate   string // user prompt template (has %s placeholders for rules, tools, message)
	extractInsightPrompt    string   // system prompt for insight extraction
	selfReflectionMarkers  []string // optional self_reflection_markers.md (JSON array)

	mu sync.Mutex
	users map[string]*UserState // keyed by canonical chatID ("telegram:123", "voice:owner")
}

// parseCommand extracts the bare command from a Telegram slash command,
// stripping an optional `@<botname>` suffix, and reports whether the command
// is addressed to this bot. Rules:
//   - "/reset" → (cmd="/reset", forUs=true)  — no target, everyone matches
//   - "/reset@LiyaDeusBot" with botUsername="LiyaDeusBot" → (cmd="/reset", forUs=true)
//   - "/reset@arlene_bot" with botUsername="LiyaDeusBot" → (cmd="/reset", forUs=false)
//   - "/reset foo" (args) → (cmd="/reset", forUs=true) — we strip args too
//
// If the gateway never learned its own username (getMe failed), every command
// with a non-empty suffix is treated as addressed (forUs=true) so users still
// have a working fallback.
func (g *Gateway) parseCommand(text string) (cmd string, forUs bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	// strip optional args after first space
	head := text
	if i := strings.IndexByte(text, ' '); i >= 0 {
		head = text[:i]
	}
	if i := strings.IndexByte(head, '@'); i >= 0 {
		target := strings.ToLower(head[i+1:])
		cmd = head[:i]
		if g.botUsername == "" || strings.EqualFold(target, g.botUsername) {
			return cmd, true
		}
		return cmd, false
	}
	return head, true
}

// shouldProcessGroupMessage decides whether a group-chat message is
// addressed to this bot. Private (1:1) chats always process and never
// call this function.
//
// Only two forms of addressing count:
//
//  1. Explicit "@<botUsername>" mention anywhere in the text.
//  2. Reply to one of our own previous messages.
//
// Anything else — including a reply to another user or bot, ambient chat,
// or a vocative "<name>, ..." without the @-mention — is skipped. This keeps
// the bot quiet in shared rooms unless the user actually invokes it via
// Telegram's built-in mention or reply UI.
func (g *Gateway) shouldProcessGroupMessage(msg *telegram.Message, text string) bool {
	if g.botUsername != "" && text != "" {
		if strings.Contains(strings.ToLower(text), "@"+strings.ToLower(g.botUsername)) {
			return true
		}
	}
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		rep := msg.ReplyToMessage.From
		if g.botID != 0 && rep.ID == g.botID {
			return true
		}
		if g.botUsername != "" && strings.EqualFold(rep.Username, g.botUsername) {
			return true
		}
	}
	return false
}

// UserState holds per-user runtime state.
type UserState struct {
	Mu       sync.Mutex
	ChatID   string // canonical chat ID ("telegram:123", "voice:owner")
	UserID   uuid.UUID
	SoulID   uuid.UUID // soul this chat is routed to; resolved per inbound batch
	IsOwner  bool
	Registry *bs.ToolRegistry
	Deps     *bs.Deps // per-user deps (carries ContextInjector set by modules)
	LoopBusy bool
	debounce *debouncer

	// Emotion state from last reflex prep — used for TTS instruct.
	LastStrategy string

	// PendingDisambiguation stores options from a clarification_needed reflex
	// so the next short answer ("1", "да") can be resolved to a tool call.
	PendingDisambiguation []bs.ClarificationOption

	// DebugMode appends tool traces to each response.
	DebugMode bool
}

// ModuleRegistry is an adapter interface for the module system.
type ModuleRegistry interface {
	RegisterAllTools(registry *bs.ToolRegistry, d *bs.Deps)
}

// NewGateway creates a new gateway. Telegram-specific fields (poller, tg
// client, bot identity) are only initialized when a bot_token is
// configured; otherwise the gateway runs in transport-agnostic mode and
// only serves non-Telegram sinks (WebSocket, future).
func NewGateway(deps *bs.Deps, modules ModuleRegistry, logger *slog.Logger) (*Gateway, error) {
	cfg := deps.Config

	tz, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		tz = time.UTC
	}

	coreDB, err := deps.DB("ship")
	if err != nil {
		return nil, fmt.Errorf("core DB: %w", err)
	}

	var whisperProvider *openai.TranscriptionProvider
	if cfg.Transcriber != nil {
		if wp, ok := cfg.Transcriber.(*openai.TranscriptionProvider); ok {
			whisperProvider = wp
		}
	}

	gw := &Gateway{
		deps:      deps,
		modules:   modules,
		store:     session.NewStore(coreDB),
		provider:  cfg.LLM,
		compactor: agent.NewCompactor(cfg.LLM, cfg, logger),
		whisper:   whisperProvider,
		tz:        tz,
		logger:    logger,
		users:     make(map[string]*UserState),
	}

	if cfg.Transport.BotToken != "" {
		gw.poller = telegram.NewPoller(cfg.Transport.BotToken, cfg.Timeouts.TelegramPoll)
		gw.tg = telegram.NewClient(cfg.Transport.BotToken, cfg.Timeouts.TelegramClient)

		// Fetch bot identity (id + username) so targeted commands like
		// "/reset@LiyaDeusBot" and reply-based addressing work in group chats
		// where multiple bots share the same Telegram group. Failure to resolve
		// is non-fatal — group routing just degrades to legacy "respond to
		// everything" behaviour.
		meCtx, meCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer meCancel()
		if me, err := gw.tg.GetMe(meCtx); err != nil {
			logger.Warn("telegram getMe failed; group command targeting disabled", "error", err)
		} else {
			gw.botID = me.ID
			gw.botUsername = me.Username
			logger.Info("telegram bot self", "id", me.ID, "username", me.Username)
		}
	} else {
		logger.Info("gateway initialised without telegram transport (WS-only)")
	}

	// Load system prompts from the agent's prompts directory (Config.Prompts).
	// Personality lives with the agent, never in the framework.
	if cfg.Prompts == "" {
		return nil, fmt.Errorf("system prompts not configured: set Config.Prompts to a directory containing <key>.md files")
	}
	if err := gw.loadSystemPrompts(cfg.Prompts); err != nil {
		return nil, fmt.Errorf("load system prompts: %w", err)
	}

	return gw, nil
}

// loadSystemPrompts composes the system prompt from <key>.md files in
// dir, ordered by Config.SystemPromptKeys. Optional pipeline prompts
// (compact, reflex-system, reflex-plan, extract-insight,
// self_reflection_markers) are picked up if present; missing optional
// files fall back to in-code defaults set elsewhere on the gateway.
func (g *Gateway) loadSystemPrompts(dir string) error {
	var parts []string
	for _, key := range g.deps.Config.SystemPromptKeys {
		data, err := os.ReadFile(filepath.Join(dir, key+".md"))
		if err != nil {
			return fmt.Errorf("read %s.md: %w", key, err)
		}
		parts = append(parts, string(data))
	}
	g.systemPrompt = strings.Join(parts, "\n\n")

	readOpt := func(key string) string {
		data, err := os.ReadFile(filepath.Join(dir, key+".md"))
		if err != nil {
			return ""
		}
		return string(data)
	}
	if v := readOpt("compact"); v != "" && g.compactor != nil {
		g.compactor.SetSystemPrompt(v)
	}
	if v := readOpt("reflex-system"); v != "" {
		g.reflexSystemPrompt = v
	}
	if v := readOpt("reflex-plan"); v != "" {
		g.reflexPlanTemplate = v
	}
	if v := readOpt("extract-insight"); v != "" {
		g.extractInsightPrompt = v
	}
	if raw := readOpt("self_reflection_markers"); raw != "" {
		var markers []string
		if json.Unmarshal([]byte(raw), &markers) == nil && len(markers) > 0 {
			g.selfReflectionMarkers = markers
		}
	}
	return nil
}

// systemPromptForSoul returns the persona prompt for a soul. If the soul
// has a row in vaelum.soul_personas (every platform-created soul does),
// that persona is used; otherwise the process-global prompt loaded from
// files at startup is returned — so the founding soul, which has no
// soul_personas row, behaves byte-for-byte as before.
func (g *Gateway) systemPromptForSoul(ctx context.Context, soulID uuid.UUID) string {
	if soulID == uuid.Nil {
		return g.systemPrompt
	}
	db, err := g.deps.DB("ship")
	if err != nil {
		return g.systemPrompt
	}
	var prompt string
	if err := db.GetContext(ctx, &prompt,
		`SELECT system_prompt FROM vaelum.soul_personas WHERE soul_id = $1`,
		soulID); err != nil || prompt == "" {
		return g.systemPrompt
	}
	return prompt
}

// tryTelegramPairing checks whether a message is a Vaelum pairing code.
// If so it links this Telegram account to the platform user behind the
// code and returns true so the caller stops processing the message.
// Intercepted before user resolution — the sender is not yet a known user.
func (g *Gateway) tryTelegramPairing(ctx context.Context, chatID, text string) bool {
	code := strings.TrimSpace(text)
	if !strings.HasPrefix(code, "pair-") {
		return false
	}
	db, err := g.deps.DB("ship")
	if err != nil {
		return false
	}
	reply := g.newTelegramSink(chatID)

	var pairingID, userID uuid.UUID
	err = db.QueryRowContext(ctx,
		`SELECT id, user_id FROM vaelum.telegram_pairings
		 WHERE code = $1 AND consumed_at IS NULL AND expires_at > now()`,
		code).Scan(&pairingID, &userID)
	if err != nil {
		_ = reply.SendText(ctx, "That pairing code is invalid or expired — generate a fresh one in Vaelum settings.")
		return true
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO vaelum.user_identities (user_id, kind, value, verified_at)
		 VALUES ($1, 'telegram', $2, now())
		 ON CONFLICT (kind, value) DO NOTHING`,
		userID, chatID); err != nil {
		g.logger.Error("telegram pairing: link failed", "error", err)
		_ = reply.SendText(ctx, "Something went wrong linking your account — try again shortly.")
		return true
	}
	_, _ = db.ExecContext(ctx,
		`UPDATE vaelum.telegram_pairings SET consumed_at = now() WHERE id = $1`, pairingID)

	g.logger.Info("telegram pairing: linked", "chat_id", chatID, "user_id", userID.String())
	_ = reply.SendText(ctx, "Your Telegram is now linked to your Vaelum account.")
	return true
}

// Run starts the polling loop and processes updates. Blocks until ctx is done.
func (g *Gateway) Run(ctx context.Context) {
	ch := make(chan telegram.Update, 100)

	go g.poller.Run(ctx, ch)
	g.logger.Info("telegram gateway started")

	for {
		select {
		case <-ctx.Done():
			return
		case update := <-ch:
			g.handleUpdate(ctx, update)
		}
	}
}

func (g *Gateway) handleUpdate(ctx context.Context, update telegram.Update) {
	// Handle callback queries (inline button presses)
	if cq := update.CallbackQuery; cq != nil {
		g.tg.AnswerCallbackQuery(ctx, cq.ID)
		if g.handleModelCallback(ctx, cq) {
			return
		}
		return
	}

	msg := update.Message
	if msg == nil || msg.From == nil {
		return
	}

	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	if msg.Document != nil && isTextFile(msg.Document) {
		content, err := g.tg.DownloadFile(ctx, msg.Document.FileID, 512*1024)
		if err != nil {
			g.logger.Warn("failed to download file", "error", err, "file", msg.Document.FileName)
		} else {
			fileText := fmt.Sprintf("[file: %s]\n```\n%s\n```", msg.Document.FileName, string(content))
			if text != "" {
				text = text + "\n\n" + fileText
			} else {
				text = fileText
			}
		}
	}

	// PDF attachments — extract text via the same pure-Go decoder
	// browser_fetch uses for web-found PDFs. The file lands in the
	// turn as `[pdf: filename.pdf]\n--- Page 1 ---\n…` so cortex can
	// reason over the contents and cite by page. Limit matches
	// browser.PDFMaxBytes so a giant scan can't blow memory.
	if msg.Document != nil && isPDFDocument(msg.Document) {
		content, err := g.tg.DownloadFile(ctx, msg.Document.FileID, browser.PDFMaxBytes)
		if err != nil {
			g.logger.Warn("failed to download pdf", "error", err, "file", msg.Document.FileName)
		} else if pdfText, pages, err := browser.ExtractPDFText(content); err != nil {
			g.logger.Warn("failed to extract pdf text", "error", err, "file", msg.Document.FileName, "size", len(content))
			// Surface the failure to cortex so it can ask the user for a
			// different file rather than silently "reading" nothing.
			fileText := fmt.Sprintf("[pdf: %s — extraction failed: %v]", msg.Document.FileName, err)
			if text != "" {
				text = text + "\n\n" + fileText
			} else {
				text = fileText
			}
		} else {
			fileText := fmt.Sprintf("[pdf: %s — %d pages]%s", msg.Document.FileName, pages, pdfText)
			if text != "" {
				text = text + "\n\n" + fileText
			} else {
				text = fileText
			}
		}
	}

	if msg.Voice != nil && g.whisper != nil && g.whisper.IsConfigured() {
		audio, err := g.tg.DownloadFile(ctx, msg.Voice.FileID, 10*1024*1024)
		if err != nil {
			g.logger.Warn("failed to download voice", "error", err)
		} else {
			transcript, err := g.whisper.Transcribe(ctx, audio, "voice.ogg")
			if err != nil {
				g.logger.Warn("failed to transcribe voice", "error", err)
			} else if transcript != "" {
				if text != "" {
					text = text + "\n\n" + transcript
				} else {
					text = transcript
				}
			}
		}
	}

	var images []bs.ContentBlock
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1] // largest resolution
		data, err := g.tg.DownloadFile(ctx, photo.FileID, 5*1024*1024)
		if err != nil {
			g.logger.Warn("failed to download photo", "error", err, "file_id", photo.FileID)
		} else {
			images = append(images, bs.ContentBlock{
				Type: "image",
				Source: &bs.ImageSource{
					Type:      "base64",
					MediaType: "image/jpeg",
					Data:      base64.StdEncoding.EncodeToString(data),
				},
			})
		}
	}

	// Prepend quoted reply context so the model sees what message the user is replying to
	if msg.ReplyToMessage != nil {
		quoted := msg.ReplyToMessage.Text
		if quoted == "" {
			quoted = msg.ReplyToMessage.Caption
		}
		if quoted != "" {
			// Truncate very long quoted messages to keep context manageable
			if len(quoted) > 500 {
				quoted = quoted[:500] + "..."
			}
			text = fmt.Sprintf("[reply to: %s]\n\n%s", quoted, text)
		} else {
			g.logger.Warn("reply-to message has no text/caption",
				"reply_msg_id", msg.ReplyToMessage.MessageID,
				"has_document", msg.ReplyToMessage.Document != nil,
			)
		}
	}

	if text == "" && len(images) == 0 {
		return
	}

	rawChatID := msg.Chat.ID
	chatID := tgCanonical(rawChatID)

	// Vaelum pairing — a "pair-…" message links this Telegram account to a
	// platform user. Handled before user resolution rejects the sender.
	if g.tryTelegramPairing(ctx, chatID, text) {
		return
	}

	// Group-chat routing: in a chat with more than one participant the bot
	// only reacts to messages that are explicitly addressed to it. Private
	// (1:1) chats bypass this filter because the human has nobody else to
	// talk to. Slash commands are handled below via parseCommand regardless
	// of this filter — commands are their own addressing mechanism.
	if msg.Chat.Type != "private" && !strings.HasPrefix(text, "/") {
		if !g.shouldProcessGroupMessage(msg, text) {
			g.logger.Debug("gateway: group message not addressed, skipping",
				"chat_id", chatID,
				"chat_type", msg.Chat.Type,
			)
			return
		}
	}

	if cmd, forUs := g.parseCommand(text); cmd != "" {
		if !forUs {
			return
		}
		switch cmd {
		case "/session":
			go g.handleSessionCommand(ctx, rawChatID)
			return
		case "/reset":
			go g.handleResetCommand(ctx, rawChatID)
			return
		case "/model":
			go g.handleModelCommand(ctx, rawChatID)
			return
		case "/voice":
			go g.handleVoiceCommand(ctx, rawChatID)
			return
		}
	}
	// A2A trace messages are informational broadcasts posted by bots into
	// a shared visibility chat (e.g. rasim lab). They are never addressed
	// to anyone — the [a2a-trace] sentinel is the cue. Gateways MUST drop
	// them before any cortex turn is triggered, otherwise bots would react
	// to each other's status lines and spin a feedback loop.
	if strings.HasPrefix(strings.TrimLeft(text, " \n\t\r"), "[a2a-trace]") {
		g.logger.Debug("gateway: a2a trace message, visibility only — skipping cortex turn",
			"chat_id", chatID)
		return
	}

	if text == "/debug" {
		us, err := g.getOrInitUser(ctx, chatID)
		if err == nil {
			us.Mu.Lock()
			us.DebugMode = !us.DebugMode
			mode := "OFF"
			if us.DebugMode {
				mode = "ON"
			}
			us.Mu.Unlock()
			g.tg.SendMessage(ctx, fmt.Sprintf("%d", rawChatID), fmt.Sprintf("Debug mode: %s", mode))
		}
		return
	}

	us, err := g.getOrInitUser(ctx, chatID)
	if err != nil {
		g.logger.Debug("ignored message", "chat_id", chatID, "error", err)
		return
	}

	us.debounce.Add(pendingMsg{
		text:      text,
		images:    images,
		messageID: msg.MessageID,
	})
}

// isPDFDocument identifies Telegram document attachments that should
// be routed through the PDF text extractor. Trusts mime type when set
// to application/pdf; falls back to filename extension because Telegram
// clients don't always populate mime_type for forwarded files.
func isPDFDocument(doc *telegram.Document) bool {
	if doc == nil || doc.FileID == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(doc.MimeType), "application/pdf") {
		return true
	}
	if strings.HasSuffix(strings.ToLower(doc.FileName), ".pdf") {
		return true
	}
	return false
}

func isTextFile(doc *telegram.Document) bool {
	if doc == nil || doc.FileID == "" {
		return false
	}
	mime := doc.MimeType
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/javascript",
		"application/x-yaml", "application/yaml", "application/toml",
		"application/x-sh", "application/sql", "application/csv":
		return true
	}
	name := strings.ToLower(doc.FileName)
	for _, ext := range []string{
		".txt", ".md", ".json", ".yaml", ".yml", ".toml", ".xml",
		".csv", ".sql", ".sh", ".py", ".go", ".js", ".ts", ".html",
		".css", ".log", ".env", ".cfg", ".conf", ".ini", ".properties",
	} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

func (g *Gateway) getOrInitUser(ctx context.Context, chatID string) (*UserState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Fast path: exact chatID match.
	if us, ok := g.users[chatID]; ok {
		return us, nil
	}

	coreDB, err := g.deps.DB("ship")
	if err != nil {
		return nil, fmt.Errorf("core DB: %w", err)
	}

	// Resolve userID from chatID. Unknown users are rejected — no fallback to
	// owner, which would let any stranger chat as the owner.
	userID, err := user.ResolveByChatID(ctx, coreDB, chatID)
	if err != nil {
		g.logger.Info("rejected unknown chat_id", "chat_id", chatID, "error", err)
		return nil, fmt.Errorf("unknown chat_id %s: not in user_profiles", chatID)
	}

	var isOwner bool
	if err := coreDB.GetContext(ctx, &isOwner,
		`SELECT is_owner FROM user_profiles WHERE id = $1`, userID.String()); err != nil {
		g.logger.Warn("is_owner lookup failed, defaulting to false", "user_id", userID, "error", err)
	}

	if !isOwner {
		g.logger.Info("rejected non-owner message", "chat_id", chatID, "user_id", userID.String())
		return nil, fmt.Errorf("non-owner user rejected")
	}

	// Resolve the soul this user routes to. Done here (not in
	// ProcessInbound) because the Telegram path enters via handleUpdate
	// → debouncer and never touches ProcessInbound — getOrInitUser is
	// the one boundary both transports share.
	var soulID uuid.UUID
	if g.deps.ResolveSoul != nil {
		soulID, err = g.deps.ResolveSoul(ctx, userID)
		if err != nil {
			g.logger.Error("gateway: soul resolution failed",
				"chat_id", chatID, "user_id", userID.String(), "error", err)
			return nil, fmt.Errorf("resolve soul: %w", err)
		}
	}

	userDeps := g.deps.ForUser(userID, chatID, isOwner)
	registry := bs.NewToolRegistry()
	tool.RegisterBuiltinTools(registry, userDeps)
	if err := tool.RegisterBrowserTools(registry, userDeps); err != nil {
		g.logger.Warn("gateway: register browser tools failed", "error", err)
	}
	if err := tool.RegisterAgentTaskTools(registry, userDeps); err != nil {
		g.logger.Warn("gateway: register agent_task tools failed", "error", err)
	}
	g.modules.RegisterAllTools(registry, userDeps)

	us := &UserState{
		ChatID:   chatID,
		UserID:   userID,
		SoulID:   soulID,
		IsOwner:  isOwner,
		Registry: registry,
		Deps:     userDeps,
	}

	us.debounce = newDebouncer(g.deps.Config.Gateway.DebounceWindow, g.deps.Config.Gateway.DebounceCap, func(msgs []pendingMsg) {
		sink := g.newTelegramSink(chatID)
		go g.processMessages(ctx, us, msgs, sink)
	})

	g.users[chatID] = us
	g.logger.Info("initialized user",
		"chat_id", chatID,
		"user_id", userID.String(),
		"is_owner", isOwner,
	)

	return us, nil
}

// GetUser returns an existing user state. Returns nil if not initialized.
func (g *Gateway) GetUser(chatID string) *UserState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.users[chatID]
}

// GetOwnerUser returns the owner's UserState, or nil if not yet initialized.
func (g *Gateway) GetOwnerUser() *UserState {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, us := range g.users {
		if us.IsOwner {
			return us
		}
	}
	return nil
}

// formatRulesAsGuidance renders a slice of ActiveRule entries into the
// "WHEN: ... DO: ... TOOLS: ..." shape the cortex prompt already understands.
// Used by the no-reflex rule-engine path so agents without a reflex pipeline
// still get guidance injection.
func formatRulesAsGuidance(rules []bs.ActiveRule) string {
	if len(rules) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Active rules\n")
	for _, r := range rules {
		if r.Trigger != "" {
			fmt.Fprintf(&b, "WHEN: %s\n", r.Trigger)
		}
		if r.Action != "" {
			fmt.Fprintf(&b, "DO: %s\n", r.Action)
		}
		if len(r.Tools) > 0 {
			fmt.Fprintf(&b, "TOOLS: %s\n", strings.Join(r.Tools, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// sendDebugDump builds a full debug dump and sends as txt file via Telegram.
// No-op for non-Telegram transports (WS-only, etc.) — Telegram is the only
// sink that supports document attachments today.
func (g *Gateway) sendDebugDump(ctx context.Context, us *UserState, injectedCtx, reflexGuidance, msgText string, preTraces, cortexTraces []agent.ToolTrace, engineRuleCount int) {
	if g.tg == nil || !g.tg.IsConfigured() {
		return
	}
	var b strings.Builder
	b.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	b.WriteString("=== DEBUG DUMP ===\n")
	b.WriteString(fmt.Sprintf("Time: %s\n", time.Now().In(g.tz).Format("2006-01-02 15:04:05")))
	b.WriteString(fmt.Sprintf("User: %s\n\n", us.ChatID))

	// Full Cortex SYSTEM PROMPT — the complete plain text that lands in
	// the LLM's `system` channel before any conversation history. Includes
	// PREAMBLE / SOUL / AGENTS / behavioral rules that the model loaded
	// from .md files. Shown verbatim (no truncation) so we can answer
	// "what does the model actually see?".
	b.WriteString("=== SYSTEM PROMPT (full plain text) ===\n")
	if g.systemPrompt != "" {
		b.WriteString(g.systemPrompt)
	} else {
		b.WriteString("(empty)")
	}
	b.WriteString("\n\n")

	// AME Traces (injected context)
	b.WriteString("=== AME TRACES (injected context) ===\n")
	if injectedCtx != "" {
		b.WriteString(injectedCtx)
	} else {
		b.WriteString("(empty)")
	}
	b.WriteString("\n\n")

	// Reflex Guidance (matched rules)
	b.WriteString("=== REFLEX GUIDANCE (active rules) ===\n")
	if reflexGuidance != "" {
		b.WriteString(reflexGuidance)
	} else {
		b.WriteString("(no rules matched)")
	}
	b.WriteString("\n\n")

	// Rule Engine
	b.WriteString(fmt.Sprintf("=== RULE ENGINE ===\n%d rules matched by structured conditions\n\n", engineRuleCount))

	// Cortex tool definitions (grouped by source)
	b.WriteString("=== CORTEX TOOLS ===\n")
	if us.Registry != nil && g.deps.RoleTools != nil {
		names := g.deps.RoleTools.Get("cortex")
		defs := us.Registry.DefinitionsForNames(names)
		local := make([]bs.ToolDefinition, 0)
		peerDefs := make(map[string][]bs.ToolDefinition)
		for _, d := range defs {
			peer := us.Registry.PeerForTool(d.Name)
			if peer == "" {
				local = append(local, d)
			} else {
				peerDefs[peer] = append(peerDefs[peer], d)
			}
		}
		if len(local) > 0 {
			b.WriteString("[local]\n")
			for _, d := range local {
				desc := strings.TrimSpace(d.Description)
				if len(desc) > 120 {
					desc = desc[:120] + "..."
				}
				fmt.Fprintf(&b, "  %s: %s\n", d.Name, desc)
			}
		}
		for peer, pd := range peerDefs {
			fmt.Fprintf(&b, "[%s]\n", peer)
			for _, d := range pd {
				desc := strings.TrimSpace(d.Description)
				if len(desc) > 120 {
					desc = desc[:120] + "..."
				}
				fmt.Fprintf(&b, "  %s: %s\n", d.Name, desc)
			}
		}
		fmt.Fprintf(&b, "(%d tools)\n", len(defs))
	}
	b.WriteString("\n")

	// Tool traces
	b.WriteString("=== TOOL CALLS ===\n")
	allTraces := append(preTraces, cortexTraces...)
	if len(allTraces) == 0 {
		b.WriteString("(no tools called)\n")
	}
	for i, t := range allTraces {
		src := "cortex"
		if i < len(preTraces) {
			src = "reflex"
		}
		errMark := ""
		if t.Error {
			errMark = " [ERROR]"
		}
		fmt.Fprintf(&b, "[%s] %s(%s)%s\n", src, t.Name, t.Input, errMark)
		if t.Output != "" {
			fmt.Fprintf(&b, "  → %s\n", t.Output)
		}
	}

	// Final layout: how Cortex actually sees the input. The gateway
	// composes prompt = (system) g.systemPrompt | (messages) chat history
	// where the LAST user message gets the [context]<reflexGuidance>\n\n<injectedCtx>[/context]
	// prefix glued to the actual user text. AME traces are NOT in the
	// system channel — they ride in as a prefix to the user message.
	// This is the single most useful section if you want to know "what
	// does the model see and where".
	b.WriteString("=== FINAL CORTEX INPUT LAYOUT ===\n")
	b.WriteString("system:\n")
	b.WriteString("  [current_datetime: ...] + <SYSTEM PROMPT shown above>\n\n")
	b.WriteString("messages: ...прошлая история чата...\n\n")
	b.WriteString("LAST user message (что Cortex реально видит как user input в этот turn):\n")
	b.WriteString("---BEGIN---\n")
	combinedCtx := ""
	if reflexGuidance != "" && injectedCtx != "" {
		combinedCtx = reflexGuidance + "\n\n" + injectedCtx
	} else if reflexGuidance != "" {
		combinedCtx = reflexGuidance
	} else {
		combinedCtx = injectedCtx
	}
	if combinedCtx != "" {
		b.WriteString("[context]\n")
		b.WriteString(combinedCtx)
		if !strings.HasSuffix(combinedCtx, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("[/context]\n\n")
	}
	if msgText != "" {
		b.WriteString(msgText)
	} else {
		b.WriteString("(empty)")
	}
	b.WriteString("\n---END---\n\n")

	// Send as file
	chatID := us.ChatID
	if idx := strings.Index(chatID, ":"); idx >= 0 {
		chatID = chatID[idx+1:]
	}
	if err := g.tg.SendDocument(ctx, chatID, "debug.md", []byte(b.String())); err != nil {
		g.logger.Warn("debug dump send failed", "error", err)
	}
}

// sendDebugError sends the actual error via sink when debug mode is on.
func (g *Gateway) sendDebugError(ctx context.Context, sink bs.ResponseSink, source string, err error) {
	if g.deps.Config.Gateway.Debug {
		msg := fmt.Sprintf("[%s] %v", source, err)
		sink.SendText(ctx, msg)
	} else {
		sink.SendText(ctx, "Sorry, something went wrong internally.")
	}
}

// notifyOwnerError sends an error to the owner's DM (for background jobs).
// Only sends when debug mode is on. Does nothing if owner is not initialized.
func (g *Gateway) notifyOwnerError(ctx context.Context, source string, err error) {
	if !g.deps.Config.Gateway.Debug {
		return
	}
	owner := g.GetOwnerUser()
	if owner == nil {
		return
	}
	msg := fmt.Sprintf("[%s] %v", source, err)
	sink := g.newTelegramSink(owner.ChatID)
	sink.SendText(ctx, msg)
}

// ProcessInbound is the public entry point for external transports (WebSocket, etc.).
// Resolves user, converts InboundMessage to internal format, and runs the full pipeline.
func (g *Gateway) ProcessInbound(ctx context.Context, chatID string, messages []bs.InboundMessage, sink bs.ResponseSink) error {
	us, err := g.getOrInitUser(ctx, chatID)
	if err != nil {
		return fmt.Errorf("resolve user: %w", err)
	}
	// Soul is resolved + stashed on us inside getOrInitUser; processMessages
	// re-attaches it to ctx. Nothing to do here.

	// Transcribe audio if present.
	for i, m := range messages {
		if len(m.Audio) > 0 && g.whisper != nil && g.whisper.IsConfigured() {
			transcript, err := g.whisper.Transcribe(ctx, m.Audio, "voice.wav")
			if err != nil {
				g.logger.Warn("transcribe failed", "error", err)
				continue
			}
			if messages[i].Text != "" {
				messages[i].Text += "\n\n" + transcript
			} else {
				messages[i].Text = transcript
			}
		}
	}

	var pending []pendingMsg
	for _, m := range messages {
		if m.Text == "" && len(m.Images) == 0 {
			continue
		}
		pending = append(pending, pendingMsg{
			text:   m.Text,
			images: m.Images,
		})
	}
	if len(pending) == 0 {
		return nil
	}

	g.processMessages(ctx, us, pending, sink)
	return nil
}

// ProcessInboundForUser is the entry point for authenticated platform
// (Vaelum) web users. The caller has already authenticated the user and
// resolved the soul, so this bypasses chatID resolution and the
// owner-only gate that getOrInitUser enforces for the Telegram path.
func (g *Gateway) ProcessInboundForUser(ctx context.Context, userID, soulID uuid.UUID, messages []bs.InboundMessage, sink bs.ResponseSink) error {
	us, err := g.getOrInitWebUser(ctx, userID, soulID)
	if err != nil {
		return fmt.Errorf("init web user: %w", err)
	}

	var pending []pendingMsg
	for _, m := range messages {
		if m.Text == "" && len(m.Images) == 0 {
			continue
		}
		pending = append(pending, pendingMsg{text: m.Text, images: m.Images})
	}
	if len(pending) == 0 {
		return nil
	}

	g.processMessages(ctx, us, pending, sink)
	return nil
}

// getOrInitWebUser builds (or reuses) a UserState for an authenticated
// platform web user. Unlike getOrInitUser it does not consult
// user_profiles and does not apply the owner gate — Vaelum is the
// authentication boundary for web users, and the soul is supplied by the
// caller (resolved from vaelum.memberships on the platform side).
func (g *Gateway) getOrInitWebUser(ctx context.Context, userID, soulID uuid.UUID) (*UserState, error) {
	chatID := "vaelum:" + userID.String()

	g.mu.Lock()
	defer g.mu.Unlock()

	if us, ok := g.users[chatID]; ok {
		return us, nil
	}

	userDeps := g.deps.ForUser(userID, chatID, false)
	registry := bs.NewToolRegistry()
	tool.RegisterBuiltinTools(registry, userDeps)
	if err := tool.RegisterBrowserTools(registry, userDeps); err != nil {
		g.logger.Warn("gateway: register browser tools failed", "error", err)
	}
	if err := tool.RegisterAgentTaskTools(registry, userDeps); err != nil {
		g.logger.Warn("gateway: register agent_task tools failed", "error", err)
	}
	g.modules.RegisterAllTools(registry, userDeps)

	us := &UserState{
		ChatID:   chatID,
		UserID:   userID,
		SoulID:   soulID,
		IsOwner:  false,
		Registry: registry,
		Deps:     userDeps,
	}
	g.users[chatID] = us
	g.logger.Info("initialized web user",
		"chat_id", chatID,
		"user_id", userID.String(),
		"soul_id", soulID.String(),
	)
	return us, nil
}

func (g *Gateway) processMessages(ctx context.Context, us *UserState, msgs []pendingMsg, sink bs.ResponseSink) {
	us.Mu.Lock()
	defer us.Mu.Unlock()
	us.LoopBusy = true
	defer func() { us.LoopBusy = false }()

	// Stash the originating chat id on the context so tool handlers can
	// surface it (e.g. coderun stores it on the task row to notify the
	// requester directly when status changes, instead of broadcasting
	// through fleet peers).
	ctx = bs.ContextWithChatID(ctx, us.ChatID)

	// Re-attach the soul. The Telegram path reaches here via a debouncer
	// goroutine whose ctx was captured at debouncer-creation time —
	// before any per-turn tagging — so the soul is sourced from
	// UserState (set in getOrInitUser), not from the inbound ctx.
	ctx = bs.WithSoulID(ctx, us.SoulID)

	typingCtx, stopTyping := context.WithCancel(ctx)
	go g.keepTypingViaSink(typingCtx, sink)
	defer stopTyping()

	var blocks []bs.ContentBlock
	for _, m := range msgs {
		blocks = append(blocks, m.images...)
		if m.text != "" {
			blocks = append(blocks, bs.ContentBlock{Type: "text", Text: m.text})
		}
	}

	var content any
	var msgText string
	if len(blocks) == 1 && blocks[0].Type == "text" {
		msgText = blocks[0].Text
		content = msgText
	} else {
		content = blocks
		// Extract text for memory encoding
		for _, b := range blocks {
			if b.Type == "text" {
				msgText = b.Text
				break
			}
		}
	}

	// Memory encoding: Recall → Compare → React (non-blocking). Detaches
	// from the request ctx but re-carries the soul so its writes stay
	// tenant-attributed.
	if msgText != "" && g.deps.MessageEncoder != nil {
		go g.deps.MessageEncoder(bs.WithSoulID(context.Background(), us.SoulID), us.UserID.String(), msgText)
	}

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Error("session error", "error", err)
		g.sendDebugError(ctx, sink, "session", err)
		return
	}

	g.logger.Info("processing message",
		"chat_id", us.ChatID,
		"session_id", sess.ID,
		"messages", len(msgs),
		"blocks", len(blocks),
	)

	// Collect message text for context injection (msgText already set above for single-block).
	for _, m := range msgs {
		if m.text != "" {
			if msgText != "" {
				msgText += " "
			}
			msgText += m.text
		}
	}

	// Pull a few prior chat turns so AME can embed the multi-turn theme,
	// not just the (often short, vague) current message. Without this,
	// "как ты" right after a heavy disclosure has no signal — cosine
	// search lands on whatever generic emotional record matches "как
	// ты" best, and the model anchors on the wrong event.
	priorContext := g.buildPriorContext(ctx, sess.ID, 6)

	// Build context and run reflex/cortex pipeline.
	var injectedCtx, reflexGuidance string
	var postActions []bs.PostAction // executed after cortex response
	var engineRuleCount int

	// Rule engine pass for agents that run WITHOUT a ReflexPreparer.
	// Two responsibilities:
	//   1. Abort the turn immediately if any silent rule matched.
	//   2. Format non-silent rules as guidance and surface them into the
	//      cortex prompt via reflexGuidance + debug dump.
	// No-op when a ReflexPreparer is wired: runReflexPipeline does the same
	// rule engine pass inline and owns both variables.
	if msgText != "" && us.Deps != nil && us.Deps.RuleEngine != nil && us.Deps.ReflexPreparer == nil {
		engineRules := us.Deps.RuleEngine(ctx, bs.RuleContext{
			UserID:  us.UserID.String(),
			Hour:    time.Now().Hour(),
			Message: msgText,
		})
		for _, r := range engineRules {
			if r.Silent {
				g.logger.Info("rule engine: silent rule matched (no-reflex path), aborting turn",
					"rule_id", r.ID,
					"trigger", r.Trigger,
					"chat_id", us.ChatID,
				)
				return
			}
		}
		engineRuleCount = len(engineRules)
		if engineRuleCount > 0 {
			reflexGuidance = formatRulesAsGuidance(engineRules)
			g.logger.Info("rule engine: non-silent rules matched (no-reflex path)",
				"count", len(engineRules),
				"chat_id", us.ChatID,
			)
		}
	}

	var preTraces []agent.ToolTrace

	// Resolve pending disambiguation: if previous turn asked "which option?"
	// and this message is a short answer, inject the chosen tool directly.
	if len(us.PendingDisambiguation) > 0 && msgText != "" {
		if chosen := resolveDisambiguation(msgText, us.PendingDisambiguation); chosen != nil {
			reflexGuidance = fmt.Sprintf("[DISAMBIGUATION RESOLVED]\nПользователь выбрал: %s\nВызови %s.\n", chosen.Label, chosen.Tool)
			us.PendingDisambiguation = nil
			g.logger.Info("disambiguation: resolved", "tool", chosen.Tool, "label", chosen.Label)
			// Still run context injection for AME traces.
			if us.Deps != nil && us.Deps.ContextInjector != nil {
				injectedCtx = us.Deps.ContextInjector(ctx, us.UserID.String(), msgText, priorContext)
			}
		} else {
			// Not a resolution — clear pending and proceed normally.
			us.PendingDisambiguation = nil
		}
	}

	if reflexGuidance == "" && msgText != "" && us.Deps != nil && us.Deps.ReflexPreparer != nil && g.reflexModel() != "" {
		// Reflex/Cortex pipeline: structured context → reflex plan → pre-actions → filtered cortex input.
		var silent bool
		injectedCtx, reflexGuidance, postActions, preTraces, engineRuleCount, silent = g.runReflexPipeline(ctx, us, msgText, priorContext)
		if silent {
			// Hard rule said "do not respond". Abort the whole turn — no
			// cortex call, no message sent, no post-actions, no debug dump.
			return
		}
	} else if msgText != "" && us.Deps != nil && us.Deps.ContextInjector != nil {
		// Fallback: legacy ContextInjector (no reflex).
		injectedCtx = us.Deps.ContextInjector(ctx, us.UserID.String(), msgText, priorContext)
	}

	loop := agent.NewLoop(g.provider, g.store, us.Registry, g.deps.RoleTools, g.deps.Config, g.logger)
	loop.SetCompactor(g.compactor)

	// Inject current datetime into the per-soul system prompt so the model
	// always knows "today". The persona is resolved per soul_id; the
	// founding soul falls back to the file-loaded process-global prompt.
	now := time.Now().In(g.tz)
	systemWithTime := fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"),
		g.systemPromptForSoul(ctx, us.SoulID))

	var cortexTemp float64
	if g.deps.ModelStore != nil {
		cortexTemp = g.deps.ModelStore.Get("cortex").Temperature
	}

	runCfg := agent.RunConfig{
		SessionID:       sess.ID,
		SystemPrompt:    systemWithTime,
		CompactSummary:  derefString(sess.CompactSummary),
		Model:           g.cortexModel(),
		MaxTokens:       g.deps.Config.Limits.MaxOutputTokens,
		MaxTurns:        g.deps.Config.Gateway.MaxTurns,
		InjectedContext: injectedCtx,
		ReflexGuidance:  reflexGuidance,
		Role:            "cortex",
		Temperature:     cortexTemp,
	}

	// Voice transport: use streaming LLM with inline sentence-level TTS.
	// Each sentence is TTS'd and sent as an audio chunk as soon as the LLM produces it.
	streamSink, isStreaming := sink.(bs.StreamingVoiceSink)
	if isStreaming && g.deps.Config.TTS != nil {
		var sentenceBuf strings.Builder
		chunkSeq := 0

		cfg := g.deps.Config
		voice := cfg.TTSVoice
		var instruct string
		if cfg.TTSInstructMapper != nil {
			instruct = cfg.TTSInstructMapper(us.LastStrategy)
		}
		mp3Provider, hasMP3 := cfg.TTS.(bs.TTSProviderMP3)
		synthesize := cfg.TTS.Synthesize
		if hasMP3 {
			synthesize = mp3Provider.SynthesizeMP3
		}

		onText := func(chunk string) {
			if cfg.TTSTextCleaner != nil {
				chunk = cfg.TTSTextCleaner(chunk)
			}
			sentenceBuf.WriteString(chunk)
			text := sentenceBuf.String()

			// Check for sentence boundary
			for _, delim := range []string{". ", "! ", "? ", ".\n", "!\n", "?\n"} {
				if idx := strings.LastIndex(text, delim); idx >= 10 {
					sentence := strings.TrimSpace(text[:idx+1])
					sentenceBuf.Reset()
					sentenceBuf.WriteString(text[idx+len(delim):])

					chunkSeq++
					seq := chunkSeq
					audio, err := synthesize(ctx, sentence, voice, instruct)
					if err != nil {
						g.logger.Warn("tts: stream chunk failed", "error", err)
						return
					}
					streamSink.SendVoiceChunk(ctx, audio, seq, false)
					return
				}
			}
		}

		reply, _, err := loop.RunStream(ctx, runCfg, content, onText, nil)
		if err != nil {
			g.logger.Error("agent loop error", "chat_id", us.ChatID, "error", err)
			g.sendDebugError(ctx, sink, "agent", err)
			return
		}

		// Flush remaining text as final chunk
		if remaining := strings.TrimSpace(sentenceBuf.String()); remaining != "" {
			chunkSeq++
			if audio, err := synthesize(ctx, remaining, voice, instruct); err == nil {
				streamSink.SendVoiceChunk(ctx, audio, chunkSeq, true)
			}
		} else if chunkSeq > 0 {
			// Mark last sent chunk as final (re-send empty final marker)
			streamSink.SendVoiceChunk(ctx, nil, chunkSeq, true)
		}

		// Also send text for logging
		if reply != "" {
			sink.SendText(ctx, reply)
		}
		if reply != "" && len(postActions) > 0 {
			g.executePostActions(ctx, us, postActions, reply)
		}
		if reply != "" {
			g.emitTurnCompleted(us, sess)
		}
		return
	}

	// Streaming path for Telegram: send placeholder, edit as chunks arrive.
	tgSink, isTelegram := sink.(*telegramSink)
	if !isTelegram {
		// Non-Telegram transports: batch mode (unchanged).
		result, err := loop.RunTracked(ctx, runCfg, content)
		if err != nil {
			g.logger.Error("agent loop error", "chat_id", us.ChatID, "error", err)
			g.sendDebugError(ctx, sink, "agent", err)
			return
		}
		reply := sanitizeLeakedToolCalls(result.Text)
		if reply != "" {
			if len(postActions) > 0 {
				g.executePostActions(ctx, us, postActions, reply)
			}
			sink.SendText(ctx, reply)
			g.emitTurnCompleted(us, sess)
		}
		if us.DebugMode || g.deps.Config.Gateway.Debug {
			go g.sendDebugDump(ctx, us, injectedCtx, reflexGuidance, msgText, preTraces, result.ToolTraces, engineRuleCount)
		}
		return
	}

	// Telegram streaming: progressive message editing.
	var (
		streamMsgID int         // Telegram message ID for edits
		streamBuf   strings.Builder
		lastEdit    time.Time
		toolStatus  string      // current tool being executed
		mu          sync.Mutex
	)

	const editInterval = 600 * time.Millisecond

	flushEdit := func() {
		mu.Lock()
		defer mu.Unlock()
		if streamMsgID == 0 {
			return
		}
		text := strings.TrimSpace(streamBuf.String())
		if toolStatus != "" {
			text += "\n\n`" + toolStatus + "`"
		}
		if text == "" {
			return
		}
		if time.Since(lastEdit) < editInterval {
			return
		}
		g.tg.EditMessageText(ctx, tgSink.chatID, streamMsgID, text, nil)
		lastEdit = time.Now()
	}

	// ensureMsg creates the Telegram message on first content (text or tool).
	ensureMsg := func(text string) {
		if streamMsgID != 0 {
			return
		}
		res, err := g.tg.SendMessage(ctx, fmt.Sprintf("%d", tgSink.chatID), text)
		if err == nil && res != nil && res.Result.MessageID != 0 {
			streamMsgID = res.Result.MessageID
			lastEdit = time.Now()
		}
	}

	onText := func(chunk string) {
		mu.Lock()
		streamBuf.WriteString(chunk)
		text := strings.TrimSpace(streamBuf.String())
		mu.Unlock()

		if streamMsgID == 0 && len(text) > 10 {
			// First meaningful chunk — create message
			ensureMsg(text)
		} else {
			flushEdit()
		}
	}

	onToolUse := func(name string) {
		mu.Lock()
		toolStatus = ">> " + name + "..."
		mu.Unlock()
		ensureMsg("`>> " + name + "...`")
		flushEdit()
	}

	reply, cortexTraces, err := loop.RunStream(ctx, runCfg, content, onText, onToolUse)
	if err != nil {
		g.logger.Error("agent loop error", "chat_id", us.ChatID, "error", err)
		g.sendDebugError(ctx, sink, "agent", err)
		return
	}

	reply = sanitizeLeakedToolCalls(reply)
	if reply == "" {
		return
	}

	// Clear tool status for final message
	mu.Lock()
	toolStatus = ""
	mu.Unlock()

	if streamMsgID != 0 {
		// Final edit with complete text
		g.tg.EditMessageText(ctx, tgSink.chatID, streamMsgID, reply, nil)
	} else {
		// No edits happened (no tools called, fast response) — just send
		sink.SendText(ctx, reply)
	}

	// Auto-detect self-reflections in cortex response and save them even
	// when reflex didn't prescribe a post_action. Long responses with
	// self-reference markers ("я поняла", "мой вывод", "я осознаю") likely
	// contain insights worth persisting.
	if len(postActions) == 0 && len(reply) > 300 && g.looksLikeSelfReflection(reply) {
		postActions = append(postActions, bs.PostAction{Type: "save_reflection"})
		g.logger.Info("auto-detected self-reflection in cortex response", "reply_len", len(reply))
	}

	if len(postActions) > 0 {
		g.executePostActions(ctx, us, postActions, reply)
	}

	g.emitTurnCompleted(us, sess)

	if us.DebugMode || g.deps.Config.Gateway.Debug {
		go g.sendDebugDump(ctx, us, injectedCtx, reflexGuidance, msgText, preTraces, cortexTraces, engineRuleCount)
	}
	if g.deps.Config.TTS != nil && g.shouldSendVoice(ctx, us, sink) {
		go g.synthesizeAndSendVoice(ctx, sink, us, reply)
	}
}

// emitTurnCompleted fires the configured TurnCompletedHook (if any) for this
// session, in a non-blocking goroutine. Called from each of the three turn-
// completion exit paths in processMessages: voice streaming, non-Telegram
// batch, and Telegram streaming. The actor (or whatever consumer is wired)
// runs its memory state machine asynchronously without blocking the response
// loop. Sessions whose ID isn't a valid UUID are skipped with a warning —
// shouldn't happen in production but defensive against future schema drift.
func (g *Gateway) emitTurnCompleted(us *UserState, sess *session.Session) {
	if g.deps.TurnCompletedHook == nil || sess == nil {
		return
	}
	sid, err := uuid.Parse(sess.ID)
	if err != nil {
		g.logger.Warn("turn_completed: invalid session id", "session_id", sess.ID, "error", err)
		return
	}
	go g.deps.TurnCompletedHook(bs.WithSoulID(context.Background(), us.SoulID), us.UserID, sid)
}

// isVoiceEnabled checks if the user has voice mode on.
func (g *Gateway) isVoiceEnabled(ctx context.Context, us *UserState) bool {
	if g.deps.Users == nil {
		return false
	}
	profile, err := g.deps.Users.GetByID(ctx, us.UserID.String())
	if err != nil {
		return false
	}
	return profile.VoiceEnabled()
}

// synthesizeAndSendVoice synthesizes TTS and sends audio via ResponseSink.
// If sink supports StreamingVoiceSink, uses sentence-level pipelining
// for lower latency (client starts playback before full audio is ready).
func (g *Gateway) synthesizeAndSendVoice(ctx context.Context, sink bs.ResponseSink, us *UserState, text string) {
	cfg := g.deps.Config
	voice := cfg.TTSVoice

	var instruct string
	if cfg.TTSInstructMapper != nil {
		instruct = cfg.TTSInstructMapper(us.LastStrategy)
	}
	if cfg.TTSTextCleaner != nil {
		text = cfg.TTSTextCleaner(text)
	}

	// Sentence-level pipelining for streaming transports.
	// Uses MP3 format (macOS/iOS compatible) instead of OGG Opus.
	if streamSink, ok := sink.(bs.StreamingVoiceSink); ok {
		mp3Provider, hasMP3 := cfg.TTS.(bs.TTSProviderMP3)
		synthesize := cfg.TTS.Synthesize
		if hasMP3 {
			synthesize = mp3Provider.SynthesizeMP3
		}

		sentences := splitSentences(text)
		if len(sentences) <= 1 {
			audio, err := synthesize(ctx, text, voice, instruct)
			if err != nil {
				g.logger.Warn("tts: synthesize failed", "error", err)
				return
			}
			streamSink.SendVoiceChunk(ctx, audio, 1, true)
			return
		}

		g.logger.Info("tts: streaming", "sentences", len(sentences), "text_len", len(text))
		for i, sentence := range sentences {
			audio, err := synthesize(ctx, sentence, voice, instruct)
			if err != nil {
				g.logger.Warn("tts: chunk synthesis failed", "sentence", i, "error", err)
				continue
			}
			if err := streamSink.SendVoiceChunk(ctx, audio, i+1, i == len(sentences)-1); err != nil {
				g.logger.Warn("tts: send chunk failed", "error", err)
				return
			}
		}
		return
	}

	// Batch mode for non-streaming transports (Telegram).
	g.synthesizeBatch(ctx, sink, text, voice, instruct)
}

func (g *Gateway) synthesizeBatch(ctx context.Context, sink bs.ResponseSink, text, voice, instruct string) {
	cfg := g.deps.Config
	g.logger.Info("tts: synthesizing", "text_len", len(text), "voice", voice, "text_preview", truncateStr(text, 200))

	audio, err := cfg.TTS.Synthesize(ctx, text, voice, instruct)
	if err != nil {
		g.logger.Warn("tts: synthesize failed", "error", err)
		return
	}
	if cfg.TTSConverter != nil {
		if converted, err := cfg.TTSConverter(audio); err == nil {
			audio = converted
		} else {
			g.logger.Warn("tts: convert failed", "error", err)
			return
		}
	}
	if err := sink.SendVoice(ctx, audio); err != nil {
		g.logger.Warn("tts: send voice failed", "error", err)
	}
}

// splitSentences splits text on sentence boundaries for TTS pipelining.
func splitSentences(text string) []string {
	var sentences []string
	var current []rune
	runes := []rune(text)

	for i, r := range runes {
		current = append(current, r)
		if (r == '.' || r == '!' || r == '?' || r == '…') && i+1 < len(runes) {
			next := runes[i+1]
			// End sentence if followed by space + uppercase or newline.
			if next == ' ' || next == '\n' {
				s := strings.TrimSpace(string(current))
				if len([]rune(s)) >= 10 { // min sentence length to avoid splitting abbreviations
					sentences = append(sentences, s)
					current = nil
				}
			}
		}
	}
	if len(current) > 0 {
		s := strings.TrimSpace(string(current))
		if s != "" {
			sentences = append(sentences, s)
		}
	}
	return sentences
}

// shouldSendVoice checks if voice response should be sent.
// Always true for streaming sinks (WebSocket voice transport).
// For Telegram, checks user preference (/voice toggle).
func (g *Gateway) shouldSendVoice(ctx context.Context, us *UserState, sink bs.ResponseSink) bool {
	if _, ok := sink.(bs.StreamingVoiceSink); ok {
		return true // voice transport always gets audio
	}
	return g.isVoiceEnabled(ctx, us)
}

// telegramSink implements bs.ResponseSink for Telegram transport.
type telegramSink struct {
	gw     *Gateway
	chatID int64
}

func (g *Gateway) newTelegramSink(canonicalChatID string) *telegramSink {
	return &telegramSink{gw: g, chatID: tgChatID(canonicalChatID)}
}

func (s *telegramSink) SendText(ctx context.Context, text string) error {
	return s.gw.tg.SendLong(ctx, s.chatID, text)
}

func (s *telegramSink) SendVoice(ctx context.Context, audio []byte) error {
	chatID := fmt.Sprintf("%d", s.chatID)
	return s.gw.deps.Sender.SendVoice(ctx, chatID, audio)
}

func (s *telegramSink) SendTyping(ctx context.Context) error {
	return s.gw.tg.SendChatAction(ctx, s.chatID, "typing")
}

// tgChatID extracts int64 from canonical "telegram:NNN" string.
func tgChatID(canonical string) int64 {
	var id int64
	fmt.Sscanf(canonical, "telegram:%d", &id)
	return id
}

// tgCanonical converts int64 Telegram chat ID to canonical string.
func tgCanonical(chatID int64) string {
	return fmt.Sprintf("telegram:%d", chatID)
}

const reflexConfidenceThreshold = 0.7
const preActionTimeout = 10 * time.Second
const maxPreActions = 2


// runReflexPipeline executes the System 1/2 pipeline:
// 1. ReflexPreparer → structured context (traces + candidate rules)
// 2. Reflex LLM (Gemini Flash) → plan (matched rules, pre/post actions, tools)
// 3. Execute pre-actions (web_search etc.) → inject results into context
// 4. Build cortex context: matched rules + research + AME traces
//
// Returns (injectedContext, reflexGuidance, postActions, preTraces, engineRuleCount, silent).
// When silent=true the caller MUST abort the turn without calling cortex or
// sending any output — a structured rule with Silent=true matched.
func (g *Gateway) runReflexPipeline(ctx context.Context, us *UserState, msgText, priorContext string) (string, string, []bs.PostAction, []agent.ToolTrace, int, bool) {
	rc := us.Deps.ReflexPreparer(ctx, us.UserID.String(), msgText, priorContext)
	if rc == nil {
		return "", "", nil, nil, 0, false
	}

	// Store emotional strategy for TTS instruct mapping.
	us.LastStrategy = rc.Strategy

	// Build reflex prompt.
	var rulesBlock strings.Builder
	for _, r := range rc.CandidateRules {
		fmt.Fprintf(&rulesBlock, "[%s] WHEN: %s → DO: %s (sr=%.0f%%)\n",
			r.ID, r.Trigger, r.Action, r.SuccessRate*100)
	}

	// Tool list for the reflex prompt: one tool per line with its full
	// description. Reflex needs descriptions to disambiguate semantically
	// close tools — name-only lists force it to guess, which is where
	// most mis-selection bugs come from. The descriptions are the same
	// DB-driven strings the cortex sees, via the per-user registry built
	// during getOrInitUser.
	toolsList := "none configured"
	if us.Registry != nil && g.deps.RoleTools != nil {
		names := g.deps.RoleTools.Get("cortex")
		if len(names) > 0 {
			// Group tools by source: local vs each peer.
			local := &strings.Builder{}
			peerTools := make(map[string]*strings.Builder)

			for _, def := range us.Registry.DefinitionsForNames(names) {
				peer := us.Registry.PeerForTool(def.Name)
				line := fmt.Sprintf("- %s: %s\n", def.Name, strings.TrimSpace(def.Description))
				if peer == "" {
					local.WriteString(line)
				} else {
					if peerTools[peer] == nil {
						peerTools[peer] = &strings.Builder{}
					}
					peerTools[peer].WriteString(line)
				}
			}

			var sb strings.Builder
			if local.Len() > 0 {
				sb.WriteString("## Мои инструменты\n")
				sb.WriteString(local.String())
			}
			for peer, buf := range peerTools {
				fmt.Fprintf(&sb, "\n## Инструменты агента %s\n", peer)
				sb.WriteString(buf.String())
			}
			if sb.Len() > 0 {
				toolsList = strings.TrimRight(sb.String(), "\n")
			}
		}
	}

	if g.reflexPlanTemplate == "" {
		g.logger.Warn("reflex-plan prompt not in DB, skipping reflex")
		return rc.FullContext, "", nil, nil, 0, false
	}
	notesBlock := rc.ActiveNotes
	if notesBlock == "" {
		notesBlock = "(нет активных заметок)"
	}
	reflexPrompt := fmt.Sprintf(g.reflexPlanTemplate, rulesBlock.String(), toolsList, notesBlock, msgText)

	reflexResult, err := g.callReflex(ctx, reflexPrompt)
	if err != nil {
		// Reflex LLM unavailable (e.g. provider 429 / network error).
		// Don't bail out — keep going with full AME context and let
		// the rule engine still inject scope=always/keyword/state
		// guidance, otherwise tool-mandating rules silently disappear
		// whenever the upstream is degraded.
		g.logger.Warn("reflex failed, falling back to full context (rule engine still runs)", "error", err)
		reflexResult = &bs.ReflexResult{Confidence: 0}
	}

	g.logger.Info("reflex plan",
		"intent", reflexResult.Intent,
		"confidence", reflexResult.Confidence,
		"matched_rules", reflexResult.MatchedRules,
		"pre_actions", len(reflexResult.PreActions),
		"post_actions", len(reflexResult.PostActions),
		"tools", reflexResult.Tools,
	)

	// Low confidence → use full context but still run the Rule Engine below.
	// Previously this was a hard return that skipped Rule Engine entirely,
	// causing scope:always rules (like "ВЫЗВАТЬ tool call НЕМЕДЛЕННО") to
	// be silently dropped. Now we only skip reflex-specific outputs
	// (matched_rules, pre_actions) but let the rule engine inject guidance.
	lowConfidence := reflexResult.Confidence < reflexConfidenceThreshold
	if lowConfidence {
		g.logger.Info("reflex low confidence, using full context but keeping rule engine",
			"confidence", reflexResult.Confidence,
		)
		reflexResult.MatchedRules = nil
		reflexResult.PreActions = nil
		reflexResult.PostActions = nil
	}
	formattedTraces := rc.FormattedTraces
	if lowConfidence {
		formattedTraces = rc.FullContext
	}

	// Execute pre-actions (web_search etc.) with timeout.
	var researchBlock strings.Builder
	var preTraces []agent.ToolTrace
	preActionsToRun := reflexResult.PreActions
	if len(preActionsToRun) > maxPreActions {
		preActionsToRun = preActionsToRun[:maxPreActions]
	}
	for _, pa := range preActionsToRun {
		paCtx, cancel := context.WithTimeout(ctx, preActionTimeout)
		result, isError := us.Registry.Execute(paCtx, pa.Tool, pa.Input)
		cancel()
		inputStr := string(pa.Input)
		if len(inputStr) > 200 {
			inputStr = inputStr[:200] + "..."
		}
		outputStr := result
		if len(outputStr) > 500 {
			outputStr = outputStr[:500] + "..."
		}
		preTraces = append(preTraces, agent.ToolTrace{Name: pa.Tool, Input: inputStr, Output: outputStr, Error: isError})
		if isError {
			g.logger.Warn("reflex pre-action failed", "tool", pa.Tool, "error", result)
			continue
		}
		g.logger.Info("reflex pre-action done", "tool", pa.Tool, "result_len", len(result))
		if researchBlock.Len() == 0 {
			researchBlock.WriteString("[research]\n")
		}
		fmt.Fprintf(&researchBlock, "[%s result]\n%s\n\n", pa.Tool, truncateStr(result, 2000))
	}

	// Expand matched rules into directive block (dedup by ID).
	var guidance strings.Builder
	var hasRules bool
	seenRuleIDs := make(map[string]bool)

	// 0. Disambiguation: reflex detected multiple plausible tools.
	if reflexResult.Intent == "clarification_needed" && len(reflexResult.ClarificationOptions) > 0 {
		guidance.WriteString("[DISAMBIGUATION REQUIRED]\n")
		guidance.WriteString("Запрос неоднозначен. Спроси пользователя что он имеет в виду:\n")
		for i, opt := range reflexResult.ClarificationOptions {
			fmt.Fprintf(&guidance, "%d. %s\n", i+1, opt.Label)
		}
		guidance.WriteString("\nНЕ вызывай инструменты. Задай короткий вопрос с вариантами.\n\n")
		// Save options for resolution on the next turn.
		us.PendingDisambiguation = reflexResult.ClarificationOptions
		g.logger.Info("reflex: disambiguation",
			"options", len(reflexResult.ClarificationOptions),
			"intent", reflexResult.Intent,
		)
	} else if g := strings.TrimSpace(reflexResult.Guidance); g != "" {
		guidance.WriteString("[reflex guidance]\n")
		guidance.WriteString(g)
		guidance.WriteString("\n\n")
	}

	// 1. Rules from reflex classification (semantic match).
	if len(reflexResult.MatchedRules) > 0 {
		matchedSet := make(map[string]bool, len(reflexResult.MatchedRules))
		for _, id := range reflexResult.MatchedRules {
			matchedSet[id] = true
		}
		for _, r := range rc.CandidateRules {
			if matchedSet[r.ID] && !seenRuleIDs[r.ID] {
				seenRuleIDs[r.ID] = true
				if !hasRules {
					guidance.WriteString("[active rules]\n")
					hasRules = true
				}
				fmt.Fprintf(&guidance, "WHEN: %s\nDO: %s\n\n", r.Trigger, r.Action)
			}
		}
	}

	// 2. Rules from structured rule engine (condition-based match).
	var engineRuleCount int
	if us.Deps.RuleEngine != nil {
		engineRules := us.Deps.RuleEngine(ctx, bs.RuleContext{
			UserID:   us.Deps.UserID.String(),
			Intent:   reflexResult.Intent,
			Strategy: rc.Strategy,
			Hour:     time.Now().Hour(),
			Message:  msgText,
		})

		// Hard-silence gate: if any matched rule is marked Silent, abort the
		// turn entirely — no cortex call, no message sent. This is the only
		// way to enforce "do not respond" reliably; soft prompt instructions
		// in the rule's Action text are routinely ignored by the cortex LLM.
		for _, r := range engineRules {
			if r.Silent {
				g.logger.Info("rule engine: silent rule matched, aborting turn",
					"rule_id", r.ID,
					"trigger", r.Trigger,
					"chat_id", us.ChatID,
				)
				return "", "", nil, nil, 0, true
			}
		}

		for _, r := range engineRules {
			if seenRuleIDs[r.ID] {
				continue // already added by reflex
			}
			seenRuleIDs[r.ID] = true
			if !hasRules {
				guidance.WriteString("[active rules]\n")
				hasRules = true
			}
			fmt.Fprintf(&guidance, "WHEN: %s\nDO: %s\n\n", r.Trigger, r.Action)

			// Execute rule-prescribed pre_actions.
			for _, pa := range r.PreActions {
				paCtx, cancel := context.WithTimeout(ctx, preActionTimeout)
				result, isError := us.Registry.Execute(paCtx, pa.Tool, pa.Input)
				cancel()
				inputStr := string(pa.Input)
				if len(inputStr) > 200 {
					inputStr = inputStr[:200] + "..."
				}
				ruleOutputStr := result
				if len(ruleOutputStr) > 500 {
					ruleOutputStr = ruleOutputStr[:500] + "..."
				}
				preTraces = append(preTraces, agent.ToolTrace{Name: pa.Tool + " [rule]", Input: inputStr, Output: ruleOutputStr, Error: isError})
				if !isError {
					if researchBlock.Len() == 0 {
						researchBlock.WriteString("[research]\n")
					}
					fmt.Fprintf(&researchBlock, "[%s result]\n%s\n\n", pa.Tool, truncateStr(result, 2000))
				}
			}
		}
		engineRuleCount = len(engineRules)
		if engineRuleCount > 0 {
			g.logger.Info("rule engine matched", "count", engineRuleCount)
		}
	}

	if hasRules {
		guidance.WriteString("[/active rules]")
	}

	// Assemble: guidance (rules) + research + traces
	if researchBlock.Len() > 0 {
		guidance.WriteString("\n\n")
		guidance.WriteString(researchBlock.String())
	}

	// Intent-based guidance injection.
	if reflexResult.Intent == "memory_operation" && rc.ActiveNotes != "" && guidance.Len() == 0 {
		guidance.WriteString("[active_notes]\n")
		guidance.WriteString(rc.ActiveNotes)
		guidance.WriteString("[/active_notes]\n")
		guidance.WriteString("Если пользователь сообщает о выполнении — вызови memory_update(id, status=done).\n")
	}

	// Close research block if any pre-actions produced results.
	if researchBlock.Len() > 0 {
		researchBlock.WriteString("[/research]")
	}

	// When temporal_recall returned data, skip AME traces — they pollute
	// temporal queries with unrelated high-scoring memories from other dates.
	for _, pa := range preActionsToRun {
		if pa.Tool == "temporal_recall" && researchBlock.Len() > 50 {
			formattedTraces = ""
			break
		}
	}

	return formattedTraces, guidance.String(), reflexResult.PostActions, preTraces, engineRuleCount, false
}

// executePostActions runs post-cortex actions (save reflection, etc.).
func (g *Gateway) executePostActions(ctx context.Context, us *UserState, actions []bs.PostAction, reply string) {
	for _, pa := range actions {
		switch pa.Type {
		case "save_reflection":
			// Extract a concise insight from the cortex response via Flash.
			insight := g.extractInsight(ctx, reply, "reflection")
			if insight == "" {
				g.logger.Warn("post-action save_reflection: extraction returned empty")
				continue
			}
			input := fmt.Sprintf(`{"kind":"reflection","content":%q}`, insight)
			result, isError := us.Registry.Execute(ctx, "memory_save", json.RawMessage(input))
			if isError {
				g.logger.Warn("post-action save_reflection failed", "error", result)
			} else {
				g.logger.Info("post-action save_reflection done", "insight", truncateStr(insight, 100))
			}
		case "save_fact":
			insight := g.extractInsight(ctx, reply, "fact")
			if insight == "" {
				g.logger.Warn("post-action save_fact: extraction returned empty")
				continue
			}
			input := fmt.Sprintf(`{"fact":%q,"category":"general","source":"reflex"}`, insight)
			result, isError := us.Registry.Execute(ctx, "memory_save", json.RawMessage(input))
			if isError {
				g.logger.Warn("post-action save_fact failed", "error", result)
			} else {
				g.logger.Info("post-action save_fact done", "insight", truncateStr(insight, 100))
			}
		default:
			g.logger.Warn("unknown post-action type", "type", pa.Type)
		}
	}
}

// extractInsight calls Flash to distill a concise insight from a long cortex response.
// extractType is "reflection" or "fact".
func (g *Gateway) extractInsight(ctx context.Context, response, extractType string) string {
	model := g.reflexModel()
	if model == "" {
		return truncateStr(response, 200) // fallback
	}

	if g.extractInsightPrompt == "" {
		g.logger.Warn("extract-insight prompt not in DB, skipping")
		return ""
	}
	prompt := fmt.Sprintf(g.extractInsightPrompt, extractType, truncateStr(response, 1500))

	resp, err := g.provider.Complete(ctx, bs.CompletionRequest{
		Model:     model,
		MaxTokens: 128,
		System:    g.reflexSystemPrompt,
		Messages:  []bs.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		g.logger.Warn("extractInsight failed", "error", err)
		return ""
	}

	text := strings.TrimSpace(bs.ExtractText(resp.Content))
	// `[skip]` is the extract-insight prompt's signal that the response was
	// only an unverified temporal claim — don't persist as reflection/fact.
	// Treating it as empty short-circuits the executePostActions write.
	if text == "[skip]" || strings.HasPrefix(text, "[skip]") {
		g.logger.Info("extractInsight skipped", "type", extractType, "reason", "unverified temporal claim")
		return ""
	}
	g.logger.Info("extractInsight done", "type", extractType, "result", truncateStr(text, 100))
	return text
}

// looksLikeSelfReflection detects cortex responses that contain self-referential
// insights or reflections worth auto-saving. Markers are loaded from
// <Config.Prompts>/self_reflection_markers.md (JSON array). Empty slice
// (file absent) makes the check a no-op.
func (g *Gateway) looksLikeSelfReflection(text string) bool {
	if len(g.selfReflectionMarkers) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	hits := 0
	for _, m := range g.selfReflectionMarkers {
		if strings.Contains(lower, m) {
			hits++
		}
	}
	return hits >= 2
}

func truncateStr(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// callReflex sends a classification request to the reflex model and parses JSON.
func (g *Gateway) callReflex(ctx context.Context, prompt string) (*bs.ReflexResult, error) {
	model := g.reflexModel()
	if model == "" {
		return nil, fmt.Errorf("reflex model not configured")
	}

	g.logger.Info("calling reflex", "model", model)

	// Inject current datetime so reflex can compute dates for temporal_recall.
	now := time.Now().In(g.tz)
	reflexSystem := fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), g.reflexSystemPrompt)

	resp, err := g.provider.Complete(ctx, bs.CompletionRequest{
		Model:     model,
		MaxTokens: 512,
		System:    reflexSystem,
		Messages: []bs.Message{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("reflex LLM: %w", err)
	}

	text := bs.ExtractText(resp.Content)
	// Strip markdown fences if present.
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) > 2 {
			text = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	// Parse with flexible tools field: Flash sometimes returns objects instead of strings.
	var raw struct {
		MatchedRules   []string        `json:"matched_rules"`
		Intent         string          `json:"intent"`
		Confidence     float64         `json:"confidence"`
		PreActions     []bs.ToolAction `json:"pre_actions"`
		PostActions    []bs.PostAction `json:"post_actions"`
		Tools                json.RawMessage          `json:"tools"`
		Guidance             string                   `json:"guidance"`
		ClarificationOptions []bs.ClarificationOption `json:"clarification_options"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("parse reflex JSON %q: %w", text, err)
	}

	result := &bs.ReflexResult{
		MatchedRules:         raw.MatchedRules,
		Intent:               raw.Intent,
		Confidence:           raw.Confidence,
		PreActions:           raw.PreActions,
		PostActions:          raw.PostActions,
		Guidance:             raw.Guidance,
		ClarificationOptions: raw.ClarificationOptions,
	}

	// Try parsing tools as []string first, then as []{"tool":"name",...} objects.
	if len(raw.Tools) > 0 {
		var toolStrings []string
		if err := json.Unmarshal(raw.Tools, &toolStrings); err == nil {
			result.Tools = toolStrings
		} else {
			var toolObjects []struct{ Tool string `json:"tool"` }
			if err := json.Unmarshal(raw.Tools, &toolObjects); err == nil {
				for _, t := range toolObjects {
					result.Tools = append(result.Tools, t.Tool)
				}
			}
		}
	}

	return result, nil
}

func (g *Gateway) keepTypingViaSink(ctx context.Context, sink bs.ResponseSink) {
	_ = sink.SendTyping(ctx)
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = sink.SendTyping(ctx)
		}
	}
}

// GetOrCreateSession gets or creates a session with daily reset.
func (g *Gateway) GetOrCreateSession(ctx context.Context, us *UserState) (*session.Session, error) {
	if g.deps.ModelStore != nil {
		_ = g.deps.ModelStore.Refresh(ctx)
	}
	model := g.cortexModelDisplay()
	sess, err := g.store.GetOrCreate(ctx, us.UserID.String(), model)
	if err != nil {
		return nil, err
	}

	resetTime := g.todayResetTime()
	if sess.CreatedAt.Before(resetTime) {
		if err := g.store.Archive(ctx, sess.ID); err != nil {
			return nil, fmt.Errorf("archive old session: %w", err)
		}
		return g.store.Create(ctx, us.UserID.String(), model)
	}

	return sess, nil
}

func (g *Gateway) todayResetTime() time.Time {
	now := time.Now().In(g.tz)
	reset := time.Date(now.Year(), now.Month(), now.Day(), g.deps.Config.Gateway.SessionResetHour, 0, 0, 0, g.tz)
	if now.Before(reset) {
		reset = reset.AddDate(0, 0, -1)
	}
	return reset
}

// Timezone returns the configured timezone.
func (g *Gateway) Timezone() *time.Location { return g.tz }

func (g *Gateway) handleSessionCommand(ctx context.Context, chatID int64) {
	us, err := g.getOrInitUser(ctx, tgCanonical(chatID))
	if err != nil {
		g.logger.Debug("session command: ignored", "chat_id", chatID, "error", err)
		return
	}

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Error("session command: session error", "error", err)
		g.tg.SendLong(ctx, chatID, "Failed to load session.")
		return
	}

	buildDate := version.BuildDate
	if buildDate == "" {
		buildDate = "dev"
	}
	commit := version.Commit
	if commit == "" {
		commit = "local"
	}

	maxContext := g.deps.Config.Limits.MaxContext
	contextTokens := sess.TokenCount
	pct := 0
	if maxContext > 0 {
		pct = contextTokens * 100 / maxContext
	}

	ago := time.Since(sess.UpdatedAt).Truncate(time.Second)

	compactK := g.deps.Config.Limits.CompactThreshold / 1000
	msg := fmt.Sprintf(
		"🚀 BlueShip %s (%s)\n"+
			"🧠 Model: %s\n"+
			"📊 Session: %d msgs · ~%dk tokens\n"+
			"📚 Context: %dk/%dk (%d%%)\n"+
			"🧵 %s · updated %s ago\n"+
			"⚙️ Runtime: telegram · Compact threshold: %dk",
		buildDate, commit,
		g.cortexModelDisplay(),
		sess.MessageCount, sess.TokenCount/1000,
		contextTokens/1000, maxContext/1000, pct,
		shortID(sess.ID), ago,
		compactK,
	)

	if sess.CompactSummary != nil && *sess.CompactSummary != "" {
		summaryLen := len(*sess.CompactSummary)
		msg += fmt.Sprintf("\n📦 Compact: active (%d chars)", summaryLen)
	}

	if err := g.tg.SendLong(ctx, chatID, msg); err != nil {
		g.logger.Error("session command: send error", "error", err)
	}
}

// cortexModel returns the cortex (response generator) model in "provider:name" format.
func (g *Gateway) cortexModel() string {
	if g.deps.ModelStore != nil {
		if s := g.deps.ModelStore.ForRouter("cortex"); s != "" {
			return s
		}
	}
	// Fallback to Config.Models.Primary (backwards compat)
	p := g.deps.Config.Models.Primary
	if p.Provider != "" {
		return p.Provider + ":" + p.Name
	}
	return p.Name
}

// reflexModel returns the reflex (classifier) model in "provider:name" format.
// Returns empty string if reflex is not configured.
func (g *Gateway) reflexModel() string {
	if g.deps.ModelStore != nil {
		return g.deps.ModelStore.ForRouter("reflex")
	}
	return ""
}

// buildPriorContext pulls the last `n` chat messages from the current
// session and renders them as a compact "user: ... / assistant: ..."
// thread excerpt for AME embedding. Each message is truncated so the
// concatenated output stays small (embedding is not cheap and long
// context drowns the embed signal). Empty when the session has no
// prior messages or when the store is unavailable.
func (g *Gateway) buildPriorContext(ctx context.Context, sessionID string, n int) string {
	if g.store == nil || sessionID == "" || n <= 0 {
		return ""
	}
	msgs, err := g.store.MessagesForAPI(ctx, sessionID, 0)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	if len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	const perTurnCap = 280
	var sb strings.Builder
	for _, m := range msgs {
		text := stringifyMessageContent(m.Content)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if len([]rune(text)) > perTurnCap {
			r := []rune(text)
			text = string(r[:perTurnCap]) + "…"
		}
		role := m.Role
		if role == "user" {
			role = "user"
		} else if role == "assistant" {
			role = "assistant"
		} else {
			continue // skip tool messages — they're noise for embed
		}
		fmt.Fprintf(&sb, "%s: %s\n", role, text)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// stringifyMessageContent flattens a message Content (which can be a
// string or a slice of content blocks) into a plain-text fragment for
// embedding. Tool-use / tool-result blocks are skipped — only text
// blocks contribute.
func stringifyMessageContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []bs.ContentBlock:
		var parts []string
		for _, b := range v {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	case []any:
		var parts []string
		for _, raw := range v {
			if m, ok := raw.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" {
					if s, _ := m["text"].(string); s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func (g *Gateway) cortexModelDisplay() string {
	if g.deps.ModelStore != nil {
		if ref := g.deps.ModelStore.Get("cortex"); ref.Name != "" {
			return ref.Name
		}
	}
	return g.deps.Config.Models.Primary.Name
}

func (g *Gateway) handleResetCommand(ctx context.Context, chatID int64) {
	us, err := g.getOrInitUser(ctx, tgCanonical(chatID))
	if err != nil {
		g.logger.Debug("reset command: ignored", "chat_id", chatID, "error", err)
		return
	}

	// Refresh model config from DB. Role-tool allowlists come from code
	// (Config.RoleTools); they are not refreshable at runtime.
	if g.deps.ModelStore != nil {
		if err := g.deps.ModelStore.Refresh(ctx); err != nil {
			g.logger.Warn("reset: failed to refresh model config", "error", err)
		}
	}

	// Archive current session and create a new one immediately.
	uid := us.UserID.String()
	model := g.cortexModelDisplay()
	sess, err := g.store.GetOrCreate(ctx, uid, g.cortexModel())
	if err == nil && sess != nil {
		_ = g.store.Archive(ctx, sess.ID)
		g.logger.Info("reset: archived session",
			"chat_id", chatID,
			"session_id", sess.ID,
			"messages", sess.MessageCount,
		)
	}

	// Create new session right away so no race between archive and next message.
	newSess, err := g.store.Create(ctx, uid, model)
	sessionInfo := ""
	if err == nil && newSess != nil {
		sessionInfo = fmt.Sprintf("\nSession: %s", newSess.ID)
	}
	msg := fmt.Sprintf("Session reset.\nModel: %s%s", model, sessionInfo)
	if err := g.tg.SendLong(ctx, chatID, msg); err != nil {
		g.logger.Error("reset command: send error", "error", err)
	}
}

func (g *Gateway) handleVoiceCommand(ctx context.Context, chatID int64) {
	us, err := g.getOrInitUser(ctx, tgCanonical(chatID))
	if err != nil {
		return
	}
	if g.deps.Users == nil {
		g.tg.SendLong(ctx, chatID, "Voice: user store not available.")
		return
	}

	profile, err := g.deps.Users.GetByID(ctx, us.UserID.String())
	if err != nil {
		g.tg.SendLong(ctx, chatID, "Voice: user not found.")
		return
	}

	newState := !profile.VoiceEnabled()
	if err := g.deps.Users.SetPreference(ctx, us.UserID.String(), "voice_enabled", newState); err != nil {
		g.logger.Error("voice command: set preference error", "error", err)
		g.tg.SendLong(ctx, chatID, "Voice: failed to update preference.")
		return
	}

	msg := "Voice mode: OFF"
	if newState {
		msg = "Voice mode: ON"
	}
	g.tg.SendLong(ctx, chatID, msg)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- Debouncer ---

type pendingMsg struct {
	text      string
	images    []bs.ContentBlock
	messageID int
}

type debouncer struct {
	mu     sync.Mutex
	msgs   []pendingMsg
	timer  *time.Timer
	fire   func([]pendingMsg)
	window time.Duration
	cap    int
}

func newDebouncer(window time.Duration, cap int, fire func([]pendingMsg)) *debouncer {
	return &debouncer{
		window: window,
		cap:    cap,
		fire:   fire,
	}
}

func (d *debouncer) Add(msg pendingMsg) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.msgs = append(d.msgs, msg)

	if len(d.msgs) >= d.cap {
		d.fireNow()
		return
	}

	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(d.window, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.fireNow()
	})
}

func (d *debouncer) fireNow() {
	if len(d.msgs) == 0 {
		return
	}
	msgs := d.msgs
	d.msgs = nil
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.fire(msgs)
}

// sanitizeLeakedToolCalls removes tool call text that Gemma sometimes
// generates as plain text instead of structured tool_calls.
// Also removes HTML artifacts (<br>, </html>, etc.).
func sanitizeLeakedToolCalls(text string) string {
	// Remove patterns like: call:tool_name{...}
	for {
		idx := strings.Index(text, "call:")
		if idx == -1 {
			break
		}
		// Find the end of the tool call (closing brace)
		end := strings.Index(text[idx:], "}")
		if end == -1 {
			break
		}
		// Also consume any trailing |> or similar tokens
		endAbs := idx + end + 1
		for endAbs < len(text) && (text[endAbs] == '|' || text[endAbs] == '>' || text[endAbs] == '<' || text[endAbs] == ' ' || text[endAbs] == '\n') {
			endAbs++
		}
		text = text[:idx] + text[endAbs:]
	}

	// Remove <tool_call>...</tool_call> blocks
	for {
		start := strings.Index(text, "<tool_call")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], "</tool_call>")
		if end == -1 {
			end = strings.Index(text[start:], ">")
			if end == -1 {
				break
			}
			text = text[:start] + text[start+end+1:]
		} else {
			text = text[:start] + text[start+end+len("</tool_call>"):]
		}
	}

	// Remove HTML artifacts
	for _, tag := range []string{"<br>", "<br/>", "<br />", "</html>", "<html>", "</body>", "<body>"} {
		text = strings.ReplaceAll(text, tag, "")
	}

	// Remove Gemma thinking/channel control tokens
	for _, tok := range []string{"<channel|>", "</channel>", "\nthought\n", "\n\nthought\n\n"} {
		text = strings.ReplaceAll(text, tok, "")
	}
	// Standalone "thought" at start of response
	text = strings.TrimPrefix(text, "thought\n")
	text = strings.TrimPrefix(text, "thought")

	return strings.TrimSpace(text)
}

// resolveDisambiguation checks if a short message resolves a pending disambiguation.
// Returns the chosen option or nil if the message doesn't match any option.
func resolveDisambiguation(msg string, options []bs.ClarificationOption) *bs.ClarificationOption {
	msg = strings.TrimSpace(strings.ToLower(msg))
	if msg == "" || len(options) == 0 {
		return nil
	}

	// Numeric choice: "1", "2", etc.
	if idx, err := strconv.Atoi(msg); err == nil && idx >= 1 && idx <= len(options) {
		return &options[idx-1]
	}

	// Keyword match against option labels.
	for i := range options {
		label := strings.ToLower(options[i].Label)
		if strings.Contains(label, msg) || strings.Contains(msg, label) {
			return &options[i]
		}
	}

	// Can't resolve programmatically — return nil.
	// Cortex will see the disambiguation in session history and decide from context.
	return nil
}
