package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestCompatibleReasoningEffortAndStreamFailures(t *testing.T) {
	tests := []struct {
		name, body        string
		stream, wantError bool
	}{
		{"complete", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`, false, false},
		{"stream", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true, false},
		{"premature-eof", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", true, true},
		{"malformed", "data: {broken\n\n", true, true},
		{"error-event", "data: {\"error\":{\"message\":\"subscription unavailable\"}}\n\n", true, true},
		{"invalid-tool-json", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call\",\"function\":{\"name\":\"run\",\"arguments\":\"{\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["reasoning_effort"] != "xhigh" {
					t.Errorf("reasoning_effort = %v", body["reasoning_effort"])
				}
				if body["temperature"] != 0.2 {
					t.Errorf("temperature = %v", body["temperature"])
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			provider := NewCompatibleProvider(server.URL, "", time.Second, nil)
			req := bs.CompletionRequest{Model: "model", Effort: "xhigh", Temperature: 0.2}
			var err error
			if tc.stream {
				_, err = provider.StreamComplete(context.Background(), req, nil)
			} else {
				_, err = provider.Complete(context.Background(), req)
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}
