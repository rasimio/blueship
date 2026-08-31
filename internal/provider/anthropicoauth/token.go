// Package anthropicoauth provides OAuth token management for Anthropic's
// Claude Code subscription flow. Tokens are minted by the standalone login
// CLI (cmd/anthropic-login) and refreshed automatically here. The companion
// CompletionProvider lives in package anthropic — it accepts a bearer-token
// source so the same request code path handles both API-key and OAuth auth.
package anthropicoauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// TokenURL is the Anthropic OAuth token endpoint (also used for refresh).
	//
	// Was console.anthropic.com until 2026-07-31, when that host stopped
	// serving this path: it answers 429 rate_limit_error to a plain client
	// and 404 not_found_error to one sending a claude-cli User-Agent, for
	// both the authorization_code and refresh_token grants. The 429 is not a
	// real rate limit — it is not IP-scoped and does not decay, so a refresh
	// failure here reads as throttling when the host is simply gone.
	TokenURL = "https://claude.ai/v1/oauth/token"

	// ClientID is the public Claude Code OAuth client_id. This is the same
	// id Claude Code's CLI uses — Anthropic publishes it as the well-known
	// public client for subscription-authed inference.
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	refreshBuffer  = 60 * time.Second
	refreshTimeout = 30 * time.Second
)

// refreshBackoffs paces retries of a failed refresh. A transient 5xx or a
// dropped connection used to surface as a failed completion, because the
// single refresh attempt was the whole budget; the caller then had no token
// until the next request happened to arrive.
var refreshBackoffs = []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second}

// ErrRefreshRejected reports a refresh the endpoint refused outright. The
// refresh token rotates on every exchange and is single-use, so this means
// the chain has moved on without us — another process refreshed from the same
// pair, or the session was revoked. No retry re-mints it; only a fresh
// interactive login does.
var ErrRefreshRejected = errors.New("anthropic-oauth: refresh token rejected")

// refreshHTTPClient is used for token-refresh requests so a hung OAuth
// endpoint can't hang the agent loop. Uses default transport so it still
// respects HTTPS_PROXY for geo-routed deployments.
var refreshHTTPClient = &http.Client{Timeout: refreshTimeout}

// TokenData holds persisted OAuth credentials.
type TokenData struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ExpiresAt int64  `json:"expires_at"` // unix seconds
}

// Status is a snapshot of token health, for operator-facing readiness checks.
// LastError is the most recent refresh failure and is cleared by the next
// success, so a non-empty value means the store is currently unable to mint
// access tokens.
type Status struct {
	Configured  bool
	ExpiresAt   time.Time
	LastRefresh time.Time
	LastError   string
	Rejected    bool // refresh token is dead; needs an interactive re-login
}

// TokenStore manages OAuth token persistence and automatic refresh.
type TokenStore struct {
	mu       sync.RWMutex
	filePath string
	data     TokenData
	logger   *slog.Logger

	lastRefresh time.Time
	lastErr     error
}

// NewTokenStore creates a token store that reads/writes tokens to filePath.
func NewTokenStore(filePath string, logger *slog.Logger) *TokenStore {
	return &TokenStore{filePath: filePath, logger: logger}
}

// Load reads tokens from disk. Returns nil if file doesn't exist.
func (s *TokenStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.filePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}

	var data TokenData
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("parse token file: %w", err)
	}
	s.data = data
	return nil
}

// Save writes tokens to disk atomically.
func (s *TokenStore) Save() error {
	s.mu.RLock()
	data := s.data
	s.mu.RUnlock()
	return writeTokenFile(s.filePath, data)
}

// writeTokenFile atomically persists token data. It's lock-free (data passed by
// value) so it can be called both from Save (under RLock) and from
// refreshLocked (which already holds the write lock) without deadlocking.
//
// The pair being replaced is kept alongside as <path>.prev. This is a manual
// escape hatch, not an automatic fallback: if a rotation lands but the new
// access token never works, an operator can inspect or restore the previous
// pair instead of having nothing at all to look at. The live file is never
// silently rolled back to it — a rotated-away refresh token is usually dead,
// and quietly retrying a dead one would only mask the need to re-login.
func writeTokenFile(filePath string, data TokenData) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}

	if prev, err := os.ReadFile(filePath); err == nil {
		_ = os.WriteFile(filePath+".prev", prev, 0o600)
	}

	tmp := filePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, filePath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename tmp: %w", err)
	}
	return nil
}

// Bootstrap sets the initial refresh token (from config/env).
// No-op if a refresh token is already present in memory.
func (s *TokenStore) Bootstrap(refreshToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Refresh == "" && refreshToken != "" {
		s.data.Refresh = refreshToken
	}
}

// IsConfigured returns true if a refresh token is present.
func (s *TokenStore) IsConfigured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Refresh != ""
}

// Status reports current token health without touching the network.
func (s *TokenStore) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := Status{
		Configured:  s.data.Refresh != "",
		LastRefresh: s.lastRefresh,
	}
	if s.data.ExpiresAt > 0 {
		st.ExpiresAt = time.Unix(s.data.ExpiresAt, 0)
	}
	if s.lastErr != nil {
		st.LastError = s.lastErr.Error()
		st.Rejected = errors.Is(s.lastErr, ErrRefreshRejected)
	}
	return st
}

