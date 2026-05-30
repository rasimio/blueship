package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// Inline Telegram-native onboarding FSM. Engages when an inbound
// message lands on a chat with no vaelum.user_identities row and the
// host has wired Deps.BotOnboarding. The flow mirrors the web wizard
// at vaelum-front/src/app/onboarding/page.tsx: name → voice → traits +
// description → confirm. The host's CompleteOnboarding hook reuses
// the same onboarding.UseCase.Complete the web endpoint invokes so
// bot-born and web-born tenants land in the same vaelum.souls row
// shape.
//
// Step names match the BotOnboarding contract:
//
//	"" / "start"      — entry; emit greeting + ask name; advance to ask_name
//	"ask_name"        — text reply is the soul's name; emit voice picker
//	                    (inline keyboard, callback_data="vc:<voice_id>");
//	                    advance to ask_voice
//	"ask_voice"       — callback_query carries the voice id; emit traits
//	                    picker (inline keyboard, callback_data="tr:<trait>"
//	                    or "traits_done"); advance to ask_traits
//	"ask_traits"      — toggle-edit callbacks update the same message's
//	                    keyboard in place. "traits_done" advances to
//	                    ask_description.
//	"ask_description" — free-form one-liner (or /skip). Advance to confirm
//	                    + emit summary + [Создать]/[Назад] keyboard.
//	"confirm"         — callback_query: confirm_ok → CompleteOnboarding +
//	                    clear state + emit done line; confirm_back →
//	                    re-emit description prompt with state preserved.
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
	// FSM step names. Persisted verbatim in
	// vaelum.bot_onboarding_state.step; the gateway dispatches on them.
	onbStepStart          = "start"
	onbStepAskName        = "ask_name"
	onbStepAskVoice       = "ask_voice"
	onbStepAskTraits      = "ask_traits"
	onbStepAskDescription = "ask_description"
	onbStepConfirm        = "confirm"

	// callback_data prefixes / tokens. Kept short — Telegram caps
	// callback_data at 64 bytes so "tr:mischievous" leaves plenty of
	// headroom for long trait names.
	onbCallbackVoice       = "vc:"          // vc:<voice_id>
	onbCallbackTrait       = "tr:"          // tr:<trait>
	onbCallbackTraitsDone  = "traits_done"  // bare token
	onbCallbackConfirmOK   = "confirm_ok"   // bare token
	onbCallbackConfirmBack = "confirm_back" // bare token

	// data blob keys (vaelum.bot_onboarding_state.data jsonb).
	onbDataName        = "name"
	onbDataVoice       = "voice_id"
	onbDataTags        = "tags"
	onbDataDescription = "description"
	onbDataTraitsMsgID = "traits_msg_id" // message_id of the live traits keyboard, for edit-in-place

	// Web-parity caps. Mirrors maxLength=30 on the name input and
	// maxLength=400 on the description textarea in
	// vaelum-front/src/app/onboarding/page.tsx, plus the
	// `prev.length < 5 ? […] : prev` toggle guard.
	onbMaxNameRunes   = 30
	onbMinNameRunes   = 2
	onbMaxDescription = 400
	onbMaxTags        = 5
)

// onbVoice is one row in the voice picker. Mirrors the VOICES export
// in vaelum-front/src/lib/persona.ts — id is the canonical token the
// web wizard sends to onboarding.UseCase.Complete, name + desc are the
// UI labels.
type onbVoice struct {
	ID   string
	Name string
	Desc string
}

// onbVoices is the canonical voice list (web parity). Order is the
// same as VOICES in persona.ts so a bot user and a web user see the
// same first/second/third option.
var onbVoices = []onbVoice{
	{ID: "clear", Name: "Clear", Desc: "Crisp and articulate."},
	{ID: "warm", Name: "Warm", Desc: "Gentle and close."},
	{ID: "quiet", Name: "Quiet", Desc: "Soft and unhurried."},
}

// onbTraits is the canonical 16-trait palette (web parity, same order
// as TRAITS in vaelum-front/src/lib/persona.ts). The bot picker shows
// these 2-per-row so the keyboard fits cleanly on mobile.
var onbTraits = []string{
	"thoughtful", "direct", "dry humor", "curious", "calm", "playful",
	"formal", "intuitive", "analytical", "warm", "bold", "patient",
	"poetic", "sharp", "grounded", "mischievous",
}

