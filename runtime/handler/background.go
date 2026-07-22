package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

// scratchpadRE strips <scratchpad>…</scratchpad> internal working notes from a
// task's final reply before it reaches the user (see the [DONE] cleaning).
var scratchpadRE = regexp.MustCompile(`(?is)<scratchpad>.*?</scratchpad>`)

// backgroundStatusTailRE catches task-runner acknowledgements that are not the
// user-facing payload. Background work is autonomous: the user should receive
// the nudge/report itself, not a receipt that the agent "did the task".
var backgroundStatusTailRE = regexp.MustCompile(`(?i)^\s*(готово[,.! ]*(напомнила|отметила|записала|сделала|проверила|обновила)?\.?|done[,.! ]*(reminded|noted|saved|updated|checked|sent)?\.?|reminded\.?|noted\.?)\s*$`)

var taskProgramDeliveryAckRE = regexp.MustCompile(`(?m)\n?\[delivered_items:\s*([^\]\r\n]*)\]\s*$`)
var taskProgramDeliveryAckAttemptRE = regexp.MustCompile(`(?i)\[\s*delivered_items\s*:`)

const maxTaskProgramDeliveryNotificationRunes = 3500

const taskProgramDecisionInstructionFrame = `The following trusted visual-flow instruction may refine or override task-specific behavior from the base task template above. It cannot override platform safety, host policy, or persona constraints.`

const backgroundAutonomyFrame = `## Background Autonomy

You are not replying to a fresh user message. This is your own autonomous background process waking up to continue a standing intention.

Treat the task/instructions as something you already chose or accepted earlier, not as an order you need to acknowledge. Do not greet, do not say "I'll check", and do not append completion receipts like "done", "noted", "готово", "напомнила", or "отметила".

If the result should reach the user, write only the message or artefact that should reach them. If nothing should be sent, output exactly [no-op].`

type backendPrefetchConfig struct {
	Tools              []backendPrefetchTool `json:"tools"`
	SkipLLMIfEmptyTool string                `json:"skip_llm_if_empty_tool"`
	DisableLLMTools    bool                  `json:"disable_llm_tools"`
	SkipLLMLocalHours  *backendPrefetchHours `json:"skip_llm_local_hours,omitempty"`
}

type backendPrefetchTool struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type backendPrefetchHours struct {
	From int `json:"from"`
	To   int `json:"to"`
}

func (c backendPrefetchConfig) enabled() bool {
	return len(c.Tools) > 0
}

type Background struct {
	tz           *time.Location
	pauseTools   map[string]bool // tool names that trigger pause when invoked
	reviseTools  map[string]bool // tool names that count as a "revision" (escalation guard)
	defaultTools []string        // role-level tool allowlist enforced at registry-execute level
}

// NewBackground constructs a scheduled-task handler.
//
// pauseTools — async/peer-callable tool names: when the LLM invokes one
// of them, the handler pauses awaiting a callback. Pass nil/empty for
// agents with no async peer integrations.
//
// reviseTools — tool names that, when called, increment the handler's
// revision counter. After maxRevisions consecutive invocations on the
// same peer task the handler pauses and notifies the owner. Prevents
// inner-thought-style agent loops from getting stuck revising a peer
// forever. Pass nil/empty to disable the guard.
//
// defaultTools — the role-level tool allowlist returned by DefaultTools().
// Empty/nil means "every registered tool is callable" (full registry),
// which is what generic agents want when they trust their model not to
// invent tool names. Hosts that need a hard ceiling (e.g. a host's
// background role MUST NOT be able to call agent_task_create no matter
// what the model decides to spit out) pass an explicit allowlist here.
// This is enforced at the scheduler's registry-subset step, so the
// downstream Loop can't Execute() anything outside the list — even if
// the model emits a tool_use the schema didn't advertise (e.g. Gemma's
// occasional plain-text-tool-call habit, or providers that don't strictly
// validate against the schema).
func NewBackground(tz *time.Location, pauseTools, reviseTools map[string]bool, defaultTools []string) *Background {
	if pauseTools == nil {
		pauseTools = map[string]bool{}
	}
	if reviseTools == nil {
		reviseTools = map[string]bool{}
	}
	return &Background{tz: tz, pauseTools: pauseTools, reviseTools: reviseTools, defaultTools: defaultTools}
}

func (b *Background) DefaultTools() []string {
	return b.defaultTools
}

const maxRevisions = 3

