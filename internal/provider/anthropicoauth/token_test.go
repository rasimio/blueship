package anthropicoauth

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// withRefreshTransport points token refresh at fn and drops the backoff sleeps,
// so a retry test costs no wall clock.
func withRefreshTransport(t *testing.T, fn roundTripFunc) {
	t.Helper()
	prevClient, prevBackoffs := refreshHTTPClient, refreshBackoffs
	refreshHTTPClient = &http.Client{Transport: fn}
	refreshBackoffs = []time.Duration{0, 0, 0}
	t.Cleanup(func() { refreshHTTPClient, refreshBackoffs = prevClient, prevBackoffs })
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func newStore(t *testing.T, data TokenData) (*TokenStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.json")
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("seed token file: %v", err)
	}
	s := NewTokenStore(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return s, path
}

func TestRefreshRetriesTransientFailure(t *testing.T) {
	calls := 0
	withRefreshTransport(t, func(*http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return jsonResponse(http.StatusBadGateway, `{"error":"server_error"}`), nil
		}
		return jsonResponse(http.StatusOK, `{"access_token":"acc-new","refresh_token":"ref-new","expires_in":28800}`), nil
	})

	s, _ := newStore(t, TokenData{Refresh: "ref-old"})
	tok, err := s.AccessToken()
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "acc-new" {
		t.Fatalf("token = %q, want acc-new", tok)
	}
	if calls != 3 {
		t.Fatalf("made %d refresh calls, want 3", calls)
	}
	if st := s.Status(); st.LastError != "" {
		t.Fatalf("LastError = %q, want cleared after success", st.LastError)
	}
}

func TestRefreshRejectedFailsFast(t *testing.T) {
	calls := 0
	withRefreshTransport(t, func(*http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"refresh token not found"}`), nil
	})

	s, _ := newStore(t, TokenData{Refresh: "ref-dead"})
	_, err := s.AccessToken()
	if err == nil {
		t.Fatal("want error for a rejected refresh token")
	}
	if !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("error = %v, want ErrRefreshRejected", err)
	}
	// Burning retries on invalid_grant only delays the operator finding out.
	if calls != 1 {
		t.Fatalf("made %d refresh calls, want 1", calls)
	}
	if st := s.Status(); !st.Rejected {
		t.Fatalf("Status().Rejected = false, want true (LastError=%q)", st.LastError)
	}
}

func TestInvalidateForcesRefresh(t *testing.T) {
	calls := 0
	withRefreshTransport(t, func(*http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(http.StatusOK, `{"access_token":"acc-2","refresh_token":"ref-2","expires_in":28800}`), nil
	})

	s, _ := newStore(t, TokenData{
		Access:    "acc-1",
		Refresh:   "ref-1",
		ExpiresAt: time.Now().Add(8 * time.Hour).Unix(),
	})

	if tok, err := s.AccessToken(); err != nil || tok != "acc-1" {
		t.Fatalf("AccessToken = %q, %v; want the cached acc-1", tok, err)
	}
	if calls != 0 {
		t.Fatalf("refreshed %d times for a token valid for 8h, want 0", calls)
	}

	s.Invalidate()

	if tok, err := s.AccessToken(); err != nil || tok != "acc-2" {
		t.Fatalf("AccessToken after Invalidate = %q, %v; want acc-2", tok, err)
	}
	if calls != 1 {
		t.Fatalf("refreshed %d times after Invalidate, want 1", calls)
	}
}

func TestEnsureFreshRefreshesAheadOfExpiry(t *testing.T) {
	calls := 0
	withRefreshTransport(t, func(*http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(http.StatusOK, `{"access_token":"acc-2","refresh_token":"ref-2","expires_in":28800}`), nil
	})

	// Five minutes left: inside a 15-minute lead, outside the 60-second
	// buffer the request path uses.
	s, _ := newStore(t, TokenData{
		Access:    "acc-1",
		Refresh:   "ref-1",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
	})

	if _, err := s.AccessToken(); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if calls != 0 {
		t.Fatalf("request path refreshed %d times with 5 min left, want 0", calls)
	}

	if err := s.EnsureFresh(15 * time.Minute); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if calls != 1 {
		t.Fatalf("EnsureFresh refreshed %d times, want 1", calls)
	}
}

func TestRotationPersistsPairAndKeepsPrevious(t *testing.T) {
	withRefreshTransport(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"access_token":"acc-2","refresh_token":"ref-2","expires_in":28800}`), nil
	})

	s, path := newStore(t, TokenData{Refresh: "ref-1"})
	if _, err := s.AccessToken(); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	var onDisk TokenData
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parse token file: %v", err)
	}
	// The rotated refresh token must be on disk before the call returns —
	// a restart between refresh and save strands the process on a dead token.
	if onDisk.Refresh != "ref-2" || onDisk.Access != "acc-2" {
		t.Fatalf("on-disk pair = %+v, want the rotated one", onDisk)
	}
	if onDisk.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expires_at = %d, want a future timestamp", onDisk.ExpiresAt)
	}

	prev, err := os.ReadFile(path + ".prev")
	if err != nil {
		t.Fatalf("read .prev: %v", err)
	}
	var prevData TokenData
	if err := json.Unmarshal(prev, &prevData); err != nil {
		t.Fatalf("parse .prev: %v", err)
	}
	if prevData.Refresh != "ref-1" {
		t.Fatalf(".prev refresh = %q, want the superseded ref-1", prevData.Refresh)
	}
}
