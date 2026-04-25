package blueship

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/rasimio/blueship/internal/fleet"
	"github.com/rasimio/blueship/internal/gateway"
	"github.com/rasimio/blueship/internal/gemini"
	"github.com/rasimio/blueship/internal/goal"
	"github.com/rasimio/blueship/internal/infrastructure/ws"
	"github.com/rasimio/blueship/internal/ollama"
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

// fleetAuth bundles Ship-side state populated by runFleet that the A2A
// server's JWT validator depends on. Wrapped in a struct so the A2A
// server can hold a stable pointer at startup time, even though the JWKS
// cache + self_agent_id only become known once Fleet is reachable.
type fleetAuth struct {
	mu          sync.RWMutex
	jwks        *fleet.JWKSCache
	selfAgentID string
}

func (f *fleetAuth) set(jwks *fleet.JWKSCache, selfAgentID string) {
	f.mu.Lock()
	f.jwks = jwks
	f.selfAgentID = selfAgentID
	f.mu.Unlock()
}

func (f *fleetAuth) snapshot() (*fleet.JWKSCache, string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.jwks, f.selfAgentID
}

// Ship is the main BlueShip runtime instance.
type Ship struct {
	cfg          Config
	modules      []Module
	handlers     map[string]core.AgentHandler          // scheduled-task handlers (heartbeat, etc.)
	goalHandlers map[core.GoalStrategy]core.GoalHandler // goal strategy executors
	logger       *slog.Logger
	fleetAuth    *fleetAuth        // populated by runFleet; consumed by A2A server's JWT middleware
	a2aRegistry  *core.ToolRegistry // shared between A2A dispatcher + Fleet identity publish
}

// New creates a new BlueShip instance with the given configuration.
func New(cfg Config) *Ship {
	cfg.ApplyDefaults()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	return &Ship{
		cfg:       cfg,
		logger:    logger,
		fleetAuth: &fleetAuth{},
	}
}

// RegisterModule adds a module to the BlueShip instance.
func (s *Ship) RegisterModule(m Module) {
	s.modules = append(s.modules, m)
}

// RegisterGoalHandler registers an executor for one GoalStrategy value.
// Agents choose which strategies their goals support by registering the
// corresponding handlers. If no handler is registered for a goal's strategy
// the scheduler fails that goal on first dispatch.
func (s *Ship) RegisterGoalHandler(strategy core.GoalStrategy, h core.GoalHandler) {
	if s.goalHandlers == nil {
		s.goalHandlers = make(map[core.GoalStrategy]core.GoalHandler)
	}
	s.goalHandlers[strategy] = h
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

	// 2c. Role-based tool allowlist comes from Config (code-driven). Roles
	// without a list fall back to "no allowlist" inside the role-aware
	// handlers.
	deps.RoleTools = core.NewRoleToolStore(s.cfg.RoleTools)

	// 2d. Prompts: file-backed store rooted at Config.Prompts. If the
	// directory is empty, individual Get calls error and callers fall
	// back to their own defaults.
	deps.Prompts = core.NewFilePromptStore(s.cfg.Prompts)
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
	s.logger.Info("a2a: config",
		"enabled", s.cfg.A2A.Enabled,
		"name", s.cfg.A2A.Name,
		"port", s.cfg.A2A.Port,
		"base_url", s.cfg.A2A.BaseURL,
		"peers", len(s.cfg.A2A.Peers))
	if s.cfg.A2A.Enabled {
		if err := s.startA2A(ctx, deps, reg); err != nil {
			s.logger.Error("a2a: startup failed, continuing without A2A", "error", err)
		}
	}

	// 3c. Fleet integration (optional). Publishes identity and refreshes
	// the peer cache in the background. Does not touch the A2A invocation
	// path in Phase 7 — federated tool handlers land in the next phase.
	var wg sync.WaitGroup
	if s.cfg.Fleet.Enabled {
		s.logger.Info("fleet: config",
			"base_url", s.cfg.Fleet.BaseURL,
			"client_id", s.cfg.Fleet.ClientID,
			"capabilities", len(s.cfg.Fleet.Capabilities),
			"interested_in", len(s.cfg.Fleet.InterestedIn))
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runFleet(ctx, deps, reg)
		}()
	}

	// 4. Start background jobs from modules
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
		if err := tool.RegisterGoalTools(globalRegistry, deps); err != nil {
			return fmt.Errorf("register goal tools: %w", err)
		}
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

		agentSched = agenttask.NewScheduler(taskStore, s.handlers, globalRegistry, msgStore, deps, notifyFn, s.logger)

		// Use trigger channel for instant callback wakeup (if configured).
		var trigger <-chan string
		if s.cfg.A2A.TaskTrigger != nil {
			trigger = s.cfg.A2A.TaskTrigger
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			scheduler.RunLoopWithTrigger(ctx, s.logger, "agent-tasks", 1*time.Minute, agentSched.Run, trigger, agentSched.WakeFromCallback)
		}()

		// Goal scheduler runs alongside agent-task scheduler. They don't share
		// state; goals use a separate table (blueship.goals) and a separate
		// handler registry keyed by GoalStrategy instead of handler name.
		// Handlers are registered by the agent (e.g. StructuredGoalExecutor
		// from blueship/handler) — the Ship itself stays agent-agnostic.
		if len(s.goalHandlers) > 0 {
			goalStore := core.NewGoalStore(shipDB)
			goalSched := goal.NewScheduler(goalStore, s.goalHandlers, globalRegistry, msgStore, deps, notifyFn, s.logger)
			wg.Add(1)
			go func() {
				defer wg.Done()
				scheduler.RunLoopWithTrigger(ctx, s.logger, "goals", 1*time.Minute, goalSched.Run, trigger, goalSched.WakeFromCallback)
			}()
		}
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
	Source      string // "yaml" or "fleet" — used for selective replacement
}

