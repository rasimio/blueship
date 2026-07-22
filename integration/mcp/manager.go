package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/core"
)

const (
	connectTimeout = 25 * time.Second // whole-soul connect budget
	serverTimeout  = 12 * time.Second // per-server budget
	cacheTTL       = 10 * time.Minute // stale-while-revalidate window
)

// Manager is a daemon-lifetime, soul-keyed pool of MCP connections. It
// implements core.MCPToolSource.
type Manager struct {
	store      ServerStore
	credFetch  CredentialFetcher
	logger     *slog.Logger
	dialServer func(context.Context, ServerRow, string) (*Client, error)
	mu         sync.Mutex
	souls      map[uuid.UUID]*soulConns
}

type soulConns struct {
	mu          sync.Mutex
	clients     []*Client
	tools       []core.MCPTool
	sig         string // serversSignature at last connect
	refreshedAt time.Time
	refreshing  bool
}

type connectResult struct {
	server ServerRow
	client *Client
	tools  []core.MCPTool
	defs   []ToolDef
}

// NewManager builds an MCP connection manager. store is the host-supplied
// persistence seam (reads server config, records status, publishes the tool
// catalog); credFetch resolves credentials to secrets.
func NewManager(store ServerStore, credFetch CredentialFetcher, logger *slog.Logger) *Manager {
	return &Manager{
		store:      store,
		credFetch:  credFetch,
		logger:     logger,
		dialServer: dial,
		souls:      make(map[uuid.UUID]*soulConns),
	}
}

// ToolsForSoul returns the soul's MCP tools. It is cached per soul; a cheap
// per-call signature of the soul's mcp_servers rows means a cabinet
// add/remove is picked up on the next turn. Stale entries (TTL) are served
// immediately while a refresh runs in the background. A cold soul connects
// synchronously. It never errors: all-down servers yield nil.
func (m *Manager) ToolsForSoul(ctx context.Context, soulID uuid.UUID) []core.MCPTool {
	if soulID == uuid.Nil {
		return nil
	}
	m.mu.Lock()
	sc := m.souls[soulID]
	if sc == nil {
		sc = &soulConns{}
		m.souls[soulID] = sc
	}
	m.mu.Unlock()

	sig := m.store.ServersSignature(ctx, soulID)

	sc.mu.Lock()
	cold := sc.refreshedAt.IsZero()
	changed := !cold && sc.sig != sig
	stale := !cold && time.Since(sc.refreshedAt) > cacheTTL
	switch {
	case !cold && !changed && !stale:
		tools := sc.tools
		sc.mu.Unlock()
		return tools
	case !cold && !changed && stale:
		tools := sc.tools
		if !sc.refreshing {
			sc.refreshing = true
			go func() {
				m.connectSoul(soulID, sc, sig)
				sc.mu.Lock()
				sc.refreshing = false
				sc.mu.Unlock()
			}()
		}
		sc.mu.Unlock()
		return tools
	default: // cold, or the soul's server config changed — connect now
		sc.mu.Unlock()
		m.connectSoul(soulID, sc, sig)
		sc.mu.Lock()
		tools := sc.tools
		sc.mu.Unlock()
		return tools
	}
}

// connectSoul dials every enabled server of a soul in parallel and swaps
// the cached connection set. Per-server failures are isolated.
func (m *Manager) connectSoul(soulID uuid.UUID, sc *soulConns, sig string) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	servers, err := m.store.ServersForSoul(ctx, soulID)
	if err != nil {
		m.logger.Warn("mcp: list servers failed", "soul_id", soulID.String(), "error", err)
		sc.mu.Lock()
		sc.refreshedAt = time.Now() // don't hammer a broken DB every turn
		sc.sig = sig
		sc.mu.Unlock()
		return
	}

	results := make([]connectResult, len(servers))
	var wg sync.WaitGroup
	for i := range servers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = m.connectServer(ctx, servers[i])
		}(i)
	}
	wg.Wait()

	owners := make(map[string]int)
	conflicts := make(map[int]string)
	for i, result := range results {
		if result.client == nil {
			continue
		}
		for _, tool := range result.tools {
			owner, exists := owners[tool.Name]
			if !exists {
				owners[tool.Name] = i
				continue
			}
			if owner == i {
				continue
			}
			if _, marked := conflicts[owner]; !marked {
				conflicts[owner] = tool.Name
			}
			if _, marked := conflicts[i]; !marked {
				conflicts[i] = tool.Name
			}
		}
	}

	var clients []*Client
	var tools []core.MCPTool
	for i, r := range results {
		if r.client != nil {
			if name, conflicted := conflicts[i]; conflicted {
				_ = r.client.close()
				err := fmt.Errorf("tool name collision across MCP servers: %q", name)
				m.store.MarkError(context.Background(), r.server.ID, err.Error())
				m.logger.Warn("mcp: cross-server tool name collision", "server", r.server.Name, "error", err)
				continue
			}
			m.store.UpsertCatalogTools(context.Background(), r.server.ID, r.server.Name, r.defs)
			m.store.MarkSynced(context.Background(), r.server.ID, len(r.defs))
			m.logger.Info("mcp: server connected", "server", r.server.Name, "tools", len(r.defs))
			clients = append(clients, r.client)
			tools = append(tools, r.tools...)
		}
	}

	sc.mu.Lock()
	old := sc.clients
	sc.clients = clients
	sc.tools = tools
	sc.sig = sig
	sc.refreshedAt = time.Now()
	sc.mu.Unlock()

	for _, c := range old {
		_ = c.close()
	}
}

