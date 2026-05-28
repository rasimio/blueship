package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/telegram"
)

// Inline Telegram-native onboarding FSM. Engages when an inbound
// message lands on a chat with no vaelum.user_identities row and the
// host has wired Deps.BotOnboarding. The flow is intentionally tiny —
// three steps end-to-end — so the dispatch is a hand-rolled switch on
// step name, not a state-machine library.
//
// Step names match the BotOnboarding contract:
//
//	"" / "start"      — entry; emit greeting + ask name; advance to ask_name
//	"ask_name"        — text reply is the user's display name; emit preset
//	                    picker (inline keyboard); advance to ask_soul_preset
//	"ask_soul_preset" — callback_query carries the preset id; CreateAccount;
//	                    ClearState; emit success line. UserState is built on
//	                    the NEXT inbound — the user is now a regular tenant.
//
// Idempotency: any /start re-issued mid-flow re-emits the current
// step's prompt without touching state — the user can always recover
// by typing /start again.
//
// All UI strings are hardcoded in this file because they are not LLM
// prompts but Telegram UI copy (the "no hardcoded text in Go" rule
// applies to model-facing prompts; user-facing chrome stays here so a
// translation pass is a code change, not a DB migration).

const (
	onbStepStart        = "start"
	onbStepAskName      = "ask_name"
	onbStepAskSoulPres  = "ask_soul_preset"
	onbCallbackPrefix   = "onb:"
	onbPresetArleneTag  = "arlene"
	onbPresetCustomTag  = "custom"
	onbStateKeyUserName = "name"
)

// onboardingMessages bundles the localised UI strings (Russian, short
// per the spec). Kept as a struct so a future locale switch is one
// data swap instead of inline edits.
type onboardingMessages struct {
	Greeting        string
	NamePromptFmt   string // %s = name
	WorkingFmt      string // %s = name
	DoneFmt         string // %s = name
	BackFmt         string // %s = name (welcome-back greeting)
	BtnArleneStyle  string
	BtnCustomStyle  string
	ErrAccountFail  string
}

var onbMsg = onboardingMessages{
	Greeting:       "Привет, я Vaelum. Помогу тебе создать собственного ассистента. Как тебя зовут?",
	NamePromptFmt:  "Приятно, %s. Выбери стиль для своего ассистента:",
	WorkingFmt:     "Готовлю...",
	DoneFmt:        "Готово, %s. Напиши что-нибудь, и я отвечу.",
	BackFmt:        "С возвращением, %s!",
	BtnArleneStyle: "Арлин-стиль (живая, эмоциональная)",
	BtnCustomStyle: "Свой стиль",
	ErrAccountFail: "Что-то пошло не так при создании аккаунта. Попробуй /start ещё раз.",
}

