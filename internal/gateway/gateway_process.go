package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/agent"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/tool"
)

func (g *Gateway) ProcessInbound(ctx context.Context, chatID string, messages []bs.InboundMessage, sink bs.ResponseSink) error {
	us, err := g.getOrInitUser(ctx, chatID)
	if err != nil {
		return fmt.Errorf("resolve user: %w", err)
	}
	// Soul is resolved + stashed on us inside getOrInitUser; processMessages
	// re-attaches it to ctx. Nothing to do here.

	// Transcribe audio if present.
	for i, m := range messages {
		if len(m.Audio) > 0 && g.whisper != nil && g.whisper.IsConfigured() {
			transcript, err := g.whisper.Transcribe(ctx, m.Audio, "voice.wav")
			if err != nil {
				g.logger.Warn("transcribe failed", "error", err)
				continue
			}
			if messages[i].Text != "" {
				messages[i].Text += "\n\n" + transcript
			} else {
				messages[i].Text = transcript
			}
		}
	}

	var pending []pendingMsg
	for _, m := range messages {
		if m.Text == "" && len(m.Images) == 0 {
			continue
		}
		pending = append(pending, pendingMsg{
			text:   m.Text,
			images: m.Images,
		})
	}
	if len(pending) == 0 {
		return nil
	}

	g.processMessages(ctx, us, pending, sink)
	return nil
}

// ProcessInboundForUser is the entry point for authenticated platform
// users (Vaelum web chat, native device voice). The caller has already
// authenticated the user and resolved the soul, so this bypasses chatID
// resolution and the owner-only gate that getOrInitUser enforces for the
// Telegram path.
//
// `transport` distinguishes parallel sessions for the same (user, soul):
// "vaelum" for the web chat, "voice" for the device WS. Each transport
// gets its own cached UserState (separate sink, independent in-flight
// loop) — the chat session and AME memory are shared because they key
// on (user, soul), not on transport.
//
// Audio in InboundMessage is transcribed via Whisper before dispatch;
// text-only callers (httpchat) are unaffected — empty Audio = no-op.
func (g *Gateway) ProcessInboundForUser(ctx context.Context, userID, soulID uuid.UUID, transport string, messages []bs.InboundMessage, sink bs.ResponseSink) error {
	us, err := g.getOrInitPlatformUser(ctx, userID, soulID, transport)
	if err != nil {
		return fmt.Errorf("init platform user: %w", err)
	}

	// Transcribe audio if present (same loop as the legacy ProcessInbound).
	for i, m := range messages {
		if len(m.Audio) > 0 && g.whisper != nil && g.whisper.IsConfigured() {
			transcript, err := g.whisper.Transcribe(ctx, m.Audio, "voice.wav")
			if err != nil {
				g.logger.Warn("transcribe failed", "error", err)
				continue
			}
			if messages[i].Text != "" {
				messages[i].Text += "\n\n" + transcript
			} else {
				messages[i].Text = transcript
			}
		}
	}

	var pending []pendingMsg
	for _, m := range messages {
		if m.Text == "" && len(m.Images) == 0 {
			continue
		}
		pending = append(pending, pendingMsg{
			text:             m.Text,
			images:           m.Images,
			replyToMessageID: m.ReplyToMessageID,
		})
	}
	if len(pending) == 0 {
		return nil
	}

	g.processMessages(ctx, us, pending, sink)
	return nil
}

// PersistInterruptedForUser is the soul-aware counterpart to
// PersistInterrupted — the barge-in voice path uses this when a turn is
// cancelled mid-stream so the partial assistant reply persists against
// the authenticated (user, soul), with no chatID round-trip.
func (g *Gateway) PersistInterruptedForUser(_ context.Context, userID, soulID uuid.UUID, transport, partial string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	us, err := g.getOrInitPlatformUser(ctx, userID, soulID, transport)
	if err != nil {
		g.logger.Warn("persist interrupted: init platform user failed", "error", err)
		return
	}
	ctx = bs.WithSoulID(ctx, us.SoulID)

	us.Mu.Lock()
	defer us.Mu.Unlock()

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Warn("persist interrupted: session failed", "error", err)
		return
	}

	text := strings.TrimSpace(partial)
	if text == "" {
		text = "[прервано пользователем]"
	} else {
		text += " […прервано]"
	}
	if err := g.store.Append(ctx, sess.ID, bs.Message{
		Role:    "assistant",
		Content: []bs.ContentBlock{{Type: "text", Text: text}},
	}); err != nil {
		g.logger.Warn("persist interrupted: append failed", "error", err)
	}
}

// getOrInitWebUser is retained as a thin wrapper for backward
// compatibility with the "vaelum" web chat transport.
func (g *Gateway) getOrInitWebUser(ctx context.Context, userID, soulID uuid.UUID) (*UserState, error) {
	return g.getOrInitPlatformUser(ctx, userID, soulID, "vaelum")
}

