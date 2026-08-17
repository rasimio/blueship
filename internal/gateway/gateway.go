package gateway

import (
	"context"
	"encoding/base64"
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
	pdfint "github.com/rasimio/blueship/integration/pdf"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/provider/openai"
	"github.com/rasimio/blueship/internal/transport/telegram"
	"github.com/rasimio/blueship/internal/webaccess/browser"
	"github.com/rasimio/blueship/runtime/session"
	"github.com/rasimio/blueship/tool"
)

// Gateway receives transport updates and routes them through the AgentLoop.
type Gateway struct {
	// personaCache: 60s-TTL per-soul persona prompts (see cachedPersona).
	personaMu    sync.Mutex
	personaCache map[uuid.UUID]personaCacheEntry

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

	// Platform prompt layers for hosts with a multi-tenant persona model.
	// Resolved once through the host's ResolvePlatformPrompts hook and
	// cached by platformPrompts. Where those layers live — files, a table,
	// anything else — is the host's business, not the framework's.
	ppMu       sync.Mutex
	ppLoaded   bool
	ppPreamble string
	ppAgents   string

	// Reflex pipeline prompts. Loaded from <Config.Prompts>/<key>.md when
	// the agent ships those files; missing files leave the default empty.
	reflexSystemPrompt       string // system prompt for reflex LLM call
	reflexPlanTemplate       string // user prompt template (has %s placeholders for rules, tools, message)
	reflexInteractionPrompt  string // interaction-tier task rules, appended to the soul prompt when InteractionTier is on
	reflexInterjectionPrompt string // system prompt for barge-in interjection classification

	mu sync.Mutex
	// Entries are keyed by transport identity. Telegram keys include bot id:
	// private-chat ids are global user ids and repeat across every bot the
	// same person talks to. Platform keys also include soul id.
	users map[string]*UserState

	// turnLocks serializes every chat turn for one (user, soul), regardless
	// of transport. A web message, a Telegram message, and an autonomous
	// draft/commit must never observe or mutate the same live session
	// concurrently.
	turnLocks sync.Map // map[string]*sync.Mutex

	// activeTurns holds the cancel handle of the turn currently running for
	// one (user, soul), so a stop arriving on any transport — a button in
	// the cabinet, an inline button in Telegram — reaches the generation it
	// refers to. Same key as turnLocks: turns are serialised per
	// conversation, so an entry is either there or the conversation is idle.
	activeTurns sync.Map // map[string]*turnHandle

	// conversationActivity closes the gap between transport ingress and
	// chat_messages persistence. In particular, a Telegram message spends a
	// short time in the debouncer before processMessages can append it. An
	// autonomous turn must already treat that message as fresh human activity.
	activityStates   sync.Map // map[string]*conversationActivityState
	activityBootOnce sync.Once
	activityBootID   uuid.UUID
}

