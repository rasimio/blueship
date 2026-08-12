package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const autonomousTurnNotificationPrefix = "[blueship:autonomous-turn:v1]"

// AutonomousTurnMessageIDs derives the two chat rows belonging to one durable
// notification attempt. Stable ids make the gateway's eager append and the
// journal's confirmation-time repair safely converge without duplicate turns.
func AutonomousTurnMessageIDs(attemptID uuid.UUID) (boundaryID, assistantID uuid.UUID) {
	return uuid.NewSHA1(attemptID, []byte("autonomous-turn-boundary/v1")),
		uuid.NewSHA1(attemptID, []byte("autonomous-turn-assistant/v1"))
}

// AutonomousTurnRequest asks the live chat cortex for an optional assistant
// turn without inventing a user message. Prompt is a prompt-only event: it is
// sent to the provider but never persisted in chat_messages.
type AutonomousTurnRequest struct {
	UserID          uuid.UUID
	SoulID          uuid.UUID
	AnchorMessageID string
	Prompt          string
	// Tools names what the soul may reach for during this turn. Empty — the
	// default, and what every cadence-driven turn uses — means none, and the
	// turn is a single pass that either produces prose or nothing.
	//
	// A turn that is allowed tools is a different animal: it needs more than
	// one pass, because the first one is spent calling and the answer only
	// exists after the result comes back. So MaxTurns follows from this field
	// rather than being a second knob a caller can get wrong.
	//
	// The reason to grant any at all is that some prompts are not "say
	// something" but "do something and then say it" — draw a picture, look up
	// the week, attach a file. Without tools the soul can only describe what
	// it would have done.
	Tools []string
}

// AutonomousTurnToolPasses bounds a tool-using autonomous turn: one pass to
// call, one to answer with the result, and a little room for a soul that
// reaches twice. Deliberately small — this is a single unprompted message,
// not an agent loop, and an unbounded one would let a cadence tick turn into
// a research session nobody asked for.
const AutonomousTurnToolPasses = 4

// Reasons a draft can come back empty. NoOp on its own says a turn was not
// produced; it does not say whether that was a decision or a protection, and
// callers need to tell those apart. A handler retrying "the model declined"
// is reasonable; retrying "the user is typing right now" is a bug that
// interrupts a live conversation.
const (
	// AutonomousNoOpUserActive — the person wrote, or is writing, after the
	// anchor this draft was requested for. Never retry: they are here.
	AutonomousNoOpUserActive = "user_active"
	// AutonomousNoOpNoDialog — the session carries no dialog message to
	// anchor a turn to. Structural, not a judgement.
	AutonomousNoOpNoDialog = "no_dialog"
	// AutonomousNoOpRuleSilent — an active rule with Silent matched. This is
	// configuration expressing intent, so it is honoured as written.
	AutonomousNoOpRuleSilent = "rule_silent"
	// AutonomousNoOpModelDeclined — the cortex returned [no-op] or produced
	// no visible text. The only reason that reflects a choice rather than a
	// fact about the conversation, and the only one worth reconsidering.
	AutonomousNoOpModelDeclined = "model_declined"
)

// AutonomousTurnDraft is an immutable candidate produced from the live chat
// session. NoOp means no turn was produced; NoOpReason says why, and is one of
// the AutonomousNoOp* constants above.
type AutonomousTurnDraft struct {
	Text            string
	SessionID       string
	DialogMessageID string
	ActivityToken   string
	NoOp            bool
	NoOpReason      string
}

// AutonomousTurnDrafter is the late-bound gateway capability exposed to
// recurring handlers. Ship wires it only after the gateway exists.
type AutonomousTurnDrafter func(context.Context, AutonomousTurnRequest) (AutonomousTurnDraft, error)

