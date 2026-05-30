package tool

import (
	"context"
	"encoding/json"

	bs "github.com/rasimio/blueship/internal/core"
)

// ToolEscalate is the canonical name of the escalation sentinel tool.
const ToolEscalate = "escalate"

// escalateSchema describes the arguments the interaction tier passes when it
// decides a turn needs the deep-reasoning (background) tier.
var escalateSchema = json.RawMessage(`{"type":"object","properties":{
	"reason":{"type":"string","description":"One short line on why the deep tier is needed (web research, multi-step reasoning, a tool action, deep memory recall)."},
	"guidance":{"type":"string","description":"Optional focusing instruction for the deep tier — what specifically to do or find."},
	"suggested_tools":{"type":"array","items":{"type":"string"},"description":"Optional tool names the deep tier is likely to need."}
},"required":["reason"]}`)

// RegisterEscalateTool registers the escalation sentinel. It is a no-op at
// execution time: escalation is detected from the tool-call trace by the
// gateway's interaction orchestrator, which then runs the background tier.
//
// The tool exists so the interaction model can express "this needs deeper
// thought" as an ordinary tool call mid-stream — it says a brief lead-in
// aloud, then calls escalate, exactly as a person says "hold on" before
// looking something up. Only the interaction (reflex) role is given access
// to it via the role allowlist.
func RegisterEscalateTool(r *bs.ToolRegistry) {
	r.Register(ToolEscalate,
		"Hand this turn off to the deep-reasoning tier (the background model) when it needs web research, tool actions, deep memory recall, or multi-step reasoning beyond a quick conversational reply. Before calling this, say a brief natural lead-in to the user out loud so they know you are working on it. Give a short reason; optionally pass guidance to focus the deep tier and suggested_tools it may need.",
		escalateSchema,
		func(ctx context.Context, input json.RawMessage) (any, error) {
			return map[string]any{"escalated": true}, nil
		},
	)
}
