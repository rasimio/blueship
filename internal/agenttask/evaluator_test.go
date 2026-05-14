package agenttask

import (
	"encoding/json"
	"strings"
	"testing"
)

// extractURLs is the cited-URL extractor used by Gate A and Gate B' for
// cross-referencing against the fetched set. Tests cover the canonical-
// keying that lets /abs/ collapse to /pdf/ (browser.Fetch rewrites),
// rejection of `httpshttps://` artifacts, and rejection of `_`-bearing
// hostnames that mark escape leakage.
func TestExtractURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // canonical keys we expect (host+path lowercased)
	}{
		{
			name: "plain http and https",
			in:   "see https://arxiv.org/abs/2401.12345 and https://example.com/foo",
			want: []string{"arxiv.org/pdf/2401.12345.pdf", "example.com/foo"},
		},
		{
			name: "abs and pdf collapse to same key",
			in:   "https://arxiv.org/abs/2401.12345 https://arxiv.org/pdf/2401.12345.pdf",
			want: []string{"arxiv.org/pdf/2401.12345.pdf"},
		},
		{
			// Embedded double-scheme inside the URL substring itself is
			// rejected by isLooksLikeRealURL. (Leading-typo form
			// `httpshttps://...` survives because the regex anchors on
			// the inner `https://` — a known shortcoming, separate from
			// what this gate covers.)
			name: "embedded double-scheme rejected",
			in:   "https://ai.meta.comhttpshttps://other.com/path",
			want: nil,
		},
		{
			name: "underscore hostname rejected",
			in:   "see https://way_ve.ai/ for details",
			want: nil,
		},
		{
			name: "trailing punctuation stripped",
			in:   "(see https://example.com/foo.)",
			want: []string{"example.com/foo"},
		},
		{
			name: "host without dot rejected",
			in:   "see https://localhost for details",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractURLs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d urls (%v), want %d (%v)", len(got), got, len(tc.want), tc.want)
			}
			for _, key := range tc.want {
				if _, ok := got[key]; !ok {
					t.Errorf("missing %q in extracted set %v", key, got)
				}
			}
		})
	}
}

// extractURLRequirement parses Gate A's count from criteria. Largest wins
// so a criteria mixing "3 sources" and "5 citations" enforces the
// stricter 5.
func TestExtractURLRequirement(t *testing.T) {
	cases := []struct {
		criteria string
		want     int
	}{
		{"Result must contain at least 3 URL citations.", 3},
		{"Cite 5 sources minimum and at least 3 references.", 5},
		{"plain description with no count", 0},
		{"vague mention of URLs without count", 0},
		{"≥ 4 citations from peer-reviewed sources", 4},
		{"7+ links to primary sources required", 7},
	}
	for _, tc := range cases {
		t.Run(tc.criteria, func(t *testing.T) {
			if got := extractURLRequirement(tc.criteria); got != tc.want {
				t.Errorf("extractURLRequirement(%q) = %d, want %d", tc.criteria, got, tc.want)
			}
		})
	}
}

// extractFetchedURLsFromTrace pulls browser_fetch URLs out of a tool-
// trace JSON blob. Used by Gate B' to verify recheck URLs were fetched
// this iteration. Non-browser_fetch entries are ignored; nil/empty
// traces yield empty sets.
func TestExtractFetchedURLsFromTrace(t *testing.T) {
	cases := []struct {
		name  string
		trace string
		want  []string // canonical keys expected
	}{
		{
			name:  "empty trace",
			trace: "",
			want:  nil,
		},
		{
			name:  "non-fetch tool ignored",
			trace: `[{"name":"browser_search","input":"{\"url\":\"https://example.com/a\"}"}]`,
			want:  nil,
		},
		{
			name:  "single browser_fetch",
			trace: `[{"name":"browser_fetch","input":"{\"url\":\"https://example.com/a\"}"}]`,
			want:  []string{"example.com/a"},
		},
		{
			name: "multiple browser_fetch entries",
			trace: `[
				{"name":"browser_fetch","input":"{\"url\":\"https://arxiv.org/abs/2401.12345\"}"},
				{"name":"browser_search","input":"{\"q\":\"x\"}"},
				{"name":"browser_fetch","input":"{\"url\":\"https://example.com/b\"}"}
			]`,
			want: []string{"arxiv.org/pdf/2401.12345.pdf", "example.com/b"},
		},
		{
			name:  "malformed json yields empty",
			trace: `not json`,
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFetchedURLsFromTrace(json.RawMessage(tc.trace))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d (%v), want %d (%v)", len(got), got, len(tc.want), tc.want)
			}
			for _, key := range tc.want {
				if _, ok := got[key]; !ok {
					t.Errorf("missing %q in %v", key, got)
				}
			}
		})
	}
}

