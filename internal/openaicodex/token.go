package openaicodex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	tokenURL = "https://auth.openai.com/oauth/token"
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	jwtAuthClaim = "https://api.openai.com/auth"

	refreshBuffer = 60 * time.Second
)

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
	data := s.data
	s.mu.RUnlock()

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
		return "", err
	}
	return s.data.Access, nil
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

	resp, err := http.PostForm(tokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.data.Refresh},
		"client_id":     {clientID},
	})
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
		return fmt.Errorf("refresh failed (%d): %s — %s", resp.StatusCode, errBody.Error, errBody.Description)
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

	// Persist in background — don't block the caller on disk I/O.
	go func() {
		if err := s.Save(); err != nil {
			s.logger.Error("openai-codex: save refreshed tokens", "error", err)
		}
	}()

	s.logger.Info("openai-codex: token refreshed", "expires_in", tokenResp.ExpiresIn, "account_id", accountID)
	return nil
}

// extractAccountID decodes the JWT access token and extracts chatgpt_account_id.
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
