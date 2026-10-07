package tool

import (
	"context"
	"encoding/json"
	"sync"

	bs "github.com/rasimio/blueship/internal/core"
)

type fetchGateEntry struct {
	token chan struct{}
	refs  int
}
type taskFetchGate struct {
	mu      sync.Mutex
	entries map[string]*fetchGateEntry
}

var backgroundFetchGate taskFetchGate

// Serialize same-task, same-URL cache misses through persistence. Independent
// URLs and tenants remain parallel. A waiter can cancel without canceling the
// active read; no detached fetch outlives its owning step.
func (g *taskFetchGate) acquire(ctx context.Context, url string) (func(), error) {
	task, ok := bs.TaskIDFromContext(ctx)
	if !ok {
		return func() {}, ctx.Err()
	}
	keyBytes, _ := json.Marshal([]string{task.String(), bs.UserIDFromContext(ctx).String(), bs.SoulIDFromContext(ctx).String(), url})
	key := string(keyBytes)
	g.mu.Lock()
	if g.entries == nil {
		g.entries = map[string]*fetchGateEntry{}
	}
	entry := g.entries[key]
	if entry == nil {
		entry = &fetchGateEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		g.entries[key] = entry
	}
	entry.refs++
	g.mu.Unlock()
	drop := func() {
		g.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(g.entries, key)
		}
		g.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	case <-entry.token:
		var once sync.Once
		release := func() { once.Do(func() { entry.token <- struct{}{}; drop() }) }
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
}
