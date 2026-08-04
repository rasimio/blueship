package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
)

// The persona must trail the platform layers. It is the strongest position in
// the system prompt for the voice, and it keeps preamble+agents — identical for
// every soul — as a shared cacheable prefix rather than one that diverges a
// thousand characters in.
func TestSystemPromptPutsPersonaAfterPlatformLayers(t *testing.T) {
	soulID := uuid.New()
	cfg := &bs.Config{}
	cfg.Gateway.ResolveSoulPersona = func(context.Context, uuid.UUID) (string, error) {
		return "PERSONA-MARKER", nil
	}
	cfg.Gateway.ResolvePlatformPrompts = func(context.Context) (string, string, error) {
		return "PREAMBLE-MARKER", "AGENTS-MARKER", nil
	}
	g := &Gateway{deps: &bs.Deps{Config: cfg}}

	prompt, err := g.systemPromptForSoul(bs.WithSoulID(context.Background(), soulID), soulID)
	if err != nil {
		t.Fatalf("systemPromptForSoul: %v", err)
	}

	preamble := strings.Index(prompt, "PREAMBLE-MARKER")
	agents := strings.Index(prompt, "AGENTS-MARKER")
	persona := strings.Index(prompt, "PERSONA-MARKER")
	if preamble < 0 || agents < 0 || persona < 0 {
		t.Fatalf("a layer is missing from the assembled prompt: %q", prompt)
	}
	if !(preamble < agents && agents < persona) {
		t.Fatalf("layer order = preamble@%d agents@%d persona@%d, want persona last:\n%s",
			preamble, agents, persona, prompt)
	}
}
