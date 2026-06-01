// Package pdf is the public-import companion to internal/browser's
// PDF extraction. Hosts that aren't part of the blueship module need
// to decode PDF bytes too — most often the host's attachment-read tool
// reading a user-uploaded file from the CDN. Keeping the body here
// rather than in internal/ lets that import path stay clean while
// internal/browser keeps its existing in-module callers.
//
// ExtractPDFText is byte-for-byte the same routine internal/browser
// uses (it now delegates here). Cp1251 mojibake repair + NUL/C0
// scrubbing also live in this package so the daemon's Telegram
// pipeline and the host's tool calls produce identical outputs.
package pdf

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"golang.org/x/text/encoding/charmap"
)

const (
	// MaxBytes caps body size we'll buffer in memory before decode.
	// 25 MiB covers nearly every paper / report we'll see; larger
	// PDFs are usually image-heavy scans the text extractor can't
	// help with anyway.
	MaxBytes = 25 * 1024 * 1024

	// TextHeadCap is the cap on the text ExtractText returns. PDFs
	// often run 30-100 pages — handing the full extraction back to
	// the model burns tokens for material it didn't ask for. Callers
	// can re-fetch a page range when they need more.
	TextHeadCap = 60_000
)

// ExtractText decodes a PDF byte buffer into plain text. Pages are
// joined with `--- Page N ---` markers so a downstream reader can
// cite by page. Returns the text (capped at TextHeadCap), the page
// count, and any extraction error.
//
// Output is sanitised: cp1251 mojibake (a frequent failure of
// ledongthuc/pdf on Cyrillic-WinAnsi fonts) is repaired, and the
// NUL byte + non-tab/newline C0 controls — which PostgreSQL jsonb
// rejects with "unsupported Unicode escape sequence" — are stripped.
func ExtractText(data []byte) (string, int, error) {
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
			fmt.Fprintf(&sb, "\n\n--- Page %d (extract failed: %v) ---\n\n", i, err)
			continue
		}
		fmt.Fprintf(&sb, "\n\n--- Page %d ---\n\n", i)
		sb.WriteString(text)
		if sb.Len() >= TextHeadCap {
			break
		}
	}
	out := sb.String()
	if len(out) > TextHeadCap {
		out = out[:TextHeadCap] + "\n\n[truncated — pull more via a more specific URL or page-range request]"
	}
	out = FixCp1251Mojibake(out)
	out = StripUnsupportedChars(out)
	return out, pageCount, nil
}

// FixCp1251Mojibake repairs a frequent failure of ledongthuc/pdf:
// when a PDF embeds a Cyrillic font with a Windows-1251 single-byte
// encoding and no /ToUnicode CMap, the library emits each byte as a
// matching latin-1 rune. The resulting "text" is valid UTF-8 but
// reads as gibberish — every Russian letter shows as a Western
// accented character (П → Ï, р → ð, etc.). Detection counts upper-
// half latin-1 runes and the share they make of the buffer; texts
// already in proper UTF-8 or mostly-ASCII strings are returned
// untouched.
func FixCp1251Mojibake(s string) string {
	if s == "" {
		return s
	}
	suspect, total := 0, 0
	for _, r := range s {
		if r >= 0x0400 && r <= 0x04FF {
			return s
		}
		if r > 0xFF {
			return s
		}
		total++
		if r >= 0x80 {
			suspect++
		}
	}
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

// StripUnsupportedChars removes characters that downstream storage
// (PostgreSQL jsonb) refuses. The hard offender is the NUL byte —
// ledongthuc/pdf occasionally emits 0x00 when a font's encoding map
// has gaps, and a literal NUL lands in the JSON envelope that jsonb
// rejects with 22P05. Tab / line feed / carriage return are
// preserved so layout-relevant whitespace survives.
func StripUnsupportedChars(s string) string {
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

// Ensure utf8 stays referenced even if a future caller pulls it in
// transitively; the encoding/charmap import already locks x/text.
var _ = utf8.ValidString
