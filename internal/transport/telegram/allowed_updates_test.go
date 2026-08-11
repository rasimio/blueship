package telegram

import (
	"encoding/json"
	"strings"
	"testing"
)

// Telegram delivers only the update types the bot asks for, and says
// nothing about the rest. A type missing here is not an error anywhere:
// the bot just never hears about it.
//
// pre_checkout_query is the one that costs money. Telegram holds a
// confirmed purchase open for about ten seconds and cancels it if the
// bot does not answer — so leaving it out of this list means every
// payment fails on the buyer's screen, looking like a declined card.
func TestAllowedUpdatesCoversPayments(t *testing.T) {
	need := map[string]string{
		"message":            "no inbound chat at all",
		"callback_query":     "every inline button stops working",
		"pre_checkout_query": "every payment is cancelled with no reason shown to the buyer",
	}
	have := map[string]bool{}
	for _, u := range AllowedUpdates {
		have[u] = true
	}
	for u, consequence := range need {
		if !have[u] {
			t.Errorf("%s is not requested: %s", u, consequence)
		}
	}

	// The list is interpolated into a URL, so it has to survive as JSON.
	var parsed []string
	if err := json.Unmarshal([]byte(allowedUpdatesJSON()), &parsed); err != nil {
		t.Fatalf("allowed_updates is not valid JSON (%q): %v", allowedUpdatesJSON(), err)
	}
	if len(parsed) != len(AllowedUpdates) {
		t.Errorf("encoded %d types, have %d", len(parsed), len(AllowedUpdates))
	}
	if strings.Contains(allowedUpdatesJSON(), " ") {
		t.Error("the encoded list has spaces in it; unescaped, they break the query string")
	}
}
