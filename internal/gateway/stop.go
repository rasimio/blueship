package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/rasimio/blueship/internal/transport/telegram"
)

// stopCommand is the typed way to stop an answer, for when the button is out
// of reach: the message carrying it has scrolled away behind a long reply, or
// the client is one that hides inline keyboards.
const stopCommand = "/stop"

// maybeRunStopCommand handles /stop and reports whether it did.
//
// It runs during inbound preparation, before the message is admitted to the
// debounce queue, and that placement is the whole point: turns for one
// conversation are serialised, so a /stop travelling as an ordinary message
// would queue behind the very turn it means to stop and arrive after it
// finished.
func (g *Gateway) maybeRunStopCommand(ctx context.Context, bi *botInstance, rawChatID int64, us *UserState, text string) bool {
	if cmd, forUs := g.parseCommand(bi, text); cmd != stopCommand || !forUs {
		return false
	}
	reply := g.deps.Config.UI.StopNothingRunning
	if g.CancelTurn(us.UserID, us.SoulID, "") {
		reply = g.deps.Config.UI.StopAcknowledged
	}
	g.logger.Info("telegram /stop", "chat_id", us.ChatID, "user_id", us.UserID, "stopped", reply != g.deps.Config.UI.StopNothingRunning)
	if bi != nil && bi.client != nil {
		_, _ = bi.client.SendMessage(ctx, fmt.Sprintf("%d", rawChatID), reply)
	}
	return true
}

// maybeHandleStopCallback handles a tap on the stop button under a streaming
// reply and reports whether it did.
//
// The turn id travels in the callback data and is checked against the turn
// actually running, so a tap that lands just after its own turn ended cannot
// stop the next one. Who may press it needs no separate check: Telegram only
// delivers callbacks from chats the sender is in, and the conversation is
// resolved from the chat the button lives in.
func (g *Gateway) maybeHandleStopCallback(ctx context.Context, bi *botInstance, cq *telegram.CallbackQuery) bool {
	if cq == nil || !strings.HasPrefix(cq.Data, stopCallbackPrefix) {
		return false
	}
	turnID := strings.TrimPrefix(cq.Data, stopCallbackPrefix)

	answer := g.deps.Config.UI.StopNothingRunning
	if us := g.stopCallbackUser(ctx, bi, cq); us != nil && g.CancelTurn(us.UserID, us.SoulID, turnID) {
		answer = g.deps.Config.UI.StopAcknowledged
		g.logger.Info("telegram stop button", "chat_id", us.ChatID, "user_id", us.UserID, "turn_id", turnID)
	}
	if bi != nil && bi.client != nil {
		_ = bi.client.AnswerCallbackQueryText(ctx, cq.ID, answer)
	}
	return true
}

// stopCallbackUser resolves the conversation a stop tap belongs to, or nil
// when the chat has no assistant to stop.
func (g *Gateway) stopCallbackUser(ctx context.Context, bi *botInstance, cq *telegram.CallbackQuery) *UserState {
	if cq.Message == nil || cq.From == nil {
		return nil
	}
	rawChatID := cq.Message.Chat.ID
	us, err := g.getOrInitTelegramUser(ctx, bi, tgCanonical(rawChatID), rawChatID, cq.From.ID)
	if err != nil {
		g.logger.Debug("stop callback: unresolved chat", "chat_id", rawChatID, "error", err)
		return nil
	}
	return us
}
