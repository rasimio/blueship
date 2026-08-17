package agenttask

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fetchRow builds one browser_fetch row: a document whose supporting
// sentence sits at `spanAt` chars in, the way a README's real content sits
// past its badges and its table of contents.
func fetchRow(url, title, span string, spanAt, size int) ToolOutput {
	if spanAt+len(span) > size {
		size = spanAt + len(span)
	}
	body := strings.Repeat("x", spanAt) + span
	body += strings.Repeat("y", size-len(body))
	return ToolOutput{
		ToolName:  "browser_fetch",
		ToolInput: json.RawMessage(fmt.Sprintf(`{"url":%q}`, url)),
		Output:    body,
		Metadata: map[string]any{
			"title":         title,
			"final_url":     url,
			"requested_url": url,
		},
	}
}

// The production failure, reproduced at its real dimensions.
//
// Task acc49aa2 re-fetched the same pages every iteration; by iteration 18
// the store held 205 rows for 96 URLs. The budget was divided across ROWS,
// so each document was cut to 250000/205 = 1219 chars, and Gate C then
// reported six "hard ungrounded" claims whose supporting sentences sat at
// offsets 1398–9579 of documents it had been handed. The report was fine;
// the gate could not see past the page headers, and it failed the task and
// exhausted an 18-iteration budget on that basis.
func TestGroundingAuditSeesSpansDeepInRefetchedDocs(t *testing.T) {
	const (
		mobileApp = "close the connection with a mobile app before connecting to the robot"
		sportMode = `subscribing "lf/sportmodestate" topic, where "lf" represents low frequency`
		ipConfig  = "Set the computer IP to 192.168.123.100, netmask 255.255.255.0"
	)
	cited := []ToolOutput{
		fetchRow("https://github.com/abizovnuralem/go2_ros2_sdk", "go2_ros2_sdk", mobileApp, 2140, 9998),
		fetchRow("https://github.com/unitreerobotics/unitree_ros2", "unitree_ros2", sportMode, 9367, 10000),
		fetchRow("https://github.com/jizhang-cmu/autonomy_stack_go2", "autonomy_stack", ipConfig, 9579, 10000),
	}
	// 20 re-fetches of each cited page, plus the long tail of pages opened
	// once and never cited — 205 rows over 96 URLs, as in production.
	var docs []ToolOutput
	for range 20 {
		docs = append(docs, cited...)
	}
	for i := 0; len(docs) < 205; i++ {
		docs = append(docs, fetchRow(
			fmt.Sprintf("https://example.test/other/%d", i), "other", "irrelevant", 10, 30_000))
	}

	report := "Отчёт. Источники: https://github.com/abizovnuralem/go2_ros2_sdk, " +
		"https://github.com/unitreerobotics/unitree_ros2 и https://github.com/jizhang-cmu/autonomy_stack_go2."

	msg, stats := buildGroundingUserMessage(report, docs)

	if stats.Rows != len(docs) {
		t.Fatalf("stats.Rows = %d, want %d", stats.Rows, len(docs))
	}
	if stats.Cited != 3 {
		t.Fatalf("stats.Cited = %d, want the 3 documents the report links", stats.Cited)
	}
	if stats.Unique >= stats.Rows {
		t.Fatalf("no dedup happened: %d unique of %d rows", stats.Unique, stats.Rows)
	}
	// The whole point: every sentence the auditor called missing is now in
	// front of it.
	for _, span := range []string{mobileApp, sportMode, ipConfig} {
		if !strings.Contains(msg, span) {
			t.Errorf("audit prompt does not contain the supporting span %q — Gate C would report it ungrounded", span)
		}
	}
	// A window that small is a page header, and a header can only fail a
	// claim, never support one.
	if stats.MinWindow < groundingMinWindow {
		t.Errorf("smallest document window = %d chars, want >= %d", stats.MinWindow, groundingMinWindow)
	}
	// Whatever did not fit has to be declared, or the auditor reads its own
	// blind spot as fabrication.
	if stats.Omitted > 0 && !strings.Contains(msg, "did not fit the audit budget") {
		t.Errorf("%d documents were dropped without telling the auditor", stats.Omitted)
	}
}

// Dedup keeps the newest fetch: a page re-read later in the task is the
// text the researcher actually worked from.
func TestGroundingDedupKeepsTheNewestFetch(t *testing.T) {
	url := "https://example.test/page"
	docs := []ToolOutput{
		fetchRow(url, "page", "STALE BODY", 10, 2_000),
		fetchRow(url, "page", "FRESH BODY", 10, 2_000),
	}
	msg, stats := buildGroundingUserMessage("no citations here", docs)
	if stats.Unique != 1 {
		t.Fatalf("stats.Unique = %d, want 1", stats.Unique)
	}
	if strings.Contains(msg, "STALE BODY") || !strings.Contains(msg, "FRESH BODY") {
		t.Error("dedup kept the older fetch")
	}
}

// When the budget genuinely binds, the documents the report cites are the
// ones worth spending it on — an uncited page cannot be what a citation
// rests on.
func TestGroundingBudgetGoesToCitedDocsFirst(t *testing.T) {
	const span = "the sentence that supports the claim"
	var docs []ToolOutput
	for i := range 40 {
		docs = append(docs, fetchRow(
			fmt.Sprintf("https://example.test/filler/%d", i), "filler", "filler text", 10, groundingPerDocCap))
	}
	docs = append(docs, fetchRow("https://example.test/source", "source", span, 20_000, groundingPerDocCap))

	msg, stats := buildGroundingUserMessage("See https://example.test/source for details.", docs)

	if stats.Omitted == 0 {
		t.Fatal("40 full-size documents fit the budget; the test no longer exercises a binding budget")
	}
	if !strings.Contains(msg, span) {
		t.Error("the cited document lost its budget to uncited filler")
	}
	if first := strings.Index(msg, "example.test/source"); first < 0 ||
		first > strings.Index(msg, "example.test/filler") {
		t.Error("cited document is not audited first")
	}
}

func TestNormalizeDocURLMatchesCitationsToRows(t *testing.T) {
	same := []struct{ report, row string }{
		{"https://example.test/doc/", "https://example.test/doc"},
		{"https://example.test/doc.", "http://example.test/doc"},
		{"https://www.example.test/doc#section", "https://example.test/doc"},
		{"(https://example.test/doc)", "https://example.test/doc"},
	}
	for _, c := range same {
		got := reportCitedURLs(c.report)
		if !got[normalizeDocURL(c.row)] {
			t.Errorf("citation %q does not match fetched row %q", c.report, c.row)
		}
	}
	if reportCitedURLs("see https://example.test/a")[normalizeDocURL("https://example.test/b")] {
		t.Error("citation matching is too loose: unrelated URLs match")
	}
}
