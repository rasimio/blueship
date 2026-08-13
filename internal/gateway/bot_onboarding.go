package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// Inline Telegram-native onboarding FSM. Engages when an inbound
// message lands on a chat the host cannot resolve to an identity and
// Deps.BotOnboarding is wired. Five steps: name → voice → traits →
// description → confirm, with the persona vocabulary supplied by the
// host through GatewayConfig.OnboardingFlow — the framework ships none
// of its own, because the ids it collects are persisted on the host's
// rows and read back by the host's other surfaces.
//
// Instant mode (OnboardingFlow.Mode) skips the questions entirely and
// mints the account from the flow's default persona, leaving the same
// five steps reachable later through /persona.
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
//	                    + emit summary + [Create]/[Back] keyboard.
//	"confirm"         — callback_query: confirm_ok → CompleteOnboarding +
//	                    clear state + emit done line; confirm_back →
//	                    re-emit description prompt with state preserved.
//
// Idempotency: any /start re-issued mid-flow re-emits the current
// step's prompt without touching state — the user can always recover
// by typing /start again.
//
// No user-facing string is written in this file. Every line the user
// reads comes from GatewayConfig.Onboarding (copy) or OnboardingFlow
// (palette and opening moves), both host-supplied — the framework has
// no language of its own.

const (
	// FSM step names, persisted verbatim by the host's GetState /
	// AdvanceStep implementation; the gateway dispatches on them.
	onbStepStart          = "start"
	onbStepAskName        = "ask_name"
	onbStepAskVoice       = "ask_voice"
	onbStepAskTraits      = "ask_traits"
	onbStepAskDescription = "ask_description"
	onbStepConfirm        = "confirm"

	// onbStepOfferPending parks an instant-mode account between signup
	// and the deferred persona offer. It is the one step that belongs to
	// an already-onboarded user, and the one step that does not consume
	// the message it sees — the row exists only to carry a turn count.
	onbStepOfferPending = "offer_pending"

	// onbStepAwaitPrefix parks a chat while a host command waits for one
	// answer. The suffix is the command the answer is delivered to, so the
	// gateway needs no table of who asked what.
	onbStepAwaitPrefix = "await:"

	// callback_data prefixes / tokens. Kept short — Telegram caps
	// callback_data at 64 bytes so "tr:mischievous" leaves plenty of
	// headroom for long trait names.
	onbCallbackVoice       = "vc:"          // vc:<voice_id>
	onbCallbackTrait       = "tr:"          // tr:<trait>
	onbCallbackTraitsDone  = "traits_done"  // bare token
	onbCallbackConfirmOK   = "confirm_ok"   // bare token
	onbCallbackConfirmBack = "confirm_back" // bare token
	onbCallbackSeed        = "sd:"          // sd:<index into OnboardingFlow.SeedButtons>
	onbCallbackOfferSetup  = "pn_setup"     // bare token
	onbCallbackOfferLater  = "pn_later"     // bare token
	onbCallbackHostCmd     = "hc:"          // hc:<host command name>

	// data blob keys, round-tripped through the host state store as JSON.
	onbDataName        = "name"
	onbDataVoice       = "voice_id"
	onbDataTags        = "tags"
	onbDataDescription = "description"
	onbDataTraitsMsgID = "traits_msg_id" // message_id of the live traits keyboard, for edit-in-place
	onbDataEdit        = "edit"          // true when the run updates an existing soul instead of creating one
	onbDataTurns       = "turns"         // exchanges counted while step=offer_pending
	onbDataSource      = "source"        // deep-link payload the /start that opened the flow carried

	// onbMaxSourceRunes bounds the recorded deep-link payload. Telegram
	// caps its own at 64; anything longer was typed by hand and is not a
	// campaign token.
	onbMaxSourceRunes = 64
)

// onbVoice aliases the host-supplied palette entry so the picker code
// below reads without a package qualifier on every line.
type onbVoice = bs.OnboardingVoice

// voices and traits read the palette off the configured flow. It is the
// host's vocabulary, in the host's language — the framework ships none.
func (g *Gateway) voices() []onbVoice           { return g.flow().Voices }
func (g *Gateway) traits() []bs.OnboardingTrait { return g.flow().Traits }

// voiceName is the short label, used on the confirm row where the
// description would be noise.
func (g *Gateway) voiceName(v onbVoice) string { return v.Name }

// voiceButton renders the picker button: "<name> — <description>", or
// just the name when the host supplied no description.
func (g *Gateway) voiceButton(v onbVoice) string {
	if v.Desc == "" {
		return v.Name
	}
	return fmt.Sprintf("%s — %s", v.Name, v.Desc)
}

// traitText renders one trait id as the host worded it.
func (g *Gateway) traitText(id string) string { return g.flow().TraitLabel(id) }

// traitTexts maps a selection of trait ids to their labels, preserving
// order. The confirm row shows what the user picked, not the ids the
// persona row stores.
func (g *Gateway) traitTexts(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, g.traitText(id))
	}
	return out
}

