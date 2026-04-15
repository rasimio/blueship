package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// TraceLevel controls how verbose the Telegram tracer should be.
type TraceLevel string

const (
	TraceLevelOff    TraceLevel = "off"
	TraceLevelErrors TraceLevel = "errors" // only failures land in the chat
	TraceLevelFull   TraceLevel = "full"   // every invocation + every event
)

// MessageSender is the minimal hook the tracer needs — matches
// bscore.MessageSender without the import dependency so a2a stays
// framework-free.
type MessageSender interface {
	SendMessage(ctx context.Context, chatID string, text string) (int, error)
}

// TelegramGroupTracer posts a human-readable copy of every outbound /
// inbound A2A call to a Telegram chat (typically a group like "rasim lab")
// so the owner can follow inter-agent conversations from their phone.
//
// Every trace message starts with the sentinel "[a2a-trace]" on the first
// line so receiving ship gateways can recognise it and skip the cortex
// turn — this is how we prevent bot-to-bot reply loops.
type TelegramGroupTracer struct {
	Sender    MessageSender
	ChatID    string
	SelfName  string // this ship's name (e.g. "arlene")
	Level     TraceLevel
	Logger    *slog.Logger
}

// TraceInvoke is called by the A2A client BEFORE the HTTP request is
// actually sent. The Call argument is already persisted in a2a_calls so
// its ID is stable.
func (t *TelegramGroupTracer) TraceInvoke(ctx context.Context, call Call) {
	if t == nil || t.Level == TraceLevelOff || t.Sender == nil || t.ChatID == "" {
		return
	}
	if t.Level == TraceLevelErrors {
		return // only post after result
	}
	text := t.format(call, "→")
	t.send(ctx, text)
}

// TraceResult is called after the HTTP response has been fully processed,
// whether success or failure. For async tools this is right after the
// InvokeResponse.Handle is returned, not on terminal event.
func (t *TelegramGroupTracer) TraceResult(ctx context.Context, call Call) {
	if t == nil || t.Level == TraceLevelOff || t.Sender == nil || t.ChatID == "" {
		return
	}
	if t.Level == TraceLevelErrors && call.State != CallStateFailed {
		return
	}
	text := t.formatResult(call)
	t.send(ctx, text)
}

// TraceEvent is called once per streamed async event.
func (t *TelegramGroupTracer) TraceEvent(ctx context.Context, call Call, ev Event) {
	if t == nil || t.Level == TraceLevelOff || t.Sender == nil || t.ChatID == "" {
		return
	}
	if t.Level == TraceLevelErrors && !ev.IsFinal {
		return
	}
	text := t.formatEvent(call, ev)
	t.send(ctx, text)
}

// format renders "[a2a-trace] [self → peer] tool_name\n<input preview>".
func (t *TelegramGroupTracer) format(call Call, arrow string) string {
	var b strings.Builder
	b.WriteString("[a2a-trace] [")
	b.WriteString(t.SelfName)
	b.WriteString(" ")
	b.WriteString(arrow)
	b.WriteString(" ")
	b.WriteString(call.PeerName)
	b.WriteString("] ")
	b.WriteString(call.ToolName)
	b.WriteString(" (")
	b.WriteString(string(call.Mode))
	b.WriteString(")\n")
	if in := previewJSON(call.Input, 200); in != "" {
		b.WriteString("in: ")
		b.WriteString(in)
	}
	return b.String()
}

// formatResult renders the final status of a call after the initial
// response has arrived. For sync calls this is the full answer; for async
// it only announces that the handle is live.
func (t *TelegramGroupTracer) formatResult(call Call) string {
	var b strings.Builder
	b.WriteString("[a2a-trace] [")
	b.WriteString(t.SelfName)
	b.WriteString(" ← ")
	b.WriteString(call.PeerName)
	b.WriteString("] ")
	b.WriteString(call.ToolName)
	b.WriteString(" — ")
	b.WriteString(string(call.State))
	if call.State == CallStateFailed && call.Error != nil {
		b.WriteString("\nerror: ")
		b.WriteString(truncate(*call.Error, 300))
		return b.String()
	}
	if call.Mode == ToolModeSync {
		if out := previewJSON(call.Output, 300); out != "" {
			b.WriteString("\nout: ")
			b.WriteString(out)
		}
	} else {
		b.WriteString("\n(streaming events…)")
	}
	return b.String()
}

// formatEvent renders one streamed event as a short line.
func (t *TelegramGroupTracer) formatEvent(call Call, ev Event) string {
	var b strings.Builder
	b.WriteString("[a2a-trace] [")
	b.WriteString(t.SelfName)
	b.WriteString(" ← ")
	b.WriteString(call.PeerName)
	b.WriteString("] event ")
	b.WriteString(call.ToolName)
	b.WriteString("#")
	short := call.ID
	if len(short) > 8 {
		short = short[:8]
	}
	b.WriteString(short)
	b.WriteString(" seq=")
	b.WriteString(fmt.Sprintf("%d", ev.Seq))
	b.WriteString(" ")
	b.WriteString(string(ev.Type))
	if ev.IsFinal {
		b.WriteString(" [final]")
	}
	if p := previewJSON(ev.Payload, 220); p != "" {
		b.WriteString("\n")
		b.WriteString(p)
	}
	return b.String()
}

// send pushes the formatted text to Telegram, swallowing errors so a trace
// failure never breaks the actual A2A call path.
func (t *TelegramGroupTracer) send(ctx context.Context, text string) {
	if _, err := t.Sender.SendMessage(ctx, t.ChatID, text); err != nil {
		if t.Logger != nil {
			t.Logger.Warn("a2a tracer: send failed", "error", err)
		}
	}
}

func previewJSON(raw json.RawMessage, max int) string {
	if len(raw) == 0 {
		return ""
	}
	// Pretty-print small payloads, raw-dump large ones up to max chars.
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return truncate(string(raw), max)
	}
	out, err := json.Marshal(generic)
	if err != nil {
		return truncate(string(raw), max)
	}
	return truncate(string(out), max)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// TraceMessagePrefix is the sentinel every Telegram trace message starts
// with. Gateways compare against it to know they must not turn a trace
// message into a cortex request — preventing bot-to-bot feedback loops.
const TraceMessagePrefix = "[a2a-trace]"

// IsTraceMessage reports whether a Telegram text looks like an A2A trace
// envelope (case-sensitive).
func IsTraceMessage(text string) bool {
	return strings.HasPrefix(strings.TrimLeft(text, " \n\t\r"), TraceMessagePrefix)
}
