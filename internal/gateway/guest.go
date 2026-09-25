package gateway

// Telegram guest mode: the bot is summoned — by an @mention, or by a reply to
// its guest answer — into a chat it is not a member of. The update carries the
// summoning message and, when there is one, the message it replied to; nothing
// else of the chat. The bot answers exactly once, through answerGuestQuery,
// and every later word goes in as an edit of that one message.
//
// The turn belongs to the caller. It runs on the soul the caller talks to on
// this bot, in their one conversation, so memory and history stay whole, and
// the history marks it as a guest turn. A user's own bot answers only its
// owner, here as in its private chat; anybody else is ignored.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/attachment"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// guestEditInterval throttles streamed edits of a guest answer. A guest
// answer usually lands in a busy group, and inline edits share the bot's
// rate limits; a second between edits still reads as live.
const guestEditInterval = time.Second

// guestClient is the slice of the Telegram client a guest answer needs.
type guestClient interface {
	AnswerGuestQuery(ctx context.Context, guestQueryID, text string) (string, error)
	EditInlineMessageText(ctx context.Context, inlineMessageID, text string) error
	FinalizeInlineWithMedia(ctx context.Context, inlineMessageID, text string, media []telegram.InlineMedia) error
	UploadForInline(ctx context.Context, chatID int64, kind, filename, mime string, data []byte) (telegram.InlineMedia, int, error)
	DeleteMessage(ctx context.Context, chatID int64, messageID int) error
}

// handleGuestMessage takes a guest summons off the dispatch loop: resolving
// the caller, downloading a replied-to picture and the turn itself would
// otherwise hold up every bot's updates. The drain guard covers it like any
// other turn.
func (g *Gateway) handleGuestMessage(ctx context.Context, bi *botInstance, msg *telegram.Message) {
	if msg.GuestQueryID == "" || msg.From == nil || bi == nil || bi.client == nil {
		return
	}
	end := g.drain.begin()
	go func() {
		defer end()
		g.answerGuest(ctx, bi, msg)
	}()
}

func (g *Gateway) answerGuest(ctx context.Context, bi *botInstance, msg *telegram.Message) {
	userID, soulID, ok := g.admitGuest(ctx, bi, msg)
	if !ok {
		return
	}
	sink := &guestSink{
		client:       bi.client,
		queryID:      msg.GuestQueryID,
		uploadChatID: msg.From.ID,
		noAnswer:     g.deps.Config.UI.GuestNoAnswer,
		logger:       g.logger,
	}
	// Answered before the turn starts: the summons has to be answered while
	// the turn is still gathering memory, and the placeholder is also the
	// only sign in that chat that anything is happening.
	if err := sink.open(ctx, g.deps.Config.UI.GuestPlaceholder); err != nil {
		g.logger.Warn("guest: could not answer the summons",
			"bot_id", bi.id.String(), "caller", msg.From.ID, "error", err)
		return
	}
	g.logger.Info("guest: summons accepted",
		"bot_id", bi.id.String(), "caller", msg.From.ID, "chat_type", msg.Chat.Type,
		"user_id", userID.String(), "soul_id", soulID.String())

	us := g.getOrInitTelegramGuestUser(bi, msg.From.ID, userID, soulID)
	pending := []pendingMsg{g.guestPendingMsg(ctx, bi, msg)}
	carryInboundAdmission(pending, g.admitInboundActivity(us))
	g.processMessages(turnContext(ctx, g.deps.Config.Gateway.DrainTimeout), us, pending, sink)
	sink.settle(ctx)
}

