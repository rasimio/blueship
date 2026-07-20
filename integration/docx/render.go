// Package docx converts between GitHub-flavored markdown and Word
// documents (.docx) by driving the pandoc binary. Pandoc is the only
// production-quality bridge for this pair — the pure-Go OOXML writers
// either cover a fraction of markdown or carry an AGPL license.
//
// The binary is resolved once per process: $PANDOC_PATH wins, then
// $PATH, then the usual Homebrew/system locations. All entry points
// return ErrPandocMissing when no binary is found, so callers can
// degrade gracefully (e.g. keep offering PDF output only).
package docx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ErrPandocMissing is returned by every conversion when the pandoc
// binary cannot be located.
var ErrPandocMissing = errors.New("docx: pandoc binary not found (install pandoc or set PANDOC_PATH)")

var (
	binOnce sync.Once
	binPath string
)

// pandocPath resolves the pandoc binary once per process.
func pandocPath() string {
	binOnce.Do(func() {
		if p := strings.TrimSpace(os.Getenv("PANDOC_PATH")); p != "" {
			if _, err := os.Stat(p); err == nil {
				binPath = p
				return
			}
		}
		if p, err := exec.LookPath("pandoc"); err == nil {
			binPath = p
			return
		}
		// The daemon runs as a LaunchAgent whose PATH may not include
		// Homebrew; probe the standard install locations directly.
		for _, p := range []string{"/opt/homebrew/bin/pandoc", "/usr/local/bin/pandoc", "/usr/bin/pandoc"} {
			if _, err := os.Stat(p); err == nil {
				binPath = p
				return
			}
		}
	})
	return binPath
}

// Available reports whether the pandoc binary can be located.
func Available() bool { return pandocPath() != "" }

// RenderMarkdown converts GitHub-flavored markdown into a .docx and
// returns the raw bytes. When reference is non-empty it must be a
// .docx whose styles (fonts, heading looks, table style, margins) the
// output inherits via pandoc's --reference-doc — the way to produce a
// document that visually matches an uploaded original without
// touching it.
func RenderMarkdown(ctx context.Context, md string, reference []byte) ([]byte, error) {
	if strings.TrimSpace(md) == "" {
		return nil, fmt.Errorf("docx: empty markdown")
	}
	bin := pandocPath()
	if bin == "" {
		return nil, ErrPandocMissing
	}

	dir, err := os.MkdirTemp("", "docx-render-")
	if err != nil {
		return nil, fmt.Errorf("docx: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "in.md")
	outPath := filepath.Join(dir, "out.docx")
	if err := os.WriteFile(inPath, []byte(md), 0o600); err != nil {
		return nil, fmt.Errorf("docx: write source: %w", err)
	}

	args := []string{"-f", "gfm", "-t", "docx", "-o", outPath}
	if len(reference) > 0 {
		refPath := filepath.Join(dir, "reference.docx")
		if err := os.WriteFile(refPath, reference, 0o600); err != nil {
			return nil, fmt.Errorf("docx: write reference: %w", err)
		}
		args = append(args, "--reference-doc", refPath)
	}
	args = append(args, inPath)

	if err := runPandoc(ctx, bin, args); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("docx: read output: %w", err)
	}
	return data, nil
}

// ExtractMarkdown converts a .docx into GitHub-flavored markdown —
// headings, lists and tables survive, which makes the result directly
// usable as an editing surface for an LLM.
func ExtractMarkdown(ctx context.Context, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("docx: empty document")
	}
	bin := pandocPath()
	if bin == "" {
		return "", ErrPandocMissing
	}

	dir, err := os.MkdirTemp("", "docx-extract-")
	if err != nil {
		return "", fmt.Errorf("docx: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "in.docx")
	outPath := filepath.Join(dir, "out.md")
	if err := os.WriteFile(inPath, data, 0o600); err != nil {
		return "", fmt.Errorf("docx: write source: %w", err)
	}

	args := []string{"-f", "docx", "-t", "gfm", "--wrap", "none", "-o", outPath, inPath}
	if err := runPandoc(ctx, bin, args); err != nil {
		return "", err
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return "", fmt.Errorf("docx: read output: %w", err)
	}
	return string(out), nil
}

func runPandoc(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return fmt.Errorf("docx: pandoc: %w: %s", err, msg)
	}
	return nil
}
