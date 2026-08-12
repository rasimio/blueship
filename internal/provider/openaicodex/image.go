package openaicodex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	bs "github.com/rasimio/blueship/internal/core"
)

// imageToolType is the Responses API built-in tool. Unlike a function tool
// it carries no name or schema — the backend owns both — which is why the
// request struct leaves those fields empty.
const imageToolType = "image_generation"

// maxImageBytes bounds what a single generation may return. Well above what
// the backend produces at its largest documented size and well under
// Telegram's own photo ceiling, so the guard fires only on something
// pathological rather than on a legitimately big picture.
const maxImageBytes = 16 << 20

// GenerateImage satisfies bs.ImageGenerator.
//
// Image generation on this surface is not a separate endpoint. It is a
// built-in tool offered on an ordinary completion: the model decides to
// call it, and the bytes arrive mid-stream as base64 on
// response.image_generation_call.* events. So this is a normal responses
// request whose only job is to carry that one tool.
func (p *CompletionProvider) GenerateImage(ctx context.Context, prompt string) (bs.ImageResult, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return bs.ImageResult{}, fmt.Errorf("openai-codex: image prompt is empty")
	}

	token, err := p.tokens.AccessToken()
	if err != nil {
		return bs.ImageResult{}, fmt.Errorf("openai-codex: image token: %w", err)
	}

	body, err := json.Marshal(responsesRequest{
		Model: p.imageModel(),
		// The instruction is deliberately flat. Any persona belongs to the
		// caller's prompt; this request exists to move pixels.
		Instructions: "Generate the image the user describes. Do not reply with text.",
		Input: []any{inputMessage{
			Role:    "user",
			Content: []any{inputTextContent{Type: "input_text", Text: prompt}},
		}},
		Stream: true,
		Store:  false,
		Tools:  []responseTool{{Type: imageToolType}},
	})
	if err != nil {
		return bs.ImageResult{}, fmt.Errorf("openai-codex: marshal image request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL, bytes.NewReader(body))
	if err != nil {
		return bs.ImageResult{}, fmt.Errorf("openai-codex: create image request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return bs.ImageResult{}, fmt.Errorf("openai-codex: image request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return bs.ImageResult{}, fmt.Errorf("openai-codex: image request status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	encoded, err := readImagePayload(resp.Body)
	if err != nil {
		return bs.ImageResult{}, err
	}
	if encoded == "" {
		// The model answered without drawing. Callers surface this to the
		// user rather than retrying: a refusal here is usually about the
		// subject, and asking again produces the same refusal.
		return bs.ImageResult{}, fmt.Errorf("openai-codex: no image in response")
	}

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return bs.ImageResult{}, fmt.Errorf("openai-codex: decode image: %w", err)
	}
	if len(data) > maxImageBytes {
		return bs.ImageResult{}, fmt.Errorf("openai-codex: image is %d bytes, over the %d limit", len(data), maxImageBytes)
	}

	result := bs.ImageResult{Data: data, MIME: sniffImageMIME(data)}
	result.Width, result.Height = pngDimensions(data)
	if p.logger != nil {
		p.logger.Info("openai-codex: image generated",
			"bytes", len(data), "mime", result.MIME,
			"width", result.Width, "height", result.Height)
	}
	return result, nil
}

// imageModel picks the model that carries the image tool. Kept as its own
// hook so a future model split (a cheaper drawing model than the chat one)
// is a one-line change rather than a rewrite of the request builder.
func (p *CompletionProvider) imageModel() string {
	return "gpt-5.5"
}

// readImagePayload walks the SSE stream and keeps the largest base64 blob it
// sees.
//
// Largest rather than last on purpose: the backend emits progressively
// refined frames on response.image_generation_call.partial_image, and a
// final result may or may not arrive as its own event depending on the
// request. Taking the biggest payload yields the most complete picture under
// either shape instead of depending on which event terminates the stream.
func readImagePayload(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	// SSE frames carrying a full-resolution image run into megabytes, far
	// past bufio's default 64 KiB line cap.
	scanner.Buffer(make([]byte, 0, 1<<20), maxImageBytes+(1<<20))

	var best string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type            string `json:"type"`
			PartialImageB64 string `json:"partial_image_b64"`
			Result          string `json:"result"`
			Item            struct {
				Result string `json:"result"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if !strings.HasPrefix(event.Type, "response.image_generation_call") &&
			!strings.HasPrefix(event.Type, "response.output_item") {
			continue
		}
		for _, candidate := range []string{event.PartialImageB64, event.Result, event.Item.Result} {
			if len(candidate) > len(best) {
				best = candidate
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("openai-codex: read image stream: %w", err)
	}
	return best, nil
}

func sniffImageMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF8")):
		return "image/gif"
	case len(data) > 12 && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// pngDimensions reads width and height out of the IHDR chunk, which PNG
// pins to a fixed offset. Returns zeros for anything else; callers treat
// that as "not measured".
func pngDimensions(data []byte) (width, height int) {
	const ihdrEnd = 24
	if len(data) < ihdrEnd || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return 0, 0
	}
	return int(binary.BigEndian.Uint32(data[16:20])), int(binary.BigEndian.Uint32(data[20:24]))
}
