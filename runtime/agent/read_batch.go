package agent

import (
	"context"
	bs "github.com/rasimio/blueship/internal/core"
	"sync"
	"time"
)

type readToolResult struct {
	output            string
	isError, timedOut bool
	started           time.Time
	elapsed           time.Duration
}

func (a *Loop) canBatchReads(cfg RunConfig, blocks []bs.ContentBlock, allowed []bs.ToolDefinition) bool {
	if cfg.ParallelReadTools < 2 {
		return false
	}
	count := 0
	for _, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		if block.Name == ToolboxToolName || !a.registry.IsReadOnly(block.Name) || (cfg.StrictTools && !toolDefinitionPresent(allowed, block.Name)) {
			return false
		}
		count++
	}
	return count > 1
}

// All results are delivered through separate buffered channels. The calling
// loop records receipts/checkpoints serially in model order. Cleanup cancels
// queued reads and allows bounded cleanup of actual handlers. A handler that
// ignores cancellation retains its slot until it exits; it cannot admit more reads.
func (a *Loop) startReadBatch(ctx context.Context, cfg RunConfig, blocks []bs.ContentBlock) (map[int]<-chan readToolResult, func()) {
	ctx, cancel := context.WithCancel(ctx)
	sem := make(chan struct{}, min(3, cfg.ParallelReadTools))
	var wg sync.WaitGroup
	results := map[int]<-chan readToolResult{}
	for index, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		ch := make(chan readToolResult, 1)
		results[index] = ch
		wg.Add(1)
		go func(block bs.ContentBlock) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				ch <- readToolResult{output: ctx.Err().Error(), isError: true, started: time.Now()}
				return
			}
			if ctx.Err() != nil {
				<-sem
				ch <- readToolResult{output: ctx.Err().Error(), isError: true, started: time.Now()}
				return
			}
			start := time.Now()
			wg.Add(1)
			output, bad, timeout := executeToolWithTimeoutObserved(ctx, a.registry, block.Name, block.Input, resolveToolExecutionTimeout(cfg.ToolTimeout, block.Name), func() {
				<-sem
				wg.Done()
			})
			ch <- readToolResult{output: output, isError: bad, timedOut: timeout, started: start, elapsed: time.Since(start)}
		}(block)
	}
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	return results, func() {
		cancel()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-drained:
		case <-timer.C:
			// Go cannot forcibly terminate a handler ignoring its context.
			// Keep its capacity occupied, but do not strand task cancellation.
		}
	}
}
