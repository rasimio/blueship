package core

import (
	"context"
	"encoding/json"
	"time"
)

// CompletionProvider sends messages to an LLM and returns a response.
type CompletionProvider interface {
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
}

// StreamCallbacks bundles per-event callbacks for streaming completions. Any
// field may be nil; providers MUST nil-check before invoking. OnText fires for
// each text delta. OnToolUse fires once a tool_use block's input JSON is fully
// assembled (i.e. on content_block_stop for Anthropic, end-of-stream for
// providers that don't surface partial tool blocks). OnToolResult is invoked
// by the agent loop after tool execution, not by the provider — providers
// don't see it. OnThinking fires for each thinking delta where supported.
// OnUsage fires once per LLM turn after the response is fully assembled,
// reporting the input/output token counts the provider returned. The agent
// loop dispatches it (not the provider) so a single per-turn callsite covers
// every provider uniformly; the web cabinet uses it to render a live
// context-window indicator that grows turn by turn.
type StreamCallbacks struct {
	OnText       func(delta string)
	OnToolUse    func(id, name string, input json.RawMessage)
	OnToolResult func(useID, output string, isError bool, latencyMs int)
	OnThinking   func(delta string)
	OnUsage      func(inputTokens, outputTokens int)
}

// StreamCompletionProvider extends CompletionProvider with streaming support.
// cb may be nil — in that case the provider behaves like Complete but uses the
// streaming endpoint. Returns the full response for storage/tool dispatch
// after streaming completes.
type StreamCompletionProvider interface {
	CompletionProvider
	StreamComplete(ctx context.Context, req CompletionRequest, cb *StreamCallbacks) (*CompletionResponse, error)
}

// CompletionRequest is the input for CompletionProvider.Complete.
type CompletionRequest struct {
	Model          string
	System         string
	Messages       []Message
	Tools          []ToolDefinition
	MaxTokens      int
	ThinkingBudget int     // -1 = provider default, 0 = disabled, >0 = explicit thinking budget
	Temperature    float64 // 0 = provider default, >0 = explicit temperature (0.0-2.0)
}

// CompletionResponse is the output of CompletionProvider.Complete.
type CompletionResponse struct {
	Content    []ContentBlock
	StopReason string
	Usage      Usage
}

// EmbeddingProvider generates vector embeddings for text.
type EmbeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// SearchEngine performs web searches.
type SearchEngine interface {
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
}

// SearchResult is a single web search result.
type SearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// WebFetcher fetches and extracts text from web pages.
type WebFetcher interface {
	Fetch(ctx context.Context, url string, maxChars int) (string, error)
}

// CalendarProvider manages calendar events.
type CalendarProvider interface {
	GetEvents(ctx context.Context, start, end time.Time) ([]CalendarEvent, error)
	CreateEvent(ctx context.Context, event CalendarEvent) (CalendarEvent, error)
	DeleteEvent(ctx context.Context, eventID string) error
}

// CalendarEvent represents a calendar event.
type CalendarEvent struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Location    string    `json:"location,omitempty"`
	Description string    `json:"description,omitempty"`
	MeetLink    string    `json:"meet_link,omitempty"`
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	Attendees   []string  `json:"attendees,omitempty"`
	AllDay      bool      `json:"all_day"`
}

// TranscriptionProvider transcribes audio to text.
type TranscriptionProvider interface {
	Transcribe(ctx context.Context, audio []byte, filename string) (string, error)
}

// TTSProvider synthesizes text to speech audio.
type TTSProvider interface {
	Synthesize(ctx context.Context, text, voice, instruct string) ([]byte, error)
}

// TTSProviderMP3 extends TTSProvider with MP3 output for non-Telegram clients.
type TTSProviderMP3 interface {
	TTSProvider
	SynthesizeMP3(ctx context.Context, text, voice, instruct string) ([]byte, error)
}

// TransportSender sends messages to users via a messaging platform.
type TransportSender interface {
	SendText(ctx context.Context, chatID int64, text string) error
	SendAction(ctx context.Context, chatID int64, action string) error
}

// MessageSender sends text messages to arbitrary chat IDs (string-based).
// Used by higher-level modules that need to send messages outside the gateway flow.
type MessageSender interface {
	SendMessage(ctx context.Context, chatID string, text string) (messageID int, err error)
	// SendLong sends a potentially long message, splitting into chunks as needed.
	SendLong(ctx context.Context, chatID string, text string) error
	// SendVoice sends an OGG Opus voice message.
	SendVoice(ctx context.Context, chatID string, audio []byte) error
}
