package attachment

import (
	"archive/zip"
	"bytes"
	"testing"
)

// makeDocx builds a minimal valid .docx — a zip whose word/document.xml holds
// the given <w:body> contents — so the classifier + extractor tests run
// against a real OOXML container rather than a hand-faked byte string.
func makeDocx(t *testing.T, bodyXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	doc := `<?xml version="1.0"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` + bodyXML + `</w:body></w:document>`
	if _, err := w.Write([]byte(doc)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func para(text string) string {
	return `<w:p><w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

func TestKind_Docx(t *testing.T) {
	docx := makeDocx(t, para("hi"))
	tests := []struct {
		name, mime, fn string
		data           []byte
		want           string
	}{
		{"docx by ext", "", "report.docx", docx, "docx"},
		{"docx by canonical mime", mimeDocx, "report", docx, "docx"},
		{"docx ext with octet-stream mime", "application/octet-stream", "report.docx", docx, "docx"},
		{"zip but not a docx ext/mime", "application/zip", "archive.zip", docx, ""},
		{"docx ext over PDF bytes stays pdf", "", "fake.docx", []byte("%PDF-1.4 still a pdf"), "pdf"},
		{"plain pdf", "application/pdf", "x.pdf", []byte("%PDF-1.7\nbody"), "pdf"},
		{"plain text", "text/plain", "notes.txt", []byte("hello world"), "text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Kind(tc.mime, tc.fn, tc.data); got != tc.want {
				t.Errorf("Kind(%q, %q) = %q, want %q", tc.mime, tc.fn, got, tc.want)
			}
		})
	}
}

func TestMaxBytesForKind_Docx(t *testing.T) {
	if got := MaxBytesForKind("docx"); got != MaxDocxBytes {
		t.Errorf("MaxBytesForKind(docx) = %d, want %d", got, MaxDocxBytes)
	}
}

func TestExtractDocxText(t *testing.T) {
	docx := makeDocx(t,
		para("Привет")+
			para("Мир")+
			`<w:p><w:r><w:t>A</w:t><w:tab/><w:t>B</w:t></w:r></w:p>`)
	got, err := ExtractDocxText(docx)
	if err != nil {
		t.Fatalf("ExtractDocxText: %v", err)
	}
	want := "Привет\nМир\nA\tB"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractDocxText_Errors(t *testing.T) {
	if _, err := ExtractDocxText([]byte("not a zip at all")); err != ErrNotDocx {
		t.Errorf("non-zip: got %v, want ErrNotDocx", err)
	}

	// A zip that isn't a docx (no word/document.xml).
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("other.txt")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	if _, err := ExtractDocxText(buf.Bytes()); err != ErrNotDocx {
		t.Errorf("zip without document.xml: got %v, want ErrNotDocx", err)
	}

	// A structurally valid docx with no <w:t> text reads as "nothing".
	if _, err := ExtractDocxText(makeDocx(t, `<w:p></w:p>`)); err != ErrNotDocx {
		t.Errorf("empty docx: got %v, want ErrNotDocx", err)
	}
}
