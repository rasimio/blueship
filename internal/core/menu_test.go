package core

import "testing"

// A menu is validated at startup because its failures are invisible
// until somebody taps: a button leading to a screen that does not exist
// is a dead end in front of a person, and nothing about it looks wrong
// in the config.
func TestMenuValidationCatchesTheDeadEnds(t *testing.T) {
	commands := []BotCommand{{Name: "plus", Host: true}, {Name: "b"}}
	for _, tc := range []struct {
		name string
		menu BotMenu
		ok   bool
	}{
		{"no menu at all is fine", BotMenu{}, true},
		{"a working two-level menu", BotMenu{
			Root: "main",
			Nodes: map[string]BotMenuNode{
				"main": {Text: "Меню", Items: []BotMenuItem{{Label: "Оплата", Node: "pay"}}},
				"pay":  {Text: "Оплата", Parent: "main", Items: []BotMenuItem{{Label: "Купить", Command: "plus"}}},
			},
		}, true},
		{"root that does not exist", BotMenu{
			Root:  "main",
			Nodes: map[string]BotMenuNode{"other": {Text: "x", Items: []BotMenuItem{{Label: "a", Command: "b"}}}},
		}, false},
		{"a button leading nowhere", BotMenu{
			Root: "main",
			Nodes: map[string]BotMenuNode{
				"main": {Text: "Меню", Items: []BotMenuItem{{Label: "Оплата", Node: "missing"}}},
			},
		}, false},
		{"a parent that does not exist", BotMenu{
			Root: "main",
			Nodes: map[string]BotMenuNode{
				"main": {Text: "Меню", Items: []BotMenuItem{{Label: "a", Command: "b"}}},
				"lost": {Text: "x", Parent: "gone", Items: []BotMenuItem{{Label: "a", Command: "b"}}},
			},
		}, false},
		// Telegram rejects the whole keyboard over one empty label, so
		// the menu simply never appears.
		{"a button with no label", BotMenu{
			Root: "main",
			Nodes: map[string]BotMenuNode{
				"main": {Text: "Меню", Items: []BotMenuItem{{Command: "plus"}}},
			},
		}, false},
		// A button naming a command nobody configured is a button that
		// does nothing at all when tapped.
		{"a button running a command that does not exist", BotMenu{
			Root: "main",
			Nodes: map[string]BotMenuNode{
				"main": {Text: "Меню", Items: []BotMenuItem{{Label: "Оплата", Command: "nosuch"}}},
			},
		}, false},
		{"a command written with its slash", BotMenu{
			Root: "main",
			Nodes: map[string]BotMenuNode{
				"main": {Text: "Меню", Items: []BotMenuItem{{Label: "Оплата", Command: "/plus"}}},
			},
		}, true},
		{"a button that does nothing", BotMenu{
			Root: "main",
			Nodes: map[string]BotMenuNode{
				"main": {Text: "Меню", Items: []BotMenuItem{{Label: "Оплата"}}},
			},
		}, false},
		{"a button that does two things", BotMenu{
			Root: "main",
			Nodes: map[string]BotMenuNode{
				"main": {Text: "Меню", Items: []BotMenuItem{{Label: "Оплата", Command: "plus", Node: "main"}}},
			},
		}, false},
		// A screen with nothing on it and nowhere back is a message the
		// reader can only close.
		{"an empty top screen", BotMenu{
			Root:  "main",
			Nodes: map[string]BotMenuNode{"main": {Text: "Меню"}},
		}, false},
	} {
		err := tc.menu.Valid(commands)
		if (err == nil) != tc.ok {
			t.Errorf("%s: Valid() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
