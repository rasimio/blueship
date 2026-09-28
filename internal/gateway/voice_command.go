package gateway

import (
	"context"
	"fmt"
	"strings"
)

// voiceCommand turns voice replies on or off for the person who types it.
// With it on, every answer in their private chat is followed by the same
// answer as a voice note. The setting is the person's, not the chat's, so it
// holds on every bot they talk to.
const voiceCommand = "/voice"

// maybeRunVoiceCommand handles /voice and reports whether it did. Bare, it
// flips the setting; "/voice on" and "/voice off" set it.
//
// It runs before the execution policy, like /stop: it spends no turn, and a
// person out of messages for the day may still want to choose how the next
// answer arrives.
func (g *Gateway) maybeRunVoiceCommand(ctx context.Context, bi *botInstance, rawChatID int64, us *UserState, text string) bool {
	if cmd, forUs := g.parseCommand(bi, text); cmd != voiceCommand || !forUs {
		return false
	}
	reply := g.setVoiceReplies(ctx, us, voiceCommandArg(text))
	if bi != nil && bi.client != nil {
		_, _ = bi.client.SendMessage(ctx, fmt.Sprintf("%d", rawChatID), reply)
	}
	return true
}

func voiceCommandArg(text string) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return ""
	}
	return strings.ToLower(fields[1])
}

// setVoiceReplies applies a /voice request and returns what to tell the
// person. Without a speech provider there is nothing to switch on, and saying
// "on" would promise voice notes that never come.
func (g *Gateway) setVoiceReplies(ctx context.Context, us *UserState, arg string) string {
	ui := g.deps.Config.UI
	if g.deps.Config.TTS == nil || g.deps.Users == nil {
		return ui.VoiceUnavailable
	}
	id := us.UserID.String()
	profile, err := g.deps.Users.GetByID(ctx, id)
	if err != nil {
		g.logger.Warn("voice command: profile lookup failed", "user_id", id, "error", err)
		return ui.VoiceUnavailable
	}
	on := !profile.VoiceEnabled()
	switch arg {
	case "on":
		on = true
	case "off":
		on = false
	}
	if err := g.deps.Users.SetPreference(ctx, id, "voice_enabled", on); err != nil {
		g.logger.Warn("voice command: could not save the setting", "user_id", id, "error", err)
		return ui.VoiceUnavailable
	}
	g.logger.Info("telegram /voice", "chat_id", us.ChatID, "user_id", id, "voice_enabled", on)
	if on {
		return ui.VoiceOn
	}
	return ui.VoiceOff
}