// onb returns the host-configured onboarding copy (generic English defaults
// filled by Config.ApplyDefaults; a host overrides via GatewayConfig.Onboarding).
func (g *Gateway) onb() bs.OnboardingMessages { return g.deps.Config.Gateway.Onboarding }

// flow returns the host-configured onboarding shape. Validated at startup
// (Ship.Run), so instant mode is known good by the time anything reads it.
func (g *Gateway) flow() bs.OnboardingFlow { return g.deps.Config.Gateway.OnboardingFlow }

// cachedUserState returns the in-process state for a (bot, chat), or nil.
func (g *Gateway) cachedUserState(botID uuid.UUID, chatID string) *UserState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if us := g.users[telegramUserCacheKey(botID, chatID)]; us != nil {
		return us
	}
	return g.users[chatID]
}

// invalidateTelegramUser drops any cached UserState for a (bot, chat) so
// the next inbound rebuilds it from the database.
//
// Onboarding is the one place this matters: the cached state was built —
// or refused — while the chat had no identity, and everything downstream
// keys off the (user, soul) it carries.
func (g *Gateway) invalidateTelegramUser(botID uuid.UUID, chatID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Both key shapes: the multi-bot path stores under the composite key,
	// the legacy single-bot path under the bare canonical chat id.
	delete(g.users, telegramUserCacheKey(botID, chatID))
	delete(g.users, chatID)
}

// maybeRunBotOnboarding intercepts inbound text that belongs to the
// signup / persona FSM. Returns true when the message has been handled
// (caller stops processing); false means the caller continues down the
// normal getOrInitTelegramUser path.
//
// Onboarded users are no longer short-circuited before the state read.
// They were while the FSM only ever created accounts — nothing it owned
// could apply to someone who already had one. Persona editing changes
// that: /persona and the deferred setup offer are both aimed at users who
// are already onboarded, so their state has to be consulted too.
//
// That costs one primary-key lookup per inbound message on a table which
// is empty for everyone except users with an edit or an offer in flight.
// Cheap, and the alternative — an in-memory "is an edit running" hint —
// loses the flag on restart and strands whoever was mid-wizard.
// tgSender is who sent the update, as far as an account cares: the parts of
// Telegram's `from` that describe the person rather than the message. Passed
// as one value because these travel together through every onboarding path,
// and threading them as loose strings turns a seven-argument function into a
// nine-argument one every time the transport starts carrying something new.
type tgSender struct {
	Name     string // from.first_name
	Handle   string // from.username, no leading "@"; empty is normal
	Language string // from.language_code
}

