package blueship

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
	"github.com/rasimio/blueship/internal/transport/httpchat"
	"github.com/rasimio/blueship/internal/transport/ws"
	"github.com/rasimio/blueship/runtime/session"
	"github.com/rasimio/blueship/tool"
)

type coordinatedTaskNotificationContextKey struct{}

// Run starts BlueShip: connects to DB, initializes providers, starts transport, runs jobs.
// Blocks until ctx is done.
func (s *Ship) Run(ctx context.Context) error {
	s.logger.Info("starting blueship")

	// Refuse a chat-native signup flow that cannot run, before anything is
	// listening: an empty palette renders pickers with no buttons, and a
	// default persona naming a voice outside the palette writes tenant
	// rows pointing at something that does not exist — one per signup,
	// silently, until somebody noticed.
	//
	// Gated on the hook, not on the config: a host that never wired chat
	// onboarding has no reason to configure a persona vocabulary, and
	// making it boot-critical for them would be the framework imposing a
	// feature they declined.
	if s.cfg.Gateway.BotOnboarding != nil {
		if err := s.cfg.Gateway.OnboardingFlow.Validate(); err != nil {
			return err
		}
	}

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
	deps.PersonaEditor = s.cfg.Gateway.PersonaEditor
	deps.DeeplinkLogin = s.cfg.Gateway.DeeplinkLogin
	deps.DeeplinkLink = s.cfg.Gateway.DeeplinkLink
	deps.AuthorizeExecution = s.cfg.AuthorizeExecution

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
	sessionStore := session.NewStore(shipDB)
	deps.Sessions = sessionStore
	deps.UsageRecorder = sessionStore

	// 3. Ensure/resolve owner user
	var uid uuid.UUID
	if s.cfg.Owner.ChatID != "" {
		uid, err = user.EnsureOwner(ctx, shipDB, s.cfg.Owner.ChatID, s.cfg.Owner.DisplayName)
		if err != nil {
			return fmt.Errorf("ensure owner: %w", err)
		}
	} else {
		// No owner configured. Resolve an existing one if present; otherwise
		// boot without a designated owner — a basic bot resolves each user when
		// they message, and owner-scoped features (proactive jobs, single-user
		// mode) are simply inactive until Config.Owner.ChatID is set. This lets
		// a fresh deployment start with zero user setup.
		uid, err = user.ResolveOwner(ctx, shipDB)
		if err != nil {
			s.logger.Warn("no owner configured and none found in DB; continuing without a designated owner — set Config.Owner.ChatID for single-user / proactive features", "error", err)
			uid = uuid.Nil
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

	// 3a. Publish the native tool catalog via the host-supplied hook (e.g. so a
	// web cabinet can enumerate every tool). Gated on PublishToolCatalog — only
	// a host that wants a catalog supplies it; generic consumers skip this. The
	// framework owns no platform schema. A failure here is non-fatal.
	if s.cfg.PublishToolCatalog != nil {
		catReg := core.NewToolRegistry()
		tool.RegisterBuiltinTools(catReg, deps)
		if err := tool.RegisterBrowserTools(catReg, deps); err != nil {
			s.logger.Warn("toolcatalog: register browser tools failed", "error", err)
		}
		if err := tool.RegisterAgentTaskTools(catReg, deps); err != nil {
			s.logger.Warn("toolcatalog: register agent_task tools failed", "error", err)
		}
		reg.RegisterAllTools(catReg, deps)
		if err := s.cfg.PublishToolCatalog(ctx, catReg.Definitions(), s.cfg.ToolMeta); err != nil {
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
	var agentTaskTrigger <-chan string
	var taskStore *core.AgentTaskStore
	var gw *gateway.Gateway
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
		// without rebuilding the registry. Local registrations keep their
		// name; delegation flows like agent_task_accept resolve the peer's
		// copy explicitly via RemoteHandlerForPeer.
		reg.AddTargetRegistry(globalRegistry)

		taskStore = core.NewAgentTaskStore(shipDB)
		msgStore := session.NewStore(shipDB) // MessageStore for agent loops

		// Notification callback: append to chat session (so cortex sees it) + send to Telegram.
		var notifyFn func(ctx context.Context, userID uuid.UUID, text string) (core.TaskNotificationReceipt, error)
		if deps.Users != nil {
			notifyFn = func(ctx context.Context, userID uuid.UUID, text string) (core.TaskNotificationReceipt, error) {
				var receipt core.TaskNotificationReceipt

				if commit, matched, parseErr := core.ParseAutonomousTurnNotification(text); matched {
					if parseErr != nil {
						return receipt, core.PermanentlyNotSent(parseErr)
					}
					if !core.SingleAttemptNotificationFromContext(ctx) {
						return receipt, core.PermanentlyNotSent(fmt.Errorf("autonomous turn requires keyed single-attempt delivery"))
					}
					if commit.UserID != userID {
						return receipt, core.PermanentlyNotSent(fmt.Errorf("autonomous turn user mismatch"))
					}
					if gw == nil {
						return receipt, core.DefinitelyNotSent(fmt.Errorf("autonomous turn gateway unavailable"))
					}
					return gw.CommitAutonomousTurn(ctx, commit)
				}
				coordinated, _ := ctx.Value(coordinatedTaskNotificationContextKey{}).(bool)
				if !coordinated && gw != nil {
					if soulID, ok := core.SoulIDFromContextOK(ctx); ok && soulID != uuid.Nil {
						coordinatedCtx := context.WithValue(
							ctx, coordinatedTaskNotificationContextKey{}, true,
						)
						return gw.CoordinateTaskNotification(
							coordinatedCtx, userID, soulID,
							func(lockedCtx context.Context, _ string) (core.TaskNotificationReceipt, error) {
								return notifyFn(lockedCtx, userID, text)
							},
						)
					}
				}

				// Keyed task-program notifications are admitted by a durable
				// at-most-once journal before this callback. Keep the transport
				// equally strict: one provider request, no rich/plain fallback,
				// and persist chat history only after Telegram confirms a message
				// id. Attachments and voice handoffs require multiple/rewritten
				// sends, so they are outside this delivery mode.
				if core.SingleAttemptNotificationFromContext(ctx) {
					if strings.HasPrefix(text, "[voice_handoff]\n") {
						return receipt, core.PermanentlyNotSent(fmt.Errorf("single-attempt notification cannot use voice handoff"))
					}
					if ids, _, ok := core.ParseAttachmentMarkers(text); ok && len(ids) > 0 {
						return receipt, core.PermanentlyNotSent(fmt.Errorf("single-attempt notification cannot include attachments"))
					}
					if deps.SendToUserOnce == nil {
						return receipt, core.PermanentlyNotSent(fmt.Errorf("single-attempt sender unavailable"))
					}
					var err error
					receipt, err = deps.SendToUserOnce(ctx, userID, text)
					if err != nil {
						return receipt, err
					}
					// Telegram may consume the outer transport deadline while still
					// returning a confirmed message id. History persistence is not part
					// of send admission, so give it a fresh DB budget and never turn a
					// confirmed send into a retryable transport error.
					historyCtx, historyCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					defer historyCancel()
					uid := userID.String()
					var sessID string
					soulID, hasSoul := core.SoulIDFromContextOK(ctx)
					if !hasSoul {
						s.logger.WarnContext(historyCtx, "agent-tasks: notification history missing soul",
							"user_id", userID)
					} else if historyErr := shipDB.GetContext(historyCtx, &sessID,
						`SELECT id FROM chat_sessions WHERE user_id = $1 AND soul_id = $2 AND source = 'chat' AND active = true ORDER BY updated_at DESC LIMIT 1`,
						uid, soulID); historyErr != nil && !errors.Is(historyErr, sql.ErrNoRows) {
						s.logger.WarnContext(historyCtx, "agent-tasks: notification history lookup failed",
							"user_id", userID, "soul_id", soulID, "error", historyErr)
					}
					if sessID != "" && strings.TrimSpace(text) != "" {
						var tgMessageID int64
						if receipt.Transport == "telegram" && receipt.MessageID != "" {
							if parsed, parseErr := strconv.ParseInt(receipt.MessageID, 10, 64); parseErr == nil {
								tgMessageID = parsed
							} else {
								s.logger.WarnContext(historyCtx, "agent-tasks: invalid telegram receipt message id",
									"message_id", receipt.MessageID, "error", parseErr)
							}
						}
						if historyErr := msgStore.Append(historyCtx, sessID, core.Message{
							Role:        "assistant",
							Content:     core.NormalizeContent(text),
							TGMessageID: tgMessageID,
						}); historyErr != nil {
							s.logger.WarnContext(historyCtx, "agent-tasks: notification history append failed",
								"session_id", sessID, "error", historyErr)
						}
					}
					return receipt, nil
				}

				profile, err := deps.Users.GetByID(ctx, userID.String())
				if err != nil {
					return receipt, fmt.Errorf("user lookup for notify: %w", err)
				}

				// Voice handoff: a router-shaped delivery task returns its
				// payload behind the [voice_handoff] marker; the soul's own
				// persona voices it so the chat reads as one continuous
				// companion, not a worker's telegram. Any failure falls back
				// to sending the raw payload — delivery beats style.
				if payload, ok := strings.CutPrefix(text, "[voice_handoff]\n"); ok {
					text = voiceHandoffText(ctx, deps, s.cfg, payload)
				}

				// Resolve [attached: UUID] markers into real file sends (a
				// research task's PDF report), then continue with the cleaned
				// text. The agent-task path used to send the raw marker as
				// text. soul_id rides on ctx (set in executeTask); without it,
				// or the host hooks, we fall back to plain text.
				cleaned := text
				var notifyErrs []error
				sentAttachment := false
				if ids, c, ok := core.ParseAttachmentMarkers(text); ok {
					cleaned = c
					soulID, hasSoul := core.SoulIDFromContextOK(ctx)
					if hasSoul && deps.AttachmentSink != nil && deps.SendToUserAttachment != nil {
						for _, id := range ids {
							rec, data, aerr := deps.AttachmentSink.Get(ctx, userID, soulID, id)
							if aerr != nil {
								notifyErrs = append(notifyErrs, fmt.Errorf("attachment %s resolve: %w", id, aerr))
								continue
							}
							if rec == nil {
								notifyErrs = append(notifyErrs, fmt.Errorf("attachment %s resolve: not found", id))
								continue
							}
							if serr := deps.SendToUserAttachment(ctx, userID, *rec, data); serr != nil {
								notifyErrs = append(notifyErrs, fmt.Errorf("attachment %s send: %w", id, serr))
								continue
							}
							sentAttachment = true
						}
					} else if len(ids) > 0 {
						notifyErrs = append(notifyErrs, fmt.Errorf("attachment sender unavailable"))
					}
				}

				// Append to active chat session so cortex sees it in conversation history.
				uid := userID.String()
				var sessID string
				_ = shipDB.GetContext(ctx, &sessID,
					`SELECT id FROM chat_sessions WHERE user_id = $1 AND soul_id = $2 AND source = 'chat' AND active = true ORDER BY updated_at DESC LIMIT 1`,
					uid, core.SoulIDFromContext(ctx))
				if sessID != "" && strings.TrimSpace(cleaned) != "" {
					_ = msgStore.Append(ctx, sessID, core.Message{
						Role:    "assistant",
						Content: core.NormalizeContent(cleaned),
					})
				}

				// A marker-only message (no prose) has nothing left to send as
				// text once the file is dispatched.
				if strings.TrimSpace(cleaned) == "" {
					if sentAttachment {
						return receipt, nil
					}
					return receipt, errors.Join(notifyErrs...)
				}

				// Send to Telegram. Prefer the per-user multi-bot sender wired
				// by the gateway — it reads the user's actual paired bot and
				// sends through it (legacy Transport.BotToken would 403 for
				// users who never opened the host owner's private bot).
				if deps.SendToUser != nil {
					if err := deps.SendToUser(ctx, userID, cleaned); err != nil {
						notifyErrs = append(notifyErrs, err)
					}
				} else if deps.Sender != nil {
					chatID := profile.ChatID
					if idx := strings.Index(chatID, ":"); idx >= 0 {
						chatID = chatID[idx+1:]
					}
					if err := deps.Sender.SendLong(ctx, chatID, cleaned); err != nil {
						notifyErrs = append(notifyErrs, err)
					}
				} else {
					notifyErrs = append(notifyErrs, fmt.Errorf("no sender configured"))
				}
				return receipt, errors.Join(notifyErrs...)
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
		if s.cfg.A2A.TaskTrigger != nil {
			agentTaskTrigger = s.cfg.A2A.TaskTrigger
		}
	}

	// 5. Start Gateway. The gateway is the inbound-message router for every
	// transport (Telegram, WebSocket, future ones). Multi-bot Telegram is
	// driven by the host's ListBots hook (or legacy BotToken fallback);
	// the gateway is built as long as ANY transport is configured because
	// HTTPChat / WebSocket sit on top of the same gateway, and ReloadBots
	// then decides whether a Telegram fan-in actually runs.
	telegramConfigured := s.cfg.Transport.Telegram.ListBots != nil || s.cfg.Transport.BotToken != ""
	wsConfigured := s.cfg.Transport.WebSocket.Port > 0
	hcConfigured := s.cfg.Transport.HTTPChat.Port > 0
	if telegramConfigured || wsConfigured || hcConfigured {
		if taskStore != nil {
			// The gateway owns pair-local turn ordering. Finalize a provider-
			// acknowledged autonomous message, and repair any prior confirmed
			// message, only while that gateway lock is held.
			deps.FinalizeAutonomousNotification = func(
				finalizeCtx context.Context,
				attemptID uuid.UUID,
				receipt core.TaskNotificationReceipt,
			) error {
				if err := taskStore.ConfirmNotificationAttempt(finalizeCtx, attemptID, receipt); err != nil {
					return err
				}
				return taskStore.ProjectAutonomousHistoryAttempt(finalizeCtx, attemptID)
			}
			deps.EnsureAutonomousHistory = taskStore.EnsureAutonomousHistoryForSession
		}
		var err error
		gw, err = gateway.NewGateway(deps, reg, s.logger)
		if err != nil {
			return fmt.Errorf("create gateway: %w", err)
		}
		// Wire the gateway's per-user/per-bot sender so the agent-task
		// scheduler can deliver Notify via the SAME bot the user paired
		// with, instead of the legacy single-bot Transport.BotToken.
		deps.SendToUser = gw.SendToUser
		deps.SendToUserOnce = gw.SendToUserOnce
		deps.SendConversationMessage = gw.SendConversationMessage
		deps.SendToUserAttachment = gw.SendToUserAttachment
		deps.DraftAutonomousTurn = gw.DraftAutonomousTurn
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

	// Start task execution only after the initial outbound gateway wiring and
	// bot load. RunLoopWithTrigger executes immediately; starting it above the
	// gateway used to race deps.SendToUserOnce assignment and could reserve a
	// due occurrence as uncertain without making any Telegram request.
	if agentSched != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			looprunner.RunLoopWithTrigger(ctx, s.logger, "agent-tasks", 1*time.Minute, agentSched.Run, agentTaskTrigger, agentSched.WakeFromCallback)
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
		hcSrv := httpchat.NewServer(gw, hcCfg.Port, hcCfg.Token, hcCfg.TransportName, hcCfg.ValidateUserSoul, hcCfg.Extras, hcCfg.Reset, s.logger)
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

// voiceHandoffText renders a delivery-task payload in the soul's chat
// persona: one short LLM pass with the persona as system prompt. The
// worker that produced the payload ran persona-free by design; this is
// where the companion's voice re-enters. Falls back to the raw payload
// on any error — delivery beats style.
func voiceHandoffText(ctx context.Context, deps *core.Deps, cfg Config, payload string) string {
	if deps == nil || deps.LLM == nil {
		return payload
	}
	persona := ""
	if cfg.Gateway.ResolveSoulPersona != nil {
		if soulID, ok := core.SoulIDFromContextOK(ctx); ok {
			if p, err := cfg.Gateway.ResolveSoulPersona(ctx, soulID); err == nil {
				persona = p
			}
		}
	}
	model := cfg.Models.Primary.ForRouter()
	if model == "" {
		return payload
	}
	system := persona + "\n\nФоновая задача, которую ты сама ставила, вернулась с результатом. Передай его пользователю своими словами: 1-3 предложения, суть + источник, твой обычный тон. Без служебных рамок, без «задача завершена», без отчётности — просто скажи как подруга, которая узнала и рассказывает."
	llmCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := deps.LLM.Complete(llmCtx, core.CompletionRequest{
		Model:     model,
		System:    system,
		MaxTokens: 400,
		Messages:  []core.Message{{Role: "user", Content: core.NormalizeContent("[результат задачи]\n" + payload)}},
	})
	if err != nil {
		return payload
	}
	voiced := strings.TrimSpace(core.ExtractText(resp.Content))
	if voiced == "" {
		return payload
	}
	return voiced
}
