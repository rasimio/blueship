package blueship

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/anthropic"
	"github.com/rasimio/blueship/internal/gemini"
	"github.com/rasimio/blueship/internal/gateway"
	"github.com/rasimio/blueship/internal/openai"
	"github.com/rasimio/blueship/internal/scheduler"
	"github.com/rasimio/blueship/internal/telegram"
	"github.com/rasimio/blueship/internal/user"
	"github.com/rasimio/blueship/internal/web"
	"github.com/rasimio/blueship/migrate"
	"github.com/rasimio/blueship/session"
)

// Ship is the main BlueShip runtime instance.
type Ship struct {
	cfg     Config
	modules []Module
	logger  *slog.Logger
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
		// Override Config.Models so all consumers see DB values
		if ref := modelStore.Get("primary"); ref.Name != "" {
			deps.Config.Models.Primary = ref
		}
		if ref := modelStore.Get("compact"); ref.Name != "" {
			deps.Config.Models.Compact = ref
		}
	}

	// 2c. Initialize stores for ship DB data (prompts, users, sessions).
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

	// 5. Start Telegram Gateway
	if s.cfg.Transport.Type == "telegram" && s.cfg.Transport.BotToken != "" {
		gw, err := gateway.NewGateway(deps, reg, s.logger)
		if err != nil {
			return fmt.Errorf("create gateway: %w", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			gw.Run(ctx)
		}()

		// Heartbeat
		hb := gateway.NewHeartbeatJob(gw)
		wg.Add(1)
		go func() {
			defer wg.Done()
			scheduler.RunLoop(ctx, s.logger, "heartbeat", 30*time.Minute, hb.Run)
		}()

		// Thinking (autonomous agent)
		if !deps.Config.Gateway.DisableThinking {
			th := gateway.NewThinkingJob(gw)
			wg.Add(1)
			go func() {
				defer wg.Done()
				scheduler.RunLoop(ctx, s.logger, "thinking", 60*time.Minute, th.Run)
			}()
		}
	}

	// 6. Block until done
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

// OAuthConfig re-exports anthropic.OAuthConfig for external use.
type OAuthConfig = anthropic.OAuthConfig

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

// AnthropicOAuth creates a CompletionProvider with OAuth token refresh.
func AnthropicOAuth(refreshToken string, oauth OAuthConfig) CompletionProvider {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return anthropic.NewOAuthProvider(
		"", oauth,
		120*time.Second,
		[]time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second},
		logger,
	)
}

// AnthropicOAuthWithConfig creates a CompletionProvider with OAuth and custom settings.
func AnthropicOAuthWithConfig(oauth OAuthConfig, timeout time.Duration, backoffs []time.Duration) CompletionProvider {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return anthropic.NewOAuthProvider("", oauth, timeout, backoffs, logger)
}
// OpenAI creates a CompletionProvider using OpenAI Chat Completions.
func OpenAI(apiKey string) CompletionProvider {
	return openai.NewCompletionProvider(apiKey, 120*time.Second)
}

// OpenAIWithConfig creates a CompletionProvider with a custom timeout.
func OpenAIWithConfig(apiKey string, timeout time.Duration) CompletionProvider {
	return openai.NewCompletionProvider(apiKey, timeout)
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
