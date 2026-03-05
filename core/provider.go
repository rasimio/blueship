package core

import (
	"context"
	"time"
)

// CompletionProvider sends messages to an LLM and returns a response.
type CompletionProvider interface {
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
}

// CompletionRequest is the input for CompletionProvider.Complete.
type CompletionRequest struct {
	Model          string
	System         string
	Messages       []Message
	Tools          []ToolDefinition
	MaxTokens      int
	ThinkingBudget int // -1 = provider default, 0 = disabled, >0 = explicit thinking budget
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

// TransportSender sends messages to users via a messaging platform.
type TransportSender interface {
	SendText(ctx context.Context, chatID int64, text string) error
	SendAction(ctx context.Context, chatID int64, action string) error
}

// MessageSender sends text messages to arbitrary chat IDs (string-based).
// Used by higher-level modules that need to send messages outside the gateway flow.
type MessageSender interface {
	SendMessage(ctx context.Context, chatID string, text string) (messageID int, err error)
}