// AutonomousTurnCommit is the durable notification payload. It contains the
// exact generated text so delivery retries never regenerate a different turn.
type AutonomousTurnCommit struct {
	UserID          uuid.UUID `json:"user_id"`
	SoulID          uuid.UUID `json:"soul_id"`
	AnchorMessageID string    `json:"anchor_message_id"`
	SessionID       string    `json:"session_id"`
	DialogMessageID string    `json:"dialog_message_id"`
	ActivityToken   string    `json:"activity_token"`
	Text            string    `json:"text"`
}

// FormatAutonomousTurnNotification wraps a generated draft in an opaque,
// versioned task-notification marker. The scheduler journals this exact string;
// the gateway unwraps it at the single-attempt delivery boundary.
func FormatAutonomousTurnNotification(req AutonomousTurnRequest, draft AutonomousTurnDraft) (string, error) {
	text := strings.TrimSpace(draft.Text)
	if req.UserID == uuid.Nil || req.SoulID == uuid.Nil {
		return "", fmt.Errorf("autonomous turn notification: user and soul are required")
	}
	if strings.TrimSpace(req.AnchorMessageID) == "" {
		return "", fmt.Errorf("autonomous turn notification: anchor message is required")
	}
	if draft.NoOp || text == "" {
		return "", fmt.Errorf("autonomous turn notification: draft is not deliverable")
	}
	if strings.TrimSpace(draft.SessionID) == "" {
		return "", fmt.Errorf("autonomous turn notification: session is required")
	}
	if strings.TrimSpace(draft.DialogMessageID) == "" {
		return "", fmt.Errorf("autonomous turn notification: dialog message is required")
	}
	if strings.TrimSpace(draft.ActivityToken) == "" {
		return "", fmt.Errorf("autonomous turn notification: activity token is required")
	}
	payload, err := json.Marshal(AutonomousTurnCommit{
		UserID:          req.UserID,
		SoulID:          req.SoulID,
		AnchorMessageID: req.AnchorMessageID,
		SessionID:       draft.SessionID,
		DialogMessageID: draft.DialogMessageID,
		ActivityToken:   draft.ActivityToken,
		Text:            text,
	})
	if err != nil {
		return "", fmt.Errorf("autonomous turn notification: marshal: %w", err)
	}
	return autonomousTurnNotificationPrefix + "\n" +
		base64.RawStdEncoding.EncodeToString(payload), nil
}

// ParseAutonomousTurnNotification identifies and decodes a marker produced by
// FormatAutonomousTurnNotification. matched=false means ordinary notification
// text and is not an error.
func ParseAutonomousTurnNotification(text string) (commit AutonomousTurnCommit, matched bool, err error) {
	raw := strings.TrimSpace(text)
	if !strings.HasPrefix(raw, autonomousTurnNotificationPrefix) {
		return AutonomousTurnCommit{}, false, nil
	}
	encoded, ok := strings.CutPrefix(raw, autonomousTurnNotificationPrefix+"\n")
	if !ok || strings.TrimSpace(encoded) == "" {
		return AutonomousTurnCommit{}, true, fmt.Errorf("autonomous turn notification: malformed marker")
	}
	payload, decodeErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	if decodeErr != nil {
		return AutonomousTurnCommit{}, true, fmt.Errorf("autonomous turn notification: decode: %w", decodeErr)
	}
	if unmarshalErr := json.Unmarshal(payload, &commit); unmarshalErr != nil {
		return AutonomousTurnCommit{}, true, fmt.Errorf("autonomous turn notification: unmarshal: %w", unmarshalErr)
	}
	commit.Text = strings.TrimSpace(commit.Text)
	if commit.UserID == uuid.Nil || commit.SoulID == uuid.Nil ||
		strings.TrimSpace(commit.AnchorMessageID) == "" ||
		strings.TrimSpace(commit.SessionID) == "" ||
		strings.TrimSpace(commit.DialogMessageID) == "" ||
		strings.TrimSpace(commit.ActivityToken) == "" || commit.Text == "" {
		return AutonomousTurnCommit{}, true, fmt.Errorf("autonomous turn notification: incomplete payload")
	}
	return commit, true, nil
}
