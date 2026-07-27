package core

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAutonomousTurnNotificationRoundTrip(t *testing.T) {
	req := AutonomousTurnRequest{
		UserID:          uuid.New(),
		SoulID:          uuid.New(),
		AnchorMessageID: uuid.NewString(),
		Prompt:          "provider-only prompt",
	}
	draft := AutonomousTurnDraft{
		Text:            "  exact generated text  ",
		SessionID:       uuid.NewString(),
		DialogMessageID: uuid.NewString(),
		ActivityToken:   uuid.NewString() + ":7",
	}

	marker, err := FormatAutonomousTurnNotification(req, draft)
	if err != nil {
		t.Fatalf("format marker: %v", err)
	}
	if strings.Contains(marker, draft.Text) {
		t.Fatal("marker should keep generated text opaque")
	}
	commit, matched, err := ParseAutonomousTurnNotification(marker)
	if err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	if !matched {
		t.Fatal("formatted marker was not recognized")
	}
	if commit.UserID != req.UserID || commit.SoulID != req.SoulID ||
		commit.AnchorMessageID != req.AnchorMessageID ||
		commit.SessionID != draft.SessionID ||
		commit.DialogMessageID != draft.DialogMessageID ||
		commit.ActivityToken != draft.ActivityToken ||
		commit.Text != strings.TrimSpace(draft.Text) {
		t.Fatalf("round-trip mismatch: %#v", commit)
	}
}

func TestParseAutonomousTurnNotificationLeavesOrdinaryTextAlone(t *testing.T) {
	_, matched, err := ParseAutonomousTurnNotification("ordinary notification")
	if err != nil || matched {
		t.Fatalf("ordinary text matched=%t err=%v", matched, err)
	}
}

func TestParseAutonomousTurnNotificationRejectsMalformedMarker(t *testing.T) {
	_, matched, err := ParseAutonomousTurnNotification(autonomousTurnNotificationPrefix + "\nnot-base64")
	if !matched || err == nil {
		t.Fatalf("malformed marker matched=%t err=%v", matched, err)
	}
}

func TestFormatAutonomousTurnNotificationRejectsNoOp(t *testing.T) {
	_, err := FormatAutonomousTurnNotification(AutonomousTurnRequest{
		UserID:          uuid.New(),
		SoulID:          uuid.New(),
		AnchorMessageID: uuid.NewString(),
	}, AutonomousTurnDraft{SessionID: uuid.NewString(), NoOp: true})
	if err == nil {
		t.Fatal("expected no-op draft rejection")
	}
}

func TestAutonomousTurnMessageIDsAreStableAndDistinct(t *testing.T) {
	attemptID := uuid.New()
	firstBoundary, firstAssistant := AutonomousTurnMessageIDs(attemptID)
	secondBoundary, secondAssistant := AutonomousTurnMessageIDs(attemptID)

	if firstBoundary != secondBoundary || firstAssistant != secondAssistant {
		t.Fatal("autonomous message ids are not deterministic")
	}
	if firstBoundary == firstAssistant || firstBoundary == uuid.Nil || firstAssistant == uuid.Nil {
		t.Fatalf("invalid autonomous message ids: boundary=%s assistant=%s", firstBoundary, firstAssistant)
	}
	otherBoundary, otherAssistant := AutonomousTurnMessageIDs(uuid.New())
	if firstBoundary == otherBoundary || firstAssistant == otherAssistant {
		t.Fatal("different notification attempts reused chat message ids")
	}
}
