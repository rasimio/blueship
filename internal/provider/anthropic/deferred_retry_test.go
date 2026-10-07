package anthropic

import (
	"context"
	"errors"
	bs "github.com/rasimio/blueship/internal/core"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDeferredProviderRetriesPreserveRateLimit(t *testing.T) {
	for _, tokens := range []int{256, 10000} {
		for _, deferred := range []bool{false, true} {
			calls := 0
			p := NewProvider("test-key", time.Second, []time.Duration{0}, slog.New(slog.DiscardHandler))
			p.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"90"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))}, nil
			})}
			ctx := context.Background()
			if deferred {
				ctx = bs.WithDeferredProviderRetries(ctx)
			}
			_, err := p.Complete(ctx, bs.CompletionRequest{Model: "test", MaxTokens: tokens})
			var failure *bs.HTTPFailure
			want := 2
			if deferred {
				want = 1
			}
			if calls != want || !errors.As(err, &failure) || failure.Code != 429 || failure.RetryDelay != 90*time.Second {
				t.Fatal(tokens, deferred, calls, err)
			}
		}
	}
}
