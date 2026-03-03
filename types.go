package blueship

import (
	"log/slog"

	"github.com/rasimio/blueship/core"
)

// Type aliases — canonical types live in core, top-level re-exports for convenience.
type Message = core.Message
type ContentBlock = core.ContentBlock
type ToolDefinition = core.ToolDefinition
type Usage = core.Usage
type ToolHandler = core.ToolHandler
type ToolRegistry = core.ToolRegistry

type CompletionProvider = core.CompletionProvider
type CompletionRequest = core.CompletionRequest
type CompletionResponse = core.CompletionResponse
type EmbeddingProvider = core.EmbeddingProvider
type SearchEngine = core.SearchEngine
type SearchResult = core.SearchResult
type WebFetcher = core.WebFetcher
type CalendarProvider = core.CalendarProvider
type CalendarEvent = core.CalendarEvent
type TranscriptionProvider = core.TranscriptionProvider
type TransportSender = core.TransportSender

// Config and infrastructure types
type Config = core.Config
type TransportConfig = core.TransportConfig
type ModelsConfig = core.ModelsConfig
type LimitsConfig = core.LimitsConfig
type TimeoutsConfig = core.TimeoutsConfig
type RetryConfig = core.RetryConfig
type GatewayConfig = core.GatewayConfig
type Deps = core.Deps
type Response = core.Response

// Convenience re-exports

func NewToolRegistry() *ToolRegistry { return core.NewToolRegistry() }
func OK(data interface{})            { core.OK(data) }
func Fail(err string)                { core.Fail(err) }
func InitDeps(cfg *Config, logger *slog.Logger) (*Deps, error) { return core.InitDeps(cfg, logger) }

// NormalizeContent converts content to the canonical []ContentBlock format.
func NormalizeContent(content any) []ContentBlock {
	return core.NormalizeContent(content)
}

// EstimateTokens estimates token count from content blocks.
func EstimateTokens(blocks []ContentBlock) int {
	return core.EstimateTokens(blocks)
}
