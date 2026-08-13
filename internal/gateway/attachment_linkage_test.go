package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

func TestInboundAttachmentLinksToExactCanonicalUserMessage(t *testing.T) {
	const parentBytes = "EXPANDED_PARENT_BASE64"
	const rawBytes = "RAW_INBOUND_FILE"

	userID := uuid.New()
	soulID := uuid.New()
	sessionID := uuid.New()
	messageID := uuid.New()
	store := &attachmentReceiptStore{nextID: messageID.String()}
	sink := newAttachmentLookupSink()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &bs.Config{
		Gateway: bs.GatewayConfig{MaxTurns: 1},
		Limits:  bs.LimitsConfig{MaxOutputTokens: 64},
	}
	provider := &attachmentReplyProvider{}
	loop := agent.NewLoop(provider, store, bs.NewToolRegistry(), nil, cfg, logger)
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, AttachmentSink: sink},
		logger: logger,
	}
	us := &UserState{UserID: userID, SoulID: soulID, ChatID: "test"}
	visible := "подпись https://example.com/file"
	pending := []pendingMsg{{
		visibleText: &visible,
		rawAttachments: []rawAttachment{{
			name: "photo.png",
			mime: "image/png",
			kind: "image",
			data: []byte(rawBytes),
		}},
	}}
	expanded := []bs.ContentBlock{
		{Type: "text", Text: "[reply to: expanded parent]\n\n" + visible},
		{Type: "image", Source: &bs.ImageSource{
			Type: "base64", MediaType: "image/png", Data: parentBytes,
		}},
	}
	runCfg := agent.RunConfig{
		SessionID:       sessionID.String(),
		SystemPrompt:    "system",
		Model:           "test",
		MaxTurns:        1,
		MessageBudget:   6000,
		VisibleUserText: &visible,
	}
	g.bindInboundEnvelopeArtifacts(&runCfg, us, sessionID, pending, visible)

	if _, _, _, err := g.runInteraction(
		context.Background(), loop, nil, runCfg, "", expanded, nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}

	if len(store.messages) == 0 || store.messages[0].Role != "user" {
		t.Fatalf("persisted messages = %#v", store.messages)
	}
	storedJSON, err := json.Marshal(store.messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedJSON), parentBytes) ||
		strings.Contains(string(storedJSON), rawBytes) ||
		strings.Contains(string(storedJSON), "expanded parent") {
		t.Fatalf("provider-only attachment payload leaked into durable content: %s", storedJSON)
	}
	if string(storedJSON) != `"`+visible+`"` {
		t.Fatalf("durable user content = %s, want canonical visible text", storedJSON)
	}

	requestJSON, err := json.Marshal(provider.requests[0].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(requestJSON), parentBytes) {
		t.Fatalf("provider request lost expanded attachment: %s", requestJSON)
	}

	if len(sink.saved) != 1 {
		t.Fatalf("saved attachments = %d, want 1", len(sink.saved))
	}
	saved := sink.saved[0]
	if saved.MessageID != messageID {
		t.Fatalf("attachment message_id = %s, want exact append id %s", saved.MessageID, messageID)
	}
	if saved.SessionID != sessionID || saved.UserID != userID || saved.SoulID != soulID {
		t.Fatalf("attachment scope = %#v", saved)
	}
	if string(saved.Data) != rawBytes {
		t.Fatalf("attachment bytes = %q", saved.Data)
	}
	if len(sink.links) != 1 || sink.links[0].MessageID != messageID {
		t.Fatalf("saved links = %#v, want exact message id %s", sink.links, messageID)
	}

	ids, err := sink.ListForMessage(context.Background(), userID, soulID, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != sink.savedIDs[0] {
		t.Fatalf("reply attachment lookup = %v, want %s", ids, sink.savedIDs[0])
	}
	replayed := g.attachmentBlocksByIDs(context.Background(), us, ids, "reply-attached")
	if len(replayed) != 1 || replayed[0].Type != "image" || replayed[0].Source == nil {
		t.Fatalf("reply expansion blocks = %#v", replayed)
	}
	if replayed[0].Source.Data != base64.StdEncoding.EncodeToString([]byte(rawBytes)) {
		t.Fatalf("reply expansion lost exact file bytes: %#v", replayed[0].Source)
	}
	foreign, err := sink.ListForMessage(context.Background(), userID, soulID, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 {
		t.Fatalf("unrelated message lookup leaked attachments: %v", foreign)
	}
}

func TestTelegramReplyResolvesAndReplaysParentAttachmentProviderOnly(t *testing.T) {
	const parentRaw = "TELEGRAM_PARENT_RAW"

	userID := uuid.New()
	soulID := uuid.New()
	sessionID := uuid.New()
	parentMessageID := uuid.New()
	childMessageID := uuid.New()
	sink := newAttachmentLookupSink()
	if _, err := sink.Save(context.Background(), bs.AttachmentParams{
		UserID:    userID,
		SoulID:    soulID,
		SessionID: sessionID,
		MessageID: parentMessageID,
		Name:      "parent.png",
		Mime:      "image/png",
		Kind:      "image",
		Data:      []byte(parentRaw),
	}); err != nil {
		t.Fatal(err)
	}
	lookup := &attachmentTGReplyLookup{parentID: parentMessageID.String()}
	visible := "что на этой картинке?"
	pending := []pendingMsg{{
		text:               visible,
		visibleText:        &visible,
		messageID:          101,
		replyToTGMessageID: 55,
	}}
	replyID, tgMessageIDs, err := resolveReplyMetadata(
		context.Background(), lookup, sessionID.String(), pending,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replyID != parentMessageID.String() || len(tgMessageIDs) != 1 || tgMessageIDs[0] != 101 {
		t.Fatalf("resolved reply = (%q, %v), want (%q, [101])", replyID, tgMessageIDs, parentMessageID)
	}
	if lookup.sessionID != sessionID.String() || lookup.tgMessageID != 55 {
		t.Fatalf("Telegram lookup scope = (%q, %d)", lookup.sessionID, lookup.tgMessageID)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &bs.Config{
		Gateway: bs.GatewayConfig{MaxTurns: 1},
		Limits:  bs.LimitsConfig{MaxOutputTokens: 64},
	}
	provider := &attachmentReplyProvider{}
	store := &attachmentReceiptStore{nextID: childMessageID.String()}
	loop := agent.NewLoop(provider, store, bs.NewToolRegistry(), nil, cfg, logger)
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, AttachmentSink: sink},
		logger: logger,
	}
	us := &UserState{UserID: userID, SoulID: soulID, ChatID: "telegram:test"}
	expanded := g.prependReplyAttachmentBlocks(
		context.Background(), us, replyID,
		[]bs.ContentBlock{{Type: "text", Text: visible}},
	)
	if len(expanded) != 2 || expanded[0].Type != "image" || expanded[1].Text != visible {
		t.Fatalf("Telegram reply expansion = %#v", expanded)
	}

	runCfg := agent.RunConfig{
		SessionID:        sessionID.String(),
		SystemPrompt:     "system",
		Model:            "test",
		MaxTurns:         1,
		MessageBudget:    6000,
		VisibleUserText:  &visible,
		ReplyToMessageID: replyID,
		TGMessageIDs:     tgMessageIDs,
	}
	g.bindInboundEnvelopeArtifacts(&runCfg, us, sessionID, pending, visible)
	if _, _, _, err := g.runInteraction(
		context.Background(), loop, nil, runCfg, "", providerContentFromBlocks(expanded), nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}

	storedJSON, err := json.Marshal(store.messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedJSON), parentRaw) ||
		strings.Contains(string(storedJSON), base64.StdEncoding.EncodeToString([]byte(parentRaw))) {
		t.Fatalf("reply-parent bytes leaked into durable child row: %s", storedJSON)
	}
	if store.messages[0].ReplyToMessageID != parentMessageID.String() ||
		len(store.messages[0].TGMessageIDs) != 1 || store.messages[0].TGMessageIDs[0] != 101 {
		t.Fatalf("durable reply metadata = %#v", store.messages[0])
	}
	requestJSON, err := json.Marshal(provider.requests[0].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(requestJSON), base64.StdEncoding.EncodeToString([]byte(parentRaw))) {
		t.Fatalf("provider prompt lost Telegram parent attachment: %s", requestJSON)
	}
}

