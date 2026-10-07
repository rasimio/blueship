package gemini

import (
	"context"
	"errors"
	bs "github.com/rasimio/blueship/internal/core"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeferredProviderRetriesPreserveRateLimit(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"slow down"}}`))
		}))
		p := NewCompletionProvider("test-key", time.Second)
		p.generateURL = server.URL + "/%s?key=%s"
		p.backoffs = []time.Duration{0}
		ctx := context.Background()
		if deferred {
			ctx = bs.WithDeferredProviderRetries(ctx)
		}
		_, err := p.Complete(ctx, bs.CompletionRequest{Model: "test", MaxTokens: 256})
		server.Close()
		var failure *bs.HTTPFailure
		want := 2
		if deferred {
			want = 1
		}
		if calls != want || !errors.As(err, &failure) || failure.Code != 429 || failure.RetryDelay != 90*time.Second {
			t.Fatal(deferred, calls, err)
		}
	}
}
