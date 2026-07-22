package mcp

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func rpcHTTPResponse(contentType, body string) *http.Response {
	return &http.Response{
		Header: http.Header{"Content-Type": []string{contentType}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

func TestReadRPCResponseLimits(t *testing.T) {
	t.Run("valid json", func(t *testing.T) {
		response := rpcHTTPResponse("application/json", `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`)
		rpc, err := readRPCResponse(response, 7)
		if err != nil || rpc.ID == nil || *rpc.ID != 7 {
			t.Fatalf("readRPCResponse() rpc=%#v err=%v", rpc, err)
		}
	})

	t.Run("oversized json", func(t *testing.T) {
		response := rpcHTTPResponse("application/json", strings.Repeat("x", maxRPCResponseBytes+1))
		if _, err := readRPCResponse(response, 1); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized JSON error = %v", err)
		}
	})

	t.Run("valid sse", func(t *testing.T) {
		response := rpcHTTPResponse("text/event-stream; charset=utf-8", "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":9,\"result\":{}}\n\n")
		rpc, err := readRPCResponse(response, 9)
		if err != nil || rpc.ID == nil || *rpc.ID != 9 {
			t.Fatalf("readRPCResponse() rpc=%#v err=%v", rpc, err)
		}
	})

	t.Run("oversized sse response", func(t *testing.T) {
		response := rpcHTTPResponse("text/event-stream", strings.Repeat(":\n", maxRPCResponseBytes/2+1))
		if _, err := readRPCResponse(response, 1); err == nil || !strings.Contains(err.Error(), "response exceeds") {
			t.Fatalf("oversized SSE response error = %v", err)
		}
	})

	t.Run("oversized sse line", func(t *testing.T) {
		response := rpcHTTPResponse("text/event-stream", "data:"+strings.Repeat("x", maxRPCLineBytes+1)+"\n")
		if _, err := readRPCResponse(response, 1); err == nil || !strings.Contains(err.Error(), "line limit") {
			t.Fatalf("oversized SSE line error = %v", err)
		}
	})
}

func TestStdioReadLoopRejectsOversizedLine(t *testing.T) {
	resultCh := make(chan stdioCallResult, 1)
	transport := &stdioTransport{pending: map[int]chan stdioCallResult{1: resultCh}}
	transport.readLoop(strings.NewReader(strings.Repeat("x", maxRPCLineBytes+2) + "\n"))
	result := <-resultCh
	if result.err == nil || !strings.Contains(result.err.Error(), "line limit") {
		t.Fatalf("oversized stdout line error = %v", result.err)
	}
}
