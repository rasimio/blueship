package gateway

import "testing"

// Passes and tool names move together. The failure this pins is quiet: a turn
// granted tools but only one pass calls something, never gets a second pass to
// speak, and the person receives nothing at all.
func TestAutonomousTurnToolConfig(t *testing.T) {
	t.Run("no tools keeps the single-pass turn", func(t *testing.T) {
		passes, override, allowed := autonomousTurnToolConfig(nil)
		if passes != 1 {
			t.Fatalf("passes = %d, want 1", passes)
		}
		if len(override) != 0 || len(allowed) != 0 {
			t.Fatalf("tool lists = %v / %v, want empty", override, allowed)
		}
	})

	t.Run("tools open the extra passes they need", func(t *testing.T) {
		passes, override, allowed := autonomousTurnToolConfig([]string{"image_generate", "attachment_include"})
		if passes < 2 {
			t.Fatalf("passes = %d — a tool call leaves nothing to answer with", passes)
		}
		if len(override) != 2 || len(allowed) != 2 {
			t.Fatalf("tool lists = %v / %v, want both names", override, allowed)
		}
	})

	t.Run("blank entries do not buy passes", func(t *testing.T) {
		passes, _, allowed := autonomousTurnToolConfig([]string{"  ", ""})
		if passes != 1 || len(allowed) != 0 {
			t.Fatalf("passes = %d, allowed = %v — whitespace was taken for a tool", passes, allowed)
		}
	})
}
