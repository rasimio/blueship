package pdf

import (
	"bytes"
	"context"
	"testing"
)

func TestPagesToImagesRendersJPEGs(t *testing.T) {
	if !PagesToImagesAvailable() {
		t.Skip("pdftoppm not installed")
	}
	ctx := context.Background()
	doc, err := RenderMarkdown(ctx, "test", "# Заголовок\n\nСтраница с русским текстом для рендера.")
	if err != nil {
		t.Skipf("chromedp unavailable for fixture render: %v", err)
	}

	pages, err := PagesToImages(ctx, doc, 3, 120)
	if err != nil {
		t.Fatalf("PagesToImages: %v", err)
	}
	if len(pages) == 0 {
		t.Fatal("no pages rendered")
	}
	for i, p := range pages {
		if !bytes.HasPrefix(p, []byte{0xFF, 0xD8}) {
			t.Fatalf("page %d is not a JPEG (len %d)", i, len(p))
		}
	}
}
