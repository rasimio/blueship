package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRegistryExecuteHonorsTurnDenylist(t *testing.T) {
	called := false
	registry := NewToolRegistry()
	registry.Register("chat_recall", "recall", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) {
			called = true
			return "found", nil
		})

	ctx := WithDeniedTools(context.Background(), []string{"chat_recall"})
	result, isError := registry.Execute(ctx, "chat_recall", json.RawMessage(`{}`))
	if called {
		t.Fatal("denied tool handler executed")
	}
	if !isError || !strings.Contains(result, "denied") {
		t.Fatalf("result=%q isError=%t", result, isError)
	}
}
