package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type staticToolTransport struct {
	defs   []ToolDef
	closed bool
}

func (t *staticToolTransport) call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	if method != "tools/list" {
		return nil, fmt.Errorf("unexpected method %q", method)
	}
	return json.Marshal(listToolsResult{Tools: t.defs})
}

func (*staticToolTransport) notify(context.Context, string, any) error { return nil }

func (t *staticToolTransport) close() error {
	t.closed = true
	return nil
}

type recordingMCPServerStore struct {
	mu             sync.Mutex
	servers        []ServerRow
	errors         []string
	errorServerIDs []uuid.UUID
	syncedCalls    int
	catalogCalls   int
}

func (s *recordingMCPServerStore) ServersForSoul(context.Context, uuid.UUID) ([]ServerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ServerRow(nil), s.servers...), nil
}

func (*recordingMCPServerStore) ServersSignature(context.Context, uuid.UUID) string { return "" }

func (s *recordingMCPServerStore) MarkSynced(context.Context, uuid.UUID, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncedCalls++
}

func (s *recordingMCPServerStore) MarkError(_ context.Context, serverID uuid.UUID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, message)
	s.errorServerIDs = append(s.errorServerIDs, serverID)
}

func (s *recordingMCPServerStore) UpsertCatalogTools(context.Context, uuid.UUID, string, []ToolDef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catalogCalls++
}

func TestNamespacedNameLongNamesUseStableHashSuffix(t *testing.T) {
	if got, want := NamespacedName("Git-Hub", "List Issues"), "mcp__git_hub__list_issues"; got != want {
		t.Fatalf("short name = %q, want unchanged %q", got, want)
	}

	common := strings.Repeat("a", 80)
	first := NamespacedName("server", common+"x")
	second := NamespacedName("server", common+"y")
	if len(first) != 64 || len(second) != 64 {
		t.Fatalf("long names have lengths %d/%d, want 64/64", len(first), len(second))
	}
	if first == second {
		t.Fatalf("names differing after the old truncation boundary collided: %q", first)
	}
	if repeat := NamespacedName("server", common+"x"); repeat != first {
		t.Fatalf("long name is not deterministic: %q then %q", first, repeat)
	}
	full := "mcp__" + sanitize("server") + "__" + sanitize(common+"x")
	digest := sha256.Sum256([]byte(full))
	wantSuffix := fmt.Sprintf("_%x", digest[:6])
	if !strings.HasSuffix(first, wantSuffix) {
		t.Fatalf("long name = %q, want SHA-256 suffix %q", first, wantSuffix)
	}
}

func TestConnectServerRejectsResolvedToolNameCollisionFailClosed(t *testing.T) {
	transport := &staticToolTransport{defs: []ToolDef{
		{Name: "a-b", InputSchema: json.RawMessage(`{}`)},
		{Name: "a_b", InputSchema: json.RawMessage(`{}`)},
	}}
	store := &recordingMCPServerStore{}
	manager := NewManager(store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.dialServer = func(context.Context, ServerRow, string) (*Client, error) {
		return &Client{t: transport}, nil
	}

	result := manager.connectServer(context.Background(), ServerRow{ID: uuid.New(), Name: "demo"})
	if result.client != nil || len(result.tools) != 0 {
		t.Fatalf("colliding server was partially admitted: %#v", result)
	}
	if !transport.closed {
		t.Fatal("colliding server connection was not closed")
	}
	if len(store.errors) != 1 || !strings.Contains(store.errors[0], "tool name collision") {
		t.Fatalf("MarkError calls = %#v, want one collision error", store.errors)
	}
	if store.syncedCalls != 0 || store.catalogCalls != 0 {
		t.Fatalf("colliding server was published: synced=%d catalog=%d", store.syncedCalls, store.catalogCalls)
	}
}

func TestConnectSoulRejectsCrossServerToolNameCollisionFailClosed(t *testing.T) {
	first := ServerRow{ID: uuid.New(), SoulID: uuid.New(), Name: "a-b"}
	second := ServerRow{ID: uuid.New(), SoulID: first.SoulID, Name: "a_b"}
	third := ServerRow{ID: uuid.New(), SoulID: first.SoulID, Name: "other"}
	store := &recordingMCPServerStore{servers: []ServerRow{first, second, third}}
	transports := map[uuid.UUID]*staticToolTransport{
		first.ID:  {defs: []ToolDef{{Name: "same_tool", InputSchema: json.RawMessage(`{}`)}}},
		second.ID: {defs: []ToolDef{{Name: "same_tool", InputSchema: json.RawMessage(`{}`)}}},
		third.ID:  {defs: []ToolDef{{Name: "unique_tool", InputSchema: json.RawMessage(`{}`)}}},
	}
	manager := NewManager(store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.dialServer = func(_ context.Context, server ServerRow, _ string) (*Client, error) {
		return &Client{t: transports[server.ID]}, nil
	}
	soul := &soulConns{}

	manager.connectSoul(first.SoulID, soul, "sig")
	soul.mu.Lock()
	clientCount := len(soul.clients)
	toolCount := len(soul.tools)
	soul.mu.Unlock()
	if clientCount != 1 || toolCount != 1 {
		t.Fatalf("conflicting servers were not isolated: clients=%d tools=%d, want unrelated server only", clientCount, toolCount)
	}
	if !transports[first.ID].closed || !transports[second.ID].closed {
		t.Fatalf("conflicting clients were not both closed: first=%v second=%v", transports[first.ID].closed, transports[second.ID].closed)
	}
	if transports[third.ID].closed {
		t.Fatal("unrelated server was closed with the conflicting servers")
	}
	if len(store.errors) != 2 {
		t.Fatalf("MarkError calls = %#v, want one per conflicting server", store.errors)
	}
	marked := map[uuid.UUID]bool{}
	for i, serverID := range store.errorServerIDs {
		marked[serverID] = true
		if !strings.Contains(store.errors[i], "collision across MCP servers") {
			t.Fatalf("MarkError[%d] = %q", i, store.errors[i])
		}
	}
	if !marked[first.ID] || !marked[second.ID] {
		t.Fatalf("error statuses missed a conflicting server: %#v", store.errorServerIDs)
	}
	if store.syncedCalls != 1 || store.catalogCalls != 1 {
		t.Fatalf("catalog publication = synced:%d catalog:%d, want unrelated server only", store.syncedCalls, store.catalogCalls)
	}
}