// maybeRunBotOnboarding intercepts inbound text from a chat with no
// vaelum identity. Returns true when the message has been handled by
// the FSM (caller stops processing); false means the caller continues
// down the normal getOrInitTelegramUser path.
//
// The detection is one DB read on the host hook (GetState). When the
// hook is nil or the user already has identity (i.e. ResolveTelegramChat
// would succeed), this returns false fast.
func (g *Gateway) maybeRunBotOnboarding(ctx context.Context, bi *botInstance, chatID string, tgChatID, tgUserID int64, text string) bool {
	if g.deps.BotOnboarding == nil || bi == nil || bi.id == uuid.Nil {
		return false
	}

	// In-process cache hit means the user already has identity in
	// vaelum.user_identities — UserState would not have been built
	// otherwise. Save the DB roundtrip and only intercept /start so
	// the welcome-back line fires.
	g.mu.Lock()
	us := g.users[chatID]
	g.mu.Unlock()
	if us != nil && us.UserID != uuid.Nil {
		if cmd, forUs := g.parseCommand(bi, text); cmd == "/start" && forUs {
			name := g.lookupDisplayName(ctx, us.UserID)
			if name == "" {
				name = "друг"
			}
			g.sendOnboardingText(ctx, bi, tgChatID, fmt.Sprintf(onbMsg.BackFmt, name))
			return true
		}
		return false
	}

	// Cold cache. Resolve identity via the same hook the gateway
	// would call inside getOrInitTelegramUser; on a hit we know this
	// is a paired user and only /start triggers welcome-back. On a
	// miss the user is fresh — proceed to FSM dispatch below.
	if g.deps.ResolveTelegramChat != nil {
		if uid, _, rerr := g.deps.ResolveTelegramChat(ctx, bi.id, tgChatID); rerr == nil && uid != uuid.Nil {
			if cmd, forUs := g.parseCommand(bi, text); cmd == "/start" && forUs {
				name := g.lookupDisplayName(ctx, uid)
				if name == "" {
					name = "друг"
				}
				g.sendOnboardingText(ctx, bi, tgChatID, fmt.Sprintf(onbMsg.BackFmt, name))
				return true
			}
			return false
		}
	}

	step, data, err := g.deps.BotOnboarding.GetState(ctx, tgUserID, bi.id)
	if err != nil {
		g.logger.Warn("gateway: onboarding GetState failed",
			"chat_id", chatID, "bot_id", bi.id.String(), "error", err)
		return false
	}

	cmd, forUs := g.parseCommand(bi, text)
	isStart := cmd == "/start" && forUs

	// No state row yet. Only /start kicks off the flow — random text
	// from an unknown chat still hits the standard unpaired-chat
	// policy (replyUnpaired), so users can't accidentally start
	// onboarding by typing "hi".
	if step == "" {
		if !isStart {
			return false
		}
		return g.onboardingStart(ctx, bi, tgChatID, tgUserID)
	}

	// /start mid-flow re-emits the current step without resetting.
	if isStart {
		return g.onboardingReissue(ctx, bi, tgChatID, step, data)
	}

	switch step {
	case onbStepAskName:
		return g.onboardingHandleName(ctx, bi, tgChatID, tgUserID, text)
	case onbStepAskSoulPres:
		// User typed instead of tapping. Re-emit the keyboard.
		return g.onboardingReissue(ctx, bi, tgChatID, step, data)
	default:
		g.logger.Warn("gateway: unknown onboarding step", "step", step, "chat_id", chatID)
		// Treat as a dead row — wipe and let next message restart.
		_ = g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id)
		return false
	}
}

// maybeRunBotOnboardingCallback handles a callback_query from an
// onboarding inline keyboard. Returns true when the callback was an
// onboarding event (caller stops); false leaves the callback for
// other handlers (legacy /model dispatch, etc).
func (g *Gateway) maybeRunBotOnboardingCallback(ctx context.Context, bi *botInstance, cq *telegram.CallbackQuery) bool {
	if g.deps.BotOnboarding == nil || bi == nil || bi.id == uuid.Nil || cq == nil {
		return false
	}
	if cq.From == nil || cq.Message == nil {
		return false
	}
	if !strings.HasPrefix(cq.Data, onbCallbackPrefix) {
		return false
	}

	tgUserID := cq.From.ID
	tgChatID := cq.Message.Chat.ID
	preset := strings.TrimPrefix(cq.Data, onbCallbackPrefix)
	if preset == "" {
		return true
	}

	step, data, err := g.deps.BotOnboarding.GetState(ctx, tgUserID, bi.id)
	if err != nil || step != onbStepAskSoulPres {
		// Stale button press; do nothing rather than confuse the user.
		g.logger.Debug("gateway: onboarding callback out of step",
			"step", step, "tg_user", tgUserID, "err", err)
		return true
	}

	name, _ := data[onbStateKeyUserName].(string)
	if name == "" {
		// Defensive: name was supposed to be saved at ask_name.
		// Restart the flow rather than create an unnamed soul.
		g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.Greeting)
		_ = g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskName, nil)
		return true
	}

	g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.WorkingFmt)

	// v1: "custom" falls back to arlene-style. TODO: when the custom
	// branch ships, intercept here, advance to an "ask_custom_traits"
	// step that asks 1-2 free-form questions, and only then
	// CreateAccount with a per-soul system prompt fragment.
	_, _, err = g.deps.BotOnboarding.CreateAccount(ctx, bs.BotOnboardingAccount{
		BotID:      bi.id,
		TGUserID:   tgUserID,
		TGChatID:   tgChatID,
		Name:       name,
		SoulPreset: presetFromCallback(preset),
	})
	if err != nil {
		g.logger.Error("gateway: onboarding CreateAccount failed",
			"tg_user", tgUserID, "bot_id", bi.id.String(), "name", name, "error", err)
		g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.ErrAccountFail)
		return true
	}

	if err := g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id); err != nil {
		g.logger.Warn("gateway: onboarding ClearState failed",
			"tg_user", tgUserID, "error", err)
		// Not fatal — the row is harmless once the user has identity.
	}

	// Drop any cached UserState built before the identity row landed,
	// so the next inbound message goes through getOrInitTelegramUser
	// and resolves the freshly-linked (user, soul).
	g.mu.Lock()
	delete(g.users, tgCanonical(tgChatID))
	g.mu.Unlock()

	g.sendOnboardingText(ctx, bi, tgChatID, fmt.Sprintf(onbMsg.DoneFmt, name))
	return true
}

