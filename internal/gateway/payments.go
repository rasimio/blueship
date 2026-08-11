package gateway

import (
	"context"
	"time"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// preCheckoutDeadline is deliberately shorter than the roughly ten
// seconds Telegram allows. A host hook that reaches over the network can
// hang; if it does, refusing on our own terms tells the buyer something,
// whereas running out Telegram's clock tells them the payment failed with
// no reason at all.
const preCheckoutDeadline = 6 * time.Second

// handlePreCheckout answers Telegram's confirmation step.
//
// Every path answers. Returning without answering is the one outcome
// with no recovery: the payment is cancelled on the buyer's screen and
// nothing here would say why.
func (g *Gateway) handlePreCheckout(ctx context.Context, bi *botInstance, q *telegram.PreCheckoutQuery) {
	approve := g.deps.Config.Gateway.ApprovePayment
	if approve == nil {
		// A bot with no host opinion must not take money. Refusing is
		// the honest outcome, and it is loud on our side because it can
		// only mean the deployment is misconfigured.
		g.logger.Error("payments: pre-checkout with no ApprovePayment hook; refusing",
			"payload", q.InvoicePayload, "currency", q.Currency, "amount", q.TotalAmount)
		g.answerPreCheckout(ctx, bi, q, core.ErrPaymentUnavailable.Error())
		return
	}

	in := core.PreCheckout{
		Currency: q.Currency,
		Amount:   q.TotalAmount,
		Payload:  q.InvoicePayload,
	}
	if q.From != nil {
		in.UserID = q.From.ID
		in.ChatID = q.From.ID
	}

	hookCtx, cancel := context.WithTimeout(ctx, preCheckoutDeadline)
	defer cancel()
	if err := approve(hookCtx, in); err != nil {
		g.logger.Warn("payments: purchase refused",
			"payload", q.InvoicePayload, "user_id", in.UserID, "error", err)
		g.answerPreCheckout(ctx, bi, q, err.Error())
		return
	}
	g.answerPreCheckout(ctx, bi, q, "")
}

func (g *Gateway) answerPreCheckout(ctx context.Context, bi *botInstance, q *telegram.PreCheckoutQuery, refusal string) {
	if err := bi.client.AnswerPreCheckoutQuery(ctx, q.ID, refusal); err != nil {
		// Nothing left to try: the buyer is already being told the
		// payment failed, and there is no second chance at this query.
		g.logger.Error("payments: could not answer the pre-checkout query",
			"payload", q.InvoicePayload, "refused", refusal != "", "error", err)
	}
}

// handleSuccessfulPayment hands a completed payment to the host.
//
// This arrives on a message with no text and no attachment — the exact
// shape the inbound path discards — so it is dispatched before any of
// that, and a receipt that is not recorded is an Error rather than a
// dropped update. The money has already moved by the time we see it.
func (g *Gateway) handleSuccessfulPayment(ctx context.Context, msg *telegram.Message) {
	p := msg.SuccessfulPayment
	received := g.deps.Config.Gateway.PaymentReceived
	if received == nil {
		g.logger.Error("payments: payment received with no PaymentReceived hook; the receipt is lost",
			"charge_id", p.TelegramPaymentChargeID, "payload", p.InvoicePayload,
			"currency", p.Currency, "amount", p.TotalAmount)
		return
	}

	in := core.PaymentReceipt{
		ChatID:       msg.Chat.ID,
		Currency:     p.Currency,
		Amount:       p.TotalAmount,
		Payload:      p.InvoicePayload,
		ChargeID:     p.TelegramPaymentChargeID,
		Recurring:    p.IsRecurring,
		FirstPayment: p.IsFirstRecurring,
	}
	if msg.From != nil {
		in.UserID = msg.From.ID
	}
	if p.SubscriptionExpirationDate > 0 {
		in.PeriodEnd = time.Unix(p.SubscriptionExpirationDate, 0).UTC()
	}

	if err := received(ctx, in); err != nil {
		g.logger.Error("payments: a paid charge was not recorded",
			"charge_id", in.ChargeID, "user_id", in.UserID, "error", err)
		return
	}
	g.logger.Info("payments: charge recorded",
		"charge_id", in.ChargeID, "user_id", in.UserID,
		"currency", in.Currency, "amount", in.Amount, "recurring", in.Recurring)
}
