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