func (g *Gateway) turnMutex(userID, soulID uuid.UUID) *sync.Mutex {
	key := conversationKey(userID, soulID)
	lock, _ := g.turnLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func conversationKey(userID, soulID uuid.UUID) string {
	return userID.String() + ":" + soulID.String()
}

type conversationActivityState struct {
	mu            sync.Mutex
	Version       uint64
	Pending       int
	LastInboundAt time.Time
	// UnfinalizedAutonomous keeps the provider receipt alive across the small
	// ACK→journal window. A later turn retries these entries under turnMu and
	// fails closed until the shared dialogue contains the sent pulse.
	UnfinalizedAutonomous map[uuid.UUID]bs.TaskNotificationReceipt
}

type conversationActivitySnapshot struct {
	Token         string
	Version       uint64
	Pending       int
	LastInboundAt time.Time
}

func (g *Gateway) activityState(userID, soulID uuid.UUID) *conversationActivityState {
	key := conversationKey(userID, soulID)
	state, _ := g.activityStates.LoadOrStore(key, &conversationActivityState{})
	return state.(*conversationActivityState)
}

func (g *Gateway) activityBoot() uuid.UUID {
	g.activityBootOnce.Do(func() {
		g.activityBootID = uuid.New()
	})
	return g.activityBootID
}

func (g *Gateway) activitySnapshotLocked(state *conversationActivityState) conversationActivitySnapshot {
	return conversationActivitySnapshot{
		Token:         fmt.Sprintf("%s:%d", g.activityBoot(), state.Version),
		Version:       state.Version,
		Pending:       state.Pending,
		LastInboundAt: state.LastInboundAt,
	}
}

func (g *Gateway) lockActivity(userID, soulID uuid.UUID) (conversationActivitySnapshot, func()) {
	state := g.activityState(userID, soulID)
	state.mu.Lock()
	return g.activitySnapshotLocked(state), state.mu.Unlock
}

func (g *Gateway) admitInboundActivity(us *UserState) uint64 {
	if us == nil {
		return 0
	}
	state := g.activityState(us.UserID, us.SoulID)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.Version++
	state.Pending++
	state.LastInboundAt = time.Now()
	return state.Version
}

func (g *Gateway) trackInboundActivity(us *UserState, msgs []pendingMsg) {
	if us == nil {
		return
	}
	var trackedIndexes []int
	for i := range msgs {
		if msgs[i].activityTracked || msgs[i].ephemeral {
			continue
		}
		msgs[i].activityTracked = true
		trackedIndexes = append(trackedIndexes, i)
	}
	if len(trackedIndexes) == 0 {
		return
	}

	state := g.activityState(us.UserID, us.SoulID)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.Version++
	for _, i := range trackedIndexes {
		msgs[i].activityVersion = state.Version
	}
	state.Pending += len(trackedIndexes)
	state.LastInboundAt = time.Now()
}

func (g *Gateway) clearInboundActivity(us *UserState, msgs []pendingMsg) {
	if us == nil {
		return
	}
	tracked := 0
	for _, msg := range msgs {
		if msg.activityTracked {
			tracked++
		}
	}
	if tracked == 0 {
		return
	}
	g.clearInboundActivityCount(us, tracked)
}

// completeInboundActivity retires a batch admitted at transport ingress.
// durable is true only when processMessages observed a new persisted user
// anchor. Failed/silent preflight must roll LastInboundAt back once no other
// admitted batch remains, while Version stays monotonic so every older
// autonomous draft remains stale.
func (g *Gateway) completeInboundActivity(us *UserState, msgs []pendingMsg, durable bool) {
	if us == nil {
		return
	}
	tracked := 0
	var latestBatchVersion uint64
	for _, msg := range msgs {
		if !msg.activityTracked {
			continue
		}
		tracked++
		if msg.activityVersion > latestBatchVersion {
			latestBatchVersion = msg.activityVersion
		}
	}
	if tracked == 0 {
		return
	}

	state := g.activityState(us.UserID, us.SoulID)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.Pending -= tracked
	if state.Pending < 0 {
		state.Pending = 0
	}
	if state.Pending != 0 {
		return
	}
	if !durable || state.Version > latestBatchVersion {
		state.LastInboundAt = time.Time{}
	}
}

func (g *Gateway) clearInboundActivityCount(us *UserState, tracked int) {
	if us == nil || tracked <= 0 {
		return
	}
	state := g.activityState(us.UserID, us.SoulID)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.Pending -= tracked
	if state.Pending < 0 {
		state.Pending = 0
	}
}

func (g *Gateway) rollbackInboundActivity(us *UserState, version uint64) {
	if us == nil || version == 0 {
		return
	}
	state := g.activityState(us.UserID, us.SoulID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.Pending > 0 {
		state.Pending--
	}
	// Keep the rotated version so every older autonomous draft stays stale,
	// but do not let an unsupported/failed preprocessing attempt suppress all
	// future pulses forever.
	if state.Version == version && state.Pending == 0 {
		state.LastInboundAt = time.Time{}
	}
}

func (g *Gateway) rememberAutonomousFinalization(
	userID, soulID, attemptID uuid.UUID,
	receipt bs.TaskNotificationReceipt,
) {
	if attemptID == uuid.Nil || g.deps.FinalizeAutonomousNotification == nil {
		return
	}
	state := g.activityState(userID, soulID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.UnfinalizedAutonomous == nil {
		state.UnfinalizedAutonomous = make(map[uuid.UUID]bs.TaskNotificationReceipt)
	}
	state.UnfinalizedAutonomous[attemptID] = receipt
}

// ensureAutonomousHistoryLocked is called only while the pair-scoped turnMu is
// held. It first closes any in-process ACK→confirm window, then drains durable
// sent rows for this exact session. A failure is intentionally returned to the
// caller: running Cortex on a dialogue that omits its own visible message would
// break conversational continuity.
func (g *Gateway) ensureAutonomousHistoryLocked(
	ctx context.Context,
	userID, soulID uuid.UUID,
	sessionID string,
) error {
	state := g.activityState(userID, soulID)
	state.mu.Lock()
	pending := make(map[uuid.UUID]bs.TaskNotificationReceipt, len(state.UnfinalizedAutonomous))
	for id, receipt := range state.UnfinalizedAutonomous {
		pending[id] = receipt
	}
	state.mu.Unlock()

	if len(pending) > 0 && g.deps.FinalizeAutonomousNotification == nil {
		return fmt.Errorf("autonomous notification finalizer unavailable")
	}
	for id, receipt := range pending {
		if err := g.deps.FinalizeAutonomousNotification(ctx, id, receipt); err != nil {
			return fmt.Errorf("finalize autonomous notification %s: %w", id, err)
		}
		state.mu.Lock()
		delete(state.UnfinalizedAutonomous, id)
		state.mu.Unlock()
	}
	if g.deps.EnsureAutonomousHistory != nil {
		if err := g.deps.EnsureAutonomousHistory(ctx, userID, soulID, sessionID); err != nil {
			return fmt.Errorf("ensure autonomous history: %w", err)
		}
	}
	return nil
}

func (g *Gateway) activitySnapshot(userID, soulID uuid.UUID) conversationActivitySnapshot {
	snapshot, unlock := g.lockActivity(userID, soulID)
	defer unlock()
	return snapshot
}

func telegramUserCacheKey(botID uuid.UUID, chatID string) string {
	return "telegram-bot:" + botID.String() + ":" + chatID
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
	// so the next short answer ("1", "yes") can be resolved to a tool call.
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
	// Personality lives with the agent, never in the framework. With no Prompts
	// configured the ship boots with an empty system prompt (a bare LLM agent) —
	// a loud warning, not a hard failure, so a fresh checkout runs immediately.
	// Set Config.Prompts to a directory of <key>.md files to give the agent a
	// personality; once it IS set, a missing file is a hard error (catches a
	// misconfigured deploy rather than silently shipping a personality-less bot).
	if cfg.Prompts == "" {
		logger.Warn("gateway: no Config.Prompts set — running with an empty system prompt; " +
			"set Config.Prompts to a directory of <key>.md files to give the agent a personality")
	} else {
		if err := gw.loadSystemPrompts(cfg.Prompts); err != nil {
			return nil, fmt.Errorf("load system prompts: %w", err)
		}
		gw.loadPlatformGreet(cfg.Prompts)
	}

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
// (compact, reflex-system, reflex-plan, reflex-interaction,
// reflex-interjection) are picked up if present; missing optional
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
	return nil
}

// systemPromptForSoul returns the fully composed system prompt for a soul:
// platform preamble + platform agents layer + the soul's own persona, in
// that order — persona LAST, so all souls share a byte-identical cacheable
// prefix and the voice gets the attention-favored final position. The
// platform layers come from the host's ResolvePlatformPrompts hook
// (typically files on disk); the persona comes from the ResolveSoulPersona
// hook (a vaelum.soul_personas row). A soul without a persona row is a
// misconfiguration and surfaces as an error — there is no silent fallback
// to file-loaded prompts. Framework consumers that do not use the vaelum
// soul model (soulID is nil) get the file-loaded process prompt.
func (g *Gateway) systemPromptForSoul(ctx context.Context, soulID uuid.UUID) (string, error) {
	if soulID == uuid.Nil {
		return g.systemPrompt, nil
	}
	resolve := g.deps.Config.Gateway.ResolveSoulPersona
	if resolve == nil {
		return "", fmt.Errorf("system prompt for soul %s: no ResolveSoulPersona hook configured", soulID)
	}
	persona, err := g.cachedPersona(ctx, soulID, resolve)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		// The turn's context-prep (AME retrieval, rule search — optional,
		// degradable work) can consume the entire turn deadline before this
		// MANDATORY, cheap lookup (single-row select by soul_id). Aborting
		// the whole turn because optional prep ran slow is the wrong
		// failure mode — observed live 2026-07-09: a huge notebook message
		// stalled the AME embed until the deadline and every subsequent
		// turn of that soul died here. Retry once on a short detached
		// deadline, preserving ctx values (soul, tenancy).
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if rescued, rerr := g.cachedPersona(rctx, soulID, resolve); rerr == nil {
			persona, err = rescued, nil
			// platformPrompts below reads the DB only on a cold cache;
			// give it the same rescue budget instead of the dead ctx.
			ctx = rctx
		}
	}
	if err != nil {
		return "", fmt.Errorf("system prompt for soul %s: persona lookup failed: %w", soulID, err)
	}
	if strings.TrimSpace(persona) == "" {
		return "", fmt.Errorf("system prompt for soul %s: persona is empty", soulID)
	}
	preamble, agents, err := g.platformPrompts(ctx)
	if err != nil {
		return "", err
	}
	// Persona goes last, after the platform layers, for two reasons.
	//
	// Attention: between a short preamble and a long operating manual it sat in
	// the weakest part of the prompt, with the voice buried under thousands of
	// tokens of tool discipline. Trailing the system prompt puts it where the
	// model reads it best.
	//
	// Cache: the platform layers are byte-identical for every soul while the
	// persona is the only per-soul part, so this order makes preamble+agents a
	// prefix all souls share and warm for each other, instead of diverging a
	// thousand characters in. (Worth little until the [current_datetime] stamp
	// stops leading the prompt and invalidating the prefix at token zero.)
	return strings.Join([]string{preamble, agents, persona}, "\n\n"), nil
}

// cachedPersona wraps the ResolveSoulPersona hook with a short TTL cache:
// personas change rarely (cabinet edits) but were fetched from Postgres on
// EVERY turn. 60s staleness is imperceptible for persona edits and removes
// a per-turn DB round-trip from the hot path.
func (g *Gateway) cachedPersona(ctx context.Context, soulID uuid.UUID, resolve func(context.Context, uuid.UUID) (string, error)) (string, error) {
	g.personaMu.Lock()
	if e, ok := g.personaCache[soulID]; ok && time.Since(e.at) < time.Minute {
		g.personaMu.Unlock()
		return e.prompt, nil
	}
	g.personaMu.Unlock()
	prompt, err := resolve(ctx, soulID)
	if err != nil {
		return "", err
	}
	g.personaMu.Lock()
	if g.personaCache == nil {
		g.personaCache = make(map[uuid.UUID]personaCacheEntry)
	}
	g.personaCache[soulID] = personaCacheEntry{prompt: prompt, at: time.Now()}
	g.personaMu.Unlock()
	return prompt, nil
}

type personaCacheEntry struct {
	prompt string
	at     time.Time
}

// platformPrompts returns the platform preamble and agents layers,
// resolved once through the host hook and cached for the process lifetime.
// A failed load is not cached, so a transient error is retried next call.
//
// Cached deliberately: the layers are composed into every turn, and a host
// backing them with files would otherwise read from disk on each one. The
// price is that an edit needs a restart to take effect.
func (g *Gateway) platformPrompts(ctx context.Context) (preamble, agents string, err error) {
	g.ppMu.Lock()
	defer g.ppMu.Unlock()
	if g.ppLoaded {
		return g.ppPreamble, g.ppAgents, nil
	}
	resolve := g.deps.Config.Gateway.ResolvePlatformPrompts
	if resolve == nil {
		return "", "", fmt.Errorf("platform prompts: no ResolvePlatformPrompts hook configured")
	}
	preamble, agents, err = resolve(ctx)
	if err != nil {
		return "", "", fmt.Errorf("platform prompts: %w", err)
	}
	if strings.TrimSpace(preamble) == "" || strings.TrimSpace(agents) == "" {
		return "", "", fmt.Errorf("platform prompts: preamble or agents layer is empty")
	}
	g.ppPreamble, g.ppAgents, g.ppLoaded = preamble, agents, true
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
	resolve := g.deps.Config.Gateway.ResolveSoulPersona
	if resolve == nil {
		return "", fmt.Errorf("reflex system prompt: no ResolveSoulPersona hook configured")
	}
	persona, err := resolve(ctx, soulID)
	if err != nil {
		return "", fmt.Errorf("reflex system prompt: no persona for soul %s: %w", soulID, err)
	}
	preamble, _, err := g.platformPrompts(ctx)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{preamble, persona}, "\n\n"), nil
}

// expandCommandPrompt rewrites a configured prompt-shortcut command into
// the question it stands for, and returns anything else untouched.
//
// The point is that the answer stays an ordinary turn: a host that wants
// "/help" to describe what the assistant can do gets a reply assembled by
// the assistant, from whatever it can actually do right now, rather than
// a static page that begins drifting from the truth the day it is
// written. Commands with no Prompt are menu entries for handlers that
// already exist and pass through untouched.
//
// Arguments are ignored rather than appended: "/help про файлы" would
// otherwise splice into the configured question and produce something
// nobody wrote.
func (g *Gateway) expandCommandPrompt(bi *botInstance, text string) string {
	if !strings.HasPrefix(strings.TrimSpace(text), "/") {
		return text
	}
	cmd, forUs := g.parseCommand(bi, text)
	if !forUs || cmd == "" {
		return text
	}
	name := strings.ToLower(strings.TrimPrefix(cmd, "/"))
	for _, c := range g.deps.Config.Gateway.Commands {
		if c.Prompt == "" {
			continue
		}
		if strings.ToLower(strings.TrimPrefix(strings.TrimSpace(c.Name), "/")) == name {
			g.logger.Debug("gateway: expanded command prompt", "command", name)
			return c.Prompt
		}
	}
	return text
}

// sendDenial delivers a refusal with whatever way out the host offered.
//
// The buttons matter more than the sentence: a person who has just been
// refused is the least likely to go and compose a command, so the escape
// has to be one tap away from the message that blocked them.
func (g *Gateway) sendDenial(ctx context.Context, bi *botInstance, tgChatID int64, text string, actions []bs.DecisionAction) {
	if bi == nil || bi.client == nil {
		return
	}
	rows := make([][]telegram.InlineKeyboardButton, 0, len(actions))
	for _, a := range actions {
		if strings.TrimSpace(a.Label) == "" || strings.TrimSpace(a.Command) == "" {
			continue
		}
		rows = append(rows, []telegram.InlineKeyboardButton{{
			Text:         a.Label,
			CallbackData: onbCallbackHostCmd + a.Command,
		}})
	}
	if len(rows) == 0 {
		_, _ = bi.client.SendMessage(ctx, fmt.Sprintf("%d", tgChatID), text)
		return
	}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, text, rows); err != nil {
		g.logger.Warn("gateway: denial keyboard send failed", "chat_id", tgChatID, "error", err)
	}
}

