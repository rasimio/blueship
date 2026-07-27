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
