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
	cfg.Gateway.ResolvePlatformPrompts = func(context.Context, string) (string, string, error) {
		return "PREAMBLE-MARKER", "AGENTS-MARKER", nil
	}
	g := &Gateway{deps: &bs.Deps{Config: cfg}}

	prompt, err := g.systemPromptForSoul(bs.WithSoulID(context.Background(), soulID), soulID, "")
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

// The platform-layer cache is keyed by profile. It used to be a one-shot
// latch, which was correct while every soul read the same two files — but the
// profile comes from model_config, which is refreshed every turn, so a latch
// would serve whichever profile happened to win the first turn for the rest of
// the process. That is a silently-wrong prompt, the exact failure this
// mechanism exists to prevent, and it would look like a model regression.
func TestPlatformPromptCacheIsKeyedByProfile(t *testing.T) {
	soulID := uuid.New()
	cfg := &bs.Config{}
	cfg.Gateway.ResolveSoulPersona = func(context.Context, uuid.UUID) (string, error) {
		return "PERSONA-MARKER", nil
	}
	var calls []string
	cfg.Gateway.ResolvePlatformPrompts = func(_ context.Context, profile string) (string, string, error) {
		calls = append(calls, profile)
		return "PREAMBLE-" + profile, "AGENTS-" + profile, nil
	}
	g := &Gateway{deps: &bs.Deps{Config: cfg}}
	ctx := bs.WithSoulID(context.Background(), soulID)

	for _, profile := range []string{"", "lite", "", "lite"} {
		prompt, err := g.systemPromptForSoul(ctx, soulID, profile)
		if err != nil {
			t.Fatalf("systemPromptForSoul(%q): %v", profile, err)
		}
		if want := "AGENTS-" + profile; !strings.Contains(prompt, want) {
			t.Fatalf("profile %q got the wrong agents layer, want %q:\n%s", profile, want, prompt)
		}
	}

	// Each distinct profile resolved exactly once — still cached, just per key.
	if len(calls) != 2 || calls[0] != "" || calls[1] != "lite" {
		t.Fatalf("hook calls = %q, want one per distinct profile", calls)
	}
}

// A failed resolve must not be cached, or one transient error would pin the
// profile as broken for the process lifetime.
func TestPlatformPromptErrorsAreNotCached(t *testing.T) {
	cfg := &bs.Config{}
	fail := true
	cfg.Gateway.ResolvePlatformPrompts = func(context.Context, string) (string, string, error) {
		if fail {
			return "", "", context.DeadlineExceeded
		}
		return "PREAMBLE", "AGENTS", nil
	}
	g := &Gateway{deps: &bs.Deps{Config: cfg}}

	if _, _, err := g.platformPrompts(context.Background(), "lite"); err == nil {
		t.Fatal("want an error on the failing resolve")
	}
	fail = false
	preamble, agents, err := g.platformPrompts(context.Background(), "lite")
	if err != nil {
		t.Fatalf("retry after a transient failure: %v", err)
	}
	if preamble != "PREAMBLE" || agents != "AGENTS" {
		t.Fatalf("retry returned %q/%q", preamble, agents)
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
	cfg.Gateway.ResolvePlatformPrompts = func(context.Context, string) (string, string, error) {
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
