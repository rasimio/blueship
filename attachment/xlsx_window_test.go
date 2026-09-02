package attachment

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// workbookWithRows builds a one-sheet workbook: a header plus n data rows
// whose first cell is "r<i>".
func workbookWithRows(t *testing.T, sheet string, n int) []byte {
	t.Helper()
	f := excelize.NewFile()
	if sheet != "Sheet1" {
		if _, err := f.NewSheet(sheet); err != nil {
			t.Fatalf("NewSheet: %v", err)
		}
		f.DeleteSheet("Sheet1")
	}
	if err := f.SetSheetRow(sheet, "A1", &[]string{"name", "phone"}); err != nil {
		t.Fatalf("SetSheetRow header: %v", err)
	}
	for i := 1; i <= n; i++ {
		row := &[]string{fmt.Sprintf("r%d", i), fmt.Sprintf("+700000%04d", i)}
		if err := f.SetSheetRow(sheet, fmt.Sprintf("A%d", i+1), row); err != nil {
			t.Fatalf("SetSheetRow %d: %v", i, err)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

// A sheet longer than the row cap is reachable in full: each slice repeats the
// header, states its position, and names the offset for the next call. Before
// this the output said "ask for a specific range" while attachment_read took
// no range — rows past 200 could not be read at all.
func TestExtractXlsxMarkdownWindowPagesWholeSheet(t *testing.T) {
	const total = xlsxMaxRowsSheet + 45
	data := workbookWithRows(t, "Контакты", total)

	seen := map[string]bool{}
	offset, slices := 0, 0
	for {
		md, err := ExtractXlsxMarkdownWindow(data, XlsxWindow{Sheet: "Контакты", RowOffset: offset})
		if err != nil {
			t.Fatalf("offset %d: %v", offset, err)
		}
		if !strings.Contains(md, "| name | phone |") {
			t.Fatalf("offset %d: slice arrived without its header:\n%s", offset, md)
		}
		for i := 1; i <= total; i++ {
			if strings.Contains(md, fmt.Sprintf("| r%d |", i)) {
				seen[fmt.Sprintf("r%d", i)] = true
			}
		}
		slices++
		if slices > 5 {
			t.Fatal("paging did not terminate")
		}
		next := fmt.Sprintf("row_offset=%d", offset+xlsxMaxRowsSheet)
		if !strings.Contains(md, next) {
			if !strings.Contains(md, "end of sheet") {
				t.Fatalf("offset %d: slice announces neither a next offset nor the end:\n%s", offset, md)
			}
			break
		}
		offset += xlsxMaxRowsSheet
	}
	if len(seen) != total {
		t.Fatalf("paging surfaced %d of %d data rows", len(seen), total)
	}
	if slices != 2 {
		t.Fatalf("slices = %d, want 2 for %d rows at a %d cap", slices, total, xlsxMaxRowsSheet)
	}
}

// The zero window is the unchanged whole-workbook head every ingest path uses.
func TestExtractXlsxMarkdownDefaultsToHead(t *testing.T) {
	data := workbookWithRows(t, "Sheet1", xlsxMaxRowsSheet+10)

	md, err := ExtractXlsxMarkdown(data)
	if err != nil {
		t.Fatalf("ExtractXlsxMarkdown: %v", err)
	}
	if !strings.Contains(md, "| r1 |") {
		t.Fatalf("head must start at the first data row:\n%s", md)
	}
	if strings.Contains(md, fmt.Sprintf("| r%d |", xlsxMaxRowsSheet+1)) {
		t.Fatal("head must stay inside the row cap")
	}
	if !strings.Contains(md, "row_offset=") {
		t.Fatalf("a truncated head must name the next offset:\n%s", md)
	}
}

// A sheet that fits needs no paging tail at all.
func TestExtractXlsxMarkdownWindowShortSheetHasNoPagingTail(t *testing.T) {
	data := workbookWithRows(t, "Sheet1", 3)

	md, err := ExtractXlsxMarkdown(data)
	if err != nil {
		t.Fatalf("ExtractXlsxMarkdown: %v", err)
	}
	if strings.Contains(md, "row_offset=") || strings.Contains(md, "end of sheet") {
		t.Fatalf("a complete sheet must not advertise paging:\n%s", md)
	}
}

func TestExtractXlsxMarkdownWindowUnknownSheetIsAnError(t *testing.T) {
	data := workbookWithRows(t, "Контакты", 5)

	_, err := ExtractXlsxMarkdownWindow(data, XlsxWindow{Sheet: "Nope"})
	if err == nil {
		t.Fatal("a misnamed sheet must be an error, not a silently empty read")
	}
	if !strings.Contains(err.Error(), "Контакты") {
		t.Fatalf("error must list the sheets that do exist: %v", err)
	}
}
