package agenttask

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGraphReviewReceiptsPreserveClocksFailuresAndEffects(t *testing.T) {
	stamp := "2026-09-22T21:31:11.884388Z"
	browser := `{"observed_at":"` + stamp + `","links":[{"url":"unfinished`
	rows := []map[string]any{
		{"name": "browser_fetch", "input": `{"url":"https://example.org/spec"}`, "output": browser, "started_at": "2026-09-22T21:34:00Z", "duration_ms": 20},
		{"name": "browser_fetch", "output": "403 Forbidden", "error": true},
		{"name": "browser_fetch", "output": "read timeout", "timed_out": true},
		{"name": "browser_fetch", "output": "unknown non-JSON receipt"},
		{"name": "write_file", "output": strings.Repeat("receipt contents ", 1000)},
		{"tool": "browser_search", "output": `{"engine_used":"google","results":[{"url":"navigation only"}]}`},
	}
	raw, _ := json.Marshal(rows)
	got, err := graphReviewReceipts(raw)
	if err != nil {
		t.Fatal(err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 2, 3, 4} {
		if parsed[i]["output"] != rows[i]["output"] {
			t.Fatalf("lost failure/action receipt %d", i)
		}
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(parsed[0]["output"].(string)), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["observed_at"] != stamp || receipt["content_omitted"] != true || len(receipt) != 2 || parsed[0]["started_at"] != rows[0]["started_at"] || parsed[0]["input"] != rows[0]["input"] {
		t.Fatal(receipt, parsed[0])
	}
	if strings.Contains(parsed[5]["output"].(string), "navigation only") || !strings.Contains(parsed[5]["output"].(string), "google") {
		t.Fatal(parsed[5])
	}
	if _, err := graphReviewReceipts(json.RawMessage(`invalid`)); err == nil {
		t.Fatal("malformed trace accepted")
	}
}

func TestBrowserReviewMetadataNeverInventsPartialObservation(t *testing.T) {
	for _, body := range []string{`{"observed_at":"2026-09-22T`, `{"observed_at":"not a date"}`, `not JSON`} {
		if got := browserReviewMetadata(body); len(got) != 0 {
			t.Fatal(got)
		}
	}
}
