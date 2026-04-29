package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Alerter pushes high-severity log records to a Telegram chat. Designed
// to be wrapped by AlertSlogHandler so callers don't have to duplicate
// the throttle / format logic at every error site.
//
// Throttle dedupes by (level, message) within the configured window. A
// tight retry loop won't flood the channel — only the first failure
// inside the window is sent.
type Alerter struct {
	botToken    string
	chatID      string
	client      *http.Client
	throttle    time.Duration
	serviceName string

	mu   sync.Mutex
	seen map[string]time.Time
}

// NewAlerter wires a Telegram alerter. Returns nil when the Telegram
// section is disabled or missing credentials — callers must nil-guard.
//
// Why nil instead of a no-op: lets the slog handler chain skip the
// alert step entirely when Telegram is off, avoiding a per-record
// branch on Enabled().
func NewAlerter(cfg Config) *Alerter {
	tg := cfg.Alerts.Telegram
	if !tg.Enabled || tg.Token == "" || tg.ChatID == "" {
		return nil
	}
	throttle := tg.Throttle
	if throttle == 0 {
		throttle = time.Minute
	}
	a := &Alerter{
		botToken:    tg.Token,
		chatID:      tg.ChatID,
		client:      &http.Client{Timeout: 5 * time.Second},
		throttle:    throttle,
		serviceName: cfg.ServiceName,
		seen:        make(map[string]time.Time),
	}
	go a.gcLoop()
	return a
}

// Send queues a Telegram message if (level, msg) wasn't seen within the
// throttle window. Non-blocking — the HTTP call runs in its own
// goroutine so a slow Telegram API doesn't stall the logger.
func (a *Alerter) Send(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	if a == nil {
		return
	}

	fingerprint := fmt.Sprintf("%s:%s", level, msg)
	a.mu.Lock()
	if last, ok := a.seen[fingerprint]; ok && time.Since(last) < a.throttle {
		a.mu.Unlock()
		return
	}
	a.seen[fingerprint] = time.Now()
	a.mu.Unlock()

	text := a.format(level, msg, attrs)
	go a.post(text)
}

func (a *Alerter) format(level slog.Level, msg string, attrs []slog.Attr) string {
	var icon string
	switch {
	case level >= slog.LevelError:
		icon = "🔴"
	case level >= slog.LevelWarn:
		icon = "🟡"
	default:
		icon = "🔵"
	}

	var b strings.Builder
	if a.serviceName != "" {
		fmt.Fprintf(&b, "%s <b>%s</b> — %s\n\n<code>%s</code>",
			icon, level, a.serviceName, escapeHTML(msg))
	} else {
		fmt.Fprintf(&b, "%s <b>%s</b>\n\n<code>%s</code>", icon, level, escapeHTML(msg))
	}

	if len(attrs) > 0 {
		b.WriteString("\n")
		for _, attr := range attrs {
			fmt.Fprintf(&b, "\n<b>%s</b>: <code>%s</code>",
				escapeHTML(attr.Key), escapeHTML(attr.Value.String()))
		}
	}
	fmt.Fprintf(&b, "\n\n<i>%s</i>", time.Now().UTC().Format("02 Jan 2006, 15:04:05 UTC"))
	return b.String()
}

func (a *Alerter) post(text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", a.botToken)
	body, _ := json.Marshal(map[string]any{
		"chat_id":    a.chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	resp, err := a.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}

// gcLoop periodically prunes the dedup map so a long-lived process doesn't
// accumulate fingerprints forever.
func (a *Alerter) gcLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-a.throttle * 10)
		a.mu.Lock()
		for k, v := range a.seen {
			if v.Before(cutoff) {
				delete(a.seen, k)
			}
		}
		a.mu.Unlock()
	}
}

// escapeHTML neutralises Telegram HTML parse errors when log messages
// contain stray <, >, or & (common in stack traces / SQL errors).
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