// admitGuest decides whether a summons gets a turn, and answers it itself
// when it does not: a stranger on the platform bot is told how to start, a
// refused turn gets the host's refusal. A summons that is ignored is left
// unanswered, which shows nothing in the chat.
func (g *Gateway) admitGuest(ctx context.Context, bi *botInstance, msg *telegram.Message) (uuid.UUID, uuid.UUID, bool) {
	callerID := msg.From.ID
	if g.deps.ResolveTelegramChat == nil {
		g.logger.Warn("guest: ResolveTelegramChat hook not configured")
		return uuid.Nil, uuid.Nil, false
	}
	// The caller's conversation is bound to their private chat with this
	// bot, whose id is their user id. The chat they summoned it into is
	// bound to nobody.
	userID, soulID, err := g.deps.ResolveTelegramChat(ctx, bi.id, callerID)
	if errors.Is(err, bs.ErrTelegramChatUnpaired) {
		if bi.kind == "user" {
			g.logger.Info("guest: ignoring a summons from a stranger on a user bot",
				"bot_id", bi.id.String(), "caller", callerID)
			return uuid.Nil, uuid.Nil, false
		}
		g.logger.Info("guest: summoned by someone the platform bot does not know",
			"bot_id", bi.id.String(), "caller", callerID)
		g.answerGuestOnce(ctx, bi, msg.GuestQueryID,
			fmt.Sprintf(g.deps.Config.UI.GuestUnpairedFmt, "https://t.me/"+bi.tgUsername))
		return uuid.Nil, uuid.Nil, false
	}
	if err != nil {
		g.logger.Warn("guest: could not resolve the caller",
			"bot_id", bi.id.String(), "caller", callerID, "error", err)
		return uuid.Nil, uuid.Nil, false
	}
	if bi.kind == "user" && userID != bi.ownerUserID {
		g.logger.Info("guest: ignoring a summons from someone other than the bot's owner",
			"bot_id", bi.id.String(), "caller", callerID, "user_id", userID.String())
		return uuid.Nil, uuid.Nil, false
	}

	decision, err := g.authorizeExecution(ctx, userID, soulID, bs.ExecutionInteractive, "telegram")
	if err != nil {
		g.logger.Warn("guest: execution authorization failed",
			"caller", callerID, "user_id", userID, "error", err)
		return uuid.Nil, uuid.Nil, false
	}
	if !decision.Allowed {
		g.logger.Info("guest: execution denied",
			"caller", callerID, "user_id", userID, "reason", decision.Reason)
		// Without the host's action buttons: they are callback buttons,
		// and a callback from a guest answer carries no chat to act in.
		denial := decision.Message
		if denial == "" {
			denial = g.deps.Config.UI.ExecutionDenied
		}
		g.answerGuestOnce(ctx, bi, msg.GuestQueryID, denial)
		return uuid.Nil, uuid.Nil, false
	}
	return userID, soulID, true
}

func (g *Gateway) answerGuestOnce(ctx context.Context, bi *botInstance, queryID, text string) {
	if _, err := bi.client.AnswerGuestQuery(ctx, queryID, text); err != nil {
		g.logger.Warn("guest: answer failed", "bot_id", bi.id.String(), "error", err)
	}
}

// getOrInitTelegramGuestUser is the caller's state for guest turns: the same
// user, soul and conversation as their private chat with the bot — ChatID
// included, so anything a tool delivers later lands in that chat, the one
// place the bot can still write to — but cached apart from it, because the
// private chat's state owns a debouncer that answers into the private chat.
func (g *Gateway) getOrInitTelegramGuestUser(bi *botInstance, callerID int64, userID, soulID uuid.UUID) *UserState {
	key := "telegram-guest:" + bi.id.String() + ":" + userID.String() + ":" + soulID.String()
	g.mu.Lock()
	defer g.mu.Unlock()
	if us, ok := g.users[key]; ok {
		us.bot = bi
		return us
	}
	us := g.buildUserState(tgCanonical(callerID), userID, soulID, false, bi, callerID)
	g.users[key] = us
	return us
}

// guestPendingMsg turns a summons into the turn's one message.
//
// The history keeps the request under a guest marker. The model additionally
// gets, for this turn only, where it has been called and by whom, and the
// message the summons replied to — which the history does not keep, since it
// may be somebody else's words from somebody else's chat.
//
// No Telegram message ids: they belong to the chat the bot was called into,
// and indexed under the caller's conversation they would collide with ids
// from the private chat.
func (g *Gateway) guestPendingMsg(ctx context.Context, bi *botInstance, msg *telegram.Message) pendingMsg {
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	text = stripBotMention(text, bi.tgUsername)

	where := g.deps.Config.UI.GuestPrivateChat
	if msg.Chat.Type != "private" && strings.TrimSpace(msg.Chat.Title) != "" {
		where = strings.TrimSpace(msg.Chat.Title)
	}
	visible := fmt.Sprintf(g.deps.Config.UI.GuestMarkerFmt, where)
	if text != "" {
		visible += " " + text
	}

	p := pendingMsg{
		text:          text,
		visibleText:   &visible,
		transportNote: guestNote(msg),
	}
	if len(msg.Photo) > 0 {
		p.images, p.rawAttachments = g.guestPhoto(ctx, bi, msg.Photo[len(msg.Photo)-1])
	}
	if parent := msg.ReplyToMessage; parent != nil {
		p.replyQuoteFallback = guestQuote(parent, bi.tgBotID)
		p.replyMediaBlocks = g.replyParentMedia(ctx, bi.client, parent)
	}
	return p
}

