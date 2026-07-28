package agent

import "testing"

func TestPromptAnatomyLogAttrsIncludePromptHashesAndSession(t *testing.T) {
	anatomy := newPromptAnatomy(
		nil,
		"raw system",
		"",
		"raw system",
		"effective system",
		"",
		nil,
		nil,
		0,
		0,
	)

	attrs := attrsMap(anatomy.logAttrs("session-123"))

	if got := attrs["session_id"]; got != "session-123" {
		t.Fatalf("session_id = %v, want session-123", got)
	}
	if got := attrs["prompt_raw_system_sha256"]; got != "b980eabec79b388c209684ea6c381aaaa53667641bddd5e7db3a693351e325a4" {
		t.Fatalf("prompt_raw_system_sha256 = %v", got)
	}
	if got := attrs["prompt_effective_system_sha256"]; got != "7b0d7caca604f36a8ca880d5ad1406898cf9ff9197eed95f619a346220e7e8dc" {
		t.Fatalf("prompt_effective_system_sha256 = %v", got)
	}
}

func attrsMap(attrs []any) map[string]any {
	out := make(map[string]any, len(attrs)/2)
	for i := 0; i+1 < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		if ok {
			out[key] = attrs[i+1]
		}
	}
	return out
}