// onboardingMessages bundles the localised UI strings (Russian, short
// per the spec). Kept as a struct so a future locale switch is one
// data swap instead of inline edits.
type onboardingMessages struct {
	Greeting            string
	NamePromptFmt       string // %s = name (already validated)
	NameTooShort        string
	VoicePromptFmt      string // %s = name (used after voice pick)
	TraitsPrompt        string
	TraitsCounterFmt    string // %d = selected count
	DescriptionPrompt   string
	ConfirmTitle        string
	ConfirmRowFmt       string // %s = label, %s = value
	ConfirmTrueQ        string
	BtnConfirmOK        string
	BtnConfirmBack      string
	WorkingFmt          string // %s = name
	DoneFmt             string // %s = name
	BackFmt             string // %s = name (welcome-back greeting)
	ErrAccountFail      string
	ErrAlreadyOnboarded string
	DashEmpty           string // shown for empty tags / description
}

var onbMsg = onboardingMessages{
	Greeting:            "Привет, я Vaelum. Помогу тебе создать собственного ассистента. Как его(её) зовут?",
	NamePromptFmt:       "Приятно, %s. Выбери голос:",
	NameTooShort:        "Имя должно быть от 2 до 30 символов. Попробуй ещё раз:",
	VoicePromptFmt:      "Приятно, %s. Выбери голос:",
	TraitsPrompt:        "Выбери до 5 черт характера. Тапни чтобы выделить, тапни ещё раз чтобы снять. Когда готов(а) — нажми «Готово».",
	TraitsCounterFmt:    "Готово · %d из 5",
	DescriptionPrompt:   "Или опиши характер своими словами (одной строкой), либо отправь /skip чтобы пропустить.",
	ConfirmTitle:        "Проверим перед тем как создать:",
	ConfirmRowFmt:       "%s: %s",
	ConfirmTrueQ:        "Всё верно?",
	BtnConfirmOK:        "✓ Создать",
	BtnConfirmBack:      "← Назад",
	WorkingFmt:          "Готовлю...",
	DoneFmt:             "Готово, познакомься со своим %s. Напиши что-нибудь, и я отвечу.",
	BackFmt:             "С возвращением, %s!",
	ErrAccountFail:      "Что-то пошло не так при создании аккаунта. Попробуй ещё раз — нажми «✓ Создать».",
	ErrAlreadyOnboarded: "У тебя уже есть ассистент. Напиши что-нибудь, и я отвечу.",
	DashEmpty:           "—",
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
	isSkip := cmd == "/skip" && forUs

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
	case onbStepAskVoice, onbStepAskTraits, onbStepConfirm:
		// User typed instead of tapping a button. Re-emit the keyboard.
		return g.onboardingReissue(ctx, bi, tgChatID, step, data)
	case onbStepAskDescription:
		return g.onboardingHandleDescription(ctx, bi, tgChatID, tgUserID, text, data, isSkip)
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

	// Only recognise our prefixes / bare tokens. Anything else (legacy
	// model_role:* etc) falls through to the legacy handler.
	d := cq.Data
	isOurs := strings.HasPrefix(d, onbCallbackVoice) ||
		strings.HasPrefix(d, onbCallbackTrait) ||
		d == onbCallbackTraitsDone ||
		d == onbCallbackConfirmOK ||
		d == onbCallbackConfirmBack
	if !isOurs {
		return false
	}

	tgUserID := cq.From.ID
	tgChatID := cq.Message.Chat.ID
	messageID := cq.Message.MessageID

	step, data, err := g.deps.BotOnboarding.GetState(ctx, tgUserID, bi.id)
	if err != nil {
		g.logger.Warn("gateway: onboarding callback GetState failed",
			"tg_user", tgUserID, "error", err)
		return true
	}

	switch {
	case strings.HasPrefix(d, onbCallbackVoice):
		if step != onbStepAskVoice {
			return true // stale tap; do nothing
		}
		return g.onboardingHandleVoice(ctx, bi, tgChatID, tgUserID, strings.TrimPrefix(d, onbCallbackVoice), data)
	case strings.HasPrefix(d, onbCallbackTrait):
		if step != onbStepAskTraits {
			return true
		}
		return g.onboardingToggleTrait(ctx, bi, tgChatID, tgUserID, messageID, strings.TrimPrefix(d, onbCallbackTrait), data)
	case d == onbCallbackTraitsDone:
		if step != onbStepAskTraits {
			return true
		}
		return g.onboardingHandleTraitsDone(ctx, bi, tgChatID, tgUserID, data)
	case d == onbCallbackConfirmOK:
		if step != onbStepConfirm {
			return true
		}
		return g.onboardingFinalize(ctx, bi, tgChatID, tgUserID, data)
	case d == onbCallbackConfirmBack:
		if step != onbStepConfirm {
			return true
		}
		return g.onboardingConfirmBack(ctx, bi, tgChatID, tgUserID, data)
	}
	return true
}

