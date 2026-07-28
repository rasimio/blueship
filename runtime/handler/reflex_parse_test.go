package handler

import "testing"

func TestParseReflexResultIgnoresLegacyPostActions(t *testing.T) {
	result, err := parseReflexResult(`{
		"intent":"free_reflection",
		"confidence":0.95,
		"pre_actions":[],
		"post_actions":[
			{"type":"save_reflection","content":"must never execute"}
		],
		"tools":[]
	}`)
	if err != nil {
		t.Fatalf("parseReflexResult: %v", err)
	}
	if result.Intent != "free_reflection" || result.Confidence != 0.95 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.PreActions) != 0 || len(result.Tools) != 0 {
		t.Fatalf("legacy post_actions affected executable output: %+v", result)
	}
}
