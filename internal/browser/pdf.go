package browser

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"
	"golang.org/x/text/encoding/charmap"
)

// PDF text extraction lives next to browser.Fetch because PDFs commonly
// surface as "regular URLs" the model wants to read — arxiv abstract
// links resolve to .pdf, government docs are PDF, datasheets are PDF.
// Routing them through chromedp gets you Chrome's PDF viewer chrome,
// not the article text. Path:
//
//   1. fetchBytes(url, proxy) — HTTP GET with the same proxy chain
//      browser.Fetch uses, follows redirects, capped at PDFFetchTimeout
//      and PDFMaxBytes so a hostile server can't tar-pit us.
//   2. ExtractPDFText(bytes) — pure-Go decode via ledongthuc/pdf,
//      concatenates page text with `--- Page N ---` separators so
//      cortex can cite by page if needed.
//
// PDF detection: a URL with .pdf path or query, OR response bytes
// starting with the `%PDF-` magic. Content-Type is checked but not
// trusted alone — servers mislabel PDFs as octet-stream more often
// than you'd hope.

const (
	// PDFFetchTimeout is the total wall-clock budget for HTTP download.
	// Long enough for ~10MB on a slow link; short enough that a stuck
	// connection doesn't hang an iteration.
	PDFFetchTimeout = 30 * time.Second

	// PDFMaxBytes caps body size we'll buffer in memory. 25MB covers
	// almost every paper / report that arxiv hosts; larger PDFs are
	// usually image-heavy scans the text-only extractor can't help with.
	PDFMaxBytes = 25 * 1024 * 1024

	// PDFTextHeadCap is what ExtractPDFText returns after concatenation.
	// PDFs often run 30-100 pages — feeding the full extraction into
	// context burns tokens for material the cortex didn't ask for. The
	// caller can re-fetch with a different range or read pages directly
	// if they need more.
	PDFTextHeadCap = 60_000
)

// PDFFetchResult is the structured return value when Fetch routes
// through the PDF path. Mirrors the FetchResult fields cortex actually
// uses.
type PDFFetchResult struct {
	URL       string
	Title     string
	Text      string
	PageCount int
}

// looksLikePDFURL is a cheap pre-check: URL ends in .pdf or carries
// .pdf in the query (some CDNs serve `?file=foo.pdf`). Not authoritative
// — the server may serve PDF without a hint, or HTML with .pdf in the
// path. Used only to short-circuit chromedp; the byte-level magic check
// is the actual guard.
func looksLikePDFURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := strings.ToLower(u.Path)
	if strings.HasSuffix(path, ".pdf") {
		return true
	}
	q := strings.ToLower(u.RawQuery)
	return strings.Contains(q, ".pdf")
}

// fetchBytes runs an HTTP GET via the proxy chain with a hard size cap.
// Returns body bytes, the resolved final URL (after redirects), and
// the Content-Type the server reported.
func fetchBytes(ctx context.Context, target, proxy string) ([]byte, string, string, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}
	if proxy != "" && proxy != "-" {
		pURL, err := url.Parse(proxy)
		if err == nil {
			tr.Proxy = http.ProxyURL(pURL)
		}
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   PDFFetchTimeout,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/pdf,text/html,*/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, resp.Request.URL.String(), resp.Header.Get("Content-Type"),
			fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, PDFMaxBytes))
	if err != nil {
		return nil, resp.Request.URL.String(), resp.Header.Get("Content-Type"), err
	}
	return body, resp.Request.URL.String(), resp.Header.Get("Content-Type"), nil
}

// isPDFContent checks both the Content-Type header and the raw bytes
// magic number (`%PDF-`). Servers occasionally mislabel PDFs as
// `application/octet-stream` or `binary/octet-stream`, and a magic
// check costs five bytes.
func isPDFContent(contentType string, body []byte) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if strings.HasPrefix(ct, "application/pdf") || strings.HasPrefix(ct, "application/x-pdf") {
		return true
	}
	if len(body) >= 5 && bytes.Equal(body[:5], []byte("%PDF-")) {
		return true
	}
	return false
}

// ExtractPDFText decodes a PDF byte buffer into plain text. Pages are
// joined with `--- Page N ---` markers so a downstream reader can cite
// by page. Returns the text (capped at PDFTextHeadCap), the page count,
// and any extraction error.
//
// Exported so the Telegram document handler can share the same path:
// PDF attachments arrive as bytes from the Telegram file API and need
// the same decoder as PDFs found on the open web.
func ExtractPDFText(data []byte) (string, int, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", 0, fmt.Errorf("pdf.NewReader: %w", err)
	}
	pageCount := r.NumPage()
	var sb strings.Builder
	for i := 1; i <= pageCount; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			// Single bad page shouldn't kill the whole document —
			// extraction libraries trip on encrypted-but-scanned or
			// malformed pages. Note the gap in output and keep going.
			fmt.Fprintf(&sb, "\n\n--- Page %d (extract failed: %v) ---\n\n", i, err)
			continue
		}
		fmt.Fprintf(&sb, "\n\n--- Page %d ---\n\n", i)
		sb.WriteString(text)
		if sb.Len() >= PDFTextHeadCap {
			break
		}
	}
	out := sb.String()
	if len(out) > PDFTextHeadCap {
		out = out[:PDFTextHeadCap] + "\n\n[truncated — pull more via a more specific URL or page-range request]"
	}
	out = fixCp1251Mojibake(out)
	out = stripUnsupportedChars(out)
	return out, pageCount, nil
}

