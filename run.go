package blueship

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/gateway"
	"github.com/rasimio/blueship/internal/looprunner"
	"github.com/rasimio/blueship/internal/migrate"
	"github.com/rasimio/blueship/internal/store/user"
	"github.com/rasimio/blueship/internal/toolcatalog"
	"github.com/rasimio/blueship/internal/transport/httpchat"
	"github.com/rasimio/blueship/internal/transport/ws"
	"github.com/rasimio/blueship/runtime/session"
	"github.com/rasimio/blueship/tool"
)

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

	// Propagate Config-level callbacks into the freshly-initialised deps.
	// The host wires these onto cfg before calling blueship.New (e.g.
	// the host's Layer-2 actor manager exposes EmitTurnCompleted as the
	// hook). Done here rather than in InitDeps so InitDeps stays a pure
	// constructor of stores/clients.
	deps.TurnCompletedHook = s.cfg.Gateway.TurnCompletedHook
	deps.AgentIterationCompletedHook = s.cfg.Gateway.AgentIterationCompletedHook
	deps.ResolveSoul = s.cfg.Gateway.ResolveSoul
	deps.ResolveTelegramChat = s.cfg.Gateway.ResolveTelegramChat
	deps.AttachmentSink = s.cfg.Gateway.AttachmentSink
	deps.BotOnboarding = s.cfg.Gateway.BotOnboarding

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
	deps.SelfAgentID = func() string {
		_, id := s.fleetAuth.snapshot()
		return id
	}
	s.logger.Info("running as owner", "user_id", uid.String())

	// 3. Create module registry adapter
	reg := &moduleRegistry{
		modules: s.modules,
	}

	// 3a. Publish the native tool catalog to vaelum.tool_catalog so the
	// Vaelum web cabinet can enumerate every tool. Gated on ToolMeta —
	// only the Vaelum host supplies tool categories; generic consumers
	// skip this entirely. A failure here is non-fatal (stale catalog).
	if s.cfg.ToolMeta != nil {
		catReg := core.NewToolRegistry()
		tool.RegisterBuiltinTools(catReg, deps)
		if err := tool.RegisterBrowserTools(catReg, deps); err != nil {
			s.logger.Warn("toolcatalog: register browser tools failed", "error", err)
		}
		if err := tool.RegisterAgentTaskTools(catReg, deps); err != nil {
			s.logger.Warn("toolcatalog: register agent_task tools failed", "error", err)
		}
		reg.RegisterAllTools(catReg, deps)
		if err := toolcatalog.Publish(ctx, shipDB, catReg.Definitions(), s.cfg.ToolMeta, s.logger); err != nil {
			s.logger.Warn("toolcatalog: publish failed", "error", err)
		}
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
					looprunner.RunLoop(ctx, s.logger, j.Name, j.Interval, j.Run)
				}(job)
			}
		}
	}

	// 4b. Start agent task scheduler (if handlers registered).
	var agentSched *agenttask.Scheduler
	// Start the agent-task scheduler if EITHER recurring handlers OR
	// strategy executors are registered. An agent that exposes only
	// strategy executors (no recurring jobs) still needs the scheduler
	// to run delegated direct/structured tasks accepted from peers.
	if len(s.handlers) > 0 || len(s.strategyHandlers) > 0 {
		// Build a global tool registry for agent tasks.
		globalRegistry := core.NewToolRegistry()
		tool.RegisterBuiltinTools(globalRegistry, deps)
		if err := tool.RegisterBrowserTools(globalRegistry, deps); err != nil {
			return fmt.Errorf("register browser tools: %w", err)
		}
		if err := tool.RegisterAgentTaskTools(globalRegistry, deps); err != nil {
			return fmt.Errorf("register agent_task tools: %w", err)
		}
		reg.RegisterAllTools(globalRegistry, deps)
		// Subscribe globalRegistry to future Fleet remote-tool pushes so
		// federation discovered after boot reaches the agent-task scheduler
		// without rebuilding the registry. RegisterRemote overwrites any
		// local registration with the same name — federation wins for
		// delegation flows like agent_task_accept.
		reg.AddTargetRegistry(globalRegistry)

		taskStore := core.NewAgentTaskStore(shipDB)
		msgStore := session.NewStore(shipDB) // MessageStore for agent loops

		// Notification callback: append to chat session (so cortex sees it) + send to Telegram.
		var notifyFn func(ctx context.Context, userID uuid.UUID, text string)
		if deps.Users != nil {
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

				// Send to Telegram. Prefer the per-user multi-bot
				// sender wired by the gateway — it reads the user's
				// actual paired bot from vaelum.bot_links and sends
				// through it. Without this the legacy Transport.BotToken
				// (host owner's private bot) was used for everyone,
				// which surfaces as "403 Forbidden: bot was blocked"
				// for every platform user who never opened that bot
				// (they paired with @VaelumBot, not @ArleneKateBot).
				if deps.SendToUser != nil {
					if err := deps.SendToUser(ctx, userID, text); err != nil {
						s.logger.Warn("agent-tasks: notify failed", "error", err)
					}
				} else if deps.Sender != nil {
					chatID := profile.ChatID
					if idx := strings.Index(chatID, ":"); idx >= 0 {
						chatID = chatID[idx+1:]
					}
					if err := deps.Sender.SendLong(ctx, chatID, text); err != nil {
						s.logger.Warn("agent-tasks: notify failed", "error", err)
					}
				}
			}
		}

		agentSched = agenttask.NewScheduler(taskStore, s.handlers, s.strategyHandlers, globalRegistry, msgStore, deps, notifyFn, s.logger)

		// Per-task tool registry: a fresh registry bound to each task's
		// owner_user_id so per-tool closures capture d.UserID =
		// task.UserID. Without this, the scheduler reused globalRegistry
		// whose tools captured the zero-value Deps — every notes /
		// memory / personal tool returned the global owner's data (or
		// empty for multi-tenant hosts), so heartbeat for non-owner
		// souls saw no notes and stayed silent forever.
		agentSched.SetRegistryBuilder(func(userDeps *core.Deps) *core.ToolRegistry {
			r := core.NewToolRegistry()
			tool.RegisterBuiltinTools(r, userDeps)
			if err := tool.RegisterBrowserTools(r, userDeps); err != nil {
				s.logger.Warn("agent-tasks: per-task browser tools registration failed", "error", err)
			}
			if err := tool.RegisterAgentTaskTools(r, userDeps); err != nil {
				s.logger.Warn("agent-tasks: per-task agent_task tools registration failed", "error", err)
			}
			reg.RegisterAllTools(r, userDeps)
			return r
		})

		// Built-in delegate callback emitter: when a task that came from
		// a peer (progress.delegated_from set) reaches a terminal status,
		// notify the origin via /a2a/callback so they can wake their
		// paused delegate task immediately instead of waiting for the
		// next polling tick or stale-wake watchdog.
		agentSched.SetStatusCallback(func(cbCtx context.Context, t core.AgentTask) {
			s.fireDelegateCallback(cbCtx, shipDB, t)
		})

		// Use trigger channel for instant callback wakeup (if configured).
		var trigger <-chan string
		if s.cfg.A2A.TaskTrigger != nil {
			trigger = s.cfg.A2A.TaskTrigger
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			looprunner.RunLoopWithTrigger(ctx, s.logger, "agent-tasks", 1*time.Minute, agentSched.Run, trigger, agentSched.WakeFromCallback)
		}()

	}

	// 5. Start Gateway. The gateway is the inbound-message router for every
	// transport (Telegram, WebSocket, future ones). Multi-bot Telegram is
	// driven by the host's ListBots hook (or legacy BotToken fallback);
	// the gateway is built as long as ANY transport is configured because
	// HTTPChat / WebSocket sit on top of the same gateway, and ReloadBots
	// then decides whether a Telegram fan-in actually runs.
	var gw *gateway.Gateway
	telegramConfigured := s.cfg.Transport.Telegram.ListBots != nil || s.cfg.Transport.BotToken != ""
	wsConfigured := s.cfg.Transport.WebSocket.Port > 0
	hcConfigured := s.cfg.Transport.HTTPChat.Port > 0
	if telegramConfigured || wsConfigured || hcConfigured {
		var err error
		gw, err = gateway.NewGateway(deps, reg, s.logger)
		if err != nil {
			return fmt.Errorf("create gateway: %w", err)
		}
		// Wire the gateway's per-user/per-bot sender so the agent-task
		// scheduler can deliver Notify via the SAME bot the user paired
		// with, instead of the legacy single-bot Transport.BotToken.
		deps.SendToUser = gw.SendToUser
	}

	// 5a. Telegram fan-in — populated by ReloadBots from the host's
	// ListBots source (or BotToken fallback). The fan-in goroutine runs
	// as long as there is any transport because the periodic reconcile
	// loop is the seam that picks up bots added at runtime via the
	// reload signal.
	if gw != nil && telegramConfigured {
		if err := gw.ReloadBots(ctx); err != nil {
			s.logger.Warn("gateway: initial ReloadBots failed", "error", err)
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

	// 6b. Start HTTP/SSE chat server (Vaelum web platform). The host's
	// optional Extras callback mounts additional internal-API routes on
	// the same port/token (the host uses this for its associate endpoint). Reset
	// is wired here so vaelum gets the same archive+new-session behaviour
	// as the Telegram /reset command without having to reach into the
	// gateway directly from the host package.
	if hcCfg := s.cfg.Transport.HTTPChat; hcCfg.Port > 0 && gw != nil {
		hcCfg.Reset = gw.ResetSession
		hcSrv := httpchat.NewServer(gw, hcCfg.Port, hcCfg.Token, hcCfg.Extras, hcCfg.Reset, s.logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := hcSrv.Run(ctx); err != nil {
				s.logger.Error("http chat server error", "error", err)
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
