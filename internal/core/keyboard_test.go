package core

import "testing"

// A key on this kind of keyboard has no callback: Telegram sends the
// label as though the person typed it. So a key mapped to nothing does
// not fail visibly — it drops its own label into the conversation, and
// the assistant answers «Подписка» as if it were a remark.
func TestKeyboardValidationCatchesKeysThatGoNowhere(t *testing.T) {
	commands := []BotCommand{{Name: "plus", Host: true}, {Name: "help", Prompt: "?"}}
	for _, tc := range []struct {
		name string
		kb   BotKeyboard
		ok   bool
	}{
		{"no keyboard at all is fine", BotKeyboard{}, true},
		{"a working keyboard", BotKeyboard{Rows: [][]BotKeyboardButton{
			{{Label: "Подписка", Command: "plus"}, {Label: "Что умеешь", Command: "help"}},
		}}, true},
		{"a command written with its slash", BotKeyboard{Rows: [][]BotKeyboardButton{
			{{Label: "Подписка", Command: "/plus"}},
		}}, true},
		{"a key running a command nobody configured", BotKeyboard{Rows: [][]BotKeyboardButton{
			{{Label: "Подписка", Command: "nosuch"}},
		}}, false},
		{"a key running nothing", BotKeyboard{Rows: [][]BotKeyboardButton{
			{{Label: "Подписка"}},
		}}, false},
		{"a key with no label", BotKeyboard{Rows: [][]BotKeyboardButton{
			{{Command: "plus"}},
		}}, false},
		// Two keys with one label are one key as far as the inbound text
		// can tell; the second is unreachable by construction.
		{"two keys with the same label", BotKeyboard{Rows: [][]BotKeyboardButton{
			{{Label: "Подписка", Command: "plus"}, {Label: "Подписка", Command: "help"}},
		}}, false},
		{"an empty row", BotKeyboard{Rows: [][]BotKeyboardButton{{}}}, false},
	} {
		if err := tc.kb.Valid(commands); (err == nil) != tc.ok {
			t.Errorf("%s: Valid() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// The mapping back from a tap must be exact. Somebody is free to type
// "подписка когда спишется" as a real question, and only the label
// Telegram sends verbatim may be rewritten into a command.
func TestKeyboardMapsOnlyTheExactLabel(t *testing.T) {
	kb := BotKeyboard{Rows: [][]BotKeyboardButton{
		{{Label: "Подписка", Command: "plus"}, {Label: "Что умеешь", Command: "help"}},
	}}
	for _, tc := range []struct {
		text string
		want string
	}{
		{"Подписка", "plus"},
		{"  Подписка  ", "plus"}, // Telegram does not add whitespace, but a person can
		{"Что умеешь", "help"},
		{"подписка", ""},                // a different string is a different message
		{"Подписка когда спишется", ""}, // a real question that starts with the label
		{"расскажи про Подписка", ""},   // and one that contains it
		{"", ""},
	} {
		got, ok := kb.CommandFor(tc.text)
		if tc.want == "" {
			if ok {
				t.Errorf("%q was rewritten to /%s — that was something the person said", tc.text, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("CommandFor(%q) = %q,%v — want %q", tc.text, got, ok, tc.want)
		}
	}
}
