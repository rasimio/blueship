package gateway

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

func guestUI() bs.UIStrings {
	return bs.UIStrings{
		ExecutionDenied:  "closed",
		GuestPlaceholder: "…",
		GuestNoAnswer:    "no answer",
		GuestUnpairedFmt: "start here: %s",
		GuestMarkerFmt:   "[guest · %s]",
		GuestPrivateChat: "private chat",
	}
}

type guestRouting struct {
	userID, soulID uuid.UUID
	err            error
	asked          []int64
	decision       bs.ExecutionDecision
}

func guestGateway(t *testing.T, r *guestRouting) (*Gateway, *fakeBotAPI) {
	t.Helper()
	f := newFakeBotAPI(t)
	cfg := &bs.Config{UI: guestUI()}
	g := &Gateway{
		deps: &bs.Deps{
			Config: cfg,
			ResolveTelegramChat: func(_ context.Context, _ uuid.UUID, tgChatID int64) (uuid.UUID, uuid.UUID, error) {
				r.asked = append(r.asked, tgChatID)
				return r.userID, r.soulID, r.err
			},
			AuthorizeExecution: func(context.Context, bs.ExecutionRequest) (bs.ExecutionDecision, error) {
				return r.decision, nil
			},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		users:  map[string]*UserState{},
	}
	return g, f
}

func guestBot(f *fakeBotAPI, kind string, owner uuid.UUID) *botInstance {
	return &botInstance{
		id:          uuid.New(),
		kind:        kind,
		ownerUserID: owner,
		tgBotID:     900,
		tgUsername:  "SoulBot",
		client:      telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second),
	}
}

func summons(text string) *telegram.Message {
	return &telegram.Message{
		MessageID:    77,
		From:         &telegram.User{ID: 4242, FirstName: "Sofa", Username: "sofa"},
		Chat:         telegram.Chat{ID: -100500, Type: "supergroup", Title: "Family"},
		Text:         text,
		GuestQueryID: "gq-1",
	}
}

// The conversation a summons belongs to is the caller's own — the one bound
// to their private chat, whose id is their user id — never the chat the bot
// was called into.
func TestAdmitGuestResolvesTheCallerNotTheChat(t *testing.T) {
	owner := uuid.New()
	r := &guestRouting{userID: owner, soulID: uuid.New(), decision: bs.ExecutionDecision{Allowed: true}}
	g, f := guestGateway(t, r)

	userID, _, ok := g.admitGuest(context.Background(), guestBot(f, "platform", uuid.Nil), summons("@SoulBot hi"))
	if !ok || userID != owner {
		t.Fatalf("admitted = %v user = %v", ok, userID)
	}
	if len(r.asked) != 1 || r.asked[0] != 4242 {
		t.Fatalf("resolved chats = %v, want the caller's id", r.asked)
	}
	if m := f.methods(); len(m) != 0 {
		t.Fatalf("an admitted summons is answered by the turn, not here: %v", m)
	}
}

func TestAdmitGuestTellsAStrangerOnThePlatformBotHowToStart(t *testing.T) {
	r := &guestRouting{err: bs.ErrTelegramChatUnpaired}
	g, f := guestGateway(t, r)

	if _, _, ok := g.admitGuest(context.Background(), guestBot(f, "platform", uuid.Nil), summons("@SoulBot hi")); ok {
		t.Fatal("a stranger must not get a turn")
	}
	body := f.last("answerGuestQuery")
	if body == nil {
		t.Fatalf("no answer to the stranger; calls = %v", f.methods())
	}
	if body["guest_query_id"] != "gq-1" {
		t.Fatalf("answered query %v", body["guest_query_id"])
	}
	text := body["result"].(map[string]any)["input_message_content"].(map[string]any)["message_text"].(string)
	if !strings.Contains(text, "t.me/SoulBot") {
		t.Fatalf("answer %q does not link to the bot", text)
	}
}

