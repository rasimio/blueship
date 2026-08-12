package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

const autonomousTurnNoOp = "[no-op]"

var errAutonomousTurnStale = errors.New("autonomous turn anchor is stale")

type conversationTurnContextKey struct{}

func contextWithConversationTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, conversationTurnContextKey{}, true)
}

func conversationTurnFromContext(ctx context.Context) bool {
	active, _ := ctx.Value(conversationTurnContextKey{}).(bool)
	return active
}

// CoordinateTaskNotification serializes every background notification that
// can mutate or externally advance a soul's conversation with interactive and
// autonomous turns. The callback may send and append ordinary task output; it
// runs only after confirmed autonomous history for this pair is complete.
func (g *Gateway) CoordinateTaskNotification(
	ctx context.Context,
	userID, soulID uuid.UUID,
	deliver func(context.Context, string) (bs.TaskNotificationReceipt, error),
) (bs.TaskNotificationReceipt, error) {
	var receipt bs.TaskNotificationReceipt
	if userID == uuid.Nil || soulID == uuid.Nil || deliver == nil {
		return receipt, bs.DefinitelyNotSent(
			fmt.Errorf("coordinate task notification: user, soul, and delivery are required"),
		)
	}

	turnMu := g.turnMutex(userID, soulID)
	turnMu.Lock()
	defer turnMu.Unlock()

	ctx = bs.WithUserID(ctx, userID)
	ctx = bs.WithSoulID(ctx, soulID)
	ctx = contextWithConversationTurn(ctx)
	us, err := g.getOrInitPlatformUser(ctx, userID, soulID, "task-notification")
	if err != nil {
		return receipt, bs.DefinitelyNotSent(
			fmt.Errorf("coordinate task notification: init user: %w", err),
		)
	}
	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		return receipt, bs.DefinitelyNotSent(
			fmt.Errorf("coordinate task notification: session: %w", err),
		)
	}
	if err := g.ensureAutonomousHistoryLocked(ctx, userID, soulID, sess.ID); err != nil {
		return receipt, bs.DefinitelyNotSent(
			fmt.Errorf("coordinate task notification: %w", err),
		)
	}
	activity, unlockActivity := g.lockActivity(userID, soulID)
	if activity.Pending > 0 {
		unlockActivity()
		return receipt, bs.DefinitelyNotSentAfter(
			fmt.Errorf("coordinate task notification: inbound user activity is pending"),
			time.Second,
		)
	}
	defer unlockActivity()
	return deliver(ctx, sess.ID)
}

// SendConversationMessage is the message_send transport for a tenant-bound
// background tool. It turns that direct send into a real assistant-only turn
// in the shared chat, under the same ordering fence as Cortex. When invoked
// from an already-coordinated interactive turn, the surrounding turn owns
// persistence and this method only performs the transport send.
func (g *Gateway) SendConversationMessage(ctx context.Context, userID uuid.UUID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("send conversation message: text is required")
	}
	if conversationTurnFromContext(ctx) {
		if g.deps != nil && g.deps.SendToUser != nil {
			return g.deps.SendToUser(ctx, userID, text)
		}
		return g.SendToUser(ctx, userID, text)
	}
	soulID, ok := bs.SoulIDFromContextOK(ctx)
	if !ok || soulID == uuid.Nil {
		return fmt.Errorf("send conversation message: soul id is required")
	}

	_, err := g.CoordinateTaskNotification(
		ctx, userID, soulID,
		func(lockedCtx context.Context, sessionID string) (bs.TaskNotificationReceipt, error) {
			receipt, sendErr := g.SendToUserOnce(lockedCtx, userID, text)
			if sendErr != nil {
				return receipt, sendErr
			}
			var tgMessageID int64
			if receipt.Transport == "telegram" && receipt.MessageID != "" {
				tgMessageID, _ = strconv.ParseInt(receipt.MessageID, 10, 64)
			}
			historyCtx, cancel := context.WithTimeout(context.WithoutCancel(lockedCtx), 10*time.Second)
			defer cancel()
			if appendErr := g.store.AppendAutonomousAssistant(
				historyCtx, uuid.New(), sessionID, text, tgMessageID, receipt.DeliveredAt,
			); appendErr != nil {
				// The provider already acknowledged this message. Returning an
				// error could make the agent call message_send again and
				// duplicate it; log the history gap and keep the send successful.
				g.logger.Error("send conversation message: history append failed",
					"user_id", userID, "soul_id", soulID,
					"session_id", sessionID, "error", appendErr)
			}
			return receipt, nil
		},
	)
	return err
}

