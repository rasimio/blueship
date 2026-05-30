package blueship

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/federation/fleet"
)

// runFleet boots the optional BlueFleet integration. Blocks until ctx is
// cancelled. Transient errors (Fleet down, token expired between refreshes)
// are logged but never take the Ship down.
func (s *Ship) runFleet(ctx context.Context, deps *Deps, reg *moduleRegistry) {
	shipDB, err := deps.DB("ship")
	if err != nil {
		s.logger.Error("fleet: ship db unavailable", "error", err)
		return
	}
	cfg := s.cfg.Fleet
	// Treat unexpanded ${FOO} placeholders as "env var was not set" so the
	// Ship silently skips Fleet until deploy provisions credentials.
	if strings.Contains(cfg.BaseURL, "${") {
		cfg.BaseURL = ""
	}
	if strings.Contains(cfg.ClientID, "${") {
		cfg.ClientID = ""
	}
	if strings.Contains(cfg.ClientSecret, "${") {
		cfg.ClientSecret = ""
	}
	if cfg.BaseURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		s.logger.Warn("fleet: missing base_url / client_id / client_secret — skipping")
		return
	}
	cli := fleet.New(fleet.Config{
		BaseURL:         cfg.BaseURL,
		ClientID:        cfg.ClientID,
		ClientSecret:    cfg.ClientSecret,
		RefreshInterval: cfg.RefreshInterval,
	}, s.logger)

	caps := make([]fleet.Capability, 0, len(cfg.Capabilities))
	for _, c := range cfg.Capabilities {
		var meta json.RawMessage
		if len(c.Metadata) > 0 {
			meta = json.RawMessage(c.Metadata)
		}
		caps = append(caps, fleet.Capability{
			Tag:         c.Tag,
			Description: c.Description,
			Metadata:    meta,
		})
	}

	bs := fleet.NewBootstrap(cli, shipDB, fleet.Identity{
		DisplayName:  cfg.DisplayName,
		Description:  cfg.Description,
		EndpointURL:  cfg.EndpointURL,
		PublicKey:    cfg.PublicKey,
		Capabilities: caps,
	}, cfg.InterestedIn, s.logger)

	// JWKS cache for inbound JWT validation. Populated before federation
	// sink is wired so the A2A server can accept Fleet tokens immediately.
	jwks := fleet.NewJWKSCache(cfg.BaseURL, 30*time.Minute, s.logger)
	go jwks.Run(ctx)

	// Look up our own agent_id so the JWT validator can enforce the
	// audience claim. If GetMe fails (cold start, Fleet down) we retry on a
	// short backoff — without it the inbound JWT path stays disabled.
	selfID := s.discoverSelfAgentID(ctx, cli)
	if selfID != "" {
		s.fleetAuth.set(jwks, selfID)
		s.logger.Info("fleet: self-id learned", "agent_id", selfID)
	} else {
		s.logger.Warn("fleet: self-id discovery failed; inbound JWT auth stays disabled")
	}

	// Federation: register peer-imported tools into the Ship's tool
	// registry on every refresh tick. invokeFn does the actual HTTP POST.
	if selfID != "" {
		bs.WithFederation(selfID, &fleetSinkAdapter{reg: reg, logger: s.logger}, s.fleetInvoke)
	}

	if s.a2aRegistry == nil {
		s.logger.Warn("fleet: A2A registry not built yet — running with empty exposed tools")
		s.a2aRegistry = core.NewToolRegistry()
	}
	bs.Run(ctx, &fleetToolPublisher{reg: s.a2aRegistry})
}

// discoverSelfAgentID asks Fleet for the calling agent's profile, retrying
// briefly so a cold-start race against Fleet doesn't leave inbound JWT
// auth permanently disabled.
func (s *Ship) discoverSelfAgentID(ctx context.Context, cli *fleet.Client) string {
	for attempt := 0; attempt < 5; attempt++ {
		card, err := cli.GetMe(ctx)
		if err == nil && card.Agent.ID != "" {
			return card.Agent.ID
		}
		if attempt < 4 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(time.Duration(2<<attempt) * time.Second):
			}
		}
	}
	return ""
}

