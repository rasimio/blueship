package attachment

import (
	"strings"
	"testing"
)

// utf16le encodes a string the way a Windows editor's "Unicode" export does:
// BOM first, little-endian code units.
func utf16le(s string, bom bool) []byte {
	var out []byte
	if bom {
		out = append(out, 0xFF, 0xFE)
	}
	for _, r := range s {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

func utf16be(s string, bom bool) []byte {
	var out []byte
	if bom {
		out = append(out, 0xFE, 0xFF)
	}
	for _, r := range s {
		out = append(out, byte(r>>8), byte(r))
	}
	return out
}

// cp1251 encodes Russian text as a single-byte Windows code page — what an
// "ANSI" save produces.
func cp1251(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x80:
			out = append(out, byte(r))
		case r >= 'А' && r <= 'я':
			out = append(out, byte(r-'А'+0xC0))
		case r == 'Ё':
			out = append(out, 0xA8)
		case r == 'ё':
			out = append(out, 0xB8)
		default:
			out = append(out, '?')
		}
	}
	return out
}

// A .txt longer than the probe window used to be rejected outright whenever
// the window's cut landed inside a multi-byte rune — which for Cyrillic is
// about half of all offsets. This is the todo.txt failure.
func TestKind_CyrillicTextOverProbeWindow(t *testing.T) {
	line := "- позвонить в банк и закрыть карту\n"
	for pad := range 8 {
		body := strings.Repeat("x", pad) + strings.Repeat(line, 400)
		if len(body) <= textProbeCap {
			t.Fatalf("pad %d: fixture too small (%d bytes)", pad, len(body))
		}
		if got := Kind("text/plain", "todo.txt", []byte(body)); got != "text" {
			t.Errorf("pad %d: Kind = %q, want text", pad, got)
		}
		decoded, ok := DecodeText([]byte(body))
		if !ok || decoded != body {
			t.Errorf("pad %d: DecodeText ok=%v, round-trip=%v", pad, ok, decoded == body)
		}
	}
}

func TestDecodeText_Encodings(t *testing.T) {
	const ru = "дела на сегодня:\n- позвонить в банк\n- забрать посылку\n"
	// Long enough to clear the BOM-less UTF-16 sniffer's sample floor.
	const en = "todo:\n- call the bank\n- pick up the parcel\n- book the flight\n"

	tests := []struct {
		name string
		data []byte
		enc  string
		want string
	}{
		{"utf-8 plain", []byte(ru), encUTF8, ru},
		{"utf-8 with BOM", append([]byte{0xEF, 0xBB, 0xBF}, ru...), encUTF8, ru},
		{"utf-16le BOM", utf16le(ru, true), encUTF16LE, ru},
		{"utf-16be BOM", utf16be(ru, true), encUTF16BE, ru},
		{"utf-16le no BOM, cyrillic", utf16le(ru, false), encUTF16LE, ru},
		{"utf-16le no BOM, latin", utf16le(en, false), encUTF16LE, en},
		{"utf-16be no BOM, cyrillic", utf16be(ru, false), encUTF16BE, ru},
		{"windows-1251", cp1251(ru), encCP1251, ru},
		{"crlf normalised", []byte("a\r\nb\r\n"), encUTF8, "a\nb\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectTextEncoding(tc.data); got != tc.enc {
				t.Errorf("DetectTextEncoding = %q, want %q", got, tc.enc)
			}
			if got := Kind("", "notes.txt", tc.data); got != "text" {
				t.Errorf("Kind = %q, want text", got)
			}
			got, ok := DecodeText(tc.data)
			if !ok {
				t.Fatalf("DecodeText: ok=false")
			}
			if got != tc.want {
				t.Errorf("DecodeText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeText_Windows1252(t *testing.T) {
	// Mostly ASCII with a few accented letters — the case that must NOT be
	// read as Cyrillic.
	data := []byte("caf\xe9 \xe0 Paris, na\xefve r\xe9sum\xe9 for the archive\n")
	if got := DetectTextEncoding(data); got != encCP1252 {
		t.Fatalf("DetectTextEncoding = %q, want %q", got, encCP1252)
	}
	got, ok := DecodeText(data)
	if !ok {
		t.Fatal("DecodeText: ok=false")
	}
	if want := "café à Paris, naïve résumé for the archive\n"; got != want {
		t.Errorf("DecodeText = %q, want %q", got, want)
	}
}

func TestDecodeText_RejectsBinary(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"NUL-riddled binary", []byte("\x89\x00\x01\x02\x00\x03compiled\x00\x00\xff\xfe\x00")},
		{"non-UTF-8 with dense control bytes", []byte("\xe0\x01\x02\xf5\x03\x04\xe1\x05\x06\x07\xff\x0b\x0e\x0f\xe2\x10\x11\x12\x13\x14")},
		{"high entropy in the 0x80-0x9F band", []byte("\x81\x8d\x8f\x90\x9d\x81\x8d\x8f\x90\x9d\x81\x8d\x8f\x90\x9d\x81\x8d\x8f\x90\x9d")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectTextEncoding(tc.data); got != "" {
				t.Errorf("DetectTextEncoding = %q, want \"\"", got)
			}
			if _, ok := DecodeText(tc.data); ok {
				t.Errorf("DecodeText: ok=true, want false")
			}
		})
	}
}

// A file whose head decodes cleanly can still carry a broken tail; whatever
// reaches the prompt and the jsonb column must be valid UTF-8 regardless.
func TestDecodeText_AlwaysValidUTF8(t *testing.T) {
	data := append([]byte(strings.Repeat("ровный текст\n", 700)), 0xC3, 0x28, 0xFF)
	got, ok := DecodeText(data)
	if !ok {
		t.Fatal("DecodeText: ok=false")
	}
	if !strings.HasPrefix(got, "ровный текст\n") {
		t.Errorf("head lost: %q", got[:20])
	}
	for i, r := range got {
		if r == '�' && i < len(got)-10 {
			t.Fatalf("replacement rune inside the clean head at %d", i)
		}
	}
}

func TestTrimPartialRune(t *testing.T) {
	full := []byte("абв")
	if got := trimPartialRune(full); len(got) != len(full) {
		t.Errorf("complete buffer trimmed: %d → %d bytes", len(full), len(got))
	}
	cut := full[:len(full)-1] // "аб" + the lead byte of "в"
	if got := trimPartialRune(cut); len(got) != 4 {
		t.Errorf("partial rune kept: got %d bytes, want 4", len(got))
	}
}
