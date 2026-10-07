package browser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const htmlMaxBytes = 4 << 20

// Keep direct HTTP connections warm. Resolve and validate at dial time so a
// redirect or DNS change cannot turn this fast path into an internal request.
var staticHTMLClient = &http.Client{
	Transport: &http.Transport{Proxy: nil, MaxIdleConns: 32, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 4, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 3 * time.Second, DialContext: publicHTTPDial},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return validateFetchURL(req.URL.String())
	},
}

func publicHTTPDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if !publicHTTPAddress(ip.IP) {
			return nil, fmt.Errorf("refusing non-public HTTP destination")
		}
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	var last error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("HTTP destination has no addresses")
	}
	return nil, last
}

func publicHTTPAddress(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

// fetchStaticHTML accepts only substantial article/main content. Thin shells,
// catalogs without semantic content, bot challenges and failures fall back to
// rendering. An explicit wait request bypasses this optimization in Fetch.
func fetchStaticHTML(ctx context.Context, target string, client *http.Client) (*FetchResult, bool) {
	if err := validateFetchURL(target); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	response, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false
	}
	ct := strings.ToLower(response.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/html") && !strings.Contains(ct, "application/xhtml+xml") {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, htmlMaxBytes+1))
	if err != nil || len(body) > htmlMaxBytes {
		return nil, false
	}
	title, text, ok := extractStaticArticle(body)
	if !ok {
		return nil, false
	}
	runes := []rune(text)
	truncated := len(runes) > 500000
	if truncated {
		text = string(runes[:500000])
	}
	links := extractPageLinks(body, response.Request.URL.String())
	return &FetchResult{LinksLimitReached: len(links) >= 1000, RequestedURL: target, URL: response.Request.URL.String(), Title: title, Text: text, SourceKind: "html", SourceTruncated: truncated, Links: links}, true
}

func extractStaticArticle(body []byte) (string, string, bool) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", "", false
	}
	var title string
	var candidates []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "title" {
				title = staticNodeText(n)
			}
			if n.Data == "article" || n.Data == "main" {
				candidates = append(candidates, n)
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	lowerTitle := strings.ToLower(title)
	for _, marker := range []string{"just a moment", "access denied", "captcha", "verify you are human", "checking your browser"} {
		if strings.Contains(lowerTitle, marker) {
			return "", "", false
		}
	}
	best := ""
	for _, candidate := range candidates {
		text := staticNodeText(candidate)
		if len(text) > len(best) {
			best = text
		}
	}
	if utf8.RuneCountInString(best) < 800 {
		return "", "", false
	}
	lower := strings.ToLower(best)
	for _, marker := range []string{"enable javascript to continue", "verify you are human", "complete the captcha", "checking your browser"} {
		if strings.Contains(lower, marker) {
			return "", "", false
		}
	}
	return strings.TrimSpace(title), best, true
}

func staticNodeText(root *html.Node) string {
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "nav", "footer", "header", "svg", "form", "template":
				return
			}
			for _, a := range n.Attr {
				if a.Key == "hidden" || (a.Key == "aria-hidden" && a.Val == "true") {
					return
				}
				if a.Key == "style" {
					style := strings.ReplaceAll(strings.ToLower(a.Val), " ", "")
					if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") {
						return
					}
				}
			}
		}
		if n.Type == html.TextNode {
			out.WriteString(n.Data)
			out.WriteByte(' ')
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "p", "div", "li", "h1", "h2", "h3", "tr", "br":
				out.WriteByte('\n')
			}
		}
	}
	walk(root)
	lines := strings.Split(out.String(), "\n")
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			clean = append(clean, line)
		}
	}
	return strings.Join(clean, "\n")
}
