package core

import (
	"context"
	"time"
)

// MessageStore abstracts message persistence for the agent loop.
// Implementations: session.Store (PostgreSQL/BlueShip), or any custom store.
type MessageStore interface {
	// Append persists a message to the session.
	Append(ctx context.Context, sessionID string, msg Message) error

	// AppendWithTokens persists a message with an explicit token count.
	AppendWithTokens(ctx context.Context, sessionID string, msg Message, tokens int) error

	// MessagesForAPI loads recent messages fitting within maxTokens budget.
	MessagesForAPI(ctx context.Context, sessionID string, maxTokens int) ([]Message, error)

	// DialogMessagesForAPI loads recent visible dialogue fitting within maxTokens
	// budget. Tool transcripts and other internal blocks are excluded so the
	// dialogue window is independent from tool scratch/history.
	DialogMessagesForAPI(ctx context.Context, sessionID string, maxTokens int) ([]Message, error)

	// RecentToolObservations loads compactable recent tool results for prompt
	// context. These are provenance hints, not visible dialogue history.
	RecentToolObservations(ctx context.Context, sessionID string, since time.Time, limit int) ([]ToolObservation, error)

	// AllMessagesForAPI loads all messages for a session (used for compaction).
	AllMessagesForAPI(ctx context.Context, sessionID string) ([]Message, error)

	// CompactSession replaces old messages with a summary, keeping keepCount recent messages.
	CompactSession(ctx context.Context, sessionID string, summary string, keepCount int) error

	// CreateSession creates a new chat session, returning its ID.
	CreateSession(ctx context.Context, userID, model string) (sessionID string, err error)

	// CreateSessionWithSource creates a session tagged with who created it.
	// source: "chat", "agent_task", "cli". sourceID: agent_tasks.id or empty.
	CreateSessionWithSource(ctx context.Context, userID, model, source, sourceID string) (sessionID string, err error)

	// ArchiveSession marks a session as inactive.
	ArchiveSession(ctx context.Context, sessionID string) error

	// LatestAssistantMessageID returns the ID of the most recently persisted
	// assistant message in the session, or "" if none exists. Used by the
	// gateway to wire the message ID into the meta SSE frame so vaelum can
	// link persisted tool_calls back to the assistant turn that owns them.
	LatestAssistantMessageID(ctx context.Context, sessionID string) (string, error)

	// RecordLastInputTokens persists the most recent cortex input token
	// count onto the session so the web cabinet can render the token-
	// window chip immediately on page load (not only after the first
	// live `usage` SSE frame). Called from the agent loop after every
	// LLM call. No-op on empty sessionID.
	RecordLastInputTokens(ctx context.Context, sessionID string, inputTokens int) error

	// RecordLLMUsage persists one provider call for budget monitoring.
	// Implementations should resolve user/soul/source from sessionID.
	RecordLLMUsage(ctx context.Context, usage LLMUsageRecord) error
}

// PersistedMessage is the exact receipt for one durable chat_messages append.
// Optional V2 appenders expose it without changing the legacy MessageStore
// contract; callers that need exact evidence IDs can feature-detect the narrow
// interfaces below while custom stores keep implementing Append as before.
type PersistedMessage struct {
	ID        string
	Role      string
	CreatedAt time.Time
}

// PersistedMessageAppender is the optional exact-ID append path.
type PersistedMessageAppender interface {
	AppendPersisted(ctx context.Context, sessionID string, msg Message) (PersistedMessage, error)
}

// PersistedMessageTokenAppender is the token-aware counterpart used by
// assistant writes once delivery receipts are wired end-to-end.
type PersistedMessageTokenAppender interface {
	AppendWithTokensPersisted(ctx context.Context, sessionID string, msg Message, tokens int) (PersistedMessage, error)
}

// ToolObservation is a previous tool result that can be summarized into the
// turn context without reintroducing raw tool transcripts as dialogue history.
type ToolObservation struct {
	Name      string
	Input     string
	Output    string
	IsError   bool
	CreatedAt time.Time
}

// LLMUsageRecorder is the narrow write side of MessageStore used by host
// code that makes direct LLM calls outside the agent loop.
type LLMUsageRecorder interface {
	RecordLLMUsage(ctx context.Context, usage LLMUsageRecord) error
}

// LLMUsageRecord stores provider-reported usage and local prompt-layout
// estimates so operators can separate real spend from prompt bloat.
type LLMUsageRecord struct {
	SessionID                   string
	Role                        string
	Provider                    string
	Model                       string
	InputTokens                 int
	OutputTokens                int
	CacheCreationTokens         int
	CacheReadTokens             int
	StopReason                  string
	MessageCount                int
	ToolCount                   int
	BaseSystemTokensEstimate    int
	SystemTokensEstimate        int
	ToolSchemaTokensEstimate    int
	DialogMessageTokensEstimate int
	ScratchpadTokensEstimate    int
	MessageTokensEstimate       int
	TurnContextTokensEstimate   int
	InjectedContextTokens       int
	MessageBudget               int
	MessageBudgetSource         string
	MaxContext                  int
	DeepContext                 bool
	LatencyMS                   int
}
