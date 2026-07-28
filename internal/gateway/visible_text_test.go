package gateway

import "testing"

func TestJoinedVisibleTextKeepsTransportTextOutOfProviderExpansion(t *testing.T) {
	visible := appendVisibleTranscript("подпись", "голосовой транскрипт")
	msgs := []pendingMsg{{
		text:        "[reply to: parent]\n\nподпись\n\n[pdf: contract]\nbytes\n\nголосовой транскрипт",
		visibleText: &visible,
	}}
	got := joinedVisibleText(msgs)
	if got == nil || *got != "подпись\n\nголосовой транскрипт" {
		t.Fatalf("visible text = %#v", got)
	}
}

func TestJoinedVisibleTextPreservesBatchOrderAndAttachmentOnly(t *testing.T) {
	first, second := "первое", "второе"
	got := joinedVisibleText([]pendingMsg{
		{text: "provider first", visibleText: &first},
		{text: "provider second", visibleText: &second},
	})
	if got == nil || *got != "первое\n\nвторое" {
		t.Fatalf("batch visible text = %#v", got)
	}

	empty := ""
	got = joinedVisibleText([]pendingMsg{{images: nil, visibleText: &empty}})
	if got == nil || *got != "" {
		t.Fatalf("attachment-only visible text = %#v, want pointer to empty", got)
	}
}