// A user's own bot answers only its owner — a stranger and a person the
// host happens to have linked are both ignored, and ignoring means saying
// nothing in the chat.
func TestAdmitGuestOnAUserBotAnswersOnlyItsOwner(t *testing.T) {
	owner := uuid.New()
	cases := map[string]*guestRouting{
		"stranger":  {err: bs.ErrTelegramChatUnpaired},
		"not owner": {userID: uuid.New(), soulID: uuid.New(), decision: bs.ExecutionDecision{Allowed: true}},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			g, f := guestGateway(t, r)
			if _, _, ok := g.admitGuest(context.Background(), guestBot(f, "user", owner), summons("@SoulBot hi")); ok {
				t.Fatal("admitted")
			}
			if m := f.methods(); len(m) != 0 {
				t.Fatalf("an ignored summons must leave no trace in the chat: %v", m)
			}
		})
	}

	r := &guestRouting{userID: owner, soulID: uuid.New(), decision: bs.ExecutionDecision{Allowed: true}}
	g, f := guestGateway(t, r)
	if _, _, ok := g.admitGuest(context.Background(), guestBot(f, "user", owner), summons("@SoulBot hi")); !ok {
		t.Fatal("the owner was not admitted on their own bot")
	}
}

func TestAdmitGuestAnswersARefusalWithTheHostsMessage(t *testing.T) {
	r := &guestRouting{userID: uuid.New(), soulID: uuid.New(),
		decision: bs.ExecutionDecision{Allowed: false, Message: "limit reached"}}
	g, f := guestGateway(t, r)
	if _, _, ok := g.admitGuest(context.Background(), guestBot(f, "platform", uuid.Nil), summons("@SoulBot hi")); ok {
		t.Fatal("a refused turn was admitted")
	}
	body := f.last("answerGuestQuery")
	if body == nil {
		t.Fatal("the refusal was not answered")
	}
	content := body["result"].(map[string]any)["input_message_content"].(map[string]any)
	if content["message_text"] != "limit reached" {
		t.Fatalf("answer = %v", content["message_text"])
	}
	if _, ok := body["result"].(map[string]any)["reply_markup"]; ok {
		t.Fatal("callback buttons on a guest answer have no chat to act in")
	}
}

func TestGuestPendingMsgMarksTheHistoryAndKeepsTheChatOut(t *testing.T) {
	g, f := guestGateway(t, &guestRouting{})
	bi := guestBot(f, "platform", uuid.Nil)
	msg := summons("@soulbot  what do you   think?")
	msg.ReplyToMessage = &telegram.Message{
		MessageID: 76,
		From:      &telegram.User{ID: 31, FirstName: "Kate"},
		Text:      "let's move the trip to May",
	}

	p := g.guestPendingMsg(context.Background(), bi, msg)
	if p.text != "what do you think?" {
		t.Fatalf("text = %q", p.text)
	}
	if p.visibleText == nil || *p.visibleText != "[guest · Family] what do you think?" {
		t.Fatalf("history text = %v", p.visibleText)
	}
	if !strings.Contains(p.transportNote, "Sofa (@sofa)") || !strings.Contains(p.transportNote, `"Family"`) {
		t.Fatalf("note does not say who called and where: %q", p.transportNote)
	}
	if p.replyQuoteFallback != "Kate: let's move the trip to May" {
		t.Fatalf("quote = %q", p.replyQuoteFallback)
	}
	if p.messageID != 0 || p.replyToTGMessageID != 0 {
		t.Fatal("message ids from the guest chat must not be indexed in the caller's conversation")
	}
}

func TestGuestPendingMsgOnABareMentionInAPrivateChat(t *testing.T) {
	g, f := guestGateway(t, &guestRouting{})
	bi := guestBot(f, "platform", uuid.Nil)
	msg := summons("@SoulBot")
	msg.Chat = telegram.Chat{ID: 31, Type: "private"}
	msg.ReplyToMessage = &telegram.Message{From: &telegram.User{ID: 900}, Text: "earlier answer"}

	p := g.guestPendingMsg(context.Background(), bi, msg)
	if p.text != "" || *p.visibleText != "[guest · private chat]" {
		t.Fatalf("text = %q history = %q", p.text, *p.visibleText)
	}
	if p.replyQuoteFallback != "your earlier answer in this chat: earlier answer" {
		t.Fatalf("quote = %q", p.replyQuoteFallback)
	}
}

