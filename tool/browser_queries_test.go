package tool

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rasimio/blueship/internal/webaccess/browser"
)

func TestBrowserQueriesFindDistantAttributesWithinSharedBudget(t *testing.T) {
	body := "Price 100 RSD\n" + strings.Repeat("я", 5356) + "\nDimenzije: 225 x 120 x 42 mm\n" + strings.Repeat("x", 2000) + "\nPower: 550 W"
	source := &browser.FetchResult{Text: body}
	out := excerptBrowserFetchQueries(source, 0, 900, "", []string{"Price", "Dimenzije", "Power", "missing"})
	if !strings.Contains(out.Text, "Price 100") || out.Total != utf8.RuneCountInString(body) || len(out.QueryExcerpts) != 4 {
		t.Fatal(out)
	}
	count := utf8.RuneCountInString(out.Text)
	for i, want := range []string{"Price 100", "225 x 120 x 42", "550 W", ""} {
		q := out.QueryExcerpts[i]
		count += utf8.RuneCountInString(q.Text)
		if want == "" {
			if q.Found || q.Text != "" || q.MatchCount != 0 {
				t.Fatal("miss invented evidence", q)
			}
		} else if !q.Found || !strings.Contains(q.Text, want) {
			t.Fatal("attribute window missing", q)
		}
	}
	if count > 900 || len(browserQueryReadOffsets(out)) != 3 || source.Text != body {
		t.Fatal("source altered or response budget exceeded", count)
	}
	// Offset applies to every query; earlier evidence is not falsely rediscovered.
	later := excerptBrowserFetchQueries(source, 6000, 900, "", []string{"Price", "Power"})
	if later.QueryExcerpts[0].Found || !later.QueryExcerpts[1].Found {
		t.Fatal(later)
	}
}

func TestBrowserQueriesValidationAndLegacyWindow(t *testing.T) {
	for _, queries := range [][]string{{""}, {"a", "A"}, {strings.Repeat("я", 161)}, {"1", "2", "3", "4", "5", "6", "7"}} {
		if validateBrowserQueries("", queries) == nil {
			t.Fatal("invalid queries accepted", queries)
		}
	}
	if validateBrowserQueries("one", []string{"two"}) == nil || validateBrowserQueries("", []string{"one", "two"}) != nil {
		t.Fatal("query modes not enforced")
	}
	source := &browser.FetchResult{Text: "first second third"}
	before := excerptBrowserFetch(source, 2, 8, "second")
	after := excerptBrowserFetchQueries(source, 2, 8, "second", nil)
	if before.Text != after.Text || before.Offset != after.Offset || before.QueryMatchCount != after.QueryMatchCount {
		t.Fatal("single-query behavior changed", before, after)
	}
}

func TestBrowserQueryMissPreservesOverviewWithoutExpandingBudget(t *testing.T) {
	source := &browser.FetchResult{Text: strings.Repeat("x", 507) + "microATX supported" + strings.Repeat("x", 7000) + "GPU 400 mm"}
	out := excerptBrowserFetchQueries(source, 0, 1500, "", []string{"GPU", "unmatched label"})
	if !strings.Contains(out.Text, "microATX supported") || !strings.Contains(out.QueryExcerpts[0].Text, "GPU 400 mm") || out.QueryExcerpts[1].Found {
		t.Fatal("miss discarded already saved overview", out)
	}
	used := utf8.RuneCountInString(out.Text)
	for _, part := range out.QueryExcerpts {
		used += utf8.RuneCountInString(part.Text)
	}
	offsets := browserQueryReadOffsets(out)
	if used > 1500 || len(offsets) != 2 || offsets[0] != 0 {
		t.Fatal("budget or recorded coverage incorrect", used, offsets)
	}
}
