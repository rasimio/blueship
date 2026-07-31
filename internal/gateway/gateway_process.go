package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
	"github.com/rasimio/blueship/tool"
)

const telegramPreviewMaxRunes = 3500

func hasTrackableInbound(messages []bs.InboundMessage) bool {
	for _, message := range messages {
		if message.Ephemeral {
			continue
		}
		if strings.TrimSpace(message.Text) != "" || len(message.Audio) > 0 || len(message.Images) > 0 {
			return true
		}
	}
	return false
}

func carryInboundAdmission(messages []pendingMsg, version uint64) bool {
	for i := range messages {
		if messages[i].ephemeral {
			continue
		}
		messages[i].activityTracked = true
		messages[i].activityVersion = version
		return true
	}
	return false
}

func telegramPreviewText(text, toolStatus string) string {
	text = strings.TrimSpace(text)
	suffix := ""
	if toolStatus != "" {
		suffix = "`" + toolStatus + "`"
		if text == "" {
			return suffix
		}
		suffix = "\n\n" + suffix
	}
	runes := []rune(text + suffix)
	if len(runes) <= telegramPreviewMaxRunes {
		return string(runes)
	}
	if suffix != "" {
		suffixRunes := []rune(suffix)
		bodyLimit := telegramPreviewMaxRunes - len(suffixRunes) - 1
		if bodyLimit > 0 {
			return string([]rune(text)[:bodyLimit]) + "…" + suffix
		}
	}
	return string(runes[:telegramPreviewMaxRunes-1]) + "…"
}

