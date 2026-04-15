package blueship

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/a2a"
	a2aclient "github.com/rasimio/blueship/a2a/client"
	a2aserver "github.com/rasimio/blueship/a2a/server"
	a2astore "github.com/rasimio/blueship/a2a/store"
	"github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/anthropic"
	"github.com/rasimio/blueship/internal/gemini"
	"github.com/rasimio/blueship/internal/gateway"
	"github.com/rasimio/blueship/internal/infrastructure/ws"
	"github.com/rasimio/blueship/internal/openai"
	"github.com/rasimio/blueship/internal/openaicodex"
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

	// 3b. A2A server + peer bootstrap — optional subsystem that lets this
	// ship expose its marked tools to peers and call theirs as if local.
	if s.cfg.A2A.Enabled {
		if err := s.startA2A(ctx, deps, reg); err != nil {
			s.logger.Error("a2a: startup failed, continuing without A2A", "error", err)
		}
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
	var agentSched *agenttask.Scheduler
	if len(s.handlers) > 0 {
		// Build a global tool registry for agent tasks.
		globalRegistry := core.NewToolRegistry()
		tool.RegisterBuiltinTools(globalRegistry, deps)
		reg.RegisterAllTools(globalRegistry, deps)

		// Load tool descriptions from DB.
		if err := globalRegistry.LoadDescriptions(shipDB); err != nil {
			s.logger.Warn("tool descriptions not loaded for agent tasks", "error", err)
		}

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

		agentSched = agenttask.NewScheduler(taskStore, s.handlers, globalRegistry, msgStore, deps, notifyFn, s.logger)
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
	if agentSched != nil {
		agentSched.Wait()
	}
	s.logger.Info("blueship stopped")
	return nil
}

// --- Module registry adapter ---

// remoteToolReg is a cached RemoteTool registration that the ship applies
// to every fresh user-scoped registry built by the gateway. Populated at
// startup during A2A peer discovery so remote tools appear alongside
// local ones in the cortex.
type remoteToolReg struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Mode        string
	PeerName    string
	Handler     core.ToolHandler
}

type moduleRegistry struct {
	modules     []Module
	remoteTools []remoteToolReg
}

func (r *moduleRegistry) RegisterAllTools(registry *ToolRegistry, d *Deps) {
	for _, m := range r.modules {
		if tp, ok := m.(ToolProvider); ok {
			tp.RegisterTools(registry, d)
		}
	}
	for _, rt := range r.remoteTools {
		registry.RegisterRemote(rt.Name, rt.Description, rt.Schema, rt.Mode, rt.PeerName, rt.Handler)
	}
}

// ---------------------------------------------------------------------------
// A2A startup
// ---------------------------------------------------------------------------

// registryShim adapts core.ToolRegistry to the a2a.RegistryLike interface
// without the a2a package having to import core (which would cycle).
type registryShim struct {
	inner *core.ToolRegistry
}

func (r *registryShim) ExposedTools() []a2a.ExposedToolInfoLike {
	src := r.inner.ExposedTools()
	out := make([]a2a.ExposedToolInfoLike, 0, len(src))
	for _, t := range src {
		out = append(out, a2a.ExposedToolInfoLike{
			Name:        t.Name,
			Description: t.Description,
			Mode:        t.Mode,
			Schema:      t.Schema,
		})
	}
	return out
}

func (r *registryShim) ToolMetadata(name string) (string, bool, bool, bool) {
	return r.inner.ToolMetadata(name)
}

func (r *registryShim) HandlerByName(name string) (func(ctx context.Context, input json.RawMessage) (any, error), bool) {
	h, ok := r.inner.HandlerByName(name)
	if !ok {
		return nil, false
	}
	return core.ToolHandler(h), true
}

