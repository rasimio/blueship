package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/telegram"
)

// botInstance is one Telegram bot the gateway maintains a poller for.
// All inbound updates carry the owning instance's id so handleUpdate can
// route the message through the right downstream lookup, and outbound
// sends use the instance's own *telegram.Client (file IDs, inline-keyboard
// callbacks, and reply messages are all bot-scoped on the Telegram side).
type botInstance struct {
	id          uuid.UUID // host-stable id (vaelum.bots.id), uuid.Nil for legacy
	kind        string    // "platform" | "user"
	ownerUserID uuid.UUID // for kind="user"; uuid.Nil for "platform"
	token       string

	client *telegram.Client
	poller *telegram.Poller

	tgBotID    int64  // numeric id from Telegram getMe
	tgUsername string // bare username (no @)

	cancel context.CancelFunc // stops this bot's polling goroutine
}

// taggedUpdate wraps a telegram.Update with the id of the bot that
// received it. The gateway fan-ins all bots' pollers into one channel,
// so handleUpdate needs the tag to recover which instance the message
// belongs to.
type taggedUpdate struct {
	botID  uuid.UUID
	update telegram.Update
}

// initBotRegistry sets up the maps and fan-in channel on a fresh Gateway.
// Called from NewGateway before either lifecycle method runs.
func (g *Gateway) initBotRegistry() {
	g.bots = make(map[uuid.UUID]*botInstance)
	g.botsByTGID = make(map[int64]*botInstance)
	// Buffer sized for short bursts across all bots without blocking the
	// pollers; the demux loop drains it as fast as handleUpdate runs.
	g.updatesChan = make(chan taggedUpdate, 256)
}