type attachmentTGReplyLookup struct {
	parentID    string
	sessionID   string
	tgMessageID int64
}

func (l *attachmentTGReplyLookup) LookupByTGMessageID(_ context.Context, sessionID string, tgMessageID int64) (string, error) {
	l.sessionID = sessionID
	l.tgMessageID = tgMessageID
	return l.parentID, nil
}

type attachmentReplyProvider struct {
	requests []bs.CompletionRequest
}

func (p *attachmentReplyProvider) Complete(_ context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	p.requests = append(p.requests, req)
	return &bs.CompletionResponse{
		Content:    []bs.ContentBlock{{Type: "text", Text: "вижу"}},
		StopReason: "end_turn",
	}, nil
}

type attachmentReceiptStore struct {
	nextID   string
	messages []bs.Message
}

func (s *attachmentReceiptStore) Append(_ context.Context, _ string, msg bs.Message) error {
	s.messages = append(s.messages, msg)
	return nil
}

func (s *attachmentReceiptStore) AppendPersisted(_ context.Context, _ string, msg bs.Message) (bs.PersistedMessage, error) {
	s.messages = append(s.messages, msg)
	id := s.nextID
	s.nextID = uuid.NewString()
	return bs.PersistedMessage{ID: id, Role: msg.Role, CreatedAt: time.Now()}, nil
}

