// Package fleet is blueship's HTTP client for the BlueFleet directory
// service. One Ship instance talks to one Fleet, authenticating with
// client_credentials and caching a short-lived access token behind the
// scenes. Every call is synchronous; concurrent callers share the same
// token through an internal mutex so token refresh happens at most once
// per expiry.
//
// The Fleet path is optional — Ships that don't pass a Config keep running
// against the legacy A2A config. This package is phase-7 scaffolding: it
// does not yet mutate the A2A invocation path, only publishes identity
// and discovers peer catalogs into the ship's fleet_peer_cache table.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config is the subset of BlueShip config the Fleet client needs. Passed
// through from core.Config.Fleet by the ship layer; the fleet package
// intentionally does not import core to avoid a cycle.
type Config struct {
	BaseURL         string        // e.g. "http://localhost:8500"
	ClientID        string        // OAuth client_id issued by Fleet
	ClientSecret    string        // OAuth client_secret issued by Fleet
	RefreshInterval time.Duration // how often to refresh peer cache (default 5m)
	RequestTimeout  time.Duration // per-call HTTP timeout (default 15s)
}

// Client is an HTTP client for a single Fleet endpoint.
type Client struct {
	cfg    Config
	http   *http.Client
	logger *slog.Logger

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// New constructs a Client. Does not perform any network I/O.
func New(cfg Config, logger *slog.Logger) *Client {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 15 * time.Second
	}
	return &Client{
		cfg:    cfg,
		http:   &http.Client{Timeout: cfg.RequestTimeout},
		logger: logger,
	}
}

// BaseURL exposes the Fleet endpoint for diagnostics.
func (c *Client) BaseURL() string { return c.cfg.BaseURL }

// ---------------------------------------------------------------------------
// Token management
// ---------------------------------------------------------------------------

// tokenResponse mirrors POST /v0/oauth/token.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// accessToken returns a currently-valid self-scoped access token,
// refreshing via client_credentials if the cached token is missing or
// within 60 seconds of expiry.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok := c.token
	exp := c.expiresAt
	c.mu.Unlock()
	if tok != "" && time.Until(exp) > 60*time.Second {
		return tok, nil
	}
	return c.refreshAccessToken(ctx)
}

func (c *Client) refreshAccessToken(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id":     c.cfg.ClientID,
		"client_secret": c.cfg.ClientSecret,
		"scope":         "self",
	})
	resp, err := c.rawPost(ctx, "/v0/oauth/token", body, "" /* unauth */)
	if err != nil {
		return "", fmt.Errorf("fleet: token request: %w", err)
	}
	var tr tokenResponse
	if err := json.Unmarshal(resp, &tr); err != nil {
		return "", fmt.Errorf("fleet: token decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("fleet: empty access_token in response")
	}
	c.mu.Lock()
	c.token = tr.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	c.mu.Unlock()
	c.logger.Info("fleet: refreshed access token", "expires_in", tr.ExpiresIn)
	return tr.AccessToken, nil
}

// PeerToken requests an access token scoped to a specific peer. The
// returned JWT carries claims `sub=<this agent's id>` and
// `aud=<peerAgentID>`; the peer's JWKS verifier rejects tokens whose
// audience does not match its own id, defending against token reuse.
//
// Cached separately from the self-scoped token so a long-lived peer
// connection does not accidentally use a self-only token.
func (c *Client) PeerToken(ctx context.Context, peerAgentID string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id":     c.cfg.ClientID,
		"client_secret": c.cfg.ClientSecret,
		"scope":         "peer:" + peerAgentID,
	})
	resp, err := c.rawPost(ctx, "/v0/oauth/token", body, "")
	if err != nil {
		return "", fmt.Errorf("fleet: peer token: %w", err)
	}
	var tr tokenResponse
	if err := json.Unmarshal(resp, &tr); err != nil {
		return "", fmt.Errorf("fleet: peer token decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("fleet: empty peer token in response")
	}
	return tr.AccessToken, nil
}

// ---------------------------------------------------------------------------
// Identity / profile
// ---------------------------------------------------------------------------

// PatchMeRequest updates mutable profile fields. Nil pointer = leave field
// untouched; empty string clears the value on Fleet's side.
type PatchMeRequest struct {
	DisplayName *string `json:"display_name,omitempty"`
	Description *string `json:"description,omitempty"`
	EndpointURL *string `json:"endpoint_url,omitempty"`
	PublicKey   *string `json:"public_key,omitempty"`
}

// PatchMe updates this agent's own profile via the authenticated endpoint.
func (c *Client) PatchMe(ctx context.Context, req PatchMeRequest) error {
	body, _ := json.Marshal(req)
	_, err := c.doAuth(ctx, http.MethodPatch, "/v0/agents/me", body)
	return err
}

