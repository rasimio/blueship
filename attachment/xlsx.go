package attachment

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

// XlsxTextHeadCap bounds the markdown ExtractXlsxMarkdown returns —
// same budget as the docx extractor: enough for any real spreadsheet's
// meaningful head, without letting a million-row export flood the
// prompt.
const XlsxTextHeadCap = 60_000

// Per-sheet shape caps. A model reads tables top-down: the first rows
// carry the schema and the story, row ten thousand carries nothing a
// summary question needs. Truncation is always announced in the output
// so the model can ask for a narrower slice instead of pretending it
// saw everything.
const (
	xlsxMaxSheets      = 10
	xlsxMaxRowsSheet   = 200
	xlsxMaxCols        = 30
	xlsxMaxCellRunes   = 200
)

// mimeXlsx is the canonical MIME for an Excel .xlsx (OOXML spreadsheet).
const mimeXlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// ExtractXlsxMarkdown renders a .xlsx workbook as GFM markdown tables —
// one section per sheet, first row treated as the header. The output is
// bounded by XlsxTextHeadCap and the per-sheet caps above; every
// truncation is stated inline.
func ExtractXlsxMarkdown(data []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("xlsx: open: %w", err)
	}
	defer f.Close()

	var b strings.Builder
	sheets := f.GetSheetList()
	shownSheets := sheets
	if len(shownSheets) > xlsxMaxSheets {
		shownSheets = shownSheets[:xlsxMaxSheets]
	}

	for _, sheet := range shownSheets {
		if b.Len() >= XlsxTextHeadCap {
			fmt.Fprintf(&b, "\n[… output cap reached; %d more sheet(s) omitted]\n", len(sheets)-indexOf(sheets, sheet))
			break
		}
		rows, rerr := f.GetRows(sheet)
		if rerr != nil {
			fmt.Fprintf(&b, "\n## %s\n\n[sheet unreadable: %v]\n", sheet, rerr)
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n", sheet)
		if len(rows) == 0 {
			b.WriteString("[empty sheet]\n")
			continue
		}

		shownRows := rows
		if len(shownRows) > xlsxMaxRowsSheet {
			shownRows = shownRows[:xlsxMaxRowsSheet]
		}
		width := 0
		for _, row := range shownRows {
			if len(row) > width {
				width = len(row)
			}
		}
		truncCols := false
		if width > xlsxMaxCols {
			width = xlsxMaxCols
			truncCols = true
		}
		if width == 0 {
			b.WriteString("[empty sheet]\n")
			continue
		}

		for i, row := range shownRows {
			b.WriteString("|")
			for c := 0; c < width; c++ {
				cell := ""
				if c < len(row) {
					cell = sanitizeXlsxCell(row[c])
				}
				b.WriteString(" " + cell + " |")
			}
			b.WriteString("\n")
			if i == 0 {
				b.WriteString("|" + strings.Repeat("---|", width) + "\n")
			}
			if b.Len() >= XlsxTextHeadCap {
				break
			}
		}
		if len(rows) > len(shownRows) || b.Len() >= XlsxTextHeadCap {
			fmt.Fprintf(&b, "\n[showing %d of %d rows — ask for a specific range for more]\n", min(len(shownRows), xlsxMaxRowsSheet), len(rows))
		}
		if truncCols {
			fmt.Fprintf(&b, "[showing first %d columns]\n", xlsxMaxCols)
		}
	}
	if len(sheets) > xlsxMaxSheets {
		fmt.Fprintf(&b, "\n[%d of %d sheets shown]\n", xlsxMaxSheets, len(sheets))
	}

	out := b.String()
	if len(out) > XlsxTextHeadCap {
		out = out[:XlsxTextHeadCap] + "\n[… truncated at extractor cap]"
	}
	return out, nil
}

// sanitizeXlsxCell keeps one cell markdown-table-safe: pipes escaped,
// newlines flattened, runaway cells clipped.
func sanitizeXlsxCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	runes := []rune(s)
	if len(runes) > xlsxMaxCellRunes {
		s = string(runes[:xlsxMaxCellRunes]) + "…"
	}
	return s
}

func indexOf(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return len(list)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
