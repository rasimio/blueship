package openaicodex

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The token endpoint names the cause of every refusal, and this decoder
// is the only thing standing between that name and the log.
//
// It was decoding OpenAI's {"error":{...}} object into a string field,
// so every failure read "refresh failed (401):  — ": a burned refresh
// token, a revoked one and an unreachable endpoint all looked identical.
// The one word that would have ended the search — refresh_token_reused —
// was in the response body the whole time.
func TestOAuthErrorSaysWhatTheEndpointSaid(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			// What auth.openai.com actually returns.
			name: "OpenAI's object shape",
			body: `{"error":{"message":"Your refresh token has already been used to generate a new access token. Please try signing in again.","type":"invalid_request_error","code":"refresh_token_reused"}}`,
			want: []string{"refresh_token_reused", "already been used"},
		},
		{
			name: "an expired token",
			body: `{"error":{"message":"Could not validate your token.","code":"token_expired"}}`,
			want: []string{"token_expired", "Could not validate"},
		},
		{
			// RFC 6749, which other deployments of this endpoint use.
			name: "the standard string shape",
			body: `{"error":"invalid_grant","error_description":"refresh token expired"}`,
			want: []string{"invalid_grant", "refresh token expired"},
		},
		{
			name: "an error with no description",
			body: `{"error":"invalid_client"}`,
			want: []string{"invalid_client"},
		},
		{
			name: "a description with no error",
			body: `{"error_description":"the account is suspended"}`,
			want: []string{"the account is suspended"},
		},
		// Silence must not read as success, and an HTML error page from a
		// proxy must say so rather than vanishing.
		{name: "an empty body", body: `{}`, want: []string{"no cause given"}},
		{name: "not JSON at all", body: `<html>502 Bad Gateway</html>`, want: []string{"unreadable"}},
	} {
		got := decodeOAuthError(strings.NewReader(tc.body))
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: decodeOAuthError = %q, want it to mention %q", tc.name, got, want)
			}
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s: decoded to nothing, which is the bug this replaced", tc.name)
		}
	}
}

// A refresh consumes the token it was given: the endpoint answers
// refresh_token_reused to anyone who presents it again. So the rotated
// token replacing it has to reach disk, and reach it before the process
// can be killed — this daemon is restarted by every deploy, at arbitrary
// moments, and a lost rotation ends the chain for good. Nothing recovers
// it but a person signing in again.
func TestRefreshPersistsTheRotatedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("refresh_token"); got != "old-token" {
			t.Errorf("presented %q, want the stored token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a.b.c","refresh_token":"rotated-token","expires_in":864000}`)
	}))
	defer srv.Close()

	saved := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = saved }()

	path := filepath.Join(t.TempDir(), "tokens.json")
	s := NewTokenStore(path, slog.New(slog.DiscardHandler))
	s.Bootstrap("old-token")

	s.mu.Lock()
	err := s.refreshLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("refreshLocked: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the rotated token never reached disk: %v", err)
	}
	var on TokenData
	if err := json.Unmarshal(raw, &on); err != nil {
		t.Fatalf("token file is not readable: %v", err)
	}
	if on.Refresh != "rotated-token" {
		t.Errorf("on disk: %q, want the rotated token — the old one is already burned", on.Refresh)
	}
	if on.Access != "a.b.c" {
		t.Errorf("access token on disk = %q", on.Access)
	}
}