// ReloadBots reconciles the in-memory bot registry against
// cfg.Transport.Telegram.ListBots() (or the legacy single BotToken).
// Idempotent: adds bots in the new set, removes bots no longer in it,
// keeps the rest untouched.
//
// Called once at startup, on every host-triggered reload request
// (e.g. POST /api/internal/telegram/bots/reload after a user adds or
// deletes a bot in the Vaelum cabinet), and periodically by the
// background reconcile loop.
func (g *Gateway) ReloadBots(ctx context.Context) error {
	desired, err := g.loadDesiredBots(ctx)
	if err != nil {
		return err
	}

	desiredByID := make(map[uuid.UUID]bs.BotConfig, len(desired))
	for _, d := range desired {
		desiredByID[d.ID] = d
	}

	g.botsMu.Lock()
	current := make([]uuid.UUID, 0, len(g.bots))
	for id := range g.bots {
		current = append(current, id)
	}
	g.botsMu.Unlock()

	// Stop bots no longer in the desired set.
	for _, id := range current {
		if _, keep := desiredByID[id]; !keep {
			g.unregisterBotLocked(id)
		}
	}

	// Start new bots, leave existing ones alone. We don't try to "update"
	// a bot in place — if the token changed in the DB, the host either
	// rotates the row's id or removes+re-adds it. The simpler path here
	// stays correct under both behaviours.
	var firstErr error
	for _, cfg := range desired {
		g.botsMu.RLock()
		_, exists := g.bots[cfg.ID]
		g.botsMu.RUnlock()
		if exists {
			continue
		}
		if err := g.registerBot(ctx, cfg); err != nil {
			g.logger.Warn("gateway: register bot failed",
				"bot_id", cfg.ID.String(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// loadDesiredBots returns the bots the gateway should currently poll.
// Sources, in order:
//   1. cfg.Transport.Telegram.ListBots — the multi-bot path (typically a
//      vaelum.bots SELECT). When set, BotToken is ignored.
//   2. cfg.Transport.BotToken — single-bot legacy fallback. Yields one
//      transient bot with uuid.Nil id; useful for dev/test configs that
//      pre-date the platform tables.
func (g *Gateway) loadDesiredBots(ctx context.Context) ([]bs.BotConfig, error) {
	cfg := g.deps.Config
	if cfg.Transport.Telegram.ListBots != nil {
		return cfg.Transport.Telegram.ListBots(ctx)
	}
	if cfg.Transport.BotToken != "" {
		return []bs.BotConfig{{
			ID:    uuid.Nil,
			Kind:  "user", // owner-only behaviour matches the legacy single-bot model
			Token: cfg.Transport.BotToken,
		}}, nil
	}
	return nil, nil
}

// registerBot stands up one botInstance: builds the client + poller,
// fetches the bot's identity via getMe, and spawns the polling goroutine
// that fans into g.updatesChan.
func (g *Gateway) registerBot(parentCtx context.Context, cfg bs.BotConfig) error {
	client := telegram.NewClient(cfg.Token, g.deps.Config.Timeouts.TelegramClient)
	poller := telegram.NewPoller(cfg.Token, g.deps.Config.Timeouts.TelegramPoll)

	meCtx, meCancel := context.WithTimeout(parentCtx, 10*time.Second)
	me, err := client.GetMe(meCtx)
	meCancel()
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}

	ctx, cancel := context.WithCancel(parentCtx)
	bi := &botInstance{
		id:          cfg.ID,
		kind:        cfg.Kind,
		ownerUserID: cfg.OwnerUserID,
		token:       cfg.Token,
		client:      client,
		poller:      poller,
		tgBotID:     me.ID,
		tgUsername:  me.Username,
		cancel:      cancel,
	}

	g.botsMu.Lock()
	// Guard against a tgBotID collision sneaking through (two host rows
	// with different host-ids but the same Telegram bot). The DB UNIQUE
	// on tg_bot_id already prevents this; defence in depth.
	if existing, ok := g.botsByTGID[me.ID]; ok && existing.id != cfg.ID {
		g.botsMu.Unlock()
		cancel()
		return fmt.Errorf("tg_bot_id %d already registered by host id %s", me.ID, existing.id)
	}
	g.bots[cfg.ID] = bi
	g.botsByTGID[me.ID] = bi
	g.botsMu.Unlock()

	g.logger.Info("gateway: registered bot",
		"bot_id", cfg.ID.String(),
		"kind", cfg.Kind,
		"tg_bot_id", me.ID,
		"username", me.Username,
	)

	// Spawn the poller goroutine. Each tick lands updates into a
	// per-bot channel that we then tag and forward into the shared
	// fan-in. A poller crash drops THIS bot's updates only; the others
	// keep going.
	go g.runBotPoller(ctx, bi)
	return nil
}

// runBotPoller drives one bot's long-polling loop, tagging every update
// with the bot's id and forwarding into the gateway's shared updates
// channel. Returns when ctx is cancelled (either via UnregisterBot or
// the gateway's own shutdown).
func (g *Gateway) runBotPoller(ctx context.Context, bi *botInstance) {
	defer g.logger.Info("gateway: bot poller stopped",
		"bot_id", bi.id.String(), "tg_bot_id", bi.tgBotID)

	ch := make(chan telegram.Update, 32)
	go bi.poller.Run(ctx, ch)

	for {
		select {
		case <-ctx.Done():
			return
		case upd := <-ch:
			tagged := taggedUpdate{botID: bi.id, update: upd}
			select {
			case g.updatesChan <- tagged:
			case <-ctx.Done():
				return
			}
		}
	}
}

// UnregisterBot stops a bot's polling goroutine and removes it from the
// registries. Caller-visible variant of unregisterBotLocked; takes the
// lock for callers that have it externally clear.
func (g *Gateway) UnregisterBot(id uuid.UUID) {
	g.unregisterBotLocked(id)
}

func (g *Gateway) unregisterBotLocked(id uuid.UUID) {
	g.botsMu.Lock()
	bi, ok := g.bots[id]
	if ok {
		delete(g.bots, id)
		delete(g.botsByTGID, bi.tgBotID)
	}
	g.botsMu.Unlock()
	if !ok {
		return
	}
	bi.cancel()
	g.logger.Info("gateway: unregistered bot",
		"bot_id", id.String(),
		"tg_bot_id", bi.tgBotID,
	)
}

// botByID looks up an instance from its host-stable id.
func (g *Gateway) botByID(id uuid.UUID) *botInstance {
	g.botsMu.RLock()
	defer g.botsMu.RUnlock()
	return g.bots[id]
}

// botByTGID looks up an instance from the numeric Telegram bot id —
// used by callback-query handling where the update carries
// from.id == bi.tgBotID.
func (g *Gateway) botByTGID(id int64) *botInstance {
	g.botsMu.RLock()
	defer g.botsMu.RUnlock()
	return g.botsByTGID[id]
}

// anyBot returns one registered bot or nil. Last-resort fallback for
// code paths that have a chatID but no bot context (legacy /reset on
// a chat whose UserState was lost across a restart). Used to keep the
// reply pipeline working until UserState rebuilds — never picked when
// a specific bot is known.
func (g *Gateway) anyBot() *botInstance {
	g.botsMu.RLock()
	defer g.botsMu.RUnlock()
	for _, bi := range g.bots {
		return bi
	}
	return nil
}

// telegramEnabled reports whether the gateway is managing any Telegram
// bots. Code paths that branch on "do we have Telegram at all" check
// this instead of the old `g.tg == nil`.
func (g *Gateway) telegramEnabled() bool {
	g.botsMu.RLock()
	defer g.botsMu.RUnlock()
	return len(g.bots) > 0
}

// runReloadLoop reconciles the bot registry against the host's source
// of truth on two triggers:
//   - periodic: every cfg.Transport.Telegram.ReloadInterval (default
//     60s). The safety-net path for missed host signals.
//   - on-demand: any send on cfg.Transport.Telegram.ReloadTrigger. The
//     low-latency path the vaelum cabinet uses after a user adds or
//     removes a bot.
//
// Returns when ctx is done.
func (g *Gateway) runReloadLoop(ctx context.Context) {
	interval := g.deps.Config.Transport.Telegram.ReloadInterval
	trigger := g.deps.Config.Transport.Telegram.ReloadTrigger

	var tickC <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tickC = t.C
	}

	if tickC == nil && trigger == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tickC:
			if err := g.ReloadBots(ctx); err != nil {
				g.logger.Warn("gateway: periodic reload failed", "error", err)
			}
		case <-trigger:
			if err := g.ReloadBots(ctx); err != nil {
				g.logger.Warn("gateway: triggered reload failed", "error", err)
			}
		}
	}
}

// errUnpaired is a sentinel checked across telegram routing paths so the
// no-link branch is unambiguous. Equivalent to bs.ErrTelegramChatUnpaired —
// re-exposed here only to avoid importing the bs alias in code paths
// that don't already depend on it.
var errUnpaired = errors.New("telegram chat not paired")
