package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/rasimio/blueship/agent"
	bs "github.com/rasimio/blueship/core"
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

	systemPrompt          string
	systemPromptHeartbeat string

	// Reflex pipeline prompts (loaded from system_prompts table).
	reflexSystemPrompt   string // system prompt for reflex LLM call
	reflexPlanTemplate   string // user prompt template (has %s placeholders for rules, tools, message)
	extractInsightPrompt string // system prompt for insight extraction

	mu    sync.Mutex
	users map[int64]*UserState
}

// UserState holds per-user runtime state.
type UserState struct {
	Mu       sync.Mutex
	ChatID   int64
	UserID   uuid.UUID
	IsOwner  bool
	Registry *bs.ToolRegistry
	Deps     *bs.Deps // per-user deps (carries ContextInjector set by modules)
	LoopBusy bool
	debounce *debouncer
}

// ModuleRegistry is an adapter interface for the module system.
type ModuleRegistry interface {
	RegisterAllTools(registry *bs.ToolRegistry, d *bs.Deps)
}

// NewGateway creates a new gateway.
func NewGateway(deps *bs.Deps, modules ModuleRegistry, logger *slog.Logger) (*Gateway, error) {
	cfg := deps.Config
	if cfg.Transport.BotToken == "" {
		return nil, fmt.Errorf("telegram bot_token not configured")
	}

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
		poller:    telegram.NewPoller(cfg.Transport.BotToken, cfg.Timeouts.TelegramPoll),
		tg:        telegram.NewClient(cfg.Transport.BotToken, cfg.Timeouts.TelegramClient),
		store:     session.NewStore(coreDB),
		provider:  cfg.LLM,
		compactor: agent.NewCompactor(cfg.LLM, cfg, logger),
		whisper:   whisperProvider,
		tz:        tz,
		logger:    logger,
		users:     make(map[int64]*UserState),
	}

	// Load system prompts: DB first, filesystem second, error if neither
	dbErr := gw.loadSystemPromptsFromDB(coreDB)
	if dbErr != nil {
		if cfg.Prompts != "" {
			gw.logger.Info("DB prompts not available, loading from filesystem", "error", dbErr)
			if err := gw.loadSystemPrompts(cfg.Prompts); err != nil {
				return nil, fmt.Errorf("load system prompts from filesystem: %w", err)
			}
		} else {
			return nil, fmt.Errorf("system prompts not configured: populate system_prompts table or set Prompts path (%w)", dbErr)
		}
	}

	// Load compact prompt from filesystem (utility prompt, not personality)
	if gw.compactor != nil && cfg.Prompts != "" {
		compactPath := filepath.Join(cfg.Prompts, "prompts", "compact.md")
		if data, err := os.ReadFile(compactPath); err == nil {
			gw.compactor.SetSystemPrompt(string(data))
		} else {
			gw.logger.Warn("compact prompt not found", "path", compactPath)
		}
	}

	return gw, nil
}

func (g *Gateway) loadSystemPromptsFromDB(db *sqlx.DB) error {
	rows, err := db.Queryx("SELECT key, content FROM system_prompts WHERE content <> ''")
	if err != nil {
		return fmt.Errorf("query system_prompts: %w", err)
	}
	defer rows.Close()

	prompts := make(map[string]string)
	for rows.Next() {
		var key, content string
		if err := rows.Scan(&key, &content); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		prompts[key] = content
	}

	// Require at least preamble + soul + agents
	for _, required := range []string{"preamble", "soul", "agents"} {
		if prompts[required] == "" {
			return fmt.Errorf("missing required prompt: %s", required)
		}
	}

	preamble := prompts["preamble"] + "\n"
	g.systemPrompt = preamble + prompts["soul"] + "\n\n" + prompts["agents"]
	g.systemPromptHeartbeat = g.systemPrompt
	if prompts["heartbeat"] != "" {
		g.systemPromptHeartbeat = g.systemPrompt + "\n\n" + prompts["heartbeat"]
	}
	if g.compactor != nil && prompts["compact"] != "" {
		g.compactor.SetSystemPrompt(prompts["compact"])
	}

	// Reflex pipeline prompts (optional — defaults used if not in DB).
	if prompts["reflex-system"] != "" {
		g.reflexSystemPrompt = prompts["reflex-system"]
	}
	if prompts["reflex-plan"] != "" {
		g.reflexPlanTemplate = prompts["reflex-plan"]
	}
	if prompts["extract-insight"] != "" {
		g.extractInsightPrompt = prompts["extract-insight"]
	}

	g.logger.Info("system prompts loaded from DB",
		"preamble", len(prompts["preamble"]),
		"soul", len(prompts["soul"]),
		"agents", len(prompts["agents"]),
		"heartbeat", len(prompts["heartbeat"]),
		"thinking", len(prompts["thinking"]),
		"compact", len(prompts["compact"]),
		"reflex-system", len(prompts["reflex-system"]),
		"reflex-plan", len(prompts["reflex-plan"]),
		"extract-insight", len(prompts["extract-insight"]),
	)
	return nil
}

