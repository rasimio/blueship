package gateway

import (
	"context"
	"strings"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

const (
	// menuCallbackNode navigates: mn:<node id>.
	menuCallbackNode = "mn:"
	// menuCallbackCommand runs a host command from a menu button:
	// mc:<command>. Its own prefix rather than the onboarding one, so the
	// menu owns what happens to itself afterwards — that path strips the
	// keyboard and would leave a menu's text standing with no buttons,
	// which is a husk, not a closed menu.
	menuCallbackCommand = "mc:"
	// menuCallbackClose makes the menu go away. A distinct token rather
	// than a reserved node id, so a host cannot name a screen "close" and
	// quietly break the control that dismisses it.
	menuCallbackClose = "mn!x"
)

func (g *Gateway) menu() bs.BotMenu { return g.deps.Config.Gateway.Menu }

// openMenu posts the menu's root screen.
func (g *Gateway) openMenu(ctx context.Context, bi *botInstance, tgChatID int64) bool {
	menu := g.menu()
	node, ok := menu.Nodes[menu.Root]
	if !ok {
		g.logger.Error("menu: no root screen configured", "root", menu.Root)
		return false
	}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, node.Text, g.menuKeyboard(menu, menu.Root)); err != nil {
		g.logger.Warn("menu: could not open", "chat_id", tgChatID, "error", err)
		return false
	}
	return true
}

// menuKeyboard renders one screen, with the two controls the transport
// owns: back where there is somewhere to go back to, and close always.
func (g *Gateway) menuKeyboard(menu bs.BotMenu, nodeID string) [][]telegram.InlineKeyboardButton {
	node := menu.Nodes[nodeID]
	rows := make([][]telegram.InlineKeyboardButton, 0, len(node.Items)+1)
	for _, item := range node.Items {
		switch {
		case item.URL != "":
			rows = append(rows, []telegram.InlineKeyboardButton{{Text: item.Label, URL: item.URL}})
		case item.Node != "":
			rows = append(rows, []telegram.InlineKeyboardButton{
				{Text: item.Label, CallbackData: menuCallbackNode + item.Node}})
		case item.Command != "":
			rows = append(rows, []telegram.InlineKeyboardButton{
				{Text: item.Label, CallbackData: menuCallbackCommand + item.Command}})
		}
	}

	// Both controls on one row: they are navigation, not choices, and a
	// full-width "close" reads like an action of its own.
	controls := make([]telegram.InlineKeyboardButton, 0, 2)
	if node.Parent != "" {
		label := menu.BackLabel
		if label == "" {
			label = "Назад"
		}
		controls = append(controls, telegram.InlineKeyboardButton{
			Text: label, CallbackData: menuCallbackNode + node.Parent})
	}
	closeLabel := menu.CloseLabel
	if closeLabel == "" {
		closeLabel = "Закрыть"
	}
	controls = append(controls, telegram.InlineKeyboardButton{
		Text: closeLabel, CallbackData: menuCallbackClose})
	return append(rows, controls)
}

// maybeHandleMenuCallback routes a menu tap. Returns true when the tap
// was ours.
//
// Navigation edits the message in place. A menu that posts a new bubble
// per tap turns a two-level tree into a wall of dead screens, each with
// live buttons pointing nowhere in particular.
func (g *Gateway) maybeHandleMenuCallback(ctx context.Context, bi *botInstance, cq *telegram.CallbackQuery) bool {
	data := strings.TrimSpace(cq.Data)
	if data != menuCallbackClose &&
		!strings.HasPrefix(data, menuCallbackNode) &&
		!strings.HasPrefix(data, menuCallbackCommand) {
		return false
	}
	if cq.Message == nil || cq.From == nil {
		// Nothing to edit or nobody to act as. Claimed anyway: it is our
		// prefix, and letting it fall through would hand it to a flow that
		// reads it as an answer to a different question.
		return true
	}
	chatID, messageID := cq.Message.Chat.ID, cq.Message.MessageID

	if data == menuCallbackClose {
		// Deleted rather than emptied: "closed" should leave the chat as
		// it was, not leave a husk of a menu scrolling past forever.
		if err := bi.client.DeleteMessage(ctx, chatID, messageID); err != nil {
			g.logger.Warn("menu: could not close", "chat_id", chatID, "error", err)
		}
		return true
	}

	if strings.HasPrefix(data, menuCallbackCommand) {
		g.runMenuCommand(ctx, bi, cq, strings.TrimPrefix(data, menuCallbackCommand))
		return true
	}

	menu := g.menu()
	nodeID := strings.TrimPrefix(data, menuCallbackNode)
	node, ok := menu.Nodes[nodeID]
	if !ok {
		// A button from a menu that has since been reconfigured. Say so
		// rather than leaving the tap silent.
		g.logger.Warn("menu: tap on a screen that no longer exists", "node", nodeID)
		if err := bi.client.DeleteMessage(ctx, chatID, messageID); err != nil {
			g.logger.Warn("menu: could not remove a stale menu", "error", err)
		}
		return true
	}
	if err := bi.client.EditMessageText(ctx, chatID, messageID, node.Text, g.menuKeyboard(menu, nodeID)); err != nil {
		g.logger.Warn("menu: could not open a screen", "node", nodeID, "error", err)
	}
	return true
}

