package core

import "context"

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
