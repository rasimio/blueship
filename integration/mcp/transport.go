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
)

// transport carries JSON-RPC messages to one MCP server.
type transport interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	close() error
}

const (
	maxRPCResponseBytes = 2 << 20
	maxRPCLineBytes     = 1 << 20
)

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
		hc:         newMCPHTTPClient(authHeader),
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
		limited := &io.LimitedReader{R: resp.Body, N: maxRPCResponseBytes + 1}
		sc := bufio.NewScanner(limited)
		sc.Buffer(make([]byte, 0, 64*1024), maxRPCLineBytes+1)
		for sc.Scan() {
			if maxRPCResponseBytes+1-limited.N > maxRPCResponseBytes {
				return nil, fmt.Errorf("mcp sse response exceeds %d bytes", maxRPCResponseBytes)
			}
			line := sc.Text()
			if len(line) > maxRPCLineBytes {
				return nil, fmt.Errorf("mcp sse line exceeds %d bytes", maxRPCLineBytes)
			}
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
			return nil, fmt.Errorf("mcp sse read (line limit %d bytes): %w", maxRPCLineBytes, err)
		}
		if limited.N == 0 {
			return nil, fmt.Errorf("mcp sse response exceeds %d bytes", maxRPCResponseBytes)
		}
		return nil, fmt.Errorf("mcp sse: no response for request %d", wantID)
	}
	body, err := readLimited(resp.Body, maxRPCResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("mcp json response: %w", err)
	}
	var rpc rpcResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, fmt.Errorf("mcp json decode: %w", err)
	}
	return &rpc, nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("exceeds %d bytes", limit)
	}
	return body, nil
}

// ── stdio transport ─────────────────────────────────────────────────────

type stdioTransport struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	id          atomic.Int64
	mu          sync.Mutex
	pending     map[int]chan stdioCallResult
	terminalErr error
	closed      atomic.Bool
}

type stdioCallResult struct {
	rpc rpcResponse
	err error
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
		pending: make(map[int]chan stdioCallResult),
	}
	go t.readLoop(stdout)
	return t, nil
}

// readLoop parses newline-delimited JSON-RPC messages from the subprocess
// and routes each response to the waiting caller by id.
func (t *stdioTransport) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxRPCLineBytes+1)
	lineTooLarge := false
	for sc.Scan() {
		if len(sc.Bytes()) > maxRPCLineBytes {
			lineTooLarge = true
			break
		}
		var rpc rpcResponse
		if json.Unmarshal(sc.Bytes(), &rpc) != nil || rpc.ID == nil {
			continue // not a response we're waiting on
		}
		t.mu.Lock()
		ch := t.pending[*rpc.ID]
		delete(t.pending, *rpc.ID)
		t.mu.Unlock()
		if ch != nil {
			ch <- stdioCallResult{rpc: rpc}
		}
	}
	// stdout closed — the process is gone. Fail every pending call.
	terminalErr := errors.New("mcp stdio: connection closed")
	if lineTooLarge {
		terminalErr = fmt.Errorf("mcp stdio stdout line exceeds %d bytes", maxRPCLineBytes)
	} else if err := sc.Err(); err != nil {
		terminalErr = fmt.Errorf("mcp stdio stdout read (line limit %d bytes): %w", maxRPCLineBytes, err)
	}
	t.mu.Lock()
	t.terminalErr = terminalErr
	t.closed.Store(true)
	for id, ch := range t.pending {
		ch <- stdioCallResult{err: terminalErr}
		delete(t.pending, id)
	}
	t.mu.Unlock()
}

func (t *stdioTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := int(t.id.Add(1))
	ch := make(chan stdioCallResult, 1)
	t.mu.Lock()
	if t.closed.Load() {
		err := t.terminalErr
		t.mu.Unlock()
		if err == nil {
			err = errors.New("mcp stdio: process has exited")
		}
		return nil, err
	}
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
	case result := <-ch:
		if result.err != nil {
			return nil, result.err
		}
		rpc := result.rpc
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
