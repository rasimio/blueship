package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestPageLinksResolveBoundAndFilter(t *testing.T) {
	body := `<base href="/catalog/"><a href="item">Product</a><a href="item">Duplicate</a><a href="javascript:alert(1)">Script</a><div hidden><a href="hidden">Hidden</a></div><a href="https://other.example/spec">Specs</a>`
	for i := 0; i < 1200; i++ {
		body += fmt.Sprintf(`<a href="p%d">%s</a>`, i, strings.Repeat("я", 200))
	}
	links := extractPageLinks([]byte(body), "https://example.com/redirected/page")
	if len(links) != 1000 || links[0].URL != "https://example.com/catalog/item" || links[1].URL != "https://other.example/spec" || len([]rune(links[2].Text)) != 160 {
		t.Fatalf("unexpected links: %+v", links[:min(len(links), 3)])
	}
}

func TestRenderedPageLinks(t *testing.T) {
	if os.Getenv("BLUESHIP_TEST_CHROME") != "1" {
		t.Skip("isolated Chrome opt-in")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/product">Product</a><a href="/product">Duplicate</a><a href="javascript:alert(1)">Script</a><a style="display:none" href="/hidden">Hidden</a></body></html>`)
	}))
	defer server.Close()
	pool := browserPool{slots: make(chan struct{}, 1)}
	defer pool.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tab, release, err := pool.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var links []PageLink
	if err := chromedp.Run(tab, chromedp.Navigate(server.URL), chromedp.Evaluate(pageLinksJS, &links)); err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].URL != server.URL+"/product" || links[0].Text != "Product" {
		t.Fatalf("unexpected rendered links: %+v", links)
	}
}

func TestPageLinksPrioritizeContentOverNavigation(t *testing.T) {
	links := extractPageLinks([]byte(`<nav><a href="/menu">Menu</a></nav><main><a href="/product">Product</a></main><footer><a href="/terms">Terms</a></footer>`), "https://example.com")
	if len(links) != 3 || links[0].URL != "https://example.com/product" {
		t.Fatal(links)
	}
}

func TestHeadingLinksPrecedeUnstructuredMenus(t *testing.T) {
	body := `<html><body><div>`
	for i := 0; i < 200; i++ {
		body += fmt.Sprintf(`<a href="/menu/%d">Category %d</a>`, i, i)
	}
	body += `</div><a href="/product">Thumbnail</a><a href="/product"><h3>Available product</h3></a><h2><a href="/article">Research article</a></h2><nav><a href="/heading-menu"><h3>Menu heading</h3></a></nav></body></html>`
	check := func(t *testing.T, links []PageLink, base string) {
		t.Helper()
		if len(links) != 203 || links[0].URL != base+"/product" || links[0].Text != "Available product" || links[1].URL != base+"/article" || links[2].URL != base+"/menu/0" {
			t.Fatalf("content headings lost behind menus: %+v", links[:min(3, len(links))])
		}
	}
	t.Run("static", func(t *testing.T) {
		check(t, extractPageLinks([]byte(body), "https://example.com"), "https://example.com")
	})
	t.Run("rendered", func(t *testing.T) {
		if os.Getenv("BLUESHIP_TEST_CHROME") != "1" {
			t.Skip("isolated Chrome opt-in")
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		defer server.Close()
		pool := browserPool{slots: make(chan struct{}, 1)}
		defer pool.shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		tab, release, err := pool.acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		var links []PageLink
		if err := chromedp.Run(tab, navigateDocument(server.URL), chromedp.Evaluate(pageLinksJS, &links)); err != nil {
			t.Fatal(err)
		}
		check(t, links, server.URL)
	})
}
