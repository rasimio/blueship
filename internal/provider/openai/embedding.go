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
}

// NewEmbeddingProvider creates a new OpenAI embedding provider.
func NewEmbeddingProvider(apiKey, model string, timeout time.Duration) *EmbeddingProvider {
	return &EmbeddingProvider{
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: timeout},
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

// Embed generates an embedding vector for the given text.
func (p *EmbeddingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("openai not configured: missing API key")
	}

	body, err := json.Marshal(embeddingRequest{
		Input: text,
		Model: p.model,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", embeddingsURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("openai API returned %d", resp.StatusCode)
		if result.Error != nil {
			msg += ": " + result.Error.Message
		}
		return nil, fmt.Errorf("%s", msg)
	}

	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("openai returned empty embedding")
	}

	return result.Data[0].Embedding, nil
}
