package agent

import (
	"strings"
	"testing"
)

func TestAppendTurnText(t *testing.T) {
	tests := []struct {
		name  string
		turns []string
		want  string
	}{
		{
			// The regression: heartbeat emits the reminder prose in the turn
			// that also calls memory_update, then restates it as the terminal
			// turn. Accumulating both produced the doubled Telegram message.
			name:  "exact repeat dropped (heartbeat double-message)",
			turns: []string{"Через 20 минут совещание по B3301 — хватай блокнот.", "Через 20 минут совещание по B3301 — хватай блокнот."},
			want:  "Через 20 минут совещание по B3301 — хватай блокнот.",
		},
		{
			name:  "distinct turns accumulate, blank-line joined",
			turns: []string{"Step 1 done.", "Step 2 done."},
			want:  "Step 1 done.\n\nStep 2 done.",
		},
		{
			name:  "repeat with surrounding whitespace still dropped",
			turns: []string{"Готово.", "  Готово.  "},
			want:  "Готово.",
		},
		{
			name:  "whitespace-only turn dropped",
			turns: []string{"hello", "   "},
			want:  "hello",
		},
		{
			name:  "empty turns dropped",
			turns: []string{"", "real text", ""},
			want:  "real text",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			for _, turn := range tc.turns {
				appendTurnText(&sb, turn)
			}
			if got := sb.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