func (g *Gateway) maybeRunBotOnboarding(ctx context.Context, bi *botInstance, chatID string, tgChatID, tgUserID int64, text string, sender tgSender) bool {
	if g.deps.BotOnboarding == nil || bi == nil || bi.id == uuid.Nil {
		return false
	}

	cmd, forUs := g.parseCommand(bi, text)
	isStart := cmd == "/start" && forUs
	isSkip := cmd == "/skip" && forUs
	isPersona := cmd == "/persona" && forUs

	// Identity resolution, cache first. A cache hit means the user has an
	// identity row — UserState would not have been built otherwise.
	g.mu.Lock()
	us := g.users[telegramUserCacheKey(bi.id, chatID)]
	g.mu.Unlock()
	userID := uuid.Nil
	if us != nil {
		userID = us.UserID
	}
	if userID == uuid.Nil && g.deps.ResolveTelegramChat != nil {
		if uid, _, rerr := g.deps.ResolveTelegramChat(ctx, bi.id, tgChatID); rerr == nil {
			userID = uid
		}
	}
	onboarded := userID != uuid.Nil

	step, data, err := g.deps.BotOnboarding.GetState(ctx, tgUserID, bi.id)
	if err != nil {
		g.logger.Warn("gateway: onboarding GetState failed",
			"chat_id", chatID, "bot_id", bi.id.String(), "error", err)
		return false
	}

	if onboarded {
		// /start on a known chat is a greeting, never a re-signup —
		// even mid-edit, where re-emitting the step below would be the
		// wrong answer to "hello".
		if isStart {
			name := g.lookupDisplayName(ctx, userID)
			if name == "" {
				name = g.onb().FallbackName
			}
			g.sendOnboardingText(ctx, bi, tgChatID, fmt.Sprintf(g.onb().BackFmt, name))
			return true
		}
		if isPersona {
			return g.onboardingStartEdit(ctx, bi, tgChatID, tgUserID)
		}
		// A host command asked one question and this is the answer.
		// Cleared before dispatch, so a handler that asks again parks a
		// fresh row and a failure cannot leave the chat waiting forever.
		if strings.HasPrefix(step, onbStepAwaitPrefix) {
			awaited := strings.TrimPrefix(step, onbStepAwaitPrefix)
			if err := g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id); err != nil {
				g.logger.Warn("gateway: clearing an awaited reply failed",
					"command", awaited, "error", err)
			}
			if g.deps.CommandHandler == nil {
				return false
			}
			us := g.cachedUserState(bi.id, chatID)
			if us == nil {
				return false
			}
			g.runHostCommand(ctx, bi, tgChatID, tgUserID, us, awaited, strings.TrimSpace(text))
			return true
		}
		// The offer is a nudge alongside the conversation, not instead
		// of it: it never consumes the message that triggered it.
		if step == onbStepOfferPending {
			g.onboardingTickOffer(ctx, bi, tgChatID, tgUserID, data)
			return false
		}
		// Anything else with no FSM row is an ordinary message.
		if step == "" {
			return false
		}
		// Otherwise fall through: an edit is in flight and these are
		// its answers.
	}

	// A user with no identity carrying an FSM row, while the flow is
	// instant, is holding a half-finished wizard from a mode that no
	// longer runs. Discard it and sign them up the way everyone else is
	// signed up — resuming a five-step form they abandoned, on a
	// deployment that has since decided not to ask those questions at
	// all, would strand them somewhere nobody else can reach.
	//
	// Not reachable for someone who already has an assistant — their
	// only reason to be holding a row is an edit in flight, which fell
	// through to here on purpose and must survive.
	//
	// The comment used to say this was unreachable for them and the
	// condition did not enforce it, so /persona asked for a name, threw
	// the run away when the name arrived, and answered "you already have
	// an assistant" — the one sentence guaranteed to be useless to
	// somebody who is trying to rename theirs.
	if !onboarded && step != "" && g.flow().Mode == bs.OnboardingModeInstant {
		g.logger.Info("gateway: discarding wizard state left over from a previous flow",
			"tg_user", tgUserID, "step", step)
		if err := g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id); err != nil {
			g.logger.Warn("gateway: clearing stale onboarding state failed",
				"tg_user", tgUserID, "error", err)
			return false
		}
		step, data = "", nil
	}

	// No state row yet.
	if step == "" {
		if g.flow().Mode == bs.OnboardingModeInstant {
			return g.onboardingInstant(ctx, bi, chatID, tgChatID, tgUserID, sender, g.signupSource(bi, text), isStart)
		}
		// Wizard: only /start kicks off the flow — random text from an
		// unknown chat still hits the standard unpaired-chat policy
		// (replyUnpaired), so users can't accidentally start onboarding
		// by typing "hi".
		if !isStart {
			return false
		}
		return g.onboardingStart(ctx, bi, tgChatID, tgUserID, g.signupSource(bi, text))
	}

	// /start mid-flow re-emits the current step without resetting.
	if isStart {
		return g.onboardingReissue(ctx, bi, tgChatID, step, data)
	}

	switch step {
	case onbStepAskName:
		return g.onboardingHandleName(ctx, bi, tgChatID, tgUserID, text, data)
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
		strings.HasPrefix(d, onbCallbackSeed) ||
		strings.HasPrefix(d, onbCallbackHostCmd) ||
		d == onbCallbackTraitsDone ||
		d == onbCallbackConfirmOK ||
		d == onbCallbackConfirmBack ||
		d == onbCallbackOfferSetup ||
		d == onbCallbackOfferLater
	if !isOurs {
		return false
	}

	// Seed and offer buttons belong to an account that already exists, so
	// they are dispatched before the FSM state lookup below — that lookup
	// returns step="" for an onboarded user and tells us nothing.
	switch {
	case strings.HasPrefix(d, onbCallbackSeed):
		return g.onboardingHandleSeed(ctx, bi, cq, strings.TrimPrefix(d, onbCallbackSeed))
	case d == onbCallbackOfferSetup:
		g.stripKeyboard(ctx, bi, cq)
		return g.onboardingStartEdit(ctx, bi, cq.Message.Chat.ID, cq.From.ID)
	case strings.HasPrefix(d, onbCallbackHostCmd):
		g.stripKeyboard(ctx, bi, cq)
		name := strings.TrimPrefix(d, onbCallbackHostCmd)
		if g.deps.CommandHandler == nil {
			return true
		}
		us := g.cachedUserState(bi.id, tgCanonical(cq.Message.Chat.ID))
		if us == nil {
			// A tap from a chat whose state was dropped — a restart, or an
			// old message. There is nobody to attribute the command to,
			// and acting as the wrong user is worse than silence.
			g.logger.Info("gateway: host-command button from an unresolved chat",
				"command", name, "tg_user", cq.From.ID)
			return true
		}
		g.runHostCommand(ctx, bi, cq.Message.Chat.ID, cq.From.ID, us, name, "")
		return true
	case d == onbCallbackOfferLater:
		g.stripKeyboard(ctx, bi, cq)
		g.sendOnboardingText(ctx, bi, cq.Message.Chat.ID, g.onb().OfferLater)
		return true
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
		return g.onboardingFinalize(ctx, bi, tgChatID, tgUserID, data, tgSender{Name: cq.From.FirstName, Handle: cq.From.Username, Language: cq.From.LanguageCode})
	case d == onbCallbackConfirmBack:
		if step != onbStepConfirm {
			return true
		}
		return g.onboardingConfirmBack(ctx, bi, tgChatID, tgUserID, data)
	}
	return true
}

