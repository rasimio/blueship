package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

const defaultTranscriptionURL = "https://api.openai.com/v1/audio/transcriptions"

// TranscriptionProvider implements bs.TranscriptionProvider using OpenAI Whisper
// or any OpenAI-compatible STT endpoint (e.g. local MLX Whisper).
type TranscriptionProvider struct {
	apiKey     string
	model      string
	endpoint   string
	httpClient *http.Client
}

// NewTranscriptionProvider creates a new Whisper transcription provider.
func NewTranscriptionProvider(apiKey, model string, timeout time.Duration) *TranscriptionProvider {
	return &TranscriptionProvider{
		apiKey:     apiKey,
		model:      model,
		endpoint:   defaultTranscriptionURL,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// NewTranscriptionProviderWithEndpoint creates a provider pointing to a custom endpoint
// (e.g. local MLX Whisper on localhost:12102).
func NewTranscriptionProviderWithEndpoint(endpoint, model string, timeout time.Duration) *TranscriptionProvider {
	return &TranscriptionProvider{
		model:      model,
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// IsConfigured returns true if the provider has an API key or a custom endpoint.
func (p *TranscriptionProvider) IsConfigured() bool {
	return p.apiKey != "" || p.endpoint != defaultTranscriptionURL
}

// Transcribe sends audio data to Whisper and returns the transcribed text.
func (p *TranscriptionProvider) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	if !p.IsConfigured() {
		return "", fmt.Errorf("transcription provider not configured")
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(audio); err != nil {
		return "", fmt.Errorf("write audio: %w", err)
	}

	_ = w.WriteField("model", p.model)
	_ = w.WriteField("response_format", "json")
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", p.endpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("whisper status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.Text, nil
}
