package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/attachment"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/webaccess/browser"
)

func (g *Gateway) resolveInlineAttachmentRefs(ctx context.Context, us *UserState, blocks []bs.ContentBlock) []bs.ContentBlock {
	if g.deps.AttachmentSink == nil || us.UserID == uuid.Nil || us.SoulID == uuid.Nil {
		return blocks
	}
	seen := map[uuid.UUID]bool{}
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		matches := uuidInTextRE.FindAllString(b.Text, -1)
		for _, m := range matches {
			id, perr := uuid.Parse(strings.ToLower(m))
			if perr != nil || seen[id] {
				continue
			}
			seen[id] = true
		}
	}
	if len(seen) == 0 {
		return blocks
	}

	ids := make([]uuid.UUID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	blocks = append(blocks, g.attachmentBlocksByIDs(ctx, us, ids, "ref")...)
	return blocks
}

// attachmentBlocksByIDs resolves a set of attachment ids via the
// host's CDN and renders each as one content block — image vision
// block for picture kinds, fenced text dump for files. `labelPrefix`
// is the bracket tag the resulting text blocks carry ("ref" for
// inline-UUID resolution, "reply-attached" for the reply-context
// expander), so cortex reads the same syntax in two cases and
// distinguishes provenance. Unknown / foreign ids are skipped
// silently — caller has already done a tenancy check via Get.
func (g *Gateway) attachmentBlocksByIDs(ctx context.Context, us *UserState, ids []uuid.UUID, labelPrefix string) []bs.ContentBlock {
	if g.deps.AttachmentSink == nil || us.UserID == uuid.Nil || us.SoulID == uuid.Nil {
		return nil
	}
	out := make([]bs.ContentBlock, 0, len(ids))
	for _, id := range ids {
		rec, data, err := g.deps.AttachmentSink.Get(ctx, us.UserID, us.SoulID, id)
		if err != nil {
			g.logger.Debug("attachment block: not resolved",
				"chat_id", us.ChatID, "id", id, "err", err)
			continue
		}
		if rec == nil || len(data) == 0 {
			continue
		}
		switch rec.Kind {
		case "image":
			media := rec.Mime
			if media == "" {
				media = "image/jpeg"
			}
			out = append(out, bs.ContentBlock{
				Type: "image",
				Source: &bs.ImageSource{
					Type:      "base64",
					MediaType: media,
					Data:      base64.StdEncoding.EncodeToString(data),
				},
			})
		case "pdf":
			// Prefer the host-supplied source (markdown for host-
			// generated PDFs) over re-extracting from the rendered
			// bytes — chromedp font subsets aren't readable by
			// ledongthuc/pdf and produce mojibake.
			if rec.SourceText != "" {
				out = append(out, bs.ContentBlock{
					Type: "text",
					Text: fmt.Sprintf("[%s: %s — pdf, markdown source]\n%s", labelPrefix, rec.Name, rec.SourceText),
				})
				continue
			}
			text, _, perr := browser.ExtractPDFText(data)
			if perr != nil {
				out = append(out, bs.ContentBlock{
					Type: "text",
					Text: fmt.Sprintf("[%s: %s — pdf extract failed: %v]", labelPrefix, rec.Name, perr),
				})
				continue
			}
			out = append(out, bs.ContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[%s: %s — pdf]\n%s", labelPrefix, rec.Name, text),
			})
		case "text":
			out = append(out, bs.ContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[%s: %s]\n```\n%s\n```", labelPrefix, rec.Name, string(data)),
			})
		}
	}
	return out
}

// attachMarkerRE matches the `[attached: UUID]` sentinel the
// attachment_include tool emits. The gateway's post-loop dispatcher
// (dispatchAttachmentMarkers) rewrites these into transport-native
// file sends — for Telegram, a SendPhoto / SendDocument per marker;
// for the cabinet, the marker stays in the text and the history
// endpoint resolves it into an attachment MessagePart at read time.
var attachMarkerRE = regexp.MustCompile(`(?i)\[attached:\s*([0-9a-f-]{36})\s*\]`)

