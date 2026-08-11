package session

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	bs "github.com/rasimio/blueship/internal/core"
)

// A Telegram reply is resolvable only if the message it points at is indexed.
// Both of these cases were not, and both failed silently: the reply reached
// the model with no quote and no re-inlined attachment, which reads as the
// soul having forgotten a file she was looking at a minute earlier.
//
// Needs a real database — the array column, its containment operator and the
// UPDATE that unions into it are exactly what a mock cannot check.
// BLUESHIP_TEST_POSTGRES_DSN, same convention as the other PostgreSQL tests.
func TestReplyLookupFindsEveryMessageOfADebouncedBurst(t *testing.T) {
	db := replyIndexTestDB(t)
	store := NewStore(db)
	ctx, sessionID := seedReplySession(t, db)

	// «Договор ок?» (18170) and the PDF that followed it a second later
	// (18171) were debounced into one turn, so they share one durable row.
	visible := "Договор ок?"
	receipt, err := store.AppendPersisted(ctx, sessionID, bs.Message{
		Role:         "user",
		Content:      "Договор ок?",
		VisibleText:  &visible,
		TGMessageIDs: []int64{18170, 18171},
	})
	if err != nil {
		t.Fatalf("append burst: %v", err)
	}

	for _, tgID := range []int64{18170, 18171} {
		got, lookupErr := store.LookupByTGMessageID(ctx, sessionID, tgID)
		if lookupErr != nil {
			t.Fatalf("lookup %d: %v", tgID, lookupErr)
		}
		if got != receipt.ID {
			t.Fatalf("reply to %d resolved to %q, want the burst row %q", tgID, got, receipt.ID)
		}
	}

	// A message from another chat that happens to share a number must not
	// match, which is what makes the lookup session-scoped.
	other, err := store.LookupByTGMessageID(ctx, uuid.New().String(), 18171)
	if err != nil {
		t.Fatalf("cross-session lookup: %v", err)
	}
	if other != "" {
		t.Fatalf("a foreign session's message id matched: %q", other)
	}
}

func TestAttachTGMessageIDMakesAnAnswerReplyable(t *testing.T) {
	db := replyIndexTestDB(t)
	store := NewStore(db)
	ctx, sessionID := seedReplySession(t, db)

	answer := "Да, всё верно — это два отдельных обязательства."
	receipt, err := store.AppendPersisted(ctx, sessionID, bs.Message{
		Role:        "assistant",
		Content:     answer,
		VisibleText: &answer,
	})
	if err != nil {
		t.Fatalf("append answer: %v", err)
	}

	// Before delivery the answer is unaddressable — this is the state every
	// answer used to stay in.
	if got, lookupErr := store.LookupByTGMessageID(ctx, sessionID, 18176); lookupErr != nil || got != "" {
		t.Fatalf("unindexed answer resolved = (%q, %v)", got, lookupErr)
	}

	if err := store.AttachTGMessageID(ctx, sessionID, receipt.ID, 18176); err != nil {
		t.Fatalf("attach: %v", err)
	}
	got, err := store.LookupByTGMessageID(ctx, sessionID, 18176)
	if err != nil || got != receipt.ID {
		t.Fatalf("reply to the answer resolved to (%q, %v), want %q", got, err, receipt.ID)
	}

	// The gateway quotes the recovered parent, and needs the role to decide
	// how much of it to carry: Rich Messages arrive with no text of their own.
	role, text, err := store.MessageForReply(ctx, sessionID, receipt.ID)
	if err != nil || role != "assistant" || text != answer {
		t.Fatalf("MessageForReply = (%q, %q, %v)", role, text, err)
	}

	// Attaching twice must not duplicate, and must not lose an earlier id.
	if err := store.AttachTGMessageID(ctx, sessionID, receipt.ID, 18176); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if err := store.AttachTGMessageID(ctx, sessionID, receipt.ID, 18178); err != nil {
		t.Fatalf("attach second: %v", err)
	}
	var ids []int64
	if err := db.Get(pq.Array(&ids),
		`SELECT tg_message_ids FROM chat_messages WHERE id = $1`, receipt.ID); err != nil {
		t.Fatalf("read ids: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("tg_message_ids = %v, want both ids exactly once", ids)
	}
}

// Rows written before the array column exist in production and carry only the
// scalar. The lookup has to keep finding them.
func TestReplyLookupStillFindsScalarOnlyRows(t *testing.T) {
	db := replyIndexTestDB(t)
	store := NewStore(db)
	ctx, sessionID := seedReplySession(t, db)

	legacyID := uuid.New()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO chat_messages (id, session_id, role, content, tg_message_id)
		 VALUES ($1, $2, 'user', '[{"type":"text","text":"старое"}]'::jsonb, 17850)`,
		legacyID, sessionID); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	got, err := store.LookupByTGMessageID(ctx, sessionID, 17850)
	if err != nil || got != legacyID.String() {
		t.Fatalf("legacy lookup = (%q, %v), want %q", got, err, legacyID)
	}
}

func seedReplySession(t *testing.T, db *sqlx.DB) (context.Context, string) {
	t.Helper()
	soulID := uuid.New()
	sessionID := uuid.New()
	if _, err := db.Exec(`INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return bs.WithSoulID(context.Background(), soulID), sessionID.String()
}

// replyIndexTestDB mirrors the columns the append + lookup path touches, in a
// per-run schema that is dropped on exit.
func replyIndexTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN to run the reply-index PostgreSQL test")
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "bs_reply_index_test_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
	if _, err := db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		db.Close()
	})
	if _, err := db.Exec(`SET search_path TO ` + schema); err != nil {
		t.Fatalf("search_path: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE chat_messages (
			id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			soul_id             uuid,
			session_id          uuid NOT NULL,
			role                text NOT NULL,
			content             jsonb NOT NULL,
			tool_use_id         text,
			token_estimate      int NOT NULL DEFAULT 0,
			reply_to_message_id uuid,
			tg_message_id       bigint,
			tg_message_ids      bigint[],
			created_at          timestamptz NOT NULL DEFAULT now(),
			visible_text        text,
			projection_status   text,
			projection_reason   text,
			projector_version   text
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE chat_sessions (
			id            uuid PRIMARY KEY,
			token_count   int NOT NULL DEFAULT 0,
			message_count int NOT NULL DEFAULT 0,
			updated_at    timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("create sessions table: %v", err)
	}
	return db
}