// runHostCommand invokes a host command by name and delivers its answer.
// Shared by the typed-command path, the refusal buttons, and the reply a
// command asked to wait for, so all three behave identically.
func (g *Gateway) runHostCommand(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, us *UserState, name, args string) {
	result, err := g.deps.CommandHandler(ctx, bs.BotCommandRequest{
		Name: name, Args: args, UserID: us.UserID, SoulID: us.SoulID,
		BotID: bi.id, BotKind: bi.kind,
	})
	if err != nil {
		g.logger.Error("gateway: host command failed",
			"command", name, "user_id", us.UserID, "error", err)
		return
	}

	// Claim the next message BEFORE answering, so a fast reply cannot
	// arrive while there is nothing recorded to route it to.
	if result.AwaitReply != "" && g.deps.BotOnboarding != nil && bi.id != uuid.Nil {
		if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id,
			onbStepAwaitPrefix+result.AwaitReply, nil); err != nil {
			g.logger.Warn("gateway: could not park the chat for a reply",
				"command", name, "error", err)
		}
	}

	if strings.TrimSpace(result.Text) == "" {
		return
	}
	if len(result.Buttons) == 0 && (result.ButtonURL == "" || result.ButtonLabel == "") {
		g.sendOnboardingText(ctx, bi, tgChatID, result.Text)
		return
	}
	rows := [][]telegram.InlineKeyboardButton{{
		{Text: result.ButtonLabel, URL: result.ButtonURL},
	}}
	if len(result.Buttons) > 0 {
		rows = rows[:0]
		for _, b := range result.Buttons {
			url := b.URL
			if b.Invoice != nil {
				link, err := bi.client.CreateInvoiceLink(ctx, telegram.InvoiceRequest{
					Title:              b.Invoice.Title,
					Description:        b.Invoice.Description,
					Payload:            b.Invoice.Payload,
					Stars:              b.Invoice.Stars,
					SubscriptionPeriod: b.Invoice.SubscriptionPeriod,
				})
				if err != nil {
					// Drop this button, keep the rest. The other ways to
					// pay still work, and offering a dead one is worse
					// than offering one fewer.
					g.logger.Error("payments: could not create an invoice link",
						"command", name, "payload", b.Invoice.Payload, "error", err)
					continue
				}
				url = link
			}
			if url == "" || b.Label == "" {
				continue
			}
			rows = append(rows, []telegram.InlineKeyboardButton{{Text: b.Label, URL: url}})
		}
	}
	if len(result.Buttons) > 0 && len(rows) == 0 {
		// Every button failed to build. The text alone still says
		// something useful; a keyboard-less message beats none.
		g.sendOnboardingText(ctx, bi, tgChatID, result.Text)
		return
	}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, result.Text, rows); err != nil {
		g.logger.Warn("gateway: host command reply failed", "command", name, "error", err)
	}
}

