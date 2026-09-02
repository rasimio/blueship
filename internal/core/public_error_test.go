package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPublicInternalErrorDoesNotExposeCause(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	cause := errors.New(`provider status 429: {"request_id":"secret-provider-id"}`)

	got := PublicInternalError(logger, "agent", cause)

	const prefix = "Internal error. Trace ID: "
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("message = %q, want prefix %q", got, prefix)
	}
	traceID := strings.TrimPrefix(got, prefix)
	if _, err := uuid.Parse(traceID); err != nil {
		t.Fatalf("trace id %q is not a UUID: %v", traceID, err)
	}
	if strings.Contains(got, "provider") || strings.Contains(got, "secret-provider-id") {
		t.Fatalf("message exposes internal cause: %q", got)
	}
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if record["trace_id"] != traceID || record["error"] != cause.Error() {
		t.Fatalf("log does not correlate trace and cause: %s", logs.String())
	}
}
