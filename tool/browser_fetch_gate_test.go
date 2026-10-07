package tool

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
)

func TestTaskFetchGateSerializesCacheFillAndReleasesEntries(t *testing.T) {
	var gate taskFetchGate
	ctx := bs.ContextWithTaskID(context.Background(), uuid.New())
	first, err := gate.acquire(ctx, "https://example.com/source")
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan struct{})
	done := make(chan struct{})
	cached := false
	reads := 0
	go func() {
		defer close(done)
		release, err := gate.acquire(ctx, "https://example.com/source")
		if err != nil {
			t.Error(err)
			return
		}
		defer release()
		close(acquired)
		if !cached {
			reads++
		}
	}()
	select {
	case <-acquired:
		t.Fatal("concurrent duplicate read admitted")
	case <-time.After(20 * time.Millisecond):
	}
	reads++
	cached = true // represents fetch and durable cache write inside the gate
	first()
	first() // cleanup is idempotent
	<-done
	if reads != 1 {
		t.Fatal("cache fill was not serialized", reads)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.entries) != 0 {
		t.Fatal("completed keys leaked", len(gate.entries))
	}
}

func TestTaskFetchGateIsolationAndWaiterCancellation(t *testing.T) {
	var gate taskFetchGate
	ctx := bs.ContextWithTaskID(context.Background(), uuid.New())
	first, err := gate.acquire(ctx, "url")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	// Different URL, task and soul must never wait behind this read.
	contexts := []context.Context{ctx, bs.ContextWithTaskID(context.Background(), uuid.New()), bs.WithSoulID(ctx, uuid.New())}
	for i, c := range contexts {
		target := "url"
		if i == 0 {
			target = "other"
		}
		bounded, cancel := context.WithTimeout(c, time.Second)
		release, err := gate.acquire(bounded, target)
		cancel()
		if err != nil {
			t.Fatal("independent read blocked", err)
		}
		release()
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if release, err := gate.acquire(canceled, "url"); err == nil {
		release()
		t.Fatal("canceled waiter admitted")
	}
	first()
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := gate.acquire(ctx, "url")
			if err != nil {
				t.Error(err)
				return
			}
			release()
		}()
	}
	wg.Wait()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.entries) != 0 {
		t.Fatal("waiter keys leaked", len(gate.entries))
	}
}