// -- Step 1: entry / ask_name -------------------------------------------------

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
	// Web parity: 2-30 rune validation. Cap above the limit silently
	// so a too-long paste is rejected with the same hint as too-short.
	rs := []rune(name)
	if len(rs) < onbMinNameRunes || len(rs) > onbMaxNameRunes {
		g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.NameTooShort)
		return true
	}

	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskVoice, map[string]any{
		onbDataName: name,
	}); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(ask_name) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingVoicePicker(ctx, bi, tgChatID, name)
	return true
}

// -- Step 2: ask_voice --------------------------------------------------------

func (g *Gateway) onboardingHandleVoice(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, voiceID string, data map[string]any) bool {
	v := findVoice(voiceID)
	if v == nil {
		// Stale / unknown voice id. Re-emit the picker so the user can
		// pick a valid one without losing the name they typed.
		name, _ := data[onbDataName].(string)
		if name == "" {
			name = "друг"
		}
		g.sendOnboardingVoicePicker(ctx, bi, tgChatID, name)
		return true
	}

	data[onbDataVoice] = v.ID
	// Initialise empty tags so the traits picker has a slice to toggle
	// into. Storing as []any (not []string) so the json roundtrip
	// through bot_onboarding_state.data jsonb preserves the type.
	data[onbDataTags] = []any{}

	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskTraits, data); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(ask_voice) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingTraitsPicker(ctx, bi, tgChatID, tgUserID, nil)
	return true
}

// -- Step 3: ask_traits (toggle + done) ---------------------------------------

// onboardingToggleTrait flips one trait's selected state and edits the
// existing inline keyboard in place — no new message, no chat
// clutter. The selected-count cap (5) is enforced silently: a tap on a
// 6th unselected trait is a no-op (the button's render stays unticked
// so the user implicitly sees they hit the limit).
func (g *Gateway) onboardingToggleTrait(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, messageID int, trait string, data map[string]any) bool {
	if !isValidTrait(trait) {
		return true
	}
	tags := tagsFromData(data)

	idx := -1
	for i, t := range tags {
		if t == trait {
			idx = i
			break
		}
	}
	if idx >= 0 {
		// Toggle off.
		tags = append(tags[:idx], tags[idx+1:]...)
	} else {
		if len(tags) >= onbMaxTags {
			// Silent ignore — user sees the counter is already 5/5.
			return true
		}
		tags = append(tags, trait)
	}

	// Persist the new selection. We keep step=ask_traits; only
	// traits_done advances.
	tagsAny := make([]any, len(tags))
	for i, t := range tags {
		tagsAny[i] = t
	}
	data[onbDataTags] = tagsAny
	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskTraits, data); err != nil {
		g.logger.Warn("gateway: onboarding toggle persist failed",
			"tg_user", tgUserID, "error", err)
		return true
	}

	// Edit the keyboard in place. The trait labels and the
	// "Готово · N из 5" counter both re-render off the fresh tags
	// list, so a single edit keeps the message consistent.
	rows := buildTraitsKeyboard(tags)
	if err := bi.client.EditMessageReplyMarkup(ctx, tgChatID, messageID, rows); err != nil {
		// Telegram returns "message is not modified" when the keyboard
		// shape is identical — harmless for our case but log other
		// failures so we notice if the API contract drifts.
		g.logger.Debug("gateway: onboarding trait edit reply markup failed",
			"tg_user", tgUserID, "error", err)
	}
	return true
}

func (g *Gateway) onboardingHandleTraitsDone(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, data map[string]any) bool {
	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskDescription, data); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(traits_done) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.DescriptionPrompt)
	return true
}

// -- Step 4: ask_description --------------------------------------------------

func (g *Gateway) onboardingHandleDescription(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, raw string, data map[string]any, isSkip bool) bool {
	var desc string
	if !isSkip {
		desc = strings.TrimSpace(raw)
		// Web-parity 400-char cap (rune count, not bytes). A too-long
		// input is silently truncated — same shape as the web textarea
		// maxLength=400 which simply refuses extra keystrokes.
		if rs := []rune(desc); len(rs) > onbMaxDescription {
			desc = string(rs[:onbMaxDescription])
		}
	}
	data[onbDataDescription] = desc

	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepConfirm, data); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(ask_description) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingConfirm(ctx, bi, tgChatID, data)
	return true
}

// -- Step 5: confirm ----------------------------------------------------------