// The note is the frame for everything else in the turn, so it comes first —
// ahead of the reply quote as well as the message.
func TestTransportNoteLeadsTheTurn(t *testing.T) {
	msgs := []pendingMsg{{transportNote: "[guest call] note"}}
	blocks := []bs.ContentBlock{
		{Type: "text", Text: "[reply to: quote]"},
		{Type: "text", Text: "question"},
	}
	got := prependTransportNotes(msgs, blocks)
	if len(got) != 3 || got[0].Text != "[guest call] note" || got[1].Text != "[reply to: quote]" {
		t.Fatalf("blocks = %+v", got)
	}
	if out := prependTransportNotes([]pendingMsg{{}}, blocks); len(out) != len(blocks) {
		t.Fatal("a message without a note gained a block")
	}
}

type fakeGuestClient struct {
	mu      sync.Mutex
	answers []string
	edits   []string
	finals  []string
	media   [][]telegram.InlineMedia
	uploads []int64 // chats files were uploaded through
	deleted []int
	// log is every call in order, so a test can say what happened before what.
	log []string
}

func (c *fakeGuestClient) AnswerGuestQuery(_ context.Context, _ string, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers = append(c.answers, text)
	c.log = append(c.log, "answer")
	return "inline-1", nil
}

func (c *fakeGuestClient) EditInlineMessageText(_ context.Context, _ string, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edits = append(c.edits, text)
	c.log = append(c.log, "edit")
	return nil
}

func (c *fakeGuestClient) FinalizeInlineWithMedia(_ context.Context, _ string, text string, media []telegram.InlineMedia) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finals = append(c.finals, text)
	c.media = append(c.media, media)
	c.log = append(c.log, "final")
	return nil
}

func (c *fakeGuestClient) UploadForInline(_ context.Context, chatID int64, kind, _, _ string, _ []byte) (telegram.InlineMedia, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uploads = append(c.uploads, chatID)
	c.log = append(c.log, "upload")
	return telegram.InlineMedia{Kind: kind, FileID: "file-" + kind}, 900 + len(c.uploads), nil
}

func (c *fakeGuestClient) DeleteMessage(_ context.Context, _ int64, messageID int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, messageID)
	c.log = append(c.log, "delete")
	return nil
}

