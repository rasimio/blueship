package gateway

import (
	"context"
	"strings"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

// Deep-link "Connect Telegram" account-linking flow. The cabinet's Settings
// page has a "Connect this Telegram" button that calls
// POST /api/telegram/link/start; the host mints a token bound to the
// signed-in user's existing soul and returns
// https://t.me/<platform_bot>?start=link_<TOKEN>. Telegram delivers that as a
// normal text message "/start link_<TOKEN>". We intercept that exact shape
// here — before the FSM-based onboarding path — and hand the token to the
// host's CompleteDeeplinkLink hook, which writes the chat→soul routing row so
// the EXISTING soul receives this chat's messages instead of the onboarding
// FSM minting a brand-new one.
//
// This is the inverse of deeplink_login.go: login_ authenticates a cabinet
// session via a Telegram-keyed user; link_ attaches a Telegram chat to a soul
// the user already created on the web. Without it, a web-signup user who opens
// the bot has no bot_links row, so onboarding fires and offers a second
// account — the bug this flow fixes.

// linkPayloadPrefix is the literal prefix Telegram bot deep links use for the
// account-linking flow. The cabinet builds the URL with `?start=link_<token>`
// and Telegram delivers the message text as "/start link_<token>".
const linkPayloadPrefix = "link_"

// maybeRunDeeplinkLink intercepts inbound text matching "/start link_<TOKEN>"
// addressed to the platform bot and runs the host's CompleteDeeplinkLink hook.
// Returns true when handled (caller must stop processing); false means the
// caller continues down the normal onboarding / cortex routing.
//
// The detection mirrors maybeRunDeeplinkLogin:
//   - the host's BotOnboarding hook must additionally implement DeeplinkLinker
//     (an optional extension), otherwise we can't link anything;
//   - only platform-kind bots act on these links — a user-owned bot has no
//     business re-homing a chat onto someone's account;
//   - only the exact "/start link_<…>" pattern matches; bare "/start" still
//     falls through to onboarding.
//
// On success the host returns a confirmation line naming the soul; on a benign
// failure (expired token, or the Telegram id already belongs to another
// account) it returns an explanatory line and a nil error. Infrastructure
// errors are logged and reported as a generic "try again" line.
func (g *Gateway) maybeRunDeeplinkLink(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, text string) bool {
	if g.deps.BotOnboarding == nil || bi == nil || bi.id == uuid.Nil {
		return false
	}
	linker, ok := g.deps.BotOnboarding.(bs.DeeplinkLinker)
	if !ok {
		return false
	}
	// Only the platform bot is meant to receive link approvals; a
	// user-owned bot could otherwise re-home arbitrary chats.
	if bi.kind != "" && bi.kind != "platform" {
		return false
	}

	cmd, args, forUs := parseStartCommandArgs(g, bi, text)
	if !forUs || cmd != "/start" {
		return false
	}
	if !strings.HasPrefix(args, linkPayloadPrefix) {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(args, linkPayloadPrefix))
	if token == "" {
		return false
	}

	message, err := linker.CompleteDeeplinkLink(ctx, token, bi.id, tgUserID, tgChatID)
	if err != nil {
		g.logger.Warn("gateway: CompleteDeeplinkLink failed",
			"bot_id", bi.id.String(), "tg_user", tgUserID, "error", err)
		message = "Link expired — open Settings on the website and try again."
	}
	g.sendDeeplinkReply(ctx, bi, tgChatID, message)

	// Drop any cached UserState built before the bot_links row landed so the
	// next inbound message resolves the freshly-linked (user, soul) via
	// getOrInitTelegramUser rather than re-running onboarding.
	g.mu.Lock()
	delete(g.users, tgCanonical(tgChatID))
	g.mu.Unlock()
	return true
}
