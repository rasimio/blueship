package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestCompletionPreservesRetryAfterWithoutJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "75")
		w.WriteHeader(429)
		_, _ = w.Write([]byte("upstream overloaded"))
	}))
	defer server.Close()
	provider := NewCompatibleProvider(server.URL, "test", time.Second, nil)
	_, err := provider.Complete(context.Background(), bs.CompletionRequest{Model: "test", Messages: []bs.Message{{Role: "user", Content: "test"}}})
	retry, delay := bs.TaskRetryPolicy(err)
	if !retry || delay != 75*time.Second {
		t.Fatal(err, retry, delay)
	}
}