type moduleRegistry struct {
	modules []Module

	mu          sync.Mutex // guards remoteTools (Bootstrap mutates concurrently)
	remoteTools []remoteToolReg
}

func (r *moduleRegistry) RegisterAllTools(registry *ToolRegistry, d *Deps) {
	for _, m := range r.modules {
		if tp, ok := m.(ToolProvider); ok {
			tp.RegisterTools(registry, d)
		}
	}
	r.mu.Lock()
	tools := append([]remoteToolReg(nil), r.remoteTools...)
	r.mu.Unlock()
	for _, rt := range tools {
		registry.RegisterRemote(rt.Name, rt.Description, rt.Schema, rt.Mode, rt.PeerName, rt.Handler)
	}
}

// AppendRemoteTool is the startup-time path used by config-driven A2A peers.
func (r *moduleRegistry) AppendRemoteTool(rt remoteToolReg) {
	r.mu.Lock()
	r.remoteTools = append(r.remoteTools, rt)
	r.mu.Unlock()
}

// ReplaceFleetRemoteTools atomically swaps every Fleet-derived remote tool
// (rows whose source matches sourceTag) with the supplied snapshot. Tools
// from other sources (legacy yaml peers) are preserved.
func (r *moduleRegistry) ReplaceFleetRemoteTools(sourceTag string, fresh []remoteToolReg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.remoteTools[:0]
	for _, t := range r.remoteTools {
		if t.Source != sourceTag {
			kept = append(kept, t)
		}
	}
	for _, t := range fresh {
		t.Source = sourceTag
		kept = append(kept, t)
	}
	r.remoteTools = kept
}

// ---------------------------------------------------------------------------
// A2A startup
// ---------------------------------------------------------------------------

// runFleet boots the optional BlueFleet integration. Blocks until ctx is
// cancelled. Transient errors (Fleet down, token expired between refreshes)
// are logged but never take the Ship down.
func (s *Ship) runFleet(ctx context.Context, deps *Deps, reg *moduleRegistry) {
	shipDB, err := deps.DB("ship")
	if err != nil {
		s.logger.Error("fleet: ship db unavailable", "error", err)
		return
	}
	cfg := s.cfg.Fleet
	// Treat unexpanded ${FOO} placeholders as "env var was not set" so the
	// Ship silently skips Fleet until deploy provisions credentials.
	if strings.Contains(cfg.BaseURL, "${") {
		cfg.BaseURL = ""
	}
	if strings.Contains(cfg.ClientID, "${") {
		cfg.ClientID = ""
	}
	if strings.Contains(cfg.ClientSecret, "${") {
		cfg.ClientSecret = ""
	}
	if cfg.BaseURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		s.logger.Warn("fleet: missing base_url / client_id / client_secret — skipping")
		return
	}
	cli := fleet.New(fleet.Config{
		BaseURL:         cfg.BaseURL,
		ClientID:        cfg.ClientID,
		ClientSecret:    cfg.ClientSecret,
		RefreshInterval: cfg.RefreshInterval,
	}, s.logger)

	caps := make([]fleet.Capability, 0, len(cfg.Capabilities))
	for _, c := range cfg.Capabilities {
		var meta json.RawMessage
		if len(c.Metadata) > 0 {
			meta = json.RawMessage(c.Metadata)
		}
		caps = append(caps, fleet.Capability{
			Tag:         c.Tag,
			Description: c.Description,
			Metadata:    meta,
		})
	}

	bs := fleet.NewBootstrap(cli, shipDB, fleet.Identity{
		DisplayName:  cfg.DisplayName,
		Description:  cfg.Description,
		EndpointURL:  cfg.EndpointURL,
		PublicKey:    cfg.PublicKey,
		Capabilities: caps,
	}, cfg.InterestedIn, s.logger)

	// JWKS cache for inbound JWT validation. Populated before federation
	// sink is wired so the A2A server can accept Fleet tokens immediately.
	jwks := fleet.NewJWKSCache(cfg.BaseURL, 30*time.Minute, s.logger)
	go jwks.Run(ctx)

	// Look up our own agent_id so the JWT validator can enforce the
	// audience claim. If GetMe fails (cold start, Fleet down) we retry on a
	// short backoff — without it the inbound JWT path stays disabled.
	selfID := s.discoverSelfAgentID(ctx, cli)
	if selfID != "" {
		s.fleetAuth.set(jwks, selfID)
		s.logger.Info("fleet: self-id learned", "agent_id", selfID)
	} else {
		s.logger.Warn("fleet: self-id discovery failed; inbound JWT auth stays disabled")
	}

	// Federation: register peer-imported tools into the Ship's tool
	// registry on every refresh tick. invokeFn does the actual HTTP POST.
	if selfID != "" {
		bs.WithFederation(selfID, &fleetSinkAdapter{reg: reg, logger: s.logger}, s.fleetInvoke)
	}

	if s.a2aRegistry == nil {
		s.logger.Warn("fleet: A2A registry not built yet — running with empty exposed tools")
		s.a2aRegistry = core.NewToolRegistry()
	}
	bs.Run(ctx, &fleetToolPublisher{reg: s.a2aRegistry})
}

