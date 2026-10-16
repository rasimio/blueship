package gateway

import (
	"context"
	"strings"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
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
// user to return to their browser tab — or, when the host implements
// DeeplinkLoginReplier, give them a button that finishes the sign-in in
// whatever browser they open it in, for when that tab is gone.

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
// Errors from the host are logged and reported as UIStrings.DeeplinkLoginFailed
// so the user gets actionable feedback instead of silence.
func (g *Gateway) maybeRunDeeplinkLogin(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, text string) bool {
	if g.deps.DeeplinkLogin == nil || bi == nil || bi.id == uuid.Nil {
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

	var reply bs.DeeplinkReply
	var err error
	if replier, ok := g.deps.DeeplinkLogin.(bs.DeeplinkLoginReplier); ok {
		reply, err = replier.ReplyDeeplinkLogin(ctx, token, tgUserID)
	} else {
		// The host encodes the approval state in the message.
		_, reply.Text, err = g.deps.DeeplinkLogin.CompleteDeeplinkLogin(ctx, token, tgUserID)
	}
	if err != nil {
		g.logger.Warn("gateway: CompleteDeeplinkLogin failed",
			"bot_id", bi.id.String(), "tg_user", tgUserID, "error", err)
		// Tell the user to retry on the website. Don't reveal internals.
		reply = bs.DeeplinkReply{Text: g.deeplinkUI().DeeplinkLoginFailedText()}
	}
	g.sendDeeplinkReply(ctx, bi, tgChatID, reply)
	return true
}

// deeplinkUI is the host's wording for the deep-link failure answers; a
// gateway without a runtime Config (tests) falls back to the defaults.
func (g *Gateway) deeplinkUI() bs.UIStrings {
	if g.deps.Config == nil {
		return bs.UIStrings{}
	}
	return g.deps.Config.UI
}

// sendDeeplinkReply pushes the host-chosen answer back to the user: plain
// text, with the host's link button under it when there is one. Plain, so
// the host's wording reaches the person exactly as written and nothing in
// it is read as markup; no link preview, so a one-time URL in the button is
// fetched by no one but the person who taps it.
func (g *Gateway) sendDeeplinkReply(ctx context.Context, bi *botInstance, tgChatID int64, reply bs.DeeplinkReply) {
	if bi == nil || bi.client == nil || reply.Text == "" {
		return
	}
	var rows [][]telegram.InlineKeyboardButton
	if reply.ButtonLabel != "" && reply.ButtonURL != "" {
		rows = [][]telegram.InlineKeyboardButton{{{Text: reply.ButtonLabel, URL: reply.ButtonURL}}}
	}
	if _, err := bi.client.SendPlainMessage(ctx, tgChatID, reply.Text, rows); err != nil {
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
