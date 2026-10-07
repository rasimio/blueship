package core

import "testing"

func TestTaskStepsRejectInvalidDependencyGraphs(t *testing.T) {
	step := func(id string, deps ...string) TaskStep {
		return TaskStep{ID: id, Goal: "Read source", Acceptance: "Source fetched", Kind: "read", Dependencies: deps}
	}
	for name, steps := range map[string][]TaskStep{"cycle": {step("a", "b"), step("b", "a")}, "missing": {step("a", "missing")}, "self": {step("a", "a")}, "duplicate_id": {step("a"), step("a")}, "duplicate_edge": {step("a"), step("b", "a", "a")}, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			if ValidateTaskSteps(steps) == nil {
				t.Fatal("accepted invalid graph")
			}
		})
	}
	if err := ValidateTaskSteps([]TaskStep{step("root"), step("a", "root"), step("b", "root"), step("report", "a", "b")}); err != nil {
		t.Fatal(err)
	}
}
