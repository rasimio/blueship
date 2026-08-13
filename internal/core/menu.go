package core

import "strings"

// Inline menus.
//
// A host describes screens and what each button does; the transport
// renders them, routes the taps and edits one message in place rather
// than posting a new one per step. A menu that grows a chat by a bubble
// per tap is worse than no menu.
//
// Two controls are provided rather than left to the host, because a menu
// without them is a trap: somewhere to go back to, and a way to make it
// go away.

// BotMenu is a small tree of screens.
type BotMenu struct {
	// Root is the node a menu command opens.
	Root string
	// Nodes are keyed by id. An id travels in Telegram callback data,
	// which is capped at 64 bytes, so keep them short.
	Nodes map[string]BotMenuNode

	// BackLabel and CloseLabel are the two controls the transport adds
	// itself. Empty falls back to plain wording rather than leaving a
	// blank button, which Telegram rejects for the whole keyboard.
	BackLabel  string
	CloseLabel string
}

// BotMenuNode is one screen.
type BotMenuNode struct {
	// Text is the message body while this screen is showing.
	Text string
	// Parent is the node the back button leads to. Empty means this is a
	// top screen and gets no back button — only close.
	Parent string
	Items  []BotMenuItem
}

// BotMenuItem is one button.
//
// Exactly one of Node, Command or URL does something. A URL item opens a
// link and leaves the menu standing; the other two act and then close
// it, because a menu that stays open after you have chosen is a menu you
// have to dismiss twice.
type BotMenuItem struct {
	Label string
	// Node navigates to another screen, editing the message in place.
	Node string
	// Command runs a host-answered command, as though it had been typed.
	Command string
	// URL opens a link.
	URL string
}

// Valid reports whether a menu can be rendered and every button in it
// leads somewhere. Checked at startup, where a typo is a boot failure
// rather than a keyboard that silently does nothing.
//
// commands is the bot's configured command list: a button naming a
// command that does not exist is the same bug as one naming a screen
// that does not exist, and neither is visible until somebody taps.
func (m BotMenu) Valid(commands []BotCommand) error {
	known := make(map[string]bool, len(commands))
	for _, c := range commands {
		known[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(c.Name), "/"))] = true
	}
	if len(m.Nodes) == 0 {
		return nil // no menu configured is a legitimate state
	}
	if _, ok := m.Nodes[m.Root]; !ok {
		return &MenuError{Reason: "root node " + m.Root + " does not exist"}
	}
	for id, node := range m.Nodes {
		if node.Parent != "" {
			if _, ok := m.Nodes[node.Parent]; !ok {
				return &MenuError{Reason: "node " + id + " has parent " + node.Parent + ", which does not exist"}
			}
		}
		if len(node.Items) == 0 && node.Parent == "" {
			return &MenuError{Reason: "node " + id + " has no items and nowhere to go back to"}
		}
		for _, item := range node.Items {
			if item.Label == "" {
				return &MenuError{Reason: "node " + id + " has a button with no label"}
			}
			set := 0
			if item.Node != "" {
				set++
				if _, ok := m.Nodes[item.Node]; !ok {
					return &MenuError{Reason: "button " + item.Label + " leads to " + item.Node + ", which does not exist"}
				}
			}
			if item.Command != "" {
				set++
				name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(item.Command), "/"))
				if !known[name] {
					return &MenuError{Reason: "button " + item.Label + " runs /" + name + ", which is not a configured command"}
				}
			}
			if item.URL != "" {
				set++
			}
			if set != 1 {
				return &MenuError{Reason: "button " + item.Label + " must do exactly one thing"}
			}
		}
	}
	return nil
}

// MenuError is a menu that cannot be rendered.
type MenuError struct{ Reason string }

func (e *MenuError) Error() string { return "bot menu: " + e.Reason }
