package tool

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rasimio/blueship/internal/webaccess/browser"
)

// The tool response is bounded independently from the saved source. Repeated
// windows can use the task cache instead of downloading the page again.
type browserExcerpt struct {
	browser.FetchResult
	QueryExcerpts     []browserQueryExcerpt `json:"query_excerpts,omitempty"`
	LinksOffset       int                   `json:"links_offset"`
	LinksTotal        int                   `json:"links_total"`
	NextLinksOffset   *int                  `json:"next_links_offset,omitempty"`
	Offset            int                   `json:"offset_chars"`
	Total             int                   `json:"total_chars"`
	Next              *int                  `json:"next_offset_chars,omitempty"`
	QueryFound        *bool                 `json:"query_found,omitempty"`
	QueryMatchCount   int                   `json:"query_match_count,omitempty"`
	QueryMatchOffsets []int                 `json:"query_match_offsets,omitempty"`
}

func excerptBrowserFetch(source *browser.FetchResult, offset, limit int, query string) browserExcerpt {
	runes := []rune(source.Text)
	if limit <= 0 {
		limit = 10000
	}
	limit = min(limit, 20000)
	offset = max(0, min(offset, len(runes)))
	out := browserExcerpt{FetchResult: *source, Total: len(runes)}
	if query != "" {
		// Rune-preserving lowercase keeps offsets correct for Unicode text.
		lower := make([]rune, len(runes))
		for i, r := range runes {
			lower[i] = unicode.ToLower(r)
		}
		needle := []rune(query)
		for i, r := range needle {
			needle[i] = unicode.ToLower(r)
		}
		remaining, term := string(lower[offset:]), string(needle)
		position := offset
		for {
			index := strings.Index(remaining, term)
			if index < 0 {
				break
			}
			position += utf8.RuneCountInString(remaining[:index])
			out.QueryMatchCount++
			// Preserve the first seven locations and the last one. Catalogs
			// repeat headings in navigation before their actual attributes.
			if len(out.QueryMatchOffsets) < 8 {
				out.QueryMatchOffsets = append(out.QueryMatchOffsets, position)
			} else {
				out.QueryMatchOffsets[7] = position
			}
			remaining = remaining[index+len(term):]
			position += len(needle)
		}
		found := out.QueryMatchCount > 0
		out.QueryFound = &found
		if found {
			offset = max(0, out.QueryMatchOffsets[0]-limit/4)
		}
	}
	end := min(len(runes), offset+limit)
	out.Offset = offset
	out.Text = string(runes[offset:end])
	if end < len(runes) {
		out.Next = &end
	}
	excerptBrowserLinks(&out, source.Links, 0)
	return out
}

// Return a bounded page of navigation targets; keep the complete observed
// list in the source cache. Unicode counts bound model-visible content.
func excerptBrowserLinks(out *browserExcerpt, links []browser.PageLink, offset int) {
	offset = max(0, min(offset, len(links)))
	out.LinksOffset, out.LinksTotal = offset, len(links)
	out.NextLinksOffset = nil
	end, chars := offset, 0
	for end < len(links) && end-offset < 12 {
		size := utf8.RuneCountInString(links[end].URL) + utf8.RuneCountInString(links[end].Text)
		if end > offset && chars+size > 3000 {
			break
		}
		chars += size
		end++
	}
	out.Links = append([]browser.PageLink(nil), links[offset:end]...)
	if end < len(links) {
		out.NextLinksOffset = &end
	}
}

// Navigation search is independent of the prose window. It only filters
// observed URLs/labels and never synthesizes destination addresses.
func filterBrowserLinks(links []browser.PageLink, query string) []browser.PageLink {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return links
	}
	matches := make([]browser.PageLink, 0)
	for _, link := range links {
		if strings.Contains(strings.ToLower(link.Text), query) || strings.Contains(strings.ToLower(link.URL), query) {
			matches = append(matches, link)
		}
	}
	return matches
}

// Multiple exact phrase windows share one response budget and one saved source.
// A miss has no unrelated header text masquerading as the requested evidence.
type browserQueryExcerpt struct {
	Query        string `json:"query"`
	Text         string `json:"text"`
	Offset       int    `json:"offset_chars"`
	Next         *int   `json:"next_offset_chars,omitempty"`
	Found        bool   `json:"query_found"`
	MatchCount   int    `json:"query_match_count"`
	MatchOffsets []int  `json:"query_match_offsets,omitempty"`
}

func validateBrowserQueries(query string, queries []string) error {
	if len(queries) == 0 {
		return nil
	}
	if query != "" || len(queries) > 6 {
		return fmt.Errorf("use query or up to six queries, not both")
	}
	seen := map[string]bool{}
	for _, term := range queries {
		key := strings.ToLower(strings.TrimSpace(term))
		if key == "" || utf8.RuneCountInString(term) > 160 || seen[key] {
			return fmt.Errorf("queries require distinct nonempty phrases up to 160 characters")
		}
		seen[key] = true
	}
	return nil
}

func excerptBrowserFetchQueries(source *browser.FetchResult, offset, limit int, query string, queries []string) browserExcerpt {
	if len(queries) == 0 {
		return excerptBrowserFetch(source, offset, limit, query)
	}
	if limit <= 0 {
		limit = 10000
	}
	limit = min(limit, 20000)
	out := browserExcerpt{FetchResult: *source, Total: utf8.RuneCountInString(source.Text)}
	out.Text = ""
	out.Offset = max(0, min(offset, out.Total))
	for i, term := range queries {
		// Rejecting too-small budgets at the tool boundary prevents a zero
		// per-window limit from falling back to the single-window default.
		budget := limit / len(queries)
		if i < limit%len(queries) {
			budget++
		}
		part := excerptBrowserFetch(source, offset, max(1, budget), term)
		if budget == 0 {
			part.Text = ""
		}
		window := browserQueryExcerpt{Query: term, Offset: part.Offset, Found: part.QueryFound != nil && *part.QueryFound, MatchCount: part.QueryMatchCount, MatchOffsets: part.QueryMatchOffsets}
		if window.Found {
			window.Text, window.Next = part.Text, part.Next
		}
		out.QueryExcerpts = append(out.QueryExcerpts, window)
		if i == 0 {
			out.Offset = part.Offset
		}
	}
	// Missed phrases leave unused budget. Preserve the page overview in that
	// space: terminology may differ, and exact-query windows can otherwise
	// hide model identity or compatibility right at the start of the source.
	used := 0
	for _, part := range out.QueryExcerpts {
		used += utf8.RuneCountInString(part.Text)
	}
	if remaining := limit - used; remaining > 0 {
		preview := excerptBrowserFetch(source, offset, remaining, "")
		out.Text, out.Offset, out.Next = preview.Text, preview.Offset, preview.Next
	}
	excerptBrowserLinks(&out, source.Links, 0)
	return out
}

func browserQueryReadOffsets(excerpt browserExcerpt) []int {
	if len(excerpt.QueryExcerpts) == 0 {
		return nil
	}
	var offsets []int
	seen := map[int]bool{}
	add := func(offset int) {
		if !seen[offset] {
			offsets = append(offsets, offset)
			seen[offset] = true
		}
	}
	if excerpt.Text != "" {
		add(excerpt.Offset)
	}
	for _, part := range excerpt.QueryExcerpts {
		if part.Found && part.Text != "" {
			add(part.Offset)
		}
	}
	return offsets
}
