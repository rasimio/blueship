package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// TaskProgramSchemaV1 is the first typed execution contract stored under
// AgentTask.Config.task_program. The surrounding AgentTask config remains an
// open host-owned object; only this nested value is interpreted by BlueShip.
const TaskProgramSchemaV1 = "blueship.task-program/v1"

const (
	TaskProgramMaxInputs                   = 32
	TaskProgramMaxDecisionTools            = 32
	TaskProgramMaxInputBytes               = 64 << 10
	TaskProgramMaxDecisionInstructionBytes = 4000

	TaskProgramOnErrorFail     = "fail"
	TaskProgramOnErrorContinue = "continue"

	TaskProgramActivationAlways      = "always"
	TaskProgramActivationAnyNonEmpty = "any_non_empty"
	TaskProgramActivationAllNonEmpty = "all_non_empty"

	TaskProgramDecisionSelected = "selected"
	TaskProgramDecisionNone     = "none"
)

var taskProgramInputIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// TaskProgram is a deliberately small, linear background-task program.
// Inputs are deterministic tool calls; activation gates the LLM decision; the
// decision lists the exact tools the LLM may use. It is not a DAG or workflow
// state machine.
type TaskProgram struct {
	Schema     string                 `json:"schema"`
	Inputs     []TaskProgramInput     `json:"inputs"`
	Activation TaskProgramActivation  `json:"activation"`
	Decision   TaskProgramDecision    `json:"decision"`
	QuietHours *TaskProgramQuietHours `json:"quiet_hours,omitempty"`
}

type TaskProgramInput struct {
	ID      string          `json:"id"`
	Tool    string          `json:"tool"`
	Input   json.RawMessage `json:"input"`
	Enabled bool            `json:"enabled"`
	OnError string          `json:"on_error"`
}

type TaskProgramActivation struct {
	Mode     string   `json:"mode"`
	InputIDs []string `json:"input_ids"`
}

type TaskProgramDecision struct {
	Mode        string   `json:"mode"`
	Tools       []string `json:"tools"`
	Instruction string   `json:"instruction,omitempty"`
}

type TaskProgramQuietHours struct {
	FromHour int `json:"from_hour"`
	ToHour   int `json:"to_hour"`
}

