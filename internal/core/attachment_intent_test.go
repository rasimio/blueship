package core

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Delivering a file used to depend on the model copying a UUID into its prose.
// Production showed the failure: a soul drew a picture, called the tool, wrote
// «смотри, что у меня вышло», and shipped no file. The tool call is the
// intent; the text is only where it usually shows up.
func TestEnsureAttachmentMarkersRecoversAForgottenFile(t *testing.T) {
	drew := uuid.New()

	out, added := EnsureAttachmentMarkers("Смотри, что у меня вышло (^_^)", []uuid.UUID{drew})
	if len(added) != 1 || added[0] != drew {
		t.Fatalf("added = %v, want the forgotten attachment recovered", added)
	}
	ids, _, ok := ParseAttachmentMarkers(out)
	if !ok || len(ids) != 1 || ids[0] != drew {
		t.Fatalf("recovered text carries %v, want one marker for %s", ids, drew)
	}
	if !strings.Contains(out, "Смотри, что у меня вышло") {
		t.Fatalf("recovery ate the message: %q", out)
	}
}

// Where the model did place a marker, its placement is part of the message.
// Appending a duplicate would send the same picture twice.
func TestEnsureAttachmentMarkersLeavesAPlacedMarkerAlone(t *testing.T) {
	drew := uuid.New()
	text := "вот он ты\n\n[attached: " + drew.String() + "]\n\nа это подпись"

	out, added := EnsureAttachmentMarkers(text, []uuid.UUID{drew})
	if len(added) != 0 {
		t.Fatalf("added = %v, want nothing — the marker was already there", added)
	}
	if out != text {
		t.Fatalf("text was rewritten:\n%q\n%q", text, out)
	}
	if ids, _, _ := ParseAttachmentMarkers(out); len(ids) != 1 {
		t.Fatalf("marker count = %d, want 1 — a duplicate would send the file twice", len(ids))
	}
}

func TestEnsureAttachmentMarkersRecoversOnlyTheMissingOnes(t *testing.T) {
	placed, forgotten := uuid.New(), uuid.New()
	text := "две картинки\n\n[attached: " + placed.String() + "]"

	out, added := EnsureAttachmentMarkers(text, []uuid.UUID{placed, forgotten})
	if len(added) != 1 || added[0] != forgotten {
		t.Fatalf("added = %v, want only the forgotten one", added)
	}
	ids, _, _ := ParseAttachmentMarkers(out)
	if len(ids) != 2 {
		t.Fatalf("markers = %d, want both files exactly once", len(ids))
	}
}

func TestEnsureAttachmentMarkersLeavesPlainTextAlone(t *testing.T) {
	const plain = "просто сообщение"
	if out, added := EnsureAttachmentMarkers(plain, nil); out != plain || added != nil {
		t.Fatalf("plain text was touched: %q, added %v", out, added)
	}
}

// The collector is per-turn and deduplicating: a model that calls the tool
// twice for the same file must not produce two sends.
func TestAttachmentIntentsCollect(t *testing.T) {
	ctx, intents := WithAttachmentIntents(context.Background())
	first, second := uuid.New(), uuid.New()

	RecordAttachmentIntent(ctx, first)
	RecordAttachmentIntent(ctx, second)
	RecordAttachmentIntent(ctx, first)
	RecordAttachmentIntent(ctx, uuid.Nil)

	got := intents.IDs()
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("ids = %v, want both files once each in call order", got)
	}
}

// A host that never arms collection keeps working — recording is a no-op
// rather than a panic, so wiring this in one path does not break the others.
func TestRecordAttachmentIntentWithoutCollectionIsHarmless(t *testing.T) {
	RecordAttachmentIntent(context.Background(), uuid.New())
	var none *AttachmentIntents
	if ids := none.IDs(); ids != nil {
		t.Fatalf("nil collector returned %v", ids)
	}
}
