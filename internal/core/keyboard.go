package core

import "strings"

// Persistent keyboards.
//
// The inline menu in menu.go hangs under one message: it can nest, and
// it scrolls away with the conversation. This one replaces the phone's
// keyboard and stays under the input field — the difference matters,
// because a menu somebody has to scroll back to find is a menu they use
// once.
//
// It nests too, by swapping itself: opening a screen sends a message
// carrying that screen's keys. The trade is that Telegram gives this
// kind of keyboard no callbacks at all. Tapping sends the label as an
// ordinary message, so the label IS the wire format and the transport
// has to recognise it coming back in. That shapes everything here: a
// label must be unique across the whole tree, and short enough to read
// on a key.

// BotKeyboard is a small tree of keyboard screens.
type BotKeyboard struct {
	// Root is the screen shown when the keyboard first appears.
	Root  string
	Nodes map[string]BotKeyboardNode

	// BackLabel and CloseLabel are the two controls the transport adds:
	// back on every screen that has a parent, close on the root.
	BackLabel  string
	CloseLabel string
	// Closed is what to say while taking the keyboard away — Telegram
	// cannot remove one without a message.
	Closed string
	// Placeholder is the grey hint in the input field.
	Placeholder string
}

// BotKeyboardNode is one screen.
type BotKeyboardNode struct {
	// Text introduces the screen. Sent as the message that carries it,
	// because a keyboard cannot change without one.
	Text   string
	Parent string
	Rows   [][]BotKeyboardKey
}

// BotKeyboardKey is one key.
//
// Exactly one of Node, Command or Say does something:
//
//   - Node swaps the keyboard to another screen and says nothing to the
//     model.
//   - Command runs a host command, as though it had been typed.
//   - Say replaces the label with the message the person meant. It is
//     how a key can be short and the message it sends can be a whole
//     sentence — a demonstration key that reads «Нарисуй картинку» and
//     actually asks for one.
type BotKeyboardKey struct {
	Label   string
	Node    string
	Command string
	Say     string
}

// Configured reports whether a keyboard was set up at all.
func (k BotKeyboard) Configured() bool { return len(k.Nodes) > 0 }

// KeyAction is what a tapped label means.
type KeyAction struct {
	// Node is the screen to switch to, when the key navigates.
	Node string
	// Text is what to hand the rest of the pipeline, when the key says
	// something: a "/command" or a replacement message.
	Text string
	// Close means take the keyboard away.
	Close bool
}

// Action resolves an inbound message against the keyboard.
//
// Exact match, not a prefix or a contains: a person is free to type
// «Подписка когда спишется» and mean it, and only the button's own text
// — which Telegram sends verbatim — may be rewritten.
func (k BotKeyboard) Action(text string) (KeyAction, bool) {
	text = strings.TrimSpace(text)
	if text == "" || !k.Configured() {
		return KeyAction{}, false
	}
	if k.CloseLabel != "" && text == k.CloseLabel {
		return KeyAction{Close: true}, true
	}
	if k.BackLabel != "" && text == k.BackLabel {
		// Back is resolved by the caller, which knows the screen the
		// person is on. Reported as a node with no id.
		return KeyAction{Node: ""}, true
	}
	for _, node := range k.Nodes {
		for _, row := range node.Rows {
			for _, key := range row {
				if key.Label != text {
					continue
				}
				switch {
				case key.Node != "":
					return KeyAction{Node: key.Node}, true
				case key.Command != "":
					return KeyAction{Text: "/" + normalizeCommandName(key.Command)}, true
				default:
					return KeyAction{Text: key.Say}, true
				}
			}
		}
	}
	return KeyAction{}, false
}

// Valid reports whether the keyboard can be shown and every key leads
// somewhere. Checked at startup: a key that resolves to nothing does not
// fail visibly, it drops its own label into the conversation as though
// the person had said it out loud.
func (k BotKeyboard) Valid(commands []BotCommand) error {
	if !k.Configured() {
		return nil
	}
	if _, ok := k.Nodes[k.Root]; !ok {
		return &MenuError{Reason: "keyboard root " + k.Root + " does not exist"}
	}
	if k.CloseLabel == "" {
		return &MenuError{Reason: "keyboard has no way to close it"}
	}
	known := make(map[string]bool, len(commands))
	for _, c := range commands {
		known[normalizeCommandName(c.Name)] = true
	}
	// Labels are the wire format, so one label is one meaning across the
	// whole tree — including the two controls the transport adds.
	seen := map[string]bool{k.CloseLabel: true}
	if k.BackLabel != "" {
		seen[k.BackLabel] = true
	}
	for id, node := range k.Nodes {
		if node.Parent != "" {
			if _, ok := k.Nodes[node.Parent]; !ok {
				return &MenuError{Reason: "screen " + id + " has parent " + node.Parent + ", which does not exist"}
			}
			if k.BackLabel == "" {
				return &MenuError{Reason: "screen " + id + " has a parent but the keyboard has no back label"}
			}
		}
		if len(node.Rows) == 0 {
			return &MenuError{Reason: "screen " + id + " has no keys"}
		}
		if strings.TrimSpace(node.Text) == "" {
			// The message carries the keyboard; without text there is
			// nothing to send, and the screen cannot open.
			return &MenuError{Reason: "screen " + id + " has no text to open with"}
		}
		for _, row := range node.Rows {
			if len(row) == 0 {
				return &MenuError{Reason: "screen " + id + " has an empty row"}
			}
			for _, key := range row {
				if err := k.validKey(id, key, known, seen); err != nil {
					return err
				}
				seen[key.Label] = true
			}
		}
	}
	return nil
}

func (k BotKeyboard) validKey(screen string, key BotKeyboardKey, known, seen map[string]bool) error {
	switch {
	case strings.TrimSpace(key.Label) == "":
		return &MenuError{Reason: "screen " + screen + " has a key with no label"}
	case seen[key.Label]:
		// Two keys with one label are one key as far as the inbound text
		// can tell; the second is unreachable by construction.
		return &MenuError{Reason: "two keys are labelled " + key.Label}
	case strings.HasPrefix(key.Label, "/"):
		// The label is sent verbatim, so a slash makes it a command the
		// host did not mean.
		return &MenuError{Reason: "key " + key.Label + " is a slash command; the label is sent as-is"}
	}
	set := 0
	if key.Node != "" {
		set++
		if _, ok := k.Nodes[key.Node]; !ok {
			return &MenuError{Reason: "key " + key.Label + " opens " + key.Node + ", which does not exist"}
		}
	}
	if key.Command != "" {
		set++
		if !known[normalizeCommandName(key.Command)] {
			return &MenuError{Reason: "key " + key.Label + " runs /" +
				normalizeCommandName(key.Command) + ", which is not a configured command"}
		}
	}
	if key.Say != "" {
		set++
	}
	if set != 1 {
		return &MenuError{Reason: "key " + key.Label + " must do exactly one thing"}
	}
	return nil
}

func normalizeCommandName(name string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "/"))
}
