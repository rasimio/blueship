package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lib/pq"

	bs "github.com/rasimio/blueship/core"
)

// Tool name constants — see builtin.go for the rationale.
const (
	ToolAgentTaskCreate  = "agent_task_create"
	ToolAgentTaskStatus  = "agent_task_status"
	ToolAgentTaskList    = "agent_task_list"
	ToolAgentTaskCancel  = "agent_task_cancel"
	ToolAgentTaskAccept  = "agent_task_accept"
	ToolAgentTaskApprove = "agent_task_approve"
)

// RegisterAgentTaskTools adds the BlueShip-primitive task tools
// (agent_task_create / status / list / cancel / approve) to the registry.
//
// agent_tasks is the universal task primitive: any agent on BlueShip can
// kick off recurring jobs (handler-driven), one-shot LLM cycles
// (strategy=direct), planned multi-step work (strategy=structured), or
// delegated work that another agent runs end-to-end (strategy=delegate).
//
// Descriptions are colocated with each Register call. Agents that need a
// persona-specific version can re-register the same name in their own
// domain modules — the registry's last writer wins.
func RegisterAgentTaskTools(r *bs.ToolRegistry, d *bs.Deps) error {
	db, err := d.DB("ship")
	if err != nil {
		return fmt.Errorf("agent_task tools: ship DB: %w", err)
	}
	store := bs.NewAgentTaskStore(db)

	// -------------------------------------------------------------------
	// agent_task_create
	// -------------------------------------------------------------------
	r.Register(ToolAgentTaskCreate,
		"Kick off an autonomous task. Choose strategy carefully — wrong choice = wasted budget:\n"+
			"  • direct — DEFAULT for almost everything. LLM runs in a loop with the configured tools (web_search, browser_fetch, memory_*, etc.), iterates freely, and finishes when acceptance_criteria is satisfied. USE FOR: research, news digests, Q&A with web sources, deep-dives, market analysis, anything that boils down to 'iterate over tools until the answer is good enough'.\n"+
			"  • structured — ONLY when the task is a fixed multi-phase pipeline with explicit ordering, peer-task callbacks, or revision gates (e.g. delegate code work to Liya: code_task_create → wait → decide → execute → wait → decide → push → open_pr → merge). The plan field MUST contain a JSON array of step objects {action:tool|wait|decide|milestone|done, ...}. NEVER use structured for research/synthesis — direct does that better.\n"+
			"  • delegate — hand off the WHOLE task to a peer agent (delegate_to = peer agent_id from BlueFleet). Peer runs its own lifecycle and reports terminal status back via callback.\n"+
			"acceptance_criteria — plain language definition of done, checked by an LLM judge AND structural gates after each iteration. Be SPECIFIC and EVIDENTIARY: vague criteria pass weak work.\n"+
				"  - Research / information-gathering: a STRONG research criterion bundles all of the following — vague criteria pass press-release-quality work, while specific ones produce S-tier briefs:\n"+
				"      * minimum URL-citation count (e.g. '≥4 distinct URL citations'),\n"+
				"      * source diversity ('citations spread across ≥3 distinct domains, no single domain over 50%' — and please do NOT pin to one vendor like 'arxiv.org or ai.meta.com only', that defeats triangulation),\n"+
				"      * at least one independent / outside-the-originating-organisation source (third-party benchmark, peer-reviewed reaction, or non-vendor analysis),\n"+
				"      * comparative positioning ('contrasts the subject against ≥2 named peer methods or competing approaches'),\n"+
				"      * limitations or open questions ('includes 2-5 bullet points naming what is contested, unverified, or unsupported'),\n"+
				"      * TL;DR up top ('2-4 sentence executive summary preceding the body').\n"+
				"      Example: 'Brief on X covering definition, paradigm contrasts, history, and 2024-25 state. ≥4 distinct URL citations across ≥3 domains with no single domain >50%; ≥1 source outside the originating org; explicit comparison against ≥2 named peer methods; TL;DR up top; Limitations section of 2-5 bullets; inline [N] citations + References list at the end.' This trips the hard-fetch / diversity / structure gates; without it the evaluator can be fooled by polished single-vendor prose.\n"+
				"  - Code: name the artifact and verification. 'PR open with passing CI on branch X, tests cover new module Y.'\n"+
				"  - Lists / digests: count and qualifier. 'List contains 5+ recent items (≤14 days), each with title, source URL, and 2-sentence summary.'\n"+
				"  - Booking / actions: outcome description. 'Confirmation number captured in the result text; booking page URL fetched.'\n"+
				"use_agents — optional allow-list of peer agent_ids (empty = no peers).\n"+
			"cadence — Go duration string (e.g. '1h', '30m', '15s'). Rate-limits how often a task is allowed to tick — scheduler skips ticks that arrive sooner without burning an iteration. USE FOR periodic monitors ('check BTC price every hour for 6 hours' = cadence: '1h', max_iterations: 6) so wall-clock duration ≈ cadence × max_iterations. Omit for tasks that should iterate as fast as the LLM allows (research, code synthesis, etc.).",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"title":{"type":"string","description":"Short task title"},
				"description":{"type":"string","description":"Detailed mission statement"},
				"acceptance_criteria":{"type":"string","description":"Plain-language definition of done — verified each iteration"},
				"strategy":{"type":"string","enum":["direct","structured","delegate"],"default":"direct"},
				"plan":{"type":"object","description":"Strategy-specific plan: ordered steps for structured, hint for direct, spec for delegate"},
				"delegate_to":{"type":"string","description":"For strategy=delegate: the peer agent_id"},
				"tools":{"type":"array","items":{"type":"string"},"description":"Tool allow-list (omit = full registry)"},
				"use_agents":{"type":"array","items":{"type":"string"},"description":"Peer agent_id allow-list (omit = no peers)"},
				"config":{"type":"object","description":"Strategy-specific config (plan_template, model overrides, etc.)"},
				"max_iterations":{"type":"integer","default":20,"description":"Safety cap on iteration count (1-100)"},
				"cadence":{"type":"string","description":"Min interval between ticks (Go duration: '1h', '30m', '15s'). Rate-limits monitors; omit for fast-iterating tasks."},
				"deadline":{"type":"string","description":"Optional ISO datetime — task fails if not done by this time"}
			},
			"required":["title","description","acceptance_criteria"]
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				Title              string          `json:"title"`
				Description        string          `json:"description"`
				AcceptanceCriteria string          `json:"acceptance_criteria"`
				Strategy           string          `json:"strategy"`
				Plan               json.RawMessage `json:"plan"`
				DelegateTo         string          `json:"delegate_to"`
				Tools              []string        `json:"tools"`
				UseAgents          []string        `json:"use_agents"`
				Config             json.RawMessage `json:"config"`
				MaxIterations      int             `json:"max_iterations"`
				Cadence            string          `json:"cadence"`
				Deadline           string          `json:"deadline"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			if p.MaxIterations <= 0 {
				p.MaxIterations = 20
			}
			if p.MaxIterations < 1 {
				p.MaxIterations = 1
			}
			if p.MaxIterations > 100 {
				p.MaxIterations = 100
			}
			strategy := p.Strategy
			if strategy == "" {
				strategy = bs.StrategyDirect
			}
			switch strategy {
			case bs.StrategyDirect, bs.StrategyStructured, bs.StrategyDelegate:
				// ok
			default:
				return nil, fmt.Errorf("invalid strategy %q (want direct|structured|delegate)", strategy)
			}
			if strategy == bs.StrategyDelegate && p.DelegateTo == "" {
				return nil, fmt.Errorf("strategy=delegate requires delegate_to (peer agent_id)")
			}

			task := bs.AgentTask{
				UserID:        d.UserID,
				Title:         p.Title,
				Description:   &p.Description,
				Strategy:      strategy,
				Plan:          p.Plan,
				Config:        p.Config,
				Tools:         pq.StringArray(p.Tools),
				UseAgents:     pq.StringArray(p.UseAgents),
				MaxIterations: p.MaxIterations,
			}
			if p.AcceptanceCriteria != "" {
				task.AcceptanceCriteria = &p.AcceptanceCriteria
			}
			if p.DelegateTo != "" {
				task.DelegateTo = &p.DelegateTo
			}
			if p.Cadence != "" {
				task.Cadence = &p.Cadence
			}
			created, err := store.Create(ctx, task)
			if err != nil {
				return nil, fmt.Errorf("create agent_task: %w", err)
			}
			return map[string]any{
				"id":             created.ID.String(),
				"title":          created.Title,
				"status":         created.Status,
				"strategy":       created.Strategy,
				"max_iterations": created.MaxIterations,
			}, nil
		},
	)

	// agent_task_status is exposed (federated) so origin Ships can poll
	// delegated peer tasks. Registration body follows.

	// -------------------------------------------------------------------
	// agent_task_status
	// -------------------------------------------------------------------
	r.Register(ToolAgentTaskStatus,
		"Read the full state of a task by id: status (pending/running/paused/done/failed/canceled), strategy, iteration, plan, progress, and final result. Use to check whether an autonomous task has met its acceptance criteria.",
		json.RawMessage(`{"type":"object","properties":{
			"task_id":{"type":"string","description":"Task UUID or 8-char prefix"}
		},"required":["task_id"]}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				TaskID string `json:"task_id"`
				ID     string `json:"id"` // legacy alias
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			raw := p.TaskID
			if raw == "" {
				raw = p.ID
			}
			t, err := store.Resolve(ctx, raw)
			if err != nil {
				return nil, fmt.Errorf("resolve task: %w", err)
			}
			return map[string]any{
				"id":                  t.ID.String(),
				"title":               t.Title,
				"description":         t.Description,
				"acceptance_criteria": t.AcceptanceCriteria,
				"strategy":            t.Strategy,
				"status":              t.Status,
				"iteration":           t.Iteration,
				"max_iterations":      t.MaxIterations,
				"plan":                t.Plan,
				"progress":            t.Progress,
				"result":              t.Result,
				"error_message":       t.ErrorMessage,
				"delegate_to":         t.DelegateTo,
				"use_agents":          []string(t.UseAgents),
				"completed_at":        t.CompletedAt,
			}, nil
		},
	)
	r.Expose("agent_task_status", bs.ToolModeSync)

	// -------------------------------------------------------------------
	// agent_task_list
	// -------------------------------------------------------------------
	r.Register(ToolAgentTaskList,
		"List the agent's tasks, optionally filtered by status. Without a filter, returns every task across every state.",
		json.RawMessage(`{"type":"object","properties":{
			"status":{"type":"string","description":"Optional status filter (pending|running|paused|done|failed|canceled)"}
		}}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(input, &p)
			tasks, err := store.ListForUser(ctx, d.UserID, p.Status)
			if err != nil {
				return nil, fmt.Errorf("list tasks: %w", err)
			}
			out := make([]map[string]any, 0, len(tasks))
			for _, t := range tasks {
				out = append(out, map[string]any{
					"id":        t.ID.String(),
					"title":     t.Title,
					"status":    t.Status,
					"strategy":  t.Strategy,
					"iteration": t.Iteration,
				})
			}
			return map[string]any{"tasks": out, "count": len(out)}, nil
		},
	)

	// -------------------------------------------------------------------
	// agent_task_cancel
	// -------------------------------------------------------------------
	r.Register(ToolAgentTaskCancel,
		"Cancel an active task (pending/running/paused). Tasks already in a terminal state (done/failed/canceled) are unchanged.",
		json.RawMessage(`{"type":"object","properties":{
			"task_id":{"type":"string","description":"Task UUID or 8-char prefix"}
		},"required":["task_id"]}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				TaskID string `json:"task_id"`
				ID     string `json:"id"` // legacy alias
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			raw := p.TaskID
			if raw == "" {
				raw = p.ID
			}
			t, err := store.Resolve(ctx, raw)
			if err != nil {
				return nil, fmt.Errorf("resolve task: %w", err)
			}
			if err := store.Cancel(ctx, t.ID); err != nil {
				return nil, fmt.Errorf("cancel task: %w", err)
			}
			return map[string]any{"id": t.ID.String(), "status": "canceled"}, nil
		},
	)

	// -------------------------------------------------------------------
	// agent_task_accept — peer-facing endpoint for delegate strategy.
	// Origin Ship calls this on peer to hand off a task; peer creates a
	// local agent_task with the supplied spec and returns the new id.
	// Marked exposed=true (federated via Fleet) + mode=sync.
	// -------------------------------------------------------------------
	r.Register(ToolAgentTaskAccept,
		"Accept a delegated task from a peer agent. Creates a local agent_task with the supplied title / description / acceptance_criteria / strategy / plan / tools / use_agents and returns its id. The origin agent watches this task via agent_task_status federated calls until it reaches a terminal state.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"title":{"type":"string"},
				"description":{"type":"string"},
				"acceptance_criteria":{"type":"string"},
				"strategy":{"type":"string","enum":["direct","structured"],"default":"direct"},
				"plan":{"type":"object"},
				"tools":{"type":"array","items":{"type":"string"}},
				"use_agents":{"type":"array","items":{"type":"string"}},
				"max_iterations":{"type":"integer","default":20},
				"origin_agent_id":{"type":"string","description":"For audit: the agent_id that delegated this task"},
				"origin_task_id":{"type":"string","description":"For audit: the originating agent_task id"}
			},
			"required":["title","description"]
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				Title              string          `json:"title"`
				Description        string          `json:"description"`
				AcceptanceCriteria string          `json:"acceptance_criteria"`
				Strategy           string          `json:"strategy"`
				Plan               json.RawMessage `json:"plan"`
				Tools              []string        `json:"tools"`
				UseAgents          []string        `json:"use_agents"`
				MaxIterations      int             `json:"max_iterations"`
				OriginAgentID      string          `json:"origin_agent_id"`
				OriginTaskID       string          `json:"origin_task_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			if p.Strategy == "" {
				p.Strategy = bs.StrategyDirect
			}
			if p.Strategy != bs.StrategyDirect && p.Strategy != bs.StrategyStructured {
				return nil, fmt.Errorf("agent_task_accept supports strategy=direct|structured, got %q", p.Strategy)
			}
			if p.MaxIterations <= 0 {
				p.MaxIterations = 20
			}

			origin := map[string]string{}
			if p.OriginAgentID != "" {
				origin["origin_agent_id"] = p.OriginAgentID
			}
			if p.OriginTaskID != "" {
				origin["origin_task_id"] = p.OriginTaskID
			}
			progress, _ := json.Marshal(map[string]any{"delegated_from": origin})

			task := bs.AgentTask{
				UserID:        d.UserID,
				Title:         p.Title,
				Description:   &p.Description,
				Strategy:      p.Strategy,
				Plan:          p.Plan,
				Tools:         pq.StringArray(p.Tools),
				UseAgents:     pq.StringArray(p.UseAgents),
				MaxIterations: p.MaxIterations,
				Progress:      progress,
			}
			if p.AcceptanceCriteria != "" {
				task.AcceptanceCriteria = &p.AcceptanceCriteria
			}
			created, err := store.Create(ctx, task)
			if err != nil {
				return nil, fmt.Errorf("create delegated task: %w", err)
			}
			return map[string]any{
				"id":     created.ID.String(),
				"status": created.Status,
			}, nil
		},
	)
	r.Expose("agent_task_accept", bs.ToolModeSync)

	// -------------------------------------------------------------------
	// agent_task_approve
	// -------------------------------------------------------------------
	r.Register(ToolAgentTaskApprove,
		"Resume a paused task — used after a manual review milestone (e.g. user approved continuing past a checkpoint). The scheduler picks the task up on its next tick.",
		json.RawMessage(`{"type":"object","properties":{
			"task_id":{"type":"string","description":"Task UUID or 8-char prefix"}
		},"required":["task_id"]}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				TaskID string `json:"task_id"`
				ID     string `json:"id"` // legacy alias
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			raw := p.TaskID
			if raw == "" {
				raw = p.ID
			}
			t, err := store.Resolve(ctx, raw)
			if err != nil {
				return nil, fmt.Errorf("resolve task: %w", err)
			}
			if err := store.Approve(ctx, t.ID); err != nil {
				return nil, fmt.Errorf("approve task: %w", err)
			}
			return map[string]any{"id": t.ID.String(), "status": "pending"}, nil
		},
	)

	return nil
}