// getOrInitPlatformUser builds (or reuses) a UserState for an
// authenticated platform user. Unlike getOrInitUser it does not consult
// user_profiles and does not apply the owner gate — Vaelum is the
// authentication boundary for platform users, and the soul is supplied
// by the caller (resolved from vaelum.memberships on the platform side
// or vaelum.devices on the device path).
//
// transport is the chatID prefix ("vaelum", "voice", …) so two transports
// for the same (user, soul) maintain independent UserStates.
func (g *Gateway) getOrInitPlatformUser(ctx context.Context, userID, soulID uuid.UUID, transport string) (*UserState, error) {
	if transport == "" {
		transport = "vaelum"
	}
	chatID := transport + ":" + userID.String()

	g.mu.Lock()
	defer g.mu.Unlock()

	if us, ok := g.users[chatID]; ok {
		return us, nil
	}

	userDeps := g.deps.ForUser(userID, chatID, false)
	registry := bs.NewToolRegistry()
	tool.RegisterBuiltinTools(registry, userDeps)
	if err := tool.RegisterBrowserTools(registry, userDeps); err != nil {
		g.logger.Warn("gateway: register browser tools failed", "error", err)
	}
	if err := tool.RegisterAgentTaskTools(registry, userDeps); err != nil {
		g.logger.Warn("gateway: register agent_task tools failed", "error", err)
	}
	g.modules.RegisterAllTools(registry, userDeps)

	us := &UserState{
		ChatID:   chatID,
		UserID:   userID,
		SoulID:   soulID,
		IsOwner:  false,
		Registry: registry,
		Deps:     userDeps,
	}
	g.users[chatID] = us
	g.logger.Info("initialized web user",
		"chat_id", chatID,
		"user_id", userID.String(),
		"soul_id", soulID.String(),
	)
	return us, nil
}

