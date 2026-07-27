package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestInvokeToolForUserAppliesPolicyAndIdentity(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	registry := bs.NewToolRegistry()
	var called bool
	registry.Register("lookup", "lookup", json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			called = true
			if bs.UserIDFromContext(ctx) != userID || bs.SoulIDFromContext(ctx) != soulID {
				t.Fatalf("runtime identity user=%s soul=%s", bs.UserIDFromContext(ctx), bs.SoulIDFromContext(ctx))
			}
			return map[string]any{"ok": true}, nil
		})
	g := directToolGateway(userID, soulID, registry, func(context.Context, uuid.UUID) (map[string]bool, []string, error) {
		return map[string]bool{"lookup": true}, nil, nil
	})

	got, err := g.InvokeToolForUser(context.Background(), userID, soulID, "lookup", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !called || got.Name != "lookup" || got.IsError || got.Output != `{"ok":true}` {
		t.Fatalf("invocation = %+v, called=%v", got, called)
	}
}

func TestInvokeToolForUserRejectsDisabledAndPolicyFailure(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	registry := bs.NewToolRegistry()
	calls := 0
	registry.Register("write", "write", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) {
			calls++
			return nil, nil
		})

	g := directToolGateway(userID, soulID, registry, func(context.Context, uuid.UUID) (map[string]bool, []string, error) {
		return map[string]bool{"write": false}, nil, nil
	})
	if _, err := g.InvokeToolForUser(context.Background(), userID, soulID, "write", json.RawMessage(`{}`)); !errors.Is(err, ErrToolDisabled) {
		t.Fatalf("disabled error = %v", err)
	}

	g.deps.Config.Gateway.ResolveSoulToolPolicy = func(context.Context, uuid.UUID) (map[string]bool, []string, error) {
		return nil, nil, errors.New("policy down")
	}
	if _, err := g.InvokeToolForUser(context.Background(), userID, soulID, "write", json.RawMessage(`{}`)); err == nil {
		t.Fatal("policy failure allowed direct execution")
	}
	if calls != 0 {
		t.Fatalf("handler called %d times", calls)
	}
}

func TestInvokeToolForUserRejectsUnknown(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	g := directToolGateway(userID, soulID, bs.NewToolRegistry(), nil)
	if _, err := g.InvokeToolForUser(context.Background(), userID, soulID, "missing", json.RawMessage(`{}`)); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestRefreshMCPForUserInvalidatesAndReloads(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	source := &recordingMCPSource{tools: []bs.MCPTool{{Name: "one"}, {Name: "two"}}}
	g := &Gateway{
		deps:   &bs.Deps{Config: &bs.Config{MCPSource: source}},
		logger: slog.Default(),
	}
	count, err := g.RefreshMCPForUser(context.Background(), userID, soulID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || source.invalidated != soulID || source.loaded != soulID {
		t.Fatalf("count=%d invalidated=%s loaded=%s", count, source.invalidated, source.loaded)
	}
}

func directToolGateway(
	userID, soulID uuid.UUID,
	registry *bs.ToolRegistry,
	policy func(context.Context, uuid.UUID) (map[string]bool, []string, error),
) *Gateway {
	cfg := &bs.Config{}
	cfg.Gateway.ResolveSoulToolPolicy = policy
	return &Gateway{
		deps:   &bs.Deps{Config: cfg},
		logger: slog.Default(),
		users: map[string]*UserState{
			platformUserCacheKey("tool", userID, soulID): {
				UserID: userID, SoulID: soulID, Registry: registry,
			},
		},
	}
}

type recordingMCPSource struct {
	tools       []bs.MCPTool
	invalidated uuid.UUID
	loaded      uuid.UUID
}

func (s *recordingMCPSource) ToolsForSoul(_ context.Context, soulID uuid.UUID) []bs.MCPTool {
	s.loaded = soulID
	return s.tools
}

func (s *recordingMCPSource) Invalidate(soulID uuid.UUID) {
	s.invalidated = soulID
}
