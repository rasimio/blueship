package blueship

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/anthropic"
	"github.com/rasimio/blueship/internal/gemini"
	"github.com/rasimio/blueship/internal/gateway"
	"github.com/rasimio/blueship/internal/infrastructure/ws"
	"github.com/rasimio/blueship/internal/openai"
	"github.com/rasimio/blueship/internal/scheduler"
	"github.com/rasimio/blueship/internal/telegram"
	"github.com/rasimio/blueship/internal/user"
	"github.com/rasimio/blueship/internal/web"
	"github.com/rasimio/blueship/migrate"
	"github.com/rasimio/blueship/session"
	"github.com/rasimio/blueship/tool"
)

// Ship is the main BlueShip runtime instance.
type Ship struct {
	cfg      Config
	modules  []Module
	handlers map[string]core.AgentHandler
	logger   *slog.Logger
}

// New creates a new BlueShip instance with the given configuration.
func New(cfg Config) *Ship {
	cfg.ApplyDefaults()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	return &Ship{
		cfg:    cfg,
		logger: logger,
	}
}

// RegisterModule adds a module to the BlueShip instance.
func (s *Ship) RegisterModule(m Module) {
	s.modules = append(s.modules, m)
}

// RegisterAgentHandler registers a named handler for autonomous agent tasks.
// Handlers are dispatched by the agent task scheduler based on the handler field in agent_tasks.
func (s *Ship) RegisterAgentHandler(name string, h core.AgentHandler) {
	if s.handlers == nil {
		s.handlers = make(map[string]core.AgentHandler)
	}
	s.handlers[name] = h
}

