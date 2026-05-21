// Package mcp is a client for the Model Context Protocol — it lets a soul
// use tools exposed by external MCP servers. It speaks JSON-RPC 2.0 over
// two transports: Streamable HTTP (remote servers) and stdio (a local
// subprocess). The Manager owns per-soul connection pooling.
package mcp

import "encoding/json"

// protocolVersion is the MCP revision this client advertises in initialize.
const protocolVersion = "2025-06-18"

// rpcRequest is a JSON-RPC 2.0 request. A nil ID makes it a notification.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// ToolDef is an MCP tool as returned by tools/list.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type listToolsResult struct {
	Tools      []ToolDef `json:"tools"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

type callToolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// contentBlock is one item of a tools/call result. Only text blocks are
// surfaced to the model; other types (image, resource) are ignored.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
