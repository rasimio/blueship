//go:build legacy_commands

// Package gateway — single-user / owner-only command handlers, parked here
// during the multi-bot Vaelum cutover (2026-05-26).
//
// These commands were designed for ArleneKateBot where one person owned
// the bot and needed knobs to manage their own session, model config, and
// voice toggles. In the Vaelum multi-tenant world the cabinet UI is the
// authoritative knob for these things — none of these belong on the bot
// surface that thousands of users share.
//
// Restoration: `go build -tags legacy_commands`. The dispatch wiring in
// handleUpdate is currently commented out — see the `// LEGACY:` markers
// in gateway.go. The bodies below still reference the pre-refactor
// single-bot `g.tg.*` client; rewrite to `bi.client.*` or `us.bot.client.*`
// before re-enabling.

package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
	"github.com/rasimio/blueship/version"
)

// sendDebugDump builds a full debug dump and sends as txt file via Telegram.
// No-op for non-Telegram transports (WS-only, etc.) — Telegram is the only
// sink that supports document attachments today.
func (g *Gateway) sendDebugDump(ctx context.Context, us *UserState, injectedCtx, reflexGuidance, msgText string, preTraces, cortexTraces []agent.ToolTrace, engineRuleCount int) {
	if g.tg == nil || !g.tg.IsConfigured() {
		return
	}
	var b strings.Builder
	b.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	b.WriteString("=== DEBUG DUMP ===\n")
	b.WriteString(fmt.Sprintf("Time: %s\n", time.Now().In(g.tz).Format("2006-01-02 15:04:05")))
	b.WriteString(fmt.Sprintf("User: %s\n\n", us.ChatID))

	b.WriteString("=== SYSTEM PROMPT (full plain text) ===\n")
	if sp, err := g.systemPromptForSoul(ctx, us.SoulID); err != nil {
		b.WriteString("(error resolving system prompt: " + err.Error() + ")")
	} else if sp != "" {
		b.WriteString(sp)
	} else {
		b.WriteString("(empty)")
	}
	b.WriteString("\n\n")

	b.WriteString("=== AME TRACES (injected context) ===\n")
	if injectedCtx != "" {
		b.WriteString(injectedCtx)
	} else {
		b.WriteString("(empty)")
	}
	b.WriteString("\n\n")

	b.WriteString("=== REFLEX GUIDANCE (active rules) ===\n")
	if reflexGuidance != "" {
		b.WriteString(reflexGuidance)
	} else {
		b.WriteString("(no rules matched)")
	}
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("=== RULE ENGINE ===\n%d rules matched by structured conditions\n\n", engineRuleCount))

	b.WriteString("=== CORTEX TOOLS ===\n")
	if us.Registry != nil && g.deps.RoleTools != nil {
		names := g.deps.RoleTools.Get("cortex")
		defs := us.Registry.DefinitionsForNames(names)
		local := make([]bs.ToolDefinition, 0)
		peerDefs := make(map[string][]bs.ToolDefinition)
		for _, d := range defs {
			peer := us.Registry.PeerForTool(d.Name)
			if peer == "" {
				local = append(local, d)
			} else {
				peerDefs[peer] = append(peerDefs[peer], d)
			}
		}
		if len(local) > 0 {
			b.WriteString("[local]\n")
			for _, d := range local {
				desc := strings.TrimSpace(d.Description)
				if len(desc) > 120 {
					desc = desc[:120] + "..."
				}
				fmt.Fprintf(&b, "  %s: %s\n", d.Name, desc)
			}
		}
		for peer, pd := range peerDefs {
			fmt.Fprintf(&b, "[%s]\n", peer)
			for _, d := range pd {
				desc := strings.TrimSpace(d.Description)
				if len(desc) > 120 {
					desc = desc[:120] + "..."
				}
				fmt.Fprintf(&b, "  %s: %s\n", d.Name, desc)
			}
		}
		fmt.Fprintf(&b, "(%d tools)\n", len(defs))
	}
	b.WriteString("\n")

	b.WriteString("=== TOOL CALLS ===\n")
	allTraces := append(preTraces, cortexTraces...)
	if len(allTraces) == 0 {
		b.WriteString("(no tools called)\n")
	}
	for i, t := range allTraces {
		src := "cortex"
		if i < len(preTraces) {
			src = "reflex"
		}
		errMark := ""
		if t.Error {
			errMark = " [ERROR]"
		}
		fmt.Fprintf(&b, "[%s] %s(%s)%s\n", src, t.Name, t.Input, errMark)
		if t.Output != "" {
			fmt.Fprintf(&b, "  → %s\n", t.Output)
		}
	}

	b.WriteString("=== FINAL CORTEX INPUT LAYOUT ===\n")
	b.WriteString("system:\n")
	b.WriteString("  [current_datetime: ...] + <SYSTEM PROMPT shown above>\n\n")
	b.WriteString("messages: ...прошлая история чата...\n\n")
	b.WriteString("LAST user message (что Cortex реально видит как user input в этот turn):\n")
	b.WriteString("---BEGIN---\n")
	combinedCtx := ""
	if reflexGuidance != "" && injectedCtx != "" {
		combinedCtx = reflexGuidance + "\n\n" + injectedCtx
	} else if reflexGuidance != "" {
		combinedCtx = reflexGuidance
	} else {
		combinedCtx = injectedCtx
	}
	if combinedCtx != "" {
		b.WriteString("[context]\n")
		b.WriteString(combinedCtx)
		if !strings.HasSuffix(combinedCtx, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("[/context]\n\n")
	}
	if msgText != "" {
		b.WriteString(msgText)
	} else {
		b.WriteString("(empty)")
	}
	b.WriteString("\n---END---\n\n")

	chatID := us.ChatID
	if idx := strings.Index(chatID, ":"); idx >= 0 {
		chatID = chatID[idx+1:]
	}
	if err := g.tg.SendDocument(ctx, chatID, "debug.md", "text/markdown", []byte(b.String())); err != nil {
		g.logger.Warn("debug dump send failed", "error", err)
	}
}