// -- Instant mode -------------------------------------------------------------

// onboardingInstant mints the account on first contact with the flow's
// default persona and returns whether it consumed the inbound message.
//
// greet distinguishes the two ways a stranger arrives, and they deserve
// different endings:
//
//   - /start (an ad deep link, the Telegram "Start" button): there is no
//     message to answer, so we consume it and open with the welcome plus
//     its seed buttons. Consuming is what stops the assistant from being
//     asked to reply to the literal string "/start".
//   - anything else ("привет", or a real question): the person already
//     said something, and answering it is a better first impression than
//     any welcome copy. We create the account and return false so the
//     message they actually wrote falls through and becomes turn one.
//
// The account row is committed before we return, so the fall-through
// path's getOrInitTelegramUser resolves the fresh identity rather than
// racing it.
func (g *Gateway) onboardingInstant(
	ctx context.Context,
	bi *botInstance,
	chatID string,
	tgChatID, tgUserID int64,
	sender tgSender,
	source string,
	greet bool,
) bool {
	flow := g.flow()

	// A payload that names a host command is an errand, not an
	// acquisition channel: someone was sent here to finish something the
	// other chat could not do. Recording "plus" as where a customer came
	// from would quietly corrupt the one field that has to stay a small
	// closed set to be worth counting.
	errand, isErrand := g.startPayloadCommand("/start " + source)
	if isErrand {
		source = ""
	}

	userID, soulID, err := g.deps.BotOnboarding.CompleteOnboarding(ctx, bs.BotOnboardingComplete{
		BotID:           bi.id,
		TGUserID:        tgUserID,
		TGChatID:        tgChatID,
		Name:            flow.DefaultName,
		UserDisplayName: sender.Name,
		UserHandle:      sender.Handle,
		UserLanguage:    sender.Language,
		SignupSource:    source,
		VoiceID:         flow.DefaultVoice,
		CharacterTags:   flow.DefaultTags,
	})
	switch {
	case errors.Is(err, bs.ErrBotOnboardingAlreadyDone):
		// The person already owns a soul; nothing was created. Whether
		// this chat now reaches it is the host's business — a host that
		// links it says so by returning the ids, and one that refuses
		// returns none.
		g.logger.Info("gateway: instant onboarding skipped, identity already has a soul",
			"tg_user", tgUserID, "bot_id", bi.id.String(), "linked", userID != uuid.Nil)
		g.invalidateTelegramUser(bi.id, chatID)
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().ErrAlreadyOnboarded)
		// Someone who arrived on an errand came to do a thing, not to be
		// told they have an account. Run it, now that we know who they
		// are — this is the whole point of the link they followed.
		if isErrand && userID != uuid.Nil {
			g.runHostCommand(ctx, bi, tgChatID, tgUserID,
				&UserState{UserID: userID, SoulID: soulID}, errand, "")
		}
		return true
	case err != nil:
		g.logger.Error("gateway: instant onboarding failed",
			"tg_user", tgUserID, "bot_id", bi.id.String(), "error", err)
		if greet {
			g.sendOnboardingText(ctx, bi, tgChatID, g.onb().ErrAccountFail)
			return true
		}
		// Fall through to the unpaired-chat policy rather than swallow
		// the message: the person gets *some* reply either way.
		return false
	}

	g.logger.Info("gateway: instant onboarding created account",
		"tg_user", tgUserID, "bot_id", bi.id.String())
	g.invalidateTelegramUser(bi.id, chatID)

	// Run the errand the link was for, once the account it needs exists.
	// Deferred to the end so the greeting lands first: arriving mid-task
	// still starts with being told where you are.
	if isErrand && userID != uuid.Nil {
		defer g.runHostCommand(ctx, bi, tgChatID, tgUserID,
			&UserState{UserID: userID, SoulID: soulID}, errand, "")
	}

	// Arm the deferred persona offer. Only instant accounts get one:
	// a wizard user chose their persona deliberately a moment ago and
	// does not need to be asked again.
	if g.flow().OfferPersonaAfterTurns > 0 && g.deps.PersonaEditor != nil {
		if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepOfferPending, map[string]any{
			onbDataTurns: 0,
		}); err != nil {
			// The account is live either way; losing the nudge is not
			// worth failing the signup over.
			g.logger.Warn("gateway: arming persona offer failed", "tg_user", tgUserID, "error", err)
		}
	}

	if !greet {
		return false
	}
	g.sendOnboardingWelcome(ctx, bi, tgChatID)
	return true
}