// startA2A boots the A2A subsystem: builds a ship-wide tool registry with
// every module's tools (so we can dispatch them from the HTTP server),
// starts the HTTP server, and walks configured peers to import RemoteTools.
func (s *Ship) startA2A(ctx context.Context, deps *Deps, reg *moduleRegistry) error {
	shipDB, err := deps.DB("ship")
	if err != nil {
		return fmt.Errorf("a2a: ship db: %w", err)
	}

	// Build a persistent tool registry for A2A-server-side dispatch. Modules
	// register into this registry once; per-user gateway registries are
	// rebuilt separately on each request, but the A2A path never sees those.
	a2aReg := core.NewToolRegistry()
	tool.RegisterBuiltinTools(a2aReg, deps)
	for _, m := range s.modules {
		if tp, ok := m.(ToolProvider); ok {
			tp.RegisterTools(a2aReg, deps)
		}
	}
	if err := a2aReg.LoadToolsConfig(shipDB); err != nil {
		s.logger.Warn("a2a: tools table not loaded — agent card may be empty", "error", err)
	}

	store := a2astore.New(shipDB)
	// The a2a server reads its list of exposed tools directly from the
	// unified `tools` table (via store.ListExposedTools), so there is no
	// per-startup mirroring step anymore — the DB row is authoritative.

	dispatcher := a2a.NewRegistryDispatcher(&registryShim{inner: a2aReg})
	srv := a2aserver.New(a2aserver.Config{
		Name:        s.cfg.A2A.Name,
		Description: "BlueShip A2A agent",
		Version:     s.cfg.A2A.Version,
		BaseURL:     s.cfg.A2A.BaseURL,
		AuthToken:   s.cfg.A2A.AuthToken,
	}, store, dispatcher, s.logger)

	// Start HTTP listener in the background; shutdown on ctx.Done.
	if s.cfg.A2A.Port > 0 {
		httpSrv := &http.Server{
			Addr:    fmt.Sprintf(":%d", s.cfg.A2A.Port),
			Handler: srv.Handler(),
		}
		go func() {
			s.logger.Info("a2a: http server starting", "addr", httpSrv.Addr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.logger.Error("a2a: http server error", "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutdownCtx)
		}()
	}

	// Discover peers and import selected remote tools. Each peer discovery
	// failure is logged but does not prevent the ship from starting — we
	// want local operation to survive transient remote outages.
	var tracer *a2a.TelegramGroupTracer
	if s.cfg.A2A.TraceChatID != "" && deps.Sender != nil {
		level := a2a.TraceLevelFull
		if s.cfg.A2A.TraceLevel != "" {
			level = a2a.TraceLevel(s.cfg.A2A.TraceLevel)
		}
		tracer = &a2a.TelegramGroupTracer{
			Sender:   senderAdapter{inner: deps.Sender},
			ChatID:   s.cfg.A2A.TraceChatID,
			SelfName: s.cfg.A2A.Name,
			Level:    level,
			Logger:   s.logger,
		}
	}

	for _, pcfg := range s.cfg.A2A.Peers {
		peer, err := store.UpsertPeer(ctx, pcfg.Name, pcfg.BaseURL, pcfg.AuthToken)
		if err != nil {
			s.logger.Warn("a2a: upsert peer failed", "peer", pcfg.Name, "error", err)
			continue
		}
		var ct a2aclient.Tracer
		if tracer != nil {
			ct = &tgTracerAdapter{inner: tracer}
		}
		cl := a2aclient.New(*peer, store, ct, s.logger)
		card, err := cl.Discover(ctx)
		if err != nil {
			s.logger.Warn("a2a: discover peer failed", "peer", pcfg.Name, "error", err)
			continue
		}
		imports := 0
		for _, rt := range card.Tools {
			if !shouldImport(pcfg.UseTools, rt.Name) {
				continue
			}
			_ = store.UpsertRemoteTool(ctx, peer.ID, rt)
			reg.remoteTools = append(reg.remoteTools, remoteToolReg{
				Name:        rt.Name,
				Description: rt.Description,
				Schema:      rt.Schema,
				Mode:        string(rt.Mode),
				PeerName:    pcfg.Name,
				Handler:     makeRemoteHandler(cl, rt.Name),
			})
			imports++
		}
		s.logger.Info("a2a: peer ready", "peer", pcfg.Name, "imported_tools", imports)
	}

	return nil
}

// senderAdapter bridges core.MessageSender (returns int, error) to the
// a2a.MessageSender interface. They have the same shape so the wrapper is
// a direct passthrough.
type senderAdapter struct {
	inner core.MessageSender
}

func (a senderAdapter) SendMessage(ctx context.Context, chatID string, text string) (int, error) {
	return a.inner.SendMessage(ctx, chatID, text)
}

// tgTracerAdapter bridges a2a.TelegramGroupTracer to the client package's
// Tracer interface. Both have identical method sets so it's a direct passthrough.
type tgTracerAdapter struct {
	inner *a2a.TelegramGroupTracer
}

func (a *tgTracerAdapter) TraceInvoke(ctx context.Context, call a2a.Call) {
	a.inner.TraceInvoke(ctx, call)
}
func (a *tgTracerAdapter) TraceResult(ctx context.Context, call a2a.Call) {
	a.inner.TraceResult(ctx, call)
}
func (a *tgTracerAdapter) TraceEvent(ctx context.Context, call a2a.Call, ev a2a.Event) {
	a.inner.TraceEvent(ctx, call, ev)
}

// shouldImport returns true if useTools is empty (import all) or the tool
// name is in the whitelist.
func shouldImport(useTools []string, name string) bool {
	if len(useTools) == 0 {
		return true
	}
	for _, t := range useTools {
		if t == name {
			return true
		}
	}
	return false
}

// makeRemoteHandler wraps an a2a client Invoke call into the core.ToolHandler
// signature so it plugs into any ToolRegistry as if it were local.
func makeRemoteHandler(cl *a2aclient.Client, toolName string) core.ToolHandler {
	return func(ctx context.Context, input json.RawMessage) (any, error) {
		resp, err := cl.Invoke(ctx, toolName, input, "")
		if err != nil {
			return nil, err
		}
		if resp.Mode == a2a.ToolModeSync {
			if len(resp.Output) == 0 {
				return map[string]any{"ok": true}, nil
			}
			var result any
			if err := json.Unmarshal(resp.Output, &result); err != nil {
				return string(resp.Output), nil
			}
			return result, nil
		}
		// Async — return the handle so the caller can poll / subscribe.
		return map[string]any{
			"call_id":    resp.CallID,
			"handle":     resp.Handle,
			"state":      string(resp.State),
			"events_url": resp.EventsURL,
		}, nil
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

// OpenAICodex creates a CompletionProvider using ChatGPT subscription via OAuth.
// refreshToken is the initial token from env; tokenFile persists rotated tokens.
func OpenAICodex(refreshToken, tokenFile string, timeout time.Duration, backoffs []time.Duration, logger *slog.Logger) CompletionProvider {
	ts := openaicodex.NewTokenStore(tokenFile, logger)
	if err := ts.Load(); err != nil {
		logger.Error("openai-codex: load tokens", "error", err)
	}
	ts.Bootstrap(refreshToken)
	return openaicodex.NewCompletionProvider(ts, timeout, backoffs, logger)
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

// WhisperLocal creates a TranscriptionProvider pointing to a local OpenAI-compatible
// STT endpoint (e.g. MLX Whisper on localhost).
func WhisperLocal(endpoint, model string, timeout time.Duration) TranscriptionProvider {
	return openai.NewTranscriptionProviderWithEndpoint(endpoint, model, timeout)
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
