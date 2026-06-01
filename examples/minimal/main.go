// Command minimal is the smallest runnable BlueShip agent: a Telegram bot
// backed by Claude, with session persistence, compaction, and the built-in
// tools (current_time, web_search, browser_fetch) wired automatically.
//
// Run it with:
//
//	ANTHROPIC_API_KEY=sk-...        \
//	TELEGRAM_BOT_TOKEN=123:abc...   \
//	DATABASE_URL=postgres://localhost/blueship?sslmode=disable \
//	go run ./examples/minimal
//
// BlueShip auto-migrates its own tables on first start. See
// docs/ARCHITECTURE.md for how the pieces fit together, and the README for
// how to add your own tools (ToolProvider), jobs (JobProvider), and CLI
// commands (CLIProvider) via modules.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/rasimio/blueship"
)

func main() {
	ship := blueship.New(blueship.Config{
		LLM:       blueship.Anthropic(os.Getenv("ANTHROPIC_API_KEY")),
		Transport: blueship.Telegram(os.Getenv("TELEGRAM_BOT_TOKEN")),
		DB:        os.Getenv("DATABASE_URL"),
	})

	// Run blocks until the process receives SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := ship.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