// connectServer dials one server, lists its tools, and rejects collisions
// inside that server. Successful catalog publication waits for connectSoul's
// cross-server collision gate. A failure returns a zero connectResult.
func (m *Manager) connectServer(ctx context.Context, srv ServerRow) connectResult {
	sctx, cancel := context.WithTimeout(ctx, serverTimeout)
	defer cancel()

	secret := ""
	if srv.CredentialID != nil && m.credFetch != nil {
		s, err := m.credFetch(sctx, *srv.CredentialID)
		if err != nil {
			m.store.MarkError(context.Background(), srv.ID, "credential: "+err.Error())
			return connectResult{}
		}
		secret = s
	}

	client, err := m.dialServer(sctx, srv, secret)
	if err != nil {
		m.store.MarkError(context.Background(), srv.ID, err.Error())
		m.logger.Warn("mcp: dial failed", "server", srv.Name, "error", err)
		return connectResult{}
	}
	defs, err := client.listTools(sctx)
	if err != nil {
		_ = client.close()
		m.store.MarkError(context.Background(), srv.ID, err.Error())
		m.logger.Warn("mcp: tools/list failed", "server", srv.Name, "error", err)
		return connectResult{}
	}

	tools := make([]core.MCPTool, 0, len(defs))
	resolvedNames := make(map[string]struct{}, len(defs))
	for _, d := range defs {
		resolvedName := NamespacedName(srv.Name, d.Name)
		if _, exists := resolvedNames[resolvedName]; exists {
			_ = client.close()
			err := fmt.Errorf("tool name collision after normalization: %q", resolvedName)
			m.store.MarkError(context.Background(), srv.ID, err.Error())
			m.logger.Warn("mcp: duplicate resolved tool name", "server", srv.Name, "error", err)
			return connectResult{}
		}
		resolvedNames[resolvedName] = struct{}{}
		origName := d.Name
		cl := client
		tools = append(tools, core.MCPTool{
			Name:        resolvedName,
			Description: d.Description,
			Schema:      d.InputSchema,
			Handler: func(hctx context.Context, input json.RawMessage) (any, error) {
				return cl.callTool(hctx, origName, input)
			},
		})
	}
	return connectResult{server: srv, client: client, tools: tools, defs: defs}
}

// Invalidate drops a soul's cached connections; the next ToolsForSoul
// reconnects. Called when the cabinet changes the soul's MCP config.
func (m *Manager) Invalidate(soulID uuid.UUID) {
	m.mu.Lock()
	sc := m.souls[soulID]
	delete(m.souls, soulID)
	m.mu.Unlock()
	if sc == nil {
		return
	}
	sc.mu.Lock()
	clients := sc.clients
	sc.mu.Unlock()
	for _, c := range clients {
		_ = c.close()
	}
}

// CloseAll tears down every connection — call on daemon shutdown.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	souls := m.souls
	m.souls = make(map[uuid.UUID]*soulConns)
	m.mu.Unlock()
	for _, sc := range souls {
		sc.mu.Lock()
		clients := sc.clients
		sc.mu.Unlock()
		for _, c := range clients {
			_ = c.close()
		}
	}
}

// NamespacedName builds the registry tool name mcp__<label>__<tool>. Long
// names keep a readable prefix and gain a stable hash suffix within the
// model's 64-byte tool-name limit, so differences after byte 64 do not alias.
func NamespacedName(label, tool string) string {
	n := "mcp__" + sanitize(label) + "__" + sanitize(tool)
	if len(n) > 64 {
		digest := sha256.Sum256([]byte(n))
		suffix := fmt.Sprintf("_%x", digest[:6])
		n = n[:64-len(suffix)] + suffix
	}
	return n
}

// sanitize lowercases s and replaces every char outside [a-z0-9_] with _.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
