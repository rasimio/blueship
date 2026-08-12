package core

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// Attaching a file depends on the model copying a UUID into its prose.
//
// attachment_include returns a `[attached: UUID]` marker and asks the model to
// paste it; the delivery path only ships files it finds markers for. That
// works often enough in ordinary chat and fails exactly where it matters most:
// an unprompted turn, where the model is composing one careful message and
// drops the token on the way. Production did it — a soul drew a picture, wrote
// "смотри, что у меня вышло", called the tool, and delivered no file.
//
// So the call itself is the intent. A tool that resolved an attachment
// records it here, and the turn reconciles what was asked for against what the
// text actually carries. Nothing about the model's compliance is assumed.

type attachmentIntentKey struct{}

// AttachmentIntents collects the attachments a single turn asked to include.
type AttachmentIntents struct {
	mu   sync.Mutex
	ids  []uuid.UUID
	seen map[uuid.UUID]bool
}

// WithAttachmentIntents arms collection for one turn. Callers that do not arm
// it lose nothing: RecordAttachmentIntent becomes a no-op, so a host wiring
// this only where it needs it stays correct.
func WithAttachmentIntents(ctx context.Context) (context.Context, *AttachmentIntents) {
	intents := &AttachmentIntents{seen: map[uuid.UUID]bool{}}
	return context.WithValue(ctx, attachmentIntentKey{}, intents), intents
}

// RecordAttachmentIntent notes that this turn asked to attach id.
func RecordAttachmentIntent(ctx context.Context, id uuid.UUID) {
	intents, _ := ctx.Value(attachmentIntentKey{}).(*AttachmentIntents)
	if intents == nil || id == uuid.Nil {
		return
	}
	intents.mu.Lock()
	defer intents.mu.Unlock()
	if intents.seen[id] {
		return
	}
	intents.seen[id] = true
	intents.ids = append(intents.ids, id)
}

// IDs returns what the turn asked to attach, in call order.
func (a *AttachmentIntents) IDs() []uuid.UUID {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uuid.UUID(nil), a.ids...)
}

// EnsureAttachmentMarkers appends a marker for every recorded intent the text
// does not already carry, and reports whether it had to.
//
// Appends rather than rewrites: where the model did paste a marker, its
// placement is a deliberate part of the message and worth keeping. Only the
// forgotten ones are added, each on its own line, in the order they were
// asked for.
func EnsureAttachmentMarkers(text string, intended []uuid.UUID) (out string, added []uuid.UUID) {
	if len(intended) == 0 {
		return text, nil
	}
	present, _, _ := ParseAttachmentMarkers(text)
	have := make(map[uuid.UUID]bool, len(present))
	for _, id := range present {
		have[id] = true
	}
	out = text
	for _, id := range intended {
		if have[id] {
			continue
		}
		have[id] = true
		added = append(added, id)
		out += "\n\n[attached: " + id.String() + "]"
	}
	return out, added
}
