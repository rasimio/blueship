package telegram

import (
	"context"
	"sync"
	"time"
)

// Telegram lets one bot send about 30 messages a second across all its
// chats; above that it answers 429 with retry_after. Every message a bot
// sends — a reply, a reminder, a broadcast — draws on that one allowance,
// so the pacing lives on the client, which is per bot, rather than with
// any one sender.
const (
	sendRatePerSecond = 25 // below Telegram's 30, so a burst never trips it
	sendBurst         = 25
	// bulkReserve is the part of the allowance a bulk send never takes:
	// with a broadcast running, the next reply to a person still has
	// tokens waiting for it instead of queueing behind the mailing.
	bulkReserve = 5
)

// pacedMethods are the requests that put a message into a chat or change
// one there. Lookups, callback answers and polling are not paced.
var pacedMethods = map[string]bool{
	"sendMessage":            true,
	"sendRichMessage":        true,
	"sendPhoto":              true,
	"sendDocument":           true,
	"sendVoice":              true,
	"editMessageText":        true,
	"editMessageMedia":       true,
	"editMessageReplyMarkup": true,
}

type bulkKey struct{}

// WithBulk marks the sends made under ctx as bulk: they yield to
// conversation traffic and wait out a flood-control answer before trying
// again. Used by mailings, never by replies.
func WithBulk(ctx context.Context) context.Context {
	return context.WithValue(ctx, bulkKey{}, true)
}

func isBulk(ctx context.Context) bool {
	v, _ := ctx.Value(bulkKey{}).(bool)
	return v
}

// sendLimiter is a token bucket shared by every send of one bot.
// Conversation sends take any token there is; bulk sends leave
// bulkReserve of them untouched and stop entirely while Telegram's last
// retry_after has not elapsed. A nil limiter paces nothing.
type sendLimiter struct {
	mu          sync.Mutex
	rate        float64 // tokens per second
	burst       float64
	reserve     float64
	tokens      float64
	last        time.Time
	pausedUntil time.Time
	now         func() time.Time
}

func newSendLimiter() *sendLimiter {
	return &sendLimiter{
		rate: sendRatePerSecond, burst: sendBurst, reserve: bulkReserve,
		tokens: sendBurst, now: time.Now,
	}
}

// take claims a token if one is available to this kind of send, or says
// how long to wait before asking again.
func (l *sendLimiter) take(bulk bool) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.last.IsZero() {
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
	}
	l.last = now
	floor := 0.0
	if bulk {
		if now.Before(l.pausedUntil) {
			return l.pausedUntil.Sub(now)
		}
		floor = l.reserve
	}
	if l.tokens >= floor+1 {
		l.tokens--
		return 0
	}
	wait := time.Duration((floor + 1 - l.tokens) / l.rate * float64(time.Second))
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	return wait
}

// wait blocks until a send of this kind may go, or ctx ends.
func (l *sendLimiter) wait(ctx context.Context, bulk bool) error {
	if l == nil {
		return nil
	}
	for {
		d := l.take(bulk)
		if d == 0 {
			return nil
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// pause holds bulk sends back for retry_after. Conversation sends are not
// held: each of them already waits out its own 429 before retrying, and a
// flood answer about one chat says nothing certain about the others.
func (l *sendLimiter) pause(d time.Duration) {
	if l == nil || d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := l.now().Add(d); until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
}