func (g *Gateway) onboardingFinalize(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, data map[string]any) bool {
	name, _ := data[onbDataName].(string)
	voiceID, _ := data[onbDataVoice].(string)
	tags := tagsFromData(data)
	desc, _ := data[onbDataDescription].(string)

	if name == "" || voiceID == "" {
		// Defensive: state somehow lacks the required fields. Restart
		// the flow rather than minting a broken soul.
		g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.Greeting)
		_ = g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskName, nil)
		return true
	}

	g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.WorkingFmt)

	_, _, err := g.deps.BotOnboarding.CompleteOnboarding(ctx, bs.BotOnboardingComplete{
		BotID:                bi.id,
		TGUserID:             tgUserID,
		TGChatID:             tgChatID,
		Name:                 name,
		VoiceID:              voiceID,
		CharacterTags:        tags,
		CharacterDescription: desc,
	})
	if err != nil {
		// Already onboarded gets a specific terminal line (user's
		// account exists; nothing to retry). Everything else stays at
		// step=confirm so the user can tap "Создать" again after we
		// fix whatever blew up server-side.
		if errors.Is(err, bs.ErrBotOnboardingAlreadyDone) {
			_ = g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id)
			g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.ErrAlreadyOnboarded)
			return true
		}
		g.logger.Error("gateway: onboarding CompleteOnboarding failed",
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

func (g *Gateway) onboardingConfirmBack(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, data map[string]any) bool {
	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskDescription, data); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(confirm_back) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.DescriptionPrompt)
	return true
}

// -- /start mid-flow ----------------------------------------------------------

func (g *Gateway) onboardingReissue(ctx context.Context, bi *botInstance, tgChatID int64, step string, data map[string]any) bool {
	switch step {
	case onbStepAskName:
		g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.Greeting)
	case onbStepAskVoice:
		name, _ := data[onbDataName].(string)
		if name == "" {
			name = "друг"
		}
		g.sendOnboardingVoicePicker(ctx, bi, tgChatID, name)
	case onbStepAskTraits:
		tags := tagsFromData(data)
		g.sendOnboardingTraitsPicker(ctx, bi, tgChatID, 0, tags)
	case onbStepAskDescription:
		g.sendOnboardingText(ctx, bi, tgChatID, onbMsg.DescriptionPrompt)
	case onbStepConfirm:
		g.sendOnboardingConfirm(ctx, bi, tgChatID, data)
	default:
		return false
	}
	return true
}

// -- senders ------------------------------------------------------------------

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

// sendOnboardingVoicePicker emits the inline keyboard for step 2.
// One button per voice, one row each, label = "Name — desc".
func (g *Gateway) sendOnboardingVoicePicker(ctx context.Context, bi *botInstance, tgChatID int64, name string) {
	if bi == nil || bi.client == nil {
		return
	}
	rows := make([][]telegram.InlineKeyboardButton, 0, len(onbVoices))
	for _, v := range onbVoices {
		rows = append(rows, []telegram.InlineKeyboardButton{
			{
				Text:         fmt.Sprintf("%s — %s", v.Name, v.Desc),
				CallbackData: onbCallbackVoice + v.ID,
			},
		})
	}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, fmt.Sprintf(onbMsg.VoicePromptFmt, name), rows); err != nil {
		g.logger.Warn("gateway: onboarding voice keyboard send failed",
			"tg_chat", tgChatID, "error", err)
	}
}

// sendOnboardingTraitsPicker emits the traits keyboard for step 3 as
// a NEW message and stashes the message_id in the FSM state so
// subsequent toggle taps can edit the same keyboard in place via
// EditMessageReplyMarkup. Called from two sites: ask_voice → ask_traits
// transition (initial paint, empty selection), and the /start re-issue
// path (post-restart, may have a pre-existing selection).
func (g *Gateway) sendOnboardingTraitsPicker(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, selected []string) {
	if bi == nil || bi.client == nil {
		return
	}
	rows := buildTraitsKeyboard(selected)
	res, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, onbMsg.TraitsPrompt, rows)
	if err != nil || res == nil {
		g.logger.Warn("gateway: onboarding traits keyboard send failed",
			"tg_chat", tgChatID, "error", err)
		return
	}
	// Stash the message id on the FSM state so onboardingToggleTrait
	// (which receives the message_id from cq.Message anyway) doesn't
	// need this — but the reissue path benefits because it re-emits a
	// fresh keyboard and discards the old one.
	if tgUserID != 0 && bi.id != uuid.Nil && res.Result.MessageID > 0 {
		step, data, gerr := g.deps.BotOnboarding.GetState(ctx, tgUserID, bi.id)
		if gerr == nil && step == onbStepAskTraits {
			if data == nil {
				data = map[string]any{}
			}
			data[onbDataTraitsMsgID] = res.Result.MessageID
			_ = g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskTraits, data)
		}
	}
}

