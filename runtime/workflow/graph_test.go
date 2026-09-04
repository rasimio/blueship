package workflow

import "testing"

func TestGraphRejectsCyclesAndUnrelatedRetries(t *testing.T) {
	for _, nodes := range [][]Node{
		{{ID: "a", DependsOn: []string{"b"}, MaxAttempts: 2}, {ID: "b", DependsOn: []string{"a"}, MaxAttempts: 2}},
		{{ID: "a", RetryFrom: "b", MaxAttempts: 2}, {ID: "b", MaxAttempts: 2}},
		{{ID: "a", DependsOn: []string{"missing"}, MaxAttempts: 2}},
	} {
		if Validate(nodes) == nil {
			t.Fatal("accepted invalid graph")
		}
	}
	nodes := []Node{{ID: "code", MaxAttempts: 3}, {ID: "review", DependsOn: []string{"code"}, RetryFrom: "code", MaxAttempts: 3}, {ID: "test", DependsOn: []string{"review"}, MaxAttempts: 3}, {ID: "independent", MaxAttempts: 1}}
	if err := Validate(nodes); err != nil {
		t.Fatal(err)
	}
	reset := Descendants(nodes, "code")
	if len(reset) != 3 || reset["independent"] {
		t.Fatal(reset)
	}
}
