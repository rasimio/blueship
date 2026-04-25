// Package client is blueship's A2A HTTP client. One client instance is
// bound to a peer record loaded from a2a_peers; it exposes Discover (fetch
// /.well-known/agent), Invoke (POST /a2a/invoke), SubscribeEvents (GET
// /a2a/events via SSE), and Cancel helpers. Every outbound call is logged
// in a2a_calls and every incoming event in a2a_events so the ship has a
// full audit trail of its inter-agent conversations.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rasimio/blueship/a2a"
	"github.com/rasimio/blueship/a2a/store"
)

// Tracer is an optional observability hook — it is called every time the
// client starts or completes an outbound call, and for each streamed event
// on an async call. TelegramGroupTracer (in a2a/tracer.go) is the default
// implementation; callers may pass nil to disable tracing.
type Tracer interface {
	TraceInvoke(ctx context.Context, call a2a.Call)
	TraceResult(ctx context.Context, call a2a.Call)
	TraceEvent(ctx context.Context, call a2a.Call, ev a2a.Event)
}

// TokenProvider is an optional callback that returns the bearer token to
// stamp on each outbound request. When set it overrides the static
// peer.AuthToken — used by the Fleet path to mint short-lived JWTs scoped
// to this specific peer. Returning an empty string falls back to the
// peer's static token (so callers can phase in JWT auth without breaking
// the legacy shared-secret path).
type TokenProvider func(ctx context.Context) (string, error)

// Client is a peer-bound A2A client.
type Client struct {
	peer     a2a.Peer
	http     *http.Client
	store    *store.Store
	tracer   Tracer
	logger   *slog.Logger
	tokenSrc TokenProvider
}

// New constructs a Client for a known peer using the peer row's static
// AuthToken as the bearer.
func New(peer a2a.Peer, st *store.Store, tracer Tracer, logger *slog.Logger) *Client {
	return &Client{
		peer: peer,
		http: &http.Client{
			Timeout: 60 * time.Second,
		},
		store:  st,
		tracer: tracer,
		logger: logger,
	}
}

// NewWithTokenProvider constructs a Client whose Authorization header is
// produced by tp on every outbound call. Used by the Fleet path so each
// invocation carries a peer-scoped JWT.
func NewWithTokenProvider(peer a2a.Peer, st *store.Store, tracer Tracer, logger *slog.Logger, tp TokenProvider) *Client {
	c := New(peer, st, tracer, logger)
	c.tokenSrc = tp
	return c
}

// Peer returns the Peer this client is bound to.
func (c *Client) Peer() a2a.Peer {
	return c.peer
}

// Discover fetches /.well-known/agent and caches the result in a2a_peers.
// Returns the decoded AgentCard so the caller can register remote tools
// into its local ToolRegistry.
func (c *Client) Discover(ctx context.Context) (*a2a.AgentCard, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.peer.BaseURL+"/.well-known/agent", nil)
	if err != nil {
		return nil, err
	}
	c.addAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", c.peer.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("discover %s: HTTP %d: %s", c.peer.Name, resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("discover %s: read body: %w", c.peer.Name, err)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		return nil, fmt.Errorf("discover %s: decode: %w", c.peer.Name, err)
	}
	if err := c.store.SaveAgentCard(ctx, c.peer.ID, body); err != nil {
		c.logger.Warn("a2a client: save agent card failed", "peer", c.peer.Name, "error", err)
	}
	return &card, nil
}