// Invalidate drops the cached access token, keeping the refresh token, so the
// next AccessToken mints a fresh one.
//
// Call this when Anthropic rejects a token the store still believed valid.
// Expiry is not the only way an access token dies — refreshing the same pair
// from somewhere else retires it, as does revoking the session — and without
// this the store keeps serving the dead token until its nominal expiry. For an
// 8-hour token that is a whole shift of requests 401ing with no self-recovery.
func (s *TokenStore) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Access = ""
	s.data.ExpiresAt = 0
}

// AccessToken returns a valid access token, refreshing if needed.
func (s *TokenStore) AccessToken() (string, error) {
	return s.ensure(refreshBuffer)
}

// EnsureFresh refreshes when the access token expires within lead, and reports
// whether the store currently holds a usable one. A caller that ticks this
// with a lead well above refreshBuffer keeps rotation off the request path: a
// refresh that fails there is retried on the next tick instead of surfacing as
// a failed completion.
func (s *TokenStore) EnsureFresh(lead time.Duration) error {
	_, err := s.ensure(lead)
	return err
}

func (s *TokenStore) ensure(lead time.Duration) (string, error) {
	s.mu.RLock()
	needsRefresh := s.data.Refresh != "" &&
		(s.data.Access == "" || time.Now().Add(lead).Unix() >= s.data.ExpiresAt)
	token := s.data.Access
	s.mu.RUnlock()

	if !needsRefresh {
		if token == "" {
			return "", fmt.Errorf("anthropic-oauth: no access token and no refresh token")
		}
		return token, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock.
	if s.data.Access != "" && time.Now().Add(lead).Unix() < s.data.ExpiresAt {
		return s.data.Access, nil
	}

	if err := s.refreshLocked(); err != nil {
		// Rotation race: another agent sharing this refresh_token may have
		// just rotated it server-side. Reload from disk in case the other
		// agent persisted the new pair, then retry once.
		if reloadErr := s.reloadFromDiskLocked(); reloadErr == nil {
			if retryErr := s.refreshOnceLocked(); retryErr == nil {
				s.lastErr = nil
				s.lastRefresh = time.Now()
				return s.data.Access, nil
			}
		}
		return "", err
	}
	return s.data.Access, nil
}

func (s *TokenStore) reloadFromDiskLocked() error {
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}
	var data TokenData
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	if data.Refresh == "" || data.Refresh == s.data.Refresh {
		return fmt.Errorf("on-disk token unchanged")
	}
	s.data = data
	return nil
}

// SetTokens stores new token data and saves to disk. Used by the login CLI
// after the initial code exchange.
func (s *TokenStore) SetTokens(data TokenData) error {
	s.mu.Lock()
	s.data = data
	s.lastErr = nil
	s.mu.Unlock()
	return s.Save()
}

// refreshLocked retries a failed refresh with backoff, stopping early once the
// endpoint says the token itself is bad. The sleeps happen under the write
// lock: concurrent callers block, but they are all waiting on the same token
// and would fail without it anyway — serializing here is what keeps two
// refreshes from racing and burning each other's single-use refresh token.
func (s *TokenStore) refreshLocked() error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		err := s.refreshOnceLocked()
		if err == nil {
			s.lastErr = nil
			s.lastRefresh = time.Now()
			return nil
		}
		lastErr = err

		if errors.Is(err, ErrRefreshRejected) || attempt >= len(refreshBackoffs) {
			break
		}
		s.logger.Warn("anthropic-oauth: refresh failed, retrying",
			"error", err,
			"attempt", attempt+1,
			"backoff", refreshBackoffs[attempt],
		)
		time.Sleep(refreshBackoffs[attempt])
	}
	s.lastErr = lastErr
	return lastErr
}

func (s *TokenStore) refreshOnceLocked() error {
	s.logger.Info("anthropic-oauth: refreshing access token")

	reqBody := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": s.data.Refresh,
		"client_id":     ClientID,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal refresh request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", TokenURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("create refresh request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := refreshHTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		json.NewDecoder(resp.Body).Decode(&errBody)
		err := fmt.Errorf("refresh failed (%d): %s — %s", resp.StatusCode, errBody.Error, errBody.Description)
		// invalid_grant is OAuth's "this token is not yours to spend" and a
		// 401 says the same about the client. Both are terminal; everything
		// else (5xx, the 429 the retired console host still emits) is worth
		// another attempt.
		if errBody.Error == "invalid_grant" || resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%w: %s", ErrRefreshRejected, err)
		}
		return err
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}

	if tokenResp.AccessToken == "" || tokenResp.RefreshToken == "" {
		return fmt.Errorf("refresh response missing tokens")
	}

	expiresAt := time.Now().Unix() + int64(tokenResp.ExpiresIn)

	s.data = TokenData{
		Access:    tokenResp.AccessToken,
		Refresh:   tokenResp.RefreshToken,
		ExpiresAt: expiresAt,
	}

	// Persist synchronously BEFORE returning. The refresh token rotates
	// server-side on every refresh, so an async save that a restart (a deploy)
	// catches mid-flight leaves the now-invalid OLD token on disk — the next
	// start then fails with invalid_grant (exactly what bit prod 2026-06-20
	// during a day of frequent redeploys). We already hold the write lock, so
	// call the lock-free writer directly.
	if err := writeTokenFile(s.filePath, s.data); err != nil {
		s.logger.Error("anthropic-oauth: save refreshed tokens", "error", err)
	}

	s.logger.Info("anthropic-oauth: token refreshed", "expires_in", tokenResp.ExpiresIn)
	return nil
}