// stripUnsupportedChars removes characters that downstream storage
// (PostgreSQL jsonb) refuses. The hard offender is the NUL byte —
// ledongthuc/pdf occasionally emits 0x00 when a font's encoding map
// has gaps, and a literal NUL lands in the JSON envelope that
// jsonb rejects with "unsupported Unicode escape sequence" (22P05).
// We also strip the other non-tab/newline C0 controls (0x01–0x08,
// 0x0B–0x0C, 0x0E–0x1F) because they're rare in real PDF text and
// occasionally trip naive renderers; tab / line feed / carriage
// return are preserved so layout-relevant whitespace survives.
func stripUnsupportedChars(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0 || (c > 0 && c < 0x20 && c != '\t' && c != '\n' && c != '\r') {
			goto rewrite
		}
	}
	return s
rewrite:
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0 || (c > 0 && c < 0x20 && c != '\t' && c != '\n' && c != '\r') {
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

// fixCp1251Mojibake repairs a frequent failure of ledongthuc/pdf: when
// a PDF embeds a Cyrillic font with a Windows-1251 single-byte
// encoding and no /ToUnicode CMap, the library emits each byte as a
// matching latin-1 rune. The resulting "text" is valid UTF-8 but
// reads as gibberish — every Russian letter shows as a Western
// accented character (П → Ï, р → ð, etc.).
//
// Detection: count runes in U+0080-U+00FF. If a meaningful share of
// the text falls in that band and there's no actual Cyrillic
// codepoint present, treat it as latin-1-misencoded cp1251. Decode
// the runes back to single bytes, run them through the Windows-1251
// table, return the UTF-8 result. Pure-ASCII strings or strings
// already containing proper Cyrillic are returned untouched.
//
// We don't try cp1252 / KOI8-R here — cp1251 covers the vast
// majority of Russian PDFs we'll see, and adding more guesses risks
// flipping correctly-encoded Western text into garbage.
func fixCp1251Mojibake(s string) string {
	if s == "" {
		return s
	}
	suspect, total := 0, 0
	for _, r := range s {
		if r >= 0x0400 && r <= 0x04FF {
			// Real Cyrillic present — text is already Unicode-correct.
			return s
		}
		if r > 0xFF {
			// Anything outside latin-1 (other scripts, emoji, etc.)
			// means the source is multi-encoded — bail rather than
			// risk corrupting it.
			return s
		}
		total++
		if r >= 0x80 {
			suspect++
		}
	}
	// Heuristic: a Russian PDF misencoded as latin-1 has a very high
	// share of upper-half bytes (every Cyrillic letter is U+0080–
	// U+00FF). A piece of Western text with a sprinkle of accents
	// stays under ~25%. The 30% threshold catches the former cleanly
	// while leaving "café résumé" alone.
	if suspect < 3 || suspect*100/total < 30 {
		return s
	}
	raw := make([]byte, 0, len(s))
	for _, r := range s {
		raw = append(raw, byte(r))
	}
	decoded, err := charmap.Windows1251.NewDecoder().Bytes(raw)
	if err != nil {
		return s
	}
	return string(decoded)
}

// fetchPDF downloads a PDF URL and returns its extracted text. Caller
// has already determined the URL is a PDF candidate (via
// looksLikePDFURL). Routes through fetchBytes so the proxy chain is
// shared with chromedp.
func fetchPDF(ctx context.Context, target, proxy string) (*PDFFetchResult, error) {
	body, finalURL, ct, err := fetchBytes(ctx, target, proxy)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if !isPDFContent(ct, body) {
		// We thought it was a PDF (URL extension) but it isn't. Bubble
		// up so Fetch can fall back to chromedp.
		return nil, errNotPDF
	}
	text, pages, err := ExtractPDFText(body)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	title := pdfTitleHeuristic(text, finalURL)
	return &PDFFetchResult{
		URL:       finalURL,
		Title:     title,
		Text:      text,
		PageCount: pages,
	}, nil
}

// errNotPDF is the sentinel fetchPDF returns when the bytes don't look
// like a PDF after all. Callers re-route to chromedp HTML render.
var errNotPDF = errors.New("not a pdf")

// pdfTitleHeuristic picks a human-readable title from the extracted
// text — first non-empty line under ~120 chars. Falls back to the
// URL's basename. PDFs sometimes have proper /Title metadata, but
// ledongthuc/pdf doesn't expose it cleanly; the first-line heuristic
// works for academic papers (the title is on top) and most reports.
func pdfTitleHeuristic(text, fallbackURL string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--- Page") {
			continue
		}
		if len(line) > 120 {
			line = line[:120] + "…"
		}
		return line
	}
	if u, err := url.Parse(fallbackURL); err == nil {
		base := u.Path
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		return base
	}
	return fallbackURL
}
