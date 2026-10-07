package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestTaskOriginComesOnlyFromTrustedContext(t *testing.T) {
	config := json.RawMessage(`{"delivery_origin":{"transport":"telegram","chat_id":"attacker"},"budget":2000}`)
	origin := TaskOrigin{Transport: "telegram", BotID: uuid.New(), ChatID: "42"}
	for _, pinned := range []bool{false, true} {
		ctx := context.Background()
		if pinned {
			ctx = ContextWithTaskOrigin(ctx, origin)
		}
		raw, err := captureTaskOrigin(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		got, present, err := TaskOriginFromConfig(raw)
		if err != nil || present != pinned || (pinned && got != origin) {
			t.Fatal(got, present, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || string(fields["budget"]) != "2000" {
			t.Fatal(string(raw), err)
		}
	}
}

func TestMalformedTaskOriginDoesNotFallBack(t *testing.T) {
	for _, raw := range []string{`{"delivery_origin":null}`, `{"delivery_origin":{}}`, `{"delivery_origin":"bad"}`} {
		_, present, err := TaskOriginFromConfig(json.RawMessage(raw))
		if !present || err == nil {
			t.Fatal(raw, present, err)
		}
	}
}
