package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// transport carries JSON-RPC messages to one MCP server.
type transport interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	close() error
}

// ── Streamable HTTP transport ───────────────────────────────────────────

type httpTransport struct {
	url        string
	authHeader string // "" = no auth
	authValue  string
	hc         *http.Client
	id         atomic.Int64
	mu         sync.Mutex
	sessionID  string
}

func newHTTPTransport(url, authHeader, authValue string) *httpTransport {
	return &httpTransport{
		url:        url,
		authHeader: authHeader,
		authValue:  authValue,
		hc:         &http.Client{Timeout: 60 * time.Second},
	}
}

func (t *httpTransport) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	if t.authHeader != "" {
		req.Header.Set(t.authHeader, t.authValue)
	}
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	return t.hc.Do(req)
}

func (t *httpTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := int(t.id.Add(1))
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	resp, err := t.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("mcp http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	rpc, err := readRPCResponse(resp, id)
	if err != nil {
		return nil, err
	}
	if rpc.Error != nil {
		return nil, rpc.Error
	}
	return rpc.Result, nil
}

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	resp, err := t.post(ctx, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (t *httpTransport) close() error {
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()
	if sid == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodDelete, t.url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Mcp-Session-Id", sid)
	if t.authHeader != "" {
		req.Header.Set(t.authHeader, t.authValue)
	}
	if resp, err := t.hc.Do(req); err == nil {
		resp.Body.Close()
	}
	return nil
}

// readRPCResponse decodes the body of an MCP HTTP response, which may be a
// single application/json object or a text/event-stream carrying the
// response as an SSE event.
func readRPCResponse(resp *http.Response, wantID int) (*rpcResponse, error) {
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(line[5:])
			if data == "" {
				continue
			}
			var rpc rpcResponse
			if json.Unmarshal([]byte(data), &rpc) != nil {
				continue
			}
			if rpc.ID != nil && *rpc.ID == wantID {
				return &rpc, nil
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("mcp sse read: %w", err)
		}
		return nil, fmt.Errorf("mcp sse: no response for request %d", wantID)
	}
	var rpc rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return nil, fmt.Errorf("mcp json decode: %w", err)
	}
	return &rpc, nil
}

// ── stdio transport ─────────────────────────────────────────────────────

type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	id      atomic.Int64
	mu      sync.Mutex
	pending map[int]chan rpcResponse
	closed  atomic.Bool
}

func newStdioTransport(command string, args []string) (*stdioTransport, error) {
	cmd := exec.Command(command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp stdio: start %q: %w", command, err)
	}
	t := &stdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		pending: make(map[int]chan rpcResponse),
	}
	go t.readLoop(stdout)
	return t, nil
}

// readLoop parses newline-delimited JSON-RPC messages from the subprocess
// and routes each response to the waiting caller by id.
func (t *stdioTransport) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var rpc rpcResponse
		if json.Unmarshal(sc.Bytes(), &rpc) != nil || rpc.ID == nil {
			continue // not a response we're waiting on
		}
		t.mu.Lock()
		ch := t.pending[*rpc.ID]
		delete(t.pending, *rpc.ID)
		t.mu.Unlock()
		if ch != nil {
			ch <- rpc
		}
	}
	// stdout closed — the process is gone. Fail every pending call.
	t.closed.Store(true)
	t.mu.Lock()
	for id, ch := range t.pending {
		close(ch)
		delete(t.pending, id)
	}
	t.mu.Unlock()
}

func (t *stdioTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if t.closed.Load() {
		return nil, errors.New("mcp stdio: process has exited")
	}
	id := int(t.id.Add(1))
	ch := make(chan rpcResponse, 1)
	t.mu.Lock()
	t.pending[id] = ch
	t.mu.Unlock()

	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		t.dropPending(id)
		return nil, err
	}
	if _, err := t.stdin.Write(append(body, '\n')); err != nil {
		t.dropPending(id)
		return nil, fmt.Errorf("mcp stdio: write: %w", err)
	}
	select {
	case <-ctx.Done():
		t.dropPending(id)
		return nil, ctx.Err()
	case rpc, ok := <-ch:
		if !ok {
			return nil, errors.New("mcp stdio: connection closed")
		}
		if rpc.Error != nil {
			return nil, rpc.Error
		}
		return rpc.Result, nil
	}
}

func (t *stdioTransport) dropPending(id int) {
	t.mu.Lock()
	delete(t.pending, id)
	t.mu.Unlock()
}

func (t *stdioTransport) notify(_ context.Context, method string, params any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	_, err = t.stdin.Write(append(body, '\n'))
	return err
}

func (t *stdioTransport) close() error {
	_ = t.stdin.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = t.cmd.Wait()
	return nil
}
