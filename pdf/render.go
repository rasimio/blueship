package pdf

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// RenderMarkdown converts a markdown document into a PDF and returns
// the raw bytes. The pipeline is:
//
//  1. goldmark renders markdown → HTML (GFM tables, autolinks,
//     strikethrough enabled).
//  2. A print-styled wrapper template (system fonts, A4 margins,
//     code-block formatting) sandwiches the HTML.
//  3. chromedp loads the document via a data: URL and calls
//     Page.printToPDF to extract bytes.
//
// Headless Chrome is the same browser the in-module browser_fetch
// tool drives, so we don't add a runtime dependency that wasn't
// already there. title goes into the HTML <title> + the PDF
// document title metadata; pass "" to fall back to "Document".
func RenderMarkdown(ctx context.Context, title, md string) ([]byte, error) {
	if strings.TrimSpace(md) == "" {
		return nil, fmt.Errorf("pdf: empty markdown")
	}
	if title == "" {
		title = "Document"
	}

	// 1. Markdown → HTML body.
	mdToHTML := goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.Footnote),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
	)
	var bodyBuf bytes.Buffer
	if err := mdToHTML.Convert([]byte(md), &bodyBuf); err != nil {
		return nil, fmt.Errorf("pdf: markdown → html: %w", err)
	}

	// 2. Wrap in a print-friendly HTML doc. CSS targets the print
	// media so user-agent quirks don't bleed into pagination.
	doc := buildPrintDoc(html.EscapeString(title), bodyBuf.String())

	// 3. chromedp navigate + printToPDF. data: URL avoids spinning
	// up a temp HTTP server just to hand the page to Chrome.
	dataURL := "data:text/html;charset=utf-8;base64," + base64.StdEncoding.EncodeToString([]byte(doc))

	browserCtx, cancel := newBrowserContext(ctx)
	defer cancel()

	var out []byte
	err := chromedp.Run(browserCtx,
		chromedp.Navigate(dataURL),
		// Brief settle so web fonts (loaded via @import in the
		// template) finish before snapshot.
		chromedp.Sleep(150*time.Millisecond),
		chromedp.ActionFunc(func(ctx context.Context) error {
			data, _, err := page.PrintToPDF().
				WithPrintBackground(true).
				WithDisplayHeaderFooter(false).
				WithMarginTop(0.5).
				WithMarginBottom(0.5).
				WithMarginLeft(0.5).
				WithMarginRight(0.5).
				WithPaperWidth(8.27).  // A4 inches
				WithPaperHeight(11.69).
				Do(ctx)
			if err != nil {
				return err
			}
			out = data
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("pdf: chromedp render: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pdf: empty render result")
	}
	return out, nil
}

// newBrowserContext spins up a headless Chrome the same way the
// browser_fetch tool does. Honours BROWSER_BIN so an explicit
// `/Applications/Google Chrome…` path on macStudio is picked up.
func newBrowserContext(parent context.Context) (context.Context, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(1024, 1400),
		chromedp.Flag("lang", "en-US"),
	)
	if bin := os.Getenv("BROWSER_BIN"); bin != "" {
		opts = append(opts, chromedp.ExecPath(bin))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(parent, opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	// 25-second timeout — render shouldn't take more than a couple
	// seconds, longer means a stuck Chrome we want to abort.
	hardCtx, hardCancel := context.WithTimeout(browserCtx, 25*time.Second)
	cancel := func() {
		hardCancel()
		browserCancel()
		allocCancel()
	}
	return hardCtx, cancel
}

// buildPrintDoc returns a self-contained HTML document with print
// styles applied. Cyrillic-safe system font stack (Inter / sans-serif
// fallback) + a monospaced code block style + amber accent on links
// matching the cabinet's visual identity.
func buildPrintDoc(escapedTitle, body string) string {
	const tmpl = `<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<title>%s</title>
<style>
:root { color-scheme: light; }
* { box-sizing: border-box; }
body {
    font-family: -apple-system, BlinkMacSystemFont, "Inter", "Segoe UI",
                 "Helvetica Neue", Arial, "PT Sans", "Liberation Sans",
                 system-ui, sans-serif;
    font-size: 11pt;
    line-height: 1.55;
    color: #1a1a1a;
    margin: 0;
    padding: 0;
}
h1, h2, h3, h4 { font-weight: 600; line-height: 1.25; margin: 1.4em 0 0.6em; }
h1 { font-size: 22pt; border-bottom: 1px solid #e0d8c8; padding-bottom: 0.3em; }
h2 { font-size: 16pt; }
h3 { font-size: 13pt; }
h4 { font-size: 12pt; }
p, ul, ol, blockquote { margin: 0.8em 0; }
ul, ol { padding-left: 1.6em; }
li { margin: 0.25em 0; }
blockquote { border-left: 3px solid #b08840; padding: 0.2em 0.9em; color: #555; background: #faf6ec; }
a { color: #a06b1f; text-decoration: none; border-bottom: 1px solid #d8c597; }
code { font-family: "SF Mono", Menlo, Consolas, monospace; font-size: 0.92em;
       background: #f3eddc; padding: 0.1em 0.35em; border-radius: 3px; }
pre { background: #f7f2e2; padding: 0.8em 1em; border-radius: 4px; overflow-x: auto;
      font-family: "SF Mono", Menlo, Consolas, monospace; font-size: 9.5pt; line-height: 1.45; }
pre code { background: transparent; padding: 0; }
hr { border: none; border-top: 1px solid #e0d8c8; margin: 1.6em 0; }
table { border-collapse: collapse; width: 100%%; margin: 0.9em 0; font-size: 10.5pt; }
th, td { border: 1px solid #d8d0bd; padding: 6px 10px; text-align: left; }
th { background: #f6efdd; font-weight: 600; }
img { max-width: 100%%; height: auto; }
@page { margin: 16mm; }
</style>
</head>
<body>
%s
</body>
</html>`
	return fmt.Sprintf(tmpl, escapedTitle, body)
}
