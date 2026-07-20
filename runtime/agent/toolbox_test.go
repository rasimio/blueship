package agent

import (
	"encoding/json"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func toolboxTestRegistry(names ...string) *bs.ToolRegistry {
	reg := bs.NewToolRegistry()
	for _, name := range names {
		reg.Register(name, "test tool "+name, json.RawMessage(`{"type":"object"}`), nil)
	}
	return reg
}

func TestWithToolboxToolAppendsOnlyWhenArmed(t *testing.T) {
	base := []bs.ToolDefinition{{Name: "notes"}}

	armed := withToolboxTool(RunConfig{
		ToolOverride:     []string{"notes"},
		ToolboxExpansion: []string{"notes", "browser_search"},
	}, base)
	if len(armed) != 2 || armed[1].Name != ToolboxToolName {
		t.Fatalf("expected open_toolbox appended, got %+v", armed)
	}

	// Empty override (pure_chat) still arms the hatch — that is the whole point.
	pureChat := withToolboxTool(RunConfig{
		ToolOverride:     []string{},
		ToolboxExpansion: []string{"browser_search"},
	}, nil)
	if len(pureChat) != 1 || pureChat[0].Name != ToolboxToolName {
		t.Fatalf("expected only open_toolbox on pure_chat turn, got %+v", pureChat)
	}

	// No override → role default already gave the full list; nothing to unlock.
	unarmed := withToolboxTool(RunConfig{ToolboxExpansion: []string{"browser_search"}}, base)
	if len(unarmed) != 1 {
		t.Fatalf("expected untouched tools without override, got %+v", unarmed)
	}

	// No expansion set → selector-less paths stay as they were.
	unarmed = withToolboxTool(RunConfig{ToolOverride: []string{"notes"}}, base)
	if len(unarmed) != 1 {
		t.Fatalf("expected untouched tools without expansion, got %+v", unarmed)
	}
}

func TestToolboxToolsAppliesAllowedToolsFilter(t *testing.T) {
	loop := &Loop{registry: toolboxTestRegistry("notes", "browser_search", "trello_boards")}

	tools := loop.toolboxTools(RunConfig{
		ToolOverride:     []string{"notes"},
		ToolboxExpansion: []string{"notes", "browser_search", "trello_boards"},
		AllowedTools:     []string{"notes", "browser_search"},
	})

	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	joined := strings.Join(names, ",")
	if joined != "notes,browser_search" {
		t.Fatalf("expected allowlist-filtered expansion, got %q", joined)
	}
}

func TestToolboxUnlockedResultListsTools(t *testing.T) {
	result := toolboxUnlockedResult([]bs.ToolDefinition{{Name: "notes"}, {Name: "browser_search"}})
	if !strings.Contains(result, "notes, browser_search") {
		t.Fatalf("expected tool names in result, got %q", result)
	}
}