// ParseTaskProgram reads and validates Config.task_program. present is false
// for legacy configs and for an explicit null value, which lets runtimes
// dual-read without changing existing task behaviour.
func ParseTaskProgram(config json.RawMessage) (program *TaskProgram, present bool, err error) {
	if len(config) == 0 {
		return nil, false, nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(config, &envelope); err != nil {
		// AgentTask.Config is normally jsonb and therefore valid. Preserve the
		// legacy handler's tolerant behaviour for malformed host-owned config
		// that does not give us a readable task_program value.
		return nil, false, nil
	}
	raw, ok := envelope["task_program"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}

	var p TaskProgram
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, true, fmt.Errorf("decode: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, true, err
	}
	return &p, true, nil
}

// Validate rejects ambiguous programs before any deterministic input can
// cause a side effect.
func (p TaskProgram) Validate() error {
	if p.Schema != TaskProgramSchemaV1 {
		return fmt.Errorf("schema must be %q", TaskProgramSchemaV1)
	}
	if len(p.Inputs) > TaskProgramMaxInputs {
		return fmt.Errorf("inputs exceeds maximum of %d", TaskProgramMaxInputs)
	}

	inputIDs := make(map[string]bool, len(p.Inputs))
	for i, input := range p.Inputs {
		id := input.ID
		tool := input.Tool
		if !taskProgramInputIDPattern.MatchString(id) {
			return fmt.Errorf("inputs[%d].id must match %s", i, taskProgramInputIDPattern.String())
		}
		if tool == "" {
			return fmt.Errorf("inputs[%d].tool is required", i)
		}
		if tool != strings.TrimSpace(tool) {
			return fmt.Errorf("input %q tool must not contain surrounding whitespace", id)
		}
		if _, exists := inputIDs[id]; exists {
			return fmt.Errorf("duplicate input id %q", id)
		}
		inputIDs[id] = input.Enabled
		// User-facing delivery is scheduler-owned so the delivery ledger can
		// be advanced only after a successful transport send. Executing
		// message_send as a deterministic input would bypass that boundary and
		// repeat the side effect on every recurrence.
		if tool == "message_send" {
			return fmt.Errorf("input %q uses scheduler-owned notification tool %q", id, tool)
		}
		// MCP tools may be safe for an LLM to select, but deterministic
		// execution needs an effect classification/approval model that v1 does
		// not have. Keep them decision-only until that contract exists.
		if strings.HasPrefix(tool, "mcp__") {
			return fmt.Errorf("input %q uses MCP tool %q; MCP tools are decision-only in task-program/v1", id, tool)
		}
		if len(input.Input) == 0 {
			return fmt.Errorf("input %q is required and must be a JSON object", id)
		}
		if len(input.Input) > TaskProgramMaxInputBytes {
			return fmt.Errorf("input %q exceeds maximum of %d bytes", id, TaskProgramMaxInputBytes)
		}
		if !json.Valid(input.Input) {
			return fmt.Errorf("input %q has invalid JSON", id)
		}
		if trimmed := bytes.TrimSpace(input.Input); len(trimmed) == 0 || trimmed[0] != '{' {
			return fmt.Errorf("input %q must be a JSON object", id)
		}
		switch input.OnError {
		case TaskProgramOnErrorFail, TaskProgramOnErrorContinue:
		default:
			return fmt.Errorf("input %q on_error must be %q or %q", id, TaskProgramOnErrorFail, TaskProgramOnErrorContinue)
		}
	}

	switch p.Activation.Mode {
	case TaskProgramActivationAlways:
		if len(p.Activation.InputIDs) != 0 {
			return fmt.Errorf("activation.input_ids must be empty for mode %q", TaskProgramActivationAlways)
		}
	case TaskProgramActivationAnyNonEmpty, TaskProgramActivationAllNonEmpty:
		if len(p.Activation.InputIDs) == 0 {
			return fmt.Errorf("activation.input_ids is required for mode %q", p.Activation.Mode)
		}
	default:
		return fmt.Errorf("activation.mode must be %q, %q, or %q", TaskProgramActivationAlways, TaskProgramActivationAnyNonEmpty, TaskProgramActivationAllNonEmpty)
	}

	activationIDs := make(map[string]struct{}, len(p.Activation.InputIDs))
	for _, id := range p.Activation.InputIDs {
		if id == "" {
			return fmt.Errorf("activation.input_ids contains an empty id")
		}
		if id != strings.TrimSpace(id) {
			return fmt.Errorf("activation.input_ids value %q must not contain surrounding whitespace", id)
		}
		if _, exists := activationIDs[id]; exists {
			return fmt.Errorf("activation.input_ids contains duplicate id %q", id)
		}
		activationIDs[id] = struct{}{}
		enabled, exists := inputIDs[id]
		if !exists {
			return fmt.Errorf("activation.input_ids references unknown input %q", id)
		}
		if !enabled {
			return fmt.Errorf("activation.input_ids references disabled input %q", id)
		}
	}

	if len(p.Decision.Tools) > TaskProgramMaxDecisionTools {
		return fmt.Errorf("decision.tools exceeds maximum of %d", TaskProgramMaxDecisionTools)
	}
	if !utf8.ValidString(p.Decision.Instruction) {
		return fmt.Errorf("decision.instruction must be valid UTF-8")
	}
	if len(p.Decision.Instruction) > TaskProgramMaxDecisionInstructionBytes {
		return fmt.Errorf("decision.instruction exceeds maximum of %d bytes", TaskProgramMaxDecisionInstructionBytes)
	}
	switch p.Decision.Mode {
	case TaskProgramDecisionSelected:
	case TaskProgramDecisionNone:
		if len(p.Decision.Tools) != 0 {
			return fmt.Errorf("decision.tools must be empty for mode %q", TaskProgramDecisionNone)
		}
		if p.Decision.Instruction != "" {
			return fmt.Errorf("decision.instruction must be empty for mode %q", TaskProgramDecisionNone)
		}
	default:
		return fmt.Errorf("decision.mode must be %q or %q", TaskProgramDecisionSelected, TaskProgramDecisionNone)
	}
	decisionTools := make(map[string]struct{}, len(p.Decision.Tools))
	for i, tool := range p.Decision.Tools {
		if tool == "" {
			return fmt.Errorf("decision.tools[%d] is empty", i)
		}
		if tool != strings.TrimSpace(tool) {
			return fmt.Errorf("decision.tools[%d] must not contain surrounding whitespace", i)
		}
		if _, exists := decisionTools[tool]; exists {
			return fmt.Errorf("decision.tools contains duplicate tool %q", tool)
		}
		// Task-program notifications are delivered by the scheduler so their
		// delivery refs can be acknowledged atomically after the send. Letting
		// the decision agent call message_send bypasses that boundary and makes
		// keyed items repeat forever on the next tick.
		if tool == "message_send" {
			return fmt.Errorf("decision tool %q is not allowed in task-program/v1; notifications are scheduler-owned", tool)
		}
		decisionTools[tool] = struct{}{}
	}

	if p.QuietHours != nil {
		if p.QuietHours.FromHour < 0 || p.QuietHours.FromHour > 23 {
			return fmt.Errorf("quiet_hours.from_hour must be between 0 and 23")
		}
		if p.QuietHours.ToHour < 0 || p.QuietHours.ToHour > 23 {
			return fmt.Errorf("quiet_hours.to_hour must be between 0 and 23")
		}
		if p.QuietHours.FromHour == p.QuietHours.ToHour {
			return fmt.Errorf("quiet_hours.from_hour and to_hour must differ")
		}
	}

	return nil
}

// RequestedTools returns the exact registry names the program can execute:
// enabled deterministic inputs followed by selected LLM tools, de-duplicated
// while preserving declaration order.
func (p TaskProgram) RequestedTools() []string {
	seen := make(map[string]struct{}, len(p.Inputs)+len(p.Decision.Tools))
	out := make([]string, 0, len(p.Inputs)+len(p.Decision.Tools))
	appendTool := func(raw string) {
		tool := raw
		if tool == "" {
			return
		}
		if _, exists := seen[tool]; exists {
			return
		}
		seen[tool] = struct{}{}
		out = append(out, tool)
	}
	for _, input := range p.Inputs {
		if input.Enabled {
			appendTool(input.Tool)
		}
	}
	if p.Decision.Mode == TaskProgramDecisionSelected {
		for _, tool := range p.Decision.Tools {
			appendTool(tool)
		}
	}
	return out
}
