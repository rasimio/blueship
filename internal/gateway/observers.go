package gateway

import (
	"context"
	"strings"
	"time"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// observerDeadline bounds one observer call. The /start observer runs
// inline, before onboarding, so this is the most a slow host can add to a
// first reply; the others run off the update loop and add nothing.
const observerDeadline = 3 * time.Second

// observe runs an observer call with a deadline and without letting it
// take the gateway down.
func (g *Gateway) observe(ctx context.Context, what string, fn func(context.Context)) {
	ctx, cancel := context.WithTimeout(ctx, observerDeadline)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			g.logger.Error("gateway: observer panicked", "observer", what, "panic", r)
		}
	}()
	fn(ctx)
}

// observeStart reports a private-chat /start to a host that watches for
// them. Errands, logins and account links are not acquisition and are left
// out; a parameter outside Telegram's deep-link alphabet is reported as a
// bare /start, because no link could have produced it.
func (g *Gateway) observeStart(ctx context.Context, bi *botInstance, msg *telegram.Message, text string) {
	obs, ok := g.deps.BotOnboarding.(core.StartObserver)
	if !ok || bi == nil || msg == nil || msg.From == nil || msg.Chat.Type != "private" {
		return
	}
	cmd, args, forUs := parseStartCommandArgs(g, bi, text)
	if !forUs || cmd != "/start" {
		return
	}
	payload := strings.TrimSpace(args)
	if payload != "" {
		if _, errand := g.startPayloadCommand("/start " + payload); errand {
			return
		}
		if strings.HasPrefix(payload, deeplinkPayloadPrefix) || strings.HasPrefix(payload, linkPayloadPrefix) {
			return
		}
		payload = g.signupSource(bi, text)
	}
	at := time.Now()
	if msg.Date > 0 {
		at = time.Unix(msg.Date, 0)
	}
	g.observe(ctx, "start", func(ctx context.Context) {
		obs.ObserveStart(ctx, core.StartSeen{
			BotID: bi.id, TGUserID: msg.From.ID, TGChatID: msg.Chat.ID,
			Payload: payload, MessageID: int64(msg.MessageID), At: at,
		})
	})
}

// observeDelivery reports a message Telegram accepted. Off the update
// loop: nothing downstream waits for it.
func (g *Gateway) observeDelivery(_ context.Context, bi *botInstance, d core.DeliverySeen) {
	obs, ok := g.deps.BotOnboarding.(core.DeliveryObserver)
	if !ok || bi == nil || d.MessageID == 0 {
		return
	}
	d.BotID = bi.id
	if d.At.IsZero() {
		d.At = time.Now()
	}
	g.observeAsync("delivery", func(ctx context.Context) { obs.ObserveDelivery(ctx, d) })
}

// observeInline makes asynchronous observers run inline. Tests only.
var observeInline bool

// observeAsync runs an observer call on its own goroutine with its own
// deadline, detached from the update that caused it.
func (g *Gateway) observeAsync(what string, fn func(context.Context)) {
	if observeInline {
		g.observe(context.Background(), what, fn)
		return
	}
	go g.observe(context.Background(), what, fn)
}

// countButtons tallies a keyboard by what its buttons do.
func countButtons(rows [][]telegram.InlineKeyboardButton, d *core.DeliverySeen) {
	for _, row := range rows {
		for _, b := range row {
			switch {
			case b.CallbackData != "":
				d.CallbackButtons++
			case strings.HasPrefix(b.URL, "https://t.me/$"), strings.HasPrefix(b.URL, "https://t.me/invoice/"):
				d.InvoiceButtons++
			case b.URL != "":
				d.URLButtons++
			}
		}
	}
}

// sentID is the message id of a successful send, 0 otherwise.
func sentID(res *telegram.SendMessageResult, err error) int64 {
	if err != nil || res == nil {
		return 0
	}
	return int64(res.Result.MessageID)
}

// observePaymentApproved reports an approved pre-checkout. It runs after
// the answer has gone to Telegram, so it can never delay a payment.
func (g *Gateway) observePaymentApproved(_ context.Context, bi *botInstance, q *telegram.PreCheckoutQuery) {
	obs, ok := g.deps.BotOnboarding.(core.PaymentObserver)
	if !ok || bi == nil || q == nil {
		return
	}
	p := core.PaymentApproved{
		BotID: bi.id, QueryID: q.ID, Payload: q.InvoicePayload,
		Currency: q.Currency, Amount: q.TotalAmount, At: time.Now(),
	}
	if q.From != nil {
		p.TGUserID = q.From.ID
	}
	g.observeAsync("payment", func(ctx context.Context) { obs.ObservePaymentApproved(ctx, p) })
}
