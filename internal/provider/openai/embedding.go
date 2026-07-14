package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const embeddingsURL = "https://api.openai.com/v1/embeddings"

// EmbeddingProvider implements bs.EmbeddingProvider using OpenAI embeddings.
type EmbeddingProvider struct {
	apiKey     string
	model      string
	httpClient *http.Client
	backoffs   []time.Duration
}

// NewEmbeddingProvider creates a new OpenAI embedding provider.
func NewEmbeddingProvider(apiKey, model string, timeout time.Duration) *EmbeddingProvider {
	return &EmbeddingProvider{
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: timeout},
		// Embeddings are quick, so a transient proxy blip (e.g. an xray VLESS
		// flap) only needs a brief retry — not the multi-second schedule the
		// LLM providers use for rate limits. Two retries on transient failures.
		backoffs: []time.Duration{500 * time.Millisecond, 2 * time.Second},
	}
}

type embeddingRequest struct {
	Input string `json:"input"`
	Model string `json:"model"`
}

type embeddingResponse struct {
	Data  []embeddingData `json:"data"`
	Error *apiError       `json:"error,omitempty"`
}

type embeddingData struct {
	Embedding []float32 `json:"embedding"`
}

type apiError struct {
	Message string `json:"message"`
}

// UnmarshalJSON accepts both shapes the upstream returns:
//   - {"error": {"message": "..."}}     — OpenAI canonical
//   - {"error": "..."}                  — bare string (some MLX builds)
func (e *apiError) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.Message = s
		return nil
	}
	type alias apiError
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*e = apiError(a)
	return nil
}

// Embed generates an embedding vector for the given text. Transient failures
// (network timeout / connection reset through the proxy, HTTP 429, HTTP 5xx)
// are retried with a short backoff; deterministic errors (bad key, 4xx, empty
// embedding) fail immediately.
func (p *EmbeddingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("openai not configured: missing API key")
	}

	var lastErr error
	for attempt := 0; attempt <= len(p.backoffs); attempt++ {
		vec, retryable, err := p.embedOnce(ctx, text)
		if err == nil {
			return vec, nil
		}
		lastErr = err
		if !retryable || attempt == len(p.backoffs) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.backoffs[attempt]):
		}
	}
	return nil, lastErr
}

// embedOnce performs a single embedding request. The bool reports whether the
// error (if any) is worth retrying.
func (p *EmbeddingProvider) embedOnce(ctx context.Context, text string) ([]float32, bool, error) {
	body, err := json.Marshal(embeddingRequest{
		Input: text,
		Model: p.model,
	})
	if err != nil {
		return nil, false, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", embeddingsURL, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		// Network-level error: client timeout, connection reset/refused through
		// the proxy. Transient — worth a retry (unless the ctx itself is done).
		return nil, ctx.Err() == nil, fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		// Truncated/garbled body (e.g. a proxy error page) — likely transient.
		return nil, true, fmt.Errorf("decode response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("openai API returned %d", resp.StatusCode)
		if result.Error != nil {
			msg += ": " + result.Error.Message
		}
		// 429 (rate limit) and 5xx (server) are transient; other 4xx are not.
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retryable, fmt.Errorf("%s", msg)
	}

	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, false, fmt.Errorf("openai returned empty embedding")
	}

	return result.Data[0].Embedding, false, nil
}