func (b *Background) Run(ctx context.Context, task core.AgentTask, deps core.AgentDeps) (iterationResult core.IterationResult, runErr error) {
	var pendingDeliveries []core.TaskDeliveryRef
	var programDeliveryRefs map[string]core.TaskDeliveryRef
	defer func() {
		if len(pendingDeliveries) > 0 {
			iterationResult.PendingDeliveries = append([]core.TaskDeliveryRef(nil), pendingDeliveries...)
		}
	}()

	taskProgram, hasTaskProgram, programErr := core.ParseTaskProgram(task.Config)
	if programErr != nil {
		return core.IterationResult{}, fmt.Errorf("task_program: %w", programErr)
	}

	// 1. Load system prompt.
	// Task config may override the instruction prompt key (default: "background-task").
	// system_prompt_keys, if set, replaces deps.Config.SystemPromptKeys for
	// this task — useful when chat-mode prompts (preamble/agents) are wrong
	// for autonomous reflection (inner-thought has no user speech, no
	// message_send confirmation, no intent detection from user input).
	// include_persona (only meaningful together with system_prompt_keys)
	// opts the soul persona back into that otherwise fully-replaced stack:
	// the resolved persona text is placed before the prompt-key contents,
	// while platform preamble/agents stay excluded.
	// instructionKey is appended last in either case.
	// notify_default controls whether the final reply is auto-pushed to the
	// user as Notify on the last iteration. Heartbeat-style tasks want true
	// (default); inner-thought-style silent reflection wants false. With
	// notify_default=false the LLM can still ping the user by including a
	// [NOTIFY] marker in the reply.
	instructionKey := "background-task"
	notifyDefault := true
	skipReflex := false
	includePersona := false
	// inputMode controls how the instruction reaches the model:
	//   prompt_key (default) — instruction in the system prompt + an
	//     autonomous background trigger turn. This is not framed as a fresh
	//     user request; the model is continuing its own standing intention.
	//   system — instruction in the system prompt + a NEUTRAL trigger turn
	//     that forbids conversational preamble. The assistant executes its
	//     own proactive tick and returns only the result/[no-op] — this is
	//     what stops a heartbeat from opening with "щас гляну".
	//   user — the instruction text is delivered AS the user's message; the
	//     model replies conversationally in persona (system carries persona
	//     only). For chat-authored "message me on a schedule" tasks.
	inputMode := "prompt_key"
	var promptKeys []string
	var skillSlugs []string
	var voiceHandoff bool
	var backendPrefetch backendPrefetchConfig
	if task.Config != nil {
		var cfg struct {
			Prompt           string                 `json:"prompt"`
			NotifyDefault    *bool                  `json:"notify_default"`
			VoiceHandoff     bool                   `json:"voice_handoff"`
			SystemPromptKeys []string               `json:"system_prompt_keys"`
			IncludePersona   bool                   `json:"include_persona"`
			SkipReflex       bool                   `json:"skip_reflex"`
			InputMode        string                 `json:"input_mode"`
			Skills           []string               `json:"skills"`
			BackendPrefetch  *backendPrefetchConfig `json:"backend_prefetch"`
		}
		if json.Unmarshal(task.Config, &cfg) == nil {
			skillSlugs = cfg.Skills
			voiceHandoff = cfg.VoiceHandoff
			if cfg.Prompt != "" {
				instructionKey = cfg.Prompt
			}
			if cfg.NotifyDefault != nil {
				notifyDefault = *cfg.NotifyDefault
			}
			if len(cfg.SystemPromptKeys) > 0 {
				promptKeys = append(promptKeys, cfg.SystemPromptKeys...)
			}
			includePersona = cfg.IncludePersona
			skipReflex = cfg.SkipReflex
			if cfg.InputMode != "" {
				inputMode = cfg.InputMode
			}
			if cfg.BackendPrefetch != nil {
				backendPrefetch = *cfg.BackendPrefetch
			}
		}
	}
	if hasTaskProgram {
		// A typed program is the complete visual execution model. Hidden AME,
		// rule guidance, and reflex pre-actions would add undeclared inputs or
		// side effects, so programs are hermetic regardless of legacy config.
		// Persona and the configured prompt stack are still composed normally.
		skipReflex = true
	}

	// Parse progress early — the role plan (if any) decides which skill body to
	// compose this iteration, so it must be known before the prompt is built.
	var progress bgProgress
	if len(task.Progress) > 0 && string(task.Progress) != "{}" {
		json.Unmarshal(task.Progress, &progress)
	}
	gw := deps.Config.Gateway
	isLast := task.MaxIterations > 0 && task.Iteration+1 >= task.MaxIterations

	// planActive: a multi-iteration research-style task (default background-task
	// flow, ≥4 iterations so plan + ≥2 exec + synthesis fits) with the skill
	// hooks wired. Below that we keep the flat phase flow (no plan theatre).
	planActive := instructionKey == "background-task" && inputMode == "prompt_key" &&
		task.MaxIterations >= 4 && gw.ResolveSkillCatalog != nil && gw.ResolveSkills != nil &&
		// Fallback: if planning (iter 0) didn't yield a usable plan, don't
		// re-plan forever — drop to the flat phase flow from iter 1 on.
		!(progress.Plan == nil && task.Iteration > 0)

	// Resolve the current step + the role for THIS iteration. A step's skill
	// overrides config.skills; planning (no plan yet) and synthesis (plan
	// exhausted) compose no role body.
	var planStep *RoleStep
	effectiveSkills := skillSlugs
	if planActive {
		if progress.Plan != nil {
			planStep = progress.Plan.currentStep()
		}
		switch {
		case progress.Plan == nil:
			effectiveSkills = nil // planning — planner sees the catalog, not a body
		case planStep == nil:
			effectiveSkills = nil // plan exhausted — synthesis is role-neutral
		case len(planStep.Skills) > 0:
			effectiveSkills = planStep.Skills // the step's primary role
		default:
			effectiveSkills = skillSlugs // step has no role → user baseline
		}
	}

	// defaultPersonaStack marks that promptKeys came from the host's default
	// SystemPromptKeys (the chat persona layer), not an explicit per-task
	// override. Only that default stack is swapped for the soul's own persona
	// below — an explicit system_prompt_keys override (e.g. a reflection job
	// that deliberately avoids chat prompts) is left exactly as configured.
	defaultPersonaStack := false
	if promptKeys == nil {
		// Research workers (direct strategy with the default
		// "background-task" prompt) must NOT see chat-cortex prompts:
		// the persona / chat-tool semantics (note_close, message_send
		// markers, feedback) are noise for a research role and actively
		// pull the model toward chat-style hedging instead of grounded
		// citation. Background-task.md is self-contained — it tells the
		// model exactly what shape its work takes. Anything else is bloat.
		//
		// Recurring handlers (heartbeat, inner-thought) still get the
		// chat persona stack because their replies go straight back to
		// the user in chat voice.
		//
		// A task that carries explicit skills runs as a CLEAN role-agent:
		// the skill body IS its persona for this work, so we skip the chat
		// persona/agents stack entirely — each iteration is a clean agent
		// (role + task), not the chat persona with a role piled on top.
		if (task.Strategy == core.StrategyDirect && instructionKey == "background-task") || len(skillSlugs) > 0 {
			// minimal: skill(s) + instruction below, no chat persona stack
		} else {
			promptKeys = append(promptKeys, deps.Config.SystemPromptKeys...)
			defaultPersonaStack = true
		}
	}

	var parts []string

	// Soul-bound tasks must speak in THEIR soul's voice. The default persona
	// stack resolves the "soul" key from the process-global file prompt store,
	// which is the founding soul's persona — so without this every soul's
	// heartbeat would address its user as the founding soul's user. When the
	// host wires the per-soul persona hooks, compose the SAME stack the live
	// gateway uses (platform preamble + this soul's persona + platform agents)
	// instead. Framework consumers without the soul model keep the file path.
	if defaultPersonaStack && task.SoulID != uuid.Nil &&
		gw.ResolveSoulPersona != nil && gw.ResolvePlatformPrompts != nil {
		preamble, agents, err := gw.ResolvePlatformPrompts(ctx)
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("background: platform prompts: %w", err)
		}
		persona, err := gw.ResolveSoulPersona(ctx, task.SoulID)
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("background: soul %s persona: %w", task.SoulID, err)
		}
		parts = append(parts, preamble, persona, agents)
	} else {
		// An explicit system_prompt_keys override replaces the whole persona
		// stack — right for research roles, voiceless for personality-driven
		// recurring tasks. include_persona opts the soul persona back in: the
		// resolved persona text leads, followed by the task's own prompt keys;
		// platform preamble/agents stay excluded. Resolution failure or an
		// absent hook degrades silently to the plain prompt-key stack.
		if includePersona && len(promptKeys) > 0 && !defaultPersonaStack &&
			task.SoulID != uuid.Nil && gw.ResolveSoulPersona != nil {
			if persona, perr := gw.ResolveSoulPersona(ctx, task.SoulID); perr != nil {
				if deps.Logger != nil {
					deps.Logger.WarnContext(ctx, "background: include_persona: soul persona unavailable",
						"task_id", task.ID, "soul_id", task.SoulID, "error", perr)
				}
			} else if strings.TrimSpace(persona) != "" {
				parts = append(parts, persona)
			}
		}
		for _, key := range promptKeys {
			p, err := deps.Prompts.Get(ctx, key)
			if err != nil {
				return core.IterationResult{}, fmt.Errorf("load prompt %q: %w", key, err)
			}
			parts = append(parts, p)
		}
	}

	// Compose the role body for THIS iteration after the persona/agents layer
	// and before the task instruction. effectiveSkills is the current plan
	// step's role (S2), or the task's config.skills baseline, or nil during
	// planning/synthesis — so the model adopts exactly one role while the
	// instruction stays the task.
	if len(effectiveSkills) > 0 && gw.ResolveSkills != nil {
		if bodies, serr := gw.ResolveSkills(ctx, effectiveSkills); serr != nil {
			if deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "background: resolve skills failed",
					"skills", effectiveSkills, "error", serr)
			}
		} else {
			parts = append(parts, bodies...)
		}
	}

	// Resolve the instruction. resolvePromptOrBody returns the file contents
	// when instructionKey names a real prompt (heartbeat, background-task, …)
	// and otherwise treats the string itself as an inline body — so a cabinet/
	// chat-authored task can carry its own instruction text without a file.
	instr := resolvePromptOrBody(ctx, deps.Prompts, instructionKey)

	// All background work gets the autonomy frame. Even user-authored scheduled
	// tasks are being executed by the agent's own background process now, not
	// answered as a live inbound chat turn.
	parts = append(parts, backgroundAutonomyFrame)

	// In `user` mode the instruction is the user's message (added as the user
	// turn below), so the system prompt is persona + autonomy only. Every other
	// mode puts the instruction in the system prompt.
	if inputMode != "user" {
		parts = append(parts, instr)
	}

	systemPrompt := strings.Join(parts, "\n\n")
	if hasTaskProgram && taskProgram.Decision.Instruction != "" {
		systemPrompt += "\n\n[task_program_decision_instruction trusted=\"true\"]\n" +
			taskProgramDecisionInstructionFrame + "\n\n" +
			taskProgram.Decision.Instruction +
			"\n[/task_program_decision_instruction]"
	}

	// [current_datetime] in the TASK OWNER's timezone (falls back to the
	// process tz). A heartbeat that reasons about reminder windows must see the
	// user's wall-clock, not the server's.
	now := time.Now().In(deps.Config.Gateway.TimezoneFor(ctx, b.tz))
	systemPrompt = fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), systemPrompt)

	var preloadedTraces []agent.ToolTrace
	var preloadedBlock string
	var programToolOverride []string
	if hasTaskProgram {
		execution, err := runTaskProgram(ctx, deps, *taskProgram, now)
		programDeliveryRefs = execution.DeliveryRefs
		if err != nil {
			toolCallsJSON, _ := json.Marshal(execution.Traces)
			if len(toolCallsJSON) == 0 {
				toolCallsJSON = json.RawMessage("[]")
			}
			return core.IterationResult{ToolCallsJSON: toolCallsJSON}, err
		}
		preloadedTraces = execution.Traces
		preloadedBlock = execution.PromptBlock
		programToolOverride = execution.ToolOverride
		if execution.SkipReason != "" {
			if deps.Logger != nil {
				deps.Logger.InfoContext(ctx, "background: task program skipped llm",
					"task_id", task.ID,
					"reason", execution.SkipReason,
				)
			}
			toolCallsJSON, _ := json.Marshal(preloadedTraces)
			if len(toolCallsJSON) == 0 {
				toolCallsJSON = json.RawMessage("[]")
			}
			return core.IterationResult{Done: true, ToolCallsJSON: toolCallsJSON}, nil
		}
	} else if backendPrefetch.enabled() {
		traces, block, skipReason, err := runBackendPrefetch(ctx, deps, backendPrefetch, now)
		if err != nil {
			return core.IterationResult{}, err
		}
		preloadedTraces = traces
		preloadedBlock = block
		if skipReason != "" {
			if deps.Logger != nil {
				deps.Logger.InfoContext(ctx, "background: backend prefetch skipped llm",
					"task_id", task.ID,
					"reason", skipReason,
				)
			}
			toolCallsJSON, _ := json.Marshal(preloadedTraces)
			if len(toolCallsJSON) == 0 {
				toolCallsJSON = json.RawMessage("[]")
			}
			return core.IterationResult{Done: true, ToolCallsJSON: toolCallsJSON}, nil
		}
	}

	// 3. Resolve model: router format for LLM, display name for session.
	//
	// Default by strategy: recurring tick-based tasks (heartbeat, inner-
	// thought, etc.) use the cheap fast `recurring` role; everything else
	// (direct one-shot deep work, structured plans, etc.) uses `background`
	// — a more capable model since those iterations do real synthesis or
	// planning, not 1-minute polling. agent_task is a universal primitive
	// so the split is strategy-driven, not handler-specific.
	//
	// Task config can override either default via `model_role` so a tiny
	// recurring monitor can run on gemma even if the deploy upgrades the
	// recurring default, and a giant research task can pin a specific
	// frontier model without the operator editing config tables.
	modelRole := "background"
	if task.Strategy == core.StrategyRecurring {
		modelRole = "recurring"
	}
	if task.Config != nil {
		var roleCfg struct {
			ModelRole string `json:"model_role"`
		}
		if json.Unmarshal(task.Config, &roleCfg) == nil && roleCfg.ModelRole != "" {
			modelRole = roleCfg.ModelRole
		}
	}

	routerModel := deps.Config.Models.Primary.ForRouter()
	displayModel := deps.Config.Models.Primary.Name
	roleMaxTokens := deps.Config.Limits.MaxOutputTokens
	roleMessageBudget := 0
	roleMessageBudgetSource := ""
	roleThinkingBudget := 0
	var roleEffort, roleThinkingMode string
	if deps.ModelStore != nil {
		if m := deps.ModelStore.ForRouter(modelRole); m != "" {
			routerModel = m
		}
		if ref := deps.ModelStore.Get(modelRole); ref.Name != "" {
			displayModel = ref.Name
			if ref.MaxTokens > 0 {
				roleMaxTokens = ref.MaxTokens
			}
			if ref.MessageBudget > 0 {
				decision := core.ResolveMessageBudget(core.MessageBudgetRequest{
					Role:     modelRole,
					ModelRef: ref,
					Config:   deps.Config,
				})
				roleMessageBudget = decision.Budget
				roleMessageBudgetSource = decision.Source
			}
			roleEffort = ref.Effort
			roleThinkingMode = ref.ThinkingMode
			roleThinkingBudget = core.ThinkingBudgetForModelRef(ref)
		}
	}

	// 4. Get or create session.
	// Recurring tasks (schedule != "") get a fresh session each iteration
	// to prevent unbounded history growth. Non-recurring tasks share a session
	// across iterations so the LLM sees full context.
	sessID := progress.SessionID
	if sessID == "" || task.Schedule != nil {
		var err error
		sessID, err = deps.Store.CreateSessionWithSource(ctx, task.UserID.String(), displayModel, "agent_task", task.ID.String())
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("create session: %w", err)
		}
		progress.SessionID = sessID
	}
	// Recurring tasks: archive session when done (progress is reset between runs).
	// Use background context — parent ctx may be cancelled on shutdown.
	if task.Schedule != nil {
		defer deps.Store.ArchiveSession(context.Background(), sessID)
	}

	// 5. Build user message based on iteration phase
	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}

	var msg string

	switch inputMode {
	case "system":
		// Instruction lives in the system prompt and IS the whole job of this
		// tick. The trigger turn frames it as the assistant's OWN proactive
		// check (not a user request) and bans any acknowledgement — only the
		// finished message or [no-op] should come back. This is the fix for a
		// heartbeat opening with "щас гляну".
		msg = "[Autonomous background tick. Carry out the standing intention in your system prompt now. " +
			"This is your own background check, not a user request. Do not acknowledge, greet, narrate, " +
			"or append a completion receipt. Reply with only the finished message to send the user, " +
			"or exactly [no-op] if there is nothing to send.]"
	case "user":
		// The instruction text is user-authored, but this is still a background
		// wakeup. Frame it as a standing instruction, not a fresh inbound chat
		// message that needs acknowledgement.
		if strings.TrimSpace(instr) != "" {
			msg = fmt.Sprintf("%s\n\nUser-authored standing instruction:\n%s",
				formatBackgroundCycleHeader(task.Title, desc), instr)
		} else {
			msg = formatBackgroundCycleHeader(task.Title, desc)
		}
	default:
		// prompt_key — pause-resume + multi-phase framing, expressed as
		// autonomous background work rather than a fresh user task.
		if progress.PeerTaskID != "" && progress.Phase == "waiting" {
			// Resumed from pause — tell LLM what woke it + last progress summary.
			resumeMsg := fmt.Sprintf("[Autonomous background resume]\nYou were paused waiting for peer task %s. Check its current status and decide next steps.",
				progress.PeerTaskID)
			if progress.Summary != "" {
				resumeMsg += fmt.Sprintf("\n\nLast progress: %s", progress.Summary)
			}
			msg = fmt.Sprintf("%s\nIteration: %d/%d", resumeMsg, task.Iteration+1, task.MaxIterations)
		} else if instructionKey != "background-task" {
			// Tasks with a custom prompt (config.prompt) are self-contained —
			// no multi-phase planning/execution/synthesis overlay.
			msg = fmt.Sprintf("%s\nIteration: %d/%d",
				formatBackgroundCycleHeader(task.Title, desc),
				task.Iteration+1, task.MaxIterations)
		} else if planActive {
			// S2: plan (iter 0) → per-step execution → synthesis. The phase is
			// driven by the plan's state, not the raw iteration index.
			var phaseKey, planBlock string
			switch {
			case progress.Plan == nil:
				// Planning: show the role catalog (descriptions, not bodies) and
				// ask for a <<<PLAN_JSON …>>> plan with one role per step.
				phaseKey = "background-plan"
				if cat, cerr := gw.ResolveSkillCatalog(ctx); cerr == nil {
					planBlock = "\n\n" + formatSkillCatalog(cat)
				}
			case planStep != nil && !isLast:
				// Execution: the current step (its role body is already in the
				// system prompt) plus the whole plan for context.
				phaseKey = "background-step"
				planBlock = "\n\n" + formatPlanForExecutor(progress.Plan, planStep)
			default:
				phaseKey = "background-synthesis"
			}
			phasePrompt, _ := deps.Prompts.Get(ctx, phaseKey)
			msg = fmt.Sprintf("%s\nIteration: %d/%d\n\n%s%s",
				formatBackgroundCycleHeader(task.Title, desc),
				task.Iteration+1, task.MaxIterations, phasePrompt, planBlock)
		} else {
			isFirst := task.Iteration == 0

			phaseKey := "background-execution"
			if isFirst {
				phaseKey = "background-planning"
			} else if isLast {
				phaseKey = "background-synthesis"
			}
			phasePrompt, _ := deps.Prompts.Get(ctx, phaseKey)

			msg = fmt.Sprintf("%s\nIteration: %d/%d\n\n%s",
				formatBackgroundCycleHeader(task.Title, desc),
				task.Iteration+1, task.MaxIterations, phasePrompt)
		}
	}

	if preloadedBlock != "" {
		msg += "\n\n" + preloadedBlock
	}

	// Budget warning.
	remaining := task.MaxIterations - (task.Iteration + 1)
	if remaining <= 3 && remaining > 0 {
		msg += fmt.Sprintf("\n\nLow iteration budget: %d remaining.", remaining)
	}

	// Recheck enforcement. When the previous iteration's Gate C identified
	// ungrounded attribution/architectural claims tied to specific URLs,
	// the task carries a required_recheck_urls list and the evaluator will
	// hard-reject any submit that didn't refetch them this iteration. Make
	// the constraint visible to the model BEFORE it plans the iteration —
	// dropping a recheck URL into the prompt is cheap; getting auto-
	// rejected for missing it costs a full iteration budget.
	if instructionKey == "background-task" && len(task.RequiredRecheckURLs) > 0 {
		var b strings.Builder
		b.WriteString("\n\n[GROUNDING RECHECK — read before any tool call]\n")
		fmt.Fprintf(&b, "Previous iteration's grounding audit flagged claims tied to %d URL(s) as ungrounded. You MUST call browser_fetch on EACH of these BEFORE writing the next report:\n", len(task.RequiredRecheckURLs))
		for i, u := range task.RequiredRecheckURLs {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, u)
		}
		b.WriteString("Acceptance gate hard-rejects the next submit if any URL in this list is missing from this iteration's tool_calls. Refetch first, then verify the specific claims the previous report got wrong, then rewrite.\n")
		msg += b.String()
	}

	// Acceptance-gate feedback. When the previous iteration's Done-claim was
	// rejected, the scheduler merges the reviewer's reason into the task's
	// progress under "acceptance_feedback" (see agenttask.injectFeedback).
	// Surface it so the retry addresses the reviewer's objection instead of
	// resubmitting blind. Staleness is structurally impossible: bgProgress
	// does not carry the key, so the handler's own progress writes drop it —
	// the block renders only on the iteration right after a rejection — and
	// a passing acceptance terminates the task, so feedback never survives
	// a success.
	if fb := acceptanceFeedbackFromProgress(task.Progress); fb != "" {
		msg += fmt.Sprintf("\n\n[acceptance feedback]\nThe previous iteration was rejected by the acceptance gate. Address this before finishing:\n%s\n[/acceptance feedback]",
			truncate(fb, 600))
	}

	// Fetch-rhythm enforcement. Background-task.md tells the model to
	// browser_fetch after every 2-3 browser_search; on 2026-05-11 task
	// 24b8ac16 the model ignored that for 6 iterations straight (1
	// search + 0 fetch per iter). Prompts alone don't enforce ratios.
	//
	// Earlier version of this guard issued a PROHIBITION ("don't run
	// another browser_search until you fetch") and the model went
	// completely passive — iter 9-10 of 24b8ac16 returned empty output
	// with zero tool calls. Negative rule with no alternative paralysed
	// the agent. Replaced with a POSITIVE DICTATION: pull URLs from the
	// task's prior search results and instruct the model to fetch one
	// specific URL right now. Concrete next action beats abstract
	// prohibition.
	if instructionKey == "background-task" && task.Iteration >= 2 {
		searches, fetches := recentBrowserToolUsage(ctx, deps, task.ID, 3)
		totalSearches, totalFetches := recentBrowserToolUsage(ctx, deps, task.ID, 100)
		// Trigger when ANY of:
		// (a) recent 3 iters look bad (≥2 search, 0 fetch — "stuck searching"),
		// (b) running total ratio is unacceptable (≥4 search, fetch < searches/3),
		// (c) recent 3 iters had ZERO tool calls AND task is past iter 3
		//     ("went passive" — 2026-05-13 world-models-v2 saw iters 6-15
		//     with 0 tools each while the LLM wrote synthesis from memory.
		//     Neither (a) nor (b) caught it because searches were also 0).
		// (d) absolute under-fetching for task age (iter ≥ 5 with 0 total
		//     fetches — task is clearly drifting toward synthesis-only).
		hardTrigger := (searches >= 2 && fetches == 0) ||
			(totalSearches >= 4 && totalFetches < totalSearches/3) ||
			(task.Iteration >= 3 && searches == 0 && fetches == 0) ||
			(task.Iteration >= 5 && totalFetches == 0)
		if hardTrigger {
			urls := recentSearchResultURLs(ctx, deps, task.ID, 5)
			var urlsBlock string
			if len(urls) > 0 {
				urlsBlock = "\nURLs your recent browser_search calls surfaced:\n"
				for i, u := range urls {
					urlsBlock += fmt.Sprintf("  %d. %s\n", i+1, u)
				}
				urlsBlock += "Pick the most relevant URL and call browser_fetch on it as your VERY FIRST action this iteration.\n"
			} else {
				urlsBlock = "Re-issue a focused browser_search and then immediately browser_fetch the top result.\n"
			}
			msg += fmt.Sprintf(
				"\n\n[SYSTEM ENFORCEMENT — read before any tool call]\n"+
					"Search/fetch ratio is failing. Recent 3 iters: %d search / %d fetch.\n"+
					"Task total: %d search / %d fetch. The acceptance gate will reject\n"+
					"a result that cites URLs you never fetched — only URLs in your\n"+
					"actual browser_fetch tool_calls count; substring `https://` in\n"+
					"output is NOT a citation.\n"+
					"%s"+
					"Do NOT just describe what you would fetch. CALL browser_fetch RIGHT NOW.",
				searches, fetches, totalSearches, totalFetches, urlsBlock)
		} else if searches >= 6 && fetches < searches/3 {
			msg += fmt.Sprintf(
				"\n\n[SYSTEM ENFORCEMENT]\n"+
					"Recent ratio is search=%d fetch=%d — too low. Target ≈2:1.\n"+
					"Pick an unread URL from your prior search results and call\n"+
					"browser_fetch on it this iteration before any further synthesis.",
				searches, fetches)
		}
	}

	// 6. Run reflex pipeline (same System 1/2 architecture as cortex gateway).
	// Skipped when task config sets skip_reflex=true. Tasks like inner-thought
	// have no user message — reflex was designed to interpret one — and the
	// shared AME engine surfaces past reflections that just feed back into the
	// next reflection. With skip_reflex the agent gets a clean prompt and
	// tools; any context it needs it must pull through the tools itself.
	var injectedCtx string
	if !skipReflex {
		reflex := runReflexPipeline(ctx, deps, b.tz, sessID, msg)
		injectedCtx = reflex.InjectedCtx
		if reflex.Guidance != "" {
			if injectedCtx != "" {
				injectedCtx += "\n\n" + reflex.Guidance
			} else {
				injectedCtx = reflex.Guidance
			}
		}
	}

	// 7. Run agent loop with tool tracing and compaction.
	loop := agent.NewLoop(deps.LLM, deps.Store, deps.Registry, deps.RoleTools, deps.Config, deps.Logger)
	loop.SetCompactor(newTaskCompactor(ctx, deps))

	maxTurns := deps.Config.Gateway.MaxTurns
	var toolOverride []string
	if hasTaskProgram {
		toolOverride = programToolOverride
		if len(toolOverride) == 0 {
			maxTurns = 1
		}
	} else if backendPrefetch.DisableLLMTools {
		toolOverride = []string{}
		maxTurns = 1
	}
	result, err := loop.RunTracked(ctx, agent.RunConfig{
		SessionID:           sessID,
		SystemPrompt:        systemPrompt,
		InjectedContext:     injectedCtx,
		Model:               routerModel,
		MaxTokens:           roleMaxTokens,
		MessageBudget:       roleMessageBudget,
		MessageBudgetSource: roleMessageBudgetSource,
		ThinkingBudget:      roleThinkingBudget,
		MaxTurns:            maxTurns,
		Role:                modelRole,
		ToolOverride:        toolOverride,
		Effort:              roleEffort,
		ThinkingMode:        roleThinkingMode,
	}, msg)
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("agent loop: %w", err)
	}
	if len(preloadedTraces) > 0 {
		combined := make([]agent.ToolTrace, 0, len(preloadedTraces)+len(result.ToolTraces))
		combined = append(combined, preloadedTraces...)
		combined = append(combined, result.ToolTraces...)
		result.ToolTraces = combined
	}

	reply := result.Text

	// S2 plan bookkeeping (handler owns the state; the model only proposes).
	// The planner's first reply builds the plan; each execution reply completes
	// its step. Adaptive patching (PLAN_PATCH_JSON) lands in S2-b.
	if planActive {
		if progress.Plan == nil {
			if p, ok := parsePlanJSON(reply); ok {
				progress.Plan = p
				if deps.Logger != nil {
					deps.Logger.InfoContext(ctx, "background: role plan created",
						"task_id", task.ID, "steps", len(p.Steps))
				}
			}
		} else if planStep != nil {
			summary := extractResultLine(reply)
			// S2-b: the executor may propose plan edits via PLAN_PATCH_JSON.
			// Mark the step that just ran done, then validate+apply the patch
			// (handler owns the state — bad slugs / done-step mutations / over-
			// budget adds are rejected).
			if patch, ok := parsePlanPatch(reply); ok {
				if patch.ResultSummary != "" {
					summary = patch.ResultSummary
				}
				progress.Plan.completeStep(planStep.ID, summary)
				remaining := task.MaxIterations - (task.Iteration + 2) // reserve synthesis
				if remaining < 0 {
					remaining = 0
				}
				if n := progress.Plan.applyPatch(patch, catalogSlugs(ctx, gw), remaining, deps.Logger); n > 0 && deps.Logger != nil {
					deps.Logger.InfoContext(ctx, "background: plan patched",
						"task_id", task.ID, "ops_applied", n, "plan_rev", progress.Plan.Rev)
				}
			} else {
				progress.Plan.completeStep(planStep.ID, summary)
			}
		}
	}

	// Serialise tool traces once; every return path passes them through
	// IterationResult so the scheduler can persist them into the
	// agent_task_iterations.tool_calls jsonb column. Empty trace yields
	// `[]` so the column always has a valid JSON array.
	toolCallsJSON, _ := json.Marshal(result.ToolTraces)
	if len(toolCallsJSON) == 0 {
		toolCallsJSON = json.RawMessage("[]")
	}

	// 8. Scan tool traces for async peer tools and revision tracking.
	var peerTaskID string
	calledRevise := false

	for _, trace := range result.ToolTraces {
		if b.pauseTools[trace.Name] {
			var out map[string]any
			if json.Unmarshal([]byte(trace.Output), &out) == nil {
				if tid, ok := out["task_id"].(string); ok && tid != "" {
					peerTaskID = tid
				}
			}
		}
		if b.reviseTools[trace.Name] {
			calledRevise = true
		}
	}

	// 9. Update revision tracking.
	if peerTaskID != "" && peerTaskID != progress.PeerTaskID {
		progress.RevisionCount = 0
	}
	if calledRevise {
		progress.RevisionCount++
	}

	// 10. Save progress with session ID.
	progress.Phase = fmt.Sprintf("iteration_%d", task.Iteration+1)
	progress.Summary = truncate(reply, 500)
	if peerTaskID != "" {
		progress.PeerTaskID = peerTaskID
	}

	// 11. Check for [DONE].
	if strings.Contains(reply, "[DONE]") || isLast {
		hadNotifyMarker := strings.Contains(reply, "[NOTIFY]")
		deliveryAckValid := len(programDeliveryRefs) == 0
		deliveryAckAttempted := taskProgramDeliveryAckAttemptRE.MatchString(reply)
		clean := strings.ReplaceAll(reply, "[DONE]", "")
		clean = strings.ReplaceAll(clean, "[CONTINUE]", "")
		clean = strings.ReplaceAll(clean, "[PAUSE]", "")
		clean = strings.ReplaceAll(clean, "[MILESTONE]", "")
		clean = strings.ReplaceAll(clean, "[NOTIFY]", "")
		// Strip <scratchpad>…</scratchpad> — the model's internal working
		// notes (e.g. the synthesis grounding self-audit). It's a forcing
		// function for the model, not something the user should ever see.
		clean = scratchpadRE.ReplaceAllString(clean, "")
		// Strip any plan machinery (PLAN_JSON / PLAN_PATCH_JSON) that slipped
		// into a user-facing reply.
		clean = stripPlanMarkers(clean)
		clean = stripBackgroundStatusTails(clean)
		if len(programDeliveryRefs) > 0 {
			clean, pendingDeliveries, deliveryAckValid = consumeTaskProgramDeliveryAck(clean, programDeliveryRefs)
		}
		// A completion receipt can appear immediately before the internal ack
		// line, so run the tail filter again after consuming that control line.
		clean = stripBackgroundStatusTails(clean)
		clean = strings.TrimSpace(clean)

		// Archive session (one-shot, no reuse after task completion).
		deps.Store.ArchiveSession(ctx, sessID)

		// Filter no-op and garbage output (e.g. raw UUIDs from tool results).
		if clean == "" || strings.Contains(clean, "[no-op]") || isGarbageOutput(clean) {
			return core.IterationResult{Done: true, ToolCallsJSON: toolCallsJSON}, nil
		}
		if len(programDeliveryRefs) == 1 && !deliveryAckValid && !deliveryAckAttempted {
			// Narrow compatibility fallback: with exactly one possible delivery,
			// a normal non-empty reminder is unambiguous even when the model
			// omitted the control line entirely. Any explicit control-line attempt
			// remains fail-closed and never reaches this branch.
			for _, ref := range programDeliveryRefs {
				pendingDeliveries = []core.TaskDeliveryRef{ref}
			}
			deliveryAckValid = true
		}
		if len(programDeliveryRefs) > 0 {
			if !deliveryAckValid {
				return core.IterationResult{ToolCallsJSON: toolCallsJSON},
					fmt.Errorf("task_program delivery notification requires a valid [delivered_items:...] acknowledgment")
			}
			if len(pendingDeliveries) == 0 {
				return core.IterationResult{ToolCallsJSON: toolCallsJSON},
					fmt.Errorf("task_program delivery notification must acknowledge at least one represented item")
			}
		}
		notify := clean
		if !notifyDefault && !hadNotifyMarker {
			notify = ""
		}
		// Courier tasks deliver their payload themselves: a successful
		// message_send in this iteration's tool log means the user already
		// has the essence — re-sending the full report on top is noise
		// (live feedback 2026-07-16: weather arrived, followed by an
		// unwanted six-section TL;DR dump).
		if notify != "" && iterationSentMessage(result.ToolTraces) {
			notify = ""
		}
		// Voice handoff (router-stamped delivery skills): the payload goes
		// to the persona layer verbatim — notifyFn voices it in the chat
		// persona instead of dumping worker text.
		if notify != "" && len(pendingDeliveries) > 0 {
			// Delivery-aware notifications must reach the transport intact: the
			// acknowledged refs correspond to this exact text. Generic digest
			// truncation or a voice-handoff rewrite could silently remove later
			// items while still marking them delivered, so reject either case and
			// retry instead.
			if voiceHandoff {
				return core.IterationResult{ToolCallsJSON: toolCallsJSON},
					fmt.Errorf("task_program delivery notification cannot use voice_handoff")
			}
			if utf8.RuneCountInString(notify) > maxTaskProgramDeliveryNotificationRunes {
				return core.IterationResult{ToolCallsJSON: toolCallsJSON},
					fmt.Errorf("task_program delivery notification exceeds %d characters", maxTaskProgramDeliveryNotificationRunes)
			}
		} else if notify != "" && voiceHandoff {
			notify = "[voice_handoff]\n" + notify
		} else if notify != "" {
			// Long reports live in the artefact/result, not in chat: the
			// notify becomes the essence — the TL;DR section when present,
			// otherwise the head — capped, with a pointer to the full text.
			notify = notifyDigest(notify)
		}
		return core.IterationResult{
			Done:          true,
			Output:        clean,
			Notify:        notify,
			ToolCallsJSON: toolCallsJSON,
		}, nil
	}

	// 12. Revision cap — escalate if stuck in error loop.
	if progress.RevisionCount >= maxRevisions {
		progress.Phase = "error_loop"
		progressJSON, _ := json.Marshal(progress)
		notify := fmt.Sprintf("%s — peer task %s failed %d times. Need human input.\n\n%s",
			task.Title, progress.PeerTaskID, progress.RevisionCount, truncate(reply, 300))
		if len(programDeliveryRefs) > 0 {
			// Keyed task-program output is user-visible only through the final
			// ACK-validated notification path above.
			notify = ""
		}

		return core.IterationResult{
			Pause:         true,
			Progress:      progressJSON,
			Notify:        notify,
			ToolCallsJSON: toolCallsJSON,
		}, nil
	}

	// 13. Determine if we should pause.
	// Pause when: new async peer tool was called, or explicit [PAUSE] in reply.
	// No aggressive auto-pause — that causes deadlocks when LLM doesn't call tools.
	shouldPause := peerTaskID != "" || strings.Contains(reply, "[PAUSE]")

	if shouldPause {
		progress.Phase = "waiting"
		progressJSON, _ := json.Marshal(progress)

		var notify string
		if strings.Contains(reply, "[MILESTONE]") {
			notify = fmt.Sprintf("%s (iteration %d/%d)\n\n%s",
				task.Title, task.Iteration+1, task.MaxIterations, truncate(reply, 400))
		}
		if len(programDeliveryRefs) > 0 {
			notify = ""
		}

		return core.IterationResult{
			Pause:         true,
			Progress:      progressJSON,
			Notify:        notify,
			Output:        reply,
			ToolCallsJSON: toolCallsJSON,
		}, nil
	}

	// 14. No pause — continue to next iteration.
	progressJSON, _ := json.Marshal(progress)

	var notify string
	if strings.Contains(reply, "[MILESTONE]") {
		notify = fmt.Sprintf("%s (iteration %d/%d)\n\n%s",
			task.Title, task.Iteration+1, task.MaxIterations, truncate(reply, 400))
	}
	if len(programDeliveryRefs) > 0 {
		notify = ""
	}

	return core.IterationResult{
		Done:          false,
		Progress:      progressJSON,
		Notify:        notify,
		Output:        reply,
		ToolCallsJSON: toolCallsJSON,
	}, nil
}

