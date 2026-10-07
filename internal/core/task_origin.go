package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// TaskOrigin is captured by the trusted transport, never from model arguments.
// Credentials are resolved at delivery time and are never stored in a task.
type TaskOrigin struct {
	Transport string    `json:"transport"`
	BotID     uuid.UUID `json:"bot_id"`
	ChatID    string    `json:"chat_id"`
}

type taskOriginKey struct{}

func ContextWithTaskOrigin(ctx context.Context, origin TaskOrigin) context.Context {
	return context.WithValue(ctx, taskOriginKey{}, origin)
}

func TaskOriginFromContext(ctx context.Context) (TaskOrigin, bool) {
	origin, ok := ctx.Value(taskOriginKey{}).(TaskOrigin)
	return origin, ok
}

func TaskOriginFromConfig(config json.RawMessage) (TaskOrigin, bool, error) {
	var fields map[string]json.RawMessage
	if len(config) == 0 {
		return TaskOrigin{}, false, nil
	}
	if err := json.Unmarshal(config, &fields); err != nil {
		return TaskOrigin{}, false, err
	}
	raw, ok := fields["delivery_origin"]
	if !ok {
		return TaskOrigin{}, false, nil
	}
	var origin TaskOrigin
	if err := json.Unmarshal(raw, &origin); err != nil {
		return origin, true, err
	}
	if origin.Transport == "" || origin.ChatID == "" {
		return origin, true, fmt.Errorf("invalid task delivery origin")
	}
	return origin, true, nil
}

func captureTaskOrigin(ctx context.Context, config json.RawMessage) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &fields); err != nil {
			return nil, err
		}
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	// Drop model-supplied routing, even when no trusted origin is available.
	delete(fields, "delivery_origin")
	if origin, ok := TaskOriginFromContext(ctx); ok {
		if origin.Transport == "" || origin.ChatID == "" {
			return nil, fmt.Errorf("invalid task delivery origin")
		}
		raw, err := json.Marshal(origin)
		if err != nil {
			return nil, err
		}
		fields["delivery_origin"] = raw
	}
	return json.Marshal(fields)
}
