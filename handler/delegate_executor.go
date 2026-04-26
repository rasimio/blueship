// Package handler — DelegateStrategyExecutor.
//
// Runs the strategy=delegate path on the originating agent. The peer is
// resolved via the agent's federated tool registry (Fleet-driven), so
// this handler does not import Fleet directly. It uses the registry to
// invoke `agent_task_accept` on the peer (round-trip 1) and
// `agent_task_status` on subsequent ticks (round-trip 2..N) until the
// peer's task reaches a terminal state.
//
// V1 uses polling, not callbacks. Each scheduler tick (~60s) does one
// status check; long-running peer tasks accumulate dozens of polls.
// Acceptable for the first cut. A callback-driven path can replace
// polling later by storing peer_task_id in progress and waking via the
// existing PausedByPeerTask mechanism in agenttask.Scheduler.
package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rasimio/blueship/core"
)

// DelegateStrategyExecutor implements core.AgentHandler for the
// "delegate" strategy.
type DelegateStrategyExecutor struct{}

// NewDelegateStrategyExecutor constructs the executor. Stateless.
func NewDelegateStrategyExecutor() *DelegateStrategyExecutor {
	return &DelegateStrategyExecutor{}
}

// DefaultTools is empty — delegate doesn't need a fixed allowlist; it
// invokes federated tools via the registry's RemoteTool handlers.
func (e *DelegateStrategyExecutor) DefaultTools() []string { return nil }

// delegateProgress is what we persist between iterations.
type delegateProgress struct {
	PeerTaskID string `json:"peer_task_id,omitempty"`
	LastStatus string `json:"last_peer_status,omitempty"`
	Phase      string `json:"phase,omitempty"` // "submitted" | "polling"
	PeerError  string `json:"peer_error,omitempty"`
}

// Run dispatches one iteration. First iteration submits the spec to the
// peer; subsequent iterations poll its status. Returns Done=true once
// the peer task is in a terminal state.
func (e *DelegateStrategyExecutor) Run(ctx context.Context, task core.AgentTask, deps core.AgentDeps) (core.IterationResult, error) {
	if task.DelegateTo == nil || *task.DelegateTo == "" {
		return core.IterationResult{}, fmt.Errorf("delegate executor: task has no delegate_to (peer agent_id)")
	}

	var prog delegateProgress
	if len(task.Progress) > 0 && string(task.Progress) != "{}" {
		_ = json.Unmarshal(task.Progress, &prog)
	}

	if prog.PeerTaskID == "" {
		// First iteration: submit the spec.
		return e.submit(ctx, task, deps, &prog)
	}
	// Subsequent iteration: poll.
	return e.poll(ctx, task, deps, &prog)
}

// submit calls agent_task_accept on the peer. The federated tool is
// registered in the local registry by the Fleet bootstrap; we look it
// up by name and invoke through it like any other tool.
func (e *DelegateStrategyExecutor) submit(ctx context.Context, task core.AgentTask, deps core.AgentDeps, prog *delegateProgress) (core.IterationResult, error) {
	handler, ok := deps.Registry.HandlerByName("agent_task_accept")
	if !ok {
		return core.IterationResult{}, fmt.Errorf("delegate: federated tool agent_task_accept not in registry — peer %s has not exposed it via Fleet", *task.DelegateTo)
	}

	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}
	criteria := ""
	if task.AcceptanceCriteria != nil {
		criteria = *task.AcceptanceCriteria
	}

	plan := task.Plan
	if len(plan) == 0 {
		plan = json.RawMessage(`{}`)
	}

	payload, _ := json.Marshal(map[string]any{
		"title":               task.Title,
		"description":         desc,
		"acceptance_criteria": criteria,
		"strategy":            core.StrategyDirect, // peer always runs as direct unless caller overrides
		"plan":                plan,
		"tools":               []string(task.Tools),
		"use_agents":          []string(task.UseAgents),
		"max_iterations":      task.MaxIterations,
		"origin_task_id":      task.ID.String(),
	})

	out, err := handler(ctx, payload)
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("delegate: agent_task_accept failed: %w", err)
	}

	peerTaskID, _ := extractStringField(out, "id")
	if peerTaskID == "" {
		return core.IterationResult{}, fmt.Errorf("delegate: peer returned no task id (got %v)", out)
	}
	prog.PeerTaskID = peerTaskID
	prog.Phase = "polling"
	progress, _ := json.Marshal(prog)

	deps.Logger.Info("delegate: peer accepted task", "peer", *task.DelegateTo, "peer_task_id", peerTaskID)

	return core.IterationResult{
		Done:     false,
		Progress: progress,
		Notify:   "[no-op]",
	}, nil
}

// poll calls agent_task_status on the peer, mirrors state into progress,
// and reports done when the peer reaches a terminal status.
func (e *DelegateStrategyExecutor) poll(ctx context.Context, task core.AgentTask, deps core.AgentDeps, prog *delegateProgress) (core.IterationResult, error) {
	handler, ok := deps.Registry.HandlerByName("agent_task_status")
	if !ok {
		return core.IterationResult{}, fmt.Errorf("delegate: federated tool agent_task_status not in registry")
	}

	payload, _ := json.Marshal(map[string]string{"id": prog.PeerTaskID})
	out, err := handler(ctx, payload)
	if err != nil {
		// Transient — keep polling on next tick.
		deps.Logger.Warn("delegate: status poll failed", "peer_task_id", prog.PeerTaskID, "error", err)
		progress, _ := json.Marshal(prog)
		return core.IterationResult{Done: false, Progress: progress, Notify: "[no-op]"}, nil
	}

	status, _ := extractStringField(out, "status")
	prog.LastStatus = status

	terminal := status == "done" || status == "failed" || status == "canceled"
	if !terminal {
		progress, _ := json.Marshal(prog)
		return core.IterationResult{Done: false, Progress: progress, Notify: "[no-op]"}, nil
	}

	// Terminal — surface peer's result/error to origin task.
	result, _ := extractStringField(out, "result")
	if status == "failed" {
		errMsg, _ := extractStringField(out, "error_message")
		prog.PeerError = errMsg
	}
	progress, _ := json.Marshal(prog)

	deps.Logger.Info("delegate: peer task terminal",
		"peer", *task.DelegateTo, "peer_task_id", prog.PeerTaskID, "status", status)

	return core.IterationResult{
		Done:     true,
		Output:   result,
		Progress: progress,
	}, nil
}

// extractStringField pulls a top-level string field out of an arbitrary
// tool result. Tool handlers return `any`; the federated remote-tool
// adapter unmarshals JSON output into `map[string]any` for sync tools.
func extractStringField(v any, key string) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	switch x := m[key].(type) {
	case string:
		return x, true
	case nil:
		return "", false
	default:
		return fmt.Sprintf("%v", x), true
	}
}
