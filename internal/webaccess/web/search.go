package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

// SerperSearch implements bs.SearchEngine using the Serper.dev Google Search API.
type SerperSearch struct {
	apiKey     string
	httpClient *http.Client
}

// NewSerperSearch creates a new Serper search client.
func NewSerperSearch(apiKey string) *SerperSearch {
	return &SerperSearch{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

type serperRequest struct {
	Q   string `json:"q"`
	Num int    `json:"num,omitempty"`
}

type serperResponse struct {
	Organic []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
	} `json:"organic"`
}

// Search performs a Google search via Serper and returns results.
func (c *SerperSearch) Search(ctx context.Context, query string, limit int) ([]bs.SearchResult, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	payload, _ := json.Marshal(serperRequest{Q: query, Num: limit})

	req, err := http.NewRequestWithContext(ctx, "POST", "https://google.serper.dev/search", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("serper API status %d: %s", resp.StatusCode, body)
	}

	var sr serperResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	results := make([]bs.SearchResult, 0, len(sr.Organic))
	for _, r := range sr.Organic {
		results = append(results, bs.SearchResult{
			Title:       r.Title,
			URL:         r.Link,
			Description: r.Snippet,
		})
	}
	return results, nil
}