// DraftAutonomousTurn runs the live soul Cortex against its existing chat
// session. The request prompt is provider-only: no synthetic role=user row and
// no assistant row are persisted until the durable notification is committed.
func (g *Gateway) DraftAutonomousTurn(ctx context.Context, req bs.AutonomousTurnRequest) (bs.AutonomousTurnDraft, error) {
	if req.UserID == uuid.Nil || req.SoulID == uuid.Nil {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: user and soul are required")
	}
	if strings.TrimSpace(req.AnchorMessageID) == "" {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: anchor message is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: prompt is required")
	}
	decision, err := g.authorizeExecution(ctx, req.UserID, req.SoulID, bs.ExecutionBackground, "autonomous_turn")
	if err != nil {
		return bs.AutonomousTurnDraft{}, err
	}
	if !decision.Allowed {
		return bs.AutonomousTurnDraft{}, bs.ErrExecutionDenied
	}

	turnMu := g.turnMutex(req.UserID, req.SoulID)
	turnMu.Lock()
	defer turnMu.Unlock()

	ctx = bs.ContextWithAutonomousTurn(ctx)
	ctx = bs.WithUserID(ctx, req.UserID)
	ctx = bs.WithSoulID(ctx, req.SoulID)
	us, err := g.getOrInitPlatformUser(ctx, req.UserID, req.SoulID, "autonomous")
	if err != nil {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: init user: %w", err)
	}
	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: session: %w", err)
	}
	if err := g.ensureAutonomousHistoryLocked(ctx, req.UserID, req.SoulID, sess.ID); err != nil {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: %w", err)
	}
	anchor, err := g.store.LatestUserMessageAnchor(ctx, sess.ID)
	if err != nil {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: anchor lookup: %w", err)
	}
	activity := g.activitySnapshot(req.UserID, req.SoulID)
	if anchor.ID == "" || anchor.ID != req.AnchorMessageID ||
		activity.Pending > 0 || activity.LastInboundAt.After(anchor.CreatedAt) {
		return bs.AutonomousTurnDraft{
			SessionID: sess.ID, NoOp: true, NoOpReason: bs.AutonomousNoOpUserActive,
		}, nil
	}
	dialogMessageID, err := g.store.LatestDialogMessageID(ctx, sess.ID)
	if err != nil {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: dialog version lookup: %w", err)
	}
	if dialogMessageID == "" {
		return bs.AutonomousTurnDraft{
			SessionID: sess.ID, NoOp: true, NoOpReason: bs.AutonomousNoOpNoDialog,
		}, nil
	}

	priorContext := g.buildPriorContext(ctx, sess.ID, 6)
	injectedCtx, reflexGuidance, silent := g.prepareAutonomousContext(ctx, us, priorContext)
	if silent {
		return bs.AutonomousTurnDraft{
			SessionID: sess.ID, NoOp: true, NoOpReason: bs.AutonomousNoOpRuleSilent,
		}, nil
	}

	prepared, err := g.prepareCortexTurn(ctx, us, sess, injectedCtx, reflexGuidance, nil, true)
	if err != nil {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: system prompt: %w", err)
	}
	prepared.config.PromptOnlyInput = true
	prepared.config.Ephemeral = true
	prepared.config.EmptyVisibleFallback = autonomousTurnNoOp
	prepared.config.MaxTurns, prepared.config.ToolOverride, prepared.config.AllowedTools =
		autonomousTurnToolConfig(req.Tools)

	text, _, err := prepared.loop.RunStream(ctx, prepared.config, strings.TrimSpace(req.Prompt), nil)
	if err != nil {
		return bs.AutonomousTurnDraft{}, fmt.Errorf("draft autonomous turn: cortex: %w", err)
	}
	text = strings.TrimSpace(sanitizeLeakedToolCalls(text))
	if text == "" || strings.EqualFold(text, autonomousTurnNoOp) {
		return bs.AutonomousTurnDraft{
			SessionID: sess.ID, NoOp: true, NoOpReason: bs.AutonomousNoOpModelDeclined,
		}, nil
	}
	currentActivity := g.activitySnapshot(req.UserID, req.SoulID)
	if currentActivity.Pending > 0 || currentActivity.Token != activity.Token ||
		currentActivity.LastInboundAt.After(anchor.CreatedAt) {
		return bs.AutonomousTurnDraft{
			SessionID: sess.ID, NoOp: true, NoOpReason: bs.AutonomousNoOpUserActive,
		}, nil
	}
	return bs.AutonomousTurnDraft{
		Text:            text,
		SessionID:       sess.ID,
		DialogMessageID: dialogMessageID,
		ActivityToken:   activity.Token,
	}, nil
}

