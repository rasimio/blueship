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

// DeepSeek V4 reasons by default. A role configured with thinking "off" must
// reach the backend as an explicit switch, on both the plain and the streaming
// path, and only on backends that asked for it.
func TestThinkingSwitch(t *testing.T) {
	tests := []struct {
		name    string
		extra   map[string]any
		mode    string
		want    any
		present bool
	}{
		{"off disables", map[string]any{"thinking_switch": true}, "off", "disabled", true},
		{"adaptive enables", map[string]any{"thinking_switch": true}, "adaptive", "enabled", true},
		{"empty keeps backend default", map[string]any{"thinking_switch": true}, "", nil, false},
		{"backend without the switch gets nothing", nil, "off", nil, false},
	}
	for _, tc := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if _, leaked := body["thinking_switch"]; leaked {
						t.Error("thinking_switch leaked into the request")
					}
					thinking, present := body["thinking"].(map[string]any)
					if present != tc.present {
						t.Errorf("thinking = %v, want present=%v", body["thinking"], tc.present)
					}
					if present && thinking["type"] != tc.want {
						t.Errorf("thinking.type = %v, want %v", thinking["type"], tc.want)
					}
					if stream {
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						return
					}
					fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
				}))
				defer server.Close()
				var extra map[string]any
				if tc.extra != nil {
					extra = map[string]any{}
					for k, v := range tc.extra {
						extra[k] = v
					}
				}
				provider := NewCompatibleProvider(server.URL, "", time.Second, extra)
				req := bs.CompletionRequest{Model: "deepseek-v4-flash", ThinkingMode: tc.mode}
				var err error
				if stream {
					_, err = provider.StreamComplete(context.Background(), req, nil)
				} else {
					_, err = provider.Complete(context.Background(), req)
				}
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
