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
}

// AutonomousTurnDraft is an immutable candidate produced from the live chat
// session. NoOp means the cortex chose not to contact the user (or the anchor
// was already stale).
type AutonomousTurnDraft struct {
	Text            string
	SessionID       string
	DialogMessageID string
	ActivityToken   string
	NoOp            bool
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
