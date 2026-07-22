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
