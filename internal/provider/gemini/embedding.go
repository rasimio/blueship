package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

const embedContentURL = "https://generativelanguage.googleapis.com/v1beta/models/%s:embedContent?key=%s"

// Task types the Gemini embedding API distinguishes: a document is
// embedded to be found, a query to find — the asymmetry is where a good
// part of the retrieval gain over symmetric encoders comes from.
const (
	taskDocument = "RETRIEVAL_DOCUMENT"
	taskQuery    = "RETRIEVAL_QUERY"
)

// EmbeddingProvider implements bs.EmbeddingProvider (and QueryEmbedder)
// over the Gemini embedding API. Embed encodes documents; EmbedQuery
// encodes queries. Vectors truncated below the model's native width
// (outputDimensionality) are renormalised to unit length, as the API
// documents — only the full-width vectors come back normalised.
type EmbeddingProvider struct {
	apiKey     string
	model      string
	dimension  int
	baseURL    string
	httpClient *http.Client
	backoffs   []time.Duration
}

// NewEmbeddingProvider creates a Gemini embedding provider for model
// (e.g. gemini-embedding-001) at dimension (0 = the model's native width).
func NewEmbeddingProvider(apiKey, model string, dimension int, timeout time.Duration) *EmbeddingProvider {
	return &EmbeddingProvider{
		apiKey:     apiKey,
		model:      model,
		dimension:  dimension,
		baseURL:    embedContentURL,
		httpClient: &http.Client{Timeout: timeout},
		backoffs:   []time.Duration{500 * time.Millisecond, 2 * time.Second},
	}
}

// WithBaseURL points the provider at another endpoint (tests). The URL is
// a format string taking the model and the key.
func (p *EmbeddingProvider) WithBaseURL(url string) *EmbeddingProvider {
	p.baseURL = url
	return p
}

type embedRequest struct {
	Model                string       `json:"model"`
	Content              embedContent `json:"content"`
	TaskType             string       `json:"taskType,omitempty"`
	OutputDimensionality int          `json:"outputDimensionality,omitempty"`
}

type embedContent struct {
	Parts []embedPart `json:"parts"`
}

type embedPart struct {
	Text string `json:"text"`
}

type embedResponse struct {
	Embedding *struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed encodes a document.
func (p *EmbeddingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return p.embed(ctx, text, taskDocument)
}

// EmbedQuery encodes a query.
func (p *EmbeddingProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return p.embed(ctx, text, taskQuery)
}

func (p *EmbeddingProvider) embed(ctx context.Context, text, task string) ([]float32, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("gemini not configured: missing API key")
	}
	var lastErr error
	for attempt := 0; attempt <= len(p.backoffs); attempt++ {
		vec, retryable, err := p.embedOnce(ctx, text, task)
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

func (p *EmbeddingProvider) embedOnce(ctx context.Context, text, task string) ([]float32, bool, error) {
	body, err := json.Marshal(embedRequest{
		Model:                "models/" + p.model,
		Content:              embedContent{Parts: []embedPart{{Text: text}}},
		TaskType:             task,
		OutputDimensionality: p.dimension,
	})
	if err != nil {
		return nil, false, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf(p.baseURL, p.model, p.apiKey), bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()
	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, true, fmt.Errorf("decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("gemini API returned %d", resp.StatusCode)
		if result.Error != nil {
			msg += ": " + result.Error.Message
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retryable, fmt.Errorf("%s", msg)
	}
	if result.Embedding == nil || len(result.Embedding.Values) == 0 {
		return nil, false, fmt.Errorf("gemini returned empty embedding")
	}
	vec := result.Embedding.Values
	if p.dimension > 0 && len(vec) != p.dimension {
		return nil, false, fmt.Errorf("gemini returned %d dimensions, configured %d", len(vec), p.dimension)
	}
	return normalize(vec), false, nil
}

// normalize scales a vector to unit length; a zero vector stays zero.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := 1 / math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) * inv)
	}
	return out
}
