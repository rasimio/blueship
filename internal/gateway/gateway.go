package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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

	if cfg.Prompts != "" {
		if err := gw.loadSystemPrompts(cfg.Prompts); err != nil {
			return nil, fmt.Errorf("load system prompts: %w", err)
		}
	}

	return gw, nil
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

	// Load compact prompt (optional — compactor works without it but with empty system prompt)
	if g.compactor != nil {
		compactPath := filepath.Join(workspacePath, "prompts", "compact.md")
		if data, err := os.ReadFile(compactPath); err == nil {
			g.compactor.SetSystemPrompt(string(data))
		} else {
			g.logger.Warn("compact prompt not found, compaction will use empty system prompt", "path", compactPath)
		}
	}

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

	if text == "" {
		return
	}

	chatID := msg.Chat.ID

	if text == "/session" {
		go g.handleSessionCommand(ctx, chatID)
		return
	}

	us, err := g.getOrInitUser(ctx, chatID)
	if err != nil {
		g.logger.Debug("ignored message", "chat_id", chatID, "error", err)
		return
	}

	us.debounce.Add(pendingMsg{
		text:      text,
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
	coreDB.GetContext(ctx, &isOwner,
		`SELECT is_owner FROM user_profiles WHERE id = $1`, userID.String())

	userDeps := g.deps.ForUser(userID, chatIDStr, isOwner)
	registry := bs.NewToolRegistry()
	tool.RegisterBuiltinTools(registry, userDeps)
	g.modules.RegisterAllTools(registry, userDeps)

	us := &UserState{
		ChatID:   chatID,
		UserID:   userID,
		IsOwner:  isOwner,
		Registry: registry,
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

func (g *Gateway) processMessages(ctx context.Context, us *UserState, msgs []pendingMsg) {
	us.Mu.Lock()
	defer us.Mu.Unlock()
	us.LoopBusy = true
	defer func() { us.LoopBusy = false }()

	typingCtx, stopTyping := context.WithCancel(ctx)
	go g.keepTyping(typingCtx, us.ChatID)
	defer stopTyping()

	texts := make([]string, len(msgs))
	for i, m := range msgs {
		texts[i] = m.text
	}
	joined := strings.Join(texts, "\n")

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Error("session error", "error", err)
		g.tg.SendLong(ctx, us.ChatID, "Internal error: failed to get session")
		return
	}

	g.logger.Info("processing message",
		"chat_id", us.ChatID,
		"session_id", sess.ID,
		"messages", len(msgs),
		"text_length", len(joined),
	)

	loop := agent.NewLoop(g.provider, g.store, us.Registry, g.deps.Config, g.logger)
	loop.SetCompactor(g.compactor)

	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:      sess.ID,
		SystemPrompt:   g.systemPrompt,
		CompactSummary: derefString(sess.CompactSummary),
		Model:          g.deps.Config.Models.Primary,
		MaxTokens:      g.deps.Config.Limits.MaxOutputTokens,
		MaxTurns:       g.deps.Config.Gateway.MaxTurns,
	}, joined)
	if err != nil {
		g.logger.Error("agent loop error",
			"chat_id", us.ChatID,
			"error", err,
		)
		g.tg.SendLong(ctx, us.ChatID, "Sorry, something went wrong internally.")
		return
	}

	if reply != "" {
		if err := g.tg.SendLong(ctx, us.ChatID, reply); err != nil {
			g.logger.Error("send reply error", "chat_id", us.ChatID, "error", err)
		}
	}
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
	sess, err := g.store.GetOrCreate(ctx, us.UserID.String(), g.deps.Config.Models.Primary)
	if err != nil {
		return nil, err
	}

	resetTime := g.todayResetTime()
	if sess.CreatedAt.Before(resetTime) {
		if err := g.store.Archive(ctx, sess.ID); err != nil {
			return nil, fmt.Errorf("archive old session: %w", err)
		}
		return g.store.Create(ctx, us.UserID.String(), g.deps.Config.Models.Primary)
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

	apiKey := ""
	maskedKey := "***"
	_ = apiKey
	_ = maskedKey

	realTokens := sess.TokenCount * 2
	maxTokens := 200000
	pct := 0
	if maxTokens > 0 {
		pct = realTokens * 100 / maxTokens
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
		g.deps.Config.Models.Primary,
		sess.MessageCount, sess.TokenCount/1000,
		realTokens/1000, maxTokens/1000, pct,
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
