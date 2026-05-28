package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/agent"
	"github.com/rasimio/blueship/attachment"
	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/browser"
	"github.com/rasimio/blueship/internal/openai"
	"github.com/rasimio/blueship/internal/telegram"
	"github.com/rasimio/blueship/session"
	"github.com/rasimio/blueship/tool"
)

// Gateway receives transport updates and routes them through the AgentLoop.
type Gateway struct {
	deps     *bs.Deps
	modules  ModuleRegistry
	store    *session.Store
	provider bs.CompletionProvider
	whisper  *openai.TranscriptionProvider
	tz       *time.Location
	logger   *slog.Logger

	// Multi-bot registry. Populated by ReloadBots from
	// cfg.Transport.Telegram.ListBots (or legacy cfg.Transport.BotToken
	// as a single-row fallback). All inbound updates fan into
	// updatesChan tagged with the receiving bot's id; outbound sends
	// reach for the bot via UserState.Bot or g.botByTGID for callbacks.
	// See gateway_bots.go for the lifecycle methods.
	botsMu      sync.RWMutex
	bots        map[uuid.UUID]*botInstance
	botsByTGID  map[int64]*botInstance
	updatesChan chan taggedUpdate

	systemPrompt string

	// platformGreet is the message sent to unpaired chats that land on a
	// platform-kind bot. Loaded once at startup from
	// <Config.Prompts>/telegram_platform_greeting.md; empty falls back to
	// an in-code default in replyUnpaired.
	platformGreetMu sync.Mutex
	platformGreet   string

	// Platform prompt layers for souls in the vaelum tenancy model. Loaded
	// once from vaelum.platform_prompts and cached by platformPrompts; the
	// database is the runtime source of truth for system prompts.
	ppMu       sync.Mutex
	ppLoaded   bool
	ppPreamble string
	ppAgents   string

	// Reflex pipeline prompts. Loaded from <Config.Prompts>/<key>.md when
	// the agent ships those files; missing files leave the default empty.
	reflexSystemPrompt       string   // system prompt for reflex LLM call
	reflexPlanTemplate       string   // user prompt template (has %s placeholders for rules, tools, message)
	reflexInteractionPrompt  string   // interaction-tier task rules, appended to the soul prompt when InteractionTier is on
	reflexInterjectionPrompt string   // system prompt for barge-in interjection classification
	extractInsightPrompt     string   // system prompt for insight extraction
	selfReflectionMarkers    []string // optional self_reflection_markers.md (JSON array)

	mu    sync.Mutex
	users map[string]*UserState // keyed by canonical chatID ("telegram:123", "voice:owner")
}

