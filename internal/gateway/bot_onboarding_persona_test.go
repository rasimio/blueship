package gateway

import (
	"encoding/json"
	"testing"
)

// The offer counter survives a round trip through the FSM row's jsonb
// data blob, and jsonb gives numbers back as float64. A plain int
// assertion returns the zero value instead — the count would restart at
// zero on every message and the persona offer would never fire, silently,
// with nothing in the logs and every write succeeding.
//
// The round trip is real rather than hand-built for that reason: asserting
// against a map literal containing int(3) would pass while production
// failed.
func TestTurnsFromDataSurvivesJSONRoundTrip(t *testing.T) {
	for _, want := range []int{0, 1, 5, 6, 41} {
		raw, err := json.Marshal(map[string]any{onbDataTurns: want})
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		if got := turnsFromData(data); got != want {
			t.Errorf("turnsFromData after round trip = %d, want %d", got, want)
		}
	}
}

func TestTurnsFromDataDefaultsToZero(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"nil map":     nil,
		"empty map":   {},
		"wrong type":  {onbDataTurns: "six"},
		"nil value":   {onbDataTurns: nil},
		"other stuff": {onbDataName: "Мира"},
	} {
		if got := turnsFromData(data); got != 0 {
			t.Errorf("%s: turnsFromData = %d, want 0", name, got)
		}
	}
}

// The edit marker decides whether the wizard's last step creates a soul
// or updates the one the user already has. Getting it wrong in the false
// direction mints a second tenant for an existing user.
func TestIsEditRunSurvivesJSONRoundTrip(t *testing.T) {
	raw, err := json.Marshal(map[string]any{onbDataEdit: true, onbDataName: "Мира"})
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if !isEditRun(data) {
		t.Error("isEditRun = false for a round-tripped edit run, want true")
	}
}

func TestIsEditRunDefaultsToCreate(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"nil map":      nil,
		"empty map":    {},
		"create run":   {onbDataName: "Мира", onbDataVoice: "warm"},
		"explicit off": {onbDataEdit: false},
		"wrong type":   {onbDataEdit: "yes"},
	} {
		if isEditRun(data) {
			t.Errorf("%s: isEditRun = true, want false — a create run must never take the update path", name)
		}
	}
}
