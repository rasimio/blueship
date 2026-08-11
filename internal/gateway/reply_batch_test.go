package gateway

import (
	"context"
	"testing"
)

// The debouncer folds a burst of Telegram messages into one turn and one
// durable row. The row therefore has to stand for ALL of them: a reply names
// one specific message, and in the live failure it named the second — a
// caption-less PDF sent a second after the question about it. Keeping only
// msgs[0] left that message unaddressable, so the reply carried neither the
// quote nor the file and the answer was that no contract had ever been seen.
func TestResolveReplyMetadataKeepsEveryMessageOfTheBurst(t *testing.T) {
	msgs := []pendingMsg{
		{text: "Договор ок?", messageID: 18170},
		{text: "[pdf: 2410 MBL RESIDENCE TC.pdf — 3 pages]", messageID: 18171},
	}

	_, tgMessageIDs, err := resolveReplyMetadata(
		context.Background(), &attachmentTGReplyLookup{}, "session", msgs,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(tgMessageIDs) != 2 || tgMessageIDs[0] != 18170 || tgMessageIDs[1] != 18171 {
		t.Fatalf("burst ids = %v, want both messages of the burst", tgMessageIDs)
	}
}

// A turn that did not come from Telegram carries no ids, and must not
// manufacture one — a zero would index the row under a message that exists in
// every chat.
func TestResolveReplyMetadataCarriesNoIDsOffTelegram(t *testing.T) {
	_, tgMessageIDs, err := resolveReplyMetadata(
		context.Background(), &attachmentTGReplyLookup{}, "session",
		[]pendingMsg{{text: "из кабинета"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(tgMessageIDs) != 0 {
		t.Fatalf("non-Telegram turn produced ids = %v", tgMessageIDs)
	}
}

// The parent is looked up from the message the user actually replied to,
// which is the first of the burst — the reply arrives on the message that
// opens a turn, and a burst has one reply target between all of it.
func TestResolveReplyMetadataLooksUpTheReplyTarget(t *testing.T) {
	lookup := &attachmentTGReplyLookup{parentID: "parent-row"}
	parent, _, err := resolveReplyMetadata(
		context.Background(), lookup, "session",
		[]pendingMsg{{text: "вот он", messageID: 18179, replyToTGMessageID: 18171}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if parent != "parent-row" {
		t.Fatalf("parent = %q, want the looked-up row", parent)
	}
	if lookup.tgMessageID != 18171 || lookup.sessionID != "session" {
		t.Fatalf("lookup scope = (%q, %d)", lookup.sessionID, lookup.tgMessageID)
	}
}