// guestPhoto reads the picture a summons was sent with, the same way the
// private chat does.
func (g *Gateway) guestPhoto(ctx context.Context, bi *botInstance, photo telegram.PhotoSize) ([]bs.ContentBlock, []rawAttachment) {
	data, err := bi.client.DownloadFile(ctx, photo.FileID, attachment.MaxImageBytes)
	if err != nil {
		g.logger.Warn("guest: failed to download photo", "error", err, "file_id", photo.FileID)
		return nil, nil
	}
	media := attachment.MimeForImage(data)
	if media == "" {
		media = "image/jpeg"
	}
	block := bs.ContentBlock{
		Type: "image",
		Source: &bs.ImageSource{
			Type:      "base64",
			MediaType: media,
			Data:      base64.StdEncoding.EncodeToString(data),
		},
	}
	raw := rawAttachment{name: "tg-" + photo.FileID + ".jpg", mime: media, kind: "image", data: data}
	return []bs.ContentBlock{block}, []rawAttachment{raw}
}

// guestNote tells the model what it cannot see for itself: that this answer
// goes to a chat full of people who are not in the conversation, as one
// message, with no history behind it.
func guestNote(msg *telegram.Message) string {
	caller := telegramDisplayName(msg.From)
	where := "a private chat between " + caller + " and someone else"
	if msg.Chat.Type != "private" {
		where = "a group chat"
		if title := strings.TrimSpace(msg.Chat.Title); title != "" {
			where = fmt.Sprintf("the group chat %q", title)
		}
	}
	return fmt.Sprintf("[guest call] %s called you into %s, which you are not a member of. "+
		"Everyone in that chat reads your answer. It is a single message of at most 4000 characters: "+
		"you cannot see the chat's history or write a second message, and the exchange continues only "+
		"if someone replies to your answer or calls you again. Answer there, to the point. "+
		"What you know from your private conversations with %s stays private unless they ask for it here.",
		caller, where, caller)
}

// guestQuote is the replied-to message as the model sees it: who wrote it
// and what it says. A guest bot gets no chat history, so this is all the
// context a summons carries besides its own words.
func guestQuote(parent *telegram.Message, botTGID int64) string {
	text := parent.Text
	if text == "" {
		text = parent.Caption
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if parent.From == nil {
		return text
	}
	if botTGID != 0 && parent.From.ID == botTGID {
		return "your earlier answer in this chat: " + text
	}
	return telegramDisplayName(parent.From) + ": " + text
}

func telegramDisplayName(u *telegram.User) string {
	if u == nil {
		return "someone"
	}
	name := strings.TrimSpace(u.FirstName)
	switch {
	case name != "" && u.Username != "":
		return name + " (@" + u.Username + ")"
	case name != "":
		return name
	case u.Username != "":
		return "@" + u.Username
	}
	return "someone"
}

var spaceRunRE = regexp.MustCompile(`[ \t]{2,}`)

// stripBotMention removes the summoning @mention, so the request reads as
// what the caller asked rather than as an address to a bot.
func stripBotMention(text, username string) string {
	if username == "" {
		return strings.TrimSpace(text)
	}
	re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(username) + `\b`)
	text = re.ReplaceAllString(text, "")
	return strings.TrimSpace(spaceRunRE.ReplaceAllString(text, " "))
}

// guestSink writes a turn into the one message a guest summons allows. The
// placeholder goes up before the turn starts, streamed text replaces it at a
// throttled pace, the final answer is rendered over it — with the turn's
// files, when it produced any — and settle covers a turn that ended with
// nothing delivered.
type guestSink struct {
	client   guestClient
	queryID  string
	noAnswer string
	logger   *slog.Logger
	// uploadChatID is the caller's private chat with the bot, where a file
	// has to be sent to get the file id an inline message needs.
	uploadChatID int64

	mu        sync.Mutex
	inlineID  string
	buf       strings.Builder
	lastEdit  time.Time
	delivered bool
	failed    bool // log the first streamed-edit failure, not every delta
	media     []telegram.InlineMedia
	staged    []int // private-chat messages that exist only to hold media
}