// fleetInvoke is the dispatch closure handed to fleet.Bootstrap. It does
// one peer-to-peer A2A call, stamping the supplied JWT bearer.
func (s *Ship) fleetInvoke(ctx context.Context, peerName, peerAgentID, endpointURL, toolName string, input []byte, bearer string) ([]byte, error) {
	if endpointURL == "" {
		return nil, fmt.Errorf("fleet: peer %q has no endpoint URL", peerName)
	}
	body, _ := json.Marshal(map[string]any{
		"tool":  toolName,
		"input": json.RawMessage(input),
	})
	url := strings.TrimRight(endpointURL, "/") + "/a2a/invoke"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	httpCli := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fleet: invoke %s/%s: %w", peerName, toolName, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fleet: invoke %s/%s: HTTP %d: %s", peerName, toolName, resp.StatusCode, string(raw))
	}
	return raw, nil
}

// fleetSinkAdapter bridges fleet.FederatedToolSink to moduleRegistry.
type fleetSinkAdapter struct {
	reg    *moduleRegistry
	logger *slog.Logger
}

func (a *fleetSinkAdapter) ReplaceFleetTools(snapshot []fleet.FederatedTool) {
	tools := make([]remoteToolReg, 0, len(snapshot))
	for _, ft := range snapshot {
		ftLocal := ft
		handler := core.ToolHandler(func(ctx context.Context, input json.RawMessage) (any, error) {
			raw, err := ftLocal.Handler(ctx, []byte(input))
			if err != nil {
				return nil, err
			}
			// Tool returns the full /a2a/invoke response body. Decode it
			// and surface either the sync output or the async handle.
			var ir struct {
				Mode      string          `json:"mode"`
				Output    json.RawMessage `json:"output"`
				CallID    string          `json:"call_id"`
				Handle    string          `json:"handle"`
				State     string          `json:"state"`
				EventsURL string          `json:"events_url"`
			}
			if err := json.Unmarshal(raw, &ir); err != nil {
				return string(raw), nil
			}
			if ir.Mode == "async" {
				return map[string]any{
					"call_id":    ir.CallID,
					"handle":     ir.Handle,
					"state":      ir.State,
					"events_url": ir.EventsURL,
				}, nil
			}
			if len(ir.Output) == 0 {
				return map[string]any{"ok": true}, nil
			}
			var result any
			if err := json.Unmarshal(ir.Output, &result); err != nil {
				return string(ir.Output), nil
			}
			return result, nil
		})
		tools = append(tools, remoteToolReg{
			Name:        ftLocal.Name,
			Description: ftLocal.Description,
			Schema:      json.RawMessage(ftLocal.Schema),
			Mode:        ftLocal.Mode,
			PeerName:    ftLocal.PeerName,
			Handler:     handler,
		})
	}
	a.reg.ReplaceFleetRemoteTools("fleet", tools)
	a.logger.Info("fleet: registered federated tools", "count", len(tools))
}

// fleetToolPublisher snapshots the registry's exposed tools and reshapes
// them into the format BlueFleet expects in PutTools. Source of truth is
// the in-memory registry — there is no DB read involved.
type fleetToolPublisher struct {
	reg *core.ToolRegistry
}

func (p *fleetToolPublisher) ListExposedTools(_ context.Context) ([]fleet.Tool, error) {
	src := p.reg.ExposedTools()
	out := make([]fleet.Tool, 0, len(src))
	for _, t := range src {
		mode, _, _, _ := p.reg.ToolMetadata(t.Name)
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{}`)
		}
		out = append(out, fleet.Tool{
			Name:        t.Name,
			Description: t.Description,
			Mode:        mode,
			InputSchema: schema,
		})
	}
	return out, nil
}