// discoverSelfAgentID asks Fleet for the calling agent's profile, retrying
// briefly so a cold-start race against Fleet doesn't leave inbound JWT
// auth permanently disabled.
func (s *Ship) discoverSelfAgentID(ctx context.Context, cli *fleet.Client) string {
	for attempt := 0; attempt < 5; attempt++ {
		card, err := cli.GetMe(ctx)
		if err == nil && card.Agent.ID != "" {
			return card.Agent.ID
		}
		if attempt < 4 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(time.Duration(2<<attempt) * time.Second):
			}
		}
	}
	return ""
}

// fleetInvoke is the dispatch closure handed to fleet.Bootstrap. It does
// one peer-to-peer A2A call, stamping the supplied JWT bearer.
func (s *Ship) fleetInvoke(ctx context.Context, peerName, peerAgentID, endpointURL, toolName string, input []byte, bearer string) ([]byte, error) {
	if endpointURL == "" {
		return nil, fmt.Errorf("fleet: peer %q has no endpoint URL", peerName)
	}
	body, _ := json.Marshal(map[string]any{
		"tool":  toolName,
		"input": json.RawMessage(input),
	})
	url := strings.TrimRight(endpointURL, "/") + "/a2a/invoke"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	httpCli := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fleet: invoke %s/%s: %w", peerName, toolName, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fleet: invoke %s/%s: HTTP %d: %s", peerName, toolName, resp.StatusCode, string(raw))
	}
	return raw, nil
}

// fleetSinkAdapter bridges fleet.FederatedToolSink to moduleRegistry.
type fleetSinkAdapter struct {
	reg    *moduleRegistry
	logger *slog.Logger
}

func (a *fleetSinkAdapter) ReplaceFleetTools(snapshot []fleet.FederatedTool) {
	tools := make([]remoteToolReg, 0, len(snapshot))
	for _, ft := range snapshot {
		ftLocal := ft
		handler := core.ToolHandler(func(ctx context.Context, input json.RawMessage) (any, error) {
			raw, err := ftLocal.Handler(ctx, []byte(input))
			if err != nil {
				return nil, err
			}
			// Tool returns the full /a2a/invoke response body. Decode it
			// and surface either the sync output or the async handle.
			var ir struct {
				Mode      string          `json:"mode"`
				Output    json.RawMessage `json:"output"`
				CallID    string          `json:"call_id"`
				Handle    string          `json:"handle"`
				State     string          `json:"state"`
				EventsURL string          `json:"events_url"`
			}
			if err := json.Unmarshal(raw, &ir); err != nil {
				return string(raw), nil
			}
			if ir.Mode == "async" {
				return map[string]any{
					"call_id":    ir.CallID,
					"handle":     ir.Handle,
					"state":      ir.State,
					"events_url": ir.EventsURL,
				}, nil
			}
			if len(ir.Output) == 0 {
				return map[string]any{"ok": true}, nil
			}
			var result any
			if err := json.Unmarshal(ir.Output, &result); err != nil {
				return string(ir.Output), nil
			}
			return result, nil
		})
		tools = append(tools, remoteToolReg{
			Name:        ftLocal.Name,
			Description: ftLocal.Description,
			Schema:      json.RawMessage(ftLocal.Schema),
			Mode:        ftLocal.Mode,
			PeerName:    ftLocal.PeerName,
			Handler:     handler,
		})
	}
	a.reg.ReplaceFleetRemoteTools("fleet", tools)
	a.logger.Info("fleet: registered federated tools", "count", len(tools))
}

