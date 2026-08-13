package session

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	bs "github.com/rasimio/blueship/internal/core"
)

// The window used to count backwards from the newest message until the token
// budget ran out, so its left edge landed wherever the SIZES of recent messages
// put it. Measured on one production session over six consecutive turns: 27876,
// 27876, 24698, 26008, 26775, 28289 dialogue tokens. Providers cache by prefix,
// so an edge that moves throws away the whole cached dialogue on the turn it
// moves — and the model gets an arbitrary slice with no indication that it is a
// slice, which is how "not in my window" comes to be answered as "you never told
// me".
//
// Needs a real database: the boundary lives in a JOIN, and a mock that pins SQL
// text would have gone green on the old behaviour too.
func TestDialogWindowStartsAtTheSummaryBoundary(t *testing.T) {
	db := windowTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mustExec(t, db, `INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID)

	base := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	// Twelve messages of 100 tokens each. A budget of 1000 cannot hold them all,
	// which is exactly the situation the old code answered by sliding.
	var ids []uuid.UUID
	for i := 0; i < 12; i++ {
		ids = append(ids, seedMessage(t, db, sessionID, i, base.Add(time.Duration(i)*time.Minute), 100))
	}

	// No summary yet: the window is the tail that fits, as before.
	before, err := store.DialogMessagesForAPI(ctx, sessionID.String(), 1000, true)
	if err != nil {
		t.Fatalf("DialogMessagesForAPI: %v", err)
	}
	if len(before) == 0 || len(before) > 10 {
		t.Fatalf("without a summary the window is %d messages, want a budget-bounded tail", len(before))
	}

	// Summarise up to message 7. Everything after it is 4 messages, 400 tokens.
	mustExec(t, db,
		`INSERT INTO chat_session_summaries (session_id, to_message_id, message_count, token_count, summary)
		 VALUES ($1, $2, 8, 800, 'сводка первых восьми')`,
		sessionID, ids[7])

	after, err := store.DialogMessagesForAPI(ctx, sessionID.String(), 1000, true)
	if err != nil {
		t.Fatalf("DialogMessagesForAPI after summary: %v", err)
	}
	if got := len(after); got != 4 {
		t.Fatalf("window after the summary = %d messages, want exactly the 4 that follow the boundary: %v", got, texts(after))
	}
	if first := text(after[0]); !strings.Contains(first, "msg-8") {
		t.Errorf("window starts at %q, want msg-8 — the first message after the boundary", first)
	}
	for _, m := range after {
		if strings.Contains(text(m), "msg-7") {
			t.Error("the boundary message itself is inside the window; it is already covered by the summary")
		}
	}
}

// The point of the boundary is a prefix that does not move. Append a turn and
// everything already in the window must come back byte-identical, in the same
// order — that is what lets a provider reuse the cached prompt.
func TestDialogWindowPrefixSurvivesANewTurn(t *testing.T) {
	db := windowTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mustExec(t, db, `INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID)

	base := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	var ids []uuid.UUID
	for i := 0; i < 6; i++ {
		ids = append(ids, seedMessage(t, db, sessionID, i, base.Add(time.Duration(i)*time.Minute), 100))
	}
	mustExec(t, db,
		`INSERT INTO chat_session_summaries (session_id, to_message_id, message_count, token_count, summary)
		 VALUES ($1, $2, 3, 300, 'сводка')`,
		sessionID, ids[2])

	first, err := store.DialogMessagesForAPI(ctx, sessionID.String(), 1000, true)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// A new turn arrives — and a large one, which under the old scheme was the
	// thing that moved the edge by thousands of tokens at once.
	seedMessage(t, db, sessionID, 6, base.Add(6*time.Minute), 400)

	second, err := store.DialogMessagesForAPI(ctx, sessionID.String(), 1000, true)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if len(second) != len(first)+1 {
		t.Fatalf("the window grew from %d to %d messages, want exactly one more: %v -> %v",
			len(first), len(second), texts(first), texts(second))
	}
	for i := range first {
		if text(first[i]) != text(second[i]) {
			t.Errorf("message %d changed when a turn was appended, so the cached prefix is thrown away:\n before: %q\n after:  %q",
				i, text(first[i]), text(second[i]))
		}
	}
}