// sendOnboardingWelcome opens an instant-mode chat: one message, plus the
// seed buttons when the host configured any. One button per row — these
// are sentences, not labels, and two per row truncates them on a phone.
func (g *Gateway) sendOnboardingWelcome(ctx context.Context, bi *botInstance, tgChatID int64) {
	if bi == nil || bi.client == nil {
		return
	}
	flow := g.flow()
	if len(flow.SeedButtons) == 0 {
		g.sendOnboardingText(ctx, bi, tgChatID, flow.Welcome)
		return
	}
	rows := make([][]telegram.InlineKeyboardButton, 0, len(flow.SeedButtons))
	for i, b := range flow.SeedButtons {
		rows = append(rows, []telegram.InlineKeyboardButton{{
			Text:         b.Label,
			CallbackData: fmt.Sprintf("%s%d", onbCallbackSeed, i),
		}})
	}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, flow.Welcome, rows); err != nil {
		g.logger.Warn("gateway: onboarding welcome send failed",
			"tg_chat", tgChatID, "error", err)
	}
}

// stripKeyboard removes the inline keyboard from the message a callback
// came from, so a one-shot prompt reads as spent and a second tap cannot
// queue a duplicate action. Best effort: Telegram answers "message is not
// modified" when there was nothing to remove.
func (g *Gateway) stripKeyboard(ctx context.Context, bi *botInstance, cq *telegram.CallbackQuery) {
	if bi == nil || bi.client == nil || cq == nil || cq.Message == nil {
		return
	}
	if err := bi.client.EditMessageReplyMarkup(ctx, cq.Message.Chat.ID, cq.Message.MessageID, nil); err != nil {
		g.logger.Debug("gateway: keyboard strip failed", "error", err)
	}
}

// onboardingHandleSeed turns a seed-button tap into an ordinary inbound
// message. The button's configured text is dispatched as though the
// person had typed it, so the reply is a real turn — same session, same
// memory, same tools — rather than a scripted answer that would stop
// being true the moment the product changed.
//
// The keyboard is stripped first so the welcome message reads as spent
// and a second tap cannot queue a duplicate turn.
func (g *Gateway) onboardingHandleSeed(ctx context.Context, bi *botInstance, cq *telegram.CallbackQuery, idxRaw string) bool {
	flow := g.flow()
	idx, err := strconv.Atoi(idxRaw)
	if err != nil || idx < 0 || idx >= len(flow.SeedButtons) {
		// Stale keyboard from before a config change. Silently done —
		// the tap was already acknowledged by handleUpdate.
		g.logger.Debug("gateway: seed tap out of range", "index", idxRaw, "configured", len(flow.SeedButtons))
		return true
	}

	g.stripKeyboard(ctx, bi, cq)

	g.handleUpdate(ctx, bi, telegram.Update{Message: &telegram.Message{
		MessageID: cq.Message.MessageID,
		From:      cq.From,
		Chat:      telegram.Chat{ID: cq.Message.Chat.ID, Type: "private"},
		Text:      flow.SeedButtons[idx].Text,
	}})
	return true
}

// -- Persona editing ----------------------------------------------------------

// onboardingStartEdit re-runs the persona wizard against a soul that
// already exists. Same five steps as signup — only the terminal action
// differs — so the FSM row is seeded with the same shape plus an edit
// marker, and every step handler below is reused verbatim.
//
// Any pending setup offer is superseded: the user is doing the thing the
// offer would have asked them to do.
func (g *Gateway) onboardingStartEdit(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64) bool {
	if g.deps.PersonaEditor == nil {
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().ErrNoPersona)
		return true
	}
	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskName, map[string]any{
		onbDataEdit: true,
	}); err != nil {
		g.logger.Warn("gateway: persona edit AdvanceStep failed", "tg_user", tgUserID, "error", err)
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().ErrPersonaFail)
		return true
	}
	g.sendOnboardingText(ctx, bi, tgChatID, g.onb().EditGreeting)
	return true
}

// onboardingTickOffer counts an exchange against the threshold and, on
// reaching it, sends the persona offer and retires the row.
//
// It never returns a "handled" signal — the message that tripped the
// counter is a real message and still gets a real answer. The offer
// arrives alongside it.
//
// State is a persisted counter rather than an in-process one because the
// interesting threshold is measured in conversation, which outlives any
// single daemon lifetime. The row is cleared once the offer fires, so the
// per-message write costs at most OfferPersonaAfterTurns upserts across a
// user's entire life.
func (g *Gateway) onboardingTickOffer(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, data map[string]any) {
	after := g.flow().OfferPersonaAfterTurns
	if after <= 0 || g.deps.PersonaEditor == nil {
		// Offer disabled or uneditable — retire the row so we stop
		// reading it on every message.
		_ = g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id)
		return
	}

	turns := turnsFromData(data) + 1
	if turns < after {
		if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepOfferPending, map[string]any{
			onbDataTurns: turns,
		}); err != nil {
			g.logger.Warn("gateway: persona offer tick failed", "tg_user", tgUserID, "error", err)
		}
		return
	}

	// Retire the row before sending: a failed send costs one missed
	// nudge, whereas a row that survives a successful send would re-offer
	// on every subsequent message.
	if err := g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id); err != nil {
		g.logger.Warn("gateway: persona offer ClearState failed", "tg_user", tgUserID, "error", err)
		return
	}
	rows := [][]telegram.InlineKeyboardButton{{
		{Text: g.onb().BtnOfferSetup, CallbackData: onbCallbackOfferSetup},
		{Text: g.onb().BtnOfferLater, CallbackData: onbCallbackOfferLater},
	}}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, g.onb().OfferText, rows); err != nil {
		g.logger.Warn("gateway: persona offer send failed", "tg_chat", tgChatID, "error", err)
	}
}

