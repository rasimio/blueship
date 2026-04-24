package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"
)

// Bootstrap holds the one-shot registration + periodic refresh logic for a
// Ship's Fleet integration. On startup it publishes the Ship's identity,
// capabilities, and exposed tool catalog. A background loop then refreshes
// the peer cache every RefreshInterval.
type Bootstrap struct {
	client    *Client
	db        *sqlx.DB
	logger    *slog.Logger
	identity  Identity
	interests []string
}

// Identity is what this Ship publishes about itself to Fleet. Tool and
// capability lists are read from the ship's DB at run time so the
// Bootstrap itself does not need to know each agent's specifics.
type Identity struct {
	DisplayName  string
	Description  string
	EndpointURL  string // A2A endpoint peers invoke; usually the A2A BaseURL
	PublicKey    string // PEM-encoded, optional
	Capabilities []Capability
}

// ToolPublisher is the narrow interface Bootstrap uses to gather exposed
// tools from the ship's unified `tools` table without reaching into
// a2a/store directly.
type ToolPublisher interface {
	ListExposedTools(ctx context.Context) ([]Tool, error)
}

// NewBootstrap constructs a Bootstrap. Call Run in a goroutine.
func NewBootstrap(client *Client, db *sqlx.DB, identity Identity, interests []string, logger *slog.Logger) *Bootstrap {
	return &Bootstrap{
		client:    client,
		db:        db,
		logger:    logger,
		identity:  identity,
		interests: interests,
	}
}

// PublishIdentity sends a single PATCH /v0/agents/me + PUT capabilities +
// PUT tools call. Idempotent — safe to invoke on every Ship startup.
func (b *Bootstrap) PublishIdentity(ctx context.Context, tp ToolPublisher) error {
	disp := b.identity.DisplayName
	desc := b.identity.Description
	ep := b.identity.EndpointURL
	pub := b.identity.PublicKey
	patch := PatchMeRequest{
		DisplayName: &disp,
		Description: &desc,
		EndpointURL: &ep,
	}
	if pub != "" {
		patch.PublicKey = &pub
	}
	if err := b.client.PatchMe(ctx, patch); err != nil {
		return fmt.Errorf("patch_me: %w", err)
	}
	if err := b.client.PutCapabilities(ctx, b.identity.Capabilities); err != nil {
		return fmt.Errorf("put capabilities: %w", err)
	}
	tools, err := tp.ListExposedTools(ctx)
	if err != nil {
		return fmt.Errorf("list exposed tools: %w", err)
	}
	if err := b.client.PutTools(ctx, tools); err != nil {
		return fmt.Errorf("put tools: %w", err)
	}
	b.logger.Info("fleet: identity published",
		"capabilities", len(b.identity.Capabilities),
		"tools", len(tools))
	return nil
}

// RefreshPeers fetches peers for each capability this Ship cares about,
// then caches their full cards in fleet_peer_cache.
func (b *Bootstrap) RefreshPeers(ctx context.Context) error {
	seen := make(map[string]bool)
	for _, tag := range b.interests {
		agents, err := b.client.Search(ctx, tag, "", 100)
		if err != nil {
			b.logger.Warn("fleet: search failed", "capability", tag, "error", err)
			continue
		}
		for _, a := range agents {
			if seen[a.ID] {
				continue
			}
			seen[a.ID] = true
			card, err := b.client.GetPeer(ctx, a.ID)
			if err != nil {
				b.logger.Warn("fleet: fetch peer card failed", "peer", a.Name, "error", err)
				continue
			}
			if err := upsertPeerCache(ctx, b.db, card); err != nil {
				b.logger.Warn("fleet: cache peer failed", "peer", a.Name, "error", err)
			}
		}
	}
	b.logger.Info("fleet: peer cache refreshed", "count", len(seen))
	return nil
}

// Run executes the startup publish + refresh loop until ctx is cancelled.
// Returns nil when ctx fires; transient errors are logged but don't stop
// the loop (Fleet should not take a Ship down).
func (b *Bootstrap) Run(ctx context.Context, tp ToolPublisher) {
	interval := 5 * time.Minute
	if b.client.cfg.RefreshInterval > 0 {
		interval = b.client.cfg.RefreshInterval
	}

	// First publish — log but don't abort; Ships must boot even if Fleet
	// is briefly unreachable.
	if err := b.PublishIdentity(ctx, tp); err != nil {
		b.logger.Error("fleet: initial publish failed", "error", err)
	}
	if err := b.RefreshPeers(ctx); err != nil {
		b.logger.Warn("fleet: initial peer refresh failed", "error", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-publish identity in case local tool catalog changed.
			if err := b.PublishIdentity(ctx, tp); err != nil {
				b.logger.Warn("fleet: publish refresh failed", "error", err)
			}
			if err := b.RefreshPeers(ctx); err != nil {
				b.logger.Warn("fleet: peer refresh failed", "error", err)
			}
		}
	}
}

// upsertPeerCache writes one peer's full card into fleet_peer_cache. The
// table is a pure read-through cache; Fleet remains the source of truth.
func upsertPeerCache(ctx context.Context, db *sqlx.DB, card *PeerCard) error {
	capsJSON, _ := json.Marshal(card.Capabilities)
	toolsJSON, _ := json.Marshal(card.Tools)
	_, err := db.ExecContext(ctx, `
		INSERT INTO fleet_peer_cache
		    (agent_id, name, display_name, description, endpoint_url,
		     public_key, status, capabilities, tools, last_refreshed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
		ON CONFLICT (agent_id) DO UPDATE SET
		    name           = EXCLUDED.name,
		    display_name   = EXCLUDED.display_name,
		    description    = EXCLUDED.description,
		    endpoint_url   = EXCLUDED.endpoint_url,
		    public_key     = EXCLUDED.public_key,
		    status         = EXCLUDED.status,
		    capabilities   = EXCLUDED.capabilities,
		    tools          = EXCLUDED.tools,
		    last_refreshed = NOW()
	`,
		card.Agent.ID,
		card.Agent.Name,
		card.Agent.DisplayName,
		nullIfEmpty(card.Agent.Description),
		nullIfEmpty(card.Agent.EndpointURL),
		nullIfEmpty(card.Agent.PublicKey),
		card.Agent.Status,
		capsJSON,
		toolsJSON,
	)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