func (g *Gateway) onboardingStart(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64) bool {
	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskName, nil); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(start) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.Greeting)
	return true
}

func (g *Gateway) onboardingHandleName(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, raw string) bool {
	name := strings.TrimSpace(raw)
	// Cap to 30 runes for sanity — same bound onboarding.usecase enforces.
	if rs := []rune(name); len(rs) > 30 {
		name = string(rs[:30])
	}
	if name == "" {
		// Empty / whitespace-only — re-prompt without advancing.
		g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.Greeting)
		return true
	}

	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskSoulPres, map[string]any{
		onbStateKeyUserName: name,
	}); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(ask_name) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingPresetPicker(ctx, bi, tgChatID, name)
	return true
}

func (g *Gateway) onboardingReissue(ctx context.Context, bi *botInstance, tgChatID int64, step string, data map[string]any) bool {
	switch step {
	case onbStepAskName:
		g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.Greeting)
	case onbStepAskSoulPres:
		name, _ := data[onbStateKeyUserName].(string)
		if name == "" {
			name = "друг"
		}
		g.sendOnboardingPresetPicker(ctx, bi, tgChatID, name)
	default:
		return false
	}
	return true
}

// sendOnboardingText is the bare-text helper. Uses SendMessage (not
// SendLong) because every onboarding line is short and we want
// deterministic message ids in case future logic needs to edit them.
func (g *Gateway) sendOnboardingText(ctx context.Context, bi *botInstance, tgChatID int64, text string) {
	if bi == nil || bi.client == nil {
		return
	}
	if _, err := bi.client.SendMessage(ctx, fmt.Sprintf("%d", tgChatID), text); err != nil {
		g.logger.Warn("gateway: onboarding send failed",
			"tg_chat", tgChatID, "error", err)
	}
}

// sendOnboardingPresetPicker emits the inline keyboard for step 2.
func (g *Gateway) sendOnboardingPresetPicker(ctx context.Context, bi *botInstance, tgChatID int64, name string) {
	if bi == nil || bi.client == nil {
		return
	}
	rows := [][]telegram.InlineKeyboardButton{
		{{Text: onbMsg.BtnArleneStyle, CallbackData: onbCallbackPrefix + onbPresetArleneTag}},
		{{Text: onbMsg.BtnCustomStyle, CallbackData: onbCallbackPrefix + onbPresetCustomTag}},
	}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, fmt.Sprintf(onbMsg.NamePromptFmt, name), rows); err != nil {
		g.logger.Warn("gateway: onboarding keyboard send failed",
			"tg_chat", tgChatID, "error", err)
	}
}

// lookupDisplayName resolves a user's preferred display name for the
// welcome-back line. Falls back to "" so the caller can pick a
// generic noun rather than the user_profiles raw chat_id.
func (g *Gateway) lookupDisplayName(ctx context.Context, userID uuid.UUID) string {
	db, err := g.deps.DB("ship")
	if err != nil {
		return ""
	}
	var name *string
	if err := db.GetContext(ctx, &name,
		`SELECT display_name FROM vaelum.memberships
		  WHERE user_id = $1 ORDER BY created_at LIMIT 1`, userID); err != nil {
		return ""
	}
	if name != nil && *name != "" {
		return *name
	}
	// Try vaelum.users.display_name as a fallback.
	var udn *string
	if err := db.GetContext(ctx, &udn,
		`SELECT display_name FROM vaelum.users WHERE id = $1`, userID); err == nil && udn != nil {
		return *udn
	}
	return ""
}

// presetFromCallback maps the short callback tag to the BotOnboardingAccount
// SoulPreset value. Unknown tags fall back to arlene_style so a stale
// keyboard button still produces a working soul.
func presetFromCallback(tag string) string {
	switch tag {
	case onbPresetArleneTag, onbPresetCustomTag:
		return "arlene_style" // v1: custom is a TODO; both seed the same persona
	default:
		return "arlene_style"
	}
}

