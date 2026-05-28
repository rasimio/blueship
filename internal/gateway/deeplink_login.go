package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Deep-link "Approve in bot" auth flow. The cabinet's "Login via
// Telegram App" button points at https://t.me/<platform_bot>?start=login_<TOKEN>;
// Telegram delivers that as a normal text message of the form
// "/start login_<TOKEN>". We intercept that exact shape here — before the
// FSM-based onboarding path or any cortex turn — hand the token to the
// host's CompleteDeeplinkLogin hook, then reply with whatever line the
// host decided fits (success confirmation or "link expired").
//
// We deliberately do NOT mutate UserState, sessions, or memory: the auth
// graduation happens browser-side, on the cabinet's next poll. The bot's
// only job here is to (a) signal Telegram-side approval and (b) tell the
// user to return to their browser tab.

// deeplinkPayloadPrefix is the literal prefix Telegram bot deep links use
// for our login-approve flow. The cabinet's StartDeeplink builds the
// URL with `?start=login_<token>` and Telegram delivers the message text
// as "/start login_<token>"; we look for exactly that prefix on the args
// portion of a parsed /start command.
const deeplinkPayloadPrefix = "login_"

// maybeRunDeeplinkLogin intercepts inbound text matching "/start login_<TOKEN>"
// addressed to this bot and runs the host's CompleteDeeplinkLogin hook.
// Returns true when the message has been handled by the deep-link path
// (caller must stop processing); false means the caller continues down
// the normal onboarding / cortex routing.
//
// The detection is intentionally narrow:
//   - only platform-kind bots are allowed to act on these links (user
//     bots have no business approving sign-in for someone else's account);
//   - only the exact "/start login_<…>" pattern matches — bare "/start"
//     falls through to the onboarding FSM as before;
//   - the host hook must be wired, otherwise we can't approve anything.
//
// Errors from the host are logged and reported as a "link expired" reply
// so the user gets actionable feedback instead of silence.
func (g *Gateway) maybeRunDeeplinkLogin(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, text string) bool {
	if g.deps.BotOnboarding == nil || bi == nil || bi.id == uuid.Nil {
		return false
	}
	// Only the platform bot is meant to receive deep-link approvals;
	// user-owned bots would otherwise be a way to approve sign-ins for
	// the platform's auth surface.
	if bi.kind != "" && bi.kind != "platform" {
		return false
	}

	cmd, args, forUs := parseStartCommandArgs(g, bi, text)
	if !forUs || cmd != "/start" {
		return false
	}
	if !strings.HasPrefix(args, deeplinkPayloadPrefix) {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(args, deeplinkPayloadPrefix))
	if token == "" {
		return false
	}

	approved, message, err := g.deps.BotOnboarding.CompleteDeeplinkLogin(ctx, token, tgUserID)
	if err != nil {
		g.logger.Warn("gateway: CompleteDeeplinkLogin failed",
			"bot_id", bi.id.String(), "tg_user", tgUserID, "error", err)
		// Treat host errors as "expired" so the user knows to retry on
		// the cabinet. Don't reveal internals.
		message = "Login link expired — try again on the website."
	}
	_ = approved // host already encoded approval state in the message
	g.sendDeeplinkReply(ctx, bi, tgChatID, message)
	return true
}

// sendDeeplinkReply pushes the host-chosen confirmation line back to the
// user. Bare-text SendMessage keeps formatting simple and deterministic —
// these strings are short by design.
func (g *Gateway) sendDeeplinkReply(ctx context.Context, bi *botInstance, tgChatID int64, text string) {
	if bi == nil || bi.client == nil || text == "" {
		return
	}
	if _, err := bi.client.SendMessage(ctx, fmt.Sprintf("%d", tgChatID), text); err != nil {
		g.logger.Warn("gateway: deeplink reply send failed",
			"tg_chat", tgChatID, "error", err)
	}
}

// parseStartCommandArgs splits "/start <args>" while honouring an optional
// "@botname" suffix on the command. Returns the bare command (e.g. "/start"),
// the args portion (everything after the first space), and forUs reflecting
// whether the command targets this bot.
//
// Lives here rather than as a Gateway method because the existing
// parseCommand only returns the command head — we need the args tail too,
// and only for this one call site, so a tiny helper keeps the surface
// of gateway.go from growing.
func parseStartCommandArgs(g *Gateway, bi *botInstance, text string) (cmd, args string, forUs bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	head := text
	rest := ""
	if i := strings.IndexByte(text, ' '); i >= 0 {
		head = text[:i]
		rest = strings.TrimSpace(text[i+1:])
	}
	if i := strings.IndexByte(head, '@'); i >= 0 {
		target := strings.ToLower(head[i+1:])
		cmd = head[:i]
		botName := ""
		if bi != nil {
			botName = bi.tgUsername
		}
		if botName == "" || strings.EqualFold(target, botName) {
			return cmd, rest, true
		}
		return cmd, rest, false
	}
	_ = g // keep signature symmetric with parseCommand for future logger use
	return head, rest, true
}