// runMenuCommand runs the command behind a menu button and takes the
// menu away.
//
// The menu goes first: the command posts its own answer, and a menu left
// standing underneath is one the reader has to dismiss twice.
//
// The command is replayed as though it had been typed, rather than
// dispatched straight to the host handler. A menu button must be able to
// carry any command the bot has — a host-answered one, a prompt
// shortcut, a transport-owned one like stop — and only the inbound path
// knows which is which. Routing to one of them directly would make the
// menu silently support a third of the command list.
func (g *Gateway) runMenuCommand(ctx context.Context, bi *botInstance, cq *telegram.CallbackQuery, name string) {
	if err := bi.client.DeleteMessage(ctx, cq.Message.Chat.ID, cq.Message.MessageID); err != nil {
		g.logger.Debug("menu: could not close after a command", "error", err)
	}
	g.handleUpdate(ctx, bi, menuCommandUpdate(cq, name))
}

// menuCommandUpdate turns a menu tap into the message the person did not
// type.
func menuCommandUpdate(cq *telegram.CallbackQuery, name string) telegram.Update {
	return telegram.Update{Message: &telegram.Message{
		MessageID: cq.Message.MessageID,
		From:      cq.From,
		Chat:      telegram.Chat{ID: cq.Message.Chat.ID, Type: "private"},
		Text:      "/" + strings.TrimPrefix(name, "/"),
	}}
}

// keyboard returns the host's persistent keyboard.
func (g *Gateway) keyboard() bs.BotKeyboard { return g.deps.Config.Gateway.Keyboard }

// showKeyboard posts text with the persistent keyboard installed under
// the input field. Falls back to a plain message when the host
// configured no keyboard, so callers need no branch of their own.
func (g *Gateway) showKeyboard(ctx context.Context, bi *botInstance, tgChatID int64, text string) {
	if bi == nil || bi.client == nil {
		return
	}
	kb := g.keyboard()
	if !kb.Configured() {
		g.sendOnboardingText(ctx, bi, tgChatID, text)
		return
	}
	rows := make([][]telegram.ReplyKeyboardButton, 0, len(kb.Rows))
	for _, row := range kb.Rows {
		keys := make([]telegram.ReplyKeyboardButton, 0, len(row))
		for _, b := range row {
			keys = append(keys, telegram.ReplyKeyboardButton{Text: b.Label})
		}
		rows = append(rows, keys)
	}
	if _, err := bi.client.SendMessageWithReplyKeyboard(ctx, tgChatID, text, telegram.ReplyKeyboard{
		Keyboard:         rows,
		ResizeKeyboard:   true,
		IsPersistent:     true,
		InputPlaceholder: kb.Placeholder,
	}); err != nil {
		g.logger.Warn("keyboard: could not show", "chat_id", tgChatID, "error", err)
		g.sendOnboardingText(ctx, bi, tgChatID, text)
	}
}

// rewriteKeyboardTap turns a tapped key back into the command it stands
// for.
//
// A reply keyboard has no callbacks: Telegram sends the label as though
// the person typed it. Without this the tap reaches the model as
// conversation — "Подписка" answered with a sentence about subscriptions
// instead of the checkout.
//
// Exact match only, and only against labels the host configured, so
// somebody who genuinely types those words mid-sentence is unaffected.
func (g *Gateway) rewriteKeyboardTap(text string) (string, bool) {
	cmd, ok := g.keyboard().CommandFor(text)
	if !ok {
		return text, false
	}
	return "/" + cmd, true
}
