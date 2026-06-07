package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

// scratchpadRE strips <scratchpad>…</scratchpad> internal working notes from a
// task's final reply before it reaches the user (see the [DONE] cleaning).
var scratchpadRE = regexp.MustCompile(`(?is)<scratchpad>.*?</scratchpad>`)

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

func (b *Background) Run(ctx context.Context, task core.AgentTask, deps core.AgentDeps) (core.IterationResult, error) {
	// 1. Load system prompt.
	// Task config may override the instruction prompt key (default: "background-task").
	// system_prompt_keys, if set, replaces deps.Config.SystemPromptKeys for
	// this task — useful when chat-mode prompts (preamble/agents) are wrong
	// for autonomous reflection (inner-thought has no user speech, no
	// message_send confirmation, no intent detection from user input).
	// instructionKey is appended last in either case.
	// notify_default controls whether the final reply is auto-pushed to the
	// user as Notify on the last iteration. Heartbeat-style tasks want true
	// (default); inner-thought-style silent reflection wants false. With
	// notify_default=false the LLM can still ping the user by including a
	// [NOTIFY] marker in the reply.
	instructionKey := "background-task"
	notifyDefault := true
	skipReflex := false
	// inputMode controls how the instruction reaches the model:
	//   prompt_key (default) — instruction in the system prompt + a [TASK:…]
	//     user turn through the chat persona (legacy behaviour).
	//   system — instruction in the system prompt + a NEUTRAL trigger turn
	//     that forbids conversational preamble. The assistant executes its
	//     own proactive tick and returns only the result/[no-op] — this is
	//     what stops a heartbeat from opening with "щас гляну".
	//   user — the instruction text is delivered AS the user's message; the
	//     model replies conversationally in persona (system carries persona
	//     only). For chat-authored "message me on a schedule" tasks.
	inputMode := "prompt_key"
	var promptKeys []string
	if task.Config != nil {
		var cfg struct {
			Prompt           string   `json:"prompt"`
			NotifyDefault    *bool    `json:"notify_default"`
			SystemPromptKeys []string `json:"system_prompt_keys"`
			SkipReflex       bool     `json:"skip_reflex"`
			InputMode        string   `json:"input_mode"`
		}
		if json.Unmarshal(task.Config, &cfg) == nil {
			if cfg.Prompt != "" {
				instructionKey = cfg.Prompt
			}
			if cfg.NotifyDefault != nil {
				notifyDefault = *cfg.NotifyDefault
			}
			if len(cfg.SystemPromptKeys) > 0 {
				promptKeys = append(promptKeys, cfg.SystemPromptKeys...)
			}
			skipReflex = cfg.SkipReflex
			if cfg.InputMode != "" {
				inputMode = cfg.InputMode
			}
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
		if task.Strategy == core.StrategyDirect && instructionKey == "background-task" {
			// minimal: only the background-task instruction below
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
	gw := deps.Config.Gateway
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
		for _, key := range promptKeys {
			p, err := deps.Prompts.Get(ctx, key)
			if err != nil {
				return core.IterationResult{}, fmt.Errorf("load prompt %q: %w", key, err)
			}
			parts = append(parts, p)
		}
	}

	// Resolve the instruction. resolvePromptOrBody returns the file contents
	// when instructionKey names a real prompt (heartbeat, background-task, …)
	// and otherwise treats the string itself as an inline body — so a cabinet/
	// chat-authored task can carry its own instruction text without a file.
	instr := resolvePromptOrBody(ctx, deps.Prompts, instructionKey)

	// In `user` mode the instruction is the user's message (added as the user
	// turn below), so the system prompt is persona-only. Every other mode puts
	// the instruction in the system prompt.
	if inputMode != "user" {
		parts = append(parts, instr)
	}

	systemPrompt := strings.Join(parts, "\n\n")

	now := time.Now().In(b.tz)
	systemPrompt = fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), systemPrompt)

	// 2. Parse progress (contains session_id for shared session + pause state)
	var progress bgProgress
	if len(task.Progress) > 0 && string(task.Progress) != "{}" {
		json.Unmarshal(task.Progress, &progress)
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
	var roleEffort, roleThinkingMode string
	if deps.ModelStore != nil {
		if m := deps.ModelStore.ForRouter(modelRole); m != "" {
			routerModel = m
		}
		if ref := deps.ModelStore.Get(modelRole); ref.Name != "" {
			displayModel = ref.Name
			roleEffort = ref.Effort
			roleThinkingMode = ref.ThinkingMode
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

	isLast := task.MaxIterations > 0 && task.Iteration+1 >= task.MaxIterations

	var msg string

	switch inputMode {
	case "system":
		// Instruction lives in the system prompt and IS the whole job of this
		// tick. The trigger turn frames it as the assistant's OWN proactive
		// check (not a user request) and bans any acknowledgement — only the
		// finished message or [no-op] should come back. This is the fix for a
		// heartbeat opening with "щас гляну".
		msg = "[Proactive tick. Carry out the instructions in your system prompt now. " +
			"This is your OWN background check, not a user request — do not acknowledge, " +
			"greet, or narrate (no \"let me check\", no \"щас гляну\"). Reply with ONLY the " +
			"finished message to send the user, or exactly [no-op] if there is nothing to send.]"
	case "user":
		// The instruction text is delivered as if the user wrote it; the model
		// replies conversationally in persona.
		if strings.TrimSpace(instr) != "" {
			msg = instr
		} else {
			msg = "(scheduled check-in)"
		}
	default:
		// prompt_key (legacy) — pause-resume + multi-phase framing.
		if progress.PeerTaskID != "" && progress.Phase == "waiting" {
			// Resumed from pause — tell LLM what woke it + last progress summary.
			resumeMsg := fmt.Sprintf("[RESUME] You were paused waiting for peer task %s. Check its current status and decide next steps.",
				progress.PeerTaskID)
			if progress.Summary != "" {
				resumeMsg += fmt.Sprintf("\n\nLast progress: %s", progress.Summary)
			}
			msg = fmt.Sprintf("%s\nIteration: %d/%d", resumeMsg, task.Iteration+1, task.MaxIterations)
		} else if instructionKey != "background-task" {
			// Tasks with a custom prompt (config.prompt) are self-contained —
			// no multi-phase planning/execution/synthesis overlay.
			msg = fmt.Sprintf("[TASK: %s]\n%s\nIteration: %d/%d",
				task.Title, desc, task.Iteration+1, task.MaxIterations)
		} else {
			isFirst := task.Iteration == 0

			phaseKey := "background-execution"
			if isFirst {
				phaseKey = "background-planning"
			} else if isLast {
				phaseKey = "background-synthesis"
			}
			phasePrompt, _ := deps.Prompts.Get(ctx, phaseKey)

			msg = fmt.Sprintf("[TASK: %s]\nMission: %s\nIteration: %d/%d\n\n%s",
				task.Title, desc, task.Iteration+1, task.MaxIterations, phasePrompt)
		}
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
		reflex := runReflexPipeline(ctx, deps, b.tz, msg)
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
	loop.SetCompactor(agent.NewCompactor(deps.LLM, deps.Config, deps.Logger))

	result, err := loop.RunTracked(ctx, agent.RunConfig{
		SessionID:       sessID,
		SystemPrompt:    systemPrompt,
		InjectedContext: injectedCtx,
		Model:           routerModel,
		MaxTokens:       deps.Config.Limits.MaxOutputTokens,
		MaxTurns:        deps.Config.Gateway.MaxTurns,
		Role:            modelRole,
		Effort:          roleEffort,
		ThinkingMode:    roleThinkingMode,
	}, msg)
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("agent loop: %w", err)
	}

	reply := result.Text

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
		clean := strings.ReplaceAll(reply, "[DONE]", "")
		clean = strings.ReplaceAll(clean, "[CONTINUE]", "")
		clean = strings.ReplaceAll(clean, "[PAUSE]", "")
		clean = strings.ReplaceAll(clean, "[MILESTONE]", "")
		clean = strings.ReplaceAll(clean, "[NOTIFY]", "")
		// Strip <scratchpad>…</scratchpad> — the model's internal working
		// notes (e.g. the synthesis grounding self-audit). It's a forcing
		// function for the model, not something the user should ever see.
		clean = scratchpadRE.ReplaceAllString(clean, "")
		clean = strings.TrimSpace(clean)

		// Archive session (one-shot, no reuse after task completion).
		deps.Store.ArchiveSession(ctx, sessID)

		// Filter no-op and garbage output (e.g. raw UUIDs from tool results).
		if clean == "" || strings.Contains(clean, "[no-op]") || isGarbageOutput(clean) {
			return core.IterationResult{Done: true, ToolCallsJSON: toolCallsJSON}, nil
		}
		notify := clean
		if !notifyDefault && !hadNotifyMarker {
			notify = ""
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

		return core.IterationResult{
			Pause:    true,
			Progress: progressJSON,
			Notify: fmt.Sprintf("%s — peer task %s failed %d times. Need human input.\n\n%s",
				task.Title, progress.PeerTaskID, progress.RevisionCount, truncate(reply, 300)),
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

	return core.IterationResult{
		Done:          false,
		Progress:      progressJSON,
		Notify:        notify,
		Output:        reply,
		ToolCallsJSON: toolCallsJSON,
	}, nil
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
