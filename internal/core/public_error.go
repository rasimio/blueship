package core

import (
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// PublicInternalError logs the full internal cause and returns only a safe,
// correlatable message for user-facing transports.
func PublicInternalError(logger *slog.Logger, source string, err error) string {
	traceID := uuid.NewString()
	logger.Error("internal error",
		"trace_id", traceID,
		"source", source,
		"error", err,
	)
	return fmt.Sprintf("Internal error. Trace ID: %s", traceID)
}
