package core

import (
	"context"

	"github.com/google/uuid"
)

// BotOnboarding is the host-supplied hook that drives a fresh Telegram
// user through account creation entirely inside the chat — a /start
// from someone who has no vaelum.user_identities row yet kicks off the
// FSM, exchanges 2-3 turns, then creates the user + soul + bot_links
// atomically and hands routing back to the normal cortex turn.
//
// The interface is intentionally narrow: the gateway owns the inbound
// detection, the outbound sends, and the callback-query dispatch; the
// host owns the state machine and the account-creation transaction.
// A nil hook on Deps disables the inline-onboarding path — the gateway
// then falls back to its replyUnpaired greeting (signup URL on
// platform-kind bots, silence on user-kind bots).
//
// Step values are short strings ("ask_name", "ask_soul_preset", "done")
// matching the migration's bot_onboarding_state.step column. The gateway
// treats these as opaque; the host decides the FSM topology.
//
// All methods scope by (tgUserID, botID) — the same composite key the
// host's bot_onboarding_state PK uses — so two bots running the same
// user through onboarding in parallel don't trample each other's state.
type BotOnboarding interface {
	// GetState looks up the FSM row for (tgUserID, botID). Returns
	// step="" when no row exists — the caller treats that as "user
	// hasn't started onboarding".
	GetState(ctx context.Context, tgUserID int64, botID uuid.UUID) (step string, data map[string]any, err error)

	// AdvanceStep upserts the FSM row to the new step + data. Idempotent:
	// the PK (tgUserID, botID) means a /start re-sent at the same step
	// just refreshes updated_at and the data blob.
	AdvanceStep(ctx context.Context, tgUserID int64, botID uuid.UUID, step string, data map[string]any) error

	// CreateAccount runs the atomic account-creation transaction:
	// vaelum.users + vaelum.user_identities (kind='telegram') +
	// vaelum.organizations + vaelum.souls + vaelum.soul_personas +
	// vaelum.memberships + baseline rules + default tasks +
	// vaelum.bot_links (so subsequent messages route via ResolveTelegramChat)
	// + public.user_profiles mirror (so the scheduler can Notify).
	//
	// Returns the new user and soul ids so the caller can stash them on
	// UserState and the cortex turn immediately resumes for the same
	// inbound message — no re-pairing round trip.
	CreateAccount(ctx context.Context, in BotOnboardingAccount) (userID, soulID uuid.UUID, err error)

	// ClearState removes the FSM row after CreateAccount commits (or
	// when the user aborts the flow). Idempotent: missing row is not an
	// error.
	ClearState(ctx context.Context, tgUserID int64, botID uuid.UUID) error

	// CompleteDeeplinkLogin is the bot's half of the cabinet's
	// "Approve in bot" deep-link auth flow. The gateway calls it when
	// /start login_<TOKEN> hits the platform bot: the host atomically
	// flips the matching tg_login_states row from 'pending' to
	// 'approved', stamping the approving Telegram user id. Returns the
	// user-facing message the gateway should reply with — success
	// confirmation when approved is true, "link expired" otherwise.
	//
	// The host MUST NOT create the vaelum.users row inside this
	// method: the cabinet's poll handler does that so the freshly
	// minted session cookie can be attached to the HTTP response.
	// Returning (true, ...) just signals approval; the actual identity
	// resolution and session mint happen on the next cabinet poll.
	CompleteDeeplinkLogin(ctx context.Context, token string, tgUserID int64) (approved bool, message string, err error)
}

// BotOnboardingAccount is the payload CreateAccount needs to mint a
// fresh tenant from a Telegram chat. tg_chat_id is the user's 1:1 chat
// with the bot — it lands in vaelum.bot_links AND in user_profiles.chat_id
// so both the gateway's routing read and the scheduler's notify path
// resolve the same channel.
//
// SoulPreset selects which built-in persona template seeds the new
// soul. v1 only ships "arlene_style"; "custom" is reserved for the
// later branch that asks the user free-form questions to author a
// per-soul system prompt.
type BotOnboardingAccount struct {
	BotID      uuid.UUID
	TGUserID   int64
	TGChatID   int64
	Name       string // user-supplied display name; also used as soul name
	SoulPreset string // "arlene_style" | "custom" (v1: both fall back to arlene_style)
}