func (g *Gateway) handleSessionCommand(ctx context.Context, chatID int64) {
	us, err := g.getOrInitUser(ctx, tgCanonical(chatID))
	if err != nil {
		g.logger.Debug("session command: ignored", "chat_id", chatID, "error", err)
		return
	}

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Error("session command: session error", "error", err)
		g.tg.SendLong(ctx, chatID, "Failed to load session.")
		return
	}

	buildDate := version.BuildDate
	if buildDate == "" {
		buildDate = "dev"
	}
	commit := version.Commit
	if commit == "" {
		commit = "local"
	}

	maxContext := g.deps.Config.Limits.MaxContext
	contextTokens := sess.TokenCount
	pct := 0
	if maxContext > 0 {
		pct = contextTokens * 100 / maxContext
	}

	ago := time.Since(sess.UpdatedAt).Truncate(time.Second)

	compactK := g.deps.Config.Limits.CompactThreshold / 1000
	msg := fmt.Sprintf(
		"🚀 BlueShip %s (%s)\n"+
			"🧠 Model: %s\n"+
			"📊 Session: %d msgs · ~%dk tokens\n"+
			"📚 Context: %dk/%dk (%d%%)\n"+
			"🧵 %s · updated %s ago\n"+
			"⚙️ Runtime: telegram · Compact threshold: %dk",
		buildDate, commit,
		g.cortexModelDisplay(),
		sess.MessageCount, sess.TokenCount/1000,
		contextTokens/1000, maxContext/1000, pct,
		shortID(sess.ID), ago,
		compactK,
	)

	if sess.CompactSummary != nil && *sess.CompactSummary != "" {
		summaryLen := len(*sess.CompactSummary)
		msg += fmt.Sprintf("\n📦 Compact: active (%d chars)", summaryLen)
	}

	if err := g.tg.SendLong(ctx, chatID, msg); err != nil {
		g.logger.Error("session command: send error", "error", err)
	}
}

func (g *Gateway) handleResetCommand(ctx context.Context, chatID int64) {
	us, err := g.getOrInitUser(ctx, tgCanonical(chatID))
	if err != nil {
		g.logger.Debug("reset command: ignored", "chat_id", chatID, "error", err)
		return
	}
	ctx = bs.WithSoulID(ctx, us.SoulID)

	if g.deps.ModelStore != nil {
		if err := g.deps.ModelStore.Refresh(ctx); err != nil {
			g.logger.Warn("reset: failed to refresh model config", "error", err)
		}
	}

	uid := us.UserID.String()
	model := g.cortexModelDisplay()
	sess, err := g.store.GetOrCreate(ctx, uid, g.cortexModel())
	if err != nil {
		g.logger.Error("reset: GetOrCreate failed", "chat_id", chatID, "error", err)
		_ = g.tg.SendLong(ctx, chatID, "Reset failed (session lookup): "+err.Error())
		return
	}
	if sess == nil {
		g.logger.Error("reset: GetOrCreate returned nil", "chat_id", chatID)
		_ = g.tg.SendLong(ctx, chatID, "Reset failed (no session)")
		return
	}
	if err := g.store.Archive(ctx, sess.ID); err != nil {
		g.logger.Error("reset: archive failed", "chat_id", chatID, "session_id", sess.ID, "error", err)
		_ = g.tg.SendLong(ctx, chatID, "Reset failed (archive): "+err.Error())
		return
	}
	g.logger.Info("reset: archived session",
		"chat_id", chatID,
		"session_id", sess.ID,
		"messages", sess.MessageCount,
	)

	newSess, err := g.store.CreateWithPrevious(ctx, uid, g.cortexModel(), sess.ID)
	if err != nil || newSess == nil {
		g.logger.Error("reset: create failed", "chat_id", chatID, "error", err)
		_ = g.tg.SendLong(ctx, chatID, "Reset failed (create new): "+fmt.Sprintf("%v", err))
		return
	}
	g.logger.Info("reset: created new session",
		"chat_id", chatID,
		"new_session_id", newSess.ID,
		"previous_id", sess.ID,
	)

	msg := fmt.Sprintf(
		"Session reset.\nModel: %s\nNew session: %s\nArchived: %s (%d msgs)",
		model, newSess.ID, sess.ID, sess.MessageCount,
	)
	if err := g.tg.SendLong(ctx, chatID, msg); err != nil {
		g.logger.Error("reset command: send error", "error", err)
	}
}

func (g *Gateway) handleVoiceCommand(ctx context.Context, chatID int64) {
	us, err := g.getOrInitUser(ctx, tgCanonical(chatID))
	if err != nil {
		return
	}
	if g.deps.Users == nil {
		g.tg.SendLong(ctx, chatID, "Voice: user store not available.")
		return
	}

	profile, err := g.deps.Users.GetByID(ctx, us.UserID.String())
	if err != nil {
		g.tg.SendLong(ctx, chatID, "Voice: user not found.")
		return
	}

	newState := !profile.VoiceEnabled()
	if err := g.deps.Users.SetPreference(ctx, us.UserID.String(), "voice_enabled", newState); err != nil {
		g.logger.Error("voice command: set preference error", "error", err)
		g.tg.SendLong(ctx, chatID, "Voice: failed to update preference.")
		return
	}

	msg := "Voice mode: OFF"
	if newState {
		msg = "Voice mode: ON"
	}
	g.tg.SendLong(ctx, chatID, msg)
}
