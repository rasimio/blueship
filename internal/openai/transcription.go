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

const transcriptionURL = "https://api.openai.com/v1/audio/transcriptions"

// TranscriptionProvider implements bs.TranscriptionProvider using OpenAI Whisper.
type TranscriptionProvider struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewTranscriptionProvider creates a new Whisper transcription provider.
func NewTranscriptionProvider(apiKey, model string, timeout time.Duration) *TranscriptionProvider {
	return &TranscriptionProvider{
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// IsConfigured returns true if the provider has an API key.
func (p *TranscriptionProvider) IsConfigured() bool {
	return p.apiKey != ""
}

// Transcribe sends audio data to Whisper and returns the transcribed text.
func (p *TranscriptionProvider) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	if !p.IsConfigured() {
		return "", fmt.Errorf("openai API key not configured")
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
	_ = w.WriteField("language", "ru")
	_ = w.WriteField("response_format", "json")
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", transcriptionURL, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
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
