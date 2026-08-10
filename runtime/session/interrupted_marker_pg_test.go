package session

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A stopped turn writes an interrupted marker so the transcript keeps
// user/assistant alternation — a user message with no reply breaks the next
// provider call. LatestDialogRole is the guard on that write: the stop button
// is live from the moment a turn starts, well before memory retrieval has let
// the turn persist anything, so a stop can easily land when there is nothing
// to answer. Appending the marker there would attach it to the *previous*
// turn's assistant message and break the very invariant it protects.
//
// Needs a real database: this is one ordering-sensitive query, and a mock that
// pins SQL text would go green on a version that reads the wrong row.
func TestLatestDialogRoleGuardsTheInterruptedMarker(t *testing.T) {
	db := windowTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mustExec(t, db, `INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID)

	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	insert := func(role string, toolUseID any, at time.Time) {
		mustExec(t, db,
			`INSERT INTO chat_messages (id, session_id, role, content, tool_use_id, created_at)
			 VALUES ($1, $2, $3, '[]'::jsonb, $4, $5)`,
			uuid.New(), sessionID, role, toolUseID, at)
	}

	// An empty session: a stop before anything at all was written.
	if role, err := store.LatestDialogRole(ctx, sessionID.String()); err != nil || role != "" {
		t.Fatalf("empty session → role %q, err %v; want \"\", nil", role, err)
	}

	// The previous turn is complete. A stop landing here has nothing to
	// answer, and the marker must not be written.
	insert("user", nil, base)
	insert("assistant", nil, base.Add(time.Second))
	if role, err := store.LatestDialogRole(ctx, sessionID.String()); err != nil || role != "assistant" {
		t.Fatalf("after a complete turn → role %q, err %v; want assistant", role, err)
	}

	// This turn's user message has landed and the answer has not. This is the
	// case the marker exists for.
	insert("user", nil, base.Add(2*time.Second))
	if role, err := store.LatestDialogRole(ctx, sessionID.String()); err != nil || role != "user" {
		t.Fatalf("with a question awaiting an answer → role %q, err %v; want user", role, err)
	}

	// Cut off mid-tool-call: the loop persisted the tool_use turn and its
	// result, which is a user-role row. Still a genuine mid-turn interruption,
	// so the marker is still correct here.
	insert("assistant", nil, base.Add(3*time.Second))
	insert("user", "toolu_abc", base.Add(4*time.Second))
	if role, err := store.LatestDialogRole(ctx, sessionID.String()); err != nil || role != "user" {
		t.Fatalf("mid-tool-call → role %q, err %v; want user", role, err)
	}

	// Rows that are not provider dialogue must not answer the question.
	insert("turn_boundary", nil, base.Add(5*time.Second))
	if role, err := store.LatestDialogRole(ctx, sessionID.String()); err != nil || role != "user" {
		t.Fatalf("a non-dialogue row changed the answer → role %q, err %v; want user", role, err)
	}
}
