package gateway

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/session"
)

func TestPlatformUserCacheKeyIncludesSoul(t *testing.T) {
	userID := uuid.New()
	firstSoul := uuid.New()
	secondSoul := uuid.New()

	first := platformUserCacheKey("vaelum", userID, firstSoul)
	second := platformUserCacheKey("vaelum", userID, secondSoul)
	if first == second {
		t.Fatalf("different souls share platform cache key %q", first)
	}
}

func TestTurnMutexIsSharedPerUserSoul(t *testing.T) {
	g := &Gateway{}
	userID := uuid.New()
	soulID := uuid.New()

	if g.turnMutex(userID, soulID) != g.turnMutex(userID, soulID) {
		t.Fatal("same user/soul did not share turn mutex")
	}
	if g.turnMutex(userID, soulID) == g.turnMutex(userID, uuid.New()) {
		t.Fatal("different souls shared a turn mutex")
	}
}

func TestConversationTurnContextMarkerSurvivesIdentityTagging(t *testing.T) {
	ctx := contextWithConversationTurn(context.Background())
	ctx = bs.WithUserID(ctx, uuid.New())
	ctx = bs.WithSoulID(ctx, uuid.New())
	if !conversationTurnFromContext(ctx) {
		t.Fatal("conversation turn marker was lost")
	}
}

func TestSendConversationMessageDoesNotRelockCoordinatedTurn(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	sent := make(chan string, 1)
	g := &Gateway{
		deps: &bs.Deps{
			SendToUser: func(_ context.Context, gotUserID uuid.UUID, text string) error {
				if gotUserID != userID {
					t.Errorf("send user = %s, want %s", gotUserID, userID)
				}
				sent <- text
				return nil
			},
		},
	}
	turnMu := g.turnMutex(userID, soulID)
	turnMu.Lock()
	defer turnMu.Unlock()

	done := make(chan error, 1)
	ctx := bs.WithSoulID(contextWithConversationTurn(context.Background()), soulID)
	go func() {
		done <- g.SendConversationMessage(ctx, userID, "already locked")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SendConversationMessage: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SendConversationMessage tried to reacquire the coordinated turn lock")
	}
	select {
	case text := <-sent:
		if text != "already locked" {
			t.Fatalf("sent text = %q", text)
		}
	default:
		t.Fatal("raw coordinated sender was not called")
	}
}

func TestInboundActivityFenceTracksDebouncedMessages(t *testing.T) {
	g := &Gateway{}
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	msgs := []pendingMsg{{text: "first"}, {text: "second"}}

	g.trackInboundActivity(us, msgs)
	first := g.activitySnapshot(us.UserID, us.SoulID)
	if first.Token == "" || first.Version != 1 || first.Pending != 2 || first.LastInboundAt.IsZero() {
		t.Fatalf("activity after ingress = %#v", first)
	}
	for i, msg := range msgs {
		if !msg.activityTracked {
			t.Fatalf("message %d was not marked as tracked", i)
		}
	}

	g.clearInboundActivity(us, msgs)
	cleared := g.activitySnapshot(us.UserID, us.SoulID)
	if cleared.Version != first.Version || cleared.Pending != 0 {
		t.Fatalf("activity after persistence = %#v", cleared)
	}

	next := []pendingMsg{{text: "third"}}
	g.trackInboundActivity(us, next)
	advanced := g.activitySnapshot(us.UserID, us.SoulID)
	if advanced.Version != first.Version+1 || advanced.Pending != 1 {
		t.Fatalf("activity after next ingress = %#v", advanced)
	}
	if advanced.Token == first.Token {
		t.Fatal("activity token did not rotate after new ingress")
	}

	otherProcess := &Gateway{}
	if otherProcess.activitySnapshot(us.UserID, us.SoulID).Token == cleared.Token {
		t.Fatal("activity token was reused across gateway boots")
	}
}

func TestActivityFenceLinearizesAdmissionWithCommitSection(t *testing.T) {
	g := &Gateway{}
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	_, unlock := g.lockActivity(us.UserID, us.SoulID)

	admitted := make(chan struct{})
	go func() {
		g.admitInboundActivity(us)
		close(admitted)
	}()

	select {
	case <-admitted:
		t.Fatal("inbound admission crossed a held commit fence")
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("inbound admission did not resume after commit fence release")
	}
}