func (g *Gateway) ProcessInbound(ctx context.Context, chatID string, messages []bs.InboundMessage, sink bs.ResponseSink) error {
	us, err := g.getOrInitUser(ctx, chatID)
	if err != nil {
		return fmt.Errorf("resolve user: %w", err)
	}
	decision, err := g.authorizeExecution(ctx, us.UserID, us.SoulID, bs.ExecutionInteractive, "legacy")
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return bs.ErrExecutionDenied
	}
	admitted := hasTrackableInbound(messages)
	handoffAdmission := false
	var admissionVersion uint64
	if admitted {
		admissionVersion = g.admitInboundActivity(us)
		defer func() {
			if !handoffAdmission {
				g.rollbackInboundActivity(us, admissionVersion)
			}
		}()
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
		visibleText := m.Text
		pending = append(pending, pendingMsg{
			text:        m.Text,
			images:      m.Images,
			visibleText: &visibleText,
		})
	}
	if len(pending) == 0 {
		return nil
	}

	handoffAdmission = carryInboundAdmission(pending, admissionVersion)
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
	decision, err := g.authorizeExecution(ctx, userID, soulID, bs.ExecutionInteractive, transport)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return bs.ErrExecutionDenied
	}
	us, err := g.getOrInitPlatformUser(ctx, userID, soulID, transport)
	if err != nil {
		return fmt.Errorf("init platform user: %w", err)
	}
	admitted := hasTrackableInbound(messages)
	handoffAdmission := false
	var admissionVersion uint64
	if admitted {
		admissionVersion = g.admitInboundActivity(us)
		defer func() {
			if !handoffAdmission {
				g.rollbackInboundActivity(us, admissionVersion)
			}
		}()
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
		visibleText := m.Text
		pending = append(pending, pendingMsg{
			text:             m.Text,
			images:           m.Images,
			visibleText:      &visibleText,
			replyToMessageID: m.ReplyToMessageID,
			ephemeral:        m.Ephemeral,
		})
	}
	if len(pending) == 0 {
		return nil
	}

	handoffAdmission = carryInboundAdmission(pending, admissionVersion)
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

	turnMu := g.turnMutex(us.UserID, us.SoulID)
	turnMu.Lock()
	defer turnMu.Unlock()

	us.Mu.Lock()
	defer us.Mu.Unlock()

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Warn("persist interrupted: session failed", "error", err)
		return
	}
	if err := g.ensureAutonomousHistoryLocked(ctx, us.UserID, us.SoulID, sess.ID); err != nil {
		g.logger.Warn("persist interrupted: autonomous history barrier failed", "error", err)
		return
	}

	text := strings.TrimSpace(partial)
	if text == "" {
		text = g.deps.Config.UI.InterruptMarker
	} else {
		text += g.deps.Config.UI.InterruptSuffix
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

func platformUserCacheKey(transport string, userID, soulID uuid.UUID) string {
	return transport + ":" + userID.String() + ":" + soulID.String()
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
	cacheKey := platformUserCacheKey(transport, userID, soulID)

	g.mu.Lock()
	defer g.mu.Unlock()

	if us, ok := g.users[cacheKey]; ok {
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
	g.users[cacheKey] = us
	g.logger.Info("initialized web user",
		"chat_id", chatID,
		"user_id", userID.String(),
		"soul_id", soulID.String(),
	)
	return us, nil
}

// appendUnlessEphemeral persists a chat message unless the turn is ephemeral (a
// private notebook ask) — then it's a no-op, leaving no chat_messages trace.
func (g *Gateway) appendUnlessEphemeral(ctx context.Context, sessionID string, msg bs.Message) error {
	if bs.EphemeralFromContext(ctx) {
		return nil
	}
	return g.store.Append(ctx, sessionID, msg)
}

// appendInteractionUser persists the canonical human envelope and reports the
// exact committed row to dependent writers. Provider-expanded reply parents
// and attachment bytes remain in content for this call only.
func (g *Gateway) appendInteractionUser(
	ctx context.Context,
	cfg agent.RunConfig,
	content any,
) error {
	if bs.EphemeralFromContext(ctx) {
		return nil
	}
	durableContent := content
	if cfg.VisibleUserText != nil {
		durableContent = *cfg.VisibleUserText
	}
	receipt, err := g.store.AppendPersisted(ctx, cfg.SessionID, bs.Message{
		Role:             "user",
		Content:          durableContent,
		VisibleText:      cfg.VisibleUserText,
		ReplyToMessageID: cfg.ReplyToMessageID,
		TGMessageID:      cfg.TGMessageID,
	})
	if err != nil {
		return err
	}
	if cfg.OnUserMessagePersisted != nil {
		cfg.OnUserMessagePersisted(ctx, receipt)
	}
	return nil
}

func (g *Gateway) saveInboundAttachments(
	ctx context.Context,
	us *UserState,
	sessionID uuid.UUID,
	messageID uuid.UUID,
	msgs []pendingMsg,
) {
	if g.deps.AttachmentSink == nil || sessionID == uuid.Nil || messageID == uuid.Nil {
		return
	}
	for _, m := range msgs {
		for _, a := range m.rawAttachments {
			if len(a.data) == 0 {
				continue
			}
			if _, err := g.deps.AttachmentSink.Save(ctx, bs.AttachmentParams{
				UserID:    us.UserID,
				SoulID:    us.SoulID,
				SessionID: sessionID,
				MessageID: messageID,
				Name:      a.name,
				Mime:      a.mime,
				Kind:      a.kind,
				Data:      a.data,
			}); err != nil {
				g.logger.Warn("attachment sink save failed",
					"chat_id", us.ChatID, "message_id", messageID,
					"name", a.name, "kind", a.kind, "err", err)
			}
		}
	}
}

func (g *Gateway) bindInboundEnvelopeArtifacts(
	cfg *agent.RunConfig,
	us *UserState,
	sessionID uuid.UUID,
	msgs []pendingMsg,
	linkText string,
) {
	if cfg == nil || g.deps == nil || g.deps.AttachmentSink == nil || sessionID == uuid.Nil {
		return
	}
	cfg.OnUserMessagePersisted = func(appendCtx context.Context, receipt bs.PersistedMessage) {
		messageID, err := uuid.Parse(receipt.ID)
		if err != nil {
			g.logger.Warn("attachment sink: persisted message id parse failed",
				"session_id", cfg.SessionID, "message_id", receipt.ID, "err", err)
			return
		}
		g.saveInboundAttachments(appendCtx, us, sessionID, messageID, msgs)
		g.scanAndSaveLinks(appendCtx, us, sessionID, messageID, "user", linkText)
	}
}

type telegramReplyLookup interface {
	LookupByTGMessageID(ctx context.Context, sessionID string, tgMessageID int64) (string, error)
}

func resolveReplyMetadata(
	ctx context.Context,
	lookup telegramReplyLookup,
	sessionID string,
	msgs []pendingMsg,
) (replyToMessageID string, tgMessageID int64, err error) {
	if len(msgs) == 0 {
		return "", 0, nil
	}
	first := msgs[0]
	if first.messageID != 0 {
		tgMessageID = int64(first.messageID)
	}
	if first.replyToMessageID != "" {
		return first.replyToMessageID, tgMessageID, nil
	}
	if first.replyToTGMessageID == 0 {
		return "", tgMessageID, nil
	}
	parentID, err := lookup.LookupByTGMessageID(ctx, sessionID, int64(first.replyToTGMessageID))
	return parentID, tgMessageID, err
}

func providerContentFromBlocks(blocks []bs.ContentBlock) any {
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return blocks[0].Text
	}
	return blocks
}

func (g *Gateway) prependReplyAttachmentBlocks(
	ctx context.Context,
	us *UserState,
	replyToMessageID string,
	blocks []bs.ContentBlock,
) []bs.ContentBlock {
	if g.deps == nil || g.deps.AttachmentSink == nil || replyToMessageID == "" {
		return blocks
	}
	parentID, err := uuid.Parse(replyToMessageID)
	if err != nil {
		return blocks
	}
	attachmentIDs, err := g.deps.AttachmentSink.ListForMessage(ctx, us.UserID, us.SoulID, parentID)
	if err != nil {
		g.logger.Warn("reply-attachments: list failed",
			"chat_id", us.ChatID, "parent_id", parentID, "err", err)
		return blocks
	}
	parentBlocks := g.attachmentBlocksByIDs(ctx, us, attachmentIDs, "reply-attached")
	if len(parentBlocks) == 0 {
		return blocks
	}
	out := make([]bs.ContentBlock, 0, len(parentBlocks)+len(blocks))
	out = append(out, parentBlocks...)
	out = append(out, blocks...)
	g.logger.Info("reply-attachments: inlined parent files",
		"chat_id", us.ChatID, "parent_id", parentID, "count", len(parentBlocks))
	return out
}

func (g *Gateway) processMessages(ctx context.Context, us *UserState, msgs []pendingMsg, sink bs.ResponseSink) {
	turnMu := g.turnMutex(us.UserID, us.SoulID)
	turnMu.Lock()
	defer turnMu.Unlock()
	ctx = contextWithConversationTurn(ctx)
	var (
		activitySessionID      string
		activityBaselineAnchor string
		activityBaselineKnown  bool
	)
	defer func() {
		durable := false
		if activityBaselineKnown && activitySessionID != "" {
			checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			latest, err := g.store.LatestUserMessageAnchor(checkCtx, activitySessionID)
			cancel()
			if err != nil {
				g.logger.Warn("inbound activity: durable anchor check failed",
					"session_id", activitySessionID, "error", err)
			} else {
				durable = latest.ID != "" && latest.ID != activityBaselineAnchor
			}
		}
		g.completeInboundActivity(us, msgs, durable)
	}()

	us.Mu.Lock()
	defer us.Mu.Unlock()
	us.LoopBusy = true
	defer func() { us.LoopBusy = false }()

	// Ephemeral turn (private notebook ask): context from memory, but persist
	// nothing. Tagged on ctx so the downstream append sites no-op via
	// appendUnlessEphemeral; the encoder + turn-hook are gated by the local.
	ephemeral := len(msgs) > 0 && msgs[0].ephemeral
	ctx = bs.WithEphemeral(ctx, ephemeral)

	// Stash the originating chat id on the context so tool handlers can
	// surface it (e.g. coderun stores it on the task row to notify the
	// requester directly when status changes, instead of broadcasting
	// through fleet peers).
	ctx = bs.ContextWithChatID(ctx, us.ChatID)

	// Re-attach the soul. The Telegram path reaches here via a debouncer
	// goroutine whose ctx was captured at debouncer-creation time —
	// before any per-turn tagging — so the soul is sourced from
	// UserState (set in getOrInitUser), not from the inbound ctx.
	ctx = bs.WithUserID(ctx, us.UserID)
	ctx = bs.WithSoulID(ctx, us.SoulID)

	timings := newTurnTimer()
	defer g.finishTurnTiming(ctx, sink, us, timings)

	typingCtx, stopTyping := context.WithCancel(ctx)
	go g.keepTypingViaSink(typingCtx, sink)
	defer stopTyping()

	prepareStarted := time.Now()
	var blocks []bs.ContentBlock
	for _, m := range msgs {
		blocks = append(blocks, m.images...)
		if m.text != "" {
			blocks = append(blocks, bs.ContentBlock{Type: "text", Text: m.text})
		}
	}
	visibleUserText := joinedVisibleText(msgs)

	// Attachment-reference resolution: if the user pasted an
	// attachment UUID into their message ("read file abc-…", "what's
	// in the picture abc-…"), look it up via the host's AttachmentSink
	// and inline the bytes — image as a vision block, pdf/text as a
	// fenced inline. Cortex then sees the file as if the user had
	// just attached it, no tool roundtrip required. Non-matching
	// UUIDs (foreign id, plain UUIDs the user happens to mention)
	// are silently passed through.
	blocks = g.resolveInlineAttachmentRefs(ctx, us, blocks)
	timings.RecordSince("gateway.prepare_content", prepareStarted, fmt.Sprintf("messages=%d blocks=%d", len(msgs), len(blocks)))

	var msgText string
	content := providerContentFromBlocks(blocks)
	if len(blocks) == 1 && blocks[0].Type == "text" {
		msgText = blocks[0].Text
	} else {
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
	if msgText != "" && g.deps.MessageEncoder != nil && !ephemeral {
		go g.deps.MessageEncoder(bs.WithSoulID(context.Background(), us.SoulID), us.UserID.String(), msgText)
	}

	sessionStarted := time.Now()
	sess, err := g.GetOrCreateSession(ctx, us)
	timings.RecordSince("gateway.session", sessionStarted, "")
	if err != nil {
		g.logger.Error("session error", "error", err)
		g.sendDebugError(ctx, sink, "session", err)
		return
	}
	if err := g.ensureAutonomousHistoryLocked(ctx, us.UserID, us.SoulID, sess.ID); err != nil {
		g.logger.Error("autonomous history barrier failed",
			"chat_id", us.ChatID, "session_id", sess.ID, "error", err)
		g.sendDebugError(ctx, sink, "autonomous history", err)
		return
	}
	if baseline, err := g.store.LatestUserMessageAnchor(ctx, sess.ID); err != nil {
		g.logger.Warn("inbound activity: baseline anchor lookup failed",
			"session_id", sess.ID, "error", err)
	} else {
		activitySessionID = sess.ID
		activityBaselineAnchor = baseline.ID
		activityBaselineKnown = true
	}

	g.logger.Info("processing message",
		"chat_id", us.ChatID,
		"session_id", sess.ID,
		"messages", len(msgs),
		"blocks", len(blocks),
	)

	// Parse the durable session id now; raw files are saved only after the
	// exact human chat_messages row commits, so reply lookup can link by
	// message_id instead of falling back to a session-wide association.
	var sessionUUID uuid.UUID
	if g.deps.AttachmentSink != nil {
		sessID, perr := uuid.Parse(sess.ID)
		if perr != nil {
			g.logger.Warn("attachment sink: session id parse failed",
				"session_id", sess.ID, "err", perr)
		} else {
			sessionUUID = sessID
		}
	}

	// Resolve the relational parent only after the session exists. Cabinet
	// replies already carry chat_messages.id; Telegram replies carry a
	// transport id that must be looked up inside this exact session.
	replyToMessageID, tgMessageID, replyErr := resolveReplyMetadata(ctx, g.store, sess.ID, msgs)
	if replyErr != nil {
		g.logger.Warn("reply: tg parent lookup failed",
			"session_id", sess.ID, "tg_id", msgs[0].replyToTGMessageID, "err", replyErr)
	}
	replyAttachmentsStarted := time.Now()
	blocks = g.prependReplyAttachmentBlocks(ctx, us, replyToMessageID, blocks)
	content = providerContentFromBlocks(blocks)
	timings.RecordSince("gateway.reply_attachments", replyAttachmentsStarted,
		fmt.Sprintf("parent=%t blocks=%d", replyToMessageID != "", len(blocks)))

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
	// "how are you" right after a heavy disclosure has no signal — cosine
	// search lands on whatever generic emotional record matches "how
	// are you" best, and the model anchors on the wrong event.
	priorStarted := time.Now()
	priorContext := g.buildPriorContext(ctx, sess.ID, 6)
	timings.RecordSince("gateway.prior_context", priorStarted, "turns=6")

	policyText := msgText
	if visibleUserText != nil {
		policyText = *visibleUserText
	}
	policyRegistry := g.cortexTurnRegistry(ctx, us, false)
	turnPolicy, allowedToolsSnapshot := g.resolveTurnPolicy(
		ctx, us, policyRegistry, policyText, ephemeral,
	)
	g.logger.Info("turn policy resolved",
		"requested_mode", turnPolicy.RequestedMode,
		"effective_mode", turnPolicy.EffectiveMode,
		"flags", turnPolicy.Flags,
		"strict", turnPolicy.Strict,
		"suppress_tool_directives", turnPolicy.SuppressToolDirectives,
		"selected_tools", turnPolicy.Tools,
		"denied_tools", turnPolicy.DeniedTools,
		"unavailable_reason", turnPolicy.UnavailableReason,
		"diagnostics", turnPolicy.Diagnostics,
	)
	if turnPolicy.UnavailableReason != "" {
		g.logger.Error("turn policy failed closed",
			"requested_mode", turnPolicy.RequestedMode,
			"effective_mode", turnPolicy.EffectiveMode,
			"reason", turnPolicy.UnavailableReason,
		)
	}
	g.startTurnPolicyObservers(ctx, us, turnPolicy)
	// The detached shadow observer intentionally sees the original context.
	// All foreground preflight paths share the hard denylist after this point.
	ctx = bs.WithDeniedTools(ctx, turnPolicy.DeniedTools)
	policyGuidance, policyTraces := g.runTurnPolicyPreActions(ctx, us, turnPolicy, timings)

	// Build Cortex context and run the turn preflight. The old reflex planner
	// is optional; AME context prep must not depend on whether that LLM tier is
	// enabled for this transport.
	var injectedCtx, reflexGuidance string
	var engineRuleCount int
	var forcedCortexTools []string
	preTraces := append([]agent.ToolTrace(nil), policyTraces...)
	var noReflexSuppressedRules []bs.MatchedRule

	// Rule engine pass for agents that run WITHOUT a ReflexPreparer.
	// Two responsibilities:
	//   1. Abort the turn immediately if any silent rule matched.
	//   2. Format non-silent rules as guidance and surface them into the
	//      cortex prompt via reflexGuidance + debug dump.
	// No-op when a ReflexPreparer is wired: runReflexPipeline does the same
	// rule engine pass inline and owns both variables.
	if msgText != "" && us.Deps != nil && us.Deps.RuleEngine != nil && us.Deps.ReflexPreparer == nil {
		ruleStarted := time.Now()
		engineRules := us.Deps.RuleEngine(ctx, bs.RuleContext{
			UserID:  us.UserID.String(),
			Hour:    time.Now().Hour(),
			Message: msgText,
		})
		timings.RecordSince("rule_engine", ruleStarted, "no_reflex")
		var activeRules []bs.ActiveRule
		for _, r := range engineRules {
			if r.Suppressed {
				noReflexSuppressedRules = append(noReflexSuppressedRules, matchedRuleFromActive(r, "engine", 0))
				continue
			}
			if turnPolicy.SuppressToolDirectives && ruleHasToolDirective(r) {
				noReflexSuppressedRules = append(noReflexSuppressedRules,
					matchedRuleFromActive(suppressToolRule(r), "engine", 0))
				continue
			}
			if r.Silent {
				g.logger.Info("rule engine: silent rule matched (no-reflex path), aborting turn",
					"rule_id", r.ID,
					"trigger", r.Trigger,
					"chat_id", us.ChatID,
				)
				return
			}
			activeRules = append(activeRules, r)
			forcedCortexTools = append(forcedCortexTools, r.Tools...)
		}
		engineRuleCount = len(activeRules)
		if engineRuleCount > 0 {
			reflexGuidance = formatRulesAsGuidance(activeRules)
			g.logger.Info("rule engine: non-silent rules matched (no-reflex path)",
				"count", len(activeRules),
				"chat_id", us.ChatID,
			)
		}
	}

	// Resolve pending disambiguation: if previous turn asked "which option?"
	// and this message is a short answer, inject the chosen tool directly.
	if len(us.PendingDisambiguation) > 0 && msgText != "" {
		if chosen := resolveDisambiguation(msgText, us.PendingDisambiguation); chosen != nil {
			reflexGuidance = fmt.Sprintf("[DISAMBIGUATION RESOLVED]\nThe user chose: %s\nCall %s.\n", chosen.Label, chosen.Tool)
			forcedCortexTools = append(forcedCortexTools, chosen.Tool)
			us.PendingDisambiguation = nil
			g.logger.Info("disambiguation: resolved", "tool", chosen.Tool, "label", chosen.Label)
			// Still run context injection for AME traces.
			if us.Deps != nil && us.Deps.ContextInjector != nil {
				contextStarted := time.Now()
				injectedCtx = us.Deps.ContextInjector(ctx, us.UserID.String(), msgText, priorContext)
				timings.RecordSince("context_injector", contextStarted, "disambiguation")
			}
		} else {
			// Not a resolution — clear pending and proceed normally.
			us.PendingDisambiguation = nil
		}
	}

	// rp carries the structured reflex pipeline output so we can both feed
	// the agent loop (InjectedCtx / ReflexGuidance) and
	// surface MemoriesCount + MatchedRules + Strategy to the web cabinet
	// via a SendContextInfo frame.
	rp := reflexPipelineResult{SuppressedRules: noReflexSuppressedRules}
	if reflexGuidance == "" && msgText != "" && us.Deps != nil && us.Deps.ReflexPreparer != nil &&
		(g.reflexModel() != "" || g.deps.Config.Gateway.InteractionTier) {
		// Context/preflight pipeline:
		//   interaction tier: AME context + RuleEngine, no reflex planner LLM.
		//   legacy tier: AME context → reflex planner → pre-actions → RuleEngine.
		pipelineStarted := time.Now()
		rp = g.runReflexPipeline(ctx, us, msgText, priorContext, timings, turnPolicy)
		timings.RecordSince("reflex_pipeline.total", pipelineStarted, "")
		if rp.Silent {
			// Hard rule said "do not respond". Abort the whole turn — no
			// cortex call, no message sent, and no debug dump.
			return
		}
		injectedCtx = rp.InjectedCtx
		reflexGuidance = rp.ReflexGuidance
		preTraces = append(preTraces, rp.PreTraces...)
		engineRuleCount = rp.EngineRuleCount
		forcedCortexTools = append(forcedCortexTools, rp.CortexTools...)
	} else if msgText != "" && us.Deps != nil && us.Deps.ContextInjector != nil {
		// Fallback: legacy ContextInjector (no reflex).
		contextStarted := time.Now()
		injectedCtx = us.Deps.ContextInjector(ctx, us.UserID.String(), msgText, priorContext)
		timings.RecordSince("context_injector", contextStarted, "fallback")
	}
	reflexGuidance = prependTurnGuidance(policyGuidance, reflexGuidance)

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
			Rules:           len(rp.MatchedRules),
			MatchedRules:    rp.MatchedRules,
			SuppressedRules: rp.SuppressedRules,
			Strategy:        rp.Strategy,
		})
	}

	prepared, err := g.prepareCortexTurnWithRegistry(
		ctx, us, sess, injectedCtx, reflexGuidance, timings, false,
		policyRegistry, &allowedToolsSnapshot,
	)
	if err != nil {
		g.logger.Error("cortex: cannot resolve system prompt, aborting turn",
			"soul_id", us.SoulID.String(), "chat_id", us.ChatID, "error", err)
		return
	}
	turnRegistry := prepared.registry
	loop := prepared.loop
	now := prepared.now
	runCfg := prepared.config
	if g.applyVisionModel(&runCfg, content) {
		g.logger.Info("cortex: turn carries images, routed to the vision model",
			"session_id", runCfg.SessionID,
			"model", runCfg.Model,
			"effort", runCfg.Effort,
			"thinking_mode", runCfg.ThinkingMode,
		)
	}

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

	runCfg.ReplyToMessageID = replyToMessageID
	runCfg.TGMessageID = tgMessageID
	runCfg.VisibleUserText = visibleUserText
	if !ephemeral && g.deps.AttachmentSink != nil && sessionUUID != uuid.Nil {
		g.bindInboundEnvelopeArtifacts(&runCfg, us, sessionUUID, msgs, policyText)
	}
	g.applyTurnToolPolicy(ctx, turnRegistry, &runCfg, msgText, forcedCortexTools, ephemeral, turnPolicy)
	g.logger.Info("turn policy applied",
		"effective_mode", turnPolicy.EffectiveMode,
		"flags", turnPolicy.Flags,
		"strict", runCfg.StrictTools,
		"tool_override", runCfg.ToolOverride,
		"denied_tools", runCfg.DeniedTools,
		"suppressed_rules", len(rp.SuppressedRules),
	)

	// Ephemeral notebook ask: a fast, private answer on the SELECTED text.
	//  - a dedicated source='notebook' session → the ask is isolated from the
	//    chat thread (cabinet/Telegram read source='chat'), so it never pollutes
	//    the conversation, yet the cortex still answers the ask (it's the user
	//    message) with the soul's memory (InjectedContext).
	//  - loop Ephemeral → the assistant reply is not persisted.
	//  - MessageBudget small → the cortex sees only a little recent history
	//    instead of the whole 30K chat (which made it muse for ~30s).
	//  - SkipUserAppend=false (set in gateway_reflex) → the loop includes the
	//    ask itself as the user message, so it answers the selected text.
	if ephemeral {
		if nb, nerr := g.store.GetOrCreateNotebook(ctx, us.UserID.String(), g.cortexModelDisplay()); nerr == nil {
			// Reset it so each ask is standalone — no prior ask bleeds in.
			_ = g.store.ClearSessionMessages(ctx, nb.ID)
			runCfg.SessionID = nb.ID
			runCfg.CompactSummary = ""
		} else {
			g.logger.Warn("notebook ask: notebook session unavailable, using chat session", "error", nerr)
		}
		runCfg.Ephemeral = true
		runCfg.MessageBudget = 3000
		runCfg.MessageBudgetSource = "notebook.ephemeral"
		// Full tools — the notebook ask is a real cortex turn (⌘J): it can
		// web_search / browser_fetch for live facts, read memory, etc. runCfg
		// keeps the soul's full AllowedTools (set above). Ephemeral tool loops
		// work because RunStream carries the assistant + tool_result turns in
		// memory for ephemeral runs (still nothing persisted to the DB).
	}
	g.logCortexTurnAnatomy(runCfg, forcedCortexTools)

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
		// The legacy SynthesizeMP3 preference existed because an old macOS voice
		// client couldn't decode OGG — that client is gone, and per-chunk MP3
		// has a ~150 ms decoder warmup on Android which manifests as the
		// "first word swallowed" on every TTS chunk. Opus warms in ~5 ms.
		synthesize := cfg.TTS.Synthesize

		// Barge-in: notify a spoken-text-aware sink of each streamed chunk so
		// it can track what the assistant is currently saying.
		noter, _ := sink.(bs.SpokenTextSink)

		// First chunk emits on any low-latency boundary (sentence end OR a
		// comma/dash after ≥ 6 chars) so the user hears the reply within ~1 s
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
			// dash / colon as a cut point so reflex's «One sec,» turns
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
		// ends so the user hears the filler ("let me check…") immediately, before
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
		interactionStarted := time.Now()
		reply, _, _, err := g.runInteraction(ctx, loop, reflexLoop, runCfg, reflexSystem, content, voiceCb, voiceCb, reflexFlush)
		timings.RecordSince("interaction.total", interactionStarted, "transport=voice")
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
		if reply != "" {
			// Voice symmetry — if the assistant happens to read a URL (uncommon
			// but not impossible on the JS-fetch-this branch), persist it
			// as a link chip so it surfaces in the cabinet.
			var assistantMsgID uuid.UUID
			if msgID, lookupErr := g.store.LatestAssistantMessageID(ctx, sess.ID); lookupErr == nil && msgID != "" {
				if parsed, perr := uuid.Parse(msgID); perr == nil {
					assistantMsgID = parsed
				}
			}
			if !ephemeral {
				g.scanAndSaveLinks(ctx, us, sessionUUID, assistantMsgID, "assistant", reply)
				g.emitTurnCompleted(us, sess)
				g.maybeScheduleSoftSummary(us, sess.ID)
			}
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
		// and fall back to the "(no response)" stalled placeholder
		// because the reply text never reaches a SendText call.
		cortexCb := buildSinkCallbacks(ctx, sink)
		interactionStarted := time.Now()
		reply, cortexTraces, _, err := g.runInteraction(ctx, loop, reflexLoop, runCfg, reflexSystem, content, cortexCb, cortexCb, nil)
		timings.RecordSince("interaction.total", interactionStarted, "transport=text")
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
		// Auto-extract URLs from the assistant's final reply and persist them
		// as kind='link' rows pinned to the assistant message_id, so the
		// cabinet can highlight which bubble produced the chip. Mirror
		// of the user-side scan above.
		g.scanAndSaveLinks(ctx, us, sessionUUID, assistantMsgID, "assistant", reply)
		// For text-streaming sinks (web SSE) the reply has already been
		// delivered chunk-by-chunk via cb.OnText; calling SendText again
		// here would duplicate the whole response in the rendered bubble.
		if reply != "" {
			if _, isStream := sink.(bs.TextStreamSink); !isStream {
				sink.SendText(ctx, reply)
			}
			if !ephemeral {
				g.emitTurnCompleted(us, sess)
				g.maybeScheduleSoftSummary(us, sess.ID)
			}
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
		previewErr  bool   // log the first transport failure, not every delta
		mu          sync.Mutex
	)

	const editInterval = 600 * time.Millisecond

	flushEdit := func() {
		mu.Lock()
		defer mu.Unlock()
		if streamMsgID == 0 {
			return
		}
		text := telegramPreviewText(streamBuf.String(), toolStatus)
		if text == "" {
			return
		}
		if time.Since(lastEdit) < editInterval {
			return
		}
		if tgSink.client != nil {
			if err := tgSink.client.EditMessageText(ctx, tgSink.chatID, streamMsgID, text, nil); err != nil && !previewErr {
				previewErr = true
				g.logger.Warn("telegram preview edit failed",
					"chat_id", us.ChatID, "message_id", streamMsgID, "error", err)
			}
		}
		lastEdit = time.Now()
	}

	// ensureMsg creates the Telegram message on first content (text or tool).
	ensureMsg := func(text string) {
		if streamMsgID != 0 || tgSink.client == nil {
			return
		}
		text = telegramPreviewText(text, "")
		res, err := tgSink.client.SendMessage(ctx, fmt.Sprintf("%d", tgSink.chatID), text)
		if err == nil && res != nil && res.Result.MessageID != 0 {
			streamMsgID = res.Result.MessageID
			lastEdit = time.Now()
		} else if err != nil && !previewErr {
			previewErr = true
			g.logger.Warn("telegram preview create failed", "chat_id", us.ChatID, "error", err)
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
	interactionStarted := time.Now()
	reply, cortexTraces, _, err := g.runInteraction(ctx, loop, reflexLoop, runCfg, reflexSystem, content, nil, tgCb, nil)
	timings.RecordSince("interaction.total", interactionStarted, "transport=telegram")
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

	// Auto-extract URLs from the assistant's reply on the Telegram path too —
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

	deliveryCtx, cancelDelivery := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	var deliveryErr error
	if tgSink.client != nil {
		deliveryErr = tgSink.client.FinalizeResponse(deliveryCtx, tgSink.chatID, streamMsgID, reply)
	} else {
		deliveryErr = sink.SendText(deliveryCtx, reply)
	}
	cancelDelivery()
	if deliveryErr != nil {
		g.logger.Error("telegram final delivery failed",
			"chat_id", us.ChatID, "preview_message_id", streamMsgID,
			"reply_runes", len([]rune(reply)), "error", deliveryErr)
	}

	if !ephemeral {
		g.emitTurnCompleted(us, sess)
		g.maybeScheduleSoftSummary(us, sess.ID)
	}

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
