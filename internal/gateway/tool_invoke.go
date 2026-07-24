package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

const maxDirectToolOutputBytes = 512 << 10

var (
	ErrToolNotFound = errors.New("blueship: tool not found")
	ErrToolDisabled = errors.New("blueship: tool disabled")
)

// ToolInvocation is the transport-neutral result of executing one exact tool.
type ToolInvocation struct {
	Name      string
	Input     json.RawMessage
	Output    string
	IsError   bool
	LatencyMS int64
}

// InvokeToolForUser executes one tool through the same per-user registry and
// soul policy used by a normal chat turn, without creating chat persistence.
func (g *Gateway) InvokeToolForUser(
	ctx context.Context,
	userID, soulID uuid.UUID,
	name string,
	input json.RawMessage,
) (ToolInvocation, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ToolInvocation{}, ErrToolNotFound
	}

	decision, err := g.authorizeExecution(ctx, userID, soulID, bs.ExecutionInteractive, "tool")
	if err != nil {
		return ToolInvocation{}, err
	}
	if !decision.Allowed {
		return ToolInvocation{}, bs.ErrExecutionDenied
	}

	us, err := g.getOrInitPlatformUser(ctx, userID, soulID, "tool")
	if err != nil {
		return ToolInvocation{}, fmt.Errorf("init platform user: %w", err)
	}
	registry := us.Registry
	if source := g.deps.Config.MCPSource; source != nil {
		if mcpTools := source.ToolsForSoul(ctx, soulID); len(mcpTools) > 0 {
			registry = us.Registry.Clone()
			for _, tool := range mcpTools {
				registry.RegisterRemote(tool.Name, tool.Description, tool.Schema, bs.ToolModeSync, "mcp", tool.Handler)
			}
		}
	}
	if !registry.Has(name) {
		return ToolInvocation{}, ErrToolNotFound
	}

	allowed, err := g.resolveAllowedToolsForSoul(ctx, soulID, registry)
	if err != nil {
		return ToolInvocation{}, err
	}
	if allowed != nil && !containsTool(allowed, name) {
		return ToolInvocation{}, ErrToolDisabled
	}

	execCtx := bs.WithSoulID(bs.WithUserID(ctx, userID), soulID)
	started := time.Now()
	output, isError := registry.Execute(execCtx, name, input)
	latency := time.Since(started).Milliseconds()
	if len(output) > maxDirectToolOutputBytes {
		output = fmt.Sprintf("tool output exceeds %d bytes", maxDirectToolOutputBytes)
		isError = true
	}
	return ToolInvocation{
		Name: name, Input: input, Output: output, IsError: isError, LatencyMS: latency,
	}, nil
}

// RefreshMCPForUser invalidates a soul's MCP cache and synchronously reloads
// its enabled servers. The returned count is the number of discovered tools.
func (g *Gateway) RefreshMCPForUser(ctx context.Context, userID, soulID uuid.UUID) (int, error) {
	decision, err := g.authorizeExecution(ctx, userID, soulID, bs.ExecutionInteractive, "mcp_refresh")
	if err != nil {
		return 0, err
	}
	if !decision.Allowed {
		return 0, bs.ErrExecutionDenied
	}
	source := g.deps.Config.MCPSource
	if source == nil {
		return 0, nil
	}
	source.Invalidate(soulID)
	ctx = bs.WithSoulID(bs.WithUserID(ctx, userID), soulID)
	return len(source.ToolsForSoul(ctx, soulID)), nil
}

func containsTool(names []string, target string) bool {
	for _, name := range names {
		if name == target {
			return true
		}
	}
	return false
}
