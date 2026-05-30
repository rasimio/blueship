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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/attachment"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/provider/openai"
	"github.com/rasimio/blueship/internal/transport/telegram"
	"github.com/rasimio/blueship/internal/webaccess/browser"
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
//     we don't want to leak that this token belongs to
//     someone in particular.
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