// dispatchAttachmentMarkers walks the assistant's reply text, looks
// up every `[attached: UUID]` reference against the host's
// AttachmentSink, and either:
//
//   - ships the bytes out the current sink (when the sink implements
//     AttachmentSendSink — Telegram does), then strips the marker
//     from the text so the user doesn't see the raw sentinel;
//   - leaves the marker in place when the sink can't send files
//     directly (cabinet's SSE sink) — vaelum's history endpoint
//     parses the marker on read and emits an attachment chip.
//
// Unknown / foreign UUIDs are stripped silently so a hallucinated
// marker doesn't leak a sentinel into chat. Sink errors are warn-
// logged but don't fail the turn — the text reply still goes out.
func (g *Gateway) dispatchAttachmentMarkers(ctx context.Context, us *UserState, sink bs.ResponseSink, reply string) string {
	if reply == "" || g.deps.AttachmentSink == nil {
		return reply
	}
	matches := attachMarkerRE.FindAllStringSubmatchIndex(reply, -1)
	if len(matches) == 0 {
		return reply
	}
	sender, sendable := sink.(bs.AttachmentSendSink)
	if !sendable {
		// Cabinet path: keep markers, history endpoint will resolve.
		return reply
	}

	// Collect ids we need (de-duped) so a marker repeated twice
	// doesn't double-send.
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	for _, m := range matches {
		idStr := strings.ToLower(reply[m[2]:m[3]])
		id, perr := uuid.Parse(idStr)
		if perr != nil {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}

	for _, id := range ids {
		rec, data, err := g.deps.AttachmentSink.Get(ctx, us.UserID, us.SoulID, id)
		if err != nil {
			g.logger.Warn("attachment marker: resolve failed",
				"chat_id", us.ChatID, "id", id, "err", err)
			continue
		}
		if rec == nil {
			continue
		}
		if err := sender.SendAttachment(ctx, *rec, data); err != nil {
			g.logger.Warn("attachment marker: send failed",
				"chat_id", us.ChatID, "id", id, "err", err)
		}
	}

	cleaned := attachMarkerRE.ReplaceAllString(reply, "")
	// Collapse the blank lines a stripped sentinel leaves behind so
	// the message text reads naturally on the user's side.
	return collapseBlankLinesGateway(cleaned)
}

// collapseBlankLinesGateway is the gateway-local copy of the same
// helper vaelum's buildParts uses; lives here so the gateway doesn't
// take a vaelum-side import.
func collapseBlankLinesGateway(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// scanAndSaveLinks extracts http/https URLs out of text and upserts
// them as kind='link' rows on the host's AttachmentSink. Symmetric to
// the byte-attachment Save call: the user side runs this against the
// pasted message, the assistant side runs it against Arlene's final
// reply. The OG enrichment worker picks the rows up from there.
//
// sessionID is required (so the cabinet's session-scoped view can
// surface the chip on the right turn). messageID is best-effort —
// callers pass uuid.Nil when the underlying chat_messages row hasn't
// been written yet (user side: append happens later inside the agent
// loop). Empty text and a nil AttachmentSink are silent no-ops. URL
// upsert failures are warn-logged and per-URL — one bad URL does not
// abort the rest of the scan or the turn.
func (g *Gateway) scanAndSaveLinks(ctx context.Context, us *UserState, sessionID, messageID uuid.UUID, source, text string) {
	if g.deps.AttachmentSink == nil || us == nil || us.UserID == uuid.Nil || us.SoulID == uuid.Nil {
		return
	}
	if text == "" {
		return
	}
	urls := attachment.ExtractURLs(text)
	if len(urls) == 0 {
		return
	}
	for _, u := range urls {
		if _, err := g.deps.AttachmentSink.SaveLink(ctx, bs.LinkParams{
			UserID:    us.UserID,
			SoulID:    us.SoulID,
			SessionID: sessionID,
			MessageID: messageID,
			URL:       u,
		}); err != nil {
			g.logger.Warn("link save failed",
				"chat_id", us.ChatID,
				"source", source,
				"url", u,
				"err", err,
			)
		}
	}
}

// hasHeavyContent reports whether a user-message payload is unsuited
// for the fast reflex tier. Two cases collapse to the same action:
//  1. Any image block — the codex provider's text-only serializer
//     silently drops image content, so reflex never sees the bytes
//     and either hallucinates a description or routes wrong.
//  2. Total text past ~16 KiB (≈ 4K tokens, double reflex's
//     MessageBudget) — the chatgpt.com codex endpoint returns a
//     misleading 400 ("expected a string, but got an object") on
//     inputs it can't handle. PDF and text-doc attachments inline
//     as a single huge text block in the daemon's /chat handler
//     and trip this on the first user turn that carries them.
//
// Either way the right move is to skip the fast tier and run cortex
// (claude-opus-4-8) directly — it has the context budget for the
// turn and would have been called via escalation anyway.
func hasHeavyContent(content any) bool {
	const heavyTextBytes = 16 << 10 // 16 KiB ≈ 4K tokens
	switch v := content.(type) {
	case string:
		return len(v) > heavyTextBytes
	case []bs.ContentBlock:
		textBytes := 0
		for _, b := range v {
			if b.Type == "image" {
				return true
			}
			if b.Type == "text" {
				textBytes += len(b.Text)
				if textBytes > heavyTextBytes {
					return true
				}
			}
		}
	}
	return false
}

// appendDocInline glues an attached document rendering onto whatever
// the user typed, separating with a blank line so the model sees two
// distinct passages rather than a wall of text. Empty existing text
// (a doc-only turn) skips the leading newlines.
func appendDocInline(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "\n\n" + addition
}

// getOrInitUser builds UserState for non-Telegram entry points
// (voice:owner legacy WS, ProcessInbound). The Telegram path uses
// getOrInitTelegramUser, which resolves through vaelum.bot_links and
// stamps the receiving bot onto UserState.