func newTestGuestSink(c *fakeGuestClient) *guestSink {
	return &guestSink{client: c, queryID: "gq", noAnswer: "no answer", uploadChatID: 4242,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestGuestSinkAnswersOnceAndEditsFromThenOn(t *testing.T) {
	c := &fakeGuestClient{}
	s := newTestGuestSink(c)
	if err := s.open(context.Background(), "…"); err != nil {
		t.Fatal(err)
	}
	// Right after the placeholder: throttled, nothing edited yet.
	_ = s.SendTextDelta(context.Background(), "Hello there")
	if len(c.edits) != 0 {
		t.Fatalf("edited inside the throttle window: %v", c.edits)
	}
	s.lastEdit = time.Now().Add(-2 * guestEditInterval)
	_ = s.SendTextDelta(context.Background(), ", friends")
	if len(c.edits) != 1 || c.edits[0] != "Hello there, friends" {
		t.Fatalf("edits = %v", c.edits)
	}

	if err := s.SendFinalText(context.Background(), "Hello there, friends. [attached: 0b7c0c8e-8f7e-4b0e-9d7e-2f6b1d1c2a3b]"); err != nil {
		t.Fatal(err)
	}
	s.settle(context.Background())
	if len(c.answers) != 1 {
		t.Fatalf("answered %d times; a summons allows one", len(c.answers))
	}
	if len(c.finals) != 1 || c.finals[0] != "Hello there, friends." {
		t.Fatalf("finals = %v (file markers must not reach the chat)", c.finals)
	}
}

func TestGuestSinkSettlesATurnThatDeliveredNothing(t *testing.T) {
	c := &fakeGuestClient{}
	s := newTestGuestSink(c)
	_ = s.open(context.Background(), "…")
	s.settle(context.Background())
	if len(c.finals) != 1 || c.finals[0] != "no answer" {
		t.Fatalf("finals = %v", c.finals)
	}

	c = &fakeGuestClient{}
	s = newTestGuestSink(c)
	_ = s.open(context.Background(), "…")
	_ = s.SendTextDelta(context.Background(), "half an answer")
	s.settle(context.Background())
	if len(c.finals) != 1 || c.finals[0] != "half an answer" {
		t.Fatalf("what streamed must stay: finals = %v", c.finals)
	}
}

// A picture the turn made has to reach the chat it was asked for in. The only
// way into an inline message is a file id, got by uploading through the
// caller's private chat; that copy goes once the answer holds the file, and
// not before.
func TestGuestSinkPutsTheTurnsFilesIntoTheAnswer(t *testing.T) {
	c := &fakeGuestClient{}
	s := newTestGuestSink(c)
	_ = s.open(context.Background(), "…")

	if _, err := s.SendAttachment(context.Background(),
		bs.AttachmentRecord{Name: "art.png", Mime: "image/png", Kind: "image"}, []byte("png")); err != nil {
		t.Fatal(err)
	}
	if len(c.uploads) != 1 || c.uploads[0] != 4242 {
		t.Fatalf("uploaded through %v, want the caller's private chat", c.uploads)
	}
	if err := s.SendFinalText(context.Background(), "here it is"); err != nil {
		t.Fatal(err)
	}
	s.settle(context.Background())

	if len(c.finals) != 1 || c.finals[0] != "here it is" {
		t.Fatalf("finals = %v", c.finals)
	}
	if len(c.media[0]) != 1 || c.media[0][0] != (telegram.InlineMedia{Kind: "photo", FileID: "file-photo"}) {
		t.Fatalf("media = %+v, want the picture as a photo", c.media[0])
	}
	if len(c.deleted) != 1 || c.deleted[0] != 901 {
		t.Fatalf("deleted = %v, want the private-chat copy removed", c.deleted)
	}
	if got := strings.Join(c.log, ","); got != "answer,upload,final,delete" {
		t.Fatalf("calls = %s: the copy may only go once the answer holds the file", got)
	}
}

// A picture with no words is still an answer.
func TestGuestSinkDeliversAFileWithoutText(t *testing.T) {
	c := &fakeGuestClient{}
	s := newTestGuestSink(c)
	_ = s.open(context.Background(), "…")
	_, _ = s.SendAttachment(context.Background(), bs.AttachmentRecord{Name: "report.md", Kind: "text"}, []byte("# r"))
	_ = s.SendFinalText(context.Background(), "[attached: 0b7c0c8e-8f7e-4b0e-9d7e-2f6b1d1c2a3b]")
	s.settle(context.Background())
	if len(c.finals) != 1 || c.finals[0] != "" || c.media[0][0].Kind != "document" {
		t.Fatalf("finals = %q media = %+v", c.finals, c.media)
	}
}

func TestStripBotMention(t *testing.T) {
	cases := map[string]string{
		"@SoulBot translate this":     "translate this",
		"translate this @soulbot":     "translate this",
		"hey  @SOULBOT   look":        "hey look",
		"@SoulBotter is someone else": "@SoulBotter is someone else",
		"@SoulBot":                    "",
	}
	for in, want := range cases {
		if got := stripBotMention(in, "SoulBot"); got != want {
			t.Errorf("stripBotMention(%q) = %q, want %q", in, got, want)
		}
	}
}
