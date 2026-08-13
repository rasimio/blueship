package attachment

import (
	"bytes"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// Text files do not arrive in one encoding. A note exported from a Windows
// editor is UTF-16 with a BOM or single-byte Windows-1251; a file from a
// modern editor is UTF-8, with or without a BOM. Everything downstream —
// the prompt, the transcript row, the jsonb column — is UTF-8, so this file
// is the single place that decides "is this text at all" and the single
// place that turns whatever the user sent into UTF-8.
//
// Signature-first still holds: nothing here trusts the declared MIME or the
// extension. The evidence is the byte pattern — BOM, NUL layout, high-byte
// density, share of control bytes.

// textProbeCap bounds how much of a buffer the encoding probe reads. 8 KiB
// is the same window `git` uses for its is-it-text check and is plenty to
// tell prose from binary; the decoders then run over the whole buffer.
const textProbeCap = 8192

// Encoding names DetectTextEncoding returns. Strings rather than an enum so
// they can go straight into a log line when a file is misread.
const (
	encUTF8    = "utf-8"
	encUTF16LE = "utf-16le"
	encUTF16BE = "utf-16be"
	encCP1251  = "windows-1251"
	encCP1252  = "windows-1252"
)

var (
	bomUTF8    = []byte{0xEF, 0xBB, 0xBF}
	bomUTF16LE = []byte{0xFF, 0xFE}
	bomUTF16BE = []byte{0xFE, 0xFF}
)

// DetectTextEncoding names the encoding of a text buffer, or "" when the
// bytes are not text in any encoding we can decode. Order matters: a BOM is
// conclusive, BOM-less UTF-16 is caught by its byte layout before the NUL
// check would reject it, UTF-8 is the common case, and single-byte legacy
// encodings are the last resort behind printability gates so arbitrary
// binary can't pass as Windows-1252.
func DetectTextEncoding(data []byte) string {
	switch {
	case bytesHavePrefix(data, bomUTF8):
		if validUTF8Head(data[len(bomUTF8):]) {
			return encUTF8
		}
		return ""
	case bytesHavePrefix(data, bomUTF16LE):
		return encUTF16LE
	case bytesHavePrefix(data, bomUTF16BE):
		return encUTF16BE
	}
	if enc := sniffBOMlessUTF16(data); enc != "" {
		return enc
	}
	if validUTF8Head(data) {
		return encUTF8
	}
	return sniffLegacy8Bit(data)
}

// DecodeText converts a text attachment's raw bytes into UTF-8 with LF line
// endings — the form every caller inlines into a prompt. ok=false means the
// bytes are not readable text, which is the caller's signal to inline the
// "can't read this" notice instead.
//
// The result is always valid UTF-8: a buffer whose head decoded cleanly can
// still carry a broken tail (a mixed-encoding log, a truncated download),
// and a lone bad byte reaching a jsonb column fails the whole write.
func DecodeText(data []byte) (string, bool) {
	switch DetectTextEncoding(data) {
	case encUTF8:
		return normalizeText(string(trimBOM(data, bomUTF8))), true
	case encUTF16LE:
		return normalizeText(decodeUTF16(trimBOM(data, bomUTF16LE), false)), true
	case encUTF16BE:
		return normalizeText(decodeUTF16(trimBOM(data, bomUTF16BE), true)), true
	case encCP1251:
		return normalizeText(decodeCharmap(charmap.Windows1251, data)), true
	case encCP1252:
		return normalizeText(decodeCharmap(charmap.Windows1252, data)), true
	}
	return "", false
}

// validUTF8Head reports whether the buffer's probe window is UTF-8 text: no
// NUL byte, valid encoding. The window is cut back to a rune boundary first
// — a Cyrillic file longer than the window ends mid-rune roughly half the
// time, and validating the raw cut used to reject it as binary.
func validUTF8Head(data []byte) bool {
	head, truncated := probeHead(data)
	if truncated {
		head = trimPartialRune(head)
	}
	if bytes.IndexByte(head, 0x00) >= 0 {
		return false
	}
	return utf8.Valid(head)
}

// sniffBOMlessUTF16 detects UTF-16 written without a BOM by the shape of the
// stream rather than by NULs alone: in text from one script, every second
// byte is the same high byte — 0x00 for Latin, 0x04 for Cyrillic, 0x03 for
// Greek. So one side of the pairs holds a couple of distinct low values
// while the other varies like ordinary text. Requires a reasonable sample so
// a short ASCII file with a few repeated characters can't trip it.
func sniffBOMlessUTF16(data []byte) string {
	head, _ := probeHead(data)
	if len(head)%2 != 0 {
		head = head[:len(head)-1]
	}
	if len(head) < 32 {
		return ""
	}
	var even, odd [256]bool
	for i := 0; i < len(head); i += 2 {
		even[head[i]] = true
		odd[head[i+1]] = true
	}
	if isUTF16HighSide(odd) && distinct(even) >= 8 {
		return encUTF16LE
	}
	if isUTF16HighSide(even) && distinct(odd) >= 8 {
		return encUTF16BE
	}
	return ""
}

// isUTF16HighSide reports whether a side of the byte pairs looks like the
// high half of UTF-16 code units for a single script: at most two distinct
// values, all inside the first few Unicode blocks (Latin, Greek, Cyrillic,
// Hebrew, Arabic — 0x00-0x07).
func isUTF16HighSide(seen [256]bool) bool {
	n := 0
	for b := range 256 {
		if !seen[b] {
			continue
		}
		if b > 0x07 {
			return false
		}
		n++
	}
	return n > 0 && n <= 2
}

func distinct(seen [256]bool) int {
	n := 0
	for b := range seen {
		if seen[b] {
			n++
		}
	}
	return n
}

// sniffLegacy8Bit decides whether a non-UTF-8 buffer is single-byte legacy
// text, and which of the two code pages we decode it as. The gates are what
// keep arbitrary binary out: a NUL, more than 1% C0 control bytes, or more
// than 5% of bytes in the 0x80-0x9F band (punctuation in both code pages,
// dense in machine data) means this is not prose.
//
// Between the two: Cyrillic prose is mostly high bytes, while Western text
// with the odd accented letter is mostly ASCII — so high-byte density picks
// Windows-1251 over Windows-1252. Other single-byte code pages (Greek,
// Hebrew, Turkish) are not detected; they decode as Windows-1252 mojibake,
// which the model can still recognise and say so about, unlike a rejection.
func sniffLegacy8Bit(data []byte) string {
	head, _ := probeHead(data)
	if len(head) == 0 {
		return ""
	}
	var ctrl, band, high int
	for _, b := range head {
		switch {
		case b == 0x00:
			return ""
		case b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != 0x0C:
			ctrl++
		case b >= 0x80 && b <= 0x9F:
			band++
		case b >= 0xA0:
			high++
		}
	}
	if ctrl*100 > len(head) || band*20 > len(head) || high == 0 {
		return ""
	}
	if high*4 > len(head) {
		return encCP1251
	}
	return encCP1252
}

// probeHead returns the leading window the sniffers read, and whether the
// buffer was cut to produce it.
func probeHead(data []byte) (head []byte, truncated bool) {
	if len(data) > textProbeCap {
		return data[:textProbeCap], true
	}
	return data, false
}

// trimPartialRune drops a multi-byte UTF-8 sequence left incomplete by the
// probe window's cut. Without this a valid file is judged invalid purely by
// where the window happened to land.
func trimPartialRune(head []byte) []byte {
	for i := len(head) - 1; i >= 0 && i > len(head)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(head[i]) {
			continue
		}
		if r, size := utf8.DecodeRune(head[i:]); r == utf8.RuneError && size <= 1 {
			return head[:i]
		}
		break
	}
	return head
}

