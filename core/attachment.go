package core

import (
	"context"

	"github.com/google/uuid"
)

// AttachmentSink is the host-supplied hook for persisting an inbound
// file (Telegram photo / document, cabinet upload) into the
// embedding application's attachment store. Implementations typically
// pair a content-addressed disk store with a metadata row that the
// cabinet's chat history endpoint can join to render a chip with a
// download link.
//
// Save is called from the gateway turn after the soul session has
// been resolved, so SessionID is non-nil; UserID and SoulID come from
// the resolved UserState. The method MUST be idempotent over the
// content: callers may retry on transient errors and a successful
// duplicate write should reuse the existing row.
//
// Returns the persistent attachment id (renders into the cabinet
// chip's URL) plus any error. A nil sink on Deps is the documented
// "no CDN" mode — the gateway silently skips persistence and
// continues serving the model from chat_messages.
type AttachmentSink interface {
	Save(ctx context.Context, p AttachmentParams) (id uuid.UUID, err error)
}

// AttachmentParams carries everything the sink needs to persist one
// file. Kind is one of "image" / "pdf" / "text" — the same lane
// vocabulary the rest of the system uses. Data is the raw bytes;
// callers shouldn't expect the slice to be retained beyond the call.
type AttachmentParams struct {
	UserID    uuid.UUID
	SoulID    uuid.UUID
	SessionID uuid.UUID
	Name      string
	Mime      string
	Kind      string
	Data      []byte
}
