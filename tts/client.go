package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is an OpenAI-compatible TTS HTTP client.
type Client struct {
	endpoint string
	model    string
	speed    float64
	client   *http.Client
}

// NewClient creates a TTS client for the given endpoint and model.
func NewClient(endpoint, model string, speed float64, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		endpoint: endpoint,
		model:    model,
		speed:    speed,
		client:   &http.Client{Timeout: timeout},
	}
}

// Synthesize sends text to the TTS API and returns WAV audio bytes.
func (c *Client) Synthesize(ctx context.Context, text, voice, instruct string) ([]byte, error) {
	payload := map[string]any{
		"model":           c.model,
		"input":           text,
		"response_format": "wav",
	}
	if voice != "" {
		payload["voice"] = voice
	}
	if instruct != "" {
		payload["instruct"] = instruct
	}
	if c.speed > 0 && c.speed != 1.0 {
		payload["speed"] = c.speed
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("tts: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tts: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tts: status %d: %s", resp.StatusCode, string(errBody))
	}

	return io.ReadAll(resp.Body)
}