// turnsFromData reads the offer counter. jsonb round-trips numbers as
// float64, so an int type assertion alone would silently restart the
// count at zero on every message and the offer would never fire.
func turnsFromData(data map[string]any) int {
	switch v := data[onbDataTurns].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// signupSource extracts the deep-link payload from a /start, or "" when
// the message is not a /start, carries no payload, or carries something
// that could not have come from a deep link.
//
// The charset check is Telegram's own: it accepts A-Za-z0-9_- in the
// start parameter and nothing else. Anything outside that was typed by
// hand into the chat, so recording it would put arbitrary user text into
// whatever the host reports acquisition from — the one field that has to
// stay a small closed set to be worth counting.
func (g *Gateway) signupSource(bi *botInstance, text string) string {
	cmd, args, forUs := parseStartCommandArgs(g, bi, text)
	if !forUs || cmd != "/start" {
		return ""
	}
	payload := strings.TrimSpace(args)
	if payload == "" || len([]rune(payload)) > onbMaxSourceRunes {
		return ""
	}
	for _, r := range payload {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			g.logger.Debug("gateway: ignoring non-deeplink /start payload")
			return ""
		}
	}
	return payload
}

// sourceFromData reads the payload stashed when the wizard began. The
// wizard creates the account several steps after the /start that carried
// it, so it has to survive in the FSM row; instant mode has no such gap
// and passes it straight through.
func sourceFromData(data map[string]any) string {
	s, _ := data[onbDataSource].(string)
	return s
}

// isEditRun reports whether the in-flight FSM run updates an existing
// soul rather than creating one.
func isEditRun(data map[string]any) bool {
	edit, _ := data[onbDataEdit].(bool)
	return edit
}

// -- Step 1: entry / ask_name -------------------------------------------------

func (g *Gateway) onboardingStart(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, source string) bool {
	var data map[string]any
	if source != "" {
		// Carried through the whole wizard because the account is created
		// four steps after the /start that named the campaign.
		data = map[string]any{onbDataSource: source}
	}
	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskName, data); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(start) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingText(ctx, bi, tgChatID, g.onb().Greeting)
	return true
}

func (g *Gateway) onboardingHandleName(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, raw string, data map[string]any) bool {
	name := strings.TrimSpace(raw)
	// Web parity: 2-30 rune validation. Cap above the limit silently
	// so a too-long paste is rejected with the same hint as too-short.
	minRunes, maxRunes := g.flow().NameBounds()
	rs := []rune(name)
	if len(rs) < minRunes || len(rs) > maxRunes {
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().NameTooShort)
		return true
	}

	// Carry the existing blob forward rather than replacing it: on an
	// edit run it holds the marker that decides whether the last step
	// creates a soul or updates one.
	if data == nil {
		data = map[string]any{}
	}
	data[onbDataName] = name
	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskVoice, data); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(ask_name) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingVoicePicker(ctx, bi, tgChatID, name)
	return true
}

// -- Step 2: ask_voice --------------------------------------------------------

