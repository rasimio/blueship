package core

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type deferredProviderRetriesKey struct{}

// WithDeferredProviderRetries assigns transient completion retries to the
// durable scheduler. Providers return the first failure, including Retry-After,
// instead of sleeping while the task holds an execution slot.
func WithDeferredProviderRetries(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferredProviderRetriesKey{}, true)
}

func DeferredProviderRetries(ctx context.Context) bool {
	deferred, _ := ctx.Value(deferredProviderRetriesKey{}).(bool)
	return deferred
}

// HTTPFailure retains transport status and Retry-After through provider wraps.
// Classification never relies on words inside a model/provider error message.
type HTTPFailure struct {
	Code       int
	RetryDelay time.Duration
	Cause      error
}

func (e *HTTPFailure) Error() string { return e.Cause.Error() }
func (e *HTTPFailure) Unwrap() error { return e.Cause }

func NewHTTPFailure(code int, retryAfter string, cause error) error {
	delay := time.Duration(0)
	if seconds, err := strconv.ParseInt(strings.TrimSpace(retryAfter), 10, 64); err == nil && seconds > 0 {
		if seconds > int64(time.Duration(1<<63-1)/time.Second) {
			delay = time.Duration(1<<63 - 1)
		} else {
			delay = time.Duration(seconds) * time.Second
		}
	} else if when, err := http.ParseTime(retryAfter); err == nil {
		delay = max(0, time.Until(when))
	}
	return &HTTPFailure{Code: code, RetryDelay: delay, Cause: cause}
}

func TaskRetryPolicy(err error) (bool, time.Duration) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrTaskClaimLost) {
		return false, 0
	}
	var httpErr *HTTPFailure
	if errors.As(err, &httpErr) {
		return httpErr.Code == 408 || httpErr.Code == 429 || (httpErr.Code >= 500 && httpErr.Code <= 599), httpErr.RetryDelay
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true, 0
	}
	var network net.Error
	if errors.As(err, &network) {
		return network.Timeout() || network.Temporary(), 0
	}
	return false, 0
}
