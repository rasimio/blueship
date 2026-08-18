package gateway

import (
	"context"
	"encoding/base64"
	"log/slog"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/attachment"
	bs "github.com/rasimio/blueship/internal/core"
)

// attachmentGetter is the read slice of the AttachmentSink history hydration
// needs: one scoped lookup, so a guessed id never crosses tenants.
type attachmentGetter interface {
	Get(ctx context.Context, userID, soulID, id uuid.UUID) (*bs.AttachmentRecord, []byte, error)
}

// historyMediaExpander returns the loop hook that re-inlines the newest
// history images referenced by `[attached: UUID]` markers in user rows, or
// nil when the host has no CDN or the knob is off.
//
// The durable envelope of a media message deliberately stores no bytes —
// only the caption and one marker per file (appendInteractionUser). The
// dialogue window is rebuilt from those rows, so from its second turn onward
// the model used to know a photo existed without ever being able to look at
// it again: it would edit, redraw, or reason about "the picture" from memory
// of its own earlier words. This hook closes that loop for the window's most
// recent images.
func (g *Gateway) historyMediaExpander(us *UserState) func(context.Context, []bs.Message) []bs.Message {
	if g.deps == nil || g.deps.AttachmentSink == nil || us == nil {
		return nil
	}
	budget := g.deps.Config.Gateway.HydrateHistoryImages
	if budget <= 0 {
		return nil
	}
	sink := g.deps.AttachmentSink
	logger := g.logger
	userID, soulID := us.UserID, us.SoulID
	return func(ctx context.Context, messages []bs.Message) []bs.Message {
		return hydrateHistoryImages(ctx, sink, logger, userID, soulID, budget, messages)
	}
}

// hydrateHistoryImages walks the window newest-first and appends a vision
// block for each image marker until the budget is spent. Newest rows win the
// cap because the photo under discussion is far more often the last one sent
// than the first.
//
// Shape rules:
//   - only role=user rows are scanned: an assistant-side marker is a dispatch
//     sentinel for a file the soul sent, and its delivery already resolved it;
//   - a row already carrying an image block is left untouched — the current
//     turn's overlay owns its own payload;
//   - marker text is kept verbatim so the prompt bytes of a row change only
//     when its images appear or fall off the cap, not on every turn;
//   - a missing row, a non-image kind, or empty bytes degrade to the marker
//     text the row already carries — losing pixels must never lose the turn.
func hydrateHistoryImages(
	ctx context.Context,
	sink attachmentGetter,
	logger *slog.Logger,
	userID, soulID uuid.UUID,
	budget int,
	messages []bs.Message,
) []bs.Message {
	remaining := budget
	hydrated := 0
	for i := len(messages) - 1; i >= 0 && remaining > 0; i-- {
		msg := &messages[i]
		if msg.Role != "user" {
			continue
		}
		blocks := bs.NormalizeContent(msg.Content)
		if blocksContainImage(blocks) {
			continue
		}
		ids := markerIDsInBlocks(blocks)
		if len(ids) == 0 {
			continue
		}
		added := make([]bs.ContentBlock, 0, len(ids))
		for _, id := range ids {
			if remaining == 0 {
				break
			}
			rec, data, err := sink.Get(ctx, userID, soulID, id)
			if err != nil || rec == nil {
				if logger != nil {
					logger.Info("history-media: marker did not resolve, keeping it as text",
						"attachment_id", id.String(), "error", err)
				}
				continue
			}
			if rec.Kind != "image" || len(data) == 0 {
				continue
			}
			media := attachment.MimeForImage(data)
			if media == "" {
				media = "image/jpeg"
			}
			added = append(added, bs.ContentBlock{Type: "image", Source: &bs.ImageSource{
				Type: "base64", MediaType: media, Data: base64.StdEncoding.EncodeToString(data),
			}})
			remaining--
		}
		if len(added) == 0 {
			continue
		}
		msg.Content = append(blocks, added...)
		hydrated += len(added)
	}
	if hydrated > 0 && logger != nil {
		logger.Info("history-media: images re-inlined into the dialogue window",
			"images", hydrated, "budget", budget)
	}
	return messages
}

func blocksContainImage(blocks []bs.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "image" {
			return true
		}
	}
	return false
}

// markerIDsInBlocks collects attachment ids referenced by the text blocks, in
// text order, de-duped across blocks.
func markerIDsInBlocks(blocks []bs.ContentBlock) []uuid.UUID {
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		found, _, ok := bs.ParseAttachmentMarkers(b.Text)
		if !ok {
			continue
		}
		for _, id := range found {
			if seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}