// autonomousTurnToolConfig decides how many passes an autonomous turn gets and
// what it may reach for.
//
// The two move together on purpose. Granting tool names without extra passes
// produces a turn that spends its only pass calling a tool and has none left
// to say anything about the result — which reaches the user as silence and the
// operator as a broken feature. Granting passes without names just burns them.
func autonomousTurnToolConfig(tools []string) (maxTurns int, override, allowed []string) {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t = strings.TrimSpace(t); t != "" {
			names = append(names, t)
		}
	}
	if len(names) == 0 {
		return 1, []string{}, []string{}
	}
	return bs.AutonomousTurnToolPasses, names, append([]string(nil), names...)
}

func (g *Gateway) prepareAutonomousContext(ctx context.Context, us *UserState, associationSeed string) (injectedCtx, reflexGuidance string, silent bool) {
	strategy := ""
	if us.Deps != nil && us.Deps.ReflexPreparer != nil {
		// Association is seeded exclusively from real dialogue. The control
		// prompt belongs only to the provider-side turn marker and must not
		// contaminate memory retrieval, emotion state, or rule matching.
		rc := us.Deps.ReflexPreparer(ctx, us.UserID.String(), associationSeed, "")
		if rc != nil {
			injectedCtx = rc.FullContext
			if injectedCtx == "" {
				injectedCtx = rc.FormattedTraces
			}
			strategy = rc.Strategy
			us.LastStrategy = rc.Strategy
		}
	} else if us.Deps != nil && us.Deps.ContextInjector != nil {
		injectedCtx = us.Deps.ContextInjector(ctx, us.UserID.String(), associationSeed, "")
	}

	if us.Deps != nil && us.Deps.RuleEngine != nil {
		now := time.Now().In(g.deps.Config.Gateway.TimezoneFor(ctx, g.tz))
		candidates := us.Deps.RuleEngine(ctx, bs.RuleContext{
			UserID:        us.UserID.String(),
			Strategy:      strategy,
			Hour:          now.Hour(),
			AllowedScopes: []string{"always", "time", "user"},
			// An autonomous event is not user speech. Message and Intent stay
			// empty, and the engine is told which event-safe scopes may
			// participate before it performs arbitration.
		})
		rules := make([]bs.ActiveRule, 0, len(candidates))
		for _, rule := range candidates {
			switch strings.ToLower(strings.TrimSpace(rule.Scope)) {
			case "always", "time", "user":
				rule.Tools = nil
				rule.PreActions = nil
				rules = append(rules, rule)
			}
		}
		for _, rule := range rules {
			if !rule.Suppressed && rule.Silent {
				return injectedCtx, "", true
			}
		}
		reflexGuidance = formatRulesAsGuidance(rules)
	}
	return injectedCtx, reflexGuidance, false
}

