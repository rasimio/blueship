package attachment

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// DocxTextHeadCap bounds the plain text ExtractDocxText returns. A .docx
// can unzip to a very large document.xml; past ~60 KB of prose the model
// already has more than enough and the rest only burns context. Mirrors
// browser.PDFTextHeadCap so PDF and DOCX attachments feel the same.
const DocxTextHeadCap = 60_000

// docxXMLReadCap bounds how many bytes of the unzipped word/document.xml we
// buffer, so a malicious "zip bomb" .docx (a few KB on the wire that inflates
// to gigabytes) can't exhaust memory. 16 MiB of XML markup is far more than
// any real report's body, and we stop appending text at DocxTextHeadCap long
// before that anyway.
const docxXMLReadCap = 16 << 20

// mimeDocx is the canonical MIME for a Word .docx (OOXML wordprocessing).
const mimeDocx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// ErrNotDocx is returned when the bytes are not a readable .docx — not a ZIP,
// missing the word/document.xml part, or containing no extractable text.
// Callers inline a short notice instead of failing the turn.
var ErrNotDocx = errors.New("attachment: not a readable .docx")

// ExtractDocxText pulls the visible text out of a Word .docx (OOXML) byte
// buffer. A .docx is a ZIP whose word/document.xml holds the body; the text
// lives in <w:t> runs, with <w:p> ending a paragraph, <w:tab> a tab, and
// <w:br>/<w:cr> a line break. We stream the XML namespace-agnostically (match
// on local element names) and ignore everything else — styles, numbering, run
// properties. Returns the text capped at DocxTextHeadCap.
//
// Exported so every transport (Telegram document handler, cabinet /chat)
// shares one decoder, exactly as browser.ExtractPDFText is shared for PDFs.
// Anthropic has no native .docx content block, so extracting to text on our
// side is the only path that lets the model read a Word document at all.
func ExtractDocxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", ErrNotDocx
	}
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return "", ErrNotDocx
	}
	rc, err := doc.Open()
	if err != nil {
		return "", ErrNotDocx
	}
	defer rc.Close()

	dec := xml.NewDecoder(io.LimitReader(rc, docxXMLReadCap))
	var sb strings.Builder
	inText := false // true only between <w:t> … </w:t>, so we never pick up
	// the whitespace XML editors sprinkle between markup elements.
	for {
		tok, terr := dec.Token()
		if terr != nil {
			// io.EOF is the clean end; a malformed tail still keeps the
			// text decoded so far rather than discarding a good extraction.
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				sb.WriteByte('\t')
			case "br", "cr":
				sb.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				// A paragraph break is the structural newline that turns a
				// stream of runs back into readable lines.
				sb.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		}
		if sb.Len() >= DocxTextHeadCap {
			break
		}
	}

	out := stripControlBytes(collapseBlankRuns(sb.String()))
	out = strings.TrimSpace(out)
	if len(out) > DocxTextHeadCap {
		out = out[:DocxTextHeadCap] + "\n\n[truncated]"
	}
	if out == "" {
		// A valid docx with no <w:t> text (image-only, or all form fields) —
		// signal "nothing to read" so the caller inlines a notice instead of
		// an empty block that reads as a successful extraction.
		return "", ErrNotDocx
	}
	return out, nil
}

// collapseBlankRuns trims trailing whitespace per line and folds runs of
// blank lines (left by empty <w:p> paragraphs) down to a single one, so the
// extracted prose reads naturally instead of double-spaced.
func collapseBlankRuns(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// stripControlBytes removes the NUL and other C0 control bytes that
// PostgreSQL jsonb refuses (a literal 0x00 trips "unsupported Unicode escape
// sequence", 22P05). Tab / newline / carriage-return survive — they carry
// layout. Operates byte-wise: UTF-8 multibyte runes are all >= 0x80, so this
// only ever drops the ASCII control range and never splits a rune.
func stripControlBytes(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0 || (c < 0x20 && c != '\t' && c != '\n' && c != '\r') {
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