// maybeRunHostCommand dispatches a command the host answers itself.
// Returns true when the message was one, so the caller stops.
//
// The reply may carry a single link button — the way a host hands over
// something the chat cannot produce on its own, a payment page being the
// case this was built for.
func (g *Gateway) maybeRunHostCommand(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, us *UserState, text string) bool {
	if us == nil {
		return false
	}
	cmd, forUs := g.parseCommand(bi, text)
	if !forUs || cmd == "" {
		return false
	}
	name := strings.ToLower(strings.TrimPrefix(cmd, "/"))
	args := ""
	if i := strings.IndexByte(strings.TrimSpace(text), ' '); i >= 0 {
		args = strings.TrimSpace(strings.TrimSpace(text)[i+1:])
	}

	if deep, ok := g.startPayloadCommand(text); ok {
		name, args = deep, ""
	}

	// A menu command is answered by the transport: what it does is the
	// menu, and there is no handler for the host to write.
	//
	// A persistent keyboard wins over the inline menu when both are
	// configured. It is the one the host meant: it lives under the input
	// field, so a person who asks for the menu is usually asking for it
	// back after hiding it, not for a second one in the transcript.
	if g.isMenuCommand(name) {
		if g.keyboard().Configured() {
			g.showKeyboard(ctx, bi, tgChatID, g.deps.Config.UI.KeyboardShown)
			return true
		}
		return g.openMenu(ctx, bi, tgChatID)
	}

	if g.deps.CommandHandler == nil || !g.isHostCommand(name) {
		return false
	}
	// Consumed either way, including on failure: the alternative is
	// handing "/plus foo@bar" to the model as though it were conversation.
	g.runHostCommand(ctx, bi, tgChatID, tgUserID, us, name, args)
	return true
}

