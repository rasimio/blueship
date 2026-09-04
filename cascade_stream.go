package blueship

import (
	"encoding/json"
	"sync/atomic"
)

func guardedCallbacks(cb *StreamCallbacks, emitted *atomic.Bool) *StreamCallbacks {
	if cb == nil {
		return nil
	}
	out := &StreamCallbacks{}
	if cb.OnText != nil {
		out.OnText = func(v string) {
			if v != "" {
				emitted.Store(true)
			}
			cb.OnText(v)
		}
	}
	if cb.OnThinking != nil {
		out.OnThinking = func(v string) {
			if v != "" {
				emitted.Store(true)
			}
			cb.OnThinking(v)
		}
	}
	if cb.OnToolUse != nil {
		out.OnToolUse = func(id, name string, input json.RawMessage) { emitted.Store(true); cb.OnToolUse(id, name, input) }
	}
	if cb.OnToolResult != nil {
		out.OnToolResult = func(id, output string, failed bool, ms int) {
			emitted.Store(true)
			cb.OnToolResult(id, output, failed, ms)
		}
	}
	if cb.OnUsage != nil {
		out.OnUsage = func(input, output int) { emitted.Store(true); cb.OnUsage(input, output) }
	}
	return out
}