// Run starts BlueShip: connects to DB, initializes providers, starts transport, runs jobs.
// Blocks until ctx is done.
func (s *Ship) Run(ctx context.Context) error {
	s.logger.Info("starting blueship")

	// 1. Initialize deps
	deps, err := InitDeps(&s.cfg, s.logger)
	if err != nil {
		return fmt.Errorf("init deps: %w", err)
	}
	defer deps.Close()

	// 2. Auto-migrate runtime tables
	shipDB, err := deps.DB("ship")
	if err != nil {
		return fmt.Errorf("ship DB: %w", err)
	}
	if err := migrate.Run(shipDB, s.logger); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}

	// 2b. Load model config from DB (overrides Config.Models at runtime)
	modelStore := core.NewModelConfigStore(shipDB)
	if err := modelStore.Load(ctx); err != nil {
		s.logger.Warn("model_config not loaded, using config defaults", "error", err)
	} else {
		deps.ModelStore = modelStore
		// Override Config.Models so all consumers see DB values.
		// "cortex" role maps to Config.Models.Primary (backwards compat).
		if ref := modelStore.Get("cortex"); ref.Name != "" {
			deps.Config.Models.Primary = ref
		}
		if ref := modelStore.Get("compact"); ref.Name != "" {
			deps.Config.Models.Compact = ref
		}
	}

	// 2c. Load role-based tool assignments from DB
	roleToolStore := core.NewRoleToolStore(shipDB)
	if err := roleToolStore.Load(ctx); err != nil {
		s.logger.Warn("role_tools not loaded, all tools enabled for all roles", "error", err)
	} else {
		deps.RoleTools = roleToolStore
	}

	// 2d. Initialize stores for ship DB data (prompts, users, sessions).
	deps.Prompts = core.NewPromptStore(shipDB)
	deps.Users = core.NewUserStore(shipDB)
	deps.Sessions = session.NewStore(shipDB)

	// 3. Ensure/resolve owner user
	var uid uuid.UUID
	if s.cfg.Owner.ChatID != "" {
		uid, err = user.EnsureOwner(ctx, shipDB, s.cfg.Owner.ChatID, s.cfg.Owner.DisplayName)
		if err != nil {
			return fmt.Errorf("ensure owner: %w", err)
		}
	} else {
		uid, err = user.ResolveOwner(ctx, shipDB)
		if err != nil {
			return fmt.Errorf("resolve owner: %w", err)
		}
	}
	deps.UserID = uid
	s.logger.Info("running as owner", "user_id", uid.String())

	// 3. Create module registry adapter
	reg := &moduleRegistry{
		modules: s.modules,
	}

	// 4. Start background jobs from modules
	var wg sync.WaitGroup
	for _, m := range s.modules {
		if jp, ok := m.(JobProvider); ok {
			for _, job := range jp.Jobs(deps) {
				wg.Add(1)
				go func(j Job) {
					defer wg.Done()
					scheduler.RunLoop(ctx, s.logger, j.Name, j.Interval, j.Run)
				}(job)
			}
		}
	}

	// 4b. Start agent task scheduler (if handlers registered).
	if len(s.handlers) > 0 {
		// Build a global tool registry for agent tasks.
		globalRegistry := core.NewToolRegistry()
		tool.RegisterBuiltinTools(globalRegistry, deps)
		reg.RegisterAllTools(globalRegistry, deps)

		taskStore := core.NewAgentTaskStore(shipDB)
		msgStore := session.NewStore(shipDB) // MessageStore for agent loops

		// Notification callback: append to chat session (so cortex sees it) + send to Telegram.
		var notifyFn func(ctx context.Context, userID uuid.UUID, text string)
		if deps.Sender != nil && deps.Users != nil {
			notifyFn = func(ctx context.Context, userID uuid.UUID, text string) {
				profile, err := deps.Users.GetByID(ctx, userID.String())
				if err != nil {
					s.logger.Warn("agent-tasks: user lookup for notify failed", "error", err)
					return
				}

				// Append to active chat session so cortex sees it in conversation history.
				uid := userID.String()
				var sessID string
				_ = shipDB.GetContext(ctx, &sessID,
					`SELECT id FROM chat_sessions WHERE user_id = $1 AND source = 'chat' AND active = true ORDER BY updated_at DESC LIMIT 1`, uid)
				if sessID != "" {
					_ = msgStore.Append(ctx, sessID, core.Message{
						Role:    "assistant",
						Content: core.NormalizeContent(text),
					})
				}

				// Send to Telegram.
				chatID := profile.ChatID
				if idx := strings.Index(chatID, ":"); idx >= 0 {
					chatID = chatID[idx+1:]
				}
				if err := deps.Sender.SendLong(ctx, chatID, text); err != nil {
					s.logger.Warn("agent-tasks: notify failed", "error", err)
				}
			}
		}

		agentSched := agenttask.NewScheduler(taskStore, s.handlers, globalRegistry, msgStore, deps, notifyFn, s.logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			scheduler.RunLoop(ctx, s.logger, "agent-tasks", 1*time.Minute, agentSched.Run)
		}()
	}

	// 5. Start Telegram Gateway
	var gw *gateway.Gateway
	if s.cfg.Transport.Type == "telegram" && s.cfg.Transport.BotToken != "" {
		var err error
		gw, err = gateway.NewGateway(deps, reg, s.logger)
		if err != nil {
			return fmt.Errorf("create gateway: %w", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			gw.Run(ctx)
		}()
	}

	// 6. Start WebSocket server (voice/desktop clients)
	if wsCfg := s.cfg.Transport.WebSocket; wsCfg.Port > 0 && gw != nil {
		wsSrv := ws.NewServer(gw, wsCfg, s.logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := wsSrv.Run(ctx); err != nil {
				s.logger.Error("websocket server error", "error", err)
			}
		}()
	}

	// 7. Block until done
	<-ctx.Done()
	s.logger.Info("shutting down, waiting for jobs...")
	wg.Wait()
	s.logger.Info("blueship stopped")
	return nil
}

// --- Module registry adapter ---

type moduleRegistry struct {
	modules []Module
}

func (r *moduleRegistry) RegisterAllTools(registry *ToolRegistry, d *Deps) {
	for _, m := range r.modules {
		if tp, ok := m.(ToolProvider); ok {
			tp.RegisterTools(registry, d)
		}
	}
}

// --- Convenience constructors for Config ---

// Anthropic creates a CompletionProvider using the Anthropic Messages API.
func Anthropic(apiKey string) CompletionProvider {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return anthropic.NewProvider(
		apiKey,
		120*time.Second,
		[]time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second},
		logger,
	)
}

