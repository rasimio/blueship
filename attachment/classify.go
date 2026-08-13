// Package attachment is the shared classifier for files arriving on
// any transport (Telegram document, cabinet upload, future channels).
// Both the Telegram gateway path and the cabinet HTTP path route every
// file through Kind so the same UTF-8 sniff / magic-byte check decides
// "is this an image / a PDF / a text doc / not supported" — without a
// hardcoded extension whitelist that drifts as new languages appear.
//
// The per-kind size constants are the single source of truth for
// transport limits; gateways and HTTP handlers read them so the only
// place to retune a cap is here.
package attachment

// Per-kind size caps. Bytes the daemon will accept for one attachment;
// everything above gets rejected by the inbound transport before it
// reaches the LLM. Aligned across Telegram + cabinet so a user gets
// the same answer regardless of which channel they sent through.
const (
	// MaxImageBytes — JPEG/PNG/etc. Anthropic's vision API will accept
	// larger but the gain in resolution past ~12 MiB is negligible
	// and we don't want a hostile client filling 200 MB into one turn.
	MaxImageBytes int64 = 12 << 20

	// MaxTextBytes — source files, configs, logs, JSON, etc. 12 MiB
	// covers anything sane; a 50 MB log is a paste-the-relevant-bit
	// situation, not an "inline the whole thing into context".
	MaxTextBytes int64 = 12 << 20

	// MaxPDFBytes — bumped vs the text/image caps because a single
	// scanned PDF (book chapter, multi-page invoice scan) often runs
	// 15-25 MB. The PDF text extractor still tops out at ~600 KB of
	// resulting prompt text via PDFTextHeadCap, so a 25 MB scan
	// doesn't translate to 25 MB of prompt — only of bytes-on-the-wire.
	MaxPDFBytes int64 = 25 << 20

	// MaxDocxBytes — a Word .docx is a ZIP, so embedded images can push a
	// real report past the text cap; 25 MB matches the PDF allowance. The
	// extracted prose is still bounded by DocxTextHeadCap, so a fat .docx
	// costs bytes-on-the-wire, not prompt tokens.
	MaxDocxBytes int64 = 25 << 20

	// MaxXlsxBytes — an Excel .xlsx is a ZIP like docx; real exports
	// with embedded styles run tens of MB. The extracted markdown is
	// bounded by XlsxTextHeadCap, so the cap is bytes-on-the-wire only.
	MaxXlsxBytes int64 = 25 << 20

	// MaxAnyBytes is the upper bound across kinds — useful as the
	// download / multipart-read cap on transports that don't know the
	// kind until they've sniffed the bytes. Per-kind enforcement
	// happens after Kind() returns.
	MaxAnyBytes int64 = MaxPDFBytes
)

// Kind classifies an attachment into "image" | "pdf" | "text". An
// empty string means "I don't recognise this — reject". The decision
// is signature-first: we only trust the bytes, never an unverified
// MIME string from the upload (an SVG arrives as image/svg+xml but
// Anthropic vision rejects SVG; a JPEG with octet-stream MIME is
// still a JPEG). Falling through to a UTF-8 sniff covers any source
// file in any language, with or without an extension. The mime /
// name arguments are kept only for diagnostics and to break the rare
// ambiguity (PDF magic missing in a truncated file, etc).
func Kind(mime, name string, data []byte) string {
	mimeLower := lowerASCII(trimSpace(mime))
	ext := lowerExt(name)

	// Image: signature only. Anthropic vision accepts JPEG/PNG/GIF/
	// WebP — every format detected here. We intentionally do NOT
	// trust `image/*` MIME alone (SVG would slip through; corrupt
	// uploads with a lying MIME would fail downstream at the API).
	if isImageSignature(data) {
		return "image"
	}
	// PDF: %PDF magic is reliable. MIME / extension are fallbacks
	// for the rare empty-or-truncated upload where bytes alone
	// won't tell us anything useful.
	if isPDFSignature(data) || mimeLower == "application/pdf" || ext == ".pdf" {
		return "pdf"
	}
	// OOXML (ZIP-based Office formats): classify on the declared type
	// (ext / MIME) gated by the ZIP magic, so a renamed PDF can't slip
	// in, but a correctly-named file whose MIME arrives as octet-stream
	// (Telegram does this) still lands here. The extractors re-validate
	// by actually opening the archive. PPTX and legacy binary .doc/.xls
	// fall through to "" (unsupported) — the transport inlines a notice
	// rather than going silent on the user.
	if isZipSignature(data) && (ext == ".docx" || mimeLower == mimeDocx) {
		return "docx"
	}
	if isZipSignature(data) && (ext == ".xlsx" || mimeLower == mimeXlsx) {
		return "xlsx"
	}
	// Text: any buffer DetectTextEncoding can read — UTF-8, UTF-16,
	// or a single-byte code page. Catches every source file we'd want
	// to inline (incl. SVG, which is XML text — we'll send the markup
	// as a fenced code block and let the model reason about it rather
	// than try to render it), and a .txt exported by a Windows editor,
	// which is not UTF-8 at all. DecodeText does the conversion.
	if DetectTextEncoding(data) != "" {
		return "text"
	}
	return ""
}

