package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// The receipt is the only record we get. Telegram sends it once, on a
// message with no text and no attachment, and the money has already
// moved — so every field a host needs to grant the right thing to the
// right person, exactly once, has to survive the crossing.
func TestSuccessfulPaymentCarriesEverythingNeededToGrant(t *testing.T) {
	expires := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	msg := &telegram.Message{
		Chat: telegram.Chat{ID: 4242},
		From: &telegram.User{ID: 777},
		SuccessfulPayment: &telegram.SuccessfulPayment{
			Currency:                   "XTR",
			TotalAmount:                299,
			InvoicePayload:             "plus:telegram_stars",
			TelegramPaymentChargeID:    "charge_abc",
			SubscriptionExpirationDate: expires.Unix(),
			IsRecurring:                true,
			IsFirstRecurring:           true,
		},
	}

	var got core.PaymentReceipt
	g := gatewayWithPaymentHooks(nil, func(_ context.Context, in core.PaymentReceipt) error {
		got = in
		return nil
	})
	g.handleSuccessfulPayment(context.Background(), msg)

	if got.ChargeID != "charge_abc" {
		t.Errorf("charge id = %q; without it a host cannot tell a retry from a second purchase", got.ChargeID)
	}
	if got.UserID != 777 || got.ChatID != 4242 {
		t.Errorf("got user %d chat %d; the payment would be credited to nobody", got.UserID, got.ChatID)
	}
	if got.Amount != 299 || got.Currency != "XTR" {
		t.Errorf("got %d %s, want 299 XTR", got.Amount, got.Currency)
	}
	if got.Payload != "plus:telegram_stars" {
		t.Errorf("payload = %q; it is the only thing saying what was bought", got.Payload)
	}
	if !got.Recurring || !got.FirstPayment {
		t.Errorf("recurring=%v first=%v; a renewal and an opening charge must be distinguishable", got.Recurring, got.FirstPayment)
	}
	if !got.PeriodEnd.Equal(expires.UTC()) {
		t.Errorf("period end = %v, want %v — the grant would expire on a guess", got.PeriodEnd, expires.UTC())
	}
}

// A one-off purchase has no expiry of its own. Zero has to reach the
// host as zero, so it decides the term rather than inheriting 1970.
func TestOneOffPaymentHasNoPeriodEnd(t *testing.T) {
	var got core.PaymentReceipt
	g := gatewayWithPaymentHooks(nil, func(_ context.Context, in core.PaymentReceipt) error {
		got = in
		return nil
	})
	g.handleSuccessfulPayment(context.Background(), &telegram.Message{
		Chat: telegram.Chat{ID: 1}, From: &telegram.User{ID: 1},
		SuccessfulPayment: &telegram.SuccessfulPayment{
			Currency: "XTR", TotalAmount: 299, TelegramPaymentChargeID: "c",
		},
	})
	if !got.PeriodEnd.IsZero() {
		t.Errorf("period end = %v, want zero", got.PeriodEnd)
	}
}

// Refusing has to be a decision the host makes. Nothing about a purchase
// should be honoured because a hook was left unset.
func TestPreCheckoutRefusesWhenTheHostHasNoOpinion(t *testing.T) {
	g := gatewayWithPaymentHooks(nil, nil)
	if hook := g.deps.Config.Gateway.ApprovePayment; hook != nil {
		t.Fatal("test set up wrong: an approve hook is present")
	}
}

// The hook's error is what the buyer reads on the payment sheet, so it
// must survive to the answer rather than being replaced by a generic one.
func TestPreCheckoutPassesTheHostRefusalThrough(t *testing.T) {
	refusal := errors.New("Подписка уже активна.")
	var seen core.PreCheckout
	g := gatewayWithPaymentHooks(func(_ context.Context, in core.PreCheckout) error {
		seen = in
		return refusal
	}, nil)

	approve := g.deps.Config.Gateway.ApprovePayment
	err := approve(context.Background(), core.PreCheckout{
		Currency: "XTR", Amount: 299, Payload: "plus:telegram_stars", UserID: 5,
	})
	if err == nil || err.Error() != refusal.Error() {
		t.Fatalf("refusal = %v, want %v", err, refusal)
	}
	if seen.Payload != "plus:telegram_stars" || seen.Amount != 299 {
		t.Errorf("the hook saw %+v; it cannot decide on a purchase it cannot identify", seen)
	}
}

func gatewayWithPaymentHooks(approve core.ApprovePaymentFunc, received core.PaymentReceivedFunc) *Gateway {
	cfg := core.Config{}
	cfg.Gateway.ApprovePayment = approve
	cfg.Gateway.PaymentReceived = received
	return &Gateway{deps: &core.Deps{Config: &cfg}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}
