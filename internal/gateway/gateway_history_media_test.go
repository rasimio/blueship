package gateway

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

func seedHistoryImage(t *testing.T, sink *attachmentLookupSink, userID, soulID uuid.UUID, kind string) uuid.UUID {
	t.Helper()
	id, err := sink.Save(context.Background(), bs.AttachmentParams{
		ID:     uuid.New(),
		UserID: userID,
		SoulID: soulID,
		Name:   "photo.jpg",
		Mime:   "image/jpeg",
		Kind:   kind,
		Data:   []byte("raw-bytes-" + kind),
	})
	if err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	return id
}

func marker(id uuid.UUID) string { return "[attached: " + id.String() + "]" }

func imageBlockCount(content any) int {
	n := 0
	for _, b := range bs.NormalizeContent(content) {
		if b.Type == "image" {
			n++
		}
	}
	return n
}

func TestHydrateHistoryImagesNewestFirstUnderCap(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	sink := newAttachmentLookupSink()
	oldID := seedHistoryImage(t, sink, userID, soulID, "image")
	midID := seedHistoryImage(t, sink, userID, soulID, "image")
	newID := seedHistoryImage(t, sink, userID, soulID, "image")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	messages := []bs.Message{
		{Role: "user", Content: "первое фото\n\n" + marker(oldID)},
		{Role: "assistant", Content: "вижу"},
		{Role: "user", Content: "второе\n\n" + marker(midID)},
		{Role: "user", Content: "третье\n\n" + marker(newID)},
	}
	out := hydrateHistoryImages(context.Background(), sink, logger, userID, soulID, 2, messages)

	if got := imageBlockCount(out[3].Content); got != 1 {
		t.Fatalf("newest message: want 1 image block, got %d", got)
	}
	if got := imageBlockCount(out[2].Content); got != 1 {
		t.Fatalf("second-newest message: want 1 image block, got %d", got)
	}
	if got := imageBlockCount(out[0].Content); got != 0 {
		t.Fatalf("oldest message must stay marker-only under the cap, got %d image blocks", got)
	}
	if got := imageBlockCount(out[1].Content); got != 0 {
		t.Fatalf("assistant message must never be hydrated, got %d image blocks", got)
	}
	// The marker text survives hydration so the row's prompt bytes stay
	// stable across turns while the image rides along.
	text := bs.ExtractText(bs.NormalizeContent(out[3].Content))
	if want := marker(newID); !strings.Contains(text, want) {
		t.Fatalf("marker text must survive hydration, text=%q", text)
	}
}

func TestHydrateHistoryImagesSkipsNonImageAndUnknown(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	sink := newAttachmentLookupSink()
	pdfID := seedHistoryImage(t, sink, userID, soulID, "pdf")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	messages := []bs.Message{
		{Role: "user", Content: "документ " + marker(pdfID)},
		{Role: "user", Content: "битая ссылка " + marker(uuid.New())},
	}
	out := hydrateHistoryImages(context.Background(), sink, logger, userID, soulID, 3, messages)

	for i, msg := range out {
		if got := imageBlockCount(msg.Content); got != 0 {
			t.Fatalf("message %d: non-image/unknown markers must not hydrate, got %d image blocks", i, got)
		}
	}
	if text := bs.ExtractText(bs.NormalizeContent(out[1].Content)); text == "" {
		t.Fatal("unresolvable marker must keep its text")
	}
}

func TestHydrateHistoryImagesRespectsTenantScope(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	sink := newAttachmentLookupSink()
	foreignID := seedHistoryImage(t, sink, uuid.New(), uuid.New(), "image")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	messages := []bs.Message{{Role: "user", Content: marker(foreignID)}}
	out := hydrateHistoryImages(context.Background(), sink, logger, userID, soulID, 3, messages)
	if got := imageBlockCount(out[0].Content); got != 0 {
		t.Fatalf("another tenant's attachment must not hydrate, got %d image blocks", got)
	}
}

func TestHydrateHistoryImagesLeavesExistingImagePayloads(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	sink := newAttachmentLookupSink()
	id := seedHistoryImage(t, sink, userID, soulID, "image")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	messages := []bs.Message{{
		Role: "user",
		Content: []bs.ContentBlock{
			{Type: "text", Text: marker(id)},
			{Type: "image", Source: &bs.ImageSource{Type: "base64", MediaType: "image/png", Data: "live"}},
		},
	}}
	out := hydrateHistoryImages(context.Background(), sink, logger, userID, soulID, 3, messages)
	if got := imageBlockCount(out[0].Content); got != 1 {
		t.Fatalf("a message already carrying its payload must stay untouched, got %d image blocks", got)
	}
}

func TestHistoryMediaExpanderGates(t *testing.T) {
	sink := newAttachmentLookupSink()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}

	off := &Gateway{
		deps:   &bs.Deps{Config: &bs.Config{}, AttachmentSink: sink},
		logger: logger,
	}
	if off.historyMediaExpander(us) != nil {
		t.Fatal("knob at zero must disable hydration")
	}

	noSink := &Gateway{deps: &bs.Deps{Config: &bs.Config{
		Gateway: bs.GatewayConfig{HydrateHistoryImages: 3},
	}}, logger: logger}
	if noSink.historyMediaExpander(us) != nil {
		t.Fatal("no CDN must disable hydration")
	}

	on := &Gateway{deps: &bs.Deps{Config: &bs.Config{
		Gateway: bs.GatewayConfig{HydrateHistoryImages: 3},
	}, AttachmentSink: sink}, logger: logger}
	if on.historyMediaExpander(us) == nil {
		t.Fatal("sink + positive knob must enable hydration")
	}
}
