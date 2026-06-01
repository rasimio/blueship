package core

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// TransportConfig holds transport configuration.
type TransportConfig struct {
	Type string // "telegram"

	// BotToken — DEPRECATED. Single-bot fallback path kept so existing
	// dev/test configs (and the legacy ArleneKateBot bootstrap) keep
	// working until the migration to multi-bot is complete. New deployments
	// supply Telegram.ListBots; the gateway then ignores BotToken with a
	// warning at startup.
	BotToken string

	// Telegram carries the multi-bot configuration. When ListBots is
	// non-nil the gateway maintains an in-memory registry of bots driven
	// by the host (typically a vaelum.bots SELECT), and ignores BotToken.
	Telegram TelegramConfig

	// WebSocket server for voice/desktop clients (runs alongside Telegram).
	WebSocket WebSocketConfig

	// HTTPChat server for the Vaelum web platform (runs alongside Telegram).
	HTTPChat HTTPChatConfig
}

// TelegramConfig configures the multi-bot Telegram transport.
//
// Vaelum is multi-tenant: the platform bot @VaelumBot routes messages to
// many souls via per-chat pairings, and any user can register their own
// bot token to get a dedicated entrypoint. The host owns persistence,
// authorisation, and token decryption — blueship is given a fully-realised
// list of (id, kind, owner, plaintext token) tuples and runs a polling
// goroutine per row.
type TelegramConfig struct {
	// ListBots is called at startup and on every reload to enumerate the
	// bots the gateway should poll. The host is responsible for decrypting
	// tokens before returning them; blueship never sees ciphertext.
	// Nil → fall back to single-bot Transport.BotToken (legacy).
	ListBots func(ctx context.Context) ([]BotConfig, error) `yaml:"-" json:"-"`

	// ReloadInterval controls how often the gateway polls ListBots in the
	// background (in addition to host-triggered reloads via Gateway.ReloadBots).
	// Default: 60 seconds. Zero or negative disables background reconcile.
	ReloadInterval time.Duration

	// ReloadTrigger is the host's poke channel. Sending an empty struct
	// asks the gateway to reconcile its registry against ListBots
	// immediately (cabinet add/delete signal). Nil = disabled; the
	// background reconcile still runs every ReloadInterval.
	ReloadTrigger chan struct{} `yaml:"-" json:"-"`
}

// BotConfig describes one Telegram bot the gateway should manage.
type BotConfig struct {
	// ID is the host's stable identifier for this bot (typically the
	// vaelum.bots.id UUID). The gateway uses it as a routing key for
	// inbound updates. uuid.Nil is reserved for the legacy BotToken
	// fallback path.
	ID uuid.UUID

	// Kind discriminates routing semantics:
	//   "platform" — open to any signed-up Vaelum user; unpaired chats
	//                get a signup greeting.
	//   "user"     — paired only to the owner; unpaired chats are
	//                silently ignored.
	Kind string

	// OwnerUserID is the Vaelum user that owns a "user"-kind bot. For
	// "platform" bots it is uuid.Nil.
	OwnerUserID uuid.UUID

	// Token is the raw Telegram bot token used to authenticate against
	// the Bot API. Decrypted host-side; passed in plaintext to blueship.
	Token string
}

// WebSocketConfig configures the optional WebSocket server.
type WebSocketConfig struct {
	Port  int    // 0 = disabled
	Token string // legacy shared bearer token (dev fallback when ResolveDevice is nil)

	// ResolveDevice authenticates a per-user device bearer token and
	// returns the (user, soul) it is bound to. When non-nil, voice
	// connections present `Authorization: Bearer <opaque>` and are
	// dispatched to ProcessInboundForUser — no chatID translation, no
	// owner gate. When nil, the legacy shared Token + voice:owner
	// chatID path runs (single-tenant dev fallback). The host supplies
	// the implementation (typically a vaelum.devices lookup); blueship
	// stays generic about the token format.
	ResolveDevice func(ctx context.Context, token string) (userID, soulID uuid.UUID, err error) `yaml:"-" json:"-"`
}

// HTTPChatConfig configures the optional HTTP/SSE chat server that serves
// the Vaelum web platform's live chat.
type HTTPChatConfig struct {
	Port  int    // 0 = disabled
	Token string // bearer service token vaelum must present

	// Extras, when non-nil, is called once with the server's mux during
	// startup so the host daemon can mount additional internal
	// API routes on the same port and share the bearer-token middleware.
	// Generic from blueship's side — it just calls the callback. Used for
	// the host's internal memory-associate endpoint that proxies AME
	// search from the Vaelum cabinet.
	Extras func(*http.ServeMux) `yaml:"-" json:"-"`

	// Reset, when non-nil, exposes POST /api/internal/chat/reset on the
	// httpchat mux. Vaelum's web cabinet calls it (relayed via
	// /api/chat/reset) to archive the active (user, soul) chat session
	// and open a fresh one — equivalent of the Telegram /reset command
	// for HTTP callers. Wired by blueship.Ship after the gateway is built;
	// the host doesn't set it.
	Reset func(ctx context.Context, userID string) (oldSessionID, newSessionID string, err error) `yaml:"-" json:"-"`
}

// ErrTelegramChatUnpaired signals "this Telegram chat is not linked to any
// Vaelum user on this bot". Hosts return it (or wrap it) from
// GatewayConfig.ResolveTelegramChat so the gateway can run the
// unpaired-chat policy instead of treating the lookup as a transient
// failure.
var ErrTelegramChatUnpaired = errors.New("blueship: telegram chat not paired")
