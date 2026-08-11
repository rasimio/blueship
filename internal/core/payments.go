package core

import (
	"context"
	"errors"
	"time"
)

// In-chat payments.
//
// The transport carries money for a host that sells something; the
// framework knows nothing about what is being sold, what it costs or who
// is entitled to it. It knows only the two moments a payment provider
// insists a bot participate in: a confirmation that must be answered
// within seconds, and a receipt that must be recorded exactly once.

// PreCheckout is a purchase a person has confirmed and not yet paid for.
//
// The provider holds the transaction open while it waits for an answer,
// and the window is about ten seconds — a host that reaches over the
// network here should carry its own, shorter deadline. No answer reads
// as a refusal, and the person is told the payment failed.
type PreCheckout struct {
	ChatID   int64
	UserID   int64
	Currency string
	// Amount is in the currency's smallest unit, except for currencies
	// that have none — Telegram Stars (XTR) counts whole stars.
	Amount int
	// Payload is whatever the host put on the invoice. It is the only
	// thing tying this confirmation back to what was being bought, and it
	// never leaves our side, so it is the right place for a plan id.
	Payload string
}

// PaymentReceipt is money that has actually moved.
//
// Delivered at least once: providers retry, and a person can be charged
// again on renewal under the same subscription. ChargeID is unique per
// charge and is what a host must key on to stay idempotent — granting
// twice is the failure this field exists to prevent.
type PaymentReceipt struct {
	ChatID   int64
	UserID   int64
	Currency string
	Amount   int
	Payload  string
	ChargeID string
	// Recurring marks a renewal of an existing subscription; FirstPayment
	// marks the initial charge of one. Both false means a one-off.
	Recurring    bool
	FirstPayment bool
	// PeriodEnd is when the paid period runs out. Zero for a one-off
	// purchase, where how long it is worth is the host's decision.
	PeriodEnd time.Time
}

// ApprovePaymentFunc decides whether a confirmed purchase may be charged.
//
// Returning an error refuses the payment, and the error's text is shown
// to the person by the payment provider — so it must read as an
// explanation, in their language, not as a diagnostic.
type ApprovePaymentFunc func(ctx context.Context, in PreCheckout) error

// PaymentReceivedFunc records a completed payment. An error is logged and
// the receipt is lost: the provider has already taken the money and will
// not deliver this again on its own, so a host that cannot record it
// immediately should persist the receipt and reconcile later rather than
// fail here.
type PaymentReceivedFunc func(ctx context.Context, in PaymentReceipt) error

// ErrPaymentUnavailable is what a buyer is told when the bot cannot take
// money at all — no host hook, so nothing on our side could honour the
// purchase. Phrased for the person looking at the payment sheet, since
// that is where the provider prints it.
var ErrPaymentUnavailable = errors.New("Оплата сейчас недоступна. Попробуй позже или напиши в поддержку.")
