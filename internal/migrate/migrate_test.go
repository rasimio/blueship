package migrate

import (
	"strings"
	"testing"
)

func TestAgentTaskDeliveriesMigrationMatchesInitSchema(t *testing.T) {
	incremental, err := migrations.ReadFile("sql/017_agent_task_deliveries.sql")
	if err != nil {
		t.Fatalf("read incremental migration: %v", err)
	}
	initSchema, err := migrations.ReadFile("sql/init.sql")
	if err != nil {
		t.Fatalf("read init schema: %v", err)
	}
	if !strings.Contains(string(initSchema), strings.TrimSpace(string(incremental))) {
		t.Fatal("init.sql agent_task_deliveries table differs from migration 017")
	}
}

func TestAgentTaskNotificationJournalMigrationMatchesInitSchema(t *testing.T) {
	incremental, err := migrations.ReadFile("sql/018_agent_task_notification_journal.sql")
	if err != nil {
		t.Fatalf("read incremental migration: %v", err)
	}
	initSchema, err := migrations.ReadFile("sql/init.sql")
	if err != nil {
		t.Fatalf("read init schema: %v", err)
	}
	if !strings.Contains(string(initSchema), strings.TrimSpace(string(incremental))) {
		t.Fatal("init.sql notification journal differs from migration 018")
	}

	sql := string(incremental)
	for _, required := range []string{
		"state IN ('dispatching', 'retryable', 'sent', 'uncertain', 'rejected')",
		"attempt_count  INTEGER NOT NULL DEFAULT 1 CHECK (attempt_count >= 1)",
		"next_attempt_at TIMESTAMPTZ",
		"last_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW()",
		"WHERE state = 'retryable'",
		"UNIQUE (task_id, occurrence_key)",
		"UNIQUE (task_id, input_id, item_key)",
		"REFERENCES agent_task_notification_attempts(id, task_id)",
		"ON DELETE CASCADE",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("notification journal migration lacks %q", required)
		}
	}
}

func TestAutonomousHistoryProjectionMigrationMatchesInitSchema(t *testing.T) {
	incremental, err := migrations.ReadFile("sql/019_autonomous_history_projection.sql")
	if err != nil {
		t.Fatalf("read incremental migration: %v", err)
	}
	initSchema, err := migrations.ReadFile("sql/init.sql")
	if err != nil {
		t.Fatalf("read init schema: %v", err)
	}
	if !strings.Contains(string(initSchema), strings.TrimSpace(string(incremental))) {
		t.Fatal("init.sql autonomous history projection differs from migration 019")
	}
}

func TestChatMessageProjectionMigrationMatchesInitSchema(t *testing.T) {
	incremental, err := migrations.ReadFile("sql/020_chat_message_projection.sql")
	if err != nil {
		t.Fatalf("read incremental migration: %v", err)
	}
	initSchema, err := migrations.ReadFile("sql/init.sql")
	if err != nil {
		t.Fatalf("read init schema: %v", err)
	}
	if !strings.Contains(string(initSchema), strings.TrimSpace(string(incremental))) {
		t.Fatal("init.sql chat message projection differs from migration 020")
	}

	sql := string(incremental)
	for _, required := range []string{
		"visible_text TEXT",
		"projection_status TEXT NOT NULL DEFAULT 'unprojectable_legacy'",
		"'projected'",
		"'non_dialogue'",
		"'unprojectable_legacy'",
		"projector_version TEXT NOT NULL DEFAULT 'legacy-unprojected'",
		"projection_status = 'projected'",
		"visible_text IS NOT NULL",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("chat message projection migration lacks %q", required)
		}
	}
	if got := strings.Count(sql, "NOT VALID"); got != 2 {
		t.Fatalf("chat message projection migration has %d NOT VALID constraints, want 2", got)
	}
}

func TestToolDescriptionsMigrationMatchesInitSchema(t *testing.T) {
	incremental, err := migrations.ReadFile("sql/021_tool_descriptions.sql")
	if err != nil {
		t.Fatalf("read incremental migration: %v", err)
	}
	initSchema, err := migrations.ReadFile("sql/init.sql")
	if err != nil {
		t.Fatalf("read init schema: %v", err)
	}
	if !strings.Contains(string(initSchema), strings.TrimSpace(string(incremental))) {
		t.Fatal("init.sql tool descriptions differs from migration 021")
	}

	sql := string(incremental)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS tool_descriptions",
		"name        TEXT PRIMARY KEY",
		"description TEXT NOT NULL",
		"version     TEXT NOT NULL DEFAULT 'v1'",
		"updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()",
		"CHECK (BTRIM(name) <> '')",
		"CHECK (BTRIM(description) <> '')",
		"CHECK (BTRIM(version) <> '')",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("tool descriptions migration lacks %q", required)
		}
	}
}

// TestInitSchemaDeclaresAgentTaskStateNotNull covers the path migration 022
// cannot: a database created from init.sql alone applies no ALTER, so a fresh
// install would keep the nullable columns that killed the scheduler loop —
// json.RawMessage has no sql.Scanner, and a single NULL progress fails every
// `SELECT * FROM agent_tasks` scan, not just its own row.
func TestInitSchemaDeclaresAgentTaskStateNotNull(t *testing.T) {
	initSchema, err := migrations.ReadFile("sql/init.sql")
	if err != nil {
		t.Fatalf("read init schema: %v", err)
	}
	for _, required := range []string{
		"progress       JSONB NOT NULL DEFAULT '{}'",
		"config         JSONB NOT NULL DEFAULT '{}'",
	} {
		if !strings.Contains(string(initSchema), required) {
			t.Fatalf("init.sql agent_tasks lacks %q — a fresh install would drift from migration 022", required)
		}
	}
}
