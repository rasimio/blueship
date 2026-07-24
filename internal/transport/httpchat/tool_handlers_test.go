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

func TestHandleInvokeToolRequiresBearerAndValidatesOwnership(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	calls := 0
	server := &Server{
		token:  "service-token",
		logger: slog.Default(),
		validateUserSoul: func(_ context.Context, gotUser, gotSoul uuid.UUID) error {
			if gotUser != userID || gotSoul != soulID {
				return errors.New("not owned")
			}
			return nil
		},
		invokeTool: func(_ context.Context, gotUser, gotSoul uuid.UUID, name string, input json.RawMessage) (gateway.ToolInvocation, error) {
			calls++
			return gateway.ToolInvocation{
				Name: name, Input: input, Output: `{"ok":true}`, LatencyMS: 4,
			}, nil
		},
	}
	handler := server.requireBearer(http.HandlerFunc(server.handleInvokeTool))
	body := `{"user_id":"` + userID.String() + `","soul_id":"` + soulID.String() + `","name":"lookup","input":{"q":"x"}}`

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/internal/tools/invoke", strings.NewReader(body)))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("without bearer status=%d", res.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/internal/tools/invoke", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", res.Code, calls, res.Body.String())
	}
	var response struct {
		Invocation toolInvocationView `json:"invocation"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Invocation.Name != "lookup" || response.Invocation.Output != `{"ok":true}` {
		t.Fatalf("response=%+v", response)
	}
}

func TestHandleInvokeToolRejectsForgedSoulAndNonObjectInput(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	server := &Server{
		logger: slog.Default(),
		validateUserSoul: func(context.Context, uuid.UUID, uuid.UUID) error {
			return errors.New("forbidden")
		},
		invokeTool: func(context.Context, uuid.UUID, uuid.UUID, string, json.RawMessage) (gateway.ToolInvocation, error) {
			t.Fatal("invoke called")
			return gateway.ToolInvocation{}, nil
		},
	}
	body := `{"user_id":"` + userID.String() + `","soul_id":"` + soulID.String() + `","name":"x","input":{}}`
	res := httptest.NewRecorder()
	server.handleInvokeTool(res, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	if res.Code != http.StatusForbidden {
		t.Fatalf("forged soul status=%d", res.Code)
	}

	server.validateUserSoul = nil
	body = `{"user_id":"` + userID.String() + `","soul_id":"` + soulID.String() + `","name":"x","input":[]}`
	res = httptest.NewRecorder()
	server.handleInvokeTool(res, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("array input status=%d", res.Code)
	}
}