// A summary is written asynchronously and can fall behind. When the messages
// since the boundary no longer fit, the window has to degrade to a tail rather
// than return a prefix that overruns the budget — the provider would reject it.
func TestDialogWindowFallsBackWhenTheSummaryIsOverdue(t *testing.T) {
	db := windowTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mustExec(t, db, `INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID)

	base := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	var ids []uuid.UUID
	for i := 0; i < 10; i++ {
		ids = append(ids, seedMessage(t, db, sessionID, i, base.Add(time.Duration(i)*time.Minute), 100))
	}
	// Boundary right at the start: 9 messages / 900 tokens follow it, and the
	// budget is 400.
	mustExec(t, db,
		`INSERT INTO chat_session_summaries (session_id, to_message_id, message_count, token_count, summary)
		 VALUES ($1, $2, 1, 100, 'сводка одного сообщения')`,
		sessionID, ids[0])

	out, err := store.DialogMessagesForAPI(ctx, sessionID.String(), 400, true)
	if err != nil {
		t.Fatalf("DialogMessagesForAPI: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("an overdue summary produced an empty window; the turn would go in with no dialogue at all")
	}
	total := 0
	for _, m := range out {
		total += estimateMessageTokens(m)
	}
	if len(out) > 5 {
		t.Errorf("the fallback returned %d messages for a 400-token budget: %v", len(out), texts(out))
	}
	// And it must be the NEWEST end that survives: the current turn matters more
	// than the oldest message after a stale boundary.
	if last := text(out[len(out)-1]); !strings.Contains(last, "msg-9") {
		t.Errorf("the fallback window ends at %q, want the newest message msg-9", last)
	}
}

// A session accumulates summaries over time, each covering further forward. The
// prompt needs the NEWEST one, because the window starts at that one's boundary —
// an older summary would describe history the window already carries and leave
// the middle of the conversation described by nobody.
func TestLatestSoftSummaryTextReturnsTheNewest(t *testing.T) {
	db := windowTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mustExec(t, db, `INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID)

	if got, err := store.LatestSoftSummaryText(ctx, sessionID.String()); err != nil || got != "" {
		t.Fatalf("no summaries yet: got %q, err %v — want empty and no error", got, err)
	}

	base := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	id := seedMessage(t, db, sessionID, 0, base, 10)
	mustExec(t, db,
		`INSERT INTO chat_session_summaries (session_id, to_message_id, summary, created_at)
		 VALUES ($1, $2, 'первая сводка', $3)`, sessionID, id, base)
	mustExec(t, db,
		`INSERT INTO chat_session_summaries (session_id, to_message_id, summary, created_at)
		 VALUES ($1, $2, 'вторая сводка', $3)`, sessionID, id, base.Add(time.Hour))

	got, err := store.LatestSoftSummaryText(ctx, sessionID.String())
	if err != nil {
		t.Fatalf("LatestSoftSummaryText: %v", err)
	}
	if got != "вторая сводка" {
		t.Errorf("got %q, want the newest summary — an older one describes history the window already carries", got)
	}
}