func (g *Gateway) onboardingHandleVoice(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, voiceID string, data map[string]any) bool {
	v := g.findVoice(voiceID)
	if v == nil {
		// Stale / unknown voice id. Re-emit the picker so the user can
		// pick a valid one without losing the name they typed.
		name, _ := data[onbDataName].(string)
		if name == "" {
			name = g.onb().FallbackName
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
	if !g.flow().HasTrait(trait) {
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
		if len(tags) >= g.flow().TagCap() {
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
	// "Done · N of 5" counter both re-render off the fresh tags
	// list, so a single edit keeps the message consistent.
	rows := g.buildTraitsKeyboard(tags)
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
	g.sendOnboardingText(ctx, bi, tgChatID, g.onb().DescriptionPrompt)
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
		if cap := g.flow().DescriptionCap(); len([]rune(desc)) > cap {
			desc = string([]rune(desc)[:cap])
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

// personaFinalize is the terminal step of an edit run: the same five
// answers, applied to the soul the user already has.
//
// On failure the FSM row is deliberately left at step=confirm so the
// user can tap Create again — the update is a single host-side statement,
// so a failure means nothing changed and a retry is safe.
func (g *Gateway) personaFinalize(
	ctx context.Context,
	bi *botInstance,
	tgChatID, tgUserID int64,
	name, voiceID string,
	tags []string,
	desc string,
) bool {
	if g.deps.PersonaEditor == nil {
		// Reachable only if the hook was unwired mid-flow.
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().ErrNoPersona)
		_ = g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id)
		return true
	}

	g.sendOnboardingText(ctx, bi, tgChatID, g.onb().WorkingFmt)

	err := g.deps.PersonaEditor.UpdatePersona(ctx, bs.BotPersonaUpdate{
		BotID:                bi.id,
		TGUserID:             tgUserID,
		Name:                 name,
		VoiceID:              voiceID,
		CharacterTags:        tags,
		CharacterDescription: desc,
	})
	switch {
	case errors.Is(err, bs.ErrBotPersonaNoSoul):
		// The identity lost its soul between /persona and confirm.
		// Nothing to retry against.
		_ = g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id)
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().ErrNoPersona)
		return true
	case err != nil:
		g.logger.Error("gateway: persona update failed",
			"tg_user", tgUserID, "bot_id", bi.id.String(), "error", err)
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().ErrPersonaFail)
		return true
	}

	if err := g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id); err != nil {
		g.logger.Warn("gateway: persona edit ClearState failed", "tg_user", tgUserID, "error", err)
	}

	// The persona is part of the system prompt, and the gateway caches
	// per-user state that was built around the old one. Drop it so the
	// next turn is answered by who the user just described.
	g.invalidateTelegramUser(bi.id, tgCanonical(tgChatID))

	g.logger.Info("gateway: persona updated", "tg_user", tgUserID, "bot_id", bi.id.String())
	g.sendOnboardingText(ctx, bi, tgChatID, fmt.Sprintf(g.onb().EditDoneFmt, name))
	return true
}

// -- Step 5: confirm ----------------------------------------------------------

func (g *Gateway) onboardingFinalize(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, data map[string]any, sender tgSender) bool {
	name, _ := data[onbDataName].(string)
	voiceID, _ := data[onbDataVoice].(string)
	tags := tagsFromData(data)
	desc, _ := data[onbDataDescription].(string)

	if name == "" || voiceID == "" {
		// Defensive: state somehow lacks the required fields. Restart
		// the flow rather than minting a broken soul.
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().Greeting)
		_ = g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskName, nil)
		return true
	}

	if isEditRun(data) {
		return g.personaFinalize(ctx, bi, tgChatID, tgUserID, name, voiceID, tags, desc)
	}

	g.sendOnboardingText(ctx, bi, tgChatID, g.onb().WorkingFmt)

	_, _, err := g.deps.BotOnboarding.CompleteOnboarding(ctx, bs.BotOnboardingComplete{
		BotID:                bi.id,
		TGUserID:             tgUserID,
		TGChatID:             tgChatID,
		Name:                 name,
		UserDisplayName:      sender.Name,
		UserHandle:           sender.Handle,
		UserLanguage:         sender.Language,
		SignupSource:         sourceFromData(data),
		VoiceID:              voiceID,
		CharacterTags:        tags,
		CharacterDescription: desc,
	})
	if err != nil {
		// Already onboarded gets a specific terminal line (user's
		// account exists; nothing to retry). Everything else stays at
		// step=confirm so the user can tap "Create" again after we
		// fix whatever blew up server-side.
		if errors.Is(err, bs.ErrBotOnboardingAlreadyDone) {
			_ = g.deps.BotOnboarding.ClearState(ctx, tgUserID, bi.id)
			g.sendOnboardingText(ctx, bi, tgChatID, g.onb().ErrAlreadyOnboarded)
			return true
		}
		g.logger.Error("gateway: onboarding CompleteOnboarding failed",
			"tg_user", tgUserID, "bot_id", bi.id.String(), "name", name, "error", err)
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().ErrAccountFail)
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
	g.invalidateTelegramUser(bi.id, tgCanonical(tgChatID))

	g.sendOnboardingText(ctx, bi, tgChatID, fmt.Sprintf(g.onb().DoneFmt, name))
	return true
}

func (g *Gateway) onboardingConfirmBack(ctx context.Context, bi *botInstance, tgChatID, tgUserID int64, data map[string]any) bool {
	if err := g.deps.BotOnboarding.AdvanceStep(ctx, tgUserID, bi.id, onbStepAskDescription, data); err != nil {
		g.logger.Warn("gateway: onboarding AdvanceStep(confirm_back) failed",
			"tg_user", tgUserID, "error", err)
		return false
	}
	g.sendOnboardingText(ctx, bi, tgChatID, g.onb().DescriptionPrompt)
	return true
}

// -- /start mid-flow ----------------------------------------------------------

