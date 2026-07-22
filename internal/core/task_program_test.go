package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func validTaskProgram() TaskProgram {
	return TaskProgram{
		Schema: TaskProgramSchemaV1,
		Inputs: []TaskProgramInput{{
			ID:      "notes_due",
			Tool:    "notes_list",
			Input:   json.RawMessage(`{"status":"due"}`),
			Enabled: true,
			OnError: TaskProgramOnErrorFail,
		}},
		Activation: TaskProgramActivation{
			Mode:     TaskProgramActivationAnyNonEmpty,
			InputIDs: []string{"notes_due"},
		},
		Decision: TaskProgramDecision{
			Mode:  TaskProgramDecisionSelected,
			Tools: []string{"safe_lookup"},
		},
	}
}

func TestParseTaskProgramV1(t *testing.T) {
	config := json.RawMessage(`{
		"prompt":"heartbeat",
		"task_program":{
			"schema":"blueship.task-program/v1",
			"inputs":[{"id":"notes_due","tool":"notes_list","input":{"status":"due"},"enabled":true,"on_error":"fail"}],
			"activation":{"mode":"any_non_empty","input_ids":["notes_due"]},
			"decision":{"mode":"selected","tools":["safe_lookup"],"instruction":"Choose only genuinely useful reminders."},
			"quiet_hours":{"from_hour":23,"to_hour":7}
		}
	}`)
	program, present, err := ParseTaskProgram(config)
	if err != nil {
		t.Fatalf("ParseTaskProgram: %v", err)
	}
	if !present || program == nil {
		t.Fatal("task_program should be present")
	}
	if got := program.RequestedTools(); len(got) != 2 || got[0] != "notes_list" || got[1] != "safe_lookup" {
		t.Fatalf("RequestedTools() = %v", got)
	}
	if program.Decision.Instruction != "Choose only genuinely useful reminders." {
		t.Fatalf("decision.instruction = %q", program.Decision.Instruction)
	}

	if program, present, err := ParseTaskProgram(json.RawMessage(`{"prompt":"heartbeat"}`)); err != nil || present || program != nil {
		t.Fatalf("legacy config parsed as program: program=%v present=%v err=%v", program, present, err)
	}
}

func TestTaskProgramValidationRejectsDuplicateBlockID(t *testing.T) {
	program := validTaskProgram()
	program.Inputs = append(program.Inputs, TaskProgramInput{
		ID:      "notes_due",
		Tool:    "calendar_list",
		Input:   json.RawMessage(`{}`),
		Enabled: true,
		OnError: TaskProgramOnErrorContinue,
	})
	if err := program.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate input id") {
		t.Fatalf("Validate() error = %v, want duplicate input id", err)
	}
}

func TestTaskProgramAllowsRepeatedToolWithDistinctBlockIDs(t *testing.T) {
	program := validTaskProgram()
	program.Inputs = []TaskProgramInput{
		{ID: "repo_one", Tool: "git_status", Input: json.RawMessage(`{"repo":"one"}`), Enabled: true, OnError: TaskProgramOnErrorFail},
		{ID: "repo_two", Tool: "git_status", Input: json.RawMessage(`{"repo":"two"}`), Enabled: true, OnError: TaskProgramOnErrorContinue},
	}
	program.Activation.InputIDs = []string{"repo_one", "repo_two"}
	if err := program.Validate(); err != nil {
		t.Fatalf("same tool with distinct block ids must be valid: %v", err)
	}
	requested := program.RequestedTools()
	if len(requested) != 2 || requested[0] != "git_status" || requested[1] != "safe_lookup" {
		t.Fatalf("RequestedTools() = %v, want deduplicated registry names", requested)
	}
}

func TestTaskProgramMCPIsDecisionOnly(t *testing.T) {
	program := validTaskProgram()
	program.Inputs[0].Tool = "mcp__github__list_issues"
	if err := program.Validate(); err == nil || !strings.Contains(err.Error(), "decision-only") {
		t.Fatalf("MCP deterministic input error = %v", err)
	}

	program = TaskProgram{
		Schema:     TaskProgramSchemaV1,
		Activation: TaskProgramActivation{Mode: TaskProgramActivationAlways},
		Decision: TaskProgramDecision{
			Mode:  TaskProgramDecisionSelected,
			Tools: []string{"mcp__github__list_issues"},
		},
	}
	if err := program.Validate(); err != nil {
		t.Fatalf("exact MCP decision tool must be valid: %v", err)
	}
}

func TestTaskProgramRejectsDeterministicMessageSend(t *testing.T) {
	program := validTaskProgram()
	program.Inputs[0].Tool = "message_send"
	if err := program.Validate(); err == nil || !strings.Contains(err.Error(), "scheduler-owned") {
		t.Fatalf("deterministic message_send error = %v", err)
	}
}