// isMenuCommand reports whether this command opens the inline menu.
func (g *Gateway) isMenuCommand(name string) bool {
	if len(g.menu().Nodes) == 0 {
		return false
	}
	for _, c := range g.deps.Config.Gateway.Commands {
		if c.Menu && strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(c.Name), "/"), name) {
			return true
		}
	}
	return false
}

// isHostCommand reports whether the configured menu marks this name as
// host-answered.
func (g *Gateway) isHostCommand(name string) bool {
	for _, c := range g.deps.Config.Gateway.Commands {
		if c.Host && strings.ToLower(strings.TrimPrefix(strings.TrimSpace(c.Name), "/")) == name {
			return true
		}
	}
	return false
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

// prepareTelegramInbound performs only the cheap routing/policy work needed to
// identify a conversation, then admits the message to its pair-scoped activity
// fence before any document download, PDF rendering, or transcription starts.
// false means the update was fully handled or intentionally ignored.
func (g *Gateway) prepareTelegramInbound(
	ctx context.Context,
	bi *botInstance,
	msg *telegram.Message,
	text string,
) (*UserState, uint64, bool) {
	rawChatID := msg.Chat.ID
	chatID := tgCanonical(rawChatID)
	tgUserID := msg.From.ID

	// Group chats are admitted only when this bot was actually addressed.
	if msg.Chat.Type != "private" && !strings.HasPrefix(text, "/") {
		if !g.shouldProcessGroupMessage(bi, msg, text) {
			g.logger.Debug("gateway: group message not addressed, skipping",
				"chat_id", chatID,
				"chat_type", msg.Chat.Type,
			)
			return nil, 0, false
		}
	}

	if strings.HasPrefix(strings.TrimLeft(text, " \n\t\r"), "[a2a-trace]") {
		g.logger.Debug("gateway: a2a trace message, visibility only — skipping cortex turn",
			"chat_id", chatID)
		return nil, 0, false
	}
	if g.maybeRunDeeplinkLogin(ctx, bi, rawChatID, tgUserID, text) {
		return nil, 0, false
	}
	if g.maybeRunDeeplinkLink(ctx, bi, rawChatID, tgUserID, text) {
		return nil, 0, false
	}
	if g.maybeRunBotOnboarding(ctx, bi, chatID, rawChatID, tgUserID, text, tgSender{
		Name:     msg.From.FirstName,
		Handle:   msg.From.Username,
		Language: msg.From.LanguageCode,
	}) {
		return nil, 0, false
	}

	us, err := g.getOrInitTelegramUser(ctx, bi, chatID, rawChatID, tgUserID)
	if err != nil {
		if errors.Is(err, bs.ErrTelegramChatUnpaired) {
			g.replyUnpaired(ctx, bi, chatID)
			return nil, 0, false
		}
		// Not "unpaired" — a real resolution failure, and the message is
		// dropped without the person being told anything. At Debug that is
		// invisible in production, which is the wrong level for the only
		// record that somebody was ignored.
		g.logger.Warn("gateway: dropping message, could not resolve the chat",
			"chat_id", chatID, "bot_id", bi.id.String(), "error", err)
		return nil, 0, false
	}

	// Host-handled commands run before the execution policy, deliberately.
	// The case they exist for is somebody the policy has just refused who
	// needs a way to do something about it — putting them behind the same
	// gate that said no would close the loop.
	if g.maybeRunHostCommand(ctx, bi, rawChatID, tgUserID, us, text) {
		return nil, 0, false
	}

	// Stopping runs before the execution policy for the same reason host
	// commands do: it consumes nothing, and a ceiling that ticks over while
	// an answer is being written must not leave the person watching it with
	// no way to end it.
	if g.maybeRunStopCommand(ctx, bi, rawChatID, us, text) {
		return nil, 0, false
	}

	decision, err := g.authorizeExecution(ctx, us.UserID, us.SoulID, bs.ExecutionInteractive, "telegram")
	if err != nil {
		g.logger.Warn("gateway: execution authorization failed",
			"chat_id", chatID, "user_id", us.UserID, "error", err)
		return nil, 0, false
	}
	if !decision.Allowed {
		g.logger.Info("gateway: execution denied",
			"chat_id", chatID, "user_id", us.UserID, "reason", decision.Reason)
		if bi != nil && bi.client != nil {
			denial := decision.Message
			if denial == "" {
				denial = g.deps.Config.UI.ExecutionDenied
			}
			g.sendDenial(ctx, bi, rawChatID, denial, decision.Actions)
		}
		return nil, 0, false
	}

	if cmd, forUs := g.parseCommand(bi, text); cmd == "/reset" && forUs {
		admissionVersion := g.admitInboundActivity(us)
		go func() {
			resetSucceeded := false
			defer func() {
				if resetSucceeded {
					g.clearInboundActivityCount(us, 1)
				} else {
					g.rollbackInboundActivity(us, admissionVersion)
				}
			}()
			rctx := bs.WithSoulID(context.Background(), us.SoulID)
			oldID, newID, resetErr := g.ResetSession(rctx, us.UserID.String())
			if resetErr != nil {
				g.logger.Warn("telegram /reset failed",
					"chat_id", us.ChatID, "user_id", us.UserID, "error", resetErr)
				if bi != nil && bi.client != nil {
					_, _ = bi.client.SendMessage(rctx, fmt.Sprintf("%d", rawChatID), g.deps.Config.UI.ResetFailed)
				}
				return
			}
			resetSucceeded = true
			g.logger.Info("telegram /reset done",
				"chat_id", us.ChatID, "user_id", us.UserID,
				"old_session_id", oldID, "new_session_id", newID)
			if bi != nil && bi.client != nil {
				_, _ = bi.client.SendMessage(rctx, fmt.Sprintf("%d", rawChatID), g.deps.Config.UI.ResetDone)
			}
		}()
		return nil, 0, false
	}

	admissionVersion := g.admitInboundActivity(us)
	return us, admissionVersion, true
}

func (g *Gateway) handleUpdate(ctx context.Context, bi *botInstance, update telegram.Update) {
	// Payments first, and before the inbound path, because both carry a
	// deadline or a debt: a pre-checkout query expires in seconds, and a
	// successful payment rides on a message with no text and no
	// attachment — which the admission checks below discard.
	if q := update.PreCheckoutQuery; q != nil {
		g.handlePreCheckout(ctx, bi, q)
		return
	}
	if m := update.Message; m != nil && m.SuccessfulPayment != nil {
		g.handleSuccessfulPayment(ctx, m)
		return
	}

	// Handle callback queries (inline button presses).
	// LEGACY: the /model command's inline-keyboard callbacks land here; the
	// dispatch is parked behind the `legacy_commands` build tag along with
	// handleModelCallback in model_command.go. Restoration: rebuild with
	// `-tags legacy_commands` and uncomment the dispatch below.
	if cq := update.CallbackQuery; cq != nil {
		// Stop answers the callback itself: it has a toast to show, and the
		// blanket answer below would consume the query before it could.
		if g.maybeHandleStopCallback(ctx, bi, cq) {
			return
		}
		bi.client.AnswerCallbackQuery(ctx, cq.ID)
		// Menu taps first: they are pure navigation and must not fall
		// through to a flow that reads them as an answer to a question.
		if g.maybeHandleMenuCallback(ctx, bi, cq) {
			return
		}
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
	if strings.TrimSpace(text) == "" && msg.Document == nil && msg.Voice == nil && msg.Video == nil && msg.VideoNote == nil && len(msg.Photo) == 0 {
		return
	}
	// A tap on the persistent keyboard arrives as its own label, because
	// that kind of keyboard has no callbacks. Turned back into the
	// command it stands for here, first, so every dispatcher below sees
	// what the person meant rather than a stray word — and so the
	// rewrite is one place rather than a check in each of them.
	if cmd, tapped := g.rewriteKeyboardTap(text); tapped {
		text = cmd
	}
	// Expand prompt shortcuts before anything looks at the text, so a
	// shortcut is indistinguishable from the user having typed the
	// question — including to the onboarding dispatcher, which would
	// otherwise treat it as an unrecognised command.
	text = g.expandCommandPrompt(bi, text)
	us, admissionVersion, admitted := g.prepareTelegramInbound(ctx, bi, msg, text)
	if !admitted {
		return
	}
	visibleText := text
	handoffAdmission := false
	defer func() {
		if !handoffAdmission {
			g.rollbackInboundActivity(us, admissionVersion)
		}
	}()

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
	var docImages []bs.ContentBlock
	var rawAttachments []rawAttachment
	if msg.Document != nil && isTranscribableVideoDocument(msg.Document.FileName, msg.Document.MimeType) {
		// A video sent as a file carries no duration in the update, so frame
		// sampling has to probe the download for it.
		text, visibleText = g.readVideoIntoTurn(ctx, bi.client, telegramTranscriptionInput{
			fileID:   msg.Document.FileID,
			kind:     "document",
			language: senderLanguage(msg),
		}, text, visibleText)
	}
	if msg.Document != nil && !isTranscribableVideoDocument(msg.Document.FileName, msg.Document.MimeType) {
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
					docImages = append(docImages, bs.ContentBlock{
						Type: "image",
						Source: &bs.ImageSource{
							Type:      "base64",
							MediaType: media,
							Data:      base64.StdEncoding.EncodeToString(data),
						},
					})
					rawAttachments = append(rawAttachments, rawAttachment{
						name: msg.Document.FileName, mime: media, kind: "image", data: data,
					})
				case "pdf":
					if pdfText, pages, perr := browser.ExtractPDFText(data); perr != nil {
						g.logger.Warn("failed to extract pdf text", "error", perr, "file", msg.Document.FileName, "size", len(data))
						text = appendDocInline(text, fmt.Sprintf("[pdf: %s — extraction failed: %v]", msg.Document.FileName, perr))
					} else if pdfint.TextLooksScanned(pdfText, pages) {
						// Scanned PDF: the pages are images and extraction has
						// nothing to give. Render the leading pages and let the
						// vision-capable model read them directly — otherwise a
						// contract photographed on a phone gets answered with
						// "there is nothing to read".
						pageImgs, ierr := pdfint.PagesToImages(ctx, data, pdfint.DefaultScanMaxPages, pdfint.DefaultScanDPI)
						if ierr != nil || len(pageImgs) == 0 {
							g.logger.Warn("scanned pdf: page render unavailable", "error", ierr, "file", msg.Document.FileName, "pages", pages)
							text = appendDocInline(text, fmt.Sprintf("[pdf: %s — %d pages, scanned (no text layer); page rendering unavailable — ask the user for a text version]", msg.Document.FileName, pages))
						} else {
							g.logger.Info("scanned pdf: pages rendered for vision", "file", msg.Document.FileName, "pages", pages, "rendered", len(pageImgs))
							text = appendDocInline(text, fmt.Sprintf("[pdf: %s — scanned, no text layer; first %d of %d pages attached as images — read them visually]", msg.Document.FileName, len(pageImgs), pages))
							for _, img := range pageImgs {
								docImages = append(docImages, bs.ContentBlock{
									Type: "image",
									Source: &bs.ImageSource{
										Type:      "base64",
										MediaType: "image/jpeg",
										Data:      base64.StdEncoding.EncodeToString(img),
									},
								})
							}
						}
					} else {
						text = appendDocInline(text, fmt.Sprintf("[pdf: %s — %d pages]%s", msg.Document.FileName, pages, pdfText))
					}
					rawAttachments = append(rawAttachments, rawAttachment{
						name: msg.Document.FileName, mime: "application/pdf", kind: "pdf", data: data,
					})
				case "xlsx":
					// Excel .xlsx has no native content block either; the
					// extractor renders sheets as markdown tables (bounded,
					// truncation announced) so the model reads real data.
					if xlsxMD, xerr := attachment.ExtractXlsxMarkdown(data); xerr != nil {
						g.logger.Warn("failed to extract xlsx", "error", xerr, "file", msg.Document.FileName, "size", len(data))
						text = appendDocInline(text, fmt.Sprintf("[xlsx: %s — could not read this Excel file]", msg.Document.FileName))
					} else {
						text = appendDocInline(text, fmt.Sprintf("[xlsx: %s]\n%s", msg.Document.FileName, xlsxMD))
					}
					rawAttachments = append(rawAttachments, rawAttachment{
						name: msg.Document.FileName, mime: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", kind: "xlsx", data: data,
					})
				case "docx":
					// Word .docx has no native Anthropic content block, so we
					// unzip word/document.xml and inline the prose as text —
					// the same shape a PDF or a source file lands in.
					if docText, derr := attachment.ExtractDocxText(data); derr != nil {
						g.logger.Warn("failed to extract docx text", "error", derr, "file", msg.Document.FileName, "size", len(data))
						text = appendDocInline(text, fmt.Sprintf("[docx: %s — could not read this Word file]", msg.Document.FileName))
					} else {
						text = appendDocInline(text, fmt.Sprintf("[docx: %s]\n%s", msg.Document.FileName, docText))
					}
					docMime := msg.Document.MimeType
					if docMime == "" {
						docMime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
					}
					rawAttachments = append(rawAttachments, rawAttachment{
						name: msg.Document.FileName, mime: docMime, kind: "docx", data: data,
					})
				case "text":
					// DecodeText, not string(data): a .txt exported by a
					// Windows editor is UTF-16 or Windows-1251, and raw
					// bytes would reach the prompt as mojibake.
					body, ok := attachment.DecodeText(data)
					if !ok {
						// Classifier and decoder read the same probe, so this
						// is unreachable in practice — but a silent break here
						// would leave a document-only message with empty text
						// and no reply at all.
						g.logger.Warn("text document classified but not decodable", "file", msg.Document.FileName, "size", len(data))
						text = appendDocInline(text, fmt.Sprintf("[file: %s — the text came through unreadable]", msg.Document.FileName))
						break
					}
					text = appendDocInline(text, fmt.Sprintf("[file: %s]\n```\n%s\n```", msg.Document.FileName, body))
					mime := msg.Document.MimeType
					if mime == "" {
						mime = "text/plain"
					}
					rawAttachments = append(rawAttachments, rawAttachment{
						name: msg.Document.FileName, mime: mime, kind: "text", data: data,
					})
				default:
					// Unsupported format (xlsx / pptx / legacy .doc / archive /
					// arbitrary binary). Inline a short notice rather than
					// dropping it silently — a document-only message would
					// otherwise leave `text` empty, trip the `text=="" && no
					// images` guard below, and the bot would never reply at all.
					// The bytes are logged as a hex prefix, not stored: a
					// rejected file leaves nothing behind, and without this
					// the next "why can't you read my .txt" has no evidence
					// to work from.
					g.logger.Info("unsupported document — inlining notice",
						"file", msg.Document.FileName, "mime", msg.Document.MimeType,
						"size", len(data), "head_hex", fmt.Sprintf("% x", data[:min(16, len(data))]))
					text = appendDocInline(text, fmt.Sprintf("[file: %s — the bytes are binary, not a format I can read; a PDF, .docx, .xlsx, an image, or a text file works]", msg.Document.FileName))
				}
			}
		}
	}

	if input, ok := telegramTranscriptionInputFor(msg); ok {
		text, visibleText = g.readVideoIntoTurn(ctx, bi.client, input, text, visibleText)
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
				visibleText = appendVisibleTranscript(visibleText, transcript)
			}
		}
	}

	images := append([]bs.ContentBlock(nil), docImages...)
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

	// The quote the model reads is NOT built here. It is built once, for
	// every transport, from the resolved parent row (see
	// prependReplyQuoteBlock) — the reply is one relational fact, and a
	// transport that renders its own copy is how the two drifted apart.
	//
	// What Telegram contributes is the raw wire quote, kept only as a
	// fallback for a parent our transcript cannot resolve: a message that
	// pre-dates the id index. Rich Messages arrive with no text at all, so
	// this is empty for anything the soul sent.
	var replyQuoteFallback string
	var replyMediaBlocks []bs.ContentBlock
	if msg.ReplyToMessage != nil {
		replyQuoteFallback = msg.ReplyToMessage.Text
		if replyQuoteFallback == "" {
			replyQuoteFallback = msg.ReplyToMessage.Caption
		}
		// The parent's file, read off the update. Only worth the download
		// when the parent actually carries one; processMessages drops these
		// again if the transcript turns out to hold the same file, which is
		// the richer copy.
		replyMediaBlocks = g.replyParentMedia(ctx, bi.client, msg.ReplyToMessage)
		if replyQuoteFallback == "" && len(replyMediaBlocks) == 0 {
			g.logger.Info("reply-to carries no wire quote — resolving from the transcript",
				"reply_msg_id", msg.ReplyToMessage.MessageID,
				"has_document", msg.ReplyToMessage.Document != nil,
			)
		}
	}

	if text == "" && len(images) == 0 {
		return
	}

	var replyToTGID int
	if msg.ReplyToMessage != nil {
		replyToTGID = msg.ReplyToMessage.MessageID
	}
	pending := []pendingMsg{{
		text:               text,
		images:             images,
		messageID:          msg.MessageID,
		visibleText:        &visibleText,
		rawAttachments:     rawAttachments,
		replyToTGMessageID: replyToTGID,
		replyQuoteFallback: replyQuoteFallback,
		replyMediaBlocks:   replyMediaBlocks,
		activityVersion:    admissionVersion,
	}}
	pending[0].activityTracked = true
	handoffAdmission = true
	us.debounce.Add(pending[0])
}

