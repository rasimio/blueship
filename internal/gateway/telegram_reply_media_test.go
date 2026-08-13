package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// A 1x1 PNG — enough for the classifier to sniff a real image signature.
var onePixelPNG = mustDecodeB64(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

func mustDecodeB64(s string) []byte {
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return out
}

type fakeDownloader struct {
	data     []byte
	err      error
	askedFor string
}

func (f *fakeDownloader) DownloadFile(_ context.Context, fileID string, _ int64) ([]byte, error) {
	f.askedFor = fileID
	return f.data, f.err
}

func testGateway() *Gateway {
	return &Gateway{logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// The picture the user is asking about is IN the update — Telegram sends the
// whole parent message, photo included. Not reading it is what made the soul
// tell a reader that a Telegram reply cannot carry a file through the API,
// which is untrue, about a picture she had drawn herself twenty minutes
// earlier.
func TestReplyParentMediaInlinesThePhoto(t *testing.T) {
	dl := &fakeDownloader{data: onePixelPNG}
	parent := &telegram.Message{
		MessageID: 18330,
		Photo: []telegram.PhotoSize{
			{FileID: "small", Width: 90},
			{FileID: "largest", Width: 1280},
		},
	}

	blocks := testGateway().replyParentMedia(context.Background(), dl, parent)

	if dl.askedFor != "largest" {
		t.Fatalf("downloaded %q, want the largest rendition", dl.askedFor)
	}
	if len(blocks) != 2 || blocks[1].Type != "image" || blocks[1].Source == nil {
		t.Fatalf("blocks = %#v, want a label plus a vision block", blocks)
	}
	// The media type comes from the bytes: a vision request whose declared
	// type disagrees with the content is rejected outright.
	if blocks[1].Source.MediaType != "image/png" {
		t.Fatalf("media type = %q, want the type sniffed from the bytes", blocks[1].Source.MediaType)
	}
	if blocks[1].Source.Data != base64.StdEncoding.EncodeToString(onePixelPNG) {
		t.Fatal("image bytes did not survive")
	}
}

func TestReplyParentMediaReadsADocument(t *testing.T) {
	dl := &fakeDownloader{data: []byte("line one\r\nline two")}
	parent := &telegram.Message{
		MessageID: 18331,
		Document:  &telegram.Document{FileID: "doc", FileName: "notes.txt", MimeType: "text/plain"},
	}

	blocks := testGateway().replyParentMedia(context.Background(), dl, parent)

	if len(blocks) != 1 || !strings.Contains(blocks[0].Text, "notes.txt") {
		t.Fatalf("blocks = %#v", blocks)
	}
	if !strings.Contains(blocks[0].Text, "line one\nline two") {
		t.Fatalf("document text missing or not normalised: %q", blocks[0].Text)
	}
}

// A message with no file is the common case; it must cost nothing.
func TestReplyParentMediaIgnoresATextParent(t *testing.T) {
	dl := &fakeDownloader{data: onePixelPNG}
	parent := &telegram.Message{MessageID: 18332, Text: "просто текст"}

	if blocks := testGateway().replyParentMedia(context.Background(), dl, parent); blocks != nil {
		t.Fatalf("blocks = %#v, want none", blocks)
	}
	if dl.askedFor != "" {
		t.Fatal("downloaded something for a parent that carries no file")
	}
}

// Losing the parent's file must not lose the message the reader actually sent.
func TestReplyParentMediaSurvivesADownloadFailure(t *testing.T) {
	dl := &fakeDownloader{err: errors.New("telegram 502")}
	parent := &telegram.Message{
		MessageID: 18333,
		Photo:     []telegram.PhotoSize{{FileID: "largest"}},
	}

	if blocks := testGateway().replyParentMedia(context.Background(), dl, parent); blocks != nil {
		t.Fatalf("blocks = %#v, want none on a failed download", blocks)
	}
}

// The transcript copy wins when there is one: it holds the processed form
// (a document's extracted text) and re-inlining the wire copy on top would
// put the same file in the turn twice.
func TestWireMediaOnlyStandsInWhenTheTranscriptHadNothing(t *testing.T) {
	msgs := []pendingMsg{{
		text:             "что тут не так?",
		replyMediaBlocks: []bs.ContentBlock{{Type: "text", Text: "[replied-to image: photo]"}},
	}}
	body := []bs.ContentBlock{{Type: "text", Text: "что тут не так?"}}

	out := prependReplyMediaBlocks(msgs, body)
	if len(out) != 2 || out[0].Text != "[replied-to image: photo]" {
		t.Fatalf("blocks = %#v, want the parent file in front", out)
	}

	// Nothing carried: the turn is untouched.
	if got := prependReplyMediaBlocks([]pendingMsg{{text: "x"}}, body); len(got) != 1 {
		t.Fatalf("blocks = %#v, want the message alone", got)
	}
}