func (s *attachmentReceiptStore) AppendWithTokens(_ context.Context, _ string, msg bs.Message, _ int) error {
	s.messages = append(s.messages, msg)
	return nil
}

func (s *attachmentReceiptStore) MessagesForAPI(context.Context, string, int) ([]bs.Message, error) {
	return append([]bs.Message(nil), s.messages...), nil
}

func (s *attachmentReceiptStore) DialogMessagesForAPI(context.Context, string, int, bool) ([]bs.Message, error) {
	return append([]bs.Message(nil), s.messages...), nil
}

func (s *attachmentReceiptStore) RecentToolObservations(context.Context, string, time.Time, int) ([]bs.ToolObservation, error) {
	return nil, nil
}

func (s *attachmentReceiptStore) AllMessagesForAPI(context.Context, string) ([]bs.Message, error) {
	return append([]bs.Message(nil), s.messages...), nil
}

func (s *attachmentReceiptStore) CompactSession(context.Context, string, string, int) error {
	return nil
}

func (s *attachmentReceiptStore) CreateSession(context.Context, string, string) (string, error) {
	return "", nil
}

func (s *attachmentReceiptStore) CreateSessionWithSource(context.Context, string, string, string, string) (string, error) {
	return "", nil
}

func (s *attachmentReceiptStore) ArchiveSession(context.Context, string) error {
	return nil
}

func (s *attachmentReceiptStore) LatestAssistantMessageID(context.Context, string) (string, error) {
	return "", nil
}

func (s *attachmentReceiptStore) RecordLastInputTokens(context.Context, string, int) error {
	return nil
}

func (s *attachmentReceiptStore) RecordLLMUsage(context.Context, bs.LLMUsageRecord) error {
	return nil
}

type attachmentLookupSink struct {
	saved    []bs.AttachmentParams
	savedIDs []uuid.UUID
	links    []bs.LinkParams
	byScope  map[string][]uuid.UUID
	records  map[uuid.UUID]bs.AttachmentRecord
	data     map[uuid.UUID][]byte
}

func newAttachmentLookupSink() *attachmentLookupSink {
	return &attachmentLookupSink{
		byScope: make(map[string][]uuid.UUID),
		records: make(map[uuid.UUID]bs.AttachmentRecord),
		data:    make(map[uuid.UUID][]byte),
	}
}

func attachmentScopeKey(userID, soulID, messageID uuid.UUID) string {
	return userID.String() + ":" + soulID.String() + ":" + messageID.String()
}

func (s *attachmentLookupSink) Save(_ context.Context, p bs.AttachmentParams) (uuid.UUID, error) {
	id := uuid.New()
	s.saved = append(s.saved, p)
	s.savedIDs = append(s.savedIDs, id)
	key := attachmentScopeKey(p.UserID, p.SoulID, p.MessageID)
	s.byScope[key] = append(s.byScope[key], id)
	s.records[id] = bs.AttachmentRecord{
		ID:     id,
		UserID: p.UserID,
		SoulID: p.SoulID,
		Name:   p.Name,
		Mime:   p.Mime,
		Kind:   p.Kind,
		Size:   int64(len(p.Data)),
	}
	s.data[id] = append([]byte(nil), p.Data...)
	return id, nil
}

func (s *attachmentLookupSink) Get(_ context.Context, userID, soulID, id uuid.UUID) (*bs.AttachmentRecord, []byte, error) {
	rec, ok := s.records[id]
	if !ok || rec.UserID != userID || rec.SoulID != soulID {
		return nil, nil, nil
	}
	return &rec, append([]byte(nil), s.data[id]...), nil
}

func (s *attachmentLookupSink) ListForMessage(_ context.Context, userID, soulID, messageID uuid.UUID) ([]uuid.UUID, error) {
	return append([]uuid.UUID(nil), s.byScope[attachmentScopeKey(userID, soulID, messageID)]...), nil
}

func (s *attachmentLookupSink) SaveLink(_ context.Context, p bs.LinkParams) (uuid.UUID, error) {
	s.links = append(s.links, p)
	return uuid.New(), nil
}
