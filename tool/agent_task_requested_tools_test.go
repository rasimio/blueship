package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func registryWithTools(names ...string) *bs.ToolRegistry {
	r := bs.NewToolRegistry()
	for _, name := range names {
		r.Register(name, "desc", json.RawMessage(`{"type":"object"}`),
			func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	}
	return r
}

// A tool allow-list is applied with SubsetForNames, which drops unknown names
// silently — so a family name like "sheets" narrows the worker's registry to
// nothing and the task burns its whole budget with no tools at all.
func TestValidateRequestedToolsRejectsUnknownNames(t *testing.T) {
	r := registryWithTools("sheets_read", "sheets_write", "attachment_read")

	err := validateRequestedTools(r, []string{"sheets", "attachments"})
	if err == nil {
		t.Fatal("an allow-list naming no existing tool must not create a task")
	}
	for _, want := range []string{"sheets", "attachments", "sheets_read", "attachment_read"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name the rejected input and the alternatives, missing %q: %v", want, err)
		}
	}
}

func TestValidateRequestedToolsRejectsPartiallyUnknownList(t *testing.T) {
	r := registryWithTools("sheets_read", "browser_fetch")

	err := validateRequestedTools(r, []string{"browser_fetch", "sheets"})
	if err == nil {
		t.Fatal("one unknown name still leaves the worker without the tool it was promised")
	}
	if strings.Contains(err.Error(), "no such tool: browser_fetch") {
		t.Fatalf("a known tool must not be reported as unknown: %v", err)
	}
}

func TestValidateRequestedToolsAcceptsKnownAndRoutedNames(t *testing.T) {
	r := registryWithTools("sheets_read", "browser_fetch")

	cases := [][]string{
		nil,
		{},
		{"sheets_read", "browser_fetch"},
		// peer: and mcp__ targets resolve per soul at dispatch time and are
		// never present in this registry.
		{"peer:liya", "mcp__github__list_repos", "sheets_read"},
	}
	for _, requested := range cases {
		if err := validateRequestedTools(r, requested); err != nil {
			t.Fatalf("validateRequestedTools(%v) = %v, want nil", requested, err)
		}
	}
}

func TestValidateRequestedToolsWithoutRegistryIsPermissive(t *testing.T) {
	if err := validateRequestedTools(nil, []string{"anything"}); err != nil {
		t.Fatalf("no registry to check against must not block creation: %v", err)
	}
}
