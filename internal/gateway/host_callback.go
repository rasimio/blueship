package gateway

import (
	"context"
	"strings"

	"github.com/rasimio/blueship/internal/transport/telegram"
)

const hostCallbackPrefix = "host:"

func (g *Gateway) isHostCallbackCommand(name string) bool {
	for _, cmd := range g.deps.Config.Gateway.Commands {
		if cmd.Name == name && cmd.Host {
			return true
		}
	}
	return false
}

func (g *Gateway) maybeHandleHostCallback(ctx context.Context, bi *botInstance, cq *telegram.CallbackQuery) bool {
	if cq == nil || !strings.HasPrefix(cq.Data, hostCallbackPrefix) {
		return false
	}
	if bi == nil || cq.From == nil || cq.Message == nil || g.deps.CommandHandler == nil {
		return true
	}
	// Personal assistant cards may contain private results. Never render them
	// into a group, or resolve identity from IDs supplied in callback payloads.
	if cq.Message.Chat.ID != cq.From.ID {
		return true
	}
	name, args, ok := strings.Cut(strings.TrimPrefix(cq.Data, hostCallbackPrefix), ":")
	if !ok || len(cq.Data) > 64 || !g.isHostCallbackCommand(name) {
		return true
	}
	us := g.stopCallbackUser(ctx, bi, cq)
	if us == nil {
		return true
	}
	g.runHostCommand(ctx, bi, callerFromCallback(cq), us, name, args, cq.Message.MessageID)
	return true
}