// uuidInTextRE matches any plausible attachment UUID inside user
// text — used by resolveInlineAttachmentRefs so a user can paste an
// id ("read abc-…", "what's in the image abc-…") and the gateway
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
// user's question ("what's in the image UUID" reads naturally with the
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
	if bi == nil || bi.id == uuid.Nil {
		return nil, fmt.Errorf("gateway: telegram bot identity is required")
	}
	cacheKey := telegramUserCacheKey(bi.id, chatID)

	g.mu.Lock()
	defer g.mu.Unlock()

	if us, ok := g.users[cacheKey]; ok {
		// A bot reload can replace the client instance while preserving its
		// database id. Keep the pair-scoped state but refresh that transport.
		us.bot = bi
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
		// Each bot/chat pair owns a separate debounce queue and UserState, even
		// though Telegram gives all of a person's private chats the same id.
		// Resolve the client at flush time so bot-token reloads take effect.
		g.mu.Lock()
		flushBot := us.bot
		g.mu.Unlock()
		if flushBot == nil {
			flushBot = bi
		}
		sink := g.newTelegramSink(chatID, flushBot)
		go g.processMessages(ctx, us, msgs, sink)
	})

	g.users[cacheKey] = us
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
	// Logged, at Info, because this is the one path that answers somebody
	// while leaving no trace of having done it. Everything upstream —
	// onboarding, the deeplink hooks, identity resolution — has already
	// declined the message by the time we get here, so their logs say
	// nothing either. A user reporting an unexplained reply produced a
	// clean log, and a clean log reads exactly like "that never happened".
	// It cost two wrong diagnoses before anyone noticed the path was mute.
	g.logger.Info("gateway: replying to an unpaired chat",
		"chat_id", chatID, "bot_id", bi.id.String(), "bot_kind", bi.kind)

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
	hasActive := false
	for _, r := range rules {
		if !r.Suppressed {
			hasActive = true
			break
		}
	}
	if !hasActive {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Active rules\n")
	order := 0
	for _, r := range rules {
		if r.Suppressed {
			continue
		}
		order++
		appendActiveRuleGuidance(&b, order, r)
	}
	return b.String()
}

