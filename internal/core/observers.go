package core

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Observers let a host see what the gateway did without being asked to
// decide anything: a /start it received, a message it delivered, a
// payment it approved. They exist for measurement — a funnel, a report —
// and must not change what the bot does.
//
// A host opts in by implementing one or more of these interfaces on the
// value it already passes as BotOnboarding; the gateway checks for them
// at the moment it needs them. Every call is bounded by a short deadline,
// its errors are the host's to log, and a panic inside one is recovered:
// an observer that fails leaves the conversation exactly as it would have
// been without it.

// StartSeen is a /start the bot received in a private chat.
type StartSeen struct {
	BotID    uuid.UUID
	TGUserID int64
	TGChatID int64
	// Payload is the deep-link parameter as Telegram delivered it, already
	// checked against Telegram's own alphabet ([A-Za-z0-9_-], ≤ 64). Empty
	// for a bare /start or a parameter that could not have come from a link.
	Payload string
	// MessageID identifies the message within its chat; with the bot and
	// chat it is a stable key for the /start, so a redelivered update can
	// be recognised.
	MessageID int64
	At        time.Time
}

// StartObserver is told about every /start that reaches onboarding —
// before onboarding runs, so whatever the host records exists by the time
// an account is created from it. Deep-link logins, account links and
// errands (a payload naming a host command) are not /starts in this
// sense and are not reported.
type StartObserver interface {
	ObserveStart(ctx context.Context, s StartSeen)
}

// Delivery kinds.
const (
	DeliveryDenial      = "denial"
	DeliveryHostCommand = "host_command"
)

// DeliverySeen is a message Telegram accepted.
type DeliverySeen struct {
	BotID    uuid.UUID
	TGChatID int64
	Kind     string // DeliveryDenial or DeliveryHostCommand
	// Name is the refusal reason for a denial, the command for a host
	// command reply.
	Name string
	// MessageID is the id Telegram assigned: proof of delivery.
	MessageID int64
	// What the message offered, by kind of button.
	URLButtons      int
	InvoiceButtons  int
	CallbackButtons int
	At              time.Time
}

// DeliveryObserver is told when a refusal or a host command's reply was
// accepted by Telegram. Nothing is reported for a send that failed.
type DeliveryObserver interface {
	ObserveDelivery(ctx context.Context, d DeliverySeen)
}

// PaymentApproved is a pre-checkout the host approved and the gateway
// answered: the buyer is about to be charged.
type PaymentApproved struct {
	BotID    uuid.UUID
	TGUserID int64
	QueryID  string
	Payload  string
	Currency string
	Amount   int
	At       time.Time
}

// PaymentObserver is told about approved pre-checkouts.
type PaymentObserver interface {
	ObservePaymentApproved(ctx context.Context, p PaymentApproved)
}
