package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/rasimio/blueship/attachment"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
	"github.com/rasimio/blueship/internal/webaccess/browser"
)

// telegramFileDownloader is the slice of the Telegram client this file needs.
type telegramFileDownloader interface {
	DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error)
}

// replyParentMedia reads the file carried by the message a reply points at,
// straight off the update.
//
// Telegram puts the whole parent in `reply_to_message`, photo and document
// included — we simply never looked. The soul told a user, confidently, that
// "a Telegram reply does not carry the file through the API unless it is
// attached to that message"; that is not true, and it was the only thing
// standing between her and the picture she was being asked about.
//
// This is the path that does not depend on anything we recorded: it works for
// a message older than the reply index, for one whose attachment never got
// linked to its row, and for a picture the soul herself sent. The transcript
// is still preferred where it has more — a document's extracted text is
// richer than a caption — so the caller only falls back here.
//
// An image comes back as a vision block; everything else as text, because
// that is what the model can actually read. Failures are logged and yield
// nothing rather than aborting the turn: losing the quote is better than
// losing the message.
func (g *Gateway) replyParentMedia(
	ctx context.Context,
	client telegramFileDownloader,
	parent *telegram.Message,
) []bs.ContentBlock {
	if parent == nil || client == nil {
		return nil
	}
	fileID, name, mime := replyParentFile(parent)
	if fileID == "" {
		return nil
	}
	data, err := client.DownloadFile(ctx, fileID, attachment.MaxAnyBytes)
	if err != nil {
		g.logger.Warn("reply-media: download failed",
			"reply_msg_id", parent.MessageID, "file", name, "error", err)
		return nil
	}
	kind := attachment.Kind(mime, name, data)
	if cap := attachment.MaxBytesForKind(kind); cap > 0 && int64(len(data)) > cap {
		g.logger.Info("reply-media: parent file over the kind cap, quoting its name only",
			"reply_msg_id", parent.MessageID, "file", name, "kind", kind, "size", len(data))
		return []bs.ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("[replied-to file: %s — too large to read here]", displayName(name, kind)),
		}}
	}

	label := displayName(name, kind)
	switch kind {
	case "image":
		// media_type comes from the bytes, never from the header: a renamed
		// PNG arrives with a stale MIME and vision refuses a request whose
		// declared type disagrees with the content.
		media := attachment.MimeForImage(data)
		if media == "" {
			media = "image/jpeg"
		}
		g.logger.Info("reply-media: parent image inlined",
			"reply_msg_id", parent.MessageID, "bytes", len(data), "mime", media)
		return []bs.ContentBlock{
			{Type: "text", Text: fmt.Sprintf("[replied-to image: %s]", label)},
			{Type: "image", Source: &bs.ImageSource{
				Type: "base64", MediaType: media, Data: base64.StdEncoding.EncodeToString(data),
			}},
		}
	case "pdf":
		text, _, perr := browser.ExtractPDFText(data)
		return replyParentText(g, parent, label, "pdf", text, perr)
	case "docx":
		text, derr := attachment.ExtractDocxText(data)
		return replyParentText(g, parent, label, "docx", text, derr)
	case "xlsx":
		text, xerr := attachment.ExtractXlsxMarkdown(data)
		return replyParentText(g, parent, label, "xlsx", text, xerr)
	case "text":
		return replyParentText(g, parent, label, "file",
			strings.ReplaceAll(string(data), "\r\n", "\n"), nil)
	}
	return nil
}

func replyParentText(g *Gateway, parent *telegram.Message, label, kind, text string, err error) []bs.ContentBlock {
	if err != nil {
		g.logger.Warn("reply-media: could not read the parent file",
			"reply_msg_id", parent.MessageID, "file", label, "kind", kind, "error", err)
		return []bs.ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("[replied-to %s: %s — could not be read]", kind, label),
		}}
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	g.logger.Info("reply-media: parent file inlined",
		"reply_msg_id", parent.MessageID, "kind", kind, "chars", len(text))
	return []bs.ContentBlock{{
		Type: "text",
		Text: fmt.Sprintf("[replied-to %s: %s]\n%s", kind, label, text),
	}}
}

// replyParentFile picks the one file a parent message carries. A photo has no
// filename of its own, so the caller labels it by kind instead.
func replyParentFile(parent *telegram.Message) (fileID, name, mime string) {
	if len(parent.Photo) > 0 {
		// Last entry is the largest rendition Telegram kept.
		return parent.Photo[len(parent.Photo)-1].FileID, "", "image/jpeg"
	}
	if parent.Document != nil {
		return parent.Document.FileID, parent.Document.FileName, parent.Document.MimeType
	}
	return "", "", ""
}

func displayName(name, kind string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	if kind == "" {
		return "file"
	}
	return kind
}