func (s *guestSink) open(ctx context.Context, placeholder string) error {
	id, err := s.client.AnswerGuestQuery(ctx, s.queryID, placeholder)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.inlineID = id
	s.lastEdit = time.Now()
	s.mu.Unlock()
	return nil
}

func (s *guestSink) SendTextDelta(ctx context.Context, delta string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.WriteString(delta)
	if s.inlineID == "" || s.delivered || time.Since(s.lastEdit) < guestEditInterval {
		return nil
	}
	text := telegramPreviewText(stripAttachMarkers(s.buf.String()), "")
	if text == "" {
		return nil
	}
	s.lastEdit = time.Now()
	if err := s.client.EditInlineMessageText(ctx, s.inlineID, text); err != nil && !s.failed {
		s.failed = true
		s.logger.Warn("guest: streamed edit failed", "error", err)
	}
	return nil
}

// SendAttachment takes a file the turn produced — a generated picture, a
// report — into the answer. It cannot be sent as a message of its own, so
// it is uploaded through the caller's private chat for a file id and goes
// into the answer's one message with the final text. Markers the gateway
// could not resolve are dropped from the text rather than shown as raw ids.
func (s *guestSink) SendAttachment(ctx context.Context, rec bs.AttachmentRecord, data []byte) (int, error) {
	kind := "document"
	if rec.Kind == "image" {
		kind = "photo"
	}
	name := rec.Name
	if name == "" {
		name = "file"
	}
	media, stagedID, err := s.client.UploadForInline(ctx, s.uploadChatID, kind, name, rec.Mime, data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if stagedID != 0 {
		s.staged = append(s.staged, stagedID)
	}
	if err != nil {
		return 0, err
	}
	s.media = append(s.media, media)
	// No message id: nothing in any chat stands for this file on its own.
	return 0, nil
}

// SendFinalText renders the finished answer, with the turn's files.
func (s *guestSink) SendFinalText(ctx context.Context, text string) error {
	text = stripAttachMarkers(text)
	s.mu.Lock()
	id := s.inlineID
	media := append([]telegram.InlineMedia(nil), s.media...)
	s.mu.Unlock()
	if text == "" && len(media) == 0 {
		return nil
	}
	if id == "" {
		return fmt.Errorf("guest answer: the summons was never answered")
	}
	// Delivery outlives the turn context, as on the private chat: an answer
	// already written must reach the chat.
	deliveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := s.client.FinalizeInlineWithMedia(deliveryCtx, id, text, media)
	if err == nil {
		s.mu.Lock()
		s.delivered = true
		s.mu.Unlock()
	}
	return err
}

// SendText carries whatever the turn says outside the streamed answer — an
// error report — into the same message.
func (s *guestSink) SendText(ctx context.Context, text string) error {
	return s.SendFinalText(ctx, text)
}

func (s *guestSink) SendVoice(context.Context, []byte) error {
	return fmt.Errorf("guest answer: voice is not supported")
}

// SendTyping is a no-op: a bot cannot show typing in a chat it is not in.
// The placeholder stands in for it.
func (s *guestSink) SendTyping(context.Context) error { return nil }

// settle closes out the answer once the turn is over.
//
// A turn that delivered no final answer — failed, silenced by a rule, or
// produced nothing — keeps what streamed; with nothing streamed the
// placeholder is replaced, so the chat is not left with a bot that looks like
// it is still thinking. Then the private-chat copies made for file ids go:
// by now the answer holds the files, and the caller never asked for them
// there.
func (s *guestSink) settle(ctx context.Context) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	s.mu.Lock()
	finish := !s.delivered && s.inlineID != ""
	s.delivered = true
	id := s.inlineID
	text := stripAttachMarkers(s.buf.String())
	media := append([]telegram.InlineMedia(nil), s.media...)
	staged := s.staged
	s.staged = nil
	s.mu.Unlock()

	if finish {
		if text == "" && len(media) == 0 {
			text = s.noAnswer
		}
		if err := s.client.FinalizeInlineWithMedia(settleCtx, id, text, media); err != nil {
			s.logger.Warn("guest: settle failed", "error", err)
		}
	}
	for _, messageID := range staged {
		if err := s.client.DeleteMessage(settleCtx, s.uploadChatID, messageID); err != nil {
			s.logger.Warn("guest: could not remove a file's private-chat copy",
				"chat_id", s.uploadChatID, "message_id", messageID, "error", err)
		}
	}
}

func stripAttachMarkers(text string) string {
	return strings.TrimSpace(attachMarkerRE.ReplaceAllString(text, ""))
}