func (g *Gateway) processMessages(ctx context.Context, us *UserState, msgs []pendingMsg, sink bs.ResponseSink) {
	us.Mu.Lock()
	defer us.Mu.Unlock()
	us.LoopBusy = true
	defer func() { us.LoopBusy = false }()

	// Stash the originating chat id on the context so tool handlers can
	// surface it (e.g. coderun stores it on the task row to notify the
	// requester directly when status changes, instead of broadcasting
	// through fleet peers).
	ctx = bs.ContextWithChatID(ctx, us.ChatID)

	// Re-attach the soul. The Telegram path reaches here via a debouncer
	// goroutine whose ctx was captured at debouncer-creation time —
	// before any per-turn tagging — so the soul is sourced from
	// UserState (set in getOrInitUser), not from the inbound ctx.
	ctx = bs.WithSoulID(ctx, us.SoulID)

	typingCtx, stopTyping := context.WithCancel(ctx)
	go g.keepTypingViaSink(typingCtx, sink)
	defer stopTyping()

	var blocks []bs.ContentBlock
	for _, m := range msgs {
		blocks = append(blocks, m.images...)
		if m.text != "" {
			blocks = append(blocks, bs.ContentBlock{Type: "text", Text: m.text})
		}
	}

	// Attachment-reference resolution: if the user pasted an
	// attachment UUID into their message ("прочти файл abc-…", "что
	// на картинке abc-…"), look it up via the host's AttachmentSink
	// and inline the bytes — image as a vision block, pdf/text as a
	// fenced inline. Cortex then sees the file as if the user had
	// just attached it, no tool roundtrip required. Non-matching
	// UUIDs (foreign id, plain UUIDs the user happens to mention)
	// are silently passed through.
	blocks = g.resolveInlineAttachmentRefs(ctx, us, blocks)

	// Reply-parent attachment expansion (cabinet path only — the
	// frontend hands us a uuid for replyToMessageID before session
	// creation). When the user replies to an older message that
	// carried images / files, pull those parent attachments back
	// into the current turn so cortex can see them, not just the
	// inline `[reply to:…snippet…]` prefix. Telegram replies take
	// a separate code path (replyToTGMessageID needs a session-id
	// lookup) and aren't covered here yet.
	if g.deps.AttachmentSink != nil && len(msgs) > 0 && msgs[0].replyToMessageID != "" {
		parentID, perr := uuid.Parse(msgs[0].replyToMessageID)
		if perr == nil {
			attIDs, lerr := g.deps.AttachmentSink.ListForMessage(ctx, us.UserID, us.SoulID, parentID)
			if lerr != nil {
				g.logger.Warn("reply-attachments: list failed",
					"chat_id", us.ChatID, "parent_id", parentID, "err", lerr)
			} else if len(attIDs) > 0 {
				parentBlocks := g.attachmentBlocksByIDs(ctx, us, attIDs, "reply-attached")
				if len(parentBlocks) > 0 {
					// Prepend so the parent's files arrive before the
					// user's typed text — cortex parses the context
					// "this file ← my reply about it" in order.
					blocks = append(parentBlocks, blocks...)
					g.logger.Info("reply-attachments: inlined parent files",
						"chat_id", us.ChatID, "parent_id", parentID, "count", len(parentBlocks))
				}
			}
		}
	}

	var content any
	var msgText string
	if len(blocks) == 1 && blocks[0].Type == "text" {
		msgText = blocks[0].Text
		content = msgText
	} else {
		content = blocks
		// Extract text for memory encoding
		for _, b := range blocks {
			if b.Type == "text" {
				msgText = b.Text
				break
			}
		}
	}

	// Memory encoding: Recall → Compare → React (non-blocking). Detaches
	// from the request ctx but re-carries the soul so its writes stay
	// tenant-attributed.
	if msgText != "" && g.deps.MessageEncoder != nil {
		go g.deps.MessageEncoder(bs.WithSoulID(context.Background(), us.SoulID), us.UserID.String(), msgText)
	}

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Error("session error", "error", err)
		g.sendDebugError(ctx, sink, "session", err)
		return
	}

	g.logger.Info("processing message",
		"chat_id", us.ChatID,
		"session_id", sess.ID,
		"messages", len(msgs),
		"blocks", len(blocks),
	)

	// Push raw attachment bytes to the host CDN once the session id
	// is known. The cabinet history endpoint joins on session_id, so
	// stamping here is what makes a Telegram-originated photo / PDF
	// show up as a chip in the web UI. Failures are warn-and-continue
	// — the LLM still has the bytes via chat_messages, the user just
	// loses the cabinet chip.
	var sessionUUID uuid.UUID
	if g.deps.AttachmentSink != nil {
		sessID, perr := uuid.Parse(sess.ID)
		if perr != nil {
			g.logger.Warn("attachment sink: session id parse failed",
				"session_id", sess.ID, "err", perr)
		} else {
			sessionUUID = sessID
			for _, m := range msgs {
				for _, a := range m.rawAttachments {
					if len(a.data) == 0 {
						continue
					}
					if _, aerr := g.deps.AttachmentSink.Save(ctx, bs.AttachmentParams{
						UserID:    us.UserID,
						SoulID:    us.SoulID,
						SessionID: sessID,
						Name:      a.name,
						Mime:      a.mime,
						Kind:      a.kind,
						Data:      a.data,
					}); aerr != nil {
						g.logger.Warn("attachment sink save failed",
							"chat_id", us.ChatID, "name", a.name, "kind", a.kind, "err", aerr)
					}
				}
			}
		}
	}

	// Auto-extract pasted URLs from the user's text and persist them as
	// kind='link' attachment rows. The OG worker (arlene daemon)
	// enriches the row with og:title / og:description / og:image_url
	// asynchronously; the cabinet's Links tab + per-message chip
	// rendering pick the row up either way (empty OG = favicon
	// fallback). message_id stays NULL on the user side because the
	// user's chat_messages row is appended later inside the agent loop;
	// the link still surfaces in the cabinet's session-scoped view via
	// session_id.
	g.scanAndSaveLinks(ctx, us, sessionUUID, uuid.Nil, "user", msgText)

	// Wire the session ID into a meta SSE frame so a vaelum-style relayer
	// can begin attributing persisted tool_calls before any tool fires.
	// Sinks that don't implement MetaSink (voice, Telegram) get no-op.
	if ms, ok := sink.(bs.MetaSink); ok {
		_ = ms.SendMeta(ctx, sess.ID, "")
	}

	// Collect message text for context injection (msgText already set above for single-block).
	for _, m := range msgs {
		if m.text != "" {
			if msgText != "" {
				msgText += " "
			}
			msgText += m.text
		}
	}

	// Pull a few prior chat turns so AME can embed the multi-turn theme,
	// not just the (often short, vague) current message. Without this,
	// "как ты" right after a heavy disclosure has no signal — cosine
	// search lands on whatever generic emotional record matches "как
	// ты" best, and the model anchors on the wrong event.
	priorContext := g.buildPriorContext(ctx, sess.ID, 6)

	// Build context and run reflex/cortex pipeline.
	var injectedCtx, reflexGuidance string
	var postActions []bs.PostAction // executed after cortex response
	var engineRuleCount int

	// Rule engine pass for agents that run WITHOUT a ReflexPreparer.
	// Two responsibilities:
	//   1. Abort the turn immediately if any silent rule matched.
	//   2. Format non-silent rules as guidance and surface them into the
	//      cortex prompt via reflexGuidance + debug dump.
	// No-op when a ReflexPreparer is wired: runReflexPipeline does the same
	// rule engine pass inline and owns both variables.
	if msgText != "" && us.Deps != nil && us.Deps.RuleEngine != nil && us.Deps.ReflexPreparer == nil {
		engineRules := us.Deps.RuleEngine(ctx, bs.RuleContext{
			UserID:  us.UserID.String(),
			Hour:    time.Now().Hour(),
			Message: msgText,
		})
		for _, r := range engineRules {
			if r.Silent {
				g.logger.Info("rule engine: silent rule matched (no-reflex path), aborting turn",
					"rule_id", r.ID,
					"trigger", r.Trigger,
					"chat_id", us.ChatID,
				)
				return
			}
		}
		engineRuleCount = len(engineRules)
		if engineRuleCount > 0 {
			reflexGuidance = formatRulesAsGuidance(engineRules)
			g.logger.Info("rule engine: non-silent rules matched (no-reflex path)",
				"count", len(engineRules),
				"chat_id", us.ChatID,
			)
		}
	}

	var preTraces []agent.ToolTrace

	// Resolve pending disambiguation: if previous turn asked "which option?"
	// and this message is a short answer, inject the chosen tool directly.
	if len(us.PendingDisambiguation) > 0 && msgText != "" {
		if chosen := resolveDisambiguation(msgText, us.PendingDisambiguation); chosen != nil {
			reflexGuidance = fmt.Sprintf("[DISAMBIGUATION RESOLVED]\nПользователь выбрал: %s\nВызови %s.\n", chosen.Label, chosen.Tool)
			us.PendingDisambiguation = nil
			g.logger.Info("disambiguation: resolved", "tool", chosen.Tool, "label", chosen.Label)
			// Still run context injection for AME traces.
			if us.Deps != nil && us.Deps.ContextInjector != nil {
				injectedCtx = us.Deps.ContextInjector(ctx, us.UserID.String(), msgText, priorContext)
			}
		} else {
			// Not a resolution — clear pending and proceed normally.
			us.PendingDisambiguation = nil
		}
	}

	// rp carries the structured reflex pipeline output so we can both feed
	// the agent loop (InjectedCtx / ReflexGuidance / PostActions) and
	// surface MemoriesCount + MatchedRules + Strategy to the web cabinet
	// via a SendContextInfo frame.
	var rp reflexPipelineResult
	if reflexGuidance == "" && msgText != "" && us.Deps != nil && us.Deps.ReflexPreparer != nil && g.reflexModel() != "" {
		// Reflex/Cortex pipeline: structured context → reflex plan → pre-actions → filtered cortex input.
		rp = g.runReflexPipeline(ctx, us, msgText, priorContext)
		if rp.Silent {
			// Hard rule said "do not respond". Abort the whole turn — no
			// cortex call, no message sent, no post-actions, no debug dump.
			return
		}
		injectedCtx = rp.InjectedCtx
		reflexGuidance = rp.ReflexGuidance
		postActions = rp.PostActions
		preTraces = rp.PreTraces
		engineRuleCount = rp.EngineRuleCount
	} else if msgText != "" && us.Deps != nil && us.Deps.ContextInjector != nil {
		// Fallback: legacy ContextInjector (no reflex).
		injectedCtx = us.Deps.ContextInjector(ctx, us.UserID.String(), msgText, priorContext)
	}

	// Surface the prepared context (AME memories + rule matches + AME
	// strategy) to streaming sinks so the web cabinet can render a
	// "🧠 N memories • M rules" chip on the assistant bubble before
	// any text/tool_use frames arrive. Sinks that don't implement
	// ContextInfoSink (voice, Telegram) silently skip this.
	if ci, ok := sink.(bs.ContextInfoSink); ok {
		_ = ci.SendContextInfo(ctx, bs.ContextInfo{
			Memories: rp.MemoriesCount,
			// MatchedRules already covers engine + reflex matches (the
			// dedup happens inside runReflexPipeline via seenRuleIDs).
			Rules:        len(rp.MatchedRules),
			MatchedRules: rp.MatchedRules,
			Strategy:     rp.Strategy,
		})
	}

	// Per-turn tool registry: the soul's native tools plus a fresh
	// snapshot of its connected MCP-server tools. The cached native
	// registry is cloned only when there are MCP tools to add — the
	// common no-MCP case reuses it untouched.
	turnRegistry := us.Registry
	if g.deps.Config.MCPSource != nil {
		if mcpTools := g.deps.Config.MCPSource.ToolsForSoul(ctx, us.SoulID); len(mcpTools) > 0 {
			turnRegistry = us.Registry.Clone()
			for _, t := range mcpTools {
				turnRegistry.RegisterRemote(t.Name, t.Description, t.Schema, bs.ToolModeSync, "mcp", t.Handler)
			}
		}
	}

	// The chat loop runs without a compactor: compaction deletes messages,
	// and chat history is permanent. The loop windows the context itself
	// (MessagesForAPI); older turns are recalled associatively by AME, not
	// by a linear summary.
	loop := agent.NewLoop(g.provider, g.store, turnRegistry, g.deps.RoleTools, g.deps.Config, g.logger)

	// Resolve the soul's full system prompt from the database (platform
	// preamble + persona + agents) and stamp the current datetime so the
	// model always knows "today". A soul with no persona row is a
	// misconfiguration — abort the turn loudly rather than answer with the
	// wrong identity.
	now := time.Now().In(g.tz)
	soulPrompt, err := g.systemPromptForSoul(ctx, us.SoulID)
	if err != nil {
		g.logger.Error("cortex: cannot resolve system prompt, aborting turn",
			"soul_id", us.SoulID.String(), "chat_id", us.ChatID, "error", err)
		return
	}
	systemWithTime := fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), soulPrompt)

	// Interaction tier needs a focused system prompt (preamble + persona, no
	// cortex agents layer) AND a registry subset so the fast tier cannot
	// execute cortex tools even if it hallucinates a call. Both computed
	// here so they can be passed to runInteraction.
	var (
		reflexSystem string
		reflexLoop   *agent.Loop
	)
	if g.deps.Config.Gateway.InteractionTier && g.reflexInteractionPrompt != "" {
		rp, rerr := g.reflexSystemPromptForSoul(ctx, us.SoulID)
		if rerr != nil {
			g.logger.Error("reflex: cannot resolve system prompt, aborting turn",
				"soul_id", us.SoulID.String(), "chat_id", us.ChatID, "error", rerr)
			return
		}
		reflexSystem = fmt.Sprintf("[current_datetime: %s]\n\n%s\n\n%s",
			now.Format("2006-01-02 15:04 MST (Monday)"), rp, g.reflexInteractionPrompt)
		reflexRegistry := turnRegistry.SubsetForNames([]string{tool.ToolEscalate})
		reflexLoop = agent.NewLoop(g.provider, g.store, reflexRegistry, g.deps.RoleTools, g.deps.Config, g.logger)
	}

	var cortexRef bs.ModelRef
	if g.deps.ModelStore != nil {
		cortexRef = g.deps.ModelStore.Get("cortex")
	}
	cortexTemp := cortexRef.Temperature
	// Per-role max_tokens wins when configured. Adaptive thinking at high/xhigh
	// effort counts thinking toward max_tokens, so cortex needs a larger cap
	// than the global default or replies truncate (stop_reason=max_tokens).
	cortexMaxTokens := g.deps.Config.Limits.MaxOutputTokens
	if cortexRef.MaxTokens > 0 {
		cortexMaxTokens = cortexRef.MaxTokens
	}

	// Reply metadata: the first pendingMsg in the batch carries
	// the user-visible reply target. Cabinet-originated replies set
	// replyToMessageID directly (the frontend knows the parent
	// uuid). Telegram-originated replies set replyToTGMessageID and
	// we resolve it via the session store's tg_message_id index.
	// TGMessageID of the inbound message itself is stamped on the
	// new row so future Telegram replies pointing at it can be
	// resolved the same way.
	var replyToMessageID string
	var tgMessageID int64
	if len(msgs) > 0 {
		first := msgs[0]
		if first.messageID != 0 {
			tgMessageID = int64(first.messageID)
		}
		if first.replyToMessageID != "" {
			replyToMessageID = first.replyToMessageID
		} else if first.replyToTGMessageID != 0 {
			if parentID, lerr := g.store.LookupByTGMessageID(ctx, sess.ID, int64(first.replyToTGMessageID)); lerr == nil {
				replyToMessageID = parentID
			} else {
				g.logger.Warn("reply: tg parent lookup failed",
					"session_id", sess.ID, "tg_id", first.replyToTGMessageID, "err", lerr)
			}
		}
	}

	runCfg := agent.RunConfig{
		SessionID:        sess.ID,
		SystemPrompt:     systemWithTime,
		CompactSummary:   derefString(sess.CompactSummary),
		Model:            g.cortexModel(),
		MaxTokens:        cortexMaxTokens,
		MaxTurns:         g.deps.Config.Gateway.MaxTurns,
		InjectedContext:  injectedCtx,
		ReflexGuidance:   reflexGuidance,
		Role:             "cortex",
		Temperature:      cortexTemp,
		ThinkingMode:     cortexRef.ThinkingMode,
		Effort:           cortexRef.Effort,
		AllowedTools:     g.allowedToolsForSoul(ctx, us.SoulID, turnRegistry),
		ReplyToMessageID: replyToMessageID,
		TGMessageID:      tgMessageID,
	}

	// Voice transport: use streaming LLM with inline sentence-level TTS.
	// Each sentence is TTS'd and sent as an audio chunk as soon as the LLM produces it.
	streamSink, isStreaming := sink.(bs.StreamingVoiceSink)
	if isStreaming && g.deps.Config.TTS != nil {
		var sentenceBuf strings.Builder
		chunkSeq := 0
		onTextCalls := 0

		cfg := g.deps.Config
		voice := cfg.TTSVoice
		var instruct string
		if cfg.TTSInstructMapper != nil {
			instruct = cfg.TTSInstructMapper(us.LastStrategy)
		}
		// Stream path uses the default Synthesize (OGG/Opus from ElevenLabs).
		// The legacy SynthesizeMP3 preference existed because macOS arlene-
		// voice couldn't decode OGG — that client is gone, and per-chunk MP3
		// has a ~150 ms decoder warmup on Android which manifests as the
		// "first word swallowed" on every TTS chunk. Opus warms in ~5 ms.
		synthesize := cfg.TTS.Synthesize

		// Barge-in: notify a spoken-text-aware sink of each streamed chunk so
		// it can track what the assistant is currently saying.
		noter, _ := sink.(bs.SpokenTextSink)

		// First chunk emits on any low-latency boundary (sentence end OR a
		// comma/dash after ≥ 6 chars) so the user hears Arlene within ~1 s
		// of the LLM starting to stream. Later chunks demand a full
		// sentence-end so the playback doesn't sound choppy.
		//
		// IMPORTANT: TTSTextCleaner is applied AFTER we extract a complete
		// sentence (in the synth call sites below), NOT per chunk. Many of
		// its regexes are multi-character patterns — kaomoji like "(^_^)"
		// and "(≧▽≦)", "**bold**", "_italic_" — that arrive token-by-token
		// in the stream. If we cleaned each chunk in isolation the regex
		// would never see the full pattern, so kaomoji slipped through to
		// the TTS engine and got read out as literal noise. The sentence
		// boundary is the first place we have the full text.
		var firstChunkEmitted bool
		onText := func(chunk string) {
			if noter != nil {
				noter.NoteSpokenText(chunk)
			}
			onTextCalls++
			sentenceBuf.WriteString(chunk)
			text := sentenceBuf.String()

			// Sentence-end delimiters — always cut here.
			delims := []string{". ", "! ", "? ", ".\n", "!\n", "?\n"}
			minIdx := 10
			// Until the first audio has gone out, also accept a comma /
			// dash / colon as a cut point so reflex's «Секунду,» turns
			// into audio immediately instead of waiting for the period.
			if !firstChunkEmitted {
				delims = append(delims, ", ", " — ", ": ", ",\n")
				minIdx = 6
			}
			// Pick the EARLIEST valid split point across all delims (was
			// LastIndex per delim, which collapsed multi-sentence bursts
			// from a fast LLM into a single TTS call — the first sentence
			// then waited for the rest of the buffer to land instead of
			// streaming out as it became available).
			bestIdx := -1
			bestDelimLen := 0
			for _, delim := range delims {
				idx := strings.Index(text, delim)
				if idx < minIdx {
					continue
				}
				if bestIdx < 0 || idx < bestIdx {
					bestIdx = idx
					bestDelimLen = len(delim)
				}
			}
			if bestIdx < 0 {
				return
			}
			sentence := strings.TrimSpace(text[:bestIdx+1])
			sentenceBuf.Reset()
			sentenceBuf.WriteString(text[bestIdx+bestDelimLen:])
			if cfg.TTSTextCleaner != nil {
				sentence = cfg.TTSTextCleaner(sentence)
			}
			if sentence == "" {
				return
			}

			chunkSeq++
			seq := chunkSeq
			audio, err := synthesize(ctx, sentence, voice, instruct)
			if err != nil {
				g.logger.Warn("tts: stream chunk failed", "error", err, "sentence_len", len(sentence))
				return
			}
			g.logger.Info("tts: stream chunk ok", "seq", seq, "audio_bytes", len(audio), "sentence_len", len(sentence), "first", !firstChunkEmitted)
			if werr := streamSink.SendVoiceChunk(ctx, audio, seq, false); werr != nil {
				g.logger.Warn("tts: send chunk failed", "error", werr, "seq", seq)
			}
			firstChunkEmitted = true
		}

		// reflexFlush emits whatever the reflex tier has spoken so far as an
		// audio chunk. Called by runInteraction the instant the reflex stream
		// ends so the user hears the filler ("щас гляну…") immediately, before
		// cortex starts thinking — the interaction-model point. On a simple
		// turn (no escalation) it also sends the answer slightly earlier than
		// the post-loop flush would.
		reflexFlush := func() {
			remaining := strings.TrimSpace(sentenceBuf.String())
			if remaining == "" {
				return
			}
			sentenceBuf.Reset()
			if cfg.TTSTextCleaner != nil {
				remaining = cfg.TTSTextCleaner(remaining)
			}
			if remaining == "" {
				return
			}
			chunkSeq++
			seq := chunkSeq
			audio, terr := synthesize(ctx, remaining, voice, instruct)
			if terr != nil {
				g.logger.Warn("tts: filler synth failed", "error", terr, "text_len", len(remaining))
				return
			}
			g.logger.Info("tts: filler sent", "seq", seq, "audio_bytes", len(audio), "text_len", len(remaining))
			if werr := streamSink.SendVoiceChunk(ctx, audio, seq, false); werr != nil {
				g.logger.Warn("tts: send filler chunk failed", "error", werr, "seq", seq)
			}
		}

		voiceCb := &bs.StreamCallbacks{OnText: onText}
		reply, _, _, err := g.runInteraction(ctx, loop, reflexLoop, runCfg, reflexSystem, content, voiceCb, voiceCb, reflexFlush)
		if err != nil {
			if ctx.Err() != nil {
				g.logger.Info("voice turn cancelled", "chat_id", us.ChatID)
				return
			}
			g.logger.Error("agent loop error", "chat_id", us.ChatID, "error", err)
			g.sendDebugError(ctx, sink, "agent", err)
			return
		}

		// Flush remaining text as final chunk.
		remaining := strings.TrimSpace(sentenceBuf.String())
		g.logger.Info("voice: stream done, flushing",
			"on_text_calls", onTextCalls,
			"chunks_streamed", chunkSeq,
			"remaining_len", len(remaining),
			"reply_len", len(reply),
		)
		if remaining != "" {
			if cfg.TTSTextCleaner != nil {
				remaining = cfg.TTSTextCleaner(remaining)
			}
		}
		if remaining != "" {
			chunkSeq++
			audio, terr := synthesize(ctx, remaining, voice, instruct)
			if terr != nil {
				g.logger.Warn("tts: flush synth failed", "error", terr, "text_len", len(remaining))
			} else {
				g.logger.Info("tts: flush sent", "seq", chunkSeq, "audio_bytes", len(audio))
				if werr := streamSink.SendVoiceChunk(ctx, audio, chunkSeq, true); werr != nil {
					g.logger.Warn("tts: send flush chunk failed", "error", werr)
				}
			}
		} else if chunkSeq > 0 {
			// Mark last sent chunk as final (re-send empty final marker).
			if werr := streamSink.SendVoiceChunk(ctx, nil, chunkSeq, true); werr != nil {
				g.logger.Warn("tts: send final marker failed", "error", werr)
			}
		}

		// Also send text for logging
		if reply != "" {
			sink.SendText(ctx, reply)
		}
		if reply != "" && len(postActions) > 0 {
			g.executePostActions(ctx, us, postActions, reply)
		}
		if reply != "" {
			// Voice symmetry — if Arlene happens to read a URL (uncommon
			// but not impossible on the JS-fetch-this branch), persist it
			// as a link chip so it surfaces in the cabinet.
			var assistantMsgID uuid.UUID
			if msgID, lookupErr := g.store.LatestAssistantMessageID(ctx, sess.ID); lookupErr == nil && msgID != "" {
				if parsed, perr := uuid.Parse(msgID); perr == nil {
					assistantMsgID = parsed
				}
			}
			g.scanAndSaveLinks(ctx, us, sessionUUID, assistantMsgID, "assistant", reply)
			g.emitTurnCompleted(us, sess)
		}
		return
	}

	// Streaming path for Telegram: send placeholder, edit as chunks arrive.
	tgSink, isTelegram := sink.(*telegramSink)
	if !isTelegram {
		// Non-Telegram transports: text-streaming if sink supports it
		// (http-chat SSE for the web cabinet), batch otherwise. Both
		// reflex and cortex use the same callback so reflex-only turns
		// (where reflex answers without escalating) still stream their
		// reply — otherwise the web cabinet would see an empty bubble
		// and fall back to the "(нет ответа)" stalled placeholder
		// because the reply text never reaches a SendText call.
		cortexCb := buildSinkCallbacks(ctx, sink)
		reply, cortexTraces, _, err := g.runInteraction(ctx, loop, reflexLoop, runCfg, reflexSystem, content, cortexCb, cortexCb, nil)
		if err != nil {
			if ctx.Err() != nil {
				g.logger.Info("turn cancelled", "chat_id", us.ChatID)
				return
			}
			g.logger.Error("agent loop error", "chat_id", us.ChatID, "error", err)
			g.sendDebugError(ctx, sink, "agent", err)
			return
		}
		reply = sanitizeLeakedToolCalls(reply)
		reply = g.dispatchAttachmentMarkers(ctx, us, sink, reply)
		// Emit a follow-up meta frame with the assistant message_id so the
		// SSE relayer can link any persisted tool_calls back to that turn.
		// Only when both sink + store support it; safe to skip otherwise.
		var assistantMsgID uuid.UUID
		if msgID, lookupErr := g.store.LatestAssistantMessageID(ctx, sess.ID); lookupErr == nil && msgID != "" {
			if parsed, perr := uuid.Parse(msgID); perr == nil {
				assistantMsgID = parsed
			}
			if ms, ok := sink.(bs.MetaSink); ok {
				_ = ms.SendMeta(ctx, sess.ID, msgID)
			}
		}
		// Auto-extract URLs from Arlene's final reply and persist them
		// as kind='link' rows pinned to the assistant message_id, so the
		// cabinet can highlight which bubble produced the chip. Mirror
		// of the user-side scan above.
		g.scanAndSaveLinks(ctx, us, sessionUUID, assistantMsgID, "assistant", reply)
		// For text-streaming sinks (web SSE) the reply has already been
		// delivered chunk-by-chunk via cb.OnText; calling SendText again
		// here would duplicate the whole response in the rendered bubble.
		if reply != "" {
			if len(postActions) > 0 {
				g.executePostActions(ctx, us, postActions, reply)
			}
			if _, isStream := sink.(bs.TextStreamSink); !isStream {
				sink.SendText(ctx, reply)
			}
			g.emitTurnCompleted(us, sess)
		}
		// LEGACY: sendDebugDump — per-turn debug.md attachment for the
		// owner. Parked with the rest of the legacy commands; restore in
		// concert with the /debug toggle.
		// if us.DebugMode || g.deps.Config.Gateway.Debug {
		// 	go g.sendDebugDump(ctx, us, injectedCtx, reflexGuidance, msgText, preTraces, cortexTraces, engineRuleCount)
		// }
		_ = injectedCtx
		_ = reflexGuidance
		_ = engineRuleCount
		_ = cortexTraces
		return
	}

	// Telegram streaming: progressive message editing.
	var (
		streamMsgID int // Telegram message ID for edits
		streamBuf   strings.Builder
		lastEdit    time.Time
		toolStatus  string // current tool being executed
		mu          sync.Mutex
	)

	const editInterval = 600 * time.Millisecond

	flushEdit := func() {
		mu.Lock()
		defer mu.Unlock()
		if streamMsgID == 0 {
			return
		}
		text := strings.TrimSpace(streamBuf.String())
		if toolStatus != "" {
			text += "\n\n`" + toolStatus + "`"
		}
		if text == "" {
			return
		}
		if time.Since(lastEdit) < editInterval {
			return
		}
		if tgSink.client != nil {
			tgSink.client.EditMessageText(ctx, tgSink.chatID, streamMsgID, text, nil)
		}
		lastEdit = time.Now()
	}

	// ensureMsg creates the Telegram message on first content (text or tool).
	ensureMsg := func(text string) {
		if streamMsgID != 0 || tgSink.client == nil {
			return
		}
		res, err := tgSink.client.SendMessage(ctx, fmt.Sprintf("%d", tgSink.chatID), text)
		if err == nil && res != nil && res.Result.MessageID != 0 {
			streamMsgID = res.Result.MessageID
			lastEdit = time.Now()
		}
	}

	onText := func(chunk string) {
		mu.Lock()
		streamBuf.WriteString(chunk)
		text := strings.TrimSpace(streamBuf.String())
		mu.Unlock()

		if streamMsgID == 0 && len(text) > 10 {
			// First meaningful chunk — create message
			ensureMsg(text)
		} else {
			flushEdit()
		}
	}

	onToolUse := func(name string) {
		mu.Lock()
		toolStatus = ">> " + name + "..."
		mu.Unlock()
		ensureMsg("`>> " + name + "...`")
		flushEdit()
	}

	tgCb := &bs.StreamCallbacks{
		OnText:    onText,
		OnToolUse: func(_, name string, _ json.RawMessage) { onToolUse(name) },
	}
	reply, cortexTraces, _, err := g.runInteraction(ctx, loop, reflexLoop, runCfg, reflexSystem, content, nil, tgCb, nil)
	if err != nil {
		if ctx.Err() != nil {
			g.logger.Info("turn cancelled", "chat_id", us.ChatID)
			return
		}
		g.logger.Error("agent loop error", "chat_id", us.ChatID, "error", err)
		g.sendDebugError(ctx, sink, "agent", err)
		return
	}

	reply = sanitizeLeakedToolCalls(reply)
	reply = g.dispatchAttachmentMarkers(ctx, us, sink, reply)
	if reply == "" {
		return
	}

	// Auto-extract URLs from Arlene's reply on the Telegram path too —
	// the assistant chip then surfaces in the cabinet's Links tab even
	// when the user is on mobile. Mirror of the non-Telegram branch
	// above; here we look up the assistant message id directly because
	// the meta-sink path doesn't run on Telegram.
	{
		var assistantMsgID uuid.UUID
		if msgID, lookupErr := g.store.LatestAssistantMessageID(ctx, sess.ID); lookupErr == nil && msgID != "" {
			if parsed, perr := uuid.Parse(msgID); perr == nil {
				assistantMsgID = parsed
			}
		}
		g.scanAndSaveLinks(ctx, us, sessionUUID, assistantMsgID, "assistant", reply)
	}

	// Clear tool status for final message
	mu.Lock()
	toolStatus = ""
	mu.Unlock()

	if streamMsgID != 0 && tgSink.client != nil {
		// Final edit with complete text
		tgSink.client.EditMessageText(ctx, tgSink.chatID, streamMsgID, reply, nil)
	} else {
		// No edits happened (no tools called, fast response) — just send
		sink.SendText(ctx, reply)
	}

	// Auto-detect self-reflections in cortex response and save them even
	// when reflex didn't prescribe a post_action. Long responses with
	// self-reference markers ("я поняла", "мой вывод", "я осознаю") likely
	// contain insights worth persisting.
	if len(postActions) == 0 && len(reply) > 300 && g.looksLikeSelfReflection(reply) {
		postActions = append(postActions, bs.PostAction{Type: "save_reflection"})
		g.logger.Info("auto-detected self-reflection in cortex response", "reply_len", len(reply))
	}

	if len(postActions) > 0 {
		g.executePostActions(ctx, us, postActions, reply)
	}

	g.emitTurnCompleted(us, sess)

	// LEGACY: sendDebugDump — see the dispatch-side comment in handleUpdate.
	// if us.DebugMode || g.deps.Config.Gateway.Debug {
	// 	go g.sendDebugDump(ctx, us, injectedCtx, reflexGuidance, msgText, preTraces, cortexTraces, engineRuleCount)
	// }
	_ = injectedCtx
	_ = reflexGuidance
	_ = engineRuleCount
	_ = preTraces
	_ = cortexTraces

	if g.deps.Config.TTS != nil && g.shouldSendVoice(ctx, us, sink) {
		go g.synthesizeAndSendVoice(ctx, sink, us, reply)
	}
}

// emitTurnCompleted fires the configured TurnCompletedHook (if any) for this
// session, in a non-blocking goroutine. Called from each of the three turn-
// completion exit paths in processMessages: voice streaming, non-Telegram
// batch, and Telegram streaming. The actor (or whatever consumer is wired)
// runs its memory state machine asynchronously without blocking the response
// loop. Sessions whose ID isn't a valid UUID are skipped with a warning —
// shouldn't happen in production but defensive against future schema drift.
