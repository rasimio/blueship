package core

import (
	"context"
	"strings"

	"github.com/rasimio/blueship/telemetry"
)

// LLMRouter routes CompletionRequest by provider name.
// If req.Model is prefixed as "provider:model", it overrides modelProviders.
type LLMRouter struct {
	defaultProvider CompletionProvider
	providers       map[string]CompletionProvider
	modelProviders  map[string]string
}

// NewLLMRouter creates a router with provider registry and model routing.
func NewLLMRouter(defaultProvider CompletionProvider, providers map[string]CompletionProvider, modelProviders map[string]string) *LLMRouter {
	if providers == nil {
		providers = make(map[string]CompletionProvider)
	}
	if modelProviders == nil {
		modelProviders = make(map[string]string)
	}
	return &LLMRouter{
		defaultProvider: defaultProvider,
		providers:       providers,
		modelProviders:  modelProviders,
	}
}

// Complete implements CompletionProvider by dispatching to a provider.
func (r *LLMRouter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	modelName, providerName := r.resolve(req.Model)
	req.Model = modelName

	provider := r.providers[providerName]
	if provider == nil {
		provider = r.defaultProvider
	}
	if provider == nil {
		return nil, ErrProviderNotConfigured
	}

	ctx, span := telemetry.StartLLMSpan(ctx, providerName, modelName)
	defer span.End()

	resp, err := provider.Complete(ctx, req)
	if err != nil {
		telemetry.RecordError(span, err)
		return nil, err
	}
	telemetry.AnnotateLLMResult(span, resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.StopReason)
	return resp, nil
}

// StreamComplete implements StreamCompletionProvider by dispatching to a streaming provider.
// Falls back to batch Complete + synthetic onText if provider doesn't support streaming.
func (r *LLMRouter) StreamComplete(ctx context.Context, req CompletionRequest, onText func(string)) (*CompletionResponse, error) {
	modelName, providerName := r.resolve(req.Model)
	req.Model = modelName

	provider := r.providers[providerName]
	if provider == nil {
		provider = r.defaultProvider
	}
	if provider == nil {
		return nil, ErrProviderNotConfigured
	}

	// Use streaming if provider supports it.
	if sp, ok := provider.(StreamCompletionProvider); ok {
		return sp.StreamComplete(ctx, req, onText)
	}

	// Fallback: batch complete, then call onText with full text.
	resp, err := provider.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if onText != nil {
		if text := ExtractText(resp.Content); text != "" {
			onText(text)
		}
	}
	return resp, nil
}

// resolve returns (modelName, providerName).
func (r *LLMRouter) resolve(model string) (string, string) {
	if parts := strings.SplitN(model, ":", 2); len(parts) == 2 {
		return parts[1], parts[0]
	}
	if provider, ok := r.modelProviders[model]; ok {
		return model, provider
	}
	return model, ""
}

// ErrProviderNotConfigured is returned when no provider is available.
var ErrProviderNotConfigured = &ProviderError{Message: "LLM provider not configured"}

// ProviderError describes provider resolution errors.
type ProviderError struct {
	Message string
}

func (e *ProviderError) Error() string { return e.Message }