func (g *Gateway) loadSystemPrompts(workspacePath string) error {
	preamble, err := os.ReadFile(filepath.Join(workspacePath, "PREAMBLE.md"))
	if err != nil {
		return fmt.Errorf("read PREAMBLE.md: %w", err)
	}
	soul, err := os.ReadFile(filepath.Join(workspacePath, "SOUL.md"))
	if err != nil {
		return fmt.Errorf("read SOUL.md: %w", err)
	}
	agents, err := os.ReadFile(filepath.Join(workspacePath, "AGENTS.md"))
	if err != nil {
		return fmt.Errorf("read AGENTS.md: %w", err)
	}
	heartbeat, err := os.ReadFile(filepath.Join(workspacePath, "HEARTBEAT.md"))
	if err != nil {
		return fmt.Errorf("read HEARTBEAT.md: %w", err)
	}

	preambleStr := string(preamble) + "\n"
	g.systemPrompt = preambleStr + string(soul) + "\n\n" + string(agents)
	g.systemPromptHeartbeat = preambleStr + string(soul) + "\n\n" + string(agents) + "\n\n" + string(heartbeat)

	// Thinking prompt (optional — fall back to regular system prompt if missing)
	return nil
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
			text = fmt.Sprintf("[reply to: %s]\n\n%s", quoted, text)
		}
	}

	if text == "" && len(images) == 0 {
		return
	}

	chatID := msg.Chat.ID

	if text == "/session" {
		go g.handleSessionCommand(ctx, chatID)
		return
	}
	if text == "/reset" {
		go g.handleResetCommand(ctx, chatID)
		return
	}
	if text == "/model" {
		go g.handleModelCommand(ctx, chatID)
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

func (g *Gateway) getOrInitUser(ctx context.Context, chatID int64) (*UserState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if us, ok := g.users[chatID]; ok {
		return us, nil
	}

	coreDB, err := g.deps.DB("ship")
	if err != nil {
		return nil, fmt.Errorf("core DB: %w", err)
	}

	chatIDStr := fmt.Sprintf("telegram:%d", chatID)
	userID, err := user.ResolveByChatID(ctx, coreDB, chatIDStr)
	if err != nil {
		return nil, fmt.Errorf("resolve user: %w", err)
	}

	var isOwner bool
	if err := coreDB.GetContext(ctx, &isOwner,
		`SELECT is_owner FROM user_profiles WHERE id = $1`, userID.String()); err != nil {
		g.logger.Warn("is_owner lookup failed, defaulting to false", "user_id", userID, "error", err)
	}

	// Single-user mode: reject non-owner messages
	if !isOwner {
		g.logger.Info("rejected non-owner message", "chat_id", chatID, "user_id", userID.String())
		_ = g.tg.SendLong(ctx, chatID, "This bot is private.")
		return nil, fmt.Errorf("non-owner user rejected")
	}

	userDeps := g.deps.ForUser(userID, chatIDStr, isOwner)
	registry := bs.NewToolRegistry()
	tool.RegisterBuiltinTools(registry, userDeps)
	g.modules.RegisterAllTools(registry, userDeps)

	us := &UserState{
		ChatID:   chatID,
		UserID:   userID,
		IsOwner:  isOwner,
		Registry: registry,
		Deps:     userDeps,
	}

	us.debounce = newDebouncer(g.deps.Config.Gateway.DebounceWindow, g.deps.Config.Gateway.DebounceCap, func(msgs []pendingMsg) {
		go g.processMessages(ctx, us, msgs)
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
func (g *Gateway) GetUser(chatID int64) *UserState {
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

// sendDebugError sends the actual error to the owner via Telegram when debug mode is on.
// For non-owner users, always sends the generic "Sorry" message.
func (g *Gateway) sendDebugError(ctx context.Context, chatID int64, source string, err error) {
	if g.deps.Config.Gateway.Debug {
		msg := fmt.Sprintf("[%s] %v", source, err)
		g.tg.SendLong(ctx, chatID, msg)
	} else {
		g.tg.SendLong(ctx, chatID, "Sorry, something went wrong internally.")
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
	g.tg.SendLong(ctx, owner.ChatID, msg)
}

func (g *Gateway) processMessages(ctx context.Context, us *UserState, msgs []pendingMsg) {
	us.Mu.Lock()
	defer us.Mu.Unlock()
	us.LoopBusy = true
	defer func() { us.LoopBusy = false }()

	typingCtx, stopTyping := context.WithCancel(ctx)
	go g.keepTyping(typingCtx, us.ChatID)
	defer stopTyping()

	var blocks []bs.ContentBlock
	for _, m := range msgs {
		blocks = append(blocks, m.images...)
		if m.text != "" {
			blocks = append(blocks, bs.ContentBlock{Type: "text", Text: m.text})
		}
	}

	var content any
	if len(blocks) == 1 && blocks[0].Type == "text" {
		content = blocks[0].Text // backwards compat: plain string
	} else {
		content = blocks
	}

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Error("session error", "error", err)
		g.sendDebugError(ctx, us.ChatID, "session", err)
		return
	}

	g.logger.Info("processing message",
		"chat_id", us.ChatID,
		"session_id", sess.ID,
		"messages", len(msgs),
		"blocks", len(blocks),
	)

	// Collect message text for context injection.
	msgText := ""
	for _, m := range msgs {
		if m.text != "" {
			if msgText != "" {
				msgText += " "
			}
			msgText += m.text
		}
	}

	// Build context and run reflex/cortex pipeline.
	var injectedCtx, reflexGuidance string
	var toolOverride []string       // nil = use role default
	var postActions []bs.PostAction // executed after cortex response

	if msgText != "" && us.Deps != nil && us.Deps.ReflexPreparer != nil && g.reflexModel() != "" {
		// Reflex/Cortex pipeline: structured context → reflex plan → pre-actions → filtered cortex input.
		injectedCtx, reflexGuidance, toolOverride, postActions = g.runReflexPipeline(ctx, us, msgText)
	} else if msgText != "" && us.Deps != nil && us.Deps.ContextInjector != nil {
		// Fallback: legacy ContextInjector (no reflex).
		injectedCtx = us.Deps.ContextInjector(ctx, us.UserID.String(), msgText)
	}

	loop := agent.NewLoop(g.provider, g.store, us.Registry, g.deps.RoleTools, g.deps.Config, g.logger)
	loop.SetCompactor(g.compactor)

	// Inject current datetime into system prompt so the model always knows "today".
	now := time.Now().In(g.tz)
	systemWithTime := fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), g.systemPrompt)

	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:       sess.ID,
		SystemPrompt:    systemWithTime,
		CompactSummary:  derefString(sess.CompactSummary),
		Model:           g.cortexModel(),
		MaxTokens:       g.deps.Config.Limits.MaxOutputTokens,
		MaxTurns:        g.deps.Config.Gateway.MaxTurns,
		InjectedContext: injectedCtx,
		ReflexGuidance:  reflexGuidance,
		Role:            "cortex",
		ToolOverride:    toolOverride,
	}, content)
	if err != nil {
		g.logger.Error("agent loop error",
			"chat_id", us.ChatID,
			"error", err,
		)
		g.sendDebugError(ctx, us.ChatID, "agent", err)
		return
	}

	// Execute post-actions (save reflection, etc.) using cortex response.
	if reply != "" && len(postActions) > 0 {
		g.executePostActions(ctx, us, postActions, reply)
	}

	if reply != "" {
		if err := g.tg.SendLong(ctx, us.ChatID, reply); err != nil {
			g.logger.Error("send reply error", "chat_id", us.ChatID, "error", err)
		}
	}
}

const reflexConfidenceThreshold = 0.7
const preActionTimeout = 10 * time.Second
const maxPreActions = 2


// runReflexPipeline executes the System 1/2 pipeline:
// 1. ReflexPreparer → structured context (traces + candidate rules)
// 2. Reflex LLM (Gemini Flash) → plan (matched rules, pre/post actions, tools)
// 3. Execute pre-actions (web_search etc.) → inject results into context
// 4. Build cortex context: matched rules + research + AME traces
// Returns (injectedContext, reflexGuidance, toolOverride, postActions).
func (g *Gateway) runReflexPipeline(ctx context.Context, us *UserState, msgText string) (string, string, []string, []bs.PostAction) {
	rc := us.Deps.ReflexPreparer(ctx, us.UserID.String(), msgText)
	if rc == nil {
		return "", "", nil, nil
	}

	// No rules to classify — skip reflex, use full context.
	if len(rc.CandidateRules) == 0 {
		return rc.FullContext, "", nil, nil
	}

	// Build reflex prompt.
	var rulesBlock strings.Builder
	for _, r := range rc.CandidateRules {
		fmt.Fprintf(&rulesBlock, "[%s] WHEN: %s → DO: %s (sr=%.0f%%)\n",
			r.ID, r.Trigger, r.Action, r.SuccessRate*100)
	}

	toolsList := "none configured"
	if g.deps.RoleTools != nil {
		if names := g.deps.RoleTools.Get("cortex"); len(names) > 0 {
			toolsList = strings.Join(names, ", ")
		}
	}

	if g.reflexPlanTemplate == "" {
		g.logger.Warn("reflex-plan prompt not in DB, skipping reflex")
		return rc.FullContext, "", nil, nil
	}
	reflexPrompt := fmt.Sprintf(g.reflexPlanTemplate, rulesBlock.String(), toolsList, msgText)

	reflexResult, err := g.callReflex(ctx, reflexPrompt)
	if err != nil {
		g.logger.Warn("reflex failed, using full context", "error", err)
		return rc.FullContext, "", nil, nil
	}

	g.logger.Info("reflex plan",
		"intent", reflexResult.Intent,
		"confidence", reflexResult.Confidence,
		"matched_rules", reflexResult.MatchedRules,
		"pre_actions", len(reflexResult.PreActions),
		"post_actions", len(reflexResult.PostActions),
		"tools", reflexResult.Tools,
	)

	// Low confidence → full fallback (all context, all role tools, no pre/post actions).
	if reflexResult.Confidence < reflexConfidenceThreshold {
		g.logger.Info("reflex low confidence, full fallback",
			"confidence", reflexResult.Confidence,
		)
		return rc.FullContext, "", nil, nil
	}

	// Execute pre-actions (web_search etc.) with timeout.
	var researchBlock strings.Builder
	preActionsToRun := reflexResult.PreActions
	if len(preActionsToRun) > maxPreActions {
		preActionsToRun = preActionsToRun[:maxPreActions]
	}
	for _, pa := range preActionsToRun {
		paCtx, cancel := context.WithTimeout(ctx, preActionTimeout)
		result, isError := us.Registry.Execute(paCtx, pa.Tool, pa.Input)
		cancel()
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

	// Expand matched rules into directive block.
	var guidance strings.Builder
	if len(reflexResult.MatchedRules) > 0 {
		matchedSet := make(map[string]bool, len(reflexResult.MatchedRules))
		for _, id := range reflexResult.MatchedRules {
			matchedSet[id] = true
		}
		guidance.WriteString("[active rules]\n")
		for _, r := range rc.CandidateRules {
			if matchedSet[r.ID] {
				fmt.Fprintf(&guidance, "WHEN: %s\nDO: %s\n\n", r.Trigger, r.Action)
			}
		}
		guidance.WriteString("[/active rules]")
	}

	// Assemble: guidance (rules) + research + traces
	if researchBlock.Len() > 0 {
		guidance.WriteString("\n\n")
		guidance.WriteString(researchBlock.String())
	}

	// Tool override: only apply if reflex explicitly selected tools.
	// Empty slice from JSON ("tools":[]") means "unspecified", not "no tools".
	var toolOverride []string
	if len(reflexResult.Tools) > 0 {
		toolOverride = reflexResult.Tools
	}

	// Close research block if any pre-actions produced results.
	if researchBlock.Len() > 0 {
		researchBlock.WriteString("[/research]")
	}

	return rc.FormattedTraces, guidance.String(), toolOverride, reflexResult.PostActions
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
			input := fmt.Sprintf(`{"type":"observation","content":%q}`, insight)
			result, isError := us.Registry.Execute(ctx, "memory_self_save", json.RawMessage(input))
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
	g.logger.Info("extractInsight done", "type", extractType, "result", truncateStr(text, 100))
	return text
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

	resp, err := g.provider.Complete(ctx, bs.CompletionRequest{
		Model:     model,
		MaxTokens: 512,
		System:    g.reflexSystemPrompt,
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
		MatchedRules []string          `json:"matched_rules"`
		Intent       string            `json:"intent"`
		Confidence   float64           `json:"confidence"`
		PreActions   []bs.ToolAction   `json:"pre_actions"`
		PostActions  []bs.PostAction   `json:"post_actions"`
		Tools        json.RawMessage   `json:"tools"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("parse reflex JSON %q: %w", text, err)
	}

	result := &bs.ReflexResult{
		MatchedRules: raw.MatchedRules,
		Intent:       raw.Intent,
		Confidence:   raw.Confidence,
		PreActions:   raw.PreActions,
		PostActions:  raw.PostActions,
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

func (g *Gateway) keepTyping(ctx context.Context, chatID int64) {
	_ = g.tg.SendChatAction(ctx, chatID, "typing")
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = g.tg.SendChatAction(ctx, chatID, "typing")
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
	us, err := g.getOrInitUser(ctx, chatID)
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

func (g *Gateway) cortexModelDisplay() string {
	if g.deps.ModelStore != nil {
		if ref := g.deps.ModelStore.Get("cortex"); ref.Name != "" {
			return ref.Name
		}
	}
	return g.deps.Config.Models.Primary.Name
}

func (g *Gateway) handleResetCommand(ctx context.Context, chatID int64) {
	us, err := g.getOrInitUser(ctx, chatID)
	if err != nil {
		g.logger.Debug("reset command: ignored", "chat_id", chatID, "error", err)
		return
	}

	// Refresh model config and role tools from DB
	if g.deps.ModelStore != nil {
		if err := g.deps.ModelStore.Refresh(ctx); err != nil {
			g.logger.Warn("reset: failed to refresh model config", "error", err)
		}
	}
	if g.deps.RoleTools != nil {
		if err := g.deps.RoleTools.Refresh(ctx); err != nil {
			g.logger.Warn("reset: failed to refresh role tools", "error", err)
		}
	}

	// Archive current session
	sess, err := g.store.GetOrCreate(ctx, us.UserID.String(), g.cortexModel())
	if err == nil && sess != nil {
		_ = g.store.Archive(ctx, sess.ID)
		g.logger.Info("reset: archived session",
			"chat_id", chatID,
			"session_id", sess.ID,
			"messages", sess.MessageCount,
		)
	}

	model := g.cortexModelDisplay()
	msg := fmt.Sprintf("Session reset.\nModel: %s", model)
	if err := g.tg.SendLong(ctx, chatID, msg); err != nil {
		g.logger.Error("reset command: send error", "error", err)
	}
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