func (g *Gateway) onboardingReissue(ctx context.Context, bi *botInstance, tgChatID int64, step string, data map[string]any) bool {
	switch step {
	case onbStepAskName:
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().Greeting)
	case onbStepAskVoice:
		name, _ := data[onbDataName].(string)
		if name == "" {
			name = g.onb().FallbackName
		}
		g.sendOnboardingVoicePicker(ctx, bi, tgChatID, name)
	case onbStepAskTraits:
		tags := tagsFromData(data)
		g.sendOnboardingTraitsPicker(ctx, bi, tgChatID, 0, tags)
	case onbStepAskDescription:
		g.sendOnboardingText(ctx, bi, tgChatID, g.onb().DescriptionPrompt)
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
	voices := g.voices()
	rows := make([][]telegram.InlineKeyboardButton, 0, len(voices))
	for _, v := range voices {
		rows = append(rows, []telegram.InlineKeyboardButton{
			{
				Text:         g.voiceButton(v),
				CallbackData: onbCallbackVoice + v.ID,
			},
		})
	}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, fmt.Sprintf(g.onb().VoicePromptFmt, name), rows); err != nil {
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
	rows := g.buildTraitsKeyboard(selected)
	res, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, g.onb().TraitsPrompt, rows)
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

// sendOnboardingConfirm emits the summary + [✓ Create] / [← Back]
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

	voiceName := g.onb().DashEmpty
	if v := g.findVoice(voiceID); v != nil {
		voiceName = g.voiceName(*v)
	}
	tagsStr := g.onb().DashEmpty
	if len(tags) > 0 {
		tagsStr = strings.Join(g.traitTexts(tags), ", ")
	}
	descStr := desc
	if strings.TrimSpace(descStr) == "" {
		descStr = g.onb().DashEmpty
	}

	var b strings.Builder
	b.WriteString(g.onb().ConfirmTitle)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, g.onb().ConfirmRowFmt+"\n", g.onb().LabelName, name)
	fmt.Fprintf(&b, g.onb().ConfirmRowFmt+"\n", g.onb().LabelVoice, voiceName)
	fmt.Fprintf(&b, g.onb().ConfirmRowFmt+"\n", g.onb().LabelTraits, tagsStr)
	fmt.Fprintf(&b, g.onb().ConfirmRowFmt+"\n", g.onb().LabelDescription, descStr)
	b.WriteString("\n")
	b.WriteString(g.onb().ConfirmTrueQ)

	rows := [][]telegram.InlineKeyboardButton{
		{
			{Text: g.onb().BtnConfirmOK, CallbackData: onbCallbackConfirmOK},
			{Text: g.onb().BtnConfirmBack, CallbackData: onbCallbackConfirmBack},
		},
	}
	if _, err := bi.client.SendMessageWithKeyboard(ctx, tgChatID, b.String(), rows); err != nil {
		g.logger.Warn("gateway: onboarding confirm send failed",
			"tg_chat", tgChatID, "error", err)
	}
}

// -- helpers ------------------------------------------------------------------

// buildTraitsKeyboard renders the 16-trait grid as a 2-per-row keyboard
// plus a trailing "Done · N of 5" row. Selected traits get a `[✓]`
// prefix, unselected `[ ]`. Order matches the configured palette.
func (g *Gateway) buildTraitsKeyboard(selected []string) [][]telegram.InlineKeyboardButton {
	selSet := make(map[string]struct{}, len(selected))
	for _, t := range selected {
		selSet[t] = struct{}{}
	}
	traits := g.traits()
	rows := make([][]telegram.InlineKeyboardButton, 0, len(traits)/2+1)
	for i := 0; i < len(traits); i += 2 {
		row := []telegram.InlineKeyboardButton{
			{Text: g.traitButton(traits[i].ID, selSet), CallbackData: onbCallbackTrait + traits[i].ID},
		}
		if i+1 < len(traits) {
			row = append(row, telegram.InlineKeyboardButton{
				Text:         g.traitButton(traits[i+1].ID, selSet),
				CallbackData: onbCallbackTrait + traits[i+1].ID,
			})
		}
		rows = append(rows, row)
	}
	rows = append(rows, []telegram.InlineKeyboardButton{
		{
			Text:         fmt.Sprintf(g.onb().TraitsCounterFmt, len(selected)),
			CallbackData: onbCallbackTraitsDone,
		},
	})
	return rows
}

func (g *Gateway) traitButton(t string, selSet map[string]struct{}) string {
	if _, ok := selSet[t]; ok {
		return "[✓] " + g.traitText(t)
	}
	return "[ ] " + g.traitText(t)
}

func (g *Gateway) findVoice(id string) *onbVoice {
	voices := g.voices()
	for i := range voices {
		if voices[i].ID == id {
			return &voices[i]
		}
	}
	return nil
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
	if h := g.deps.Config.Gateway.ResolveDisplayName; h != nil {
		return h(ctx, userID)
	}
	return ""
}
