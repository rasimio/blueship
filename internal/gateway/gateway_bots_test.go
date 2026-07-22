package gateway

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rasimio/blueship/internal/transport/telegram"
)

func TestExplicitTelegramRejection(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "bot api error code in successful http envelope",
			err:  &telegram.APIError{StatusCode: 200, ErrorCode: 429},
			want: true,
		},
		{
			name: "http client rejection",
			err:  &telegram.APIError{StatusCode: 403},
			want: true,
		},
		{
			name: "wrapped parse rejection",
			err:  fmt.Errorf("send rich: %w", &telegram.APIError{StatusCode: 400, ErrorCode: 400}),
			want: true,
		},
		{
			name: "server error remains ambiguous",
			err:  &telegram.APIError{StatusCode: 500, ErrorCode: 500},
			want: false,
		},
		{
			name: "request timeout remains ambiguous",
			err:  &telegram.APIError{StatusCode: 408, ErrorCode: 408},
			want: false,
		},
		{
			name: "transport eof remains ambiguous",
			err:  errors.New("unexpected EOF"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := explicitTelegramRejection(tt.err)
			if got != tt.want {
				t.Fatalf("explicitTelegramRejection() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserBotResolutionRetryClassification(t *testing.T) {
	transient := userBotResolutionFailure(errors.New("database unavailable"), true)
	if !isRetryableUserBotResolution(transient) {
		t.Fatal("transient resolution failure classified as permanent")
	}
	permanent := userBotResolutionFailure(errors.New("primary channel is not telegram"), false)
	if isRetryableUserBotResolution(permanent) {
		t.Fatal("permanent resolution failure classified as retryable")
	}
	if isRetryableUserBotResolution(errors.New("plain transport error")) {
		t.Fatal("untyped error classified as retryable resolution failure")
	}
}
