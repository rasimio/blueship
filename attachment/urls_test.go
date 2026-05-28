package attachment

import (
	"reflect"
	"testing"
)

// TestExtractURLs locks the contract the gateway depends on: markdown
// link syntax, parenthetical asides, trailing punctuation, and
// duplicate pastes all normalise to a stable, de-duplicated slice in
// source order. Each case names the user-visible behaviour it guards
// so a future refactor can read why the assertion looks the way it does.
func TestExtractURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "plain url",
			in:   "see https://example.com for details",
			want: []string{"https://example.com"},
		},
		{
			name: "markdown link strips parens",
			in:   "check [the docs](https://example.com/x) please",
			want: []string{"https://example.com/x"},
		},
		{
			name: "parenthetical aside",
			in:   "the page (https://example.com/y) was great",
			want: []string{"https://example.com/y"},
		},
		{
			name: "trailing period stripped",
			in:   "found at https://example.com/page.",
			want: []string{"https://example.com/page"},
		},
		{
			name: "trailing comma stripped",
			in:   "https://example.com/a, https://example.com/b",
			want: []string{"https://example.com/a", "https://example.com/b"},
		},
		{
			name: "duplicate deduped, order preserved",
			in:   "https://a.com and https://b.com, then https://a.com again",
			want: []string{"https://a.com", "https://b.com"},
		},
		{
			name: "http and https both kept",
			in:   "old http://example.com plus new https://example.com/v2",
			want: []string{"http://example.com", "https://example.com/v2"},
		},
		{
			name: "no urls in plain prose",
			in:   "just a normal sentence with no links",
			want: nil,
		},
		{
			name: "no-dot host rejected",
			in:   "http://localhost:8200/api",
			want: nil,
		},
		{
			name: "too short rejected",
			in:   "http://x",
			want: nil,
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractURLs(tc.in)
			// Treat a zero-length result and a nil slice as equivalent —
			// the gateway only iterates the slice, so the distinction is
			// not observable downstream and pinning it would just make
			// the assertion fragile to implementation choice.
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExtractURLs(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}