// Anchoring on the summary was correct and inert: seven sessions out of 172222
// had a summary, because the threshold is 80000 stored tokens. Everything else
// kept the old sliding edge, which is the bug. So a session with NO summary must
// still get a stable prefix — held by the block anchor.
//
// This is the property, stated as the cache sees it: append a turn and the window
// must come back with the same messages plus one, for a whole block of turns, and
// change its left edge exactly once when the block fills.
func TestDialogWindowPrefixHoldsWithoutAnySummary(t *testing.T) {
	db := windowTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mustExec(t, db, `INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID)

	base := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	at := 0
	add := func() {
		seedMessage(t, db, sessionID, at, base.Add(time.Duration(at)*time.Minute), 20)
		at++
	}

	// Start above one block so the anchor is engaged rather than trivially zero.
	for i := 0; i < 61; i++ {
		add()
	}

	const budget = 100000 // ample: this test is about the edge, not the budget
	first, err := store.DialogMessagesForAPI(ctx, sessionID.String(), budget, true)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(first) < dialogWindowBlock || len(first) >= 2*dialogWindowBlock {
		t.Fatalf("window is %d messages, want it held inside [%d, %d)",
			len(first), dialogWindowBlock, 2*dialogWindowBlock)
	}
	head := text(first[0])

	// Append turns one at a time. Until the block fills, the left edge must not
	// move — every earlier message identical, one more at the end.
	prev := first
	moves := 0
	for turn := 0; turn < dialogWindowBlock; turn++ {
		add()
		next, err := store.DialogMessagesForAPI(ctx, sessionID.String(), budget, true)
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if text(next[0]) != text(prev[0]) {
			moves++
			prev = next
			continue
		}
		if len(next) != len(prev)+1 {
			t.Fatalf("turn %d: window went from %d to %d messages with the same left edge — appending should add exactly one",
				turn, len(prev), len(next))
		}
		for i := range prev {
			if text(prev[i]) != text(next[i]) {
				t.Fatalf("turn %d: message %d changed while the left edge stayed put, so the cached prefix is lost anyway", turn, i)
			}
		}
		prev = next
	}

	if moves == 0 {
		t.Errorf("the left edge never moved across %d appended turns, so the window is unbounded", dialogWindowBlock)
	}
	if moves > 1 {
		t.Errorf("the left edge moved %d times across %d turns, want exactly once — that is the difference between a cached prefix and none", moves, dialogWindowBlock)
	}
	if text(prev[0]) == head && moves > 0 {
		t.Error("the edge was counted as moved but the first message is unchanged")
	}
}

// The window must stay bounded. Without the block anchor the retained count grew
// with the session, which is the failure mode the first version of the
// arithmetic had: making the KEPT count a multiple of the block keeps everything
// at n=60.
func TestDialogWindowStaysBoundedAsTheSessionGrows(t *testing.T) {
	db := windowTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mustExec(t, db, `INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID)
	base := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 200; i++ {
		seedMessage(t, db, sessionID, i, base.Add(time.Duration(i)*time.Minute), 20)
	}

	out, err := store.DialogMessagesForAPI(ctx, sessionID.String(), 100000, true)
	if err != nil {
		t.Fatalf("DialogMessagesForAPI: %v", err)
	}
	if len(out) >= 2*dialogWindowBlock {
		t.Errorf("200 messages produced a window of %d, want under %d — the window grows with the session",
			len(out), 2*dialogWindowBlock)
	}
	if last := text(out[len(out)-1]); !strings.Contains(last, "msg-199") {
		t.Errorf("the window ends at %q, want the newest message", last)
	}
}

func text(m bs.Message) string {
	if s, ok := m.Content.(string); ok {
		return s
	}
	return ExtractText(bs.NormalizeContent(m.Content))
}

func texts(msgs []bs.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, text(m))
	}
	return out
}

func estimateMessageTokens(m bs.Message) int {
	return len(text(m)) / 4
}

// seedMessage writes a message whose VISIBLE size drives the budget. The stored
// token_estimate covers the whole persisted row including tool traffic, but the
// dialogue window strips tool blocks and re-estimates what is left, so the text
// is what has to be sized here — padding to roughly 4 chars per token. Getting
// this wrong is how the first version of these tests passed a budget check and
// then watched the selector admit everything anyway.
func seedMessage(t *testing.T, db *sqlx.DB, sessionID uuid.UUID, i int, at time.Time, tokens int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	role := "user"
	if i%2 == 1 {
		role = "assistant"
	}
	body := fmt.Sprintf("msg-%d %s", i, strings.Repeat("x", tokens*4))
	mustExec(t, db, `
		INSERT INTO chat_messages (id, session_id, role, content, token_estimate, created_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6)`,
		id, sessionID, role,
		fmt.Sprintf(`[{"type":"text","text":%q}]`, body), tokens, at)
	return id
}

