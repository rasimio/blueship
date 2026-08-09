package gateway

import (
	"log/slog"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func sourceGateway() (*Gateway, *botInstance) {
	g := &Gateway{deps: &bs.Deps{Config: &bs.Config{}}, logger: slog.Default()}
	return g, &botInstance{tgUsername: "TestBot"}
}

// The payload is the whole point of the exercise: without it a campaign
// cannot be told apart from organic traffic, and the ad spend is blind.
func TestSignupSourceExtractsDeeplinkPayload(t *testing.T) {
	g, bi := sourceGateway()
	for name, test := range map[string]struct{ in, want string }{
		"plain":            {"/start ads_memory_01", "ads_memory_01"},
		"addressed":        {"/start@TestBot ads_memory_01", "ads_memory_01"},
		"hyphens":          {"/start tg-ads-2026-08", "tg-ads-2026-08"},
		"digits only":      {"/start 12345", "12345"},
		"mixed case":       {"/start Seed_VC_List", "Seed_VC_List"},
		"extra spacing":    {"/start   ads_01  ", "ads_01"},
		"64 chars exactly": {"/start " + rep("a", 64), rep("a", 64)},
	} {
		if got := g.signupSource(bi, test.in); got != test.want {
			t.Errorf("%s: signupSource(%q) = %q, want %q", name, test.in, got, test.want)
		}
	}
}

// Everything that could not have come from a Telegram deep link has to
// read as "no source". This field is the dimension acquisition is
// reported by, so it only stays countable while it holds a small closed
// set — one row of arbitrary chat text in there and every group-by grows
// a garbage bucket.
func TestSignupSourceIgnoresAnythingNotFromADeeplink(t *testing.T) {
	g, bi := sourceGateway()
	for name, in := range map[string]string{
		"bare start":        "/start",
		"start with spaces": "/start    ",
		"another bot":       "/start@OtherBot ads_01",
		"not a command":     "ads_01",
		"different command": "/help ads_01",
		"empty":             "",
		"spaces in payload": "/start hello there",
		"cyrillic":          "/start реклама",
		"punctuation":       "/start ads_01!",
		"slash inside":      "/start ads/01",
		"over 64 chars":     "/start " + rep("a", 65),
	} {
		if got := g.signupSource(bi, in); got != "" {
			t.Errorf("%s: signupSource(%q) = %q, want empty", name, in, got)
		}
	}
}

// The wizard creates the account four steps after the /start that named
// the campaign, so the payload only survives if it round-trips through
// the FSM row — which is JSON, where a wrong type assertion is silent.
func TestSourceFromDataSurvivesTheWizard(t *testing.T) {
	if got := sourceFromData(map[string]any{onbDataSource: "ads_01"}); got != "ads_01" {
		t.Errorf("sourceFromData = %q, want %q", got, "ads_01")
	}
	for name, data := range map[string]map[string]any{
		"nil":        nil,
		"empty":      {},
		"other keys": {onbDataName: "Мира"},
		"wrong type": {onbDataSource: 42},
	} {
		if got := sourceFromData(data); got != "" {
			t.Errorf("%s: sourceFromData = %q, want empty", name, got)
		}
	}
}

func rep(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s[0])
	}
	return string(out)
}
