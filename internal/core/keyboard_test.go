package core

import "testing"

func screen(text string, keys ...BotKeyboardKey) BotKeyboardNode {
	return BotKeyboardNode{Text: text, Rows: [][]BotKeyboardKey{keys}}
}

func childScreen(text, parent string, keys ...BotKeyboardKey) BotKeyboardNode {
	n := screen(text, keys...)
	n.Parent = parent
	return n
}

func workingKeyboard() BotKeyboard {
	return BotKeyboard{
		Root: "main", BackLabel: "‹ Назад", CloseLabel: "Закрыть", Closed: "Закрыла.",
		Nodes: map[string]BotKeyboardNode{
			"main": screen("Меню",
				BotKeyboardKey{Label: "Умения", Node: "skills"},
				BotKeyboardKey{Label: "Подписка", Command: "plus"}),
			"skills": childScreen("Что попробовать", "main",
				BotKeyboardKey{Label: "Нарисуй картинку", Say: "Нарисуй кота в скафандре, акварелью."}),
		},
	}
}

var testCommands = []BotCommand{{Name: "plus", Host: true}}

// A key on this kind of keyboard has no callback: Telegram sends the
// label as though the person typed it. So the label is the wire format,
// and everything below follows from that.
func TestKeyboardResolvesEachKindOfKey(t *testing.T) {
	kb := workingKeyboard()
	for _, tc := range []struct {
		name string
		text string
		want KeyAction
		ok   bool
	}{
		{"a key that opens a screen", "Умения", KeyAction{Node: "skills"}, true},
		{"a key that runs a command", "Подписка", KeyAction{Text: "/plus"}, true},
		{"a key that stands for a sentence", "Нарисуй картинку",
			KeyAction{Text: "Нарисуй кота в скафандре, акварелью."}, true},
		{"back", "‹ Назад", KeyAction{}, true},
		{"close", "Закрыть", KeyAction{Close: true}, true},
		// Everything else is something the person said.
		{"a real question containing a label", "Подписка когда спишется", KeyAction{}, false},
		{"a different case", "подписка", KeyAction{}, false},
		{"empty", "", KeyAction{}, false},
	} {
		got, ok := kb.Action(tc.text)
		if ok != tc.ok {
			t.Errorf("%s: Action(%q) matched=%v, want %v", tc.name, tc.text, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%s: Action(%q) = %+v, want %+v", tc.name, tc.text, got, tc.want)
		}
	}
}

func TestKeyboardValidationCatchesKeysThatGoNowhere(t *testing.T) {
	broken := func(mutate func(*BotKeyboard)) BotKeyboard {
		kb := workingKeyboard()
		mutate(&kb)
		return kb
	}
	for _, tc := range []struct {
		name string
		kb   BotKeyboard
		ok   bool
	}{
		{"no keyboard at all is fine", BotKeyboard{}, true},
		{"a working keyboard", workingKeyboard(), true},
		{"a root that does not exist", broken(func(k *BotKeyboard) { k.Root = "nope" }), false},
		{"no way to close it", broken(func(k *BotKeyboard) { k.CloseLabel = "" }), false},
		{"a child screen with no back label", broken(func(k *BotKeyboard) { k.BackLabel = "" }), false},
		{"a key opening a screen that does not exist", broken(func(k *BotKeyboard) {
			k.Nodes["main"] = screen("Меню", BotKeyboardKey{Label: "Умения", Node: "gone"})
		}), false},
		{"a key running a command nobody configured", broken(func(k *BotKeyboard) {
			k.Nodes["main"] = screen("Меню", BotKeyboardKey{Label: "Подписка", Command: "nosuch"})
		}), false},
		{"a key doing nothing", broken(func(k *BotKeyboard) {
			k.Nodes["main"] = screen("Меню", BotKeyboardKey{Label: "Подписка"})
		}), false},
		{"a key doing two things", broken(func(k *BotKeyboard) {
			k.Nodes["main"] = screen("Меню", BotKeyboardKey{Label: "Подписка", Command: "plus", Say: "х"})
		}), false},
		{"a screen with no text to open with", broken(func(k *BotKeyboard) {
			k.Nodes["main"] = screen("", BotKeyboardKey{Label: "Подписка", Command: "plus"})
		}), false},
		{"a screen with no keys", broken(func(k *BotKeyboard) {
			k.Nodes["main"] = BotKeyboardNode{Text: "Меню"}
		}), false},
		// The label is sent verbatim, so these two break the wire.
		{"two keys with the same label", broken(func(k *BotKeyboard) {
			k.Nodes["main"] = screen("Меню",
				BotKeyboardKey{Label: "Одно", Command: "plus"},
				BotKeyboardKey{Label: "Одно", Node: "skills"})
		}), false},
		{"a key that collides with the close control", broken(func(k *BotKeyboard) {
			k.Nodes["main"] = screen("Меню", BotKeyboardKey{Label: "Закрыть", Command: "plus"})
		}), false},
		{"a key labelled as a slash command", broken(func(k *BotKeyboard) {
			k.Nodes["main"] = screen("Меню", BotKeyboardKey{Label: "/plus", Command: "plus"})
		}), false},
	} {
		if err := tc.kb.Valid(testCommands); (err == nil) != tc.ok {
			t.Errorf("%s: Valid() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
