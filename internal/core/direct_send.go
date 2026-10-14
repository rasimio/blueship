package core

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DirectMessage is one message a host composed in full, for one chat of
// one of the gateway's bots: Telegram HTML exactly as given, an optional
// photo, and buttons. It bypasses the conversation entirely — nothing is
// written to chat history, no turn starts, no quota is spent — which is
// what a mailing needs and a reply must never do.
type DirectMessage struct {
	BotID  uuid.UUID
	ChatID int64
	// HTML is the text, or the photo's caption. Telegram's HTML subset;
	// the host escapes everything it did not mean as markup.
	HTML    string
	Photo   *DirectPhoto
	Buttons [][]DirectButton
	// Silent delivers without a sound.
	Silent bool
	// Bulk sends yield to conversation traffic under the bot's shared
	// send allowance, and wait out a flood-control answer before the next
	// one goes.
	Bulk bool
}

// DirectPhoto is the photo of a DirectMessage: a file id this bot got for
// it earlier, or the bytes to upload.
type DirectPhoto struct {
	FileID string
	Name   string
	MIME   string
	Data   []byte
}

// DirectButton is a link button (URL) or a button that runs a host
// command (Command, with Args) for whoever taps it — the gateway fills in
// who that is, never the button.
type DirectButton struct {
	Text    string
	URL     string
	Command string
	Args    string
}

// DirectReceipt is Telegram's acknowledgement of a delivered message.
type DirectReceipt struct {
	MessageID int
	// PhotoFileID lets later messages from the same bot reuse the photo.
	PhotoFileID string
	DeliveredAt time.Time
}

// DirectOutcome classifies a failed direct send by what it means for the
// message, which is what a sender decides retries on.
type DirectOutcome string

const (
	// DirectBotUnavailable: the bot is not running in this process (not
	// loaded yet, or removed). Nothing was sent, and nothing about the
	// recipient is known — it says no more about them than about anyone
	// else that bot would have reached.
	DirectBotUnavailable DirectOutcome = "bot_unavailable"
	// DirectNotSent: the request never reached Telegram. Safe to send
	// again.
	DirectNotSent DirectOutcome = "not_sent"
	// DirectRateLimited: Telegram refused under flood control and created
	// nothing. Safe to send again after RetryAfter.
	DirectRateLimited DirectOutcome = "rate_limited"
	// DirectRejected: Telegram refused the request for good (the person
	// blocked the bot, the chat is gone, the content is invalid). Created
	// nothing; sending the same thing again gets the same answer.
	DirectRejected DirectOutcome = "rejected"
	// DirectUnknown: the request went out and no clear answer came back.
	// The message may be in the chat. Sending again risks a duplicate.
	DirectUnknown DirectOutcome = "unknown"
)

// DirectSendError is a failed direct send. Description is Telegram's own
// words when it answered; it never carries the bot token.
type DirectSendError struct {
	Outcome     DirectOutcome
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *DirectSendError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("direct send %s (%d): %s", e.Outcome, e.Code, e.Description)
	}
	return fmt.Sprintf("direct send %s: %s", e.Outcome, e.Description)
}

// DirectSender delivers a DirectMessage with exactly one Telegram request.
type DirectSender func(ctx context.Context, msg DirectMessage) (DirectReceipt, error)
