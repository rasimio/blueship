package blueship

import "context"

// CascadeSelection changes one request's preferred route and generation settings.
// Overrides apply only to the preferred route; fallbacks keep their own settings.
// An empty Route selects the first configured route. Only disables fallback.
type CascadeSelection struct {
	Route  string
	Model  string
	Effort *string
	Only   bool
}

type CascadeAttempt struct {
	Route  string `json:"route"`
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
	Phase  string `json:"phase"`
	Usage  Usage  `json:"usage"`
}

type cascadeSelectionKey struct{}
type cascadeObserverKey struct{}

func WithCascadeSelection(ctx context.Context, selection CascadeSelection) context.Context {
	return context.WithValue(ctx, cascadeSelectionKey{}, selection)
}

// WithCascadeObserver observes public execution metadata, never prompts or reasoning.
// The callback runs synchronously and must return promptly.
func WithCascadeObserver(ctx context.Context, observer func(CascadeAttempt)) context.Context {
	return context.WithValue(ctx, cascadeObserverKey{}, observer)
}