func mustExec(t *testing.T, db *sqlx.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %.60s: %v", query, err)
	}
}

// windowTestDB gives each run its own schema and drops it on exit.
func windowTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN to run the dialog-window boundary tests")
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "bs_window_test_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
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

	// Only what these queries touch. A wider mirror of production would drift;
	// a narrower one would stop exercising the boundary JOIN.
	if _, err := db.Exec(`
		CREATE TABLE chat_sessions (
			id            uuid PRIMARY KEY,
			token_count   int NOT NULL DEFAULT 0,
			message_count int NOT NULL DEFAULT 0,
			updated_at    timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE chat_messages (
			id                uuid PRIMARY KEY,
			soul_id           uuid,
			session_id        uuid NOT NULL,
			role              text NOT NULL,
			content           jsonb NOT NULL,
			token_estimate    int NOT NULL DEFAULT 0,
			tg_message_id     bigint,
			tool_use_id       text,
			created_at        timestamptz NOT NULL DEFAULT now(),
			visible_text      text NOT NULL DEFAULT '',
			projection_status text NOT NULL DEFAULT '',
			projection_reason text NOT NULL DEFAULT '',
			projector_version text NOT NULL DEFAULT ''
		);
		CREATE TABLE chat_session_summaries (
			id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			session_id    uuid NOT NULL,
			user_id       uuid,
			soul_id       uuid,
			source        text,
			to_message_id uuid REFERENCES chat_messages(id) ON DELETE SET NULL,
			message_count int NOT NULL DEFAULT 0,
			token_count   int NOT NULL DEFAULT 0,
			summary       text NOT NULL,
			created_at    timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

// The boundary and the summary text must travel together. anchorToSummary is
// how the caller says "the text is in this prompt": when it is false — the
// feature switched off, or the summary load failed — a window that still
// started at the boundary would hand the model LESS history than a plain
// tail, with nothing carrying what fell off. That silent shrink is exactly
// what happened on every failed summary load before the flag existed.
func TestDialogWindowIgnoresTheBoundaryWithoutTheSummaryText(t *testing.T) {
	db := windowTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mustExec(t, db, `INSERT INTO chat_sessions (id) VALUES ($1)`, sessionID)

	base := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	var ids []uuid.UUID
	for i := 0; i < 12; i++ {
		ids = append(ids, seedMessage(t, db, sessionID, i, base.Add(time.Duration(i)*time.Minute), 100))
	}
	// A summary row exists — up to message 9, leaving one complete user+
	// assistant pair after the boundary (a lone leading assistant turn would
	// be dropped as partial).
	mustExec(t, db,
		`INSERT INTO chat_session_summaries (session_id, to_message_id, message_count, token_count, summary)
		 VALUES ($1, $2, 10, 1000, 'сводка почти всего')`,
		sessionID, ids[9])

	anchored, err := store.DialogMessagesForAPI(ctx, sessionID.String(), 1000, true)
	if err != nil {
		t.Fatalf("anchored: %v", err)
	}
	if len(anchored) != 2 {
		t.Fatalf("with the summary in the prompt the window is %d messages, want the 2 post-boundary ones", len(anchored))
	}

	plain, err := store.DialogMessagesForAPI(ctx, sessionID.String(), 1000, false)
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if len(plain) <= len(anchored) {
		t.Fatalf("without the summary text the window stayed boundary-cut: %d messages vs %d anchored — the model loses history AND has no summary to cover it", len(plain), len(anchored))
	}
}