// AnthropicWithConfig creates a CompletionProvider with custom timeout and retry settings.
func AnthropicWithConfig(apiKey string, timeout time.Duration, backoffs []time.Duration) CompletionProvider {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return anthropic.NewProvider(apiKey, timeout, backoffs, logger)
}
// OpenAI creates a CompletionProvider using OpenAI Chat Completions.
func OpenAI(apiKey string) CompletionProvider {
	return openai.NewCompletionProvider(apiKey, 120*time.Second)
}

// OpenAIWithConfig creates a CompletionProvider with a custom timeout.
func OpenAIWithConfig(apiKey string, timeout time.Duration) CompletionProvider {
	return openai.NewCompletionProvider(apiKey, timeout)
}

// OpenAICompatible creates a CompletionProvider for any OpenAI-compatible API
// (MLX, vLLM, Ollama, LM Studio, etc.). Pass empty apiKey if auth is not required.
// extraParams are merged into every request JSON (e.g. for chat_template_kwargs).
func OpenAICompatible(baseURL, apiKey string, timeout time.Duration, extraParams map[string]any) CompletionProvider {
	return openai.NewCompatibleProvider(baseURL, apiKey, timeout, extraParams)
}

// Gemini creates a CompletionProvider using Gemini generateContent.
func Gemini(apiKey string) CompletionProvider {
	return gemini.NewCompletionProvider(apiKey, 120*time.Second)
}

// GeminiWithConfig creates a CompletionProvider with a custom timeout.
func GeminiWithConfig(apiKey string, timeout time.Duration) CompletionProvider {
	return gemini.NewCompletionProvider(apiKey, timeout)
}



// Telegram creates a TransportConfig for Telegram.
func Telegram(botToken string) TransportConfig {
	return TransportConfig{
		Type:     "telegram",
		BotToken: botToken,
	}
}

// OpenAIEmbedding creates an EmbeddingProvider using OpenAI embeddings.
func OpenAIEmbedding(apiKey string) EmbeddingProvider {
	return openai.NewEmbeddingProvider(apiKey, "text-embedding-3-small", 15*time.Second)
}

// OpenAIEmbeddingWithModel creates an EmbeddingProvider with a custom model.
func OpenAIEmbeddingWithModel(apiKey, model string, timeout time.Duration) EmbeddingProvider {
	return openai.NewEmbeddingProvider(apiKey, model, timeout)
}

// Serper creates a SearchEngine using the Serper.dev Google Search API.
func Serper(apiKey string) SearchEngine {
	return web.NewSerperSearch(apiKey)
}

// NewHTTPFetcher creates a WebFetcher that downloads and extracts text from web pages.
func NewHTTPFetcher() WebFetcher {
	return web.NewHTTPFetcher()
}

// Whisper creates a TranscriptionProvider using OpenAI Whisper.
func Whisper(apiKey string) TranscriptionProvider {
	return openai.NewTranscriptionProvider(apiKey, "whisper-1", 30*time.Second)
}

// WhisperWithModel creates a TranscriptionProvider with a custom model.
func WhisperWithModel(apiKey, model string, timeout time.Duration) TranscriptionProvider {
	return openai.NewTranscriptionProvider(apiKey, model, timeout)
}
 // TelegramSender creates a MessageSender using the Telegram Bot API.
func TelegramSender(botToken string, timeout time.Duration) MessageSender {
	return &telegramSenderAdapter{client: telegram.NewClient(botToken, timeout)}
}

// telegramSenderAdapter wraps telegram.Client to satisfy MessageSender.
type telegramSenderAdapter struct {
	client *telegram.Client
}

func (a *telegramSenderAdapter) SendMessage(ctx context.Context, chatID string, text string) (int, error) {
	result, err := a.client.SendMessage(ctx, chatID, text)
	if err != nil {
		return 0, err
	}
	return result.Result.MessageID, nil
}

func (a *telegramSenderAdapter) SendLong(ctx context.Context, chatID string, text string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		_, err = a.client.SendMessage(ctx, chatID, text)
		return err
	}
	return a.client.SendLong(ctx, id, text)
}

func (a *telegramSenderAdapter) SendVoice(ctx context.Context, chatID string, audio []byte) error {
	return a.client.SendVoice(ctx, chatID, audio)
}
