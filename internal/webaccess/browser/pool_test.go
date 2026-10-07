package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestBrowserPoolReusesProcessWithoutCookiesOrStorage(t *testing.T) {
	if os.Getenv("BLUESHIP_TEST_CHROME") != "1" {
		t.Skip("set BLUESHIP_TEST_CHROME=1 for isolated Chrome integration")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body>Isolated browser test</body></html>`))
	}))
	defer server.Close()
	pool := browserPool{slots: make(chan struct{}, 1)}
	defer pool.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first, closeFirst, err := pool.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	browser := chromedp.FromContext(first).Browser
	if err := chromedp.Run(first, chromedp.Navigate(server.URL), chromedp.Evaluate(`document.cookie="private=one"; localStorage.setItem("private","one")`, nil)); err != nil {
		closeFirst()
		t.Fatal(err)
	}
	closeFirst()
	second, closeSecond, err := pool.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSecond()
	if chromedp.FromContext(second).Browser != browser {
		t.Fatal("Chrome process was not reused")
	}
	var cookies string
	var stored any
	if err := chromedp.Run(second, chromedp.Navigate(server.URL), chromedp.Evaluate(`document.cookie`, &cookies), chromedp.Evaluate(`localStorage.getItem("private")`, &stored)); err != nil {
		t.Fatal(err)
	}
	if cookies != "" || stored != nil {
		t.Fatal("private browsing data leaked", cookies, stored)
	}
	waiting, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if _, cleanup, err := pool.acquire(waiting); err == nil {
		cleanup()
		t.Fatal("pool exceeded context bound")
	}
	closeSecond() // cleanup is idempotent
}

func TestDocumentNavigationDoesNotWaitForUnrelatedSubresource(t *testing.T) {
	if os.Getenv("BLUESHIP_TEST_CHROME") != "1" {
		t.Skip("isolated Chrome opt-in")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/product", http.StatusFound)
			return
		}
		if r.URL.Path == "/slow-image" {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`<html><body><main>Verified product price 123</main><img src="/slow-image"></body></html>`))
	}))
	defer server.Close()
	pool := browserPool{slots: make(chan struct{}, 1)}
	defer pool.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tab, release, err := pool.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	bounded, end := context.WithTimeout(tab, 3*time.Second)
	defer end()
	var text, location string
	if err := chromedp.Run(bounded, navigateDocument(server.URL), chromedp.Location(&location), chromedp.Evaluate(`document.body.innerText`, &text)); err != nil {
		t.Fatal("subresource blocked readable document", err)
	}
	if text != "Verified product price 123" || location != server.URL+"/product" {
		t.Fatal(text)
	}
}

// A readable SERP must not consume the engine timeout waiting for trackers or images.
func TestSearchExtractionWithPendingSubresource(t *testing.T) {
	if os.Getenv("BLUESHIP_TEST_CHROME") != "1" {
		t.Skip("isolated Chrome opt-in")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pending" {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`<html><body><div class="result"><a class="result__a" href="https://example.org/product"><h3>Product</h3></a><a class="result__snippet">Price checked</a></div><img src="/pending"></body></html>`))
	}))
	defer server.Close()
	pool := browserPool{slots: make(chan struct{}, 1)}
	defer pool.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, engine := range []string{"google", "ddg"} {
		t.Run(engine, func(t *testing.T) {
			tab, release, err := pool.acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			bounded, end := context.WithTimeout(tab, 3*time.Second)
			defer end()
			script := googleExtractJS(8)
			if engine == "ddg" {
				script = ddgExtractJS(8)
			}
			var raw json.RawMessage
			if err := chromedp.Run(bounded, navigateDocument(server.URL), chromedp.Evaluate(script, &raw)); err != nil {
				t.Fatal(err)
			}
			var items []SearchResultItem
			if err := json.Unmarshal(raw, &items); err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].URL != "https://example.org/product" || items[0].Title != "Product" {
				t.Fatalf("lost readable search result: %s", raw)
			}
		})
	}
}
