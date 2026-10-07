package agenttask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

type graphResultRepairer interface {
	RepairGraphResult(context.Context, core.AgentTask, []core.TaskStep, core.AgentDeps, string, string) (string, error)
}

type graphFinalValidation struct {
	RepairPending   bool               `json:"repair_pending,omitempty"`
	RepairAttempts  int                `json:"repair_attempts,omitempty"`
	RepairBody      string             `json:"repair_body,omitempty"`
	Verdict         *AcceptanceVerdict `json:"verdict,omitempty"`
	CandidateHash   string             `json:"candidate_hash"`
	Attempts        int                `json:"attempts"`
	ResponseChecked bool               `json:"response_checked"`
	Accepted        bool               `json:"accepted"`
	SafeBody        string             `json:"safe_body"`
	// Retained while the next repaired candidate is awaiting its output guard.
	// This is a checked draft, never a completed acceptance verdict.
	LastCheckedBody string `json:"last_checked_body,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

func (v graphFinalValidation) checkedBody() string {
	if v.ResponseChecked && strings.TrimSpace(v.SafeBody) != "" {
		return v.SafeBody
	}
	return v.LastCheckedBody
}

// validateGraphResult validates the final artifact against the original task,
// with evidence from all confirmed branches. Persisted validation progress
// allows an unavailable reviewer to retry without executing any graph step.
func (s *Scheduler) validateGraphResult(ctx context.Context, task core.AgentTask, steps []core.TaskStep, deps core.AgentDeps, body string, handler GraphHandler) error {
	stop := core.TaskStopDeadline(task)
	current, err := s.store.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	var progress struct {
		Final graphFinalValidation `json:"final_validation"`
	}
	if err := json.Unmarshal(current.Progress, &progress); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(digest[:])
	state := progress.Final
	if state.CandidateHash != hash {
		state = graphFinalValidation{CandidateHash: hash}
	}
	if state.Accepted {
		return s.finishGraph(ctx, task, "completed", state.SafeBody, "")
	}
	save := func() error {
		raw, err := json.Marshal(map[string]any{"phase": "finalizing", "final_validation": state})
		if err != nil {
			return err
		}
		return s.store.SaveGraphProgress(ctx, task.ID, raw)
	}
	originalBody := body
	if state.RepairBody != "" {
		body = state.RepairBody
	}
	if state.RepairPending {
		repairer, ok := handler.(graphResultRepairer)
		// Unsupported optional wording can be removed or corrected from existing
		// evidence. Missing required facts still fail the original acceptance
		// criteria: every repaired candidate must pass both gates again.
		grounded := state.Verdict == nil || graphGroundingRepairable(state.Verdict.Grounding)
		enoughTime := stop == nil || time.Until(*stop) > 30*time.Second
		if !ok || !grounded || !enoughTime || state.RepairAttempts >= 2 {
			return s.finishGraph(ctx, task, "partial", state.checkedBody(), "final_acceptance_rejected: "+state.Reason)
		}
		state.RepairAttempts++
		if err := save(); err != nil {
			return err
		}
		repairCtx, end := context.WithTimeout(ctx, time.Minute)
		revised, repairErr := repairer.RepairGraphResult(repairCtx, task, steps, deps, state.SafeBody, state.Reason)
		end()
		if repairErr != nil || strings.TrimSpace(revised) == "" {
			if state.RepairAttempts >= 2 {
				return s.finishGraph(ctx, task, "partial", state.checkedBody(), "final_repair_unavailable")
			}
			_, delay := core.TaskRetryPolicy(repairErr)
			next := time.Now().Add(max(5*time.Second, delay))
			if stop != nil && !next.Before(*stop) {
				return s.finishGraph(ctx, task, "partial", state.checkedBody(), "final_repair_unavailable")
			}
			return s.store.YieldGraphTask(ctx, task.ID, next)
		}
		state.RepairBody = revised
		// Attempts belongs to this exact candidate. A previous reviewer outage
		// must not exhaust review of a newly repaired report. RepairAttempts and
		// the task deadline still bound the overall repair cycle.
		state.Attempts = 0
		state.RepairPending = false
		state.LastCheckedBody = state.checkedBody()
		state.ResponseChecked = false
		state.SafeBody = ""
		state.Verdict = nil
		if err := save(); err != nil {
			return err
		}
		body = revised
	}

	if state.Attempts >= 3 {
		return s.finishGraph(ctx, task, "partial", state.checkedBody(), "final_verification_unavailable")
	}
	state.Attempts++
	if err := save(); err != nil {
		return err
	}
	var receipts []core.ToolExecutionResult
	var traces []json.RawMessage
	for _, step := range steps {
		if step.Status != "done" {
			continue
		}
		var cp struct {
			Receipts []core.ToolExecutionResult `json:"receipts"`
			Traces   []json.RawMessage          `json:"traces"`
		}
		if len(step.Checkpoint) > 0 {
			if err := json.Unmarshal(step.Checkpoint, &cp); err != nil {
				return err
			}
		}
		receipts = append(receipts, cp.Receipts...)
		traces = append(traces, cp.Traces...)
	}
	deferCheck := func(reason string, delay time.Duration) error {
		state.Reason = reason
		if err := save(); err != nil {
			return err
		}
		next := time.Now().Add(max(time.Duration(state.Attempts)*5*time.Second, delay))
		if state.Attempts >= 3 || (stop != nil && !next.Before(*stop)) {
			return s.finishGraph(ctx, task, "partial", state.checkedBody(), "final_verification_unavailable")
		}
		return s.store.YieldGraphTask(ctx, task.ID, next)
	}
	if !state.ResponseChecked {
		safe := body
		if validate := s.deps.Config.ResponseValidator; validate != nil {
			userText := task.Title
			if task.Description != nil {
				userText += "\n" + *task.Description
			}
			validationCtx, end := context.WithTimeout(ctx, time.Minute)
			safe, err = validate(validationCtx, core.ResponseValidationRequest{CurrentDatetime: time.Now().UTC().Format(time.RFC3339), Timezone: s.deps.Config.Timezone, Text: body, UserText: userText, Tools: receipts})
			end()
			if err != nil {
				_, delay := core.TaskRetryPolicy(err)
				return deferCheck("response_validation_unavailable", delay)
			}
			if strings.TrimSpace(safe) == "" {
				return deferCheck("response_validation_empty", 0)
			}
		}
		state.SafeBody = safe
		state.LastCheckedBody = safe
		state.ResponseChecked = true
		if err := save(); err != nil {
			return err
		}
	}
	rawTraces, err := json.Marshal(traces)
	if err != nil {
		return err
	}
	reviewCtx, end := context.WithTimeout(ctx, 2*time.Minute)
	verdict := evaluateAcceptanceWithPrior(reviewCtx, deps, task, state.SafeBody, rawTraces, state.Verdict)
	end()
	state.Verdict = &verdict
	if verdict.Unavailable {
		return deferCheck(verdict.Reason, verdict.RetryDelay)
	}
	if !verdict.Met {
		state.Reason = verdict.Reason
		state.RepairPending = true
		if err := save(); err != nil {
			return err
		}
		return s.validateGraphResult(ctx, task, steps, deps, originalBody, handler)
	}
	state.Accepted = true
	state.Reason = ""
	if err := save(); err != nil {
		return err
	}
	return s.finishGraph(ctx, task, "completed", state.SafeBody, "")
}
