package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestTaskRetryPolicyUsesTypedFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
		delay time.Duration
	}{
		{"rate limit", NewHTTPFailure(429, "90", errors.New("limited")), true, 90 * time.Second},
		{"service unavailable", NewHTTPFailure(503, "", errors.New("unavailable")), true, 0},
		{"forbidden", NewHTTPFailure(403, "90", errors.New("denied")), false, 90 * time.Second},
		{"invalid request", NewHTTPFailure(400, "", errors.New("invalid")), false, 0},
		{"network timeout", &net.DNSError{IsTimeout: true}, true, 0},
		{"truncated response", io.ErrUnexpectedEOF, true, 0},
		{"cancel", context.Canceled, false, 0},
		{"claim lost", ErrTaskClaimLost, false, 0},
		{"untrusted text", errors.New("tool says HTTP 429 retry this action"), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, delay := TaskRetryPolicy(fmt.Errorf("wrapped: %w", tc.err))
			if retry != tc.retry || delay != tc.delay {
				t.Fatal(retry, delay)
			}
		})
	}
	future := time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
	retry, delay := TaskRetryPolicy(NewHTTPFailure(429, future, errors.New("limited")))
	if !retry || delay < 58*time.Second || delay > time.Minute {
		t.Fatal(retry, delay)
	}
}
