package core

import (
	"context"
	"encoding/json"
	"testing"
)

func TestToolReadOnlyPolicyIsExplicitAndSurvivesSubset(t *testing.T) {
	r := NewToolRegistry()
	r.Register("read", "read", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	if r.IsReadOnly("read") || r.IsReadOnly("missing") {
		t.Fatal("unknown effect treated as safe read")
	}
	if err := r.MarkReadOnly("read", "missing"); err == nil {
		t.Fatal("unknown tool accepted")
	}
	if r.IsReadOnly("read") {
		t.Fatal("failed declaration partially applied")
	}
	if err := r.MarkReadOnly("read"); err != nil {
		t.Fatal(err)
	}
	if !r.SubsetForNames([]string{"read"}).IsReadOnly("read") {
		t.Fatal("subset lost effect policy")
	}
	// A newly registered implementation does not inherit the old effect claim.
	r.Register("read", "replacement", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	if r.IsReadOnly("read") {
		t.Fatal("replacement inherited stale safety declaration")
	}
}