func consumeTaskProgramDeliveryAck(text string, refs map[string]core.TaskDeliveryRef) (string, []core.TaskDeliveryRef, bool) {
	match := taskProgramDeliveryAckRE.FindStringSubmatchIndex(text)
	if match == nil {
		return text, nil, false
	}
	body := strings.TrimSpace(text[:match[0]])
	if strings.Count(text, "[delivered_items:") != 1 {
		return body, nil, false
	}
	payload := strings.TrimSpace(text[match[2]:match[3]])
	if payload == "" {
		return body, nil, false
	}
	if strings.EqualFold(payload, "none") {
		return body, nil, true
	}
	seen := make(map[core.TaskDeliveryRef]bool)
	acknowledged := make([]core.TaskDeliveryRef, 0)
	for _, token := range strings.Split(payload, ",") {
		token = strings.TrimSpace(token)
		ref, ok := refs[token]
		if token == "" || strings.EqualFold(token, "none") || !ok {
			// The control line is one exact acknowledgment set. Any unknown or
			// contradictory member invalidates the whole set; retrying is safer
			// than marking a possibly unrelated item delivered.
			return body, nil, false
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		acknowledged = append(acknowledged, ref)
	}
	return body, acknowledged, true
}

// bgProgress extends TaskProgress with session management and pause state.
type bgProgress struct {
	core.TaskProgress
	SessionID     string         `json:"session_id"`               // shared session across iterations
	PeerTaskID    string         `json:"peer_task_id,omitempty"`   // async peer task being awaited
	RevisionCount int            `json:"revision_count,omitempty"` // consecutive revisions for same peer task
	DelegatedFrom map[string]any `json:"delegated_from,omitempty"` // preserved across iterations so the
	// scheduler's terminal-status callback can route
	// back to the originating agent.
	Plan *RolePlan `json:"plan,omitempty"` // S2 role-assigned step plan (nil until the planner builds it)
}

// acceptanceFeedbackFromProgress extracts the reject reason the acceptance
// gate merged into the task's progress blob. Deliberately parsed apart from
// bgProgress: keeping the key out of bgProgress means the handler's own
// progress writes never round-trip it, so it self-clears after one iteration.
func acceptanceFeedbackFromProgress(progress json.RawMessage) string {
	if len(progress) == 0 {
		return ""
	}
	var p struct {
		AcceptanceFeedback string `json:"acceptance_feedback"`
	}
	if err := json.Unmarshal(progress, &p); err != nil {
		return ""
	}
	return strings.TrimSpace(p.AcceptanceFeedback)
}

// newTaskCompactor builds the background-loop compactor and equips it with
// the "compact" prompt file as its summarization instruction. A missing or
// empty prompt degrades to an instruction-less summary (the prior behavior)
// rather than failing the iteration.
func newTaskCompactor(ctx context.Context, deps core.AgentDeps) *agent.Compactor {
	c := agent.NewCompactor(deps.LLM, deps.Config, deps.Logger)
	if c == nil {
		return nil
	}
	if deps.Prompts != nil {
		if p, err := deps.Prompts.Get(ctx, "compact"); err == nil && strings.TrimSpace(p) != "" {
			c.SetSystemPrompt(p)
		}
	}
	return c
}

// isGarbageOutput detects raw tool output that shouldn't be sent to users
// (e.g. bare UUID lists, JSON fragments from tool results).
func isGarbageOutput(s string) bool {
	s = strings.TrimSpace(s)
	// Strip commas, spaces, brackets, newlines — if only UUIDs remain, it's garbage.
	cleaned := strings.NewReplacer(",", "", " ", "", "\n", "", "[", "", "]", "").Replace(s)
	// Check if it's just concatenated UUIDs (36 chars each: 8-4-4-4-12)
	if len(cleaned) > 0 && len(cleaned)%36 == 0 {
		allHex := true
		for _, c := range cleaned {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == '-') {
				allHex = false
				break
			}
		}
		if allHex {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// resolvePromptOrBody resolves a task's instruction. When keyOrBody names a
// real prompt file (heartbeat, background-task, …) its contents are returned;
// otherwise the string is treated as an inline instruction body. This lets
// seed/template tasks reference a prompt KEY while cabinet/chat-authored tasks
// carry their own text — without the old hard-fail when Get didn't find a key.
func resolvePromptOrBody(ctx context.Context, prompts core.PromptStore, keyOrBody string) string {
	if prompts != nil {
		if v, err := prompts.Get(ctx, keyOrBody); err == nil && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return keyOrBody
}

func formatBackgroundCycleHeader(title, desc string) string {
	title = strings.TrimSpace(title)
	desc = strings.TrimSpace(desc)
	if title == "" {
		title = "background work"
	}
	var b strings.Builder
	b.WriteString("[Autonomous background cycle]\n")
	b.WriteString("Standing intention: ")
	b.WriteString(title)
	if desc != "" {
		b.WriteString("\nContext: ")
		b.WriteString(desc)
	}
	b.WriteString("\n\nThis is your own background process continuing that intention. Do not acknowledge a task assignment; produce only the next useful action, notification payload, final artefact, or [no-op].")
	return b.String()
}

func stripBackgroundStatusTails(s string) string {
	lines := strings.Split(s, "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if last <= 0 {
		return s
	}
	if !backgroundStatusTailRE.MatchString(lines[last]) {
		return s
	}
	hasPayloadBefore := false
	for i := 0; i < last; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			hasPayloadBefore = true
			break
		}
	}
	if !hasPayloadBefore {
		return s
	}
	return strings.TrimSpace(strings.Join(lines[:last], "\n"))
}

func runBackendPrefetch(ctx context.Context, deps core.AgentDeps, cfg backendPrefetchConfig, now time.Time) ([]agent.ToolTrace, string, string, error) {
	traces := make([]agent.ToolTrace, 0, len(cfg.Tools))
	outputs := make(map[string]string, len(cfg.Tools))
	for _, tool := range cfg.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		input := tool.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		output, isError := deps.Registry.Execute(ctx, name, input)
		trace := agent.ToolTrace{
			Name:   name,
			Input:  string(input),
			Output: output,
			Error:  isError,
		}
		traces = append(traces, trace)
		outputs[name] = output
		if isError {
			return traces, "", "", fmt.Errorf("backend prefetch %s: %s", name, output)
		}
	}

	if cfg.SkipLLMIfEmptyTool != "" && toolResultIsEmpty(outputs[cfg.SkipLLMIfEmptyTool]) {
		return traces, "", "empty_tool:" + cfg.SkipLLMIfEmptyTool, nil
	}
	if cfg.SkipLLMLocalHours != nil && hourInRange(now.Hour(), cfg.SkipLLMLocalHours.From, cfg.SkipLLMLocalHours.To) {
		return traces, "", fmt.Sprintf("local_hour:%02d", now.Hour()), nil
	}
	return traces, formatBackendPrefetchBlock(traces, cfg.DisableLLMTools), "", nil
}

func formatBackendPrefetchBlock(traces []agent.ToolTrace, toolsDisabled bool) string {
	if len(traces) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[backend_prefetched_tool_results]\n")
	b.WriteString("The backend has already executed these tool calls. Treat the results as authoritative input.\n")
	if toolsDisabled {
		b.WriteString("Tools are unavailable for this LLM turn. Do not emit tool calls or tool-like text. Do not emit memory_update; the backend records delivery after a successful notification.\n")
	}
	for _, trace := range traces {
		fmt.Fprintf(&b, "\n[%s input]\n%s\n[%s result]\n%s\n", trace.Name, trace.Input, trace.Name, trace.Output)
	}
	b.WriteString("[/backend_prefetched_tool_results]")
	return b.String()
}

func hourInRange(hour, from, to int) bool {
	if from < 0 {
		from = 0
	}
	if from > 23 {
		from = 23
	}
	if to < 0 {
		to = 0
	}
	if to > 24 {
		to = 24
	}
	if from == to {
		return false
	}
	if from < to {
		return hour >= from && hour < to
	}
	return hour >= from || hour < to
}

func toolResultIsEmpty(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" || s == "null" || s == "[]" || s == "{}" {
		return true
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return false
	}
	return jsonValueIsEmpty(v)
}

func jsonValueIsEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case []any:
		return len(x) == 0
	case map[string]any:
		if len(x) == 0 {
			return true
		}
		for _, key := range []string{"notes", "items", "results", "data", "list"} {
			if child, ok := x[key]; ok {
				return jsonValueIsEmpty(child)
			}
		}
	}
	return false
}

// iterationSentMessage reports whether this iteration's tool trace holds
// a successful message_send — the payload already reached the user.
func iterationSentMessage(traces []agent.ToolTrace) bool {
	for _, t := range traces {
		if t.Name == "message_send" && !t.Error {
			return true
		}
	}
	return false
}

// notifyDigest reduces a completed task's notify to chat-sized essence.
// Reports keep their full text in agent_tasks.result and the artefact
// page; Telegram gets the TL;DR section when the report has one,
// otherwise the head, capped and pointed at the artefacts.
func notifyDigest(text string) string {
	const maxRunes = 600
	body := text
	if m := tldrRE.FindStringSubmatchIndex(text); m != nil {
		section := text[m[1]:]
		if nl := nextHeadingRE.FindStringIndex(section); nl != nil {
			section = section[:nl[0]]
		}
		if s := strings.TrimSpace(section); s != "" {
			body = s
		}
	}
	r := []rune(strings.TrimSpace(body))
	truncated := len(r) > maxRunes || len(body) < len(strings.TrimSpace(text))
	if len(r) > maxRunes {
		body = string(r[:maxRunes]) + "…"
	}
	if truncated {
		body += "\n\nПолный отчёт — в артефактах."
	}
	return body
}

var (
	tldrRE        = regexp.MustCompile(`(?mi)^#*\s*(?:\d+\.\s*)?TL;?DR:?\s*$|TL;?DR\s*[:—-]`)
	nextHeadingRE = regexp.MustCompile(`(?m)^\s*(?:#{1,3}\s+|\d+\.\s+[А-ЯA-Z])`)
)
