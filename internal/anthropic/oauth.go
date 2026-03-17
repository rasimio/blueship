package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	tokenURL        = "https://platform.claude.com/v1/oauth/token"
	defaultClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	refreshMargin   = 5 * time.Minute
)

// OAuthConfig configures OAuth token refresh for the Anthropic provider.
type OAuthConfig struct {
	RefreshToken string
	ClientID     string                                 // defaults to Claude Code client ID
	OnRefresh    func(accessToken, refreshToken string) // called after successful refresh
}

type oauthState struct {
	mu           sync.Mutex
	refreshToken string
	clientID     string
	expiresAt    time.Time
	onRefresh    func(accessToken, refreshToken string)
	httpClient   *http.Client
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func newOAuthState(cfg OAuthConfig) *oauthState {
	clientID := cfg.ClientID
	if clientID == "" {
		clientID = defaultClientID
	}
	return &oauthState{
		refreshToken: cfg.RefreshToken,
		clientID:     clientID,
		expiresAt:    time.Now(), // trigger refresh on first use
		onRefresh:    cfg.OnRefresh,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
}

// needsRefresh returns true if the token will expire within refreshMargin.
// Must be called with mu held.
func (o *oauthState) needsRefresh() bool {
	return time.Now().Add(refreshMargin).After(o.expiresAt)
}

// doRefresh exchanges the refresh token for a new access token.
// Must be called with mu held.
func (o *oauthState) doRefresh() (string, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": o.refreshToken,
		"client_id":     o.clientID,
	})

	resp, err := o.httpClient.Post(tokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("oauth refresh request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth refresh status %d: %s", resp.StatusCode, respBody)
	}

	var tok tokenResponse
	if err := json.Unmarshal(respBody, &tok); err != nil {
		return "", fmt.Errorf("oauth refresh decode: %w", err)
	}

	if tok.RefreshToken != "" {
		o.refreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		o.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}

	if o.onRefresh != nil {
		o.onRefresh(tok.AccessToken, o.refreshToken)
	}

	return tok.AccessToken, nil
}