// parseCommand extracts the bare command from a Telegram slash command,
// stripping an optional `@<botname>` suffix, and reports whether the command
// is addressed to this bot. Rules:
//   - "/reset" → (cmd="/reset", forUs=true)  — no target, everyone matches
//   - "/reset@LiyaDeusBot" with bot.tgUsername="LiyaDeusBot" → forUs=true
//   - "/reset@arlene_bot" with bot.tgUsername="LiyaDeusBot" → forUs=false
//   - "/reset foo" (args) → cmd="/reset" — args stripped
//
// If we never learned the bot's username (getMe failed), every command
// with a non-empty suffix is treated as addressed (forUs=true) so users
// still have a working fallback.
func (g *Gateway) parseCommand(bi *botInstance, text string) (cmd string, forUs bool) {
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
		botName := ""
		if bi != nil {
			botName = bi.tgUsername
		}
		if botName == "" || strings.EqualFold(target, botName) {
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
func (g *Gateway) shouldProcessGroupMessage(bi *botInstance, msg *telegram.Message, text string) bool {
	var botName string
	var botID int64
	if bi != nil {
		botName = bi.tgUsername
		botID = bi.tgBotID
	}
	if botName != "" && text != "" {
		if strings.Contains(strings.ToLower(text), "@"+strings.ToLower(botName)) {
			return true
		}
	}
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		rep := msg.ReplyToMessage.From
		if botID != 0 && rep.ID == botID {
			return true
		}
		if botName != "" && strings.EqualFold(rep.Username, botName) {
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

	// bot carries the Telegram bot this chat is bound to. Set on
	// getOrInitTelegramUser; nil for the voice:owner / WS-only paths.
	// Outbound sends (debug docs, /reset replies, streaming edits) use
	// bot.client to talk back on the same bot the user pinged.
	bot *botInstance

	// tgChatID is the numeric Telegram chat id (chatID without the
	// "telegram:" prefix). Cached on init so command handlers don't have
	// to re-parse the canonical string for every send.
	tgChatID int64

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

// NewGateway creates a new gateway. The Telegram bot registry starts
// empty; the caller must invoke ReloadBots(ctx) once after construction
// to populate it from cfg.Transport.Telegram.ListBots (or the legacy
// cfg.Transport.BotToken fallback). Without bots registered the gateway
// runs in transport-agnostic mode and only serves non-Telegram sinks
// (WebSocket, HTTPChat).
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
		deps:     deps,
		modules:  modules,
		store:    session.NewStore(coreDB),
		provider: cfg.LLM,
		whisper:  whisperProvider,
		tz:       tz,
		logger:   logger,
		users:    make(map[string]*UserState),
	}
	gw.initBotRegistry()

	// Load system prompts from the agent's prompts directory (Config.Prompts).
	// Personality lives with the agent, never in the framework.
	if cfg.Prompts == "" {
		return nil, fmt.Errorf("system prompts not configured: set Config.Prompts to a directory containing <key>.md files")
	}
	if err := gw.loadSystemPrompts(cfg.Prompts); err != nil {
		return nil, fmt.Errorf("load system prompts: %w", err)
	}
	gw.loadPlatformGreet(cfg.Prompts)

	return gw, nil
}

// loadPlatformGreet reads the greeting shown to unpaired chats on
// platform-kind bots. Optional: a missing file leaves platformGreet
// empty and replyUnpaired falls back to a minimal default.
func (g *Gateway) loadPlatformGreet(dir string) {
	data, err := os.ReadFile(filepath.Join(dir, "telegram_platform_greeting.md"))
	if err != nil {
		g.logger.Info("gateway: telegram_platform_greeting.md not found; using built-in default", "error", err)
		return
	}
	g.platformGreetMu.Lock()
	g.platformGreet = strings.TrimSpace(string(data))
	g.platformGreetMu.Unlock()
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
	if v := readOpt("reflex-system"); v != "" {
		g.reflexSystemPrompt = v
	}
	if v := readOpt("reflex-plan"); v != "" {
		g.reflexPlanTemplate = v
	}
	if v := readOpt("reflex-interaction"); v != "" {
		g.reflexInteractionPrompt = v
	}
	if v := readOpt("reflex-interjection"); v != "" {
		g.reflexInterjectionPrompt = v
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

// systemPromptForSoul returns the fully composed system prompt for a soul:
// the platform preamble + the soul's own persona + the platform agents
// layer, all read from the database. A soul without a vaelum.soul_personas
// row is a misconfiguration and surfaces as an error — there is no silent
// fallback to file-loaded prompts. Framework consumers that do not use the
// vaelum soul model (soulID is nil) get the file-loaded process prompt.
func (g *Gateway) systemPromptForSoul(ctx context.Context, soulID uuid.UUID) (string, error) {
	if soulID == uuid.Nil {
		return g.systemPrompt, nil
	}
	db, err := g.deps.DB("ship")
	if err != nil {
		return "", fmt.Errorf("system prompt for soul %s: db unavailable: %w", soulID, err)
	}
	var persona string
	if err := db.GetContext(ctx, &persona,
		`SELECT system_prompt FROM vaelum.soul_personas WHERE soul_id = $1`,
		soulID); err != nil {
		return "", fmt.Errorf("system prompt for soul %s: no persona row in vaelum.soul_personas: %w", soulID, err)
	}
	if strings.TrimSpace(persona) == "" {
		return "", fmt.Errorf("system prompt for soul %s: persona row has empty system_prompt", soulID)
	}
	preamble, agents, err := g.platformPrompts(ctx)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{preamble, persona, agents}, "\n\n"), nil
}

// platformPrompts returns the platform preamble and agents layers, loaded
// once from vaelum.platform_prompts and cached for the process lifetime.
// A failed load is not cached, so a transient error is retried next call.
func (g *Gateway) platformPrompts(ctx context.Context) (preamble, agents string, err error) {
	g.ppMu.Lock()
	defer g.ppMu.Unlock()
	if g.ppLoaded {
		return g.ppPreamble, g.ppAgents, nil
	}
	db, err := g.deps.DB("ship")
	if err != nil {
		return "", "", fmt.Errorf("platform prompts: db unavailable: %w", err)
	}
	var rows []struct {
		Key     string `db:"key"`
		Content string `db:"content"`
	}
	if err := db.SelectContext(ctx, &rows,
		`SELECT key, content FROM vaelum.platform_prompts WHERE key IN ('preamble', 'agents')`); err != nil {
		return "", "", fmt.Errorf("platform prompts: query vaelum.platform_prompts: %w", err)
	}
	layer := map[string]string{}
	for _, r := range rows {
		layer[r.Key] = r.Content
	}
	if strings.TrimSpace(layer["preamble"]) == "" || strings.TrimSpace(layer["agents"]) == "" {
		return "", "", fmt.Errorf("platform prompts: vaelum.platform_prompts is missing the 'preamble' or 'agents' row")
	}
	g.ppPreamble, g.ppAgents, g.ppLoaded = layer["preamble"], layer["agents"], true
	return g.ppPreamble, g.ppAgents, nil
}

// reflexSystemPromptForSoul composes the interaction-tier system prompt:
// platform preamble + the soul's persona only. The agents layer (cortex's
// full operational manual with all its tools) is deliberately excluded —
// with it, the fast tier behaves like cortex and tries to call cortex's
// tools (memory_search, browser_fetch, …) directly instead of escalating.
func (g *Gateway) reflexSystemPromptForSoul(ctx context.Context, soulID uuid.UUID) (string, error) {
	if soulID == uuid.Nil {
		return g.systemPrompt, nil
	}
	db, err := g.deps.DB("ship")
	if err != nil {
		return "", fmt.Errorf("reflex system prompt: db unavailable: %w", err)
	}
	var persona string
	if err := db.GetContext(ctx, &persona,
		`SELECT system_prompt FROM vaelum.soul_personas WHERE soul_id = $1`, soulID); err != nil {
		return "", fmt.Errorf("reflex system prompt: no persona for soul %s: %w", soulID, err)
	}
	preamble, _, err := g.platformPrompts(ctx)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{preamble, persona}, "\n\n"), nil
}

// Run drives the multi-bot fan-in: every registered bot's poller writes
// into g.updatesChan tagged with its id; this loop dispatches each
// tagged update to handleUpdate. The host is expected to have called
// ReloadBots(ctx) before Run so the registry is non-empty.
//
// The periodic reconcile loop runs alongside as a goroutine so the
// gateway recovers if a host-triggered ReloadBots call was missed
// (e.g. internal HTTP signal dropped).
func (g *Gateway) Run(ctx context.Context) {
	go g.runReloadLoop(ctx)
	g.logger.Info("telegram gateway started")

	for {
		select {
		case <-ctx.Done():
			return
		case tagged := <-g.updatesChan:
			bi := g.botByID(tagged.botID)
			if bi == nil {
				// Bot was unregistered after the poller dequeued this
				// update but before we dispatched. Drop quietly.
				continue
			}
			g.handleUpdate(ctx, bi, tagged.update)
		}
	}
}

func (g *Gateway) handleUpdate(ctx context.Context, bi *botInstance, update telegram.Update) {
	// Handle callback queries (inline button presses).
	// LEGACY: the /model command's inline-keyboard callbacks land here; the
	// dispatch is parked behind the `legacy_commands` build tag along with
	// handleModelCallback in model_command.go. Restoration: rebuild with
	// `-tags legacy_commands` and uncomment the dispatch below.
	if cq := update.CallbackQuery; cq != nil {
		bi.client.AnswerCallbackQuery(ctx, cq.ID)
		// Bot-onboarding inline keyboards (preset picker) land here.
		// Handled before the legacy model dispatch so a fresh user's
		// keyboard taps reach the FSM finalizer even without the
		// legacy_commands build tag.
		if g.maybeRunBotOnboardingCallback(ctx, bi, cq) {
			return
		}
		// if g.handleModelCallback(ctx, bi, cq) {
		// 	return
		// }
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

	// Document attachments — single ingest path through the shared
	// content-based classifier (blueship/attachment.Kind). Downloads
	// up to the cross-kind max, then dispatches by sniffed kind so
	// an image-as-document (e.g. PNG sent uncompressed) lands in the
	// vision lane, a renamed PDF still reaches the extractor, and
	// any UTF-8 text — including languages the old whitelist missed
	// (.cpp, .rs, .kt, Dockerfile, files without extensions) — gets
	// inlined as a fenced block. Raw bytes also accumulate in
	// rawAttachments so processMessages can hand them off to the
	// AttachmentSink — that's what makes Telegram-originated files
	// show up as chips in the cabinet on reload.
	var docImage *bs.ContentBlock
	var rawAttachments []rawAttachment
	if msg.Document != nil {
		data, err := bi.client.DownloadFile(ctx, msg.Document.FileID, attachment.MaxAnyBytes)
		if err != nil {
			g.logger.Warn("failed to download document", "error", err, "file", msg.Document.FileName)
		} else {
			kind := attachment.Kind(msg.Document.MimeType, msg.Document.FileName, data)
			if cap := attachment.MaxBytesForKind(kind); cap > 0 && int64(len(data)) > cap {
				g.logger.Warn("document over kind cap", "file", msg.Document.FileName, "kind", kind, "size", len(data), "cap", cap)
				text = appendDocInline(text, fmt.Sprintf("[file: %s — too large (%d bytes; %s cap is %d)]", msg.Document.FileName, len(data), kind, cap))
			} else {
				switch kind {
				case "image":
					// Always source media_type from the bytes; a renamed
					// PNG sent as a Document arrives with a stale or
					// missing MIME header, and Anthropic vision refuses
					// requests where declared media_type disagrees with
					// the bytes.
					media := attachment.MimeForImage(data)
					if media == "" {
						g.logger.Warn("document classified as image but no signature match", "file", msg.Document.FileName)
						break
					}
					docImage = &bs.ContentBlock{
						Type: "image",
						Source: &bs.ImageSource{
							Type:      "base64",
							MediaType: media,
							Data:      base64.StdEncoding.EncodeToString(data),
						},
					}
					rawAttachments = append(rawAttachments, rawAttachment{
						name: msg.Document.FileName, mime: media, kind: "image", data: data,
					})
				case "pdf":
					if pdfText, pages, perr := browser.ExtractPDFText(data); perr != nil {
						g.logger.Warn("failed to extract pdf text", "error", perr, "file", msg.Document.FileName, "size", len(data))
						text = appendDocInline(text, fmt.Sprintf("[pdf: %s — extraction failed: %v]", msg.Document.FileName, perr))
					} else {
						text = appendDocInline(text, fmt.Sprintf("[pdf: %s — %d pages]%s", msg.Document.FileName, pages, pdfText))
					}
					rawAttachments = append(rawAttachments, rawAttachment{
						name: msg.Document.FileName, mime: "application/pdf", kind: "pdf", data: data,
					})
				case "text":
					text = appendDocInline(text, fmt.Sprintf("[file: %s]\n```\n%s\n```", msg.Document.FileName, strings.ReplaceAll(string(data), "\r\n", "\n")))
					mime := msg.Document.MimeType
					if mime == "" {
						mime = "text/plain"
					}
					rawAttachments = append(rawAttachments, rawAttachment{
						name: msg.Document.FileName, mime: mime, kind: "text", data: data,
					})
				default:
					g.logger.Info("ignoring unsupported document", "file", msg.Document.FileName, "mime", msg.Document.MimeType)
				}
			}
		}
	}

	if msg.Voice != nil && g.whisper != nil && g.whisper.IsConfigured() {
		audio, err := bi.client.DownloadFile(ctx, msg.Voice.FileID, 10*1024*1024)
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
	if docImage != nil {
		images = append(images, *docImage)
	}
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1] // largest resolution
		data, err := bi.client.DownloadFile(ctx, photo.FileID, attachment.MaxImageBytes)
		if err != nil {
			g.logger.Warn("failed to download photo", "error", err, "file_id", photo.FileID)
		} else {
			// Telegram clients re-encode photos as JPEG before
			// upload — even a PNG sent through the photo lane
			// arrives as image/jpeg. MimeForImage confirms from
			// the bytes; on the rare miss, fall back to JPEG
			// since that's what Telegram actually sent.
			media := attachment.MimeForImage(data)
			if media == "" {
				media = "image/jpeg"
			}
			images = append(images, bs.ContentBlock{
				Type: "image",
				Source: &bs.ImageSource{
					Type:      "base64",
					MediaType: media,
					Data:      base64.StdEncoding.EncodeToString(data),
				},
			})
			// Telegram photos have no filename. Pick a stable one
			// derived from the file id so the cabinet chip has
			// something to render and downloads land with a
			// reasonable name.
			name := "telegram-photo"
			if photo.FileID != "" {
				name = "tg-" + photo.FileID + ".jpg"
			}
			rawAttachments = append(rawAttachments, rawAttachment{
				name: name, mime: media, kind: "image", data: data,
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
	tgUserID := msg.From.ID

	// Group-chat routing: in a chat with more than one participant the bot
	// only reacts to messages that are explicitly addressed to it. Private
	// (1:1) chats bypass this filter because the human has nobody else to
	// talk to. Slash commands are handled below via parseCommand regardless
	// of this filter — commands are their own addressing mechanism.
	if msg.Chat.Type != "private" && !strings.HasPrefix(text, "/") {
		if !g.shouldProcessGroupMessage(bi, msg, text) {
			g.logger.Debug("gateway: group message not addressed, skipping",
				"chat_id", chatID,
				"chat_type", msg.Chat.Type,
			)
			return
		}
	}

	_ = bi // keep `bi` referenced even when the broader command dispatch is off
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

	// LEGACY: /debug toggle. Same rationale as the other commands above —
	// the cabinet is the right place for per-soul debug visibility, not
	// a shared bot. Restoration: uncomment along with the sendDebugDump
	// invocations in the response path (see `// LEGACY: sendDebugDump`).
	//
	// if text == "/debug" {
	// 	us, err := g.getOrInitTelegramUser(ctx, bi, chatID, rawChatID, tgUserID)
	// 	if err == nil {
	// 		us.Mu.Lock()
	// 		us.DebugMode = !us.DebugMode
	// 		mode := "OFF"
	// 		if us.DebugMode {
	// 			mode = "ON"
	// 		}
	// 		us.Mu.Unlock()
	// 		bi.client.SendMessage(ctx, fmt.Sprintf("%d", rawChatID), fmt.Sprintf("Debug mode: %s", mode))
	// 	}
	// 	return
	// }

	// Deep-link "Approve in bot" auth. The cabinet's "Login via
	// Telegram App" button points at https://t.me/<bot>?start=login_<TOKEN>
	// which Telegram delivers to us as /start login_<TOKEN>. We hand the
	// token to the host's CompleteDeeplinkLogin, send the resulting
	// confirmation/error line, and STOP — the cabinet's poll will pick
	// up the approval next tick. Must run before maybeRunBotOnboarding
	// because the FSM treats every /start as either welcome-back or the
	// start of in-chat onboarding, neither of which makes sense for an
	// auth-approval click.
	if g.maybeRunDeeplinkLogin(ctx, bi, rawChatID, tgUserID, text) {
		return
	}

	// Inline bot onboarding: when the host has wired Deps.BotOnboarding,
	// intercept messages from chats with no vaelum.user_identities row
	// and run the in-chat account-creation FSM. The hook checks pairing
	// itself so a paired user's /start lands in the welcome-back path
	// and any other inbound from a paired user falls through to the
	// normal getOrInitTelegramUser routing. Unpaired non-/start inbound
	// continues to replyUnpaired below — onboarding only starts when
	// the user explicitly types /start.
	if g.maybeRunBotOnboarding(ctx, bi, chatID, rawChatID, tgUserID, text) {
		return
	}

	us, err := g.getOrInitTelegramUser(ctx, bi, chatID, rawChatID, tgUserID)
	if err != nil {
		if errors.Is(err, bs.ErrTelegramChatUnpaired) {
			g.replyUnpaired(ctx, bi, chatID)
			return
		}
		g.logger.Debug("ignored message", "chat_id", chatID, "error", err)
		return
	}

	// /reset — multi-tenant: archive the active (user, soul) chat session
	// and open a fresh one via the same gateway method the cabinet's web
	// reset button uses, so Telegram and HTTP/SSE stay behaviourally
	// identical. Other legacy single-user slash commands (/session,
	// /model, /voice) remain parked behind the `legacy_commands` build
	// tag — those don't fit the multi-bot Vaelum world.
	if cmd, forUs := g.parseCommand(bi, text); cmd == "/reset" && forUs {
		go func() {
			rctx := bs.WithSoulID(context.Background(), us.SoulID)
			oldID, newID, rerr := g.ResetSession(rctx, us.UserID.String())
			if rerr != nil {
				g.logger.Warn("telegram /reset failed",
					"chat_id", us.ChatID, "user_id", us.UserID, "error", rerr)
				if bi != nil && bi.client != nil {
					_, _ = bi.client.SendMessage(rctx, fmt.Sprintf("%d", rawChatID), "Reset failed.")
				}
				return
			}
			g.logger.Info("telegram /reset done",
				"chat_id", us.ChatID, "user_id", us.UserID,
				"old_session_id", oldID, "new_session_id", newID)
			if bi != nil && bi.client != nil {
				_, _ = bi.client.SendMessage(rctx, fmt.Sprintf("%d", rawChatID), "Session reset. New thread.")
			}
		}()
		return
	}

	var replyToTGID int
	if msg.ReplyToMessage != nil {
		replyToTGID = msg.ReplyToMessage.MessageID
	}
	us.debounce.Add(pendingMsg{
		text:               text,
		images:             images,
		messageID:          msg.MessageID,
		rawAttachments:     rawAttachments,
		replyToTGMessageID: replyToTGID,
	})
}

// uuidInTextRE matches any plausible attachment UUID inside user
// text — used by resolveInlineAttachmentRefs so a user can paste an
// id ("прочти abc-…", "что на картинке abc-…") and the gateway
// inlines the file as if it had been attached natively. The pattern
// is the standard 8-4-4-4-12 hex shape; tenant-scoped lookups in
// Sink.Get drop the rare false positive (a random UUID the user
// mentioned for unrelated reasons) without leaking anything.
var uuidInTextRE = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// resolveInlineAttachmentRefs scans text blocks in the user's turn
// for attachment UUIDs, resolves each via the host's AttachmentSink,
// and appends the resulting content as additional blocks (image for
// kind=image, fenced text for kind=pdf/text). The triggering text
// itself stays in place so the model can still understand the
// user's question ("что на картинке UUID" reads naturally with the
// image attached). De-dups by id so a UUID mentioned twice doesn't
// produce two copies of the file.
func (g *Gateway) resolveInlineAttachmentRefs(ctx context.Context, us *UserState, blocks []bs.ContentBlock) []bs.ContentBlock {
	if g.deps.AttachmentSink == nil || us.UserID == uuid.Nil || us.SoulID == uuid.Nil {
		return blocks
	}
	seen := map[uuid.UUID]bool{}
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		matches := uuidInTextRE.FindAllString(b.Text, -1)
		for _, m := range matches {
			id, perr := uuid.Parse(strings.ToLower(m))
			if perr != nil || seen[id] {
				continue
			}
			seen[id] = true
		}
	}
	if len(seen) == 0 {
		return blocks
	}

	ids := make([]uuid.UUID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	blocks = append(blocks, g.attachmentBlocksByIDs(ctx, us, ids, "ref")...)
	return blocks
}

// attachmentBlocksByIDs resolves a set of attachment ids via the
// host's CDN and renders each as one content block — image vision
// block for picture kinds, fenced text dump for files. `labelPrefix`
// is the bracket tag the resulting text blocks carry ("ref" for
// inline-UUID resolution, "reply-attached" for the reply-context
// expander), so cortex reads the same syntax in two cases and
// distinguishes provenance. Unknown / foreign ids are skipped
// silently — caller has already done a tenancy check via Get.
func (g *Gateway) attachmentBlocksByIDs(ctx context.Context, us *UserState, ids []uuid.UUID, labelPrefix string) []bs.ContentBlock {
	if g.deps.AttachmentSink == nil || us.UserID == uuid.Nil || us.SoulID == uuid.Nil {
		return nil
	}
	out := make([]bs.ContentBlock, 0, len(ids))
	for _, id := range ids {
		rec, data, err := g.deps.AttachmentSink.Get(ctx, us.UserID, us.SoulID, id)
		if err != nil {
			g.logger.Debug("attachment block: not resolved",
				"chat_id", us.ChatID, "id", id, "err", err)
			continue
		}
		if rec == nil || len(data) == 0 {
			continue
		}
		switch rec.Kind {
		case "image":
			media := rec.Mime
			if media == "" {
				media = "image/jpeg"
			}
			out = append(out, bs.ContentBlock{
				Type: "image",
				Source: &bs.ImageSource{
					Type:      "base64",
					MediaType: media,
					Data:      base64.StdEncoding.EncodeToString(data),
				},
			})
		case "pdf":
			// Prefer the host-supplied source (markdown for host-
			// generated PDFs) over re-extracting from the rendered
			// bytes — chromedp font subsets aren't readable by
			// ledongthuc/pdf and produce mojibake.
			if rec.SourceText != "" {
				out = append(out, bs.ContentBlock{
					Type: "text",
					Text: fmt.Sprintf("[%s: %s — pdf, markdown source]\n%s", labelPrefix, rec.Name, rec.SourceText),
				})
				continue
			}
			text, _, perr := browser.ExtractPDFText(data)
			if perr != nil {
				out = append(out, bs.ContentBlock{
					Type: "text",
					Text: fmt.Sprintf("[%s: %s — pdf extract failed: %v]", labelPrefix, rec.Name, perr),
				})
				continue
			}
			out = append(out, bs.ContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[%s: %s — pdf]\n%s", labelPrefix, rec.Name, text),
			})
		case "text":
			out = append(out, bs.ContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[%s: %s]\n```\n%s\n```", labelPrefix, rec.Name, string(data)),
			})
		}
	}
	return out
}

// attachMarkerRE matches the `[attached: UUID]` sentinel the
// attachment_include tool emits. The gateway's post-loop dispatcher
// (dispatchAttachmentMarkers) rewrites these into transport-native
// file sends — for Telegram, a SendPhoto / SendDocument per marker;
// for the cabinet, the marker stays in the text and the history
// endpoint resolves it into an attachment MessagePart at read time.
var attachMarkerRE = regexp.MustCompile(`(?i)\[attached:\s*([0-9a-f-]{36})\s*\]`)

// dispatchAttachmentMarkers walks the assistant's reply text, looks
// up every `[attached: UUID]` reference against the host's
// AttachmentSink, and either:
//
//   - ships the bytes out the current sink (when the sink implements
//     AttachmentSendSink — Telegram does), then strips the marker
//     from the text so the user doesn't see the raw sentinel;
//   - leaves the marker in place when the sink can't send files
//     directly (cabinet's SSE sink) — vaelum's history endpoint
//     parses the marker on read and emits an attachment chip.
//
// Unknown / foreign UUIDs are stripped silently so a hallucinated
// marker doesn't leak a sentinel into chat. Sink errors are warn-
// logged but don't fail the turn — the text reply still goes out.
func (g *Gateway) dispatchAttachmentMarkers(ctx context.Context, us *UserState, sink bs.ResponseSink, reply string) string {
	if reply == "" || g.deps.AttachmentSink == nil {
		return reply
	}
	matches := attachMarkerRE.FindAllStringSubmatchIndex(reply, -1)
	if len(matches) == 0 {
		return reply
	}
	sender, sendable := sink.(bs.AttachmentSendSink)
	if !sendable {
		// Cabinet path: keep markers, history endpoint will resolve.
		return reply
	}

	// Collect ids we need (de-duped) so a marker repeated twice
	// doesn't double-send.
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	for _, m := range matches {
		idStr := strings.ToLower(reply[m[2]:m[3]])
		id, perr := uuid.Parse(idStr)
		if perr != nil {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}

	for _, id := range ids {
		rec, data, err := g.deps.AttachmentSink.Get(ctx, us.UserID, us.SoulID, id)
		if err != nil {
			g.logger.Warn("attachment marker: resolve failed",
				"chat_id", us.ChatID, "id", id, "err", err)
			continue
		}
		if rec == nil {
			continue
		}
		if err := sender.SendAttachment(ctx, *rec, data); err != nil {
			g.logger.Warn("attachment marker: send failed",
				"chat_id", us.ChatID, "id", id, "err", err)
		}
	}

	cleaned := attachMarkerRE.ReplaceAllString(reply, "")
	// Collapse the blank lines a stripped sentinel leaves behind so
	// the message text reads naturally on the user's side.
	return collapseBlankLinesGateway(cleaned)
}

// collapseBlankLinesGateway is the gateway-local copy of the same
// helper vaelum's buildParts uses; lives here so the gateway doesn't
// take a vaelum-side import.
func collapseBlankLinesGateway(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// scanAndSaveLinks extracts http/https URLs out of text and upserts
// them as kind='link' rows on the host's AttachmentSink. Symmetric to
// the byte-attachment Save call: the user side runs this against the
// pasted message, the assistant side runs it against Arlene's final
// reply. The OG enrichment worker picks the rows up from there.
//
// sessionID is required (so the cabinet's session-scoped view can
// surface the chip on the right turn). messageID is best-effort —
// callers pass uuid.Nil when the underlying chat_messages row hasn't
// been written yet (user side: append happens later inside the agent
// loop). Empty text and a nil AttachmentSink are silent no-ops. URL
// upsert failures are warn-logged and per-URL — one bad URL does not
// abort the rest of the scan or the turn.
func (g *Gateway) scanAndSaveLinks(ctx context.Context, us *UserState, sessionID, messageID uuid.UUID, source, text string) {
	if g.deps.AttachmentSink == nil || us == nil || us.UserID == uuid.Nil || us.SoulID == uuid.Nil {
		return
	}
	if text == "" {
		return
	}
	urls := attachment.ExtractURLs(text)
	if len(urls) == 0 {
		return
	}
	for _, u := range urls {
		if _, err := g.deps.AttachmentSink.SaveLink(ctx, bs.LinkParams{
			UserID:    us.UserID,
			SoulID:    us.SoulID,
			SessionID: sessionID,
			MessageID: messageID,
			URL:       u,
		}); err != nil {
			g.logger.Warn("link save failed",
				"chat_id", us.ChatID,
				"source", source,
				"url", u,
				"err", err,
			)
		}
	}
}

// hasHeavyContent reports whether a user-message payload is unsuited
// for the fast reflex tier. Two cases collapse to the same action:
//   1. Any image block — the codex provider's text-only serializer
//      silently drops image content, so reflex never sees the bytes
//      and either hallucinates a description or routes wrong.
//   2. Total text past ~16 KiB (≈ 4K tokens, double reflex's
//      MessageBudget) — the chatgpt.com codex endpoint returns a
//      misleading 400 ("expected a string, but got an object") on
//      inputs it can't handle. PDF and text-doc attachments inline
//      as a single huge text block in the daemon's /chat handler
//      and trip this on the first user turn that carries them.
//
// Either way the right move is to skip the fast tier and run cortex
// (claude-opus-4-8) directly — it has the context budget for the
// turn and would have been called via escalation anyway.
func hasHeavyContent(content any) bool {
	const heavyTextBytes = 16 << 10 // 16 KiB ≈ 4K tokens
	switch v := content.(type) {
	case string:
		return len(v) > heavyTextBytes
	case []bs.ContentBlock:
		textBytes := 0
		for _, b := range v {
			if b.Type == "image" {
				return true
			}
			if b.Type == "text" {
				textBytes += len(b.Text)
				if textBytes > heavyTextBytes {
					return true
				}
			}
		}
	}
	return false
}

// appendDocInline glues an attached document rendering onto whatever
// the user typed, separating with a blank line so the model sees two
// distinct passages rather than a wall of text. Empty existing text
// (a doc-only turn) skips the leading newlines.
func appendDocInline(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "\n\n" + addition
}

// getOrInitUser builds UserState for non-Telegram entry points
// (voice:owner legacy WS, ProcessInbound). The Telegram path uses
// getOrInitTelegramUser, which resolves through vaelum.bot_links and
// stamps the receiving bot onto UserState.
func (g *Gateway) getOrInitUser(ctx context.Context, chatID string) (*UserState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if us, ok := g.users[chatID]; ok {
		return us, nil
	}

	// Legacy voice:owner — single-tenant fallback used by ws.handleConnectionLegacy
	// when the device-token auth path is not configured. Resolved against
	// the public.user_profiles owner row so the dev/test setup keeps
	// working without a Vaelum membership graph.
	if chatID != "voice:owner" {
		return nil, fmt.Errorf("getOrInitUser: chatID %q not supported on multi-bot gateway (Telegram chats route through getOrInitTelegramUser)", chatID)
	}

	coreDB, err := g.deps.DB("ship")
	if err != nil {
		return nil, fmt.Errorf("core DB: %w", err)
	}
	var userID uuid.UUID
	if err := coreDB.GetContext(ctx, &userID,
		`SELECT id FROM user_profiles WHERE is_owner = true LIMIT 1`); err != nil {
		return nil, fmt.Errorf("voice transport: no owner in user_profiles: %w", err)
	}

	var soulID uuid.UUID
	if g.deps.ResolveSoul != nil {
		soulID, err = g.deps.ResolveSoul(ctx, userID)
		if err != nil {
			g.logger.Error("gateway: soul resolution failed (voice)",
				"chat_id", chatID, "user_id", userID.String(), "error", err)
			return nil, fmt.Errorf("resolve soul: %w", err)
		}
	}

	us := g.buildUserState(chatID, userID, soulID, true, nil, 0)
	g.users[chatID] = us
	g.logger.Info("initialized voice user", "chat_id", chatID, "user_id", userID.String())

	return us, nil
}

// buildUserState assembles a fresh UserState — common scaffolding shared by
// getOrInitUser (voice:owner) and getOrInitTelegramUser (Telegram).
// Does NOT register the entry in g.users or set up a debouncer — callers
// own those steps so each transport can choose its own response sink.
func (g *Gateway) buildUserState(chatID string, userID, soulID uuid.UUID, isOwner bool, bi *botInstance, tgChatID int64) *UserState {
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

	return &UserState{
		ChatID:   chatID,
		UserID:   userID,
		SoulID:   soulID,
		IsOwner:  isOwner,
		Registry: registry,
		Deps:     userDeps,
		bot:      bi,
		tgChatID: tgChatID,
	}
}

// getOrInitTelegramUser resolves a Telegram chat (received on bot bi) to
// its (user, soul) via the host-provided ResolveTelegramChat hook
// (typically a vaelum.bot_links lookup) and assembles a UserState bound
// to the receiving bot. Returns ErrTelegramChatUnpaired (or any error
// whose Is-chain reaches it) when the chat has not been paired yet — the
// caller runs the unpaired-chat policy via replyUnpaired.
func (g *Gateway) getOrInitTelegramUser(ctx context.Context, bi *botInstance, chatID string, tgChatID, tgUserID int64) (*UserState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if us, ok := g.users[chatID]; ok {
		// Cached UserState retains its original bot binding even if a
		// later message arrives on a different bot — same chat_id across
		// bots should never happen in practice (Telegram chat IDs are
		// globally unique), but rebind defensively so outbound sends use
		// the bot the user most recently pinged.
		if bi != nil {
			us.bot = bi
		}
		return us, nil
	}

	if g.deps.ResolveTelegramChat == nil {
		return nil, fmt.Errorf("gateway: ResolveTelegramChat hook not configured")
	}

	userID, soulID, err := g.deps.ResolveTelegramChat(ctx, bi.id, tgChatID)
	if err != nil {
		if errors.Is(err, bs.ErrTelegramChatUnpaired) {
			return nil, bs.ErrTelegramChatUnpaired
		}
		g.logger.Warn("gateway: ResolveTelegramChat failed",
			"chat_id", chatID, "bot_id", bi.id.String(), "error", err)
		return nil, err
	}

	us := g.buildUserState(chatID, userID, soulID, false, bi, tgChatID)
	us.debounce = newDebouncer(g.deps.Config.Gateway.DebounceWindow, g.deps.Config.Gateway.DebounceCap, func(msgs []pendingMsg) {
		sink := g.newTelegramSink(chatID, bi)
		go g.processMessages(ctx, us, msgs, sink)
	})

	g.users[chatID] = us
	g.logger.Info("initialized telegram user",
		"chat_id", chatID,
		"bot_id", bi.id.String(),
		"user_id", userID.String(),
		"soul_id", soulID.String(),
	)
	return us, nil
}

// replyUnpaired runs the policy for a Telegram message from a chat the
// host has not paired yet:
//   - platform bot: greet + signup link (drives signups);
//   - user bot:     silent — only the owner is meant to talk to it, and
//                   we don't want to leak that this token belongs to
//                   someone in particular.
//
// The greeting text lives in <Config.Prompts>/telegram_platform_greeting.md
// so it can be edited without redeploying the binary; missing file falls
// back to a minimal in-code line.
func (g *Gateway) replyUnpaired(ctx context.Context, bi *botInstance, chatID string) {
	if bi == nil || bi.client == nil {
		return
	}
	if bi.kind == "user" {
		g.logger.Info("gateway: dropping message on unpaired user-bot chat",
			"bot_id", bi.id.String(), "chat_id", chatID)
		return
	}
	g.platformGreetMu.Lock()
	greeting := g.platformGreet
	g.platformGreetMu.Unlock()
	if greeting == "" {
		greeting = "I don't know you yet — type /start to get going."
	}
	if err := bi.client.SendLong(ctx, tgChatID(chatID), greeting); err != nil {
		g.logger.Warn("gateway: send greeting failed", "error", err, "chat_id", chatID)
	}
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
//
// LEGACY: this assumes a single-owner ArleneKateBot deployment. In the
// multi-bot Vaelum world there is no platform-wide "owner", so the
// function is a no-op until a per-soul error-notification channel is
// designed. Restoration: route through the owner's bot via
// (owner.bot != nil ? owner.bot : g.anyBot()).
func (g *Gateway) notifyOwnerError(ctx context.Context, source string, err error) {
	if !g.deps.Config.Gateway.Debug {
		return
	}
	owner := g.GetOwnerUser()
	if owner == nil || owner.bot == nil {
		return
	}
	msg := fmt.Sprintf("[%s] %v", source, err)
	sink := g.newTelegramSink(owner.ChatID, owner.bot)
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
// users (Vaelum web chat, native device voice). The caller has already
// authenticated the user and resolved the soul, so this bypasses chatID
// resolution and the owner-only gate that getOrInitUser enforces for the
// Telegram path.
//
// `transport` distinguishes parallel sessions for the same (user, soul):
// "vaelum" for the web chat, "voice" for the device WS. Each transport
// gets its own cached UserState (separate sink, independent in-flight
// loop) — the chat session and AME memory are shared because they key
// on (user, soul), not on transport.
//
// Audio in InboundMessage is transcribed via Whisper before dispatch;
// text-only callers (httpchat) are unaffected — empty Audio = no-op.
func (g *Gateway) ProcessInboundForUser(ctx context.Context, userID, soulID uuid.UUID, transport string, messages []bs.InboundMessage, sink bs.ResponseSink) error {
	us, err := g.getOrInitPlatformUser(ctx, userID, soulID, transport)
	if err != nil {
		return fmt.Errorf("init platform user: %w", err)
	}

	// Transcribe audio if present (same loop as the legacy ProcessInbound).
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
			text:             m.Text,
			images:           m.Images,
			replyToMessageID: m.ReplyToMessageID,
		})
	}
	if len(pending) == 0 {
		return nil
	}

	g.processMessages(ctx, us, pending, sink)
	return nil
}

// PersistInterruptedForUser is the soul-aware counterpart to
// PersistInterrupted — the barge-in voice path uses this when a turn is
// cancelled mid-stream so the partial assistant reply persists against
// the authenticated (user, soul), with no chatID round-trip.
func (g *Gateway) PersistInterruptedForUser(_ context.Context, userID, soulID uuid.UUID, transport, partial string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	us, err := g.getOrInitPlatformUser(ctx, userID, soulID, transport)
	if err != nil {
		g.logger.Warn("persist interrupted: init platform user failed", "error", err)
		return
	}
	ctx = bs.WithSoulID(ctx, us.SoulID)

	us.Mu.Lock()
	defer us.Mu.Unlock()

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Warn("persist interrupted: session failed", "error", err)
		return
	}

	text := strings.TrimSpace(partial)
	if text == "" {
		text = "[прервано пользователем]"
	} else {
		text += " […прервано]"
	}
	if err := g.store.Append(ctx, sess.ID, bs.Message{
		Role:    "assistant",
		Content: []bs.ContentBlock{{Type: "text", Text: text}},
	}); err != nil {
		g.logger.Warn("persist interrupted: append failed", "error", err)
	}
}

// getOrInitWebUser is retained as a thin wrapper for backward
// compatibility with the "vaelum" web chat transport.
func (g *Gateway) getOrInitWebUser(ctx context.Context, userID, soulID uuid.UUID) (*UserState, error) {
	return g.getOrInitPlatformUser(ctx, userID, soulID, "vaelum")
}

// getOrInitPlatformUser builds (or reuses) a UserState for an
// authenticated platform user. Unlike getOrInitUser it does not consult
// user_profiles and does not apply the owner gate — Vaelum is the
// authentication boundary for platform users, and the soul is supplied
// by the caller (resolved from vaelum.memberships on the platform side
// or vaelum.devices on the device path).
//
// transport is the chatID prefix ("vaelum", "voice", …) so two transports
// for the same (user, soul) maintain independent UserStates.
func (g *Gateway) getOrInitPlatformUser(ctx context.Context, userID, soulID uuid.UUID, transport string) (*UserState, error) {
	if transport == "" {
		transport = "vaelum"
	}
	chatID := transport + ":" + userID.String()

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

	// Attachment-reference resolution: if the user pasted an
	// attachment UUID into their message ("прочти файл abc-…", "что
	// на картинке abc-…"), look it up via the host's AttachmentSink
	// and inline the bytes — image as a vision block, pdf/text as a
	// fenced inline. Cortex then sees the file as if the user had
	// just attached it, no tool roundtrip required. Non-matching
	// UUIDs (foreign id, plain UUIDs the user happens to mention)
	// are silently passed through.
	blocks = g.resolveInlineAttachmentRefs(ctx, us, blocks)

	// Reply-parent attachment expansion (cabinet path only — the
	// frontend hands us a uuid for replyToMessageID before session
	// creation). When the user replies to an older message that
	// carried images / files, pull those parent attachments back
	// into the current turn so cortex can see them, not just the
	// inline `[reply to:…snippet…]` prefix. Telegram replies take
	// a separate code path (replyToTGMessageID needs a session-id
	// lookup) and aren't covered here yet.
	if g.deps.AttachmentSink != nil && len(msgs) > 0 && msgs[0].replyToMessageID != "" {
		parentID, perr := uuid.Parse(msgs[0].replyToMessageID)
		if perr == nil {
			attIDs, lerr := g.deps.AttachmentSink.ListForMessage(ctx, us.UserID, us.SoulID, parentID)
			if lerr != nil {
				g.logger.Warn("reply-attachments: list failed",
					"chat_id", us.ChatID, "parent_id", parentID, "err", lerr)
			} else if len(attIDs) > 0 {
				parentBlocks := g.attachmentBlocksByIDs(ctx, us, attIDs, "reply-attached")
				if len(parentBlocks) > 0 {
					// Prepend so the parent's files arrive before the
					// user's typed text — cortex parses the context
					// "this file ← my reply about it" in order.
					blocks = append(parentBlocks, blocks...)
					g.logger.Info("reply-attachments: inlined parent files",
						"chat_id", us.ChatID, "parent_id", parentID, "count", len(parentBlocks))
				}
			}
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

	// Push raw attachment bytes to the host CDN once the session id
	// is known. The cabinet history endpoint joins on session_id, so
	// stamping here is what makes a Telegram-originated photo / PDF
	// show up as a chip in the web UI. Failures are warn-and-continue
	// — the LLM still has the bytes via chat_messages, the user just
	// loses the cabinet chip.
	var sessionUUID uuid.UUID
	if g.deps.AttachmentSink != nil {
		sessID, perr := uuid.Parse(sess.ID)
		if perr != nil {
			g.logger.Warn("attachment sink: session id parse failed",
				"session_id", sess.ID, "err", perr)
		} else {
			sessionUUID = sessID
			for _, m := range msgs {
				for _, a := range m.rawAttachments {
					if len(a.data) == 0 {
						continue
					}
					if _, aerr := g.deps.AttachmentSink.Save(ctx, bs.AttachmentParams{
						UserID:    us.UserID,
						SoulID:    us.SoulID,
						SessionID: sessID,
						Name:      a.name,
						Mime:      a.mime,
						Kind:      a.kind,
						Data:      a.data,
					}); aerr != nil {
						g.logger.Warn("attachment sink save failed",
							"chat_id", us.ChatID, "name", a.name, "kind", a.kind, "err", aerr)
					}
				}
			}
		}
	}

	// Auto-extract pasted URLs from the user's text and persist them as
	// kind='link' attachment rows. The OG worker (arlene daemon)
	// enriches the row with og:title / og:description / og:image_url
	// asynchronously; the cabinet's Links tab + per-message chip
	// rendering pick the row up either way (empty OG = favicon
	// fallback). message_id stays NULL on the user side because the
	// user's chat_messages row is appended later inside the agent loop;
	// the link still surfaces in the cabinet's session-scoped view via
	// session_id.
	g.scanAndSaveLinks(ctx, us, sessionUUID, uuid.Nil, "user", msgText)

	// Wire the session ID into a meta SSE frame so a vaelum-style relayer
	// can begin attributing persisted tool_calls before any tool fires.
	// Sinks that don't implement MetaSink (voice, Telegram) get no-op.
	if ms, ok := sink.(bs.MetaSink); ok {
		_ = ms.SendMeta(ctx, sess.ID, "")
	}

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

	// rp carries the structured reflex pipeline output so we can both feed
	// the agent loop (InjectedCtx / ReflexGuidance / PostActions) and
	// surface MemoriesCount + MatchedRules + Strategy to the web cabinet
	// via a SendContextInfo frame.
	var rp reflexPipelineResult
	if reflexGuidance == "" && msgText != "" && us.Deps != nil && us.Deps.ReflexPreparer != nil && g.reflexModel() != "" {
		// Reflex/Cortex pipeline: structured context → reflex plan → pre-actions → filtered cortex input.
		rp = g.runReflexPipeline(ctx, us, msgText, priorContext)
		if rp.Silent {
			// Hard rule said "do not respond". Abort the whole turn — no
			// cortex call, no message sent, no post-actions, no debug dump.
			return
		}
		injectedCtx = rp.InjectedCtx
		reflexGuidance = rp.ReflexGuidance
		postActions = rp.PostActions
		preTraces = rp.PreTraces
		engineRuleCount = rp.EngineRuleCount
	} else if msgText != "" && us.Deps != nil && us.Deps.ContextInjector != nil {
		// Fallback: legacy ContextInjector (no reflex).
		injectedCtx = us.Deps.ContextInjector(ctx, us.UserID.String(), msgText, priorContext)
	}

	// Surface the prepared context (AME memories + rule matches + AME
	// strategy) to streaming sinks so the web cabinet can render a
	// "🧠 N memories • M rules" chip on the assistant bubble before
	// any text/tool_use frames arrive. Sinks that don't implement
	// ContextInfoSink (voice, Telegram) silently skip this.
	if ci, ok := sink.(bs.ContextInfoSink); ok {
		_ = ci.SendContextInfo(ctx, bs.ContextInfo{
			Memories: rp.MemoriesCount,
			// MatchedRules already covers engine + reflex matches (the
			// dedup happens inside runReflexPipeline via seenRuleIDs).
			Rules:        len(rp.MatchedRules),
			MatchedRules: rp.MatchedRules,
			Strategy:     rp.Strategy,
		})
	}

	// Per-turn tool registry: the soul's native tools plus a fresh
	// snapshot of its connected MCP-server tools. The cached native
	// registry is cloned only when there are MCP tools to add — the
	// common no-MCP case reuses it untouched.
	turnRegistry := us.Registry
	if g.deps.Config.MCPSource != nil {
		if mcpTools := g.deps.Config.MCPSource.ToolsForSoul(ctx, us.SoulID); len(mcpTools) > 0 {
			turnRegistry = us.Registry.Clone()
			for _, t := range mcpTools {
				turnRegistry.RegisterRemote(t.Name, t.Description, t.Schema, bs.ToolModeSync, "mcp", t.Handler)
			}
		}
	}

	// The chat loop runs without a compactor: compaction deletes messages,
	// and chat history is permanent. The loop windows the context itself
	// (MessagesForAPI); older turns are recalled associatively by AME, not
	// by a linear summary.
	loop := agent.NewLoop(g.provider, g.store, turnRegistry, g.deps.RoleTools, g.deps.Config, g.logger)

	// Resolve the soul's full system prompt from the database (platform
	// preamble + persona + agents) and stamp the current datetime so the
	// model always knows "today". A soul with no persona row is a
	// misconfiguration — abort the turn loudly rather than answer with the
	// wrong identity.
	now := time.Now().In(g.tz)
	soulPrompt, err := g.systemPromptForSoul(ctx, us.SoulID)
	if err != nil {
		g.logger.Error("cortex: cannot resolve system prompt, aborting turn",
			"soul_id", us.SoulID.String(), "chat_id", us.ChatID, "error", err)
		return
	}
	systemWithTime := fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), soulPrompt)

	// Interaction tier needs a focused system prompt (preamble + persona, no
	// cortex agents layer) AND a registry subset so the fast tier cannot
	// execute cortex tools even if it hallucinates a call. Both computed
	// here so they can be passed to runInteraction.
	var (
		reflexSystem string
		reflexLoop   *agent.Loop
	)
	if g.deps.Config.Gateway.InteractionTier && g.reflexInteractionPrompt != "" {
		rp, rerr := g.reflexSystemPromptForSoul(ctx, us.SoulID)
		if rerr != nil {
			g.logger.Error("reflex: cannot resolve system prompt, aborting turn",
				"soul_id", us.SoulID.String(), "chat_id", us.ChatID, "error", rerr)
			return
		}
		reflexSystem = fmt.Sprintf("[current_datetime: %s]\n\n%s\n\n%s",
			now.Format("2006-01-02 15:04 MST (Monday)"), rp, g.reflexInteractionPrompt)
		reflexRegistry := turnRegistry.SubsetForNames([]string{tool.ToolEscalate})
		reflexLoop = agent.NewLoop(g.provider, g.store, reflexRegistry, g.deps.RoleTools, g.deps.Config, g.logger)
	}

	var cortexRef bs.ModelRef
	if g.deps.ModelStore != nil {
		cortexRef = g.deps.ModelStore.Get("cortex")
	}
	cortexTemp := cortexRef.Temperature
	// Per-role max_tokens wins when configured. Adaptive thinking at high/xhigh
	// effort counts thinking toward max_tokens, so cortex needs a larger cap
	// than the global default or replies truncate (stop_reason=max_tokens).
	cortexMaxTokens := g.deps.Config.Limits.MaxOutputTokens
	if cortexRef.MaxTokens > 0 {
		cortexMaxTokens = cortexRef.MaxTokens
	}

	// Reply metadata: the first pendingMsg in the batch carries
	// the user-visible reply target. Cabinet-originated replies set
	// replyToMessageID directly (the frontend knows the parent
	// uuid). Telegram-originated replies set replyToTGMessageID and
	// we resolve it via the session store's tg_message_id index.
	// TGMessageID of the inbound message itself is stamped on the
	// new row so future Telegram replies pointing at it can be
	// resolved the same way.
	var replyToMessageID string
	var tgMessageID int64
	if len(msgs) > 0 {
		first := msgs[0]
		if first.messageID != 0 {
			tgMessageID = int64(first.messageID)
		}
		if first.replyToMessageID != "" {
			replyToMessageID = first.replyToMessageID
		} else if first.replyToTGMessageID != 0 {
			if parentID, lerr := g.store.LookupByTGMessageID(ctx, sess.ID, int64(first.replyToTGMessageID)); lerr == nil {
				replyToMessageID = parentID
			} else {
				g.logger.Warn("reply: tg parent lookup failed",
					"session_id", sess.ID, "tg_id", first.replyToTGMessageID, "err", lerr)
			}
		}
	}

	runCfg := agent.RunConfig{
		SessionID:        sess.ID,
		SystemPrompt:     systemWithTime,
		CompactSummary:   derefString(sess.CompactSummary),
		Model:            g.cortexModel(),
		MaxTokens:        cortexMaxTokens,
		MaxTurns:         g.deps.Config.Gateway.MaxTurns,
		InjectedContext:  injectedCtx,
		ReflexGuidance:   reflexGuidance,
		Role:             "cortex",
		Temperature:      cortexTemp,
		ThinkingMode:     cortexRef.ThinkingMode,
		Effort:           cortexRef.Effort,
		AllowedTools:     g.allowedToolsForSoul(ctx, us.SoulID, turnRegistry),
		ReplyToMessageID: replyToMessageID,
		TGMessageID:      tgMessageID,
	}

	// Voice transport: use streaming LLM with inline sentence-level TTS.
	// Each sentence is TTS'd and sent as an audio chunk as soon as the LLM produces it.
	streamSink, isStreaming := sink.(bs.StreamingVoiceSink)
	if isStreaming && g.deps.Config.TTS != nil {
		var sentenceBuf strings.Builder
		chunkSeq := 0
		onTextCalls := 0

		cfg := g.deps.Config
		voice := cfg.TTSVoice
		var instruct string
		if cfg.TTSInstructMapper != nil {
			instruct = cfg.TTSInstructMapper(us.LastStrategy)
		}
		// Stream path uses the default Synthesize (OGG/Opus from ElevenLabs).
		// The legacy SynthesizeMP3 preference existed because macOS arlene-
		// voice couldn't decode OGG — that client is gone, and per-chunk MP3
		// has a ~150 ms decoder warmup on Android which manifests as the
		// "first word swallowed" on every TTS chunk. Opus warms in ~5 ms.
		synthesize := cfg.TTS.Synthesize

		// Barge-in: notify a spoken-text-aware sink of each streamed chunk so
		// it can track what the assistant is currently saying.
		noter, _ := sink.(bs.SpokenTextSink)

		// First chunk emits on any low-latency boundary (sentence end OR a
		// comma/dash after ≥ 6 chars) so the user hears Arlene within ~1 s
		// of the LLM starting to stream. Later chunks demand a full
		// sentence-end so the playback doesn't sound choppy.
		//
		// IMPORTANT: TTSTextCleaner is applied AFTER we extract a complete
		// sentence (in the synth call sites below), NOT per chunk. Many of
		// its regexes are multi-character patterns — kaomoji like "(^_^)"
		// and "(≧▽≦)", "**bold**", "_italic_" — that arrive token-by-token
		// in the stream. If we cleaned each chunk in isolation the regex
		// would never see the full pattern, so kaomoji slipped through to
		// the TTS engine and got read out as literal noise. The sentence
		// boundary is the first place we have the full text.
		var firstChunkEmitted bool
		onText := func(chunk string) {
			if noter != nil {
				noter.NoteSpokenText(chunk)
			}
			onTextCalls++
			sentenceBuf.WriteString(chunk)
			text := sentenceBuf.String()

			// Sentence-end delimiters — always cut here.
			delims := []string{". ", "! ", "? ", ".\n", "!\n", "?\n"}
			minIdx := 10
			// Until the first audio has gone out, also accept a comma /
			// dash / colon as a cut point so reflex's «Секунду,» turns
			// into audio immediately instead of waiting for the period.
			if !firstChunkEmitted {
				delims = append(delims, ", ", " — ", ": ", ",\n")
				minIdx = 6
			}
			// Pick the EARLIEST valid split point across all delims (was
			// LastIndex per delim, which collapsed multi-sentence bursts
			// from a fast LLM into a single TTS call — the first sentence
			// then waited for the rest of the buffer to land instead of
			// streaming out as it became available).
			bestIdx := -1
			bestDelimLen := 0
			for _, delim := range delims {
				idx := strings.Index(text, delim)
				if idx < minIdx {
					continue
				}
				if bestIdx < 0 || idx < bestIdx {
					bestIdx = idx
					bestDelimLen = len(delim)
				}
			}
			if bestIdx < 0 {
				return
			}
			sentence := strings.TrimSpace(text[:bestIdx+1])
			sentenceBuf.Reset()
			sentenceBuf.WriteString(text[bestIdx+bestDelimLen:])
			if cfg.TTSTextCleaner != nil {
				sentence = cfg.TTSTextCleaner(sentence)
			}
			if sentence == "" {
				return
			}

			chunkSeq++
			seq := chunkSeq
			audio, err := synthesize(ctx, sentence, voice, instruct)
			if err != nil {
				g.logger.Warn("tts: stream chunk failed", "error", err, "sentence_len", len(sentence))
				return
			}
			g.logger.Info("tts: stream chunk ok", "seq", seq, "audio_bytes", len(audio), "sentence_len", len(sentence), "first", !firstChunkEmitted)
			if werr := streamSink.SendVoiceChunk(ctx, audio, seq, false); werr != nil {
				g.logger.Warn("tts: send chunk failed", "error", werr, "seq", seq)
			}
			firstChunkEmitted = true
		}

		// reflexFlush emits whatever the reflex tier has spoken so far as an
		// audio chunk. Called by runInteraction the instant the reflex stream
		// ends so the user hears the filler ("щас гляну…") immediately, before
		// cortex starts thinking — the interaction-model point. On a simple
		// turn (no escalation) it also sends the answer slightly earlier than
		// the post-loop flush would.
		reflexFlush := func() {
			remaining := strings.TrimSpace(sentenceBuf.String())
			if remaining == "" {
				return
			}
			sentenceBuf.Reset()
			if cfg.TTSTextCleaner != nil {
				remaining = cfg.TTSTextCleaner(remaining)
			}
			if remaining == "" {
				return
			}
			chunkSeq++
			seq := chunkSeq
			audio, terr := synthesize(ctx, remaining, voice, instruct)
			if terr != nil {
				g.logger.Warn("tts: filler synth failed", "error", terr, "text_len", len(remaining))
				return
			}
			g.logger.Info("tts: filler sent", "seq", seq, "audio_bytes", len(audio), "text_len", len(remaining))
			if werr := streamSink.SendVoiceChunk(ctx, audio, seq, false); werr != nil {
				g.logger.Warn("tts: send filler chunk failed", "error", werr, "seq", seq)
			}
		}

		voiceCb := &bs.StreamCallbacks{OnText: onText}
		reply, _, _, err := g.runInteraction(ctx, loop, reflexLoop, runCfg, reflexSystem, content, voiceCb, voiceCb, reflexFlush)
		if err != nil {
			if ctx.Err() != nil {
				g.logger.Info("voice turn cancelled", "chat_id", us.ChatID)
				return
			}
			g.logger.Error("agent loop error", "chat_id", us.ChatID, "error", err)
			g.sendDebugError(ctx, sink, "agent", err)
			return
		}

		// Flush remaining text as final chunk.
		remaining := strings.TrimSpace(sentenceBuf.String())
		g.logger.Info("voice: stream done, flushing",
			"on_text_calls", onTextCalls,
			"chunks_streamed", chunkSeq,
			"remaining_len", len(remaining),
			"reply_len", len(reply),
		)
		if remaining != "" {
			if cfg.TTSTextCleaner != nil {
				remaining = cfg.TTSTextCleaner(remaining)
			}
		}
		if remaining != "" {
			chunkSeq++
			audio, terr := synthesize(ctx, remaining, voice, instruct)
			if terr != nil {
				g.logger.Warn("tts: flush synth failed", "error", terr, "text_len", len(remaining))
			} else {
				g.logger.Info("tts: flush sent", "seq", chunkSeq, "audio_bytes", len(audio))
				if werr := streamSink.SendVoiceChunk(ctx, audio, chunkSeq, true); werr != nil {
					g.logger.Warn("tts: send flush chunk failed", "error", werr)
				}
			}
		} else if chunkSeq > 0 {
			// Mark last sent chunk as final (re-send empty final marker).
			if werr := streamSink.SendVoiceChunk(ctx, nil, chunkSeq, true); werr != nil {
				g.logger.Warn("tts: send final marker failed", "error", werr)
			}
		}

		// Also send text for logging
		if reply != "" {
			sink.SendText(ctx, reply)
		}
		if reply != "" && len(postActions) > 0 {
			g.executePostActions(ctx, us, postActions, reply)
		}
		if reply != "" {
			// Voice symmetry — if Arlene happens to read a URL (uncommon
			// but not impossible on the JS-fetch-this branch), persist it
			// as a link chip so it surfaces in the cabinet.
			var assistantMsgID uuid.UUID
			if msgID, lookupErr := g.store.LatestAssistantMessageID(ctx, sess.ID); lookupErr == nil && msgID != "" {
				if parsed, perr := uuid.Parse(msgID); perr == nil {
					assistantMsgID = parsed
				}
			}
			g.scanAndSaveLinks(ctx, us, sessionUUID, assistantMsgID, "assistant", reply)
			g.emitTurnCompleted(us, sess)
		}
		return
	}

	// Streaming path for Telegram: send placeholder, edit as chunks arrive.
	tgSink, isTelegram := sink.(*telegramSink)
	if !isTelegram {
		// Non-Telegram transports: text-streaming if sink supports it
		// (http-chat SSE for the web cabinet), batch otherwise. Both
		// reflex and cortex use the same callback so reflex-only turns
		// (where reflex answers without escalating) still stream their
		// reply — otherwise the web cabinet would see an empty bubble
		// and fall back to the "(нет ответа)" stalled placeholder
		// because the reply text never reaches a SendText call.
		cortexCb := buildSinkCallbacks(ctx, sink)
		reply, cortexTraces, _, err := g.runInteraction(ctx, loop, reflexLoop, runCfg, reflexSystem, content, cortexCb, cortexCb, nil)
		if err != nil {
			if ctx.Err() != nil {
				g.logger.Info("turn cancelled", "chat_id", us.ChatID)
				return
			}
			g.logger.Error("agent loop error", "chat_id", us.ChatID, "error", err)
			g.sendDebugError(ctx, sink, "agent", err)
			return
		}
		reply = sanitizeLeakedToolCalls(reply)
		reply = g.dispatchAttachmentMarkers(ctx, us, sink, reply)
		// Emit a follow-up meta frame with the assistant message_id so the
		// SSE relayer can link any persisted tool_calls back to that turn.
		// Only when both sink + store support it; safe to skip otherwise.
		var assistantMsgID uuid.UUID
		if msgID, lookupErr := g.store.LatestAssistantMessageID(ctx, sess.ID); lookupErr == nil && msgID != "" {
			if parsed, perr := uuid.Parse(msgID); perr == nil {
				assistantMsgID = parsed
			}
			if ms, ok := sink.(bs.MetaSink); ok {
				_ = ms.SendMeta(ctx, sess.ID, msgID)
			}
		}
		// Auto-extract URLs from Arlene's final reply and persist them
		// as kind='link' rows pinned to the assistant message_id, so the
		// cabinet can highlight which bubble produced the chip. Mirror
		// of the user-side scan above.
		g.scanAndSaveLinks(ctx, us, sessionUUID, assistantMsgID, "assistant", reply)
		// For text-streaming sinks (web SSE) the reply has already been
		// delivered chunk-by-chunk via cb.OnText; calling SendText again
		// here would duplicate the whole response in the rendered bubble.
		if reply != "" {
			if len(postActions) > 0 {
				g.executePostActions(ctx, us, postActions, reply)
			}
			if _, isStream := sink.(bs.TextStreamSink); !isStream {
				sink.SendText(ctx, reply)
			}
			g.emitTurnCompleted(us, sess)
		}
		// LEGACY: sendDebugDump — per-turn debug.md attachment for the
		// owner. Parked with the rest of the legacy commands; restore in
		// concert with the /debug toggle.
		// if us.DebugMode || g.deps.Config.Gateway.Debug {
		// 	go g.sendDebugDump(ctx, us, injectedCtx, reflexGuidance, msgText, preTraces, cortexTraces, engineRuleCount)
		// }
		_ = injectedCtx
		_ = reflexGuidance
		_ = engineRuleCount
		_ = cortexTraces
		return
	}

	// Telegram streaming: progressive message editing.
	var (
		streamMsgID int // Telegram message ID for edits
		streamBuf   strings.Builder
		lastEdit    time.Time
		toolStatus  string // current tool being executed
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
		if tgSink.client != nil {
			tgSink.client.EditMessageText(ctx, tgSink.chatID, streamMsgID, text, nil)
		}
		lastEdit = time.Now()
	}

	// ensureMsg creates the Telegram message on first content (text or tool).
	ensureMsg := func(text string) {
		if streamMsgID != 0 || tgSink.client == nil {
			return
		}
		res, err := tgSink.client.SendMessage(ctx, fmt.Sprintf("%d", tgSink.chatID), text)
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

	tgCb := &bs.StreamCallbacks{
		OnText:    onText,
		OnToolUse: func(_, name string, _ json.RawMessage) { onToolUse(name) },
	}
	reply, cortexTraces, _, err := g.runInteraction(ctx, loop, reflexLoop, runCfg, reflexSystem, content, nil, tgCb, nil)
	if err != nil {
		if ctx.Err() != nil {
			g.logger.Info("turn cancelled", "chat_id", us.ChatID)
			return
		}
		g.logger.Error("agent loop error", "chat_id", us.ChatID, "error", err)
		g.sendDebugError(ctx, sink, "agent", err)
		return
	}

	reply = sanitizeLeakedToolCalls(reply)
	reply = g.dispatchAttachmentMarkers(ctx, us, sink, reply)
	if reply == "" {
		return
	}

	// Auto-extract URLs from Arlene's reply on the Telegram path too —
	// the assistant chip then surfaces in the cabinet's Links tab even
	// when the user is on mobile. Mirror of the non-Telegram branch
	// above; here we look up the assistant message id directly because
	// the meta-sink path doesn't run on Telegram.
	{
		var assistantMsgID uuid.UUID
		if msgID, lookupErr := g.store.LatestAssistantMessageID(ctx, sess.ID); lookupErr == nil && msgID != "" {
			if parsed, perr := uuid.Parse(msgID); perr == nil {
				assistantMsgID = parsed
			}
		}
		g.scanAndSaveLinks(ctx, us, sessionUUID, assistantMsgID, "assistant", reply)
	}

	// Clear tool status for final message
	mu.Lock()
	toolStatus = ""
	mu.Unlock()

	if streamMsgID != 0 && tgSink.client != nil {
		// Final edit with complete text
		tgSink.client.EditMessageText(ctx, tgSink.chatID, streamMsgID, reply, nil)
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

	// LEGACY: sendDebugDump — see the dispatch-side comment in handleUpdate.
	// if us.DebugMode || g.deps.Config.Gateway.Debug {
	// 	go g.sendDebugDump(ctx, us, injectedCtx, reflexGuidance, msgText, preTraces, cortexTraces, engineRuleCount)
	// }
	_ = injectedCtx
	_ = reflexGuidance
	_ = engineRuleCount
	_ = preTraces
	_ = cortexTraces

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

// telegramSink implements bs.ResponseSink for Telegram transport. Each
// instance is bound to one bot's client at construction so replies land
// on the bot the user pinged (file IDs, callbacks, and inline keyboards
// are all bot-scoped in Telegram, so sends must use the same client).
type telegramSink struct {
	gw     *Gateway
	chatID int64
	client *telegram.Client // never nil in practice; set by newTelegramSink
}

// newTelegramSink builds a sink for one Telegram chat on one bot. bi must
// be non-nil for the sink to actually send anything — the legacy single-bot
// fallback path keeps bi pointing at the cfg.Transport.BotToken bot.
func (g *Gateway) newTelegramSink(canonicalChatID string, bi *botInstance) *telegramSink {
	var client *telegram.Client
	if bi != nil {
		client = bi.client
	}
	return &telegramSink{gw: g, chatID: tgChatID(canonicalChatID), client: client}
}

func (s *telegramSink) SendText(ctx context.Context, text string) error {
	if s.client == nil {
		return fmt.Errorf("telegramSink.SendText: no telegram client (chat %d)", s.chatID)
	}
	return s.client.SendLong(ctx, s.chatID, text)
}

func (s *telegramSink) SendVoice(ctx context.Context, audio []byte) error {
	chatID := fmt.Sprintf("%d", s.chatID)
	if s.gw.deps.Sender != nil {
		return s.gw.deps.Sender.SendVoice(ctx, chatID, audio)
	}
	return fmt.Errorf("telegramSink.SendVoice: no MessageSender configured")
}

func (s *telegramSink) SendTyping(ctx context.Context) error {
	if s.client == nil {
		return nil
	}
	return s.client.SendChatAction(ctx, s.chatID, "typing")
}

// SendAttachment implements bs.AttachmentSendSink. Routes by kind:
// images go through SendPhoto so TG renders the gallery preview,
// PDFs and text-shaped docs go through SendDocument so the chat
// shows a file icon with the filename. Unknown kinds fall back to
// SendDocument — a download is always better than nothing.
func (s *telegramSink) SendAttachment(ctx context.Context, rec bs.AttachmentRecord, data []byte) error {
	if s.client == nil {
		return fmt.Errorf("telegramSink.SendAttachment: no telegram client (chat %d)", s.chatID)
	}
	chatID := fmt.Sprintf("%d", s.chatID)
	name := rec.Name
	if name == "" {
		name = "file"
	}
	if rec.Kind == "image" {
		return s.client.SendPhoto(ctx, chatID, name, rec.Mime, data, "")
	}
	return s.client.SendDocument(ctx, chatID, name, rec.Mime, data)
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

// reflexPipelineResult groups everything runReflexPipeline computes so the
// caller can wire the structured pieces (memories + matched rules +
// strategy) into the SSE context_info frame, while keeping the prompt
// glue strings the agent loop actually consumes (injectedCtx /
// reflexGuidance) in the same envelope.
type reflexPipelineResult struct {
	InjectedCtx     string
	ReflexGuidance  string
	PostActions     []bs.PostAction
	PreTraces       []agent.ToolTrace
	EngineRuleCount int
	MemoriesCount   int
	MatchedRules    []bs.MatchedRule
	Strategy        string
	Silent          bool
}

// runReflexPipeline executes the System 1/2 pipeline:
// 1. ReflexPreparer → structured context (traces + candidate rules)
// 2. Reflex LLM (Gemini Flash) → plan (matched rules, pre/post actions, tools)
// 3. Execute pre-actions (web_search etc.) → inject results into context
// 4. Build cortex context: matched rules + research + AME traces
//
// When result.Silent=true the caller MUST abort the turn without calling
// cortex or sending any output — a structured rule with Silent=true matched.
func (g *Gateway) runReflexPipeline(ctx context.Context, us *UserState, msgText, priorContext string) reflexPipelineResult {
	// Interaction tier: skip the ReflexPreparer entirely. The full AME pass
	// (memory_associate + scoring + diversity filter + emotion detection)
	// costs ~3-5 s per turn and the streaming reflex doesn't need it — for
	// chatty/social turns it answers from session history alone, and for
	// memory-needing turns it escalates to cortex which still has the full
	// chat history and all tools. We only run the structured rule engine
	// here (cheap; catches Silent rules and injects scope-based guidance).
	if g.deps.Config.Gateway.InteractionTier {
		var guidance strings.Builder
		hasRules := false
		engineRuleCount := 0
		var matchedRules []bs.MatchedRule
		if us.Deps.RuleEngine != nil {
			engineRules := us.Deps.RuleEngine(ctx, bs.RuleContext{
				UserID:  us.Deps.UserID.String(),
				Hour:    time.Now().Hour(),
				Message: msgText,
			})
			for _, r := range engineRules {
				if r.Silent {
					g.logger.Info("rule engine: silent rule matched, aborting turn",
						"rule_id", r.ID, "trigger", r.Trigger, "chat_id", us.ChatID)
					return reflexPipelineResult{Silent: true}
				}
			}
			for _, r := range engineRules {
				if !hasRules {
					guidance.WriteString("[active rules]\n")
					hasRules = true
				}
				fmt.Fprintf(&guidance, "WHEN: %s\nDO: %s\n\n", r.Trigger, r.Action)
				matchedRules = append(matchedRules, bs.MatchedRule{
					ID: r.ID, Trigger: r.Trigger, Action: r.Action, Source: "engine",
				})
			}
			engineRuleCount = len(engineRules)
			if engineRuleCount > 0 {
				g.logger.Info("rule engine matched (interaction tier)", "count", engineRuleCount)
			}
		}
		if hasRules {
			guidance.WriteString("[/active rules]")
		}
		return reflexPipelineResult{
			ReflexGuidance:  guidance.String(),
			EngineRuleCount: engineRuleCount,
			MatchedRules:    matchedRules,
		}
	}

	rc := us.Deps.ReflexPreparer(ctx, us.UserID.String(), msgText, priorContext)
	if rc == nil {
		return reflexPipelineResult{}
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
		return reflexPipelineResult{
			InjectedCtx:   rc.FullContext,
			MemoriesCount: rc.MemoriesCount,
			Strategy:      rc.Strategy,
		}
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
	var matchedRulesInfo []bs.MatchedRule

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
				matchedRulesInfo = append(matchedRulesInfo, bs.MatchedRule{
					ID: r.ID, Trigger: r.Trigger, Action: r.Action, Source: "reflex",
				})
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
				return reflexPipelineResult{Silent: true}
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
			matchedRulesInfo = append(matchedRulesInfo, bs.MatchedRule{
				ID: r.ID, Trigger: r.Trigger, Action: r.Action, Source: "engine",
			})

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

	return reflexPipelineResult{
		InjectedCtx:     formattedTraces,
		ReflexGuidance:  guidance.String(),
		PostActions:     reflexResult.PostActions,
		PreTraces:       preTraces,
		EngineRuleCount: engineRuleCount,
		MemoriesCount:   rc.MemoriesCount,
		MatchedRules:    matchedRulesInfo,
		Strategy:        rc.Strategy,
	}
}

// escalateArgs is the parsed input of an escalate tool call.
type escalateArgs struct {
	Reason         string   `json:"reason"`
	Guidance       string   `json:"guidance"`
	SuggestedTools []string `json:"suggested_tools"`
}

// findEscalate scans tool traces for an escalate call. Detection keys on the
// tool name (never truncated); args are parsed best-effort — a truncated trace
// input still escalates, just without the guidance hint.
func findEscalate(traces []agent.ToolTrace) *escalateArgs {
	for _, tr := range traces {
		if tr.Name != tool.ToolEscalate {
			continue
		}
		var a escalateArgs
		_ = json.Unmarshal([]byte(tr.Input), &a)
		return &a
	}
	return nil
}

// runInteraction runs a turn through the two-tier interaction model when
// Gateway.InteractionTier is enabled, falling back to a direct Cortex call
// otherwise. It replaces the bare loop call in each of processMessages'
// transport blocks.
//
// Interaction-tier flow: the user message is persisted once, then the Reflex
// tier streams a reply. If Reflex answers directly (no escalate tool call)
// that reply is the turn. If Reflex calls escalate, its streamed text was a
// short spoken filler — the Cortex tier then runs and its answer becomes the
// turn. reflexCb is nil for non-voice transports so a filler is never shown
// as text; cortexCb drives the transport's own streaming (text deltas,
// tool_use / tool_result / thinking events for sinks that surface them).
func (g *Gateway) runInteraction(
	ctx context.Context,
	loop *agent.Loop,
	reflexLoop *agent.Loop,
	cortexCfg agent.RunConfig,
	reflexSystemPrompt string,
	content any,
	reflexCb, cortexCb *bs.StreamCallbacks,
	onReflexDone func(),
) (reply string, traces []agent.ToolTrace, escalated bool, err error) {
	// Legacy path: interaction tier off, or the caller didn't wire it up
	// (missing prompt / reflex loop / reflex system prompt).
	if !g.deps.Config.Gateway.InteractionTier || g.reflexInteractionPrompt == "" ||
		reflexLoop == nil || reflexSystemPrompt == "" {
		if g.deps.Config.Gateway.InteractionTier {
			g.logger.Warn("interaction tier enabled but not fully wired — running cortex directly")
		}
		reply, traces, err = loop.RunStream(ctx, cortexCfg, content, cortexCb)
		return reply, traces, false, err
	}

	// Heavy-content bypass: the reflex tier (openai-codex gpt-5.5) is
	// sized for short routing turns and chokes on either image bytes
	// (silently dropped by its text-only serializer) or oversized
	// text inputs (codex backend returns a confusing 400). PDFs and
	// large text-doc attachments inline as one huge text block in the
	// daemon's /chat handler and trip the size case. See
	// hasHeavyContent for the precise rule. Skip reflex and run
	// cortex directly with the full content; persist the user turn
	// once, matching the interaction-tier append-once pattern below.
	if hasHeavyContent(content) {
		if err = g.store.Append(ctx, cortexCfg.SessionID, bs.Message{
			Role:             "user",
			Content:          content,
			ReplyToMessageID: cortexCfg.ReplyToMessageID,
			TGMessageID:      cortexCfg.TGMessageID,
		}); err != nil {
			return "", nil, false, fmt.Errorf("interaction: append user message (heavy bypass): %w", err)
		}
		cortexCfg.SkipUserAppend = true
		g.logger.Info("interaction: heavy content, bypassing reflex tier", "session_id", cortexCfg.SessionID)
		reply, traces, err = loop.RunStream(ctx, cortexCfg, content, cortexCb)
		return reply, traces, true, err
	}

	// Derive the Reflex (interaction-tier) config from the Cortex config:
	// same session / context / rules, but the fast model, the reflex role
	// (escalate-only tools) and the focused interaction-tier system prompt
	// (preamble + persona; no cortex agents/tools manual).
	reflexCfg := cortexCfg
	reflexCfg.Model = g.reflexModel()
	reflexCfg.Role = "reflex"
	reflexCfg.SystemPrompt = reflexSystemPrompt
	reflexCfg.MaxTurns = 1
	reflexCfg.Ephemeral = true
	reflexCfg.SkipUserAppend = true
	reflexCfg.MaxTokens = 0
	reflexCfg.Temperature = 0
	// Tight history window for the fast tier — routing/answer decisions need
	// the recent conversation, not the full session. ~4 K tokens ≈ last 15-25
	// messages, enough for short-term continuity; full context lives on the
	// cortex side when escalation happens.
	reflexCfg.MessageBudget = 4000
	// AllowedTools cleared — reflex's only tool is the system `escalate`
	// sentinel, which must not be dropped by the per-soul cabinet allowlist.
	reflexCfg.AllowedTools = nil
	if g.deps.ModelStore != nil {
		ref := g.deps.ModelStore.Get("reflex")
		reflexCfg.MaxTokens = ref.MaxTokens
		reflexCfg.Temperature = ref.Temperature
		// Per-role thinking budget: 0 in DB = disabled. -1 forces the
		// agent loop's chooseThinkingBudget to ignore the global default.
		// Without this, reflex inherited cortex's 4096-token thinking
		// budget and gemma4-nothinker (and any thinking-capable model)
		// burned 400-500 hidden reasoning tokens per turn — ~5-6 s of
		// pure latency burn on the voice path.
		if ref.ThinkingBudget > 0 {
			reflexCfg.ThinkingBudget = ref.ThinkingBudget
		} else {
			reflexCfg.ThinkingBudget = -1
		}
	}

	// Persist the user message once; both tiers read it, neither re-appends.
	if err = g.store.Append(ctx, cortexCfg.SessionID, bs.Message{Role: "user", Content: content}); err != nil {
		return "", nil, false, fmt.Errorf("interaction: append user message: %w", err)
	}

	// Reflex runs against an escalate-only registry subset — if it
	// hallucinates a cortex tool (memory_search etc.) the registry rejects
	// it as unknown instead of executing it for real.
	reflexReply, reflexTraces, rerr := reflexLoop.RunStream(ctx, reflexCfg, content, reflexCb)
	if rerr != nil {
		return "", reflexTraces, false, fmt.Errorf("interaction: reflex: %w", rerr)
	}

	// Voice transports flush the reflex's spoken text-so-far the instant the
	// stream ends, so the filler reaches the user BEFORE cortex starts.
	if onReflexDone != nil {
		onReflexDone()
	}

	esc := findEscalate(reflexTraces)
	if esc == nil && len(reflexTraces) > 0 {
		// Reflex tried to call a tool but not (or not only) escalate — it
		// hallucinated a cortex tool. Any tool intent from the fast tier
		// means "I need the deep tier", so escalate. The hallucinated tool
		// name goes into the reason for observability.
		esc = &escalateArgs{Reason: "fast tier requested a tool: " + reflexTraces[0].Name}
		g.logger.Info("interaction: reflex called non-escalate tool, treating as escalation",
			"tool", reflexTraces[0].Name)
	}
	if esc == nil {
		// Simple turn — Reflex answered it. Persist its reply as the turn.
		if strings.TrimSpace(reflexReply) != "" {
			if aerr := g.store.Append(ctx, cortexCfg.SessionID, bs.Message{
				Role:    "assistant",
				Content: []bs.ContentBlock{{Type: "text", Text: reflexReply}},
			}); aerr != nil {
				g.logger.Warn("interaction: persist reflex reply failed", "error", aerr)
			}
		}
		g.logger.Info("interaction: reflex answered, no escalation")
		return reflexReply, reflexTraces, false, nil
	}

	// Escalation — run the Cortex tier with the full registry. The user
	// message is already persisted; Cortex persists its own answer normally.
	g.logger.Info("interaction: escalating to cortex", "reason", truncateStr(esc.Reason, 120))
	cortexCfg.SkipUserAppend = true
	if esc.Guidance != "" {
		note := "[escalation note] " + esc.Guidance
		if cortexCfg.ReflexGuidance != "" {
			cortexCfg.ReflexGuidance = note + "\n\n" + cortexCfg.ReflexGuidance
		} else {
			cortexCfg.ReflexGuidance = note
		}
	}
	reply, traces, err = loop.RunStream(ctx, cortexCfg, content, cortexCb)
	return reply, traces, true, err
}

// buildSinkCallbacks composes a *bs.StreamCallbacks from whichever optional
// streaming interfaces the sink implements. Returns nil if the sink supports
// no streaming at all (batch mode — only the final aggregated text reaches
// ResponseSink.SendText after the loop returns).
//
// Sinks that participate:
//   - TextStreamSink → cb.OnText forwards each delta as an SSE/WS frame.
//   - ToolUseSink → cb.OnToolUse / cb.OnToolResult render LLM tool calls.
//   - ThinkingSink → cb.OnThinking streams extended-thinking deltas.
func buildSinkCallbacks(ctx context.Context, sink bs.ResponseSink) *bs.StreamCallbacks {
	cb := &bs.StreamCallbacks{}
	any := false
	if ts, ok := sink.(bs.TextStreamSink); ok {
		cb.OnText = func(delta string) { _ = ts.SendTextDelta(ctx, delta) }
		any = true
	}
	if tu, ok := sink.(bs.ToolUseSink); ok {
		cb.OnToolUse = func(id, name string, input json.RawMessage) {
			_ = tu.SendToolUse(ctx, id, name, input)
		}
		cb.OnToolResult = func(useID, output string, isError bool, latencyMs int) {
			_ = tu.SendToolResult(ctx, useID, output, isError, latencyMs)
		}
		any = true
	}
	if th, ok := sink.(bs.ThinkingSink); ok {
		cb.OnThinking = func(delta string) { _ = th.SendThinking(ctx, delta) }
		any = true
	}
	if us, ok := sink.(bs.UsageSink); ok {
		cb.OnUsage = func(input, output int) { _ = us.SendUsage(ctx, input, output) }
		any = true
	}
	if !any {
		return nil
	}
	return cb
}

// BargeInEnabled reports whether the barge-in voice path is enabled. The
// WebSocket transport reads it to choose its connection-handling loop.
func (g *Gateway) BargeInEnabled() bool {
	return g.deps.Config.Gateway.BargeIn
}

// TranscribeAudio runs speech-to-text on raw audio bytes. Used by the barge-in
// turn manager to transcribe an interjection before classifying it.
func (g *Gateway) TranscribeAudio(ctx context.Context, audio []byte) (string, error) {
	if g.whisper == nil || !g.whisper.IsConfigured() {
		return "", fmt.Errorf("transcription not configured")
	}
	return g.whisper.Transcribe(ctx, audio, "voice.wav")
}

// ClassifyInterjection decides whether a user utterance that arrived mid-
// response is a backchannel (keep the turn running) or a real interruption
// (cancel it). It is a single cheap reflex-model call; it deliberately does
// not run the AME / rule pipeline so it stays fast and lock-free while the
// active turn is still streaming. inflightTail is what the assistant is
// currently saying — without it the classifier cannot tell "да-да, понятно"
// from "да-да, не то ищешь".
func (g *Gateway) ClassifyInterjection(ctx context.Context, transcript, inflightTail string) (bs.InterjectionClass, error) {
	model := g.reflexModel()
	if model == "" {
		return bs.InterjectionUnclear, fmt.Errorf("reflex model not configured")
	}
	if g.reflexInterjectionPrompt == "" {
		return bs.InterjectionUnclear, fmt.Errorf("reflex-interjection prompt not loaded")
	}

	// Only the recent tail of the in-flight response matters for the decision.
	tail := []rune(inflightTail)
	if len(tail) > 600 {
		tail = tail[len(tail)-600:]
	}
	prompt := fmt.Sprintf("Ассистент сейчас говорит:\n%s\n\nПользователь перебил репликой:\n%s",
		strings.TrimSpace(string(tail)), transcript)

	resp, err := g.provider.Complete(ctx, bs.CompletionRequest{
		Model:     model,
		MaxTokens: 16,
		System:    g.reflexInterjectionPrompt,
		Messages:  []bs.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return bs.InterjectionUnclear, fmt.Errorf("classify interjection: %w", err)
	}

	out := strings.ToLower(strings.TrimSpace(bs.ExtractText(resp.Content)))
	switch {
	case strings.Contains(out, "interrupt"):
		return bs.InterjectionInterrupt, nil
	case strings.Contains(out, "backchannel"):
		return bs.InterjectionBackchannel, nil
	default:
		return bs.InterjectionUnclear, nil
	}
}

// PersistInterrupted records a cancelled turn's partial response as an
// assistant message so the session keeps user/assistant alternation intact —
// a dangling user message with no reply would break the next turn's API call.
// Runs on a fresh background context because the turn's own context is, by
// definition, already cancelled.
func (g *Gateway) PersistInterrupted(_ context.Context, chatID, partial string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	us, err := g.getOrInitUser(ctx, chatID)
	if err != nil {
		g.logger.Warn("persist interrupted: resolve user failed", "error", err)
		return
	}
	ctx = bs.WithSoulID(ctx, us.SoulID)

	us.Mu.Lock()
	defer us.Mu.Unlock()

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Warn("persist interrupted: session failed", "error", err)
		return
	}

	text := strings.TrimSpace(partial)
	if text == "" {
		text = "[прервано пользователем]"
	} else {
		text += " […прервано]"
	}
	if err := g.store.Append(ctx, sess.ID, bs.Message{
		Role:    "assistant",
		Content: []bs.ContentBlock{{Type: "text", Text: text}},
	}); err != nil {
		g.logger.Warn("persist interrupted: append failed", "error", err)
	}
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
		MatchedRules         []string                 `json:"matched_rules"`
		Intent               string                   `json:"intent"`
		Confidence           float64                  `json:"confidence"`
		PreActions           []bs.ToolAction          `json:"pre_actions"`
		PostActions          []bs.PostAction          `json:"post_actions"`
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
			var toolObjects []struct {
				Tool string `json:"tool"`
			}
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

// GetOrCreateSession returns the soul's single permanent chat session,
// creating it on first contact. There is no rotation: the conversation
// is one continuous thread per (user, soul). Chat history is permanent
// and decoupled from the LLM context window — the agent loop windows the
// context itself (MessagesForAPI) and AME recalls older turns
// associatively. The session is never archived; "resetting" it would
// mean destroying history.
func (g *Gateway) GetOrCreateSession(ctx context.Context, us *UserState) (*session.Session, error) {
	if g.deps.ModelStore != nil {
		_ = g.deps.ModelStore.Refresh(ctx)
	}
	return g.store.GetOrCreate(ctx, us.UserID.String(), g.cortexModelDisplay())
}

// ResetSession archives the active (user, soul) chat session and opens a
// fresh one in its place. The chat_messages rows stay on disk — history
// rendering and AME recall still see them — but the LLM-side context
// window starts blank, so the next turn begins a new thread. Mirrors
// the Telegram /reset command's behaviour for HTTP / web callers.
//
// Caller MUST pin the soul on ctx via bs.WithSoulID; without it
// GetOrCreate cross-pollinates sessions across souls.
func (g *Gateway) ResetSession(ctx context.Context, userID string) (oldID, newID string, err error) {
	if g.deps.ModelStore != nil {
		_ = g.deps.ModelStore.Refresh(ctx)
	}
	sess, err := g.store.GetOrCreate(ctx, userID, g.cortexModelDisplay())
	if err != nil {
		return "", "", fmt.Errorf("reset: get session: %w", err)
	}
	if sess == nil {
		return "", "", fmt.Errorf("reset: no session for user %s", userID)
	}
	oldID = sess.ID
	if err := g.store.Archive(ctx, sess.ID); err != nil {
		return oldID, "", fmt.Errorf("reset: archive: %w", err)
	}
	newSess, err := g.store.CreateWithPrevious(ctx, userID, g.cortexModel(), sess.ID)
	if err != nil {
		return oldID, "", fmt.Errorf("reset: create new: %w", err)
	}
	if newSess == nil {
		return oldID, "", fmt.Errorf("reset: create returned nil")
	}
	g.logger.Info("reset: archived + recreated session",
		"user_id", userID,
		"old_session_id", oldID,
		"new_session_id", newSess.ID,
		"messages_in_old", sess.MessageCount,
	)
	return oldID, newSess.ID, nil
}

// Timezone returns the configured timezone.
func (g *Gateway) Timezone() *time.Location { return g.tz }


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
	// rawAttachments is the per-turn list of files we want to push
	// into the host's CDN (vaelum.chat_attachments + disk store) once
	// the session id is known. We keep the bytes here, not in the
	// images slice, because images travel base64-encoded for the
	// model and the sink needs raw bytes — decoding back would mean
	// allocating twice. Empty on text-only turns.
	rawAttachments []rawAttachment
	// replyToTGMessageID is the Telegram message id of the parent
	// when the user replied via Telegram. processMessages resolves
	// this to our chat_messages.id via the session store before
	// stamping the new row's reply_to_message_id column.
	replyToTGMessageID int
	// replyToMessageID is a directly-supplied parent uuid. Set by
	// the cabinet path (where the frontend knows the parent id
	// natively); 0 / empty for Telegram inbound. When both are set
	// the direct id wins.
	replyToMessageID string
}

// rawAttachment is one inbound file held by pendingMsg until the
// debouncer flushes and the gateway has a session id to stamp it
// with. Kind is the same lane vocabulary as elsewhere ("image" /
// "pdf" / "text").
type rawAttachment struct {
	name string
	mime string
	kind string
	data []byte
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