// scoreGroundingVerdict computes Met + RecheckURLs from the parsed
// per-claim Claims slice. The Met rule (ratio>=0.70 AND no hard-category
// ungrounded) is load-bearing; the RecheckURLs hand-off (attribution +
// architectural ungrounded → recheck) is what makes Gate B' actionable.
func TestScoreGroundingVerdict(t *testing.T) {
	cases := []struct {
		name              string
		claims            []ClaimGrounding
		wantMet           bool
		wantGrounded      int
		wantUngrounded    int
		wantRecheckCount  int
		wantReasonHas     string
	}{
		{
			name: "all grounded — pass",
			claims: []ClaimGrounding{
				{Status: "grounded", ClaimType: "attribution"},
				{Status: "grounded", ClaimType: "numerical"},
				{Status: "grounded", ClaimType: "framing"},
			},
			wantMet:          true,
			wantGrounded:     3,
			wantUngrounded:   0,
			wantRecheckCount: 0,
			wantReasonHas:    "100%",
		},
		{
			name: "ungrounded attribution — fail with recheck",
			claims: []ClaimGrounding{
				{Status: "grounded", ClaimType: "framing"},
				{Status: "grounded", ClaimType: "numerical"},
				{Status: "ungrounded", ClaimType: "attribution",
					Claim: "Zhang et al. proposed X", SupportingDocURL: "https://example.com/paper"},
				{Status: "grounded", ClaimType: "numerical"},
			},
			wantMet:          false,
			wantGrounded:     3,
			wantUngrounded:   1,
			wantRecheckCount: 1,
			wantReasonHas:    "attribution",
		},
		{
			// Framing-ungrounded is not a hard category — it doesn't
			// trip the hasHardUngrounded guard. The ratio still applies,
			// so we need enough grounded claims for ratio>=0.70 to hold.
			// 4/5 = 80% qualifies.
			name: "ungrounded framing alone — pass when ratio holds",
			claims: []ClaimGrounding{
				{Status: "grounded", ClaimType: "attribution"},
				{Status: "grounded", ClaimType: "numerical"},
				{Status: "grounded", ClaimType: "quote"},
				{Status: "grounded", ClaimType: "architectural"},
				{Status: "ungrounded", ClaimType: "framing"},
			},
			wantMet:          true,
			wantGrounded:     4,
			wantUngrounded:   1,
			wantRecheckCount: 0,
		},
		{
			name: "below ratio — fail without hard category",
			claims: []ClaimGrounding{
				{Status: "grounded", ClaimType: "framing"},
				{Status: "partial", ClaimType: "numerical"},
				{Status: "partial", ClaimType: "framing"},
				{Status: "partial", ClaimType: "framing"},
			},
			wantMet:          false,
			wantGrounded:     1,
			wantUngrounded:   0,
			wantRecheckCount: 0,
			wantReasonHas:    "threshold",
		},
		{
			name:           "no claims — vacuously pass",
			claims:         nil,
			wantMet:        true,
			wantGrounded:   0,
			wantUngrounded: 0,
		},
		{
			name: "duplicate recheck URLs deduplicated",
			claims: []ClaimGrounding{
				{Status: "ungrounded", ClaimType: "attribution", SupportingDocURL: "https://x.com/y"},
				{Status: "ungrounded", ClaimType: "architectural", SupportingDocURL: "https://x.com/y"},
				{Status: "grounded", ClaimType: "framing"},
				{Status: "grounded", ClaimType: "numerical"},
				{Status: "grounded", ClaimType: "quote"},
			},
			wantMet:          false,
			wantGrounded:     3,
			wantUngrounded:   2,
			wantRecheckCount: 1, // same URL, deduped
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := scoreGroundingVerdict(GroundingVerdict{Claims: tc.claims})
			if v.Met != tc.wantMet {
				t.Errorf("Met = %v, want %v (reason: %q)", v.Met, tc.wantMet, v.Reason)
			}
			if v.GroundedCount != tc.wantGrounded {
				t.Errorf("GroundedCount = %d, want %d", v.GroundedCount, tc.wantGrounded)
			}
			if v.UngroundedCount != tc.wantUngrounded {
				t.Errorf("UngroundedCount = %d, want %d", v.UngroundedCount, tc.wantUngrounded)
			}
			if len(v.RecheckURLs) != tc.wantRecheckCount {
				t.Errorf("RecheckURLs count = %d, want %d (%v)",
					len(v.RecheckURLs), tc.wantRecheckCount, v.RecheckURLs)
			}
			if tc.wantReasonHas != "" && !strings.Contains(v.Reason, tc.wantReasonHas) {
				t.Errorf("Reason %q does not contain %q", v.Reason, tc.wantReasonHas)
			}
		})
	}
}

// parseGroundingResponse strips prose and pulls a Claims array out of
// the auditor's JSON. Must tolerate ```json fences, trailing
// explanations, and surface a clean error on truly malformed output.
func TestParseGroundingResponse(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantClaims  int
		wantErr     bool
	}{
		{
			name: "clean json",
			raw: `{"claims": [
				{"claim": "x", "claim_type": "attribution", "status": "grounded"}
			]}`,
			wantClaims: 1,
		},
		{
			name: "prose before json",
			raw: `Here is my verdict:
{"claims": [{"claim": "x", "claim_type": "attribution", "status": "grounded"}]}
Hope that helps.`,
			wantClaims: 1,
		},
		{
			name:    "no json object",
			raw:     `I don't know what to say.`,
			wantErr: true,
		},
		{
			name:       "empty claims array",
			raw:        `{"claims": []}`,
			wantClaims: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseGroundingResponse(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(v.Claims) != tc.wantClaims {
				t.Errorf("got %d claims, want %d", len(v.Claims), tc.wantClaims)
			}
		})
	}
}

// injectFeedback merges acceptance_feedback into a progress JSON blob.
// Preserves other keys; tolerates nil / empty / malformed progress.
func TestInjectFeedback(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		reason string
		want   string // substring expected in output
	}{
		{
			name:   "empty progress",
			in:     "",
			reason: "missing URLs",
			want:   `"acceptance_feedback":"missing URLs"`,
		},
		{
			name:   "preserves existing keys",
			in:     `{"phase":"iteration_5","summary":"foo"}`,
			reason: "no fetches",
			want:   `"phase":"iteration_5"`,
		},
		{
			name:   "empty reason no-op",
			in:     `{"phase":"x"}`,
			reason: "",
			want:   `"phase":"x"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := injectFeedback(json.RawMessage(tc.in), tc.reason)
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("expected %q in %q", tc.want, string(out))
			}
		})
	}
}
