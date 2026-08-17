package gateway

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

// A rule that prescribes a tool is an operator decision already taken. Before
// this, the prescription reached the model only as the rule's DO: text on the
// text path, so the call happened when the model felt like it — measured at 21
// turns in 30 on the local model, and silent when it did not: the answer looks
// the same, it is simply composed from nothing.
func TestRulePreActionsRunAndTheirOutputReachesTheModel(t *testing.T) {
	reg := bs.NewToolRegistry()
	reg.Register("browser_fetch", "fetch", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) { return "CPU 49.1, GPU 44.9", nil })

	var traces []agent.ToolTrace
	var research strings.Builder
	rule := bs.ActiveRule{
		ID:         "server",
		PreActions: []bs.ToolAction{{Tool: "browser_fetch", Input: json.RawMessage(`{"url":"https://example.invalid"}`)}},
	}

	testGateway().runRulePreActions(context.Background(),
		&UserState{Registry: reg}, newTurnTimer(), rule, &traces, &research)

	if !strings.Contains(research.String(), "CPU 49.1") {
		t.Fatalf("the tool ran but its output never reached the prompt: %q", research.String())
	}
	if len(traces) != 1 || !strings.HasSuffix(traces[0].Name, "[rule]") {
		t.Fatalf("trace should mark this as operator-prescribed, got %#v", traces)
	}
}

// A failing pre-action must not put an error string in front of the model as
// if it were research. The turn continues; the model simply has no data, which
// is the honest state.
func TestRulePreActionFailureIsNotPresentedAsResearch(t *testing.T) {
	reg := bs.NewToolRegistry()
	reg.Register("browser_fetch", "fetch", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) { return nil, io.ErrUnexpectedEOF })

	var traces []agent.ToolTrace
	var research strings.Builder
	rule := bs.ActiveRule{ID: "server", PreActions: []bs.ToolAction{{Tool: "browser_fetch", Input: json.RawMessage(`{}`)}}}

	testGateway().runRulePreActions(context.Background(),
		&UserState{Registry: reg}, newTurnTimer(), rule, &traces, &research)

	if research.Len() != 0 {
		t.Fatalf("a failed fetch was presented as research: %q", research.String())
	}
	if len(traces) != 1 || !traces[0].Error {
		t.Fatalf("the failure should still be traceable for /debug, got %#v", traces)
	}
}

// A rule with no prescribed tools must not fabricate an empty research block.
func TestRuleWithoutPreActionsWritesNothing(t *testing.T) {
	var traces []agent.ToolTrace
	var research strings.Builder
	testGateway().runRulePreActions(context.Background(),
		&UserState{Registry: bs.NewToolRegistry()}, newTurnTimer(), bs.ActiveRule{ID: "plain"}, &traces, &research)
	if research.Len() != 0 || len(traces) != 0 {
		t.Fatalf("a rule with no pre_actions produced output: %q / %#v", research.String(), traces)
	}
}