// sendOnboardingConfirm emits the summary + [✓ Создать] / [← Назад]
// keyboard. The summary mirrors the web wizard's confirm card (Name /
// Voice / Character / Description rows, "—" placeholder when empty).
func (g *Gateway) sendOnboardingConfirm(ctx context.Context, bi *botInstance, tgChatID int64, data map[string]any) {
	if bi == nil || bi.client == nil {
		return
	}
	name, _ := data[onbDataName].(string)
	voiceID, _ := data[onbDataVoice].(string)
	tags := tagsFromData(data)
	desc, _ := data[onbDataDescription].(string)

	voiceName := onbMsg.DashEmpty
	if v := findVoice(voiceID); v != nil {
		voiceName = v.Name
	}
	tagsStr := onbMsg.DashEmpty
	if len(tags) > 0 {
		tagsStr = strings.Join(tags, ", ")
	}
	descStr := desc
	if strings.TrimSpace(descStr) == "" {
		descStr = onbMsg.DashEmpty
	}

	var b strings.Builder
	b.WriteString(onbMsg.ConfirmTitle)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, onbMsg.ConfirmRowFmt+"\n", "Имя", name)
	fmt.Fprintf(&b, onbMsg.ConfirmRowFmt+"\n", "Голос", voiceName)
	fmt.Fprintf(&b, onbMsg.ConfirmRowFmt+"\n", "Черты", tagsStr)
	fmt.Fprintf(&b, onbMsg.ConfirmRowFmt+"\n", "Описание", descStr)
	b.WriteString("\n")
	b.WriteString(onbMsg.ConfirmTrueQ)

	rows := [][]telegram.InlineKeyboardButton{
		{
			{Text: onbMsg.BtnConfirmOK, CallbackData: onbCallbackConfirmOK},
			{Text: onbMsg.BtnConfirmBack, CallbackData: onbCallbackConfirmBack},
		},
	}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, b.String(), rows); err != nil {
		g.logger.Warn("gateway: onboarding confirm send failed",
			"tg_chat", tgChatID, "error", err)
	}
}

// -- helpers ------------------------------------------------------------------

// buildTraitsKeyboard renders the 16-trait grid as a 2-per-row keyboard
// plus a trailing "Готово · N из 5" row. Selected traits get a `[✓]`
// prefix, unselected `[ ]`. Order matches onbTraits (web parity).
func buildTraitsKeyboard(selected []string) [][]telegram.InlineKeyboardButton {
	selSet := make(map[string]struct{}, len(selected))
	for _, t := range selected {
		selSet[t] = struct{}{}
	}
	rows := make([][]telegram.InlineKeyboardButton, 0, len(onbTraits)/2+1)
	for i := 0; i < len(onbTraits); i += 2 {
		row := []telegram.InlineKeyboardButton{
			{Text: traitLabel(onbTraits[i], selSet), CallbackData: onbCallbackTrait + onbTraits[i]},
		}
		if i+1 < len(onbTraits) {
			row = append(row, telegram.InlineKeyboardButton{
				Text:         traitLabel(onbTraits[i+1], selSet),
				CallbackData: onbCallbackTrait + onbTraits[i+1],
			})
		}
		rows = append(rows, row)
	}
	rows = append(rows, []telegram.InlineKeyboardButton{
		{
			Text:         fmt.Sprintf(onbMsg.TraitsCounterFmt, len(selected)),
			CallbackData: onbCallbackTraitsDone,
		},
	})
	return rows
}

func traitLabel(t string, selSet map[string]struct{}) string {
	if _, ok := selSet[t]; ok {
		return "[✓] " + t
	}
	return "[ ] " + t
}

func findVoice(id string) *onbVoice {
	for i := range onbVoices {
		if onbVoices[i].ID == id {
			return &onbVoices[i]
		}
	}
	return nil
}

func isValidTrait(t string) bool {
	for _, v := range onbTraits {
		if v == t {
			return true
		}
	}
	return false
}

// tagsFromData extracts the persisted tags slice from the jsonb data
// blob. jsonb roundtrips slices as []any, so we coerce each element to
// string and drop anything that isn't (defensive — we always write
// []any of strings, but a malformed/legacy row shouldn't crash).
func tagsFromData(data map[string]any) []string {
	raw, ok := data[onbDataTags]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
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
