package core

import (
	"context"

	"github.com/google/uuid"
)

// AttachmentSink is the host-supplied hook for the CDN that backs
// inbound (Telegram photo, cabinet upload) and outbound (Arlene's
// attachment_include) file flow. Implementations typically pair a
// content-addressed disk store with a metadata row that the cabinet's
// chat history endpoint joins for chip rendering.
//
// Save is called from the gateway turn after the soul session has
// been resolved, so SessionID is non-nil; UserID and SoulID come from
// the resolved UserState. The method MUST be idempotent over the
// content.
//
// Get reads a previously stored attachment by id, scoped to the
// (userID, soulID) the request is running under so a guessed id
// never crosses tenants. Returned bytes are the raw file body — the
// caller decides whether to ship them out a transport or feed them
// back into the model.
//
// A nil sink on Deps is the documented "no CDN" mode — the gateway
// silently skips persistence/resolution and continues serving the
// model from chat_messages alone.
type AttachmentSink interface {
	Save(ctx context.Context, p AttachmentParams) (id uuid.UUID, err error)
	Get(ctx context.Context, userID, soulID, id uuid.UUID) (*AttachmentRecord, []byte, error)
	// ListForMessage returns the attachment ids linked to a specific
	// chat_messages row, scoped to (userID, soulID). Used by the
	// gateway's reply-context expander: when the user replies to an
	// older message that carried attachments, those parent files are
	// pulled back into the current turn's content so the model can
	// reason about them, not just the inline text snippet.
	ListForMessage(ctx context.Context, userID, soulID, messageID uuid.UUID) ([]uuid.UUID, error)
	// SaveLink upserts a kind='link' row keyed on (user, soul, url).
	// Returns the row id; the call is idempotent — the same URL twice
	// for the same (user, soul) returns the same row id and does NOT
	// insert a new row. Bytes are NOT stored: the cabinet renders the
	// link chip straight from the URL plus the OpenGraph fields the
	// async enrichment worker populates after the row lands. Callers
	// (the gateway's user/assistant scan) pass MessageID / SessionID
	// best-effort — first writer wins, later upserts only fill them in
	// when the existing row had them NULL.
	SaveLink(ctx context.Context, p LinkParams) (id uuid.UUID, err error)
}

// LinkParams carries the metadata the sink needs to persist one
// auto-extracted URL. URL is the canonical https://… string as
// returned by attachment.ExtractURLs. UserID / SoulID are required;
// SessionID and MessageID are best-effort (zero uuid = unknown, sink
// inserts NULL).
type LinkParams struct {
	UserID    uuid.UUID
	SoulID    uuid.UUID
	SessionID uuid.UUID
	MessageID uuid.UUID
	URL       string
}

// AttachmentRecord carries metadata for a resolved attachment —
// enough for a transport to decide between SendPhoto vs SendDocument
// and render a sensible filename. SourceText is optional and only
// populated for rows whose canonical content lives somewhere other
// than the bytes the disk store holds (today: PDFs the host
// generated from markdown, where the markdown is the human-readable
// source and the rendered PDF font subset can't be re-decoded
// cleanly). Empty SourceText falls back to extracting from the
// bytes the caller already has.
type AttachmentRecord struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	SoulID     uuid.UUID
	Name       string
	Mime       string
	Kind       string // "image" | "pdf" | "text"
	Size       int64
	CreatedAt  int64 // unix seconds; transport-agnostic
	SourceText string
}

// AttachmentSendSink is the optional transport-side capability for
// shipping a CDN-resolved file out to the user. Telegram implements
// it via SendPhoto / SendDocument; the web cabinet's SSE sink does
// not — cabinet rendering happens at history-read time via the
// chat_attachments join. The gateway checks this interface after the
// agent loop returns: if the sink implements it, it dispatches one
// SendAttachment per [attached: UUID] marker. Sinks that don't
// implement it leave the markers in the text; downstream renderers
// (cabinet buildParts) handle the chip swap themselves.
type AttachmentSendSink interface {
	SendAttachment(ctx context.Context, rec AttachmentRecord, data []byte) error
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
