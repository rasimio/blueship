package attachment

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func buildTestXlsx(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	sheet := "Sheet1"
	_ = f.SetCellValue(sheet, "A1", "Товар")
	_ = f.SetCellValue(sheet, "B1", "Цена|шт")
	_ = f.SetCellValue(sheet, "A2", "Хлеб")
	_ = f.SetCellValue(sheet, "B2", 42)
	_, _ = f.NewSheet("Пустой")
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("write xlsx: %v", err)
	}
	return buf.Bytes()
}

func TestExtractXlsxMarkdown(t *testing.T) {
	data := buildTestXlsx(t)

	if kind := Kind("application/octet-stream", "report.xlsx", data); kind != "xlsx" {
		t.Fatalf("Kind = %q, want xlsx", kind)
	}

	md, err := ExtractXlsxMarkdown(data)
	if err != nil {
		t.Fatalf("ExtractXlsxMarkdown: %v", err)
	}
	for _, needle := range []string{"## Sheet1", "Товар", "Цена\\|шт", "Хлеб", "42", "## Пустой", "[empty sheet]"} {
		if !strings.Contains(md, needle) {
			t.Fatalf("markdown missing %q:\n%s", needle, md)
		}
	}
	if !strings.Contains(md, "|---|") {
		t.Fatal("markdown table separator missing")
	}
}

func TestExtractXlsxMarkdownRejectsGarbage(t *testing.T) {
	if _, err := ExtractXlsxMarkdown([]byte("PK\x03\x04 not a real zip")); err == nil {
		t.Fatal("expected error on corrupt xlsx")
	}
}
