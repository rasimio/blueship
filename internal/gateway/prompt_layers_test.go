package gateway

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/session"
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

// The clock must not lead the system prompt. It changes every minute, and the
// system prompt is the head of the cacheable prefix: a stamp there invalidates
// the tools, the dialog and everything behind it on every single message.
func TestSystemPromptCarriesNoClock(t *testing.T) {
	soulID := uuid.New()
	cfg := &bs.Config{}
	cfg.Gateway.ResolveSoulPersona = func(context.Context, uuid.UUID) (string, error) {
		return "PERSONA-MARKER", nil
	}
	cfg.Gateway.ResolvePlatformPrompts = func(context.Context) (string, string, error) {
		return "PREAMBLE-MARKER", "AGENTS-MARKER", nil
	}
	g := &Gateway{deps: &bs.Deps{Config: cfg}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), tz: time.UTC}

	prepared, err := g.prepareCortexTurn(
		bs.WithSoulID(context.Background(), soulID),
		&UserState{SoulID: soulID, Registry: bs.NewToolRegistry()},
		&session.Session{ID: uuid.NewString()},
		"", "", nil, true,
	)
	if err != nil {
		t.Fatalf("prepareCortexTurn: %v", err)
	}
	if strings.Contains(prepared.config.SystemPrompt, "current_datetime") {
		t.Fatalf("the clock is back in the system prompt: %q", prepared.config.SystemPrompt)
	}
	// It still has to reach the model — the loop renders it from TurnNow.
	if prepared.config.TurnNow.IsZero() {
		t.Fatal("TurnNow is unset, so the turn context has no clock to render")
	}
}
