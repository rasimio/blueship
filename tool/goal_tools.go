package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/core"
)

// RegisterGoalTools adds the BlueShip-primitive goal tools (goal_create,
// goal_status, goal_list, goal_cancel, goal_approve) to the registry.
//
// These tools operate on the `goals` table via core.GoalStore. Any agent
// running on BlueShip can use them — they're agent-agnostic: the same
// `goal_create` works for Arlene creating a "write a Prometheus exporter"
// goal and for a hypothetical marketing agent creating a "draft Q2
// announcement" goal.
//
// Descriptions are loaded from the tool_descriptions DB table at startup
// (same pattern as other registered tools). Missing description rows in
// the DB cause a startup failure; that's intentional — tool docs are a
// first-class artifact, not code comments.
func RegisterGoalTools(r *bs.ToolRegistry, d *bs.Deps) error {
	db, err := d.DB("ship")
	if err != nil {
		return fmt.Errorf("goal tools: ship DB: %w", err)
	}
	store := bs.NewGoalStore(db)

	r.Register("goal_create", "",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"title":{"type":"string","description":"Short goal title"},
				"description":{"type":"string","description":"Detailed goal description with acceptance criteria"},
				"strategy":{"type":"string","enum":["direct","structured","delegate"],"default":"structured","description":"How to run the goal"},
				"delegate_to":{"type":"string","description":"For strategy=delegate: peer agent_id"},
				"config":{"type":"object","description":"Strategy-specific config (plan_template, etc.)"},
				"tools":{"type":"array","items":{"type":"string"},"description":"Tool allow-list (omit = full registry)"},
				"max_iterations":{"type":"integer","default":20,"description":"Iteration cap (5-50)"}
			},
			"required":["title","description"]
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				Title         string          `json:"title"`
				Description   string          `json:"description"`
				Strategy      string          `json:"strategy"`
				DelegateTo    string          `json:"delegate_to"`
				Config        json.RawMessage `json:"config"`
				Tools         []string        `json:"tools"`
				MaxIterations int             `json:"max_iterations"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			if p.MaxIterations <= 0 {
				p.MaxIterations = 20
			}
			if p.MaxIterations < 5 {
				p.MaxIterations = 5
			}
			if p.MaxIterations > 50 {
				p.MaxIterations = 50
			}
			strategy := bs.GoalStrategy(p.Strategy)
			if strategy == "" {
				strategy = bs.GoalStrategyStructured
			}

			g := bs.Goal{
				UserID:        d.UserID,
				Title:         p.Title,
				Description:   &p.Description,
				Strategy:      strategy,
				Config:        p.Config,
				Tools:         p.Tools,
				MaxIterations: p.MaxIterations,
			}
			if p.DelegateTo != "" {
				g.DelegateTo = &p.DelegateTo
			}
			created, err := store.Create(ctx, g)
			if err != nil {
				return nil, fmt.Errorf("create goal: %w", err)
			}
			return map[string]any{
				"id":             created.ID.String(),
				"title":          created.Title,
				"status":         string(created.Status),
				"strategy":       string(created.Strategy),
				"max_iterations": created.MaxIterations,
			}, nil
		},
	)

	r.Register("goal_status", "",
		json.RawMessage(`{
			"type":"object",
			"properties":{"id":{"type":"string","description":"Goal UUID"}},
			"required":["id"]
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct{ ID string `json:"id"` }
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			id, err := uuid.Parse(p.ID)
			if err != nil {
				return nil, fmt.Errorf("invalid goal id: %w", err)
			}
			g, err := store.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			return g, nil
		},
	)

	r.Register("goal_list", "",
		json.RawMessage(`{
			"type":"object",
			"properties":{"status":{"type":"string","description":"Filter by status (pending/running/paused/done/failed/canceled)"}}
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct{ Status string `json:"status"` }
			_ = json.Unmarshal(input, &p)
			goals, err := store.ListForUser(ctx, d.UserID, p.Status)
			if err != nil {
				return nil, err
			}
			return goals, nil
		},
	)

	r.Register("goal_cancel", "",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"id":{"type":"string","description":"Goal UUID"},
				"reason":{"type":"string","description":"Why cancelled (optional)"}
			},
			"required":["id"]
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				ID     string `json:"id"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			id, err := uuid.Parse(p.ID)
			if err != nil {
				return nil, fmt.Errorf("invalid goal id: %w", err)
			}
			reason := p.Reason
			if reason == "" {
				reason = "cancelled"
			}
			if err := store.Cancel(ctx, id, reason); err != nil {
				return nil, err
			}
			return map[string]string{"id": p.ID, "status": string(bs.GoalStatusCanceled)}, nil
		},
	)

	// goal_approve: unpause a paused goal (e.g. after the owner reviews a
	// milestone). Semantically "resume the scheduler's tick on this goal".
	r.Register("goal_approve", "",
		json.RawMessage(`{
			"type":"object",
			"properties":{"id":{"type":"string","description":"Goal UUID (or short prefix)"}},
			"required":["id"]
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct{ ID string `json:"id"` }
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			// For now, approve = update status from paused back to pending.
			// Supports full UUID. Short-prefix resolution can be added later.
			id, err := uuid.Parse(p.ID)
			if err != nil {
				return nil, fmt.Errorf("invalid goal id: %w", err)
			}
			_, err = db.ExecContext(ctx,
				`UPDATE goals SET status='pending' WHERE id=$1 AND status='paused'`, id)
			if err != nil {
				return nil, fmt.Errorf("approve goal: %w", err)
			}
			return map[string]string{"id": p.ID, "status": "pending"}, nil
		},
	)

	return nil
}
