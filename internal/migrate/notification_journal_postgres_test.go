package migrate

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/rasimio/blueship/internal/core"
)

func TestNotificationJournalPostgresConcurrency(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN to run PostgreSQL journal tests")
	}

	store, db := newNotificationJournalPostgresStore(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("concurrent exact claim admits one sender", func(t *testing.T) {
		taskID, userID := insertNotificationTestTask(t, ctx, db)
		refs := []core.TaskDeliveryRef{{InputID: "notes", ItemKey: "note:exact"}}
		type result struct {
			id      uuid.UUID
			created bool
			err     error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				id, created, err := store.BeginNotificationAttempt(ctx, taskID, userID, "exact", refs)
				results <- result{id: id, created: created, err: err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		var got []result
		for r := range results {
			if r.err != nil {
				t.Fatalf("BeginNotificationAttempt: %v", r.err)
			}
			got = append(got, r)
		}
		if len(got) != 2 || got[0].created == got[1].created || got[0].id != got[1].id {
			t.Fatalf("concurrent results = %#v, want one create and one exact replay", got)
		}
	})

	t.Run("partial overlap rolls back all free refs", func(t *testing.T) {
		taskID, userID := insertNotificationTestTask(t, ctx, db)
		a := core.TaskDeliveryRef{InputID: "calendar", ItemKey: "event:a"}
		b := core.TaskDeliveryRef{InputID: "notes", ItemKey: "note:b"}
		if _, created, err := store.BeginNotificationAttempt(ctx, taskID, userID, "A", []core.TaskDeliveryRef{a}); err != nil || !created {
			t.Fatalf("claim A: created=%v err=%v", created, err)
		}
		if _, _, err := store.BeginNotificationAttempt(ctx, taskID, userID, "A+B", []core.TaskDeliveryRef{a, b}); err == nil || !strings.Contains(err.Error(), "partial overlap") {
			t.Fatalf("overlap error = %v, want partial overlap", err)
		}
		delivered, err := store.LookupDelivered(ctx, taskID, []core.TaskDeliveryRef{a, b})
		if err != nil {
			t.Fatal(err)
		}
		if !delivered[a] || delivered[b] {
			t.Fatalf("reserved after rollback = %#v, want only A", delivered)
		}
		if _, created, err := store.BeginNotificationAttempt(ctx, taskID, userID, "B", []core.TaskDeliveryRef{b}); err != nil || !created {
			t.Fatalf("claim B after rollback: created=%v err=%v", created, err)
		}
	})

	t.Run("defer retries immutable intent without reopening occurrence", func(t *testing.T) {
		taskID, userID := insertNotificationTestTask(t, ctx, db)
		if err := store.DeferNotificationAttempt(ctx, uuid.New(), "missing", time.Now().Add(time.Minute)); err == nil {
			t.Fatal("defer of missing attempt unexpectedly succeeded")
		}
		gitRef := core.TaskDeliveryRef{InputID: "git", ItemKey: "commit:retry"}
		calendarRef := core.TaskDeliveryRef{InputID: "calendar", ItemKey: "event:retry"}
		refs := []core.TaskDeliveryRef{gitRef, calendarRef} // deliberately unsorted
		id, created, err := store.BeginNotificationAttempt(ctx, taskID, userID, "immutable retry text", refs)
		if err != nil || !created {
			t.Fatalf("claim: created=%v err=%v", created, err)
		}
		retryAt := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
		if err := store.DeferNotificationAttempt(ctx, id, "provider asked us to retry", retryAt); err != nil {
			t.Fatal(err)
		}
		if err := store.DeferNotificationAttempt(ctx, id, "duplicate defer", retryAt.Add(time.Hour)); err != nil {
			t.Fatalf("idempotent defer: %v", err)
		}
		if replayID, created, err := store.BeginNotificationAttempt(ctx, taskID, userID, "rewritten text must lose", refs); err != nil || created || replayID != id {
			t.Fatalf("exact replay: id=%s created=%v err=%v", replayID, created, err)
		}
		reserved, err := store.LookupDelivered(ctx, taskID, refs)
		if err != nil || !reserved[gitRef] || !reserved[calendarRef] {
			t.Fatalf("retryable reservation missing: reserved=%#v err=%v", reserved, err)
		}
		if early, err := store.ClaimRetryableNotification(ctx, retryAt.Add(-time.Second)); err != nil || early != nil {
			t.Fatalf("early claim = %#v err=%v", early, err)
		}
		intent, err := store.ClaimRetryableNotification(ctx, retryAt.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if intent == nil || intent.ID != id || intent.TaskID != taskID || intent.UserID != userID || intent.Text != "immutable retry text" ||
			len(intent.Refs) != 2 || intent.Refs[0] != calendarRef || intent.Refs[1] != gitRef {
			t.Fatalf("claimed intent = %#v", intent)
		}
		var row struct {
			State         string     `db:"state"`
			AttemptCount  int        `db:"attempt_count"`
			NextAttemptAt *time.Time `db:"next_attempt_at"`
			LastAttemptAt time.Time  `db:"last_attempt_at"`
		}
		if err := db.GetContext(ctx, &row, `
			SELECT state, attempt_count, next_attempt_at, last_attempt_at
			FROM agent_task_notification_attempts WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if row.State != "dispatching" || row.AttemptCount != 2 || row.NextAttemptAt != nil || !row.LastAttemptAt.Equal(retryAt.Add(time.Second)) {
			t.Fatalf("claimed row = %#v", row)
		}
		if again, err := store.ClaimRetryableNotification(ctx, retryAt.Add(time.Hour)); err != nil || again != nil {
			t.Fatalf("second claim = %#v err=%v", again, err)
		}
	})

	t.Run("concurrent retry claim admits one drainer", func(t *testing.T) {
		taskID, userID := insertNotificationTestTask(t, ctx, db)
		ref := core.TaskDeliveryRef{InputID: "notes", ItemKey: "note:retry-race"}
		id, created, err := store.BeginNotificationAttempt(ctx, taskID, userID, "retry once", []core.TaskDeliveryRef{ref})
		if err != nil || !created {
			t.Fatalf("claim: created=%v err=%v", created, err)
		}
		due := time.Now().UTC().Truncate(time.Microsecond)
		if err := store.DeferNotificationAttempt(ctx, id, "rate limited", due); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		results := make(chan *core.TaskNotificationIntent, 2)
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				<-start
				intent, claimErr := store.ClaimRetryableNotification(ctx, due.Add(time.Second))
				results <- intent
				errs <- claimErr
			}()
		}
		close(start)
		var claimed, empty int
		for range 2 {
			if claimErr := <-errs; claimErr != nil {
				t.Fatal(claimErr)
			}
			if intent := <-results; intent == nil {
				empty++
			} else if intent.ID == id {
				claimed++
			} else {
				t.Fatalf("claimed unexpected intent %#v", intent)
			}
		}
		if claimed != 1 || empty != 1 {
			t.Fatalf("claimed=%d empty=%d, want 1/1", claimed, empty)
		}
	})

	t.Run("reject is terminal and keeps reservation", func(t *testing.T) {
		taskID, userID := insertNotificationTestTask(t, ctx, db)
		if err := store.RejectNotificationAttempt(ctx, uuid.New(), "missing"); err == nil {
			t.Fatal("reject of missing attempt unexpectedly succeeded")
		}
		ref := core.TaskDeliveryRef{InputID: "calendar", ItemKey: "event:rejected"}
		id, created, err := store.BeginNotificationAttempt(ctx, taskID, userID, "cannot deliver", []core.TaskDeliveryRef{ref})
		if err != nil || !created {
			t.Fatalf("claim: created=%v err=%v", created, err)
		}
		if err := store.RejectNotificationAttempt(ctx, id, "user blocked the bot"); err != nil {
			t.Fatal(err)
		}
		if err := store.RejectNotificationAttempt(ctx, id, "duplicate reject"); err != nil {
			t.Fatalf("idempotent reject: %v", err)
		}
		reserved, err := store.LookupDelivered(ctx, taskID, []core.TaskDeliveryRef{ref})
		if err != nil || !reserved[ref] {
			t.Fatalf("rejected reservation missing: reserved=%#v err=%v", reserved, err)
		}
		if claimed, err := store.ClaimRetryableNotification(ctx, time.Now().Add(24*time.Hour)); err != nil || claimed != nil {
			t.Fatalf("rejected claim = %#v err=%v", claimed, err)
		}
	})

	t.Run("confirmed autonomous turn is projected exactly once", func(t *testing.T) {
		taskID, userID := insertNotificationTestTask(t, ctx, db)
		soulID, sessionID := uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO chat_sessions
			    (id, user_id, soul_id, source, token_count, message_count)
			VALUES ($1, $2, $3, 'chat', 0, 0)`,
			sessionID, userID, soulID); err != nil {
			t.Fatal(err)
		}
		request := core.AutonomousTurnRequest{
			UserID:          userID,
			SoulID:          soulID,
			AnchorMessageID: uuid.NewString(),
			Prompt:          "provider-only",
		}
		draft := core.AutonomousTurnDraft{
			Text:            "I thought of you.",
			SessionID:       sessionID.String(),
			DialogMessageID: uuid.NewString(),
			ActivityToken:   uuid.NewString() + ":0",
		}
		marker, err := core.FormatAutonomousTurnNotification(request, draft)
		if err != nil {
			t.Fatal(err)
		}
		ref := core.TaskDeliveryRef{InputID: "relational-pulse", ItemKey: "anchor:24h"}
		attemptID, created, err := store.BeginNotificationAttempt(
			ctx, taskID, userID, marker, []core.TaskDeliveryRef{ref},
		)
		if err != nil || !created {
			t.Fatalf("begin autonomous attempt: created=%t err=%v", created, err)
		}
		receipt := core.TaskNotificationReceipt{
			Transport: "telegram",
			BotID:     uuid.NewString(),
			ChatID:    "42",
			MessageID: "777",
		}
		if err := store.ConfirmNotificationAttempt(ctx, attemptID, receipt); err != nil {
			t.Fatal(err)
		}
		if err := store.EnsureAutonomousHistoryForSession(
			ctx, userID, soulID, sessionID.String(),
		); err != nil {
			t.Fatalf("ensure projection: %v", err)
		}
		if err := store.EnsureAutonomousHistoryForSession(
			ctx, userID, soulID, sessionID.String(),
		); err != nil {
			t.Fatalf("idempotent ensure projection: %v", err)
		}

		var projectedAt *time.Time
		if err := db.GetContext(ctx, &projectedAt, `
			SELECT autonomous_history_projected_at
			FROM agent_task_notification_attempts
			WHERE id = $1`, attemptID); err != nil {
			t.Fatal(err)
		}
		if projectedAt == nil {
			t.Fatal("autonomous history projection was not marked complete")
		}
		var row struct {
			MessageCount int `db:"message_count"`
			TokenCount   int `db:"token_count"`
		}
		if err := db.GetContext(ctx, &row, `
			SELECT message_count, token_count
			FROM chat_sessions WHERE id = $1`, sessionID); err != nil {
			t.Fatal(err)
		}
		if row.MessageCount != 2 || row.TokenCount <= 0 {
			t.Fatalf("session counters = %#v, want one boundary plus one assistant", row)
		}
		var roles []string
		if err := db.SelectContext(ctx, &roles, `
			SELECT role FROM chat_messages
			WHERE session_id = $1 ORDER BY created_at, id`, sessionID); err != nil {
			t.Fatal(err)
		}
		if len(roles) != 2 || roles[0] != "turn_boundary" || roles[1] != "assistant" {
			t.Fatalf("projected roles = %#v", roles)
		}
	})

	t.Run("confirm and uncertain race has one irreversible winner", func(t *testing.T) {
		taskID, userID := insertNotificationTestTask(t, ctx, db)
		ref := core.TaskDeliveryRef{InputID: "calendar", ItemKey: "event:race"}
		id, created, err := store.BeginNotificationAttempt(ctx, taskID, userID, "race", []core.TaskDeliveryRef{ref})
		if err != nil || !created {
			t.Fatalf("claim: created=%v err=%v", created, err)
		}

		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			errs <- store.ConfirmNotificationAttempt(ctx, id, core.TaskNotificationReceipt{
				Transport: "telegram", BotID: "bot", ChatID: "42", MessageID: "7",
			})
		}()
		go func() {
			<-start
			errs <- store.MarkNotificationUncertain(ctx, id, "ambiguous timeout")
		}()
		close(start)
		first, second := <-errs, <-errs
		if first != nil && second != nil {
			t.Fatalf("both terminal transitions failed: %v / %v", first, second)
		}

		var state string
		if err := db.GetContext(ctx, &state, `SELECT state FROM agent_task_notification_attempts WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		delivered, err := store.LookupDelivered(ctx, taskID, []core.TaskDeliveryRef{ref})
		if err != nil || !delivered[ref] {
			t.Fatalf("terminal reservation missing: delivered=%#v err=%v", delivered, err)
		}
		var ledgerCount int
		if err := db.GetContext(ctx, &ledgerCount,
			`SELECT count(*) FROM agent_task_deliveries WHERE task_id = $1 AND input_id = $2 AND item_key = $3`,
			taskID, ref.InputID, ref.ItemKey); err != nil {
			t.Fatal(err)
		}
		if (state == "sent" && ledgerCount != 1) || (state == "uncertain" && ledgerCount != 0) {
			t.Fatalf("state=%s ledger_count=%d", state, ledgerCount)
		}
		if state != "sent" && state != "uncertain" {
			t.Fatalf("unexpected terminal state %q", state)
		}
		if err := store.DeferNotificationAttempt(ctx, id, "must not reopen terminal", time.Now().Add(time.Minute)); err == nil {
			t.Fatalf("defer unexpectedly reopened %s attempt", state)
		}
		if err := store.RejectNotificationAttempt(ctx, id, "must not overwrite terminal"); err == nil {
			t.Fatalf("reject unexpectedly overwrote %s attempt", state)
		}
	})
}

func newNotificationJournalPostgresStore(t *testing.T, dsn string) (*core.AgentTaskStore, *sqlx.DB) {
	t.Helper()
	admin, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	schema := "notification_journal_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA ` + pq.QuoteIdentifier(schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	testDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	query := testDSN.Query()
	query.Set("search_path", schema)
	testDSN.RawQuery = query.Encode()
	db, err := sqlx.Connect("postgres", testDSN.String())
	if err != nil {
		t.Fatalf("connect test schema: %v", err)
	}
	db.SetMaxOpenConns(8)

	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(schema) + ` CASCADE`)
		admin.Close()
	})
	if _, err := db.Exec(`
		CREATE TABLE agent_tasks (
			id UUID PRIMARY KEY,
			user_id UUID NOT NULL
		);
		CREATE TABLE chat_sessions (
			id UUID PRIMARY KEY,
			user_id UUID NOT NULL,
			soul_id UUID NOT NULL,
			source TEXT NOT NULL DEFAULT 'chat',
			token_count INT NOT NULL DEFAULT 0,
			message_count INT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE chat_messages (
			id UUID PRIMARY KEY,
			soul_id UUID NOT NULL,
			session_id UUID NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
			role TEXT NOT NULL,
			content JSONB NOT NULL,
			token_estimate INT NOT NULL DEFAULT 0,
			tg_message_id BIGINT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			-- Not decoration: the write path under test inserts into these, so a
			-- fixture without them fails with 42703 for every input. This test
			-- skips unless BLUESHIP_TEST_POSTGRES_DSN is set and CI never set it,
			-- so it had never once run — it was red the first time any database
			-- was pointed at it.
			-- Nullability copied from production, not chosen: visible_text and
			-- projection_reason are nullable there and the writer passes NULL for
			-- them, so a stricter fixture fails with 23502 instead of testing
			-- anything. The two NOT NULL columns carry production's defaults for
			-- the same reason.
			visible_text      TEXT,
			projection_status TEXT NOT NULL DEFAULT 'unprojectable_legacy',
			projection_reason TEXT,
			projector_version TEXT NOT NULL DEFAULT 'legacy-unprojected',
			tool_use_id       TEXT
		);
		CREATE TABLE agent_task_deliveries (
			task_id UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
			input_id TEXT NOT NULL,
			item_key TEXT NOT NULL,
			delivered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (task_id, input_id, item_key)
		);`); err != nil {
		t.Fatalf("create base tables: %v", err)
	}
	migration, err := migrations.ReadFile("sql/018_agent_task_notification_journal.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration)); err != nil {
		t.Fatalf("apply notification journal migration: %v", err)
	}
	projectionMigration, err := migrations.ReadFile("sql/019_autonomous_history_projection.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(projectionMigration)); err != nil {
		t.Fatalf("apply autonomous history projection migration: %v", err)
	}
	return core.NewAgentTaskStore(db), db
}

func insertNotificationTestTask(t *testing.T, ctx context.Context, db *sqlx.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	taskID, userID := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_tasks (id, user_id) VALUES ($1, $2)`, taskID, userID); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return taskID, userID
}
