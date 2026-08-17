package agent

import (
	"encoding/json"
	"sort"
	"strconv"
	"unicode/utf8"

	bs "github.com/rasimio/blueship/internal/core"
)

const (
	// List-shaped tools (calendar, recall, notes) routinely produce more than
	// a few thousand runes, and truncateMiddle excises the centre — a week of
	// calendar events lost the middle day outright. The ceiling is generous
	// because it costs nothing until a tool actually returns that much, and
	// tool results never enter durable history: DialogMessagesForAPI strips
	// every tool block, so they live only in this turn's scratchpad. Errors
	// keep the smaller budget: a long one is a stack dump or an HTML error
	// page, never signal.
	maxPromptToolResultChars        = 40000
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
	if compacted, ok := compactJSONObjectForPrompt(s, maxChars); ok {
		return compacted
	}
	return truncateMiddle(s, maxChars)
}

// compactJSONObjectForPrompt keeps every scalar field of a JSON object and
// spends what is left of the budget on the long ones, rather than cutting the
// object by position.
//
// Position is the wrong axis for a JSON object. Go marshals maps with sorted
// keys, so which fields survive a middle cut is decided by their names: for a
// task status, `acceptance_criteria` and `description` are long and sort first,
// so they fill the head, `status` and `title` sort last and hold the tail, and
// `iteration` and `max_iterations` — the two integers the question was about —
// sit in the middle and vanish. The reader is then handed a record that looks
// complete and is missing exactly the field it was asked for, with nothing to
// say so. A model in that position does not report a gap it cannot see; it
// answers with a plausible number, which is indistinguishable from lying.
//
// Scalars are cheap — the ones that disappeared here cost 27 runes together —
// so the budget is better spent guaranteeing all of them than on more prose.
func compactJSONObjectForPrompt(s string, maxChars int) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil || len(obj) == 0 {
		return "", false
	}

	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	const longFieldThreshold = 200
	kept := make(map[string]json.RawMessage, len(obj))
	var long []string
	used := 0
	for _, k := range keys {
		v := obj[k]
		n := utf8.RuneCountInString(string(v))
		if n <= longFieldThreshold {
			kept[k] = v
			used += len(k) + n + 4 // "key":value,
			continue
		}
		long = append(long, k)
	}
	// If the scalars alone do not fit there is nothing clever left to do, and a
	// positional cut is at least honest about being one.
	if used >= maxChars {
		return "", false
	}

	budget := maxChars - used
	share := budget
	if len(long) > 0 {
		share = budget / len(long)
	}
	for _, k := range long {
		raw := string(obj[k])
		if share <= 0 {
			kept[k] = json.RawMessage(`"[опущено, ` + strconv.Itoa(utf8.RuneCountInString(raw)) + ` симв.]"`)
			continue
		}
		clipped := truncateMiddle(raw, share)
		enc, err := json.Marshal(clipped + " [обрезано]")
		if err != nil {
			return "", false
		}
		kept[k] = json.RawMessage(enc)
	}

	out, err := json.Marshal(kept)
	if err != nil {
		return "", false
	}
	// A projection that came out bigger than the positional cut helps nobody.
	if utf8.RuneCountInString(string(out)) > maxChars*2 {
		return "", false
	}
	return string(out), true
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
