package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/lib/pq"

	bs "github.com/rasimio/blueship/core"
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
	r.Register("agent_task_create",
		"Kick off an autonomous task. Strategy decides how it runs:\n"+
			"  • direct — single LLM cycle with the configured tools; finishes when acceptance_criteria is met.\n"+
			"  • structured — executor walks the supplied plan step-by-step; revises on failure; finishes when acceptance_criteria is met.\n"+
			"  • delegate — plan is shipped to delegate_to (a peer agent_id from BlueFleet); the peer runs the lifecycle locally and reports milestones back.\n"+
			"acceptance_criteria is plain language describing what 'done' looks like — checked after each iteration. use_agents is an optional allow-list of peer agent_ids the task may call (empty = no peers).",
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

	// -------------------------------------------------------------------
	// agent_task_status
	// -------------------------------------------------------------------
	r.Register("agent_task_status",
		"Read the full state of a task by id: status (pending/running/paused/done/failed/canceled), strategy, iteration, plan, progress, and final result. Use to check whether an autonomous task has met its acceptance criteria.",
		json.RawMessage(`{"type":"object","properties":{
			"id":{"type":"string","description":"Task UUID"}
		},"required":["id"]}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			id, err := uuid.Parse(p.ID)
			if err != nil {
				return nil, fmt.Errorf("invalid id: %w", err)
			}
			t, err := store.Get(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("get task: %w", err)
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

	// -------------------------------------------------------------------
	// agent_task_list
	// -------------------------------------------------------------------
	r.Register("agent_task_list",
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
	r.Register("agent_task_cancel",
		"Cancel an active task (pending/running/paused). Tasks already in a terminal state (done/failed/canceled) are unchanged.",
		json.RawMessage(`{"type":"object","properties":{
			"id":{"type":"string","description":"Task UUID"}
		},"required":["id"]}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			id, err := uuid.Parse(p.ID)
			if err != nil {
				return nil, fmt.Errorf("invalid id: %w", err)
			}
			if err := store.Cancel(ctx, id); err != nil {
				return nil, fmt.Errorf("cancel task: %w", err)
			}
			return map[string]any{"id": id.String(), "status": "canceled"}, nil
		},
	)

	// -------------------------------------------------------------------
	// agent_task_approve
	// -------------------------------------------------------------------
	r.Register("agent_task_approve",
		"Resume a paused task — used after a manual review milestone (e.g. user approved continuing past a checkpoint). The scheduler picks the task up on its next tick.",
		json.RawMessage(`{"type":"object","properties":{
			"id":{"type":"string","description":"Task UUID"}
		},"required":["id"]}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			id, err := uuid.Parse(p.ID)
			if err != nil {
				return nil, fmt.Errorf("invalid id: %w", err)
			}
			if err := store.Approve(ctx, id); err != nil {
				return nil, fmt.Errorf("approve task: %w", err)
			}
			return map[string]any{"id": id.String(), "status": "pending"}, nil
		},
	)

	return nil
}