// CommitAutonomousTurn is the single-attempt delivery boundary for a journaled
// draft. It rechecks the user anchor under the same conversation mutex, sends
// the exact immutable text once, then records an invisible turn boundary plus
// the clean assistant message in the shared chat session.
func (g *Gateway) CommitAutonomousTurn(ctx context.Context, commit bs.AutonomousTurnCommit) (bs.TaskNotificationReceipt, error) {
	var receipt bs.TaskNotificationReceipt
	if commit.UserID == uuid.Nil || commit.SoulID == uuid.Nil ||
		strings.TrimSpace(commit.AnchorMessageID) == "" ||
		strings.TrimSpace(commit.SessionID) == "" ||
		strings.TrimSpace(commit.DialogMessageID) == "" || strings.TrimSpace(commit.Text) == "" {
		return receipt, bs.PermanentlyNotSent(fmt.Errorf("commit autonomous turn: incomplete payload"))
	}
	if ctxSoul, ok := bs.SoulIDFromContextOK(ctx); ok && ctxSoul != commit.SoulID {
		return receipt, bs.PermanentlyNotSent(fmt.Errorf("commit autonomous turn: soul mismatch"))
	}
	attemptID, ok := bs.NotificationAttemptIDFromContext(ctx)
	if !ok {
		return receipt, bs.PermanentlyNotSent(fmt.Errorf("commit autonomous turn: notification attempt id is required"))
	}
	decision, err := g.authorizeExecution(ctx, commit.UserID, commit.SoulID, bs.ExecutionBackground, "autonomous_turn")
	if err != nil {
		return receipt, bs.DefinitelyNotSent(err)
	}
	if !decision.Allowed {
		return receipt, bs.PermanentlyNotSent(bs.ErrExecutionDenied)
	}

	turnMu := g.turnMutex(commit.UserID, commit.SoulID)
	turnMu.Lock()
	defer turnMu.Unlock()

	ctx = bs.ContextWithAutonomousTurn(ctx)
	ctx = bs.WithUserID(ctx, commit.UserID)
	ctx = bs.WithSoulID(ctx, commit.SoulID)
	us, err := g.getOrInitPlatformUser(ctx, commit.UserID, commit.SoulID, "autonomous")
	if err != nil {
		return receipt, bs.DefinitelyNotSent(fmt.Errorf("commit autonomous turn: init user: %w", err))
	}
	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		return receipt, bs.DefinitelyNotSent(fmt.Errorf("commit autonomous turn: session: %w", err))
	}
	if sess.ID != commit.SessionID {
		return receipt, bs.PermanentlyNotSent(errAutonomousTurnStale)
	}
	if err := g.ensureAutonomousHistoryLocked(ctx, commit.UserID, commit.SoulID, sess.ID); err != nil {
		return receipt, bs.DefinitelyNotSent(fmt.Errorf("commit autonomous turn: %w", err))
	}
	anchor, err := g.store.LatestUserMessageAnchor(ctx, sess.ID)
	if err != nil {
		return receipt, bs.DefinitelyNotSent(fmt.Errorf("commit autonomous turn: anchor lookup: %w", err))
	}
	activity, unlockActivity := g.lockActivity(commit.UserID, commit.SoulID)
	activityLocked := true
	defer func() {
		if activityLocked {
			unlockActivity()
		}
	}()
	if anchor.ID == "" || anchor.ID != commit.AnchorMessageID ||
		activity.Pending > 0 || activity.Token != commit.ActivityToken ||
		activity.LastInboundAt.After(anchor.CreatedAt) {
		return receipt, bs.PermanentlyNotSent(errAutonomousTurnStale)
	}
	dialogMessageID, err := g.store.LatestDialogMessageID(ctx, sess.ID)
	if err != nil {
		return receipt, bs.DefinitelyNotSent(fmt.Errorf("commit autonomous turn: dialog version lookup: %w", err))
	}
	if dialogMessageID == "" || dialogMessageID != commit.DialogMessageID {
		return receipt, bs.PermanentlyNotSent(errAutonomousTurnStale)
	}

	receipt, err = g.SendToUserOnce(ctx, commit.UserID, strings.TrimSpace(commit.Text))
	if err != nil {
		return receipt, err
	}
	// Admission was blocked through the external send. Once Telegram has
	// acknowledged the pulse, inbound messages may be admitted, but their
	// Cortex processing still waits on turnMu until history finalization below.
	unlockActivity()
	activityLocked = false
	// From this point the message is externally visible. Preserve the receipt
	// in pair-local memory before any DB write so a later turn can retry the
	// durable finalization and fail closed until history is complete.
	g.rememberAutonomousFinalization(commit.UserID, commit.SoulID, attemptID, receipt)

	historyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var tgMessageID int64
	if receipt.Transport == "telegram" && receipt.MessageID != "" {
		tgMessageID, _ = strconv.ParseInt(receipt.MessageID, 10, 64)
	}
	if err := g.store.AppendAutonomousAssistant(
		historyCtx, attemptID, sess.ID, commit.Text, tgMessageID, receipt.DeliveredAt,
	); err != nil {
		// Delivery is already confirmed. Never return a retryable transport
		// error and risk misclassifying a sent message; the atomic append
		// guarantees history is either boundary+assistant or neither.
		g.logger.Error("commit autonomous turn: history append failed",
			"user_id", commit.UserID, "soul_id", commit.SoulID,
			"session_id", sess.ID, "error", err)
	} else {
		g.logger.Debug("commit autonomous turn: eager history append complete",
			"user_id", commit.UserID, "soul_id", commit.SoulID,
			"session_id", sess.ID)
	}
	if err := g.ensureAutonomousHistoryLocked(
		historyCtx, commit.UserID, commit.SoulID, sess.ID,
	); err != nil {
		// Delivery already succeeded, so never surface a retryable transport
		// failure. The receipt remains in pair-local state; the next turn will
		// retry this exact finalization under turnMu before Cortex can run.
		g.logger.Error("commit autonomous turn: durable finalization pending",
			"user_id", commit.UserID, "soul_id", commit.SoulID,
			"session_id", sess.ID, "attempt_id", attemptID, "error", err)
	}
	return receipt, nil
}
