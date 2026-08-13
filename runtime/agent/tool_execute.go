package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

const (
	defaultToolExecutionTimeout = 90 * time.Second
	browserSearchTimeout        = 35 * time.Second
	browserFetchTimeout         = 55 * time.Second
	// Image generation lives where the default cap bites: production runs
	// 54-85s on a normal day, so 90s cancels the healthy tail whenever the
	// backend slows at all — three cancellations in one afternoon, each of a
	// generation already paid for and likely seconds from done. The cancel is
	// the expensive part: the request is torn down, so a result at 95s is
	// discarded rather than late.
	imageGenerationTimeout = 4 * time.Minute
)

type toolExecutionResult struct {
	output  string
	isError bool
}

func resolveToolExecutionTimeout(override time.Duration, name string) time.Duration {
	if override > 0 {
		return override
	}
	switch name {
	case "browser_search":
		return browserSearchTimeout
	case "browser_fetch":
		return browserFetchTimeout
	case "image_generate":
		return imageGenerationTimeout
	default:
		return defaultToolExecutionTimeout
	}
}

func executeToolWithTimeout(ctx context.Context, registry *bs.ToolRegistry, name string, input json.RawMessage, timeout time.Duration) (string, bool, bool) {
	if timeout <= 0 {
		output, isError := registry.Execute(ctx, name, input)
		return output, isError, false
	}

	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan toolExecutionResult, 1)
	go func() {
		output, isError := registry.Execute(toolCtx, name, input)
		done <- toolExecutionResult{output: output, isError: isError}
	}()

	select {
	case result := <-done:
		return result.output, result.isError, false
	case <-toolCtx.Done():
		return fmt.Sprintf("tool %s timed out after %s: %v", name, timeout, toolCtx.Err()), true, true
	}
}
