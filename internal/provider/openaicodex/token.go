package openaicodex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// tokenURL is a var, not a const, so the refresh path can be exercised
// against a stub. It is the only untested branch that can destroy a
// credential outright, which is a poor thing to leave unreachable.
var tokenURL = "https://auth.openai.com/oauth/token"

const (
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	jwtAuthClaim = "https://api.openai.com/auth"

	refreshBuffer  = 60 * time.Second
	refreshTimeout = 30 * time.Second
)

// refreshHTTPClient is used for token-refresh requests so a hung OAuth
// endpoint can't hang the whole agent loop. Uses default transport so it
// still respects HTTPS_PROXY for geo-routed deployments.
var refreshHTTPClient = &http.Client{Timeout: refreshTimeout}

// TokenData holds persisted OAuth credentials.
type TokenData struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ExpiresAt int64  `json:"expires_at"` // unix seconds
	AccountID string `json:"account_id"`
}

// TokenStore manages OAuth token persistence and automatic refresh.
type TokenStore struct {
	mu       sync.RWMutex
	filePath string
	data     TokenData
	logger   *slog.Logger
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
	defer s.mu.RUnlock()
	return s.saveLocked()
}

// saveLocked is Save for a caller that already holds the lock — the
// refresh path does, and must persist the rotated token before it
// returns rather than racing a restart to it.
func (s *TokenStore) saveLocked() error {
	data := s.data

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}

	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}

	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.filePath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename tmp: %w", err)
	}
	return nil
}

// Bootstrap sets the initial refresh token (from config/env).
// If a token file already exists with a valid refresh token, this is a no-op.
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

// AccountID returns the stored ChatGPT account ID.
func (s *TokenStore) AccountID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.AccountID
}

// AccessToken returns a valid access token, refreshing if needed.
func (s *TokenStore) AccessToken() (string, error) {
	s.mu.RLock()
	needsRefresh := s.data.Refresh != "" &&
		(s.data.Access == "" || time.Now().Unix()+int64(refreshBuffer.Seconds()) >= s.data.ExpiresAt)
	token := s.data.Access
	s.mu.RUnlock()

	if !needsRefresh {
		if token == "" {
			return "", fmt.Errorf("openai-codex: no access token and no refresh token")
		}
		return token, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock.
	if s.data.Access != "" && time.Now().Unix()+int64(refreshBuffer.Seconds()) < s.data.ExpiresAt {
		return s.data.Access, nil
	}

	if err := s.refreshLocked(); err != nil {
		// Rotation race: another agent that shares this refresh_token may
		// have just rotated it server-side, leaving us with a stale value.
		// Reload from disk in case the other agent persisted the new pair
		// to a shared token file, then retry once.
		if reloadErr := s.reloadFromDiskLocked(); reloadErr == nil {
			if retryErr := s.refreshLocked(); retryErr == nil {
				return s.data.Access, nil
			}
		}
		return "", err
	}
	return s.data.Access, nil
}

// reloadFromDiskLocked re-reads the token file. Caller must hold s.mu.
// Used by AccessToken to recover from cross-agent token rotation races
// when a shared token file is configured. No-op if the file doesn't
// exist or the on-disk refresh token matches what we already have.
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

// SetTokens stores new token data and saves to disk.
func (s *TokenStore) SetTokens(data TokenData) error {
	s.mu.Lock()
	s.data = data
	s.mu.Unlock()
	return s.Save()
}

func (s *TokenStore) refreshLocked() error {
	s.logger.Info("openai-codex: refreshing access token")

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.data.Refresh},
		"client_id":     {clientID},
	}
	resp, err := refreshHTTPClient.PostForm(tokenURL, form)
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh failed (%d): %s", resp.StatusCode, decodeOAuthError(resp.Body))
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

	accountID := extractAccountID(tokenResp.AccessToken)
	expiresAt := time.Now().Unix() + int64(tokenResp.ExpiresIn)

	s.data = TokenData{
		Access:    tokenResp.AccessToken,
		Refresh:   tokenResp.RefreshToken,
		ExpiresAt: expiresAt,
		AccountID: accountID,
	}

	// Persisted before returning, not in the background.
	//
	// The refresh token rotates: this response consumed the old one and
	// the endpoint will answer refresh_token_reused to anyone who tries
	// it again. So the new one existing only in this process's memory is
	// a credential one kill away from being lost — and losing it is not
	// a retry, it is the end of the chain. Nothing recovers it but a
	// human signing in again.
	//
	// This daemon is restarted by every deploy, with kickstart -k, at
	// arbitrary moments. A background write lost that race at least
	// once: both the seed token in the environment and the rotated one
	// on disk came back refresh_token_reused, ten days apart from any
	// successful refresh.
	//
	// The write is one small atomic rename. Blocking the caller on it is
	// the cheaper half of the trade by a wide margin.
	if err := s.saveLocked(); err != nil {
		s.logger.Error("openai-codex: save refreshed tokens", "error", err)
	}

	s.logger.Info("openai-codex: token refreshed", "expires_in", tokenResp.ExpiresIn, "account_id", accountID)
	return nil
}

// extractAccountID decodes the JWT access token and extracts chatgpt_account_id.
// decodeOAuthError renders whatever the token endpoint said about a
// failure, in either shape it uses.
//
// RFC 6749 says the body carries error and error_description as strings.
// OpenAI's endpoint answers with an object: {"error":{"message":...,
// "code":"refresh_token_reused"}}. Decoding it into a string field left
// both empty, so every failure logged as "refresh failed (401):  — " —
// and a burned refresh token, a revoked one and a dead endpoint all
// looked exactly alike. The endpoint had been naming the cause the whole
// time; we were throwing it away.
func decodeOAuthError(body io.Reader) string {
	var raw struct {
		Error       json.RawMessage `json:"error"`
		Description string          `json:"error_description"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return "unreadable error body: " + err.Error()
	}

	// The object shape.
	var obj struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(raw.Error, &obj); err == nil && (obj.Code != "" || obj.Message != "") {
		if obj.Code == "" {
			return obj.Message
		}
		return obj.Code + ": " + obj.Message
	}

	// The RFC shape.
	var str string
	if err := json.Unmarshal(raw.Error, &str); err == nil && str != "" {
		if raw.Description == "" {
			return str
		}
		return str + ": " + raw.Description
	}
	if raw.Description != "" {
		return raw.Description
	}
	return "no cause given"
}

func extractAccountID(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return ""
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}

	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}

	auth, ok := claims[jwtAuthClaim].(map[string]any)
	if !ok {
		return ""
	}

	if id, ok := auth["chatgpt_account_id"].(string); ok && id != "" {
		return id
	}
	if id, ok := auth["chatgpt_account_user_id"].(string); ok && id != "" {
		return id
	}
	return ""
}