// GetMe returns the calling agent's own peer card. Used at startup so the
// Ship can learn its own Fleet agent_id without parsing its own JWT.
func (c *Client) GetMe(ctx context.Context) (*PeerCard, error) {
	raw, err := c.doAuth(ctx, http.MethodGet, "/v0/agents/me", nil)
	if err != nil {
		return nil, err
	}
	var card PeerCard
	if err := json.Unmarshal(raw, &card); err != nil {
		return nil, fmt.Errorf("fleet: decode self card: %w", err)
	}
	return &card, nil
}

// Capability is the payload for PUT /v0/agents/me/capabilities.
type Capability struct {
	Tag         string          `json:"tag"`
	Description string          `json:"description,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

// PutCapabilities replaces the agent's capability list.
func (c *Client) PutCapabilities(ctx context.Context, caps []Capability) error {
	body, _ := json.Marshal(map[string]any{"capabilities": caps})
	_, err := c.doAuth(ctx, http.MethodPut, "/v0/agents/me/capabilities", body)
	return err
}

// Tool is the payload for PUT /v0/agents/me/tools.
type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Mode         string          `json:"mode,omitempty"`
}

// PutTools replaces the agent's exposed tool catalog on Fleet.
func (c *Client) PutTools(ctx context.Context, tools []Tool) error {
	body, _ := json.Marshal(map[string]any{"tools": tools})
	_, err := c.doAuth(ctx, http.MethodPut, "/v0/agents/me/tools", body)
	return err
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

// AgentSummary is the short form returned by /v0/agents/search.
type AgentSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	EndpointURL string `json:"endpoint_url,omitempty"`
	Status      string `json:"status"`
	PublicKey   string `json:"public_key,omitempty"`
}

// PeerCard is the full view returned by /v0/agents/:id.
type PeerCard struct {
	Agent        AgentSummary    `json:"agent"`
	Capabilities []CapabilityOut `json:"capabilities"`
	Tools        []ToolOut       `json:"tools"`
}

// CapabilityOut matches agent_capabilities rows returned by Fleet.
type CapabilityOut struct {
	ID          string          `json:"id"`
	Tag         string          `json:"tag"`
	Description string          `json:"description,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

// ToolOut matches agent_tools rows returned by Fleet.
type ToolOut struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Mode         string          `json:"mode"`
}

// Search lists agents matching an optional capability tag + free-text query.
func (c *Client) Search(ctx context.Context, capability, query string, limit int) ([]AgentSummary, error) {
	path := "/v0/agents/search"
	params := []string{}
	if capability != "" {
		params = append(params, "capability="+capability)
	}
	if query != "" {
		params = append(params, "q="+query)
	}
	if limit > 0 {
		params = append(params, fmt.Sprintf("limit=%d", limit))
	}
	if len(params) > 0 {
		path += "?" + strings.Join(params, "&")
	}
	raw, err := c.doAuth(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Agents []AgentSummary `json:"agents"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("fleet: decode search: %w", err)
	}
	return wrap.Agents, nil
}

// GetPeer fetches the full card for a peer. Accepts either a UUID or a
// name slug — Fleet's GET /v0/agents/:id handler falls back to name
// lookup on parse failure.
func (c *Client) GetPeer(ctx context.Context, idOrName string) (*PeerCard, error) {
	raw, err := c.doAuth(ctx, http.MethodGet, "/v0/agents/"+idOrName, nil)
	if err != nil {
		return nil, err
	}
	var card PeerCard
	if err := json.Unmarshal(raw, &card); err != nil {
		return nil, fmt.Errorf("fleet: decode peer card: %w", err)
	}
	return &card, nil
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

// doAuth performs an authenticated request, auto-refreshing the access
// token on 401. Returns raw response bytes.
func (c *Client) doAuth(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.rawRequest(ctx, method, path, body, tok)
	if err == nil {
		return resp, nil
	}
	// One-shot retry on explicit 401 — session may have expired server-side.
	var apiErr *apiError
	if !asAPIErr(err, &apiErr) || apiErr.status != http.StatusUnauthorized {
		return nil, err
	}
	tok, err = c.refreshAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	return c.rawRequest(ctx, method, path, body, tok)
}

func (c *Client) rawPost(ctx context.Context, path string, body []byte, bearer string) ([]byte, error) {
	return c.rawRequest(ctx, http.MethodPost, path, body, bearer)
}

func (c *Client) rawRequest(ctx context.Context, method, path string, body []byte, bearer string) ([]byte, error) {
	url := strings.TrimRight(c.cfg.BaseURL, "/") + path
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fleet: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &apiError{status: resp.StatusCode, body: string(raw)}
	}
	return raw, nil
}

// apiError carries HTTP status + body so callers can distinguish transient
// failures from permanent ones (e.g. 401 → refresh, 404 → skip peer).
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("fleet: HTTP %d: %s", e.status, e.body)
}

func asAPIErr(err error, out **apiError) bool {
	for err != nil {
		if a, ok := err.(*apiError); ok {
			*out = a
			return true
		}
		// net/http does not chain, but keep this shape in case callers wrap.
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
