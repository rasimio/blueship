package core

import (
	"context"
	"strings"
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
	return provider.Complete(ctx, req)
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
