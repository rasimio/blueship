package docx

import (
	"context"
	"strings"
	"testing"
)

func TestMarkdownDocxRoundTrip(t *testing.T) {
	if !Available() {
		t.Skip("pandoc not installed")
	}
	ctx := context.Background()
	md := "# Анкета\n\n| Поле | Значение |\n|---|---|\n| Имя | Расим |\n| Город | Белград |\n\nПримечание **жирным**."

	rendered, err := RenderMarkdown(ctx, md, nil)
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if len(rendered) < 1000 || rendered[0] != 'P' || rendered[1] != 'K' {
		t.Fatalf("expected ZIP-shaped docx, got %d bytes", len(rendered))
	}

	back, err := ExtractMarkdown(ctx, rendered)
	if err != nil {
		t.Fatalf("ExtractMarkdown: %v", err)
	}
	for _, needle := range []string{"Анкета", "Расим", "Белград", "жирным"} {
		if !strings.Contains(back, needle) {
			t.Fatalf("round-trip lost %q; got:\n%s", needle, back)
		}
	}

	// Re-render using the first output as the style reference — the
	// fill-a-form path. Must still produce a valid document.
	styled, err := RenderMarkdown(ctx, md, rendered)
	if err != nil {
		t.Fatalf("RenderMarkdown with reference: %v", err)
	}
	if len(styled) < 1000 {
		t.Fatalf("styled render too small: %d bytes", len(styled))
	}
}
