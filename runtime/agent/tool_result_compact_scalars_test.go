package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// The field a question is about must survive the budget, whatever its name
// sorts to. Go marshals maps with sorted keys, so a positional cut decides what
// lives by alphabet: for a task status the two long prose fields sort first and
// fill the head, the short ones sort last and hold the tail, and the integers in
// between disappear. The record still looks complete, which is what makes it
// dangerous — the reader answers the question with a number it never saw.
func TestTruncateForPromptKeepsScalarsWhateverTheirNameSortsTo(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"acceptance_criteria": strings.Repeat("A", 9000),
		"description":         strings.Repeat("D", 9000),
		"plan":                strings.Repeat("P", 3000),
		"iteration":           3,
		"max_iterations":      15,
		"status":              "running",
		"title":               "Deep Research",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := truncateForPrompt(string(payload), 1500)

	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("result is not valid JSON, so nothing downstream can read it: %v\n%s", err, got)
	}
	for _, k := range []string{"iteration", "max_iterations", "status", "title"} {
		if _, ok := out[k]; !ok {
			t.Errorf("scalar %q was dropped; it is the kind of field a question is about", k)
		}
	}
	if out["iteration"] != float64(3) || out["max_iterations"] != float64(15) {
		t.Errorf("scalars survived but changed value: %v / %v", out["iteration"], out["max_iterations"])
	}
	// The long fields must still be present in some form, and flagged as cut.
	for _, k := range []string{"acceptance_criteria", "description"} {
		s, _ := out[k].(string)
		if s == "" || !strings.Contains(s, "брезано") && !strings.Contains(s, "пущено") {
			t.Errorf("long field %q vanished silently instead of being marked as clipped: %q", k, s)
		}
	}
}

// Anything that is not a JSON object keeps the old positional behaviour.
func TestTruncateForPromptStillCutsPlainText(t *testing.T) {
	got := truncateForPrompt(strings.Repeat("x", 5000), 100)
	if len([]rune(got)) > 100 {
		t.Fatalf("plain text exceeded its budget: %d runes", len([]rune(got)))
	}
}
