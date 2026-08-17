package core

import "strings"

// Persistent keyboards.
//
// The inline menu in menu.go hangs under one message: it can nest, and
// it scrolls away with the conversation. This one replaces the phone's
// keyboard instead, sits under the input field and stays there — the
// difference matters, because a menu somebody has to scroll back to
// find is a menu they use once.
//
// The trade is that Telegram gives it no callbacks. Tapping a button
// sends its label as an ordinary message, so the label IS the wire
// format, and the transport has to recognise it on the way back in.
// That is why a button carries a command: without the mapping, "Подписка"
// arrives as something a person said and gets answered as conversation.

// BotKeyboard is the keyboard shown under the input field.
type BotKeyboard struct {
	// Rows of buttons, laid out as given. Telegram shrinks them to fit;
	// three per row is where labels start truncating on a narrow phone.
	Rows [][]BotKeyboardButton
	// Placeholder is the grey hint inside the input field while the
	// keyboard is showing. Optional.
	Placeholder string
}

// BotKeyboardButton is one key.
type BotKeyboardButton struct {
	// Label is what the person sees AND what Telegram sends when they
	// tap it. Two buttons cannot share one.
	Label string
	// Command this button stands for, without the slash. The transport
	// rewrites an inbound message equal to Label into "/Command", so the
	// tap is answered by the same code a typed command reaches.
	Command string
}

// Configured reports whether a keyboard was set up at all.
func (k BotKeyboard) Configured() bool { return len(k.Rows) > 0 }

// CommandFor returns the command a tapped label stands for.
//
// Exact match, not a prefix or a contains: a person is free to type
// "подписка когда спишется" as a real question, and only the button's
// own text — which Telegram sends verbatim — may be rewritten.
func (k BotKeyboard) CommandFor(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	for _, row := range k.Rows {
		for _, b := range row {
			if b.Label == text {
				return b.Command, true
			}
		}
	}
	return "", false
}

// Valid reports whether the keyboard can be shown and every key leads
// somewhere. Checked at startup: a key that answers nothing is
// indistinguishable from one that works until somebody presses it, and
// what it does instead is put a stray word into the conversation.
func (k BotKeyboard) Valid(commands []BotCommand) error {
	if !k.Configured() {
		return nil
	}
	known := make(map[string]bool, len(commands))
	for _, c := range commands {
		known[normalizeCommandName(c.Name)] = true
	}
	seen := make(map[string]bool)
	for _, row := range k.Rows {
		if len(row) == 0 {
			return &MenuError{Reason: "keyboard has an empty row"}
		}
		for _, b := range row {
			switch {
			case strings.TrimSpace(b.Label) == "":
				return &MenuError{Reason: "keyboard has a key with no label"}
			case seen[b.Label]:
				// Two keys with one label are one key as far as the
				// inbound text is concerned; the second is unreachable.
				return &MenuError{Reason: "keyboard has two keys labelled " + b.Label}
			case strings.TrimSpace(b.Command) == "":
				return &MenuError{Reason: "key " + b.Label + " runs no command"}
			case !known[normalizeCommandName(b.Command)]:
				return &MenuError{Reason: "key " + b.Label + " runs /" +
					normalizeCommandName(b.Command) + ", which is not a configured command"}
			}
			seen[b.Label] = true
		}
	}
	return nil
}

func normalizeCommandName(name string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "/"))
}