func TestTaskProgramInputMustBeJSONObject(t *testing.T) {
	for _, raw := range []string{`[]`, `null`, `"text"`, `42`, `true`} {
		t.Run(raw, func(t *testing.T) {
			program := validTaskProgram()
			program.Inputs[0].Input = json.RawMessage(raw)
			if err := program.Validate(); err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
				t.Fatalf("Validate(%s) error = %v", raw, err)
			}
		})
	}

	program := validTaskProgram()
	program.Inputs[0].Input = nil
	if err := program.Validate(); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("omitted input error = %v, want required JSON object", err)
	}
}

func TestTaskProgramContractBounds(t *testing.T) {
	t.Run("inputs", func(t *testing.T) {
		program := validTaskProgram()
		program.Inputs = make([]TaskProgramInput, TaskProgramMaxInputs+1)
		for i := range program.Inputs {
			program.Inputs[i] = TaskProgramInput{
				ID:      fmt.Sprintf("input_%d", i),
				Tool:    "notes_list",
				Input:   json.RawMessage(`{}`),
				Enabled: true,
				OnError: TaskProgramOnErrorFail,
			}
		}
		program.Activation = TaskProgramActivation{Mode: TaskProgramActivationAlways}
		if err := program.Validate(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("maximum of %d", TaskProgramMaxInputs)) {
			t.Fatalf("Validate() error = %v, want input bound", err)
		}
	})

	t.Run("decision tools", func(t *testing.T) {
		program := validTaskProgram()
		program.Decision.Tools = make([]string, TaskProgramMaxDecisionTools+1)
		for i := range program.Decision.Tools {
			program.Decision.Tools[i] = fmt.Sprintf("safe_tool_%d", i)
		}
		if err := program.Validate(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("maximum of %d", TaskProgramMaxDecisionTools)) {
			t.Fatalf("Validate() error = %v, want decision tool bound", err)
		}
	})

	t.Run("input bytes", func(t *testing.T) {
		program := validTaskProgram()
		program.Inputs[0].Input = json.RawMessage(strings.Repeat(" ", TaskProgramMaxInputBytes) + `{}`)
		if err := program.Validate(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("maximum of %d bytes", TaskProgramMaxInputBytes)) {
			t.Fatalf("Validate() error = %v, want input byte bound", err)
		}
	})

	t.Run("decision instruction bytes", func(t *testing.T) {
		program := validTaskProgram()
		program.Decision.Instruction = strings.Repeat("я", TaskProgramMaxDecisionInstructionBytes/2)
		if err := program.Validate(); err != nil {
			t.Fatalf("exact UTF-8 byte limit rejected: %v", err)
		}
		program.Decision.Instruction += "я"
		if err := program.Validate(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("maximum of %d bytes", TaskProgramMaxDecisionInstructionBytes)) {
			t.Fatalf("Validate() error = %v, want instruction byte bound", err)
		}
	})
}

func TestTaskProgramDecisionInstructionValidation(t *testing.T) {
	program := validTaskProgram()
	program.Decision.Instruction = string([]byte{0xff})
	if err := program.Validate(); err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}

	program = validTaskProgram()
	program.Decision.Mode = TaskProgramDecisionNone
	program.Decision.Tools = nil
	program.Decision.Instruction = "unused instruction"
	if err := program.Validate(); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("none-mode instruction error = %v", err)
	}
}

func TestTaskProgramValidationModesAndReferences(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TaskProgram)
		want   string
	}{
		{"schema", func(p *TaskProgram) { p.Schema = "v0" }, "schema must be"},
		{"input id format", func(p *TaskProgram) { p.Inputs[0].ID = "notes due" }, "must match"},
		{"input id leading punctuation", func(p *TaskProgram) { p.Inputs[0].ID = "_notes" }, "must match"},
		{"input tool whitespace", func(p *TaskProgram) { p.Inputs[0].Tool = " notes_list" }, "surrounding whitespace"},
		{"on error", func(p *TaskProgram) { p.Inputs[0].OnError = "ignore" }, "on_error"},
		{"activation mode", func(p *TaskProgram) { p.Activation.Mode = "some" }, "activation.mode"},
		{"activation id whitespace", func(p *TaskProgram) { p.Activation.InputIDs = []string{"notes_due "} }, "surrounding whitespace"},
		{"unknown activation input", func(p *TaskProgram) { p.Activation.InputIDs = []string{"missing"} }, "unknown input"},
		{"disabled activation input", func(p *TaskProgram) { p.Inputs[0].Enabled = false }, "disabled input"},
		{"decision mode", func(p *TaskProgram) { p.Decision.Mode = "agent" }, "decision.mode"},
		{"decision tool whitespace", func(p *TaskProgram) { p.Decision.Tools = []string{" message_send"} }, "surrounding whitespace"},
		{"scheduler-owned notification", func(p *TaskProgram) { p.Decision.Tools = []string{"message_send"} }, "scheduler-owned"},
		{"none with tools", func(p *TaskProgram) { p.Decision.Mode = TaskProgramDecisionNone }, "must be empty"},
		{"equal quiet hours", func(p *TaskProgram) { p.QuietHours = &TaskProgramQuietHours{FromHour: 2, ToHour: 2} }, "must differ"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			program := validTaskProgram()
			test.mutate(&program)
			if err := program.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}
