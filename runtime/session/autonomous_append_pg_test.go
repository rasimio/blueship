package session

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	bs "github.com/rasimio/blueship/internal/core"
)

// AppendAutonomousAssistant writes two rows in one transaction, and its
// boundary INSERT subtracts an interval from a bound parameter. That shape is
// only wrong at PLAN time, so a mock that never reaches PostgreSQL reports a
// green path while production has failed every single time since the method
// was written — the table held no boundary row at all, and the alert
// ("history append failed") was read as an incident rather than as proof the
// feature had never run.
//
// Needs a real database: BLUESHIP_TEST_POSTGRES_DSN, same convention as the
// notification-journal tests.
func TestAppendAutonomousAssistantWritesBothRows(t *testing.T) {
	db := autonomousTestDB(t)
	store := NewStore(db)

	soulID := uuid.New()
	sessionID := uuid.New()
	if _, err := db.Exec(`INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO chat_messages (id, soul_id, session_id, role, content, token_estimate, created_at)
	                      VALUES ($1, $2, $3, 'user', $4::jsonb, 0, now())`,
		uuid.New(), soulID, sessionID, `[{"type":"text","text":"anchor"}]`); err != nil {
		t.Fatalf("seed anchor row: %v", err)
	}

	deliveredAt := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ctx := bs.WithSoulID(context.Background(), soulID)
	if err := store.AppendAutonomousAssistant(ctx, uuid.New(), sessionID.String(), "проактивная реплика", 0, deliveredAt); err != nil {
		t.Fatalf("AppendAutonomousAssistant: %v", err)
	}

	var roles []string
	if err := db.Select(&roles,
		`SELECT role FROM chat_messages WHERE session_id = $1 AND role <> 'user' ORDER BY created_at`,
		sessionID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(roles) != 2 || roles[0] != "turn_boundary" || roles[1] != "assistant" {
		t.Fatalf("rows = %v, want the boundary before the assistant reply", roles)
	}

	// The boundary exists to order strictly before the reply for readers that
	// only see created_at; equal timestamps would make the order arbitrary.
	var boundaryBefore bool
	if err := db.Get(&boundaryBefore, `
		SELECT (SELECT created_at FROM chat_messages WHERE session_id = $1 AND role = 'turn_boundary')
		     < (SELECT created_at FROM chat_messages WHERE session_id = $1 AND role = 'assistant')`,
		sessionID); err != nil {
		t.Fatalf("compare timestamps: %v", err)
	}
	if !boundaryBefore {
		t.Fatal("boundary row does not sort before the assistant reply")
	}
}

// autonomousTestDB gives each run its own schema so a failed run cannot
// poison the next one, and drops it on exit.
func autonomousTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN to run the autonomous-append PostgreSQL test")
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "bs_autonomous_test_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
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
	// Only the columns this write path touches. A wider mirror of production
	// would drift; a narrower one would stop exercising the failing INSERT.
	if _, err := db.Exec(`
		CREATE TABLE chat_messages (
			id                uuid PRIMARY KEY,
			soul_id           uuid,
			session_id        uuid NOT NULL,
			role              text NOT NULL,
			content           jsonb NOT NULL,
			token_estimate    int  NOT NULL DEFAULT 0,
			tg_message_id     bigint,
			created_at        timestamptz NOT NULL DEFAULT now(),
			visible_text      text,
			projection_status text,
			projection_reason text,
			projector_version text
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// The same transaction bumps the session counters; without this table the
	// write fails for a reason unrelated to what the test is pinning.
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