func TestRollbackInboundActivityClearsFailedLatestAdmission(t *testing.T) {
	g := &Gateway{}
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}

	version := g.admitInboundActivity(us)
	admitted := g.activitySnapshot(us.UserID, us.SoulID)
	g.rollbackInboundActivity(us, version)
	rolledBack := g.activitySnapshot(us.UserID, us.SoulID)

	if rolledBack.Pending != 0 || !rolledBack.LastInboundAt.IsZero() {
		t.Fatalf("rolled-back activity = %#v", rolledBack)
	}
	if rolledBack.Version != admitted.Version || rolledBack.Token != admitted.Token {
		t.Fatalf("rollback reused an older activity token: before=%#v after=%#v", admitted, rolledBack)
	}
}

func TestCompleteInboundActivityRequiresDurableAnchor(t *testing.T) {
	t.Run("silent or failed preflight clears latest inbound time", func(t *testing.T) {
		g := &Gateway{}
		us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
		version := g.admitInboundActivity(us)
		msgs := []pendingMsg{{
			text:            "silenced",
			activityTracked: true,
			activityVersion: version,
		}}

		g.completeInboundActivity(us, msgs, false)
		got := g.activitySnapshot(us.UserID, us.SoulID)
		if got.Pending != 0 || !got.LastInboundAt.IsZero() || got.Version != version {
			t.Fatalf("failed batch activity = %#v", got)
		}
	})

	t.Run("durable batch does not preserve a newer failed admission", func(t *testing.T) {
		g := &Gateway{}
		us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
		durableVersion := g.admitInboundActivity(us)
		msgs := []pendingMsg{{
			text:            "persisted",
			activityTracked: true,
			activityVersion: durableVersion,
		}}
		failedVersion := g.admitInboundActivity(us)
		g.rollbackInboundActivity(us, failedVersion)

		g.completeInboundActivity(us, msgs, true)
		got := g.activitySnapshot(us.UserID, us.SoulID)
		if got.Pending != 0 || !got.LastInboundAt.IsZero() || got.Version != failedVersion {
			t.Fatalf("mixed admission activity = %#v", got)
		}
	})

	t.Run("latest durable batch retains inbound time", func(t *testing.T) {
		g := &Gateway{}
		us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
		version := g.admitInboundActivity(us)
		msgs := []pendingMsg{{
			text:            "persisted",
			activityTracked: true,
			activityVersion: version,
		}}

		g.completeInboundActivity(us, msgs, true)
		got := g.activitySnapshot(us.UserID, us.SoulID)
		if got.Pending != 0 || got.LastInboundAt.IsZero() || got.Version != version {
			t.Fatalf("durable batch activity = %#v", got)
		}
	})
}

func TestAutonomousHistoryBarrierRetriesReceiptThenEnsuresSession(t *testing.T) {
	userID, soulID, attemptID := uuid.New(), uuid.New(), uuid.New()
	sessionID := uuid.NewString()
	receipt := bs.TaskNotificationReceipt{
		Transport: "telegram",
		MessageID: "42",
	}
	finalizeCalls := 0
	ensureCalls := 0
	failFinalize := true
	g := &Gateway{
		deps: &bs.Deps{
			FinalizeAutonomousNotification: func(
				_ context.Context,
				gotID uuid.UUID,
				gotReceipt bs.TaskNotificationReceipt,
			) error {
				finalizeCalls++
				if gotID != attemptID || gotReceipt.MessageID != receipt.MessageID {
					t.Fatalf("finalization got id=%s receipt=%#v", gotID, gotReceipt)
				}
				if failFinalize {
					return context.DeadlineExceeded
				}
				return nil
			},
			EnsureAutonomousHistory: func(
				_ context.Context,
				gotUserID, gotSoulID uuid.UUID,
				gotSessionID string,
			) error {
				ensureCalls++
				if gotUserID != userID || gotSoulID != soulID || gotSessionID != sessionID {
					t.Fatalf("ensure got user=%s soul=%s session=%s",
						gotUserID, gotSoulID, gotSessionID)
				}
				return nil
			},
		},
	}
	g.rememberAutonomousFinalization(userID, soulID, attemptID, receipt)

	if err := g.ensureAutonomousHistoryLocked(
		context.Background(), userID, soulID, sessionID,
	); err == nil {
		t.Fatal("failed finalization did not close the history barrier")
	}
	if finalizeCalls != 1 || ensureCalls != 0 {
		t.Fatalf("calls after failure: finalize=%d ensure=%d", finalizeCalls, ensureCalls)
	}

	failFinalize = false
	if err := g.ensureAutonomousHistoryLocked(
		context.Background(), userID, soulID, sessionID,
	); err != nil {
		t.Fatalf("retry history barrier: %v", err)
	}
	if finalizeCalls != 2 || ensureCalls != 1 {
		t.Fatalf("calls after retry: finalize=%d ensure=%d", finalizeCalls, ensureCalls)
	}
	if pending := g.activityState(userID, soulID).UnfinalizedAutonomous; len(pending) != 0 {
		t.Fatalf("finalized receipt remained pending: %#v", pending)
	}

	if err := g.ensureAutonomousHistoryLocked(
		context.Background(), userID, soulID, sessionID,
	); err != nil {
		t.Fatalf("second history barrier: %v", err)
	}
	if finalizeCalls != 2 || ensureCalls != 2 {
		t.Fatalf("idempotent calls: finalize=%d ensure=%d", finalizeCalls, ensureCalls)
	}
}

