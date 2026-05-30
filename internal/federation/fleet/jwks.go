package fleet

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWKSCache fetches and caches Fleet's public keys for JWT validation.
// Refreshes opportunistically: if a token's kid is unknown, it forces one
// re-fetch before failing. A background goroutine refreshes every
// RefreshInterval to amortise key rotation.
type JWKSCache struct {
	baseURL  string
	http     *http.Client
	logger   *slog.Logger
	interval time.Duration

	mu   sync.RWMutex
	keys map[string]*rsa.PublicKey
}

// NewJWKSCache constructs a JWKS cache pointed at Fleet's public base URL.
// interval defaults to 30 minutes if zero.
func NewJWKSCache(baseURL string, interval time.Duration, logger *slog.Logger) *JWKSCache {
	if interval == 0 {
		interval = 30 * time.Minute
	}
	return &JWKSCache{
		baseURL:  strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: 15 * time.Second},
		logger:   logger,
		interval: interval,
		keys:     map[string]*rsa.PublicKey{},
	}
}

// Run starts the background refresh loop until ctx is cancelled. Initial
// fetch happens before the loop so the first inbound JWT does not hit a
// cold cache.
func (j *JWKSCache) Run(ctx context.Context) {
	if err := j.Refresh(ctx); err != nil {
		j.logger.Warn("jwks: initial fetch failed", "error", err)
	}
	t := time.NewTicker(j.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := j.Refresh(ctx); err != nil {
				j.logger.Warn("jwks: refresh failed", "error", err)
			}
		}
	}
}

// Refresh re-fetches /.well-known/jwks.json and replaces the cache atomically.
func (j *JWKSCache) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.baseURL+"/.well-known/jwks.json", nil)
	if err != nil {
		return err
	}
	resp, err := j.http.Do(req)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("jwks decode: %w", err)
	}
	parsed := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			j.logger.Warn("jwks: bad n", "kid", k.Kid, "error", err)
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			j.logger.Warn("jwks: bad e", "kid", k.Kid, "error", err)
			continue
		}
		// e is variable-length big-endian; pad to int.
		var e int
		for _, b := range eb {
			e = (e << 8) | int(b)
		}
		parsed[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	j.mu.Lock()
	j.keys = parsed
	j.mu.Unlock()
	j.logger.Info("jwks: cache populated", "keys", len(parsed))
	return nil
}

// Lookup returns the cached public key for kid. If unknown, triggers a
// single synchronous Refresh and tries again.
func (j *JWKSCache) Lookup(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	j.mu.RLock()
	if k, ok := j.keys[kid]; ok {
		j.mu.RUnlock()
		return k, nil
	}
	j.mu.RUnlock()
	if err := j.Refresh(ctx); err != nil {
		return nil, err
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	if k, ok := j.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

// ValidatedClaims is the subset of JWT fields the Ship actually inspects.
type ValidatedClaims struct {
	CallerAgentID string // sub
	Audience      string // first aud entry
	Scope         string // raw scope claim
	Issuer        string
	Expires       time.Time
}

// Validate parses and validates a Fleet-issued JWT against the cached
// JWKS. Audience must equal expectedAudience (this Ship's own agent_id)
// to defend against token reuse across peers.
func (j *JWKSCache) Validate(ctx context.Context, raw, expectedAudience string) (*ValidatedClaims, error) {
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}))
	type claims struct {
		Scope string `json:"scope"`
		jwt.RegisteredClaims
	}
	tok, err := parser.ParseWithClaims(raw, &claims{}, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("missing kid header")
		}
		return j.Lookup(ctx, kid)
	})
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if !tok.Valid {
		return nil, fmt.Errorf("invalid jwt")
	}
	c, ok := tok.Claims.(*claims)
	if !ok {
		return nil, fmt.Errorf("unexpected claims type")
	}
	aud := ""
	if len(c.Audience) > 0 {
		aud = c.Audience[0]
	}
	if expectedAudience != "" && aud != expectedAudience {
		return nil, fmt.Errorf("audience mismatch: got %q want %q", aud, expectedAudience)
	}
	exp := time.Time{}
	if c.ExpiresAt != nil {
		exp = c.ExpiresAt.Time
	}
	return &ValidatedClaims{
		CallerAgentID: c.Subject,
		Audience:      aud,
		Scope:         c.Scope,
		Issuer:        c.Issuer,
		Expires:       exp,
	}, nil
}
