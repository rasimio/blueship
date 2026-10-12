package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// callerFromMessage is the Telegram account behind an inbound message.
func callerFromMessage(msg *telegram.Message) bs.TelegramCaller {
	if msg == nil || msg.From == nil {
		return bs.TelegramCaller{}
	}
	return bs.TelegramCaller{
		UserID:       msg.From.ID,
		ChatID:       msg.Chat.ID,
		Username:     msg.From.Username,
		FirstName:    msg.From.FirstName,
		LanguageCode: msg.From.LanguageCode,
	}
}

// callerFromCallback is the Telegram account behind a button tap. The chat
// is the one the tapped message lives in; nothing is read from the button.
func callerFromCallback(cq *telegram.CallbackQuery) bs.TelegramCaller {
	if cq == nil || cq.From == nil {
		return bs.TelegramCaller{}
	}
	c := bs.TelegramCaller{
		UserID:       cq.From.ID,
		Username:     cq.From.Username,
		FirstName:    cq.From.FirstName,
		LanguageCode: cq.From.LanguageCode,
	}
	if cq.Message != nil {
		c.ChatID = cq.Message.Chat.ID
	}
	return c
}

// callerFromSender rebuilds the caller on the onboarding paths, which carry
// the parts of the update they need as loose values.
func callerFromSender(tgChatID, tgUserID int64, sender tgSender) bs.TelegramCaller {
	return bs.TelegramCaller{
		UserID:       tgUserID,
		ChatID:       tgChatID,
		Username:     sender.Handle,
		FirstName:    sender.Name,
		LanguageCode: sender.Language,
	}
}

func senderFromCaller(c bs.TelegramCaller) tgSender {
	return tgSender{Name: c.FirstName, Handle: c.Username, Language: c.LanguageCode}
}

// hasPrivateCommands reports whether any configured command is listed only
// to the CommandAudience.
func (g *Gateway) hasPrivateCommands() bool {
	for _, c := range g.deps.Config.Gateway.Commands {
		if c.Private {
			return true
		}
	}
	return false
}

// maybePublishPrivateCommands gives a private chat the menu with the Private
// commands in it when its owner is in the audience.
//
// Published from the inbound path because that is the first moment the
// gateway knows who is in a chat: the bot-wide menu goes out at
// registration, when there is nobody to ask about. Once per chat per
// process, and in the background — a menu is discoverability, and the
// message that prompted it must not wait on Telegram for it.
func (g *Gateway) maybePublishPrivateCommands(ctx context.Context, bi *botInstance, caller bs.TelegramCaller) {
	audience := g.deps.Config.Gateway.CommandAudience
	if audience == nil || bi == nil || bi.client == nil || !caller.Private() || !g.hasPrivateCommands() {
		return
	}
	if !audience(caller) {
		return
	}
	key := telegramUserCacheKey(bi.id, tgCanonical(caller.ChatID))
	if _, done := g.privateMenus.LoadOrStore(key, struct{}{}); done {
		return
	}
	commands := privateTelegramCommands(g.deps.Config.Gateway.Commands)
	go func() {
		cmdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := bi.client.SetChatCommands(cmdCtx, caller.ChatID, commands); err != nil {
			// Forget the attempt so the next message tries again.
			g.privateMenus.Delete(key)
			g.logger.Warn("gateway: publishing a chat's private command menu failed",
				"bot_id", bi.id.String(), "chat_id", caller.ChatID, "error", err)
			return
		}
		g.logger.Info("gateway: published a chat's private command menu",
			"bot_id", bi.id.String(), "chat_id", caller.ChatID, "count", len(commands))
	}()
}

// isExclusiveCommand reports whether a host command must run with its
// conversation quiet.
func (g *Gateway) isExclusiveCommand(name string) bool {
	for _, c := range g.deps.Config.Gateway.Commands {
		if c.Host && c.Exclusive && strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(c.Name), "/"), name) {
			return true
		}
	}
	return false
}

// quietWait bounds how long an Exclusive command waits for the answer in
// flight to stop. A turn honours cancellation between steps, so this is
// generous; one that still has not let go is stuck, and the command gives
// up rather than waiting on it forever. A var so tests can shorten it.
var quietWait = 30 * time.Second

