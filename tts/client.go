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

// Client is a TTS HTTP client supporting OpenAI-compatible and ElevenLabs APIs.
type Client struct {
	endpoint    string
	endpointMP3 string // ElevenLabs MP3 endpoint (for non-Telegram clients)
	model       string
	speed       float64
	apiKey      string
	client      *http.Client
}

// NewClient creates a TTS client for an OpenAI-compatible endpoint.
func NewClient(endpoint, model, apiKey string, speed float64, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		endpoint: endpoint,
		model:    model,
		apiKey:   apiKey,
		speed:    speed,
		client:   &http.Client{Timeout: timeout},
	}
}

// NewElevenLabsClient creates a TTS client for the ElevenLabs API.
func NewElevenLabsClient(apiKey, voiceID, model string, speed float64, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if model == "" {
		model = "eleven_multilingual_v2"
	}
	base := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s", voiceID)
	return &Client{
		endpoint:    base + "?output_format=opus_48000_128",
		endpointMP3: base + "?output_format=mp3_44100_128",
		model:       model,
		speed:       speed,
		apiKey:      apiKey,
		client:      &http.Client{Timeout: timeout},
	}
}

// Synthesize sends text to the TTS API and returns audio bytes.
// For ElevenLabs: returns OGG Opus directly (no conversion needed).
// For OpenAI-compatible: returns WAV.
func (c *Client) Synthesize(ctx context.Context, text, voice, instruct string) ([]byte, error) {
	if c.endpointMP3 != "" {
		return c.synthesizeElevenLabs(ctx, text, instruct)
	}
	return c.synthesizeOpenAI(ctx, text, voice, instruct)
}

// SynthesizeMP3 returns MP3 audio (for clients that don't support OGG Opus).
func (c *Client) SynthesizeMP3(ctx context.Context, text, voice, instruct string) ([]byte, error) {
	if c.endpointMP3 != "" {
		return c.synthesizeElevenLabsWithEndpoint(ctx, c.endpointMP3, text, instruct)
	}
	// Fallback to default format
	return c.Synthesize(ctx, text, voice, instruct)
}

func (c *Client) synthesizeElevenLabs(ctx context.Context, text, instruct string) ([]byte, error) {
	return c.synthesizeElevenLabsWithEndpoint(ctx, c.endpoint, text, instruct)
}

func (c *Client) synthesizeElevenLabsWithEndpoint(ctx context.Context, endpoint, text, instruct string) ([]byte, error) {
	payload := map[string]any{
		"text":          text,
		"model_id":      c.model,
		"language_code": "ru",
		"voice_settings": map[string]any{
			"stability":         0.75,
			"similarity_boost":  0.95,
			"style":             0.0,
			"use_speaker_boost": true,
		},
	}
	if c.speed > 0 && c.speed != 1.0 {
		payload["speed"] = c.speed
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("tts: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tts: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", c.apiKey)

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

func (c *Client) synthesizeOpenAI(ctx context.Context, text, voice, instruct string) ([]byte, error) {
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
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

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
