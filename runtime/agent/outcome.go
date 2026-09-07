package agent

import (
	"context"
	"errors"
)

// RunStopReason describes why one bounded model/tool run stopped. Completed
// means the model ended this run normally; it is not proof that an external
// task, goal, or deployment has satisfied its acceptance criteria.
type RunStopReason string

const (
	RunStopCompleted    RunStopReason = "completed"
	RunStopToolBudget   RunStopReason = "tool_budget"
	RunStopTurnLimit    RunStopReason = "turn_limit"
	RunStopOutputLimit  RunStopReason = "output_limit"
	RunStopRefusal      RunStopReason = "refusal"
	RunStopEmptyOutput  RunStopReason = "empty_output"
	RunStopError        RunStopReason = "error"
	RunStopCancelled    RunStopReason = "cancelled"
	RunStopProviderStop RunStopReason = "provider_stop"
)

// RunOutcome is independent of generated prose. Hosts can checkpoint and
// schedule continuation without interpreting a model's localized answer.
type RunOutcome struct {
	Reason             RunStopReason `json:"reason"`
	Turns              int           `json:"turns"`
	ToolTurns          int           `json:"tool_turns"`
	ProviderStopReason string        `json:"provider_stop_reason,omitempty"`
}

func finishRunOutcome(ctx context.Context, cfg RunConfig, outcome RunOutcome, err error) RunOutcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		outcome.Reason = RunStopCancelled
	} else if err != nil && outcome.Reason != RunStopTurnLimit {
		outcome.Reason = RunStopError
	}
	if cfg.OnOutcome != nil {
		cfg.OnOutcome(outcome)
	}
	return outcome
}

func terminalRunReason(outcome RunOutcome, stop string) RunStopReason {
	if outcome.Reason == RunStopEmptyOutput {
		return RunStopEmptyOutput
	}
	if stop == "max_tokens" {
		return RunStopOutputLimit
	}
	if outcome.Reason == RunStopToolBudget {
		return RunStopToolBudget
	}
	return RunStopCompleted
}