// decodeUTF16 assembles code units in the given byte order and decodes them,
// surrogate pairs included. A trailing odd byte is dropped — half a code
// unit carries nothing.
func decodeUTF16(data []byte, bigEndian bool) string {
	if len(data)%2 != 0 {
		data = data[:len(data)-1]
	}
	units := make([]uint16, 0, len(data)/2)
	for i := 0; i < len(data); i += 2 {
		if bigEndian {
			units = append(units, uint16(data[i])<<8|uint16(data[i+1]))
		} else {
			units = append(units, uint16(data[i+1])<<8|uint16(data[i]))
		}
	}
	return string(utf16.Decode(units))
}

// decodeCharmap maps every byte through a single-byte code page. Byte-wise
// rather than through the streaming decoder because the mapping is total —
// there is no error path to handle.
func decodeCharmap(cm *charmap.Charmap, data []byte) string {
	var sb strings.Builder
	sb.Grow(len(data))
	for _, b := range data {
		sb.WriteRune(cm.DecodeByte(b))
	}
	return sb.String()
}

// normalizeText puts decoded text in the shape callers inline: LF line
// endings, no stray NUL (jsonb rejects it outright), valid UTF-8 throughout.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.ToValidUTF8(s, "�")
}

func trimBOM(data, bom []byte) []byte {
	if bytesHavePrefix(data, bom) {
		return data[len(bom):]
	}
	return data
}

func bytesHavePrefix(data, prefix []byte) bool {
	if len(data) < len(prefix) {
		return false
	}
	for i := range prefix {
		if data[i] != prefix[i] {
			return false
		}
	}
	return true
}
