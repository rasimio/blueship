package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// voiceUsers is the slice of the user store /voice touches.
type voiceUsers struct {
	bs.UserStore
	mu    sync.Mutex
	prefs map[string]bool
}

func (u *voiceUsers) GetByID(_ context.Context, id string) (*bs.UserProfile, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	raw, _ := json.Marshal(map[string]bool{"voice_enabled": u.prefs[id]})
	return &bs.UserProfile{ID: id, Preferences: raw}, nil
}

func (u *voiceUsers) SetPreference(_ context.Context, id, key string, value any) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if key != "voice_enabled" {
		return errors.New("unexpected preference " + key)
	}
	u.prefs[id] = value.(bool)
	return nil
}

type fakeTTS struct {
	mu     sync.Mutex
	inputs []string
	ctxErr []error
}

func (f *fakeTTS) Synthesize(ctx context.Context, text, _, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, text)
	f.ctxErr = append(f.ctxErr, ctx.Err())
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return []byte("ogg:" + text), nil
}

type voiceSink struct {
	mu     sync.Mutex
	voices []string
}

func (s *voiceSink) SendText(context.Context, string) error { return nil }
func (s *voiceSink) SendTyping(context.Context) error       { return nil }
func (s *voiceSink) SendVoice(_ context.Context, audio []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.voices = append(s.voices, string(audio))
	return nil
}

func voiceGateway(tts bs.TTSProvider, users bs.UserStore) *Gateway {
	cfg := &bs.Config{UI: bs.UIStrings{VoiceOn: "on", VoiceOff: "off", VoiceUnavailable: "unavailable"}}
	if tts != nil {
		cfg.TTS = tts
	}
	return &Gateway{
		deps:   &bs.Deps{Config: cfg, Users: users},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		users:  map[string]*UserState{},
	}
}

func TestVoiceCommandTogglesAndSetsThePersonsVoiceReplies(t *testing.T) {
	users := &voiceUsers{prefs: map[string]bool{}}
	g := voiceGateway(&fakeTTS{}, users)
	f := newFakeBotAPI(t)
	bi := &botInstance{tgUsername: "SoulBot", client: telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second)}
	us := &UserState{UserID: uuid.New(), ChatID: "telegram:31"}
	id := us.UserID.String()

	steps := []struct {
		text  string
		want  bool
		reply string
	}{
		{"/voice", true, "on"},
		{"/voice", false, "off"},
		{"/voice on", true, "on"},
		{"/voice on", true, "on"},
		{"/voice@SoulBot off", false, "off"},
	}
	for _, step := range steps {
		if !g.maybeRunVoiceCommand(context.Background(), bi, 31, us, step.text) {
			t.Fatalf("%q was not handled", step.text)
		}
		if users.prefs[id] != step.want {
			t.Fatalf("after %q voice_enabled = %v", step.text, users.prefs[id])
		}
		if got := f.last("sendMessage")["text"]; got != step.reply {
			t.Fatalf("after %q replied %v, want %q", step.text, got, step.reply)
		}
	}
	if g.maybeRunVoiceCommand(context.Background(), bi, 31, us, "/voice@OtherBot") {
		t.Fatal("a command addressed to another bot was taken")
	}
	if g.maybeRunVoiceCommand(context.Background(), bi, 31, us, "voice please") {
		t.Fatal("plain text was taken for the command")
	}
}

// Without a speech provider there is nothing to switch on; saying "on" would
// promise voice notes that never arrive.
func TestVoiceCommandWithoutASpeechProvider(t *testing.T) {
	users := &voiceUsers{prefs: map[string]bool{}}
	g := voiceGateway(nil, users)
	us := &UserState{UserID: uuid.New()}
	if got := g.setVoiceReplies(context.Background(), us, ""); got != "unavailable" {
		t.Fatalf("reply = %q", got)
	}
	if users.prefs[us.UserID.String()] {
		t.Fatal("the setting was switched on with no provider")
	}
}

// The voice note is sent after the turn returns, and returning ends the
// turn's context. Synthesis must not inherit that cancellation.
func TestVoiceNoteOutlivesTheTurnsContext(t *testing.T) {
	tts := &fakeTTS{}
	g := voiceGateway(tts, nil)
	sink := &voiceSink{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g.synthesizeAndSendVoice(ctx, sink, &UserState{}, "Привет. Как дела?")

	if len(tts.ctxErr) != 1 || tts.ctxErr[0] != nil {
		t.Fatalf("synthesis ran on a cancelled context: %v", tts.ctxErr)
	}
	if len(sink.voices) != 1 {
		t.Fatalf("voice notes sent = %d", len(sink.voices))
	}
}

// A long answer becomes several voice notes, in order, none over the
// provider's input limit.
func TestLongAnswerIsSpokenInPartsInOrder(t *testing.T) {
	tts := &fakeTTS{}
	g := voiceGateway(tts, nil)
	sink := &voiceSink{}
	sentence := strings.Repeat("слово ", 150) + "конец."
	text := strings.TrimSpace(strings.Repeat(sentence+" ", 8))

	g.synthesizeBatch(context.Background(), sink, text, "", "")

	if len(tts.inputs) < 2 {
		t.Fatalf("parts = %d, want the answer split", len(tts.inputs))
	}
	for i, in := range tts.inputs {
		if n := utf8.RuneCountInString(in); n > ttsMaxInputRunes {
			t.Fatalf("part %d is %d runes, over %d", i, n, ttsMaxInputRunes)
		}
		if sink.voices[i] != "ogg:"+in {
			t.Fatalf("voice note %d is out of order", i)
		}
	}
	if got := strings.Join(tts.inputs, " "); strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(text), " ") {
		t.Fatal("splitting lost or changed words")
	}
}

func TestSplitForSpeech(t *testing.T) {
	if got := splitForSpeech("  коротко.  ", 4000); len(got) != 1 || got[0] != "коротко." {
		t.Fatalf("short text = %q", got)
	}
	if got := splitForSpeech("   ", 4000); got != nil {
		t.Fatalf("blank text = %q", got)
	}
	word := strings.Repeat("я", 25)
	for _, part := range splitForSpeech(word+" "+word, 10) {
		if utf8.RuneCountInString(part) > 10 {
			t.Fatalf("part %q is over the limit", part)
		}
	}
}

// A voice note goes out through the bot the person is talking to, never
// through the host's one fixed sender.
func TestTelegramVoiceGoesThroughThePersonsBot(t *testing.T) {
	f := newFakeBotAPI(t)
	global := &recordingSender{}
	g := &Gateway{deps: &bs.Deps{Sender: global}}
	sink := g.newTelegramSink("telegram:31", &botInstance{client: telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second)})

	if err := sink.SendVoice(context.Background(), []byte("ogg")); err != nil {
		t.Fatal(err)
	}
	if m := f.methods(); len(m) != 1 || m[0] != "sendVoice" {
		t.Fatalf("bot calls = %v", m)
	}
	if global.voices != 0 {
		t.Fatal("the voice note went through the host's fixed sender")
	}
}

type recordingSender struct{ voices int }

func (s *recordingSender) SendMessage(context.Context, string, string) (int, error) { return 0, nil }
func (s *recordingSender) SendLong(context.Context, string, string) error           { return nil }
func (s *recordingSender) SendVoice(context.Context, string, []byte) error {
	s.voices++
	return nil
}