// MaxBytesForKind returns the per-kind size cap. An empty / unknown
// kind returns 0 so the caller's `size > cap` check naturally rejects
// it; the classifier returning "" already means "unsupported" in any
// case.
func MaxBytesForKind(kind string) int64 {
	switch kind {
	case "image":
		return MaxImageBytes
	case "pdf":
		return MaxPDFBytes
	case "docx":
		return MaxDocxBytes
	case "xlsx":
		return MaxXlsxBytes
	case "text":
		return MaxTextBytes
	}
	return 0
}

// MimeForImage returns the canonical content-type for an image payload
// derived from its signature bytes, ignoring whatever MIME the
// uploader claimed. Anthropic's vision API rejects requests where the
// declared media_type disagrees with the bytes, so we always rebuild
// the field from what we actually see. Returns "" when the bytes are
// not a recognised image — callers should only invoke this after Kind
// already returned "image".
func MimeForImage(data []byte) string {
	if len(data) < 12 {
		return ""
	}
	switch {
	case data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47:
		return "image/png"
	case data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46:
		return "image/gif"
	case data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50:
		return "image/webp"
	}
	return ""
}

// isImageSignature peeks at well-known image magic numbers. We
// recognise exactly the formats Anthropic's vision API ingests today
// (JPEG, PNG, GIF, WebP) — and nothing else. HEIC is deliberately
// excluded: even though modern iPhones default to it, the vision API
// won't accept it, so detecting HEIC here would only let us pass
// bytes that fail downstream at the LLM call. Modern iOS Safari
// transcodes HEIC → JPEG on multipart upload, so the practical user
// impact is minimal.
func isImageSignature(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	switch {
	case data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF: // JPEG
		return true
	case data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47: // PNG
		return true
	case data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46: // GIF
		return true
	case data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 && // "RIFF"
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50: // "WEBP"
		return true
	}
	return false
}

// isPDFSignature checks the "%PDF" magic at offset 0. The PDF spec
// allows up to 1 KiB of leading junk before the header, but in
// practice every PDF we'll see has %PDF at offset 0 — and accepting
// junk-prefixed PDFs would also let "%PDF" appear in arbitrary
// archives and misclassify them.
func isPDFSignature(data []byte) bool {
	return len(data) >= 4 && data[0] == '%' && data[1] == 'P' && data[2] == 'D' && data[3] == 'F'
}

// isZipSignature checks for the ZIP local-file-header magic "PK\x03\x04" (and
// the empty-archive "PK\x05\x06" / spanned "PK\x07\x08" variants for
// completeness). Every OOXML file — .docx / .xlsx / .pptx — is a ZIP, so this
// is the guard that stops a renamed non-archive from being treated as one;
// the ext / MIME check decides which OOXML kind it actually is.
func isZipSignature(data []byte) bool {
	return len(data) >= 4 && data[0] == 'P' && data[1] == 'K' &&
		((data[2] == 0x03 && data[3] == 0x04) ||
			(data[2] == 0x05 && data[3] == 0x06) ||
			(data[2] == 0x07 && data[3] == 0x08))
}

// --- tiny string helpers (avoid pulling in `strings` for two trims) ---

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func lowerExt(name string) string {
	dot := -1
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			dot = i
			break
		}
		if name[i] == '/' || name[i] == '\\' {
			break
		}
	}
	if dot < 0 {
		return ""
	}
	return lowerASCII(name[dot:])
}
