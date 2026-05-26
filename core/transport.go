package core

import (
	"context"
	"encoding/json"
)

// InboundMessage is a transport-agnostic incoming message.
type InboundMessage struct {
	Text     string         // user text (may include transcribed audio)
	Audio    []byte         // raw audio for STT (nil if text-only)
	Images   []ContentBlock // image content blocks
	ReplyCtx string         // quoted reply context (optional)
}

// ResponseSink delivers pipeline output back to the user via the originating transport.
type ResponseSink interface {
	SendText(ctx context.Context, text string) error
	SendVoice(ctx context.Context, audio []byte) error
	SendTyping(ctx context.Context) error
}

// StreamingVoiceSink extends ResponseSink with chunked audio delivery.
// Transports that support streaming (WebSocket) implement this for
// sentence-level TTS pipelining: audio chunks are sent as they're
// synthesized, allowing the client to start playback immediately.
type StreamingVoiceSink interface {
	ResponseSink
	// SendVoiceChunk sends one audio chunk with sequence number.
	// final=true indicates the last chunk.
	SendVoiceChunk(ctx context.Context, audio []byte, seq int, final bool) error
}

// SpokenTextSink is an optional sink capability: the gateway calls
// NoteSpokenText with each text chunk as it streams a voice response, so a
// barge-in–aware transport can track what the assistant is currently saying
// (used to classify a user interjection). Sinks that do not implement it are
// simply not notified.
type SpokenTextSink interface {
	NoteSpokenText(text string)
}

// ToolUseSink is an optional sink capability for transports that surface
// LLM tool invocations in their UI (web chat with collapsible tool-call
// blocks). SendToolUse fires when the LLM emits a tool call; SendToolResult
// fires after the agent loop executes it. Sinks that don't implement these
// (Telegram, voice) simply don't get tool events.
type ToolUseSink interface {
	SendToolUse(ctx context.Context, id, name string, input json.RawMessage) error
	SendToolResult(ctx context.Context, useID, output string, isError bool, latencyMs int) error
}

// TextStreamSink is an optional sink capability for transports that deliver
// text deltas as they arrive (web SSE). Voice uses StreamingVoiceSink for
// audio; Telegram batches into progressive message edits; HTTP-chat sends
// each chunk as an SSE frame. Sinks that don't implement it receive only
// the final aggregated text via ResponseSink.SendText.
type TextStreamSink interface {
	SendTextDelta(ctx context.Context, delta string) error
}

// MetaSink is an optional sink capability for transports that need to know
// the session ID / assistant message ID of the current turn (so an upstream
// relayer can link persisted tool_calls back to the message that owns them).
// Gateway calls SendMeta twice per turn: once with sessionID before any
// stream events (messageID="") and once after the loop returns with the
// freshly-persisted assistant messageID.
type MetaSink interface {
	SendMeta(ctx context.Context, sessionID, messageID string) error
}

// ThinkingSink is an optional sink capability for streaming extended-thinking
// deltas (Anthropic, Gemini). UI typically renders them collapsed.
type ThinkingSink interface {
	SendThinking(ctx context.Context, delta string) error
}
