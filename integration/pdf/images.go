package pdf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ErrPdftoppmMissing is returned by PagesToImages when poppler's pdftoppm
// binary cannot be located.
var ErrPdftoppmMissing = errors.New("pdf: pdftoppm binary not found (install poppler or set PDFTOPPM_PATH)")

var (
	ppmOnce sync.Once
	ppmPath string
)

// pdftoppmPath resolves the pdftoppm binary once per process — same
// resolution order as the pandoc bridge in integration/docx.
func pdftoppmPath() string {
	ppmOnce.Do(func() {
		if p := strings.TrimSpace(os.Getenv("PDFTOPPM_PATH")); p != "" {
			if _, err := os.Stat(p); err == nil {
				ppmPath = p
				return
			}
		}
		if p, err := exec.LookPath("pdftoppm"); err == nil {
			ppmPath = p
			return
		}
		for _, p := range []string{"/opt/homebrew/bin/pdftoppm", "/usr/local/bin/pdftoppm", "/usr/bin/pdftoppm"} {
			if _, err := os.Stat(p); err == nil {
				ppmPath = p
				return
			}
		}
	})
	return ppmPath
}

// PagesToImagesAvailable reports whether the pdftoppm binary can be located.
func PagesToImagesAvailable() bool { return pdftoppmPath() != "" }

// PagesToImages renders the first maxPages pages of a PDF into JPEG images
// (poppler pdftoppm). The escape hatch for scanned PDFs with no text layer:
// extraction yields nothing, but a vision-capable model reads the rendered
// pages directly. dpi ~150 keeps an A4 page around 1250×1750 px — crisp for
// document text, comfortably inside vision input limits.
func PagesToImages(ctx context.Context, data []byte, maxPages, dpi int) ([][]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("pdf: empty document")
	}
	bin := pdftoppmPath()
	if bin == "" {
		return nil, ErrPdftoppmMissing
	}
	if maxPages <= 0 {
		maxPages = 6
	}
	if dpi <= 0 {
		dpi = 150
	}

	dir, err := os.MkdirTemp("", "pdf-pages-")
	if err != nil {
		return nil, fmt.Errorf("pdf: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("pdf: write source: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin,
		"-jpeg", "-jpegopt", "quality=82",
		"-r", strconv.Itoa(dpi),
		"-f", "1", "-l", strconv.Itoa(maxPages),
		inPath, filepath.Join(dir, "page"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("pdf: pdftoppm: %w: %s", err, msg)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("pdf: read output dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "page") && strings.HasSuffix(e.Name(), ".jpg") {
			names = append(names, e.Name())
		}
	}
	// pdftoppm zero-pads page numbers per document, so lexical order is
	// page order.
	sort.Strings(names)

	pages := make([][]byte, 0, len(names))
	for _, name := range names {
		img, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			return nil, fmt.Errorf("pdf: read page %s: %w", name, rerr)
		}
		pages = append(pages, img)
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("pdf: pdftoppm produced no pages")
	}
	return pages, nil
}
