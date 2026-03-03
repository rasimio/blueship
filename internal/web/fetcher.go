package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// HTTPFetcher implements bs.WebFetcher by downloading web pages and extracting text.
type HTTPFetcher struct {
	httpClient *http.Client
}

// NewHTTPFetcher creates a new web page fetcher.
func NewHTTPFetcher() *HTTPFetcher {
	return &HTTPFetcher{
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

// Fetch downloads a URL and returns its text content, truncated to maxChars.
func (f *HTTPFetcher) Fetch(ctx context.Context, rawURL string, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 8000
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "BlueShip/1.0")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	text := string(body)

	if strings.Contains(contentType, "text/html") {
		text = htmlToText(text)
	}

	runes := []rune(text)
	if len(runes) > maxChars {
		text = string(runes[:maxChars]) + "\n[truncated]"
	}

	return text, nil
}

func htmlToText(s string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	skip := 0

	skipTags := map[string]bool{
		"script": true, "style": true, "nav": true, "footer": true, "noscript": true,
	}

	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			return cleanText(b.String())
		case html.StartTagToken:
			tn, _ := tokenizer.TagName()
			tag := string(tn)
			if skipTags[tag] {
				skip++
			}
			if isBlockTag(tag) && b.Len() > 0 {
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			tn, _ := tokenizer.TagName()
			tag := string(tn)
			if skipTags[tag] && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				text := strings.TrimSpace(string(tokenizer.Text()))
				if text != "" {
					if b.Len() > 0 {
						b.WriteByte(' ')
					}
					b.WriteString(text)
				}
			}
		}
	}
}

func isBlockTag(tag string) bool {
	switch tag {
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6",
		"li", "br", "hr", "tr", "td", "th", "article", "section",
		"header", "main", "blockquote", "pre":
		return true
	}
	return false
}

func cleanText(s string) string {
	lines := strings.Split(s, "\n")
	var result []string
	blankCount := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			blankCount++
			if blankCount <= 1 {
				result = append(result, "")
			}
		} else {
			blankCount = 0
			result = append(result, line)
		}
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}