func appendActiveRuleGuidance(b *strings.Builder, order int, r bs.ActiveRule) {
	appendActiveRuleHeader(b, order, r)
	if r.Trigger != "" {
		fmt.Fprintf(b, "WHEN: %s\n", r.Trigger)
	}
	if r.Action != "" {
		fmt.Fprintf(b, "DO: %s\n", r.Action)
	}
	if len(r.Tools) > 0 {
		fmt.Fprintf(b, "TOOLS: %s\n", strings.Join(r.Tools, ", "))
	}
	b.WriteString("\n")
}

func appendActiveRuleHeader(b *strings.Builder, order int, r bs.ActiveRule) {
	meta := activeRuleMeta(r)
	if meta != "" {
		fmt.Fprintf(b, "RULE #%d (%s)\n", order, meta)
	} else {
		fmt.Fprintf(b, "RULE #%d\n", order)
	}
}

func appendRuleGuidance(b *strings.Builder, order int, trigger, action, matchType, scope, reason string) {
	appendRuleHeader(b, order, matchType, scope, reason)
	if trigger != "" {
		fmt.Fprintf(b, "WHEN: %s\n", trigger)
	}
	if action != "" {
		fmt.Fprintf(b, "DO: %s\n", action)
	}
	b.WriteString("\n")
}

