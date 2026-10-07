package browser

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type htmlRoundTripper func(*http.Request) (*http.Response, error)

func TestStaticArticlePreservesLateEvidence(t *testing.T) {
	body := "<html><title>Long reference</title><main>" + strings.Repeat("Introduction and context. ", 2000) + "NOWAIT fails immediately." + "</main></html>"
	_, text, ok := extractStaticArticle([]byte(body))
	if !ok || len(text) <= 10000 || !strings.Contains(text, "NOWAIT fails immediately.") {
		t.Fatal("long source silently truncated", len(text), ok)
	}
}

func (f htmlRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStaticHTMLKeepsArticleAndSkipsHiddenNoise(t *testing.T) {
	prose := strings.Repeat("A verified paragraph about the research subject. ", 30)
	body := `<html><head><title>Research &amp; evidence</title><script>SECRET_SCRIPT</script></head><body><nav>MENU</nav><main><h1>Research</h1><p>` + prose + `</p><div hidden>HIDDEN</div><p style="display: none">HIDDEN_STYLE</p><script>EVIL</script></main><footer>FOOTER</footer></body></html>`
	title, text, ok := extractStaticArticle([]byte(body))
	if !ok || title != "Research & evidence" || !strings.Contains(text, prose[:100]) {
		t.Fatal(title, text, ok)
	}
	for _, noise := range []string{"SECRET_SCRIPT", "MENU", "HIDDEN", "EVIL", "FOOTER"} {
		if strings.Contains(text, noise) {
			t.Fatal("included hidden noise", noise)
		}
	}
}

func TestStaticHTMLFallsBackForShellAndChallenge(t *testing.T) {
	for name, body := range map[string]string{
		"shell":       `<main><div id="app">Loading…</div></main>`,
		"script_only": `<main><script>` + strings.Repeat("not visible ", 300) + `</script></main>`,
		"challenge":   `<title>Just a moment</title><main>` + strings.Repeat("Verify you are human ", 100) + `</main>`,
		"navigation":  `<nav>` + strings.Repeat("menu ", 500) + `</nav>`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := extractStaticArticle([]byte(body)); ok {
				t.Fatal("shell accepted as research evidence")
			}
		})
	}
}

func TestStaticHTMLHTTPAndDestinationGuards(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: htmlRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("User-Agent") == "" {
			t.Fatal("missing user agent")
		}
		return &http.Response{StatusCode: 200, Request: req, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(`<title>Article</title><article>` + strings.Repeat("Confirmed content. ", 100) + `</article>`))}, nil
	})}
	res, ok := fetchStaticHTML(context.Background(), "https://example.com/article", client)
	if !ok || res.URL != "https://example.com/article" || res.SourceKind != "html" {
		t.Fatal(res, ok)
	}
	for _, target := range []string{"http://127.0.0.1/", "file:///etc/passwd", "http://localhost/"} {
		if _, ok := fetchStaticHTML(context.Background(), target, client); ok {
			t.Fatal("internal destination accepted")
		}
	}
	if calls != 1 {
		t.Fatal("guard ran after request", calls)
	}
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fc00::1", "0.0.0.0"} {
		if publicHTTPAddress(net.ParseIP(ip)) {
			t.Fatal("private dial accepted", ip)
		}
	}
	redirect, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	if staticHTMLClient.CheckRedirect(redirect, nil) == nil {
		t.Fatal("private redirect accepted")
	}
}

func BenchmarkStaticArticleExtraction(b *testing.B) {
	body := []byte(`<title>Article</title><main>` + strings.Repeat(`<p>Confirmed research content with source facts.</p>`, 200) + `</main>`)
	b.ReportAllocs()
	for b.Loop() {
		_, _, ok := extractStaticArticle(body)
		if !ok {
			b.Fatal("fixture rejected")
		}
	}
}