// quietConversation stops the turn running in one conversation and takes
// its turn lock, so nothing reads or writes the conversation until the
// returned release is called.
//
// The stop is repeated while waiting: a message that was still in the
// debouncer when the first one landed starts a turn of its own, and that
// turn has to be stopped too.
func (g *Gateway) quietConversation(ctx context.Context, userID, soulID uuid.UUID) (func(), error) {
	mu := g.turnMutex(userID, soulID)
	deadline := time.Now().Add(quietWait)
	for {
		g.CancelTurn(userID, soulID, "")
		if mu.TryLock() {
			return mu.Unlock, nil
		}
		if time.Now().After(deadline) {
			return nil, bs.ErrConversationBusy
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

var errConversationRetired = errors.New("gateway: the conversation's account was reset")

// retireConversation marks a conversation whose account was reset to its
// first day. Its soul is gone and soul ids are never reused, so the mark
// is permanent: whatever was queued for it — a message still in the
// debouncer, a turn waiting on its lock, an autonomous draft — is dropped
// when it gets there instead of answering from an assistant that no
// longer exists.
func (g *Gateway) retireConversation(userID, soulID uuid.UUID) {
	g.retired.Store(conversationKey(userID, soulID), struct{}{})
}

func (g *Gateway) conversationRetired(userID, soulID uuid.UUID) bool {
	_, retired := g.retired.Load(conversationKey(userID, soulID))
	return retired
}

// isPrivateCommand reports whether a command is listed only to the
// CommandAudience.
func (g *Gateway) isPrivateCommand(name string) bool {
	for _, c := range g.deps.Config.Gateway.Commands {
		if c.Private && strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(c.Name), "/"), name) {
			return true
		}
	}
	return false
}

// maybeRetractPrivateCommands takes the private menu back from a chat whose
// owner used a Private command they are no longer admitted to — typically
// after being taken off the host's list, or after changing the username
// the list named. Telegram keeps a chat's menu until told otherwise, and
// this is the moment the gateway learns the one it published is out of
// date.
func (g *Gateway) maybeRetractPrivateCommands(ctx context.Context, bi *botInstance, name string, caller bs.TelegramCaller) {
	if bi == nil || bi.client == nil || !caller.Private() || !g.isPrivateCommand(name) {
		return
	}
	if audience := g.deps.Config.Gateway.CommandAudience; audience != nil && audience(caller) {
		return
	}
	g.privateMenus.Delete(telegramUserCacheKey(bi.id, tgCanonical(caller.ChatID)))
	go func() {
		cmdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := bi.client.DeleteChatCommands(cmdCtx, caller.ChatID); err != nil {
			g.logger.Warn("gateway: retracting a chat's private command menu failed",
				"bot_id", bi.id.String(), "chat_id", caller.ChatID, "error", err)
		}
	}()
}

// forgetTelegramAccount drops everything cached about an account's Telegram
// chats, on every bot: the conversation each chat was bound to, the
// keyboard screen it was on, the keyboard question it had open.
//
// Every bot, not just the one the reset came from: the same person reaches
// the same account through each of them, and a chat left cached on another
// bot would go on talking to the account that no longer exists.
func (g *Gateway) forgetTelegramAccount(userID uuid.UUID) {
	if userID == uuid.Nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, us := range g.users {
		if us == nil || us.UserID != userID || !isTelegramChatKey(key) {
			continue
		}
		delete(g.users, key)
		delete(g.kbScreen, key)
		delete(g.kbAsk, key)
	}
}

// isTelegramChatKey reports whether a users key names a Telegram chat — the
// composite multi-bot key or the legacy bare chat id — rather than a web or
// voice session, which are keyed by soul and rebuilt on their own.
func isTelegramChatKey(key string) bool {
	return strings.HasPrefix(key, "telegram-bot:") || strings.HasPrefix(key, "telegram:")
}

// greetAsNew opens a chat whose account was just reset the way its first
// /start would. What that is belongs to the onboarding hook — an account
// on the spot with its welcome, or the first question of the wizard — so
// the person sees exactly what a newcomer sees, not an imitation of it.
func (g *Gateway) greetAsNew(ctx context.Context, bi *botInstance, caller bs.TelegramCaller) {
	if bi == nil || caller.UserID == 0 {
		return
	}
	chatID := tgCanonical(caller.ChatID)
	g.invalidateTelegramUser(bi.id, chatID)
	if !g.maybeRunBotOnboarding(ctx, bi, chatID, caller.ChatID, caller.UserID, "/start", senderFromCaller(caller)) {
		// No chat onboarding on this deployment: say what an unpaired
		// chat is told, which is how a newcomer would be met here.
		g.replyUnpaired(ctx, bi, chatID)
	}
}