// Invoke calls a remote tool. The returned InvokeResponse carries a
// CallID, mode, and (for sync) the output or (for async) a handle + events
// URL. The call is recorded in a2a_calls immediately with direction=out
// and is transitioned to running/done/failed as the HTTP response arrives.
func (c *Client) Invoke(ctx context.Context, tool string, input json.RawMessage, correlationID string) (*a2a.InvokeResponse, error) {
	peerID := c.peer.ID
	var corrPtr *string
	if correlationID != "" {
		corrPtr = &correlationID
	}
	call := a2a.Call{
		PeerID:        &peerID,
		PeerName:      c.peer.Name,
		Direction:     a2a.CallDirectionOut,
		ToolName:      tool,
		CorrelationID: corrPtr,
		Input:         input,
		State:         a2a.CallStatePending,
	}
	callID, err := c.store.CreateCall(ctx, call)
	if err != nil {
		return nil, err
	}
	call.ID = callID
	if c.tracer != nil {
		c.tracer.TraceInvoke(ctx, call)
	}

	body, err := json.Marshal(a2a.InvokeRequest{
		Tool:          tool,
		Input:         input,
		CorrelationID: correlationID,
	})
	if err != nil {
		_ = c.store.UpdateCallState(ctx, callID, a2a.CallStateFailed, nil, err.Error())
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.peer.BaseURL+"/a2a/invoke", bytes.NewReader(body))
	if err != nil {
		_ = c.store.UpdateCallState(ctx, callID, a2a.CallStateFailed, nil, err.Error())
		return nil, err
	}
	c.addAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		_ = c.store.UpdateCallState(ctx, callID, a2a.CallStateFailed, nil, err.Error())
		return nil, fmt.Errorf("invoke %s/%s: %w", c.peer.Name, tool, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = c.store.UpdateCallState(ctx, callID, a2a.CallStateFailed, nil, err.Error())
		return nil, err
	}

	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error a2a.APIError `json:"error"`
		}
		_ = json.Unmarshal(respBody, &apiErr)
		msg := apiErr.Error.Message
		if msg == "" {
			msg = string(respBody)
		}
		_ = c.store.UpdateCallState(ctx, callID, a2a.CallStateFailed, nil, msg)
		call.State = a2a.CallStateFailed
		call.Error = &msg
		if c.tracer != nil {
			c.tracer.TraceResult(ctx, call)
		}
		return nil, apiErr.Error
	}

	var ir a2a.InvokeResponse
	if err := json.Unmarshal(respBody, &ir); err != nil {
		_ = c.store.UpdateCallState(ctx, callID, a2a.CallStateFailed, nil, err.Error())
		return nil, err
	}
	// Override server-side CallID with ours (so client audit uses its own
	// row) — but also keep the server id as the handle for events.
	if ir.Mode == a2a.ToolModeAsync && ir.Handle == "" {
		ir.Handle = ir.CallID
	}
	ir.CallID = callID

	switch ir.Mode {
	case a2a.ToolModeSync:
		_ = c.store.UpdateCallState(ctx, callID, a2a.CallStateDone, ir.Output, "")
		call.State = a2a.CallStateDone
		call.Output = ir.Output
	case a2a.ToolModeAsync:
		_ = c.store.UpdateCallState(ctx, callID, a2a.CallStateRunning, ir.Output, "")
		call.State = a2a.CallStateRunning
	}
	if c.tracer != nil {
		c.tracer.TraceResult(ctx, call)
	}
	return &ir, nil
}

// SubscribeEvents opens an SSE connection to the peer and invokes onEvent
// for every incoming Event. The loop exits when a terminal event is seen,
// ctx is cancelled, or the connection dies. On reconnect the caller should
// re-call SubscribeEvents with a fresh since cursor (or rely on the server
// replay).
func (c *Client) SubscribeEvents(ctx context.Context, remoteHandle string, since int, onEvent func(a2a.Event)) error {
	url := fmt.Sprintf("%s/a2a/events?call=%s&since=%d", c.peer.BaseURL, remoteHandle, since)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c.addAuth(req)
	req.Header.Set("Accept", "text/event-stream")

	// Events are long-lived; give the HTTP client no read timeout here.
	httpClient := &http.Client{Timeout: 0}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("subscribe %s/%s: %w", c.peer.Name, remoteHandle, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("subscribe %s: HTTP %d: %s", c.peer.Name, resp.StatusCode, body)
	}

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var dataBuf strings.Builder
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read sse: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// End of an event — dispatch buffered data.
			if dataBuf.Len() > 0 {
				var ev a2a.Event
				if err := json.Unmarshal([]byte(dataBuf.String()), &ev); err == nil {
					onEvent(ev)
					if ev.IsFinal {
						return nil
					}
				}
				dataBuf.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			// Comment / heartbeat.
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataBuf.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		// id:/event: headers currently ignored — we rely on the embedded
		// Event.Seq field instead.
	}
}

// SendCallback posts a fire-and-forget notification to the peer.
func (c *Client) SendCallback(ctx context.Context, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal callback payload: %w", err)
	}
	cb := a2a.Callback{
		Peer:    "self", // overridden by the server from auth header / config
		Event:   event,
		Payload: data,
	}
	body, _ := json.Marshal(cb)
	url := strings.TrimRight(c.peer.BaseURL, "/") + "/a2a/callback"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.addAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("callback POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("callback: status %d: %s", resp.StatusCode, string(b))
	}
	c.logger.Info("a2a: callback sent", "peer", c.peer.Name, "event", event)
	return nil
}

// addAuth stamps a bearer token on a request. Prefers the configured
// TokenProvider (Fleet-issued JWT); falls back to the static peer auth
// token (legacy shared-secret path) on empty/error.
func (c *Client) addAuth(req *http.Request) {
	bearer := ""
	if c.tokenSrc != nil {
		if tok, err := c.tokenSrc(req.Context()); err == nil && tok != "" {
			bearer = tok
		} else if err != nil {
			c.logger.Warn("a2a client: token provider failed, falling back to static",
				"peer", c.peer.Name, "error", err)
		}
	}
	if bearer == "" {
		bearer = c.peer.AuthToken
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-A2A-Peer", "self")
}
