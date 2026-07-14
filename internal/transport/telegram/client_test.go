package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testClient(rt roundTripFunc) *Client {
	return &Client{
		token:      "test",
		httpClient: &http.Client{Transport: rt},
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestPostJSONSurfacesTelegramAPIError(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests,
			`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":2}}`), nil
	})

	_, err := c.postJSON(context.Background(), "editMessageText", map[string]any{"text": "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiErr.ErrorCode != 429 || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("unexpected codes: %+v", apiErr)
	}
	if apiErr.Parameters.RetryAfter != 2 {
		t.Fatalf("retry_after = %d, want 2", apiErr.Parameters.RetryAfter)
	}
}

func TestSplitMessageUsesRunesAndPreservesText(t *testing.T) {
	input := strings.Repeat("я", 9000)
	chunks := splitMessage(input, maxTelegramMessageLength)
	if got := strings.Join(chunks, ""); got != input {
		t.Fatal("split chunks do not reconstruct the original text")
	}
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid UTF-8", i)
		}
		if n := utf8.RuneCountInString(chunk); n > maxTelegramMessageLength {
			t.Fatalf("chunk %d has %d runes", i, n)
		}
	}
}

func TestFinalizeResponseEditsRawRichMarkdown(t *testing.T) {
	report := "# Отчёт\n\n| KPI | Значение |\n|---|---:|\n| Uptime | 99.9% |\n\n" +
		"1. Сводка\n   - Вложенный пункт\n\n$x^2 + y^2$\n\n" +
		"![График](https://example.com/chart.png)"
	var requests []map[string]any
	c := testClient(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, payload)
		if !strings.HasSuffix(req.URL.Path, "/editMessageText") {
			t.Fatalf("unexpected method path: %s", req.URL.Path)
		}
		return jsonResponse(http.StatusOK, `{"ok":true,"result":{"message_id":99}}`), nil
	})

	if err := c.FinalizeResponse(context.Background(), 42, 99, report); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if _, exists := requests[0]["text"]; exists {
		t.Fatal("rich final edit unexpectedly used legacy text")
	}
	rich, ok := requests[0]["rich_message"].(map[string]any)
	if !ok {
		t.Fatalf("missing rich_message payload: %#v", requests[0])
	}
	if got := rich["markdown"]; got != report {
		t.Fatalf("markdown was modified:\n%v", got)
	}
}

func TestPrepareRichMarkdownNormalizesDisplayMath(t *testing.T) {
	input := "До\n\n$$\nE = mc^2\n$$\n\nПосле\n\n\\[a^2+b^2=c^2\\]"
	want := "До\n\n<tg-math-block>E = mc^2</tg-math-block>\n\nПосле\n\n<tg-math-block>a^2+b^2=c^2</tg-math-block>"
	if got := prepareRichMarkdown(input); got != want {
		t.Fatalf("display math normalization:\n got: %q\nwant: %q", got, want)
	}
}

func TestPrepareRichMarkdownNormalizesFencedPipeTable(t *testing.T) {
	input := "Вот срез:\n\n```\nДата | Подходы | Объём\n-----|---------|------\n13.07.26 | 8/8/7 | 44.5\n```\n\nИтог"
	want := "Вот срез:\n\n| Дата | Подходы | Объём |\n| ----- | --------- | ------ |\n| 13.07.26 | 8/8/7 | 44.5 |\n\nИтог"
	if got := prepareRichMarkdown(input); got != want {
		t.Fatalf("fenced table normalization:\n got: %q\nwant: %q", got, want)
	}
}

func TestPrepareRichMarkdownPreservesCodeFences(t *testing.T) {
	tests := []string{
		"```go\na := left | right\n```",
		"```\na | b\nresult := a | b\n```",
	}
	for _, input := range tests {
		if got := prepareRichMarkdown(input); got != input {
			t.Fatalf("code block was modified:\n got: %q\nwant: %q", got, input)
		}
	}
}

func TestFinalizeResponseFallsBackToLegacyEdit(t *testing.T) {
	var payloads []map[string]any
	c := testClient(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
		if len(payloads) == 1 {
			return jsonResponse(http.StatusBadRequest,
				`{"ok":false,"error_code":400,"description":"Bad Request: can't parse rich message"}`), nil
		}
		return jsonResponse(http.StatusOK, `{"ok":true,"result":{"message_id":7}}`), nil
	})

	if err := c.FinalizeResponse(context.Background(), 42, 7, "# Report\n\nBody"); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 2 {
		t.Fatalf("requests = %d, want rich edit + legacy edit", len(payloads))
	}
	if _, ok := payloads[0]["rich_message"]; !ok {
		t.Fatal("first request was not rich")
	}
	if _, ok := payloads[1]["text"]; !ok {
		t.Fatal("second request was not the legacy fallback")
	}
}

func TestFinalizeResponseSplitsOversizedRichReport(t *testing.T) {
	input := strings.Repeat("я", maxTelegramRichChunkLength+5000)
	var richChunks []string
	var paths []string
	c := testClient(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			RichMessage InputRichMessage `json:"rich_message"`
		}
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		richChunks = append(richChunks, payload.RichMessage.Markdown)
		paths = append(paths, req.URL.Path)
		return jsonResponse(http.StatusOK, `{"ok":true,"result":{"message_id":8}}`), nil
	})

	if err := c.FinalizeResponse(context.Background(), 42, 8, input); err != nil {
		t.Fatal(err)
	}
	if len(richChunks) != 2 {
		t.Fatalf("rich chunks = %d, want 2", len(richChunks))
	}
	if !strings.HasSuffix(paths[0], "/editMessageText") || !strings.HasSuffix(paths[1], "/sendRichMessage") {
		t.Fatalf("unexpected paths: %v", paths)
	}
	if got := strings.Join(richChunks, ""); got != input {
		t.Fatal("rich chunks do not reconstruct the report")
	}
	for i, chunk := range richChunks {
		if n := utf8.RuneCountInString(chunk); n > maxTelegramRichChunkLength {
			t.Fatalf("chunk %d has %d runes", i, n)
		}
	}
}
