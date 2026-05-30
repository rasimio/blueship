package blueship

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/federation/a2a"
	a2aclient "github.com/rasimio/blueship/internal/federation/a2a/client"
	a2aserver "github.com/rasimio/blueship/internal/federation/a2a/server"
	a2astore "github.com/rasimio/blueship/internal/federation/a2a/store"
	"github.com/rasimio/blueship/tool"
)

// fireDelegateCallback notifies the originating agent that a delegated task
// reached a terminal status, so the origin can wake its paused delegate task
// immediately instead of waiting for the next polling tick. No-op when the
// task wasn't accepted from a peer or the origin isn't in the local peer
// cache yet. Failures are logged but never propagated — the origin's
// stale-wake watchdog backstops a missed callback.
func (s *Ship) fireDelegateCallback(ctx context.Context, db *sqlx.DB, task core.AgentTask) {
	if len(task.Progress) == 0 {
		return
	}
	var prog struct {
		DelegatedFrom struct {
			OriginAgentID string `json:"origin_agent_id"`
			OriginTaskID  string `json:"origin_task_id"`
		} `json:"delegated_from"`
	}
	if err := json.Unmarshal(task.Progress, &prog); err != nil {
		return
	}
	if prog.DelegatedFrom.OriginAgentID == "" {
		return
	}

	var endpoint string
	if err := db.GetContext(ctx, &endpoint,
		`SELECT endpoint_url FROM fleet_peer_cache WHERE agent_id = $1`,
		prog.DelegatedFrom.OriginAgentID); err != nil || endpoint == "" {
		s.logger.Warn("delegate-callback: origin not in peer cache",
			"origin_agent_id", prog.DelegatedFrom.OriginAgentID, "task_id", task.ID, "error", err)
		return
	}

	// Build callback envelope. The "task_id" key carries OUR (peer)
	// task id — that's what the origin's WakePausedByPeerTask matches
	// against progress->>'peer_task_id'.
	resultText := ""
	if task.Result != nil {
		resultText = *task.Result
	}
	cbPayload, _ := json.Marshal(map[string]string{
		"task_id": task.ID.String(),
		"status":  task.Status,
		"summary": resultText,
	})
	envelope, _ := json.Marshal(a2a.Callback{
		Peer:    s.cfg.A2A.Name,
		Event:   "task_status_changed",
		Payload: cbPayload,
	})

	url := strings.TrimRight(endpoint, "/") + "/a2a/callback"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(envelope))
	if err != nil {
		s.logger.Warn("delegate-callback: build request failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.A2A.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.A2A.AuthToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.logger.Warn("delegate-callback: send failed", "url", url, "error", err)
		return
	}
	resp.Body.Close()
	s.logger.Info("delegate-callback: sent",
		"origin_agent_id", prog.DelegatedFrom.OriginAgentID,
		"task_id", task.ID, "status", task.Status)
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
	if err := tool.RegisterBrowserTools(a2aReg, deps); err != nil {
		s.logger.Warn("a2a: register browser tools failed", "error", err)
	}
	if err := tool.RegisterAgentTaskTools(a2aReg, deps); err != nil {
		s.logger.Warn("a2a: register agent_task tools failed", "error", err)
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

	// Wrap the A2A mux with a top-level mux that adds /metrics — exposed
	// without auth so Prometheus can scrape. The A2A handler keeps its
	// own auth on /a2a/* paths.
	rootMux := http.NewServeMux()
	rootMux.HandleFunc("/metrics", s.handleShipMetrics(shipDB))
	rootMux.Handle("/", srv.Handler())

	// Start HTTP listener in the background; shutdown on ctx.Done.
	if s.cfg.A2A.Port > 0 {
		httpSrv := &http.Server{
			Addr:    fmt.Sprintf(":%d", s.cfg.A2A.Port),
			Handler: rootMux,
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
