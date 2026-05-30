package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Client is an initialized MCP session with one server.
type Client struct {
	t transport
}

// dial opens a connection to one MCP server and runs the initialize
// handshake. secret, when non-empty, is sent as an HTTP bearer token.
func dial(ctx context.Context, srv ServerRow, secret string) (*Client, error) {
	var t transport
	switch srv.Transport {
	case "stdio":
		st, err := newStdioTransport(srv.Command, srv.Args)
		if err != nil {
			return nil, err
		}
		t = st
	default: // "http"
		authHeader, authValue := "", ""
		if secret != "" {
			authHeader, authValue = "Authorization", "Bearer "+secret
		}
		t = newHTTPTransport(srv.URL, authHeader, authValue)
	}
	c := &Client{t: t}
	if err := c.initialize(ctx); err != nil {
		_ = t.close()
		return nil, err
	}
	return c, nil
}

func (c *Client) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "blueship", "version": "1"},
	}
	if _, err := c.t.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := c.t.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	return nil
}

// listTools fetches every tool the server exposes, following pagination.
func (c *Client) listTools(ctx context.Context) ([]ToolDef, error) {
	var all []ToolDef
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.t.call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var res listToolsResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("tools/list decode: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
		if len(all) > 500 { // pagination runaway guard
			break
		}
	}
	return all, nil
}

// callTool invokes one tool and returns the flattened text result. An MCP
// isError result is surfaced as a Go error so the agent loop reports it as
// a failed tool call and the turn continues.
func (c *Client) callTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = json.RawMessage(args)
	} else {
		params["arguments"] = map[string]any{}
	}
	raw, err := c.t.call(ctx, "tools/call", params)
	if err != nil {
		return "", err
	}
	var res callToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("tools/call decode: %w", err)
	}
	text := flattenContent(res.Content)
	if res.IsError {
		if text == "" {
			text = "the MCP tool reported an error"
		}
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}

func (c *Client) close() error { return c.t.close() }

// flattenContent joins the text blocks of a tools/call result.
func flattenContent(blocks []contentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(b.Text)
	}
	return sb.String()
}
