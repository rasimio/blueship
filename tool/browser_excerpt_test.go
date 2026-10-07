package tool

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rasimio/blueship/internal/webaccess/browser"
)

func TestBrowserExcerptFindsEvidenceBeyondOldCutoff(t *testing.T) {
	full := strings.Repeat("вступление ", 3000) + "NOWAIT fails immediately. SKIP LOCKED skips locked rows." + strings.Repeat(" конец", 1000)
	source := &browser.FetchResult{Text: full, URL: "https://example.com/docs", FromCache: true}
	first := excerptBrowserFetch(source, 0, 0, "")
	if strings.Contains(first.Text, "NOWAIT") || first.Next == nil || first.Total != utf8.RuneCountInString(full) {
		t.Fatal("window metadata lost", first)
	}
	next := excerptBrowserFetch(source, 0, 1000, "nowait")
	if next.QueryFound == nil || !*next.QueryFound || !strings.Contains(next.Text, "NOWAIT fails immediately") || !utf8.ValidString(next.Text) || !next.FromCache {
		t.Fatal(next)
	}
	if source.Text != full {
		t.Fatal("excerpt destroyed source evidence")
	}
	missing := excerptBrowserFetch(source, 0, 100, "absent")
	if missing.QueryFound == nil || *missing.QueryFound {
		t.Fatal("invented match", missing)
	}
	end := excerptBrowserFetch(source, first.Total+1, 100, "")
	if end.Text != "" || end.Next != nil {
		t.Fatal("invalid end window", end)
	}
}

func TestBrowserLinksPaginationPreservesEveryTargetAndSource(t *testing.T) {
	source := &browser.FetchResult{Text: "price evidence"}
	for i := 0; i < 100; i++ {
		source.Links = append(source.Links, browser.PageLink{URL: fmt.Sprintf("https://example.com/%d", i), Text: strings.Repeat("я", 160)})
	}
	seen := map[string]bool{}
	offset := 0
	for {
		out := excerptBrowserFetch(source, 0, 0, "")
		excerptBrowserLinks(&out, source.Links, offset)
		if len(out.Links) > 12 || out.LinksTotal != 100 || out.Text != "price evidence" {
			t.Fatal(out)
		}
		for _, link := range out.Links {
			if seen[link.URL] {
				t.Fatal("duplicate link")
			}
			seen[link.URL] = true
		}
		if out.NextLinksOffset == nil {
			break
		}
		if *out.NextLinksOffset <= offset {
			t.Fatal("paging did not advance")
		}
		offset = *out.NextLinksOffset
	}
	if len(seen) != 100 || len(source.Links) != 100 {
		t.Fatal("lost observed targets")
	}
	out := excerptBrowserFetch(source, 0, 0, "")
	out.Links[0].Text = "changed"
	if source.Links[0].Text == "changed" {
		t.Fatal("mutated cached source")
	}
	excerptBrowserLinks(&out, source.Links, 1000)
	if len(out.Links) != 0 || out.NextLinksOffset != nil {
		t.Fatal("invalid end offset")
	}
}

func TestBrowserLinkQueryFindsLateObservedProduct(t *testing.T) {
	links := make([]browser.PageLink, 0, 201)
	for i := 0; i < 200; i++ {
		links = append(links, browser.PageLink{URL: fmt.Sprintf("https://example.com/filter/%d", i), Text: "Menu"})
	}
	links = append(links, browser.PageLink{URL: "https://example.com/item/actual-id", Text: "Память Kingston 32 GB"})
	matches := filterBrowserLinks(links, "KINGSTON")
	var page browserExcerpt
	excerptBrowserLinks(&page, matches, 0)
	if page.LinksTotal != 1 || len(page.Links) != 1 || page.Links[0].URL != "https://example.com/item/actual-id" {
		t.Fatal(page)
	}
	if len(filterBrowserLinks(links, "missing-product")) != 0 || len(filterBrowserLinks(links, "  ")) != 201 || len(links) != 201 {
		t.Fatal("invented link or lost source")
	}
}

func TestBrowserQueryLocationsSkipRepeatedNavigation(t *testing.T) {
	full := "Меню: ХАРАКТЕРИСТИКИ\n" + strings.Repeat("предложение Характеристики\n", 12) + "Характеристики: высота 159 мм"
	source := &browser.FetchResult{Text: full}
	out := excerptBrowserFetch(source, 0, 30, "характеристики")
	if out.QueryMatchCount != 14 || len(out.QueryMatchOffsets) != 8 {
		t.Fatal(out)
	}
	for _, offset := range out.QueryMatchOffsets {
		if !strings.HasPrefix(strings.ToLower(string([]rune(full)[offset:])), "характеристики") {
			t.Fatal("not a character offset", offset)
		}
	}
	last := out.QueryMatchOffsets[7]
	detail := excerptBrowserFetch(source, last, 100, "характеристики")
	if detail.QueryMatchCount != 1 || !strings.Contains(detail.Text, "159 мм") || source.Text != full {
		t.Fatal(detail)
	}
	missing := excerptBrowserFetch(source, last, 100, "отсутствует")
	if missing.QueryMatchCount != 0 || len(missing.QueryMatchOffsets) != 0 || *missing.QueryFound {
		t.Fatal(missing)
	}
}