func appendRuleHeader(b *strings.Builder, order int, matchType, scope, reason string) {
	meta := ruleMeta(matchType, scope, reason)
	if meta != "" {
		fmt.Fprintf(b, "RULE #%d (%s)\n", order, meta)
	} else {
		fmt.Fprintf(b, "RULE #%d\n", order)
	}
}

func matchedRuleFromActive(r bs.ActiveRule, source string, order int) bs.MatchedRule {
	rank := r.Rank
	if rank == 0 {
		rank = order
	}
	return bs.MatchedRule{
		ID:               r.ID,
		Trigger:          r.Trigger,
		Action:           r.Action,
		Source:           source,
		MatchType:        r.MatchType,
		Scope:            r.Scope,
		Reason:           r.Reason,
		Rank:             rank,
		Disposition:      r.Disposition,
		Anchor:           r.Anchor,
		EligibilityScore: r.EligibilityScore,
		Suppressed:       r.Suppressed,
		SuppressedReason: r.SuppressedReason,
	}
}

func activeRuleMeta(r bs.ActiveRule) string {
	parts := ruleMetaParts(r.MatchType, r.Scope, r.Reason)
	if r.Disposition != "" {
		parts = append(parts, "disposition="+r.Disposition)
	}
	if r.Anchor != "" {
		parts = append(parts, "anchor="+quoteRuleMeta(r.Anchor))
	}
	if r.EligibilityScore > 0 {
		parts = append(parts, fmt.Sprintf("score=%.2f", r.EligibilityScore))
	}
	return strings.Join(parts, "; ")
}

func ruleMeta(matchType, scope, reason string) string {
	return strings.Join(ruleMetaParts(matchType, scope, reason), "; ")
}

func ruleMetaParts(matchType, scope, reason string) []string {
	parts := make([]string, 0, 3)
	if matchType != "" {
		parts = append(parts, "match="+matchType)
	}
	if scope != "" {
		parts = append(parts, "scope="+scope)
	}
	if reason != "" {
		parts = append(parts, "reason="+quoteRuleMeta(reason))
	}
	return parts
}

func quoteRuleMeta(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `'`) + `"`
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

// startPayloadCommand reads a deep link that names a host command.
//
// A deep link carries exactly one thing — the /start payload — and it is
// the only way one chat can hand a person to another. A host needs that
// when the bot someone is in cannot finish what they came to do: an
// invoice is paid to the bot that created it, so a purchase begun on
// somebody else's bot has to be completed on the seller's.
//
// Only host-answered commands qualify. Anything else stays a payload and
// reaches acquisition reporting unchanged, which is what most of them
// are.
func (g *Gateway) startPayloadCommand(text string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) != 2 {
		return "", false
	}
	head := strings.ToLower(fields[0])
	if at := strings.IndexByte(head, '@'); at >= 0 {
		head = head[:at]
	}
	if head != "/start" {
		return "", false
	}
	payload := strings.ToLower(fields[1])
	if !g.isHostCommand(payload) {
		return "", false
	}
	return payload, true
}