func TestPrepareCortexTurnAllowsNoTimingCollector(t *testing.T) {
	soulID := uuid.New()
	cfg := &bs.Config{}
	cfg.Gateway.ResolveSoulPersona = func(context.Context, uuid.UUID) (string, error) {
		return "soul prompt", nil
	}
	cfg.Gateway.ResolvePlatformPrompts = func(context.Context) (string, string, error) {
		return "platform preamble", "agents layer", nil
	}
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg},
		logger: slog.Default(),
		tz:     time.UTC,
	}
	us := &UserState{
		SoulID:   soulID,
		Registry: bs.NewToolRegistry(),
	}

	prepared, err := g.prepareCortexTurn(
		bs.WithSoulID(context.Background(), soulID),
		us,
		&session.Session{ID: uuid.NewString()},
		"association",
		"",
		nil,
		true,
	)
	if err != nil {
		t.Fatalf("prepareCortexTurn: %v", err)
	}
	if !strings.Contains(prepared.config.SystemPrompt, "soul prompt") {
		t.Fatalf("system prompt = %q", prepared.config.SystemPrompt)
	}
}

func TestPrepareAutonomousContextUsesOnlyDialogueSeed(t *testing.T) {
	userID := uuid.New()
	var gotMessage, gotPrior string
	var sawOrigin bool
	var gotRuleContext bs.RuleContext
	userDeps := &bs.Deps{
		UserID: userID,
		ReflexPreparer: func(ctx context.Context, _ string, message, prior string) *bs.ReflexContext {
			sawOrigin = bs.IsAutonomousTurn(ctx)
			gotMessage, gotPrior = message, prior
			return &bs.ReflexContext{FullContext: "full association", Strategy: "warm"}
		},
		RuleEngine: func(_ context.Context, rc bs.RuleContext) []bs.ActiveRule {
			gotRuleContext = rc
			return []bs.ActiveRule{
				{Scope: "keyword", Action: "must not appear", Silent: true},
				{Scope: "always", Action: "always guidance"},
			}
		},
	}
	g := &Gateway{
		deps: &bs.Deps{Config: &bs.Config{}},
		tz:   time.UTC,
	}
	us := &UserState{UserID: userID, Deps: userDeps}
	ctx := bs.ContextWithAutonomousTurn(context.Background())

	injected, guidance, silent := g.prepareAutonomousContext(ctx, us, "real recent dialogue")
	if silent {
		t.Fatal("keyword-scoped silent rule must not silence an autonomous turn")
	}
	if !sawOrigin {
		t.Fatal("ReflexPreparer did not receive autonomous origin")
	}
	if gotMessage != "real recent dialogue" || gotPrior != "" {
		t.Fatalf("association args message=%q prior=%q", gotMessage, gotPrior)
	}
	if injected != "full association" {
		t.Fatalf("injected context = %q", injected)
	}
	if !slices.Equal(gotRuleContext.AllowedScopes, []string{"always", "time", "user"}) {
		t.Fatalf("autonomous allowed scopes = %#v", gotRuleContext.AllowedScopes)
	}
	if !strings.Contains(guidance, "always guidance") || strings.Contains(guidance, "must not appear") {
		t.Fatalf("filtered guidance = %q", guidance)
	}
}

func TestPrepareAutonomousContextHonorsAlwaysSilentRule(t *testing.T) {
	userID := uuid.New()
	g := &Gateway{
		deps: &bs.Deps{Config: &bs.Config{}},
		tz:   time.UTC,
	}
	us := &UserState{
		UserID: userID,
		Deps: &bs.Deps{
			UserID: userID,
			RuleEngine: func(context.Context, bs.RuleContext) []bs.ActiveRule {
				return []bs.ActiveRule{{Scope: "always", Silent: true}}
			},
		},
	}

	_, _, silent := g.prepareAutonomousContext(bs.ContextWithAutonomousTurn(context.Background()), us, "dialogue")
	if !silent {
		t.Fatal("always-scoped silent rule was ignored")
	}
}
