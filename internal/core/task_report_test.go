package core

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTaskReportNotificationSummarizesAndCarriesButton(t *testing.T) {
	id := uuid.New()
	body := "# Рынок AI-звонилок\n\n## TL;DR\nЛидеры — Vapi и Retell. Цена минуты $0.07–0.15.\n\n| Игрок | Цена |\n|---|---|\n| Vapi | $0.05 |\n\n## Детали\nДлинный раздел."
	text := TaskReportNotification("Готово: «%s»", "Рынок AI-звонилок", body, id)
	clean, got, ok := SplitTaskReportMarker(text)
	if !ok || got != id {
		t.Fatalf("button marker lost: %q", text)
	}
	if clean != "Готово: «Рынок AI-звонилок»\n\nЛидеры — Vapi и Retell. Цена минуты $0.07–0.15." {
		t.Fatalf("summary: %q", clean)
	}
	if strings.Contains(clean, "task_report") || strings.Contains(clean, "|") || strings.Contains(clean, "Детали") {
		t.Fatalf("notice leaks marker, table or later sections: %q", clean)
	}
	long := TaskReportNotification("Done: %s", "X", strings.Repeat("слово ", 400), id)
	if clean, _, _ := SplitTaskReportMarker(long); len([]rune(clean)) > 760 || !strings.HasSuffix(clean, "…") {
		t.Fatalf("long summary not bounded: %d", len([]rune(clean)))
	}
	if clean, _, ok := SplitTaskReportMarker("plain reminder text"); ok || clean != "plain reminder text" {
		t.Fatal("ordinary text changed")
	}
}
