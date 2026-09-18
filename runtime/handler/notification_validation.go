package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/rasimio/blueship/internal/core"
)

// Validate only the outgoing prose, after task-control markers were handled.
// The private task plan/result is not a user acknowledgement and must remain
// machine-readable for the next iteration.
func validateBackgroundNotification(ctx context.Context, validate core.ResponseValidator, request core.ResponseValidationRequest, text string, keyedDelivery bool) (string, error) {
	const handoff = "[voice_handoff]\n"
	prefix := ""
	if strings.HasPrefix(text, handoff) {
		prefix, text = handoff, strings.TrimPrefix(text, handoff)
	}
	request.Text, request.PendingTools = text, nil
	safe, err := validate(ctx, request)
	if err != nil {
		return "", fmt.Errorf("validate task notification: %w", err)
	}
	if strings.TrimSpace(safe) == "" {
		return "", fmt.Errorf("validate task notification: empty replacement")
	}
	// A rewritten reminder may no longer cover every acknowledged item. Never
	// mark their deliveries based on a different body.
	if keyedDelivery && safe != text {
		return "", fmt.Errorf("validate task notification: keyed delivery text changed")
	}
	return prefix + safe, nil
}
