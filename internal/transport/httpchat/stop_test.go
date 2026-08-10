package httpchat

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/gateway"
)

func stopTestServer(t *testing.T, owner, soul uuid.UUID) *Server {
	t.Helper()
	return &Server{
		gw:     &gateway.Gateway{},
		logger: slog.New(slog.DiscardHandler),
		validateUserSoul: func(_ context.Context, gotUser, gotSoul uuid.UUID) error {
			if gotUser != owner || gotSoul != soul {
				return errors.New("not owned")
			}
			return nil
		},
	}
}

// Knowing the service token is not the same as being allowed to touch a given
// soul: vaelum holds one token for every user it relays.
func TestHandleStopRefusesAConversationTheCallerDoesNotOwn(t *testing.T) {
	owner, soul := uuid.New(), uuid.New()
	server := stopTestServer(t, owner, soul)

	body := `{"user_id":"` + uuid.New().String() + `","soul_id":"` + soul.String() + `"}`
	res := httptest.NewRecorder()
	server.handleStop(res, httptest.NewRequest(http.MethodPost, "/chat/stop", strings.NewReader(body)))

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
}

func TestHandleStopRejectsMalformedIdentifiers(t *testing.T) {
	owner, soul := uuid.New(), uuid.New()
	server := stopTestServer(t, owner, soul)

	for _, body := range []string{
		`{"user_id":"not-a-uuid","soul_id":"` + soul.String() + `"}`,
		`{"user_id":"` + owner.String() + `","soul_id":"nope"}`,
		`{`,
	} {
		res := httptest.NewRecorder()
		server.handleStop(res, httptest.NewRequest(http.MethodPost, "/chat/stop", strings.NewReader(body)))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("body %q → status %d, want 400", body, res.Code)
		}
	}
}

// A stop for a conversation that just finished is an ordinary race, not an
// error: the client asked in good faith and the answer arrived first.
func TestHandleStopReportsNothingStoppedOnAnIdleConversation(t *testing.T) {
	owner, soul := uuid.New(), uuid.New()
	server := stopTestServer(t, owner, soul)

	body := `{"user_id":"` + owner.String() + `","soul_id":"` + soul.String() + `","turn_id":"whatever"}`
	res := httptest.NewRecorder()
	server.handleStop(res, httptest.NewRequest(http.MethodPost, "/chat/stop", strings.NewReader(body)))

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	var out stopResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Stopped {
		t.Fatal("stopped = true with no turn running")
	}
}

// The cabinet asks this on load. Before it existed the answer was a guess
// ("the newest row is a user message and it is under 30 minutes old"), which
// left a permanent fake loader whenever a turn died.
func TestHandleStateReportsAnIdleConversation(t *testing.T) {
	owner, soul := uuid.New(), uuid.New()
	server := stopTestServer(t, owner, soul)

	req := httptest.NewRequest(http.MethodGet,
		"/chat/state?user_id="+owner.String()+"&soul_id="+soul.String(), nil)
	res := httptest.NewRecorder()
	server.handleState(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	var out stateResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Streaming || out.TurnID != "" {
		t.Fatalf("idle conversation reported as streaming: %+v", out)
	}
}

func TestHandleStateRefusesAConversationTheCallerDoesNotOwn(t *testing.T) {
	owner, soul := uuid.New(), uuid.New()
	server := stopTestServer(t, owner, soul)

	req := httptest.NewRequest(http.MethodGet,
		"/chat/state?user_id="+uuid.New().String()+"&soul_id="+soul.String(), nil)
	res := httptest.NewRecorder()
	server.handleState(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
}

// Both routes sit on the shared mux, so the bearer middleware has to cover
// them — stopping someone else's answer needs no more than an unauthed POST
// otherwise.
func TestStopRoutesAreBehindTheBearerToken(t *testing.T) {
	owner, soul := uuid.New(), uuid.New()
	server := stopTestServer(t, owner, soul)
	server.token = "service-token"
	handler := server.handler()

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/chat/stop",
			strings.NewReader(`{"user_id":"`+owner.String()+`","soul_id":"`+soul.String()+`"}`)),
		httptest.NewRequest(http.MethodGet,
			"/chat/state?user_id="+owner.String()+"&soul_id="+soul.String(), nil),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s → status %d, want 401", req.Method, req.URL.Path, res.Code)
		}
	}
}