// fleetToolPublisher snapshots the registry's exposed tools and reshapes
// them into the format BlueFleet expects in PutTools. Source of truth is
// the in-memory registry — there is no DB read involved.
type fleetToolPublisher struct {
	reg *core.ToolRegistry
}

func (p *fleetToolPublisher) ListExposedTools(_ context.Context) ([]fleet.Tool, error) {
	src := p.reg.ExposedTools()
	out := make([]fleet.Tool, 0, len(src))
	for _, t := range src {
		mode, _, _, _ := p.reg.ToolMetadata(t.Name)
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{}`)
		}
		out = append(out, fleet.Tool{
			Name:        t.Name,
			Description: t.Description,
			Mode:        mode,
			InputSchema: schema,
		})
	}
	return out, nil
}

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
	if err := tool.RegisterGoalTools(a2aReg, deps); err != nil {
		s.logger.Warn("a2a: register goal tools failed", "error", err)
	}
	for _, m := range s.modules {
		if tp, ok := m.(ToolProvider); ok {
			tp.RegisterTools(a2aReg, deps)
		}
	}
	// Publish to the Ship struct so runFleet can surface the same set of
	// exposed tools to BlueFleet without rebuilding the registry.
	s.a2aRegistry = a2aReg

	store := a2astore.New(shipDB)
	// Exposed tools (those returned by the agent card and dispatched by
	// /a2a/invoke) come from the registry directly. Tools mark themselves
	// exposed at registration site via Expose(name, mode); no DB lookup
	// is performed at startup.

	dispatcher := a2a.NewRegistryDispatcher(&registryShim{inner: a2aReg})

	// Adapt core.A2AConfig.CallbackHandler to a2a.CallbackHandler.
	var cbHandler a2a.CallbackHandler
	if s.cfg.A2A.CallbackHandler != nil {
		h := s.cfg.A2A.CallbackHandler
		cbHandler = func(ctx context.Context, cb a2a.Callback) {
			h(ctx, cb.Peer, cb.Event, cb.Payload)
		}
	}

	// Late-bound JWT validator: returns failure until runFleet populates
	// the JWKS cache + this agent's own ID. Once Fleet is up, every inbound
	// JWT is verified against the cached keys + audience.
	jwtValidator := func(ctx context.Context, raw string) (string, error) {
		jwks, selfID := s.fleetAuth.snapshot()
		if jwks == nil || selfID == "" {
			return "", fmt.Errorf("fleet: jwt validator not ready")
		}
		claims, err := jwks.Validate(ctx, raw, selfID)
		if err != nil {
			return "", err
		}
		return claims.CallerAgentID, nil
	}

	srv := a2aserver.New(a2aserver.Config{
		Name:         s.cfg.A2A.Name,
		Description:  "BlueShip A2A agent",
		Version:      s.cfg.A2A.Version,
		BaseURL:      s.cfg.A2A.BaseURL,
		AuthToken:    s.cfg.A2A.AuthToken,
		JWTValidator: jwtValidator,
	}, store, dispatcher, cbHandler, s.logger)

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
			_ = store.UpsertRemoteTool(ctx, peer.ID, rt)
			reg.AppendRemoteTool(remoteToolReg{
				Name:        rt.Name,
				Description: rt.Description,
				Schema:      rt.Schema,
				Mode:        string(rt.Mode),
				PeerName:    pcfg.Name,
				Handler:     makeRemoteHandler(cl, rt.Name),
				Source:      "yaml",
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
// (vLLM, LM Studio, etc.). Pass empty apiKey if auth is not required.
// extraParams are merged into every request JSON (e.g. for chat_template_kwargs).
// For Ollama prefer Ollama() below — its /v1/ endpoint has bugs around the
// Gemma reasoning field.
func OpenAICompatible(baseURL, apiKey string, timeout time.Duration, extraParams map[string]any) CompletionProvider {
	return openai.NewCompatibleProvider(baseURL, apiKey, timeout, extraParams)
}

// Ollama creates a CompletionProvider that speaks Ollama's native /api/chat
// protocol (NDJSON streaming, options-nested generation params, think=false).
// Pass empty baseURL for http://localhost:11434.
func Ollama(baseURL string, timeout time.Duration) CompletionProvider {
	return ollama.NewCompletionProvider(baseURL, timeout)
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
