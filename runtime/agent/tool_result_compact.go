package agent

import (
	"encoding/json"
	"unicode/utf8"

	bs "github.com/rasimio/blueship/internal/core"
)

const (
	maxPromptToolResultChars        = 4000
	maxPromptBrowserFetchTextChars  = 3000
	maxPromptErroredToolResultChars = 4000
)

func compactToolResultBlockForPrompt(block bs.ContentBlock) bs.ContentBlock {
	if block.Type != "tool_result" {
		return block
	}
	content, ok := block.Content.(string)
	if !ok {
		data, err := json.Marshal(block.Content)
		if err != nil {
			return block
		}
		content = string(data)
	}
	block.Content = compactToolResultForPrompt(block.Name, content, block.IsError)
	return block
}

func compactToolResultForPrompt(toolName, raw string, isError bool) string {
	if isError {
		return truncateForPrompt(raw, maxPromptErroredToolResultChars)
	}
	if toolName == "browser_fetch" {
		if compacted, ok := compactBrowserFetchForPrompt(raw); ok {
			return compacted
		}
	}
	return truncateForPrompt(raw, maxPromptToolResultChars)
}

func compactBrowserFetchForPrompt(raw string) (string, bool) {
	var payload struct {
		RequestedURL string `json:"requested_url,omitempty"`
		URL          string `json:"url,omitempty"`
		Title        string `json:"title,omitempty"`
		Text         string `json:"text,omitempty"`
		PartialError string `json:"partial_error,omitempty"`
		PageCount    int    `json:"page_count,omitempty"`
		SourceKind   string `json:"source_kind,omitempty"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", false
	}
	if payload.Text == "" || utf8.RuneCountInString(payload.Text) <= maxPromptBrowserFetchTextChars {
		return raw, true
	}

	originalChars := utf8.RuneCountInString(payload.Text)
	payload.Text = truncateMiddle(payload.Text, maxPromptBrowserFetchTextChars)
	out := map[string]any{
		"requested_url":       payload.RequestedURL,
		"url":                 payload.URL,
		"title":               payload.Title,
		"text":                payload.Text,
		"partial_error":       payload.PartialError,
		"page_count":          payload.PageCount,
		"source_kind":         payload.SourceKind,
		"truncated":           true,
		"original_text_chars": originalChars,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return truncateForPrompt(raw, maxPromptToolResultChars), true
	}
	return string(data), true
}

func truncateForPrompt(s string, maxChars int) string {
	if maxChars <= 0 || utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	return truncateMiddle(s, maxChars)
}

func truncateMiddle(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	marker := "\n\n[...truncated for prompt...]\n\n"
	markerLen := len([]rune(marker))
	if maxChars <= markerLen {
		return string(runes[:maxChars])
	}
	keep := maxChars - markerLen
	head := keep * 3 / 4
	tail := keep - head
	return string(runes[:head]) + marker + string(runes[len(runes)-tail:])
}
