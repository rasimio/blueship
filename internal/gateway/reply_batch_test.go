package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	bs "github.com/rasimio/blueship/internal/core"
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

type fakeReplyParents struct {
	role, text string
	err        error
	askedID    string
}

func (f *fakeReplyParents) MessageForReply(_ context.Context, _, messageID string) (string, string, error) {
	f.askedID = messageID
	return f.role, f.text, f.err
}

func quoteBlockText(t *testing.T, blocks []bs.ContentBlock) string {
	t.Helper()
	if len(blocks) == 0 {
		t.Fatal("no blocks")
	}
	return blocks[0].Text
}

// The quote the model reads comes from the transcript, not from whatever the
// transport happened to include — the stored row holds the PROCESSED parent
// (a document's extracted text), which the wire quote is at best a preview of
// and, for a Rich Message, is not at all.
func TestReplyQuoteComesFromTheTranscript(t *testing.T) {
	parents := &fakeReplyParents{role: "user", text: "[pdf: contract] полный текст договора"}
	msgs := []pendingMsg{{text: "вот он", replyQuoteFallback: "превью с провода"}}

	out := prependReplyQuoteBlock(context.Background(), parents, testLogger(),
		"session", "parent-row", msgs, []bs.ContentBlock{{Type: "text", Text: "вот он"}})

	if len(out) != 2 {
		t.Fatalf("blocks = %#v, want quote + message", out)
	}
	if got := quoteBlockText(t, out); got != "[reply to: [pdf: contract] полный текст договора]" {
		t.Fatalf("quote = %q", got)
	}
	if parents.askedID != "parent-row" {
		t.Fatalf("asked for %q", parents.askedID)
	}
}

// A human parent is kept whole: it may be a processed attachment, and that
// expansion is not in the dialogue window — it IS the thing being asked
// about. The soul's own answer is already in the window, so it is cut.
func TestReplyQuoteKeepsTheHumanWholeAndCutsTheAnswer(t *testing.T) {
	long := strings.Repeat("я", 900)
	msgs := []pendingMsg{{text: "и что"}}

	whole := prependReplyQuoteBlock(context.Background(),
		&fakeReplyParents{role: "user", text: long}, testLogger(),
		"session", "parent", msgs, nil)
	if !strings.Contains(quoteBlockText(t, whole), long) {
		t.Fatal("a human parent was truncated")
	}

	cut := prependReplyQuoteBlock(context.Background(),
		&fakeReplyParents{role: "assistant", text: long}, testLogger(),
		"session", "parent", msgs, nil)
	quoted := quoteBlockText(t, cut)
	if !strings.HasSuffix(quoted, "...]") {
		t.Fatalf("an answer was not cut: %q", quoted)
	}
	// Cut by runes, never bytes: a byte cut lands mid-codepoint on Cyrillic
	// and hands the model broken UTF-8.
	if !utf8.ValidString(quoted) {
		t.Fatal("the cut produced invalid UTF-8")
	}
}

// A parent older than the id index resolves to nothing. The transport's own
// quote is what is left, and it is better than no context at all.
func TestReplyQuoteFallsBackToTheWireQuote(t *testing.T) {
	msgs := []pendingMsg{{text: "вот он", replyQuoteFallback: "Через 4 минуты начинается АЗН"}}

	out := prependReplyQuoteBlock(context.Background(),
		&fakeReplyParents{}, testLogger(), "session", "", msgs, nil)

	if got := quoteBlockText(t, out); got != "[reply to: Через 4 минуты начинается АЗН]" {
		t.Fatalf("quote = %q", got)
	}
}

// Nothing to quote must add nothing: an empty `[reply to: ]` in front of the
// turn is noise the model has to interpret.
func TestReplyQuoteAddsNothingWithoutAParent(t *testing.T) {
	blocks := []bs.ContentBlock{{Type: "text", Text: "просто сообщение"}}

	out := prependReplyQuoteBlock(context.Background(),
		&fakeReplyParents{}, testLogger(), "session", "", []pendingMsg{{text: "просто сообщение"}}, blocks)

	if len(out) != 1 || out[0].Text != "просто сообщение" {
		t.Fatalf("blocks = %#v, want the message untouched", out)
	}
}

// A lookup failure is not a reason to lose the reply: the wire quote still
// stands in, and the failure is logged rather than swallowed.
func TestReplyQuoteSurvivesALookupFailure(t *testing.T) {
	msgs := []pendingMsg{{text: "и?", replyQuoteFallback: "родитель"}}

	out := prependReplyQuoteBlock(context.Background(),
		&fakeReplyParents{err: errors.New("db down")}, testLogger(), "session", "parent", msgs, nil)

	if got := quoteBlockText(t, out); got != "[reply to: родитель]" {
		t.Fatalf("quote = %q", got)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
