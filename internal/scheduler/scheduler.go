package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// RunLoop runs fn immediately, then on interval until ctx is cancelled.
// Panics are recovered and logged.
func RunLoop(ctx context.Context, logger *slog.Logger, name string, interval time.Duration, fn func(ctx context.Context) error) {
	if interval <= 0 {
		logger.Error("invalid loop interval, skipping", "name", name, "interval", interval)
		return
	}

	logger.Info("starting loop", "name", name, "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	run := func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("loop panic recovered", "name", name, "panic", r)
			}
		}()
		if err := fn(ctx); err != nil {
			logger.Error("loop iteration failed", "name", name, "error", err)
		}
	}

	run()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
