package agenttask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/core"
)

type recordingMCPSource struct {
	seen  []uuid.UUID
	tools []core.MCPTool
}

func (s *recordingMCPSource) ToolsForSoul(_ context.Context, soulID uuid.UUID) []core.MCPTool {
	s.seen = append(s.seen, soulID)
	return s.tools
}

func (*recordingMCPSource) Invalidate(uuid.UUID) {}

func noopTool(context.Context, json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil }

type schedulerDeliveryLedger struct {
	mark func([]core.TaskDeliveryRef) error
}

func (*schedulerDeliveryLedger) LookupDelivered(context.Context, uuid.UUID, []core.TaskDeliveryRef) (map[core.TaskDeliveryRef]bool, error) {
	return nil, nil
}

func (l *schedulerDeliveryLedger) MarkDelivered(_ context.Context, _ uuid.UUID, refs []core.TaskDeliveryRef) error {
	if l.mark == nil {
		return nil
	}
	return l.mark(refs)
}

func TestDeliverTaskNotificationMarksOnlyAfterSuccessfulNotify(t *testing.T) {
	taskID, userID := uuid.New(), uuid.New()
	refs := []core.TaskDeliveryRef{{InputID: "calendar", ItemKey: "event:1"}}

	t.Run("success orders notify before mark", func(t *testing.T) {
		var order []string
		ledger := &schedulerDeliveryLedger{mark: func(got []core.TaskDeliveryRef) error {
			order = append(order, "mark")
			if len(got) != 1 || got[0] != refs[0] {
				t.Fatalf("marked refs = %#v, want %#v", got, refs)
			}
			return nil
		}}
		notified, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) error {
				order = append(order, "notify")
				return nil
			},
			ledger, taskID, userID, "soon", refs,
		)
		if err != nil || !notified || strings.Join(order, ",") != "notify,mark" {
			t.Fatalf("notified=%v err=%v order=%v", notified, err, order)
		}
	})

	t.Run("failed notify does not mark", func(t *testing.T) {
		marked := false
		ledger := &schedulerDeliveryLedger{mark: func([]core.TaskDeliveryRef) error {
			marked = true
			return nil
		}}
		notified, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) error { return errors.New("telegram down") },
			ledger, taskID, userID, "soon", refs,
		)
		if err == nil || notified || marked {
			t.Fatalf("notified=%v err=%v marked=%v", notified, err, marked)
		}
	})

	t.Run("mark failure reports transport success", func(t *testing.T) {
		notifyCalls := 0
		ledger := &schedulerDeliveryLedger{mark: func([]core.TaskDeliveryRef) error {
			return errors.New("database down")
		}}
		notified, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) error {
				notifyCalls++
				return nil
			},
			ledger, taskID, userID, "soon", refs,
		)
		if err == nil || !strings.Contains(err.Error(), "mark deliveries") || !notified || notifyCalls != 1 {
			t.Fatalf("notified=%v err=%v notify_calls=%d, want sent-but-uncommitted", notified, err, notifyCalls)
		}
	})

	t.Run("no-op does not notify or mark", func(t *testing.T) {
		notifyCalls, markCalls := 0, 0
		ledger := &schedulerDeliveryLedger{mark: func([]core.TaskDeliveryRef) error {
			markCalls++
			return nil
		}}
		notified, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) error {
				notifyCalls++
				return nil
			},
			ledger, taskID, userID, "[no-op]", refs,
		)
		if err != nil || notified || notifyCalls != 0 || markCalls != 0 {
			t.Fatalf("notified=%v err=%v notify=%d mark=%d", notified, err, notifyCalls, markCalls)
		}
	})
}

func TestRegistryForTaskEnforcesHandlerCeiling(t *testing.T) {
	base := core.NewToolRegistry()
	base.Register("safe", "", json.RawMessage(`{}`), noopTool)
	base.Register("dangerous", "", json.RawMessage(`{}`), noopTool)

	registry := registryForTask(base, []string{"safe"}, []string{"safe", "dangerous"}, true)
	if !registry.Has("safe") || registry.Has("dangerous") || registry.Count() != 1 {
		t.Fatalf("task expanded handler ceiling: definitions=%v", registry.Definitions())
	}

	// Without a handler ceiling, the legacy task list remains the narrowing.
	registry = registryForTask(base, nil, []string{"dangerous"}, true)
	if registry.Has("safe") || !registry.Has("dangerous") || registry.Count() != 1 {
		t.Fatalf("legacy no-ceiling selection changed: definitions=%v", registry.Definitions())
	}
}

func TestComposeSoulTaskRegistryCallsMCPForRequestedToolAndPreservesSoul(t *testing.T) {
	soulID := uuid.New()
	source := &recordingMCPSource{tools: []core.MCPTool{{
		Name:        "mcp__github__list_issues",
		Description: "issues",
		Schema:      json.RawMessage(`{}`),
		Handler:     noopTool,
	}}}
	cfg := &core.Config{MCPSource: source}
	scheduler := &Scheduler{deps: &core.Deps{Config: cfg}}
	base := core.NewToolRegistry()
	base.Register("native", "", json.RawMessage(`{}`), noopTool)

	registry, err := scheduler.composeSoulTaskRegistry(context.Background(), core.AgentTask{SoulID: soulID}, base, []string{"mcp__github__list_issues"})
	if err != nil {
		t.Fatalf("composeSoulTaskRegistry: %v", err)
	}
	if len(source.seen) != 1 || source.seen[0] != soulID {
		t.Fatalf("MCP source souls = %v, want [%s]", source.seen, soulID)
	}
	if !registry.Has("mcp__github__list_issues") || registry.PeerForTool("mcp__github__list_issues") != "mcp" {
		t.Fatalf("MCP tool not registered as peer:mcp: %v", registry.Definitions())
	}
	if base.Has("mcp__github__list_issues") {
		t.Fatal("shared native registry was mutated")
	}

	// The host wildcard admits MCP as a class, then the exact task request
	// narrows it to this one concrete capability.
	registry = registryForTask(registry, []string{"native", "peer:mcp"}, []string{"mcp__github__list_issues"}, true)
	if registry.Count() != 1 || !registry.Has("mcp__github__list_issues") || registry.Has("native") {
		t.Fatalf("MCP wildcard/request intersection failed: %v", registry.Definitions())
	}
}

func TestComposeSoulTaskRegistryDoesNotWakeMCPForNativeOnlyTask(t *testing.T) {
	source := &recordingMCPSource{}
	cfg := &core.Config{MCPSource: source}
	scheduler := &Scheduler{deps: &core.Deps{Config: cfg}}
	base := core.NewToolRegistry()
	base.Register("notes_list", "", json.RawMessage(`{}`), noopTool)

	registry, err := scheduler.composeSoulTaskRegistry(context.Background(), core.AgentTask{SoulID: uuid.New()}, base, []string{"notes_list"})
	if err != nil {
		t.Fatalf("composeSoulTaskRegistry: %v", err)
	}
	if len(source.seen) != 0 {
		t.Fatalf("native-only task cold-connected MCP for souls %v", source.seen)
	}
	if registry != base {
		t.Fatal("native-only task should reuse its native registry when no policy is configured")
	}
}

func TestTaskProgramQuietHoursSuppressMCPDiscovery(t *testing.T) {
	program := core.TaskProgram{
		Schema:     core.TaskProgramSchemaV1,
		Activation: core.TaskProgramActivation{Mode: core.TaskProgramActivationAlways},
		Decision:   core.TaskProgramDecision{Mode: core.TaskProgramDecisionSelected, Tools: []string{"mcp__github__list_issues"}},
		QuietHours: &core.TaskProgramQuietHours{FromHour: 22, ToHour: 7},
	}
	config, _ := json.Marshal(map[string]any{"task_program": program})
	task := core.AgentTask{SoulID: uuid.New(), Config: config}
	if !taskProgramInQuietHours(context.Background(), task, &core.Config{Timezone: "UTC"}, time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC)) {
		t.Fatal("23:00 UTC should be inside wrapped quiet hours")
	}
	if taskProgramInQuietHours(context.Background(), task, &core.Config{Timezone: "UTC"}, time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("12:00 UTC should be outside wrapped quiet hours")
	}
}

func TestComposeSoulTaskRegistryPolicyErrorFailsClosed(t *testing.T) {
	soulID := uuid.New()
	cfg := &core.Config{}
	cfg.Gateway.ResolveSoulToolPolicy = func(context.Context, uuid.UUID) (map[string]bool, []string, error) {
		return nil, nil, errors.New("policy database down")
	}
	scheduler := &Scheduler{deps: &core.Deps{Config: cfg}}
	base := core.NewToolRegistry()
	base.Register("message_send", "", json.RawMessage(`{}`), noopTool)

	registry, err := scheduler.composeSoulTaskRegistry(context.Background(), core.AgentTask{SoulID: soulID}, base, []string{"message_send"})
	if err == nil || !strings.Contains(err.Error(), "policy database down") {
		t.Fatalf("policy error = %v, want explicit failure", err)
	}
	if registry.Count() != 0 {
		t.Fatalf("policy error did not fail closed: %v", registry.Definitions())
	}
}

func TestComposeSoulTaskRegistryStripsDisconnectedAndDisabledTools(t *testing.T) {
	soulID := uuid.New()
	var policySoul uuid.UUID
	cfg := &core.Config{ToolMeta: map[string]core.ToolMeta{
		"calendar_list": {Provider: "google_calendar"},
		"disabled":      {},
		"core_internal": {Core: true},
	}}
	cfg.Gateway.ResolveSoulToolPolicy = func(_ context.Context, gotSoul uuid.UUID) (map[string]bool, []string, error) {
		policySoul = gotSoul
		return map[string]bool{"disabled": false, "core_internal": false}, []string{"github"}, nil
	}
	scheduler := &Scheduler{deps: &core.Deps{Config: cfg}}
	base := core.NewToolRegistry()
	for _, name := range []string{"calendar_list", "disabled", "core_internal", "notes_list"} {
		base.Register(name, "", json.RawMessage(`{}`), noopTool)
	}

	registry, err := scheduler.composeSoulTaskRegistry(context.Background(), core.AgentTask{SoulID: soulID}, base, []string{"notes_list"})
	if err != nil {
		t.Fatalf("composeSoulTaskRegistry: %v", err)
	}
	if policySoul != soulID {
		t.Fatalf("policy soul = %s, want %s", policySoul, soulID)
	}
	if registry.Has("calendar_list") || registry.Has("disabled") {
		t.Fatalf("disconnected/disabled tools survived policy: %v", registry.Definitions())
	}
	if !registry.Has("notes_list") || !registry.Has("core_internal") {
		t.Fatalf("default-on/core tools were lost: %v", registry.Definitions())
	}
}

func TestDurationDueTolerance(t *testing.T) {
	interval := 5 * time.Minute
	if !durationDue(interval-500*time.Millisecond, interval) {
		t.Fatal("sub-second early scheduler tick should be due")
	}
	if durationDue(interval-time.Second-time.Nanosecond, interval) {
		t.Fatal("scheduler must not run more than one second early")
	}
	if !durationDue(interval, interval) {
		t.Fatal("exact interval must be due")
	}

	// The tolerance is clamped below very short intervals.
	if durationDue(0, 500*time.Millisecond) {
		t.Fatal("short interval became continuously due")
	}
}

// newDailyGateScheduler builds the minimal Scheduler the daily_at gate
// needs: deps with a Config (process tz = UTC, i.e. "server time" in
// these tests) plus an optional per-soul ResolveTimezone hook, and a
// logger. No store/handlers — the gate must decide skips without them.
func newDailyGateScheduler(resolveTZ func(ctx context.Context) string, logW io.Writer) *Scheduler {
	if logW == nil {
		logW = io.Discard
	}
	cfg := &core.Config{Timezone: "UTC"}
	cfg.Gateway.ResolveTimezone = resolveTZ
	return &Scheduler{
		deps:   &core.Deps{Config: cfg},
		logger: slog.New(slog.NewTextHandler(logW, nil)),
	}
}

func dailyTask(config string, lastRun *time.Time) core.AgentTask {
	sched := "30m"
	t := core.AgentTask{
		ID:        uuid.New(),
		Title:     "morning digest",
		Handler:   "digest",
		Schedule:  &sched,
		LastRunAt: lastRun,
	}
	if config != "" {
		t.Config = json.RawMessage(config)
	}
	return t
}

// TestDailyGateOpen covers the once-a-day window semantics on the
// server-tz fallback path (no ResolveTimezone hook → process tz = UTC).
// A false result is a pre-dispatch skip: Run() `continue`s before
// SetRunning, so no handler run, no iteration, no last_run_at write.
// A true result only means the gate defers to the existing schedule/
// cadence checks — which is also why every absent/malformed-config case
// must come back true regardless of clock or last run.
func TestDailyGateOpen(t *testing.T) {
	at := func(hour, min int) time.Time {
		return time.Date(2026, 7, 8, hour, min, 0, 0, time.UTC)
	}
	dispatched := at(9, 5) // what SetRunning persisted at the 09:05 dispatch
	const cfg9 = `{"daily_at": {"hour": 9}}`

	cases := []struct {
		name    string
		config  string
		lastRun *time.Time
		now     time.Time
		want    bool
	}{
		// Window + once-per-day flow.
		{"before window skips", cfg9, nil, at(8, 55), false},
		{"in window, not yet run today, dispatches", cfg9, nil, at(9, 5), true},
		{"in window but already dispatched today skips", cfg9, &dispatched, at(9, 35), false},
		{"next day in window dispatches again", cfg9, &dispatched, at(9, 5).AddDate(0, 0, 1), true},
		{"ran yesterday outside window still skips", cfg9, &dispatched, at(15, 0).AddDate(0, 0, 1), false},
		// Hour boundary: the window is the whole local hour.
		{"last minute of window dispatches", cfg9, nil, at(9, 59), true},
		{"top of the next hour skips", cfg9, nil, at(10, 0), false},
		// No daily_at → gate open, existing schedule/cadence behavior untouched.
		{"nil config leaves gate open", "", &dispatched, at(9, 35), true},
		{"empty config object leaves gate open", `{}`, &dispatched, at(3, 0), true},
		{"config without daily_at leaves gate open", `{"prompt":"heartbeat"}`, &dispatched, at(3, 0), true},
		// Malformed hour → treated as absent (gate open).
		{"hour 24 treated as absent", `{"daily_at":{"hour":24}}`, &dispatched, at(3, 0), true},
		{"negative hour treated as absent", `{"daily_at":{"hour":-1}}`, nil, at(3, 0), true},
		{"missing hour treated as absent", `{"daily_at":{}}`, nil, at(3, 0), true},
		// Midnight is a valid window, distinct from "hour missing".
		{"hour 0 gates on midnight", `{"daily_at":{"hour":0}}`, nil, at(0, 30), true},
		{"hour 0 skips outside midnight", `{"daily_at":{"hour":0}}`, nil, at(9, 30), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDailyGateScheduler(nil, nil)
			task := dailyTask(tc.config, tc.lastRun)
			if got := s.dailyGateOpen(context.Background(), task, tc.now); got != tc.want {
				t.Errorf("dailyGateOpen(now=%s, lastRun=%v) = %v, want %v",
					tc.now.Format("2006-01-02 15:04"), tc.lastRun, got, tc.want)
			}
		})
	}
}

// TestDailyGateSurvivesRestart proves the once-per-day guard is derived
// from the persisted task row (last_run_at, written by SetRunning at
// dispatch), not from scheduler memory: a brand-new Scheduler — a daemon
// restart — must still skip the rest of the day and reopen tomorrow.
func TestDailyGateSurvivesRestart(t *testing.T) {
	at := func(hour, min int) time.Time {
		return time.Date(2026, 7, 8, hour, min, 0, 0, time.UTC)
	}
	task := dailyTask(`{"daily_at": {"hour": 9}}`, nil)

	s1 := newDailyGateScheduler(nil, nil)
	if !s1.dailyGateOpen(context.Background(), task, at(9, 5)) {
		t.Fatal("09:05 first tick of the day: gate must be open")
	}
	// The dispatch stamped last_run_at = NOW() via SetRunning; that is the
	// only state the guard may rely on.
	ts := at(9, 5)
	task.LastRunAt = &ts

	s2 := newDailyGateScheduler(nil, nil) // restart: fresh in-memory state
	if s2.dailyGateOpen(context.Background(), task, at(9, 35)) {
		t.Error("09:35 after restart: gate must stay closed for the rest of the day")
	}
	if !s2.dailyGateOpen(context.Background(), task, at(9, 5).AddDate(0, 0, 1)) {
		t.Error("next day 09:05 after restart: gate must reopen")
	}
}

// TestDailyGateOwnerTimezone pins the gate to the task OWNER's wall
// clock, resolved through the soul-pinned Gateway.ResolveTimezone hook
// (the same one the background handler uses for [current_datetime]).
// Owner at UTC+3, server at UTC: 06:30 server time IS the owner's 9
// o'clock; 09:30 server time is not.
func TestDailyGateOwnerTimezone(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Moscow"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	utc := func(hour, min int) time.Time {
		return time.Date(2026, 7, 8, hour, min, 0, 0, time.UTC)
	}
	task := dailyTask(`{"daily_at": {"hour": 9}}`, nil)
	task.SoulID = uuid.New()

	var hookSoul uuid.UUID
	s := newDailyGateScheduler(func(ctx context.Context) string {
		hookSoul = core.SoulIDFromContext(ctx)
		return "Europe/Moscow" // UTC+3, no DST
	}, nil)

	if !s.dailyGateOpen(context.Background(), task, utc(6, 30)) {
		t.Error("06:30 UTC = 09:30 owner-local: gate must be open")
	}
	if hookSoul != task.SoulID {
		t.Errorf("ResolveTimezone saw soul %s, want the task's soul %s", hookSoul, task.SoulID)
	}
	if s.dailyGateOpen(context.Background(), task, utc(9, 30)) {
		t.Error("09:30 UTC = 12:30 owner-local: gate must be closed (server hour must not win)")
	}

	// Once-per-day compares owner-LOCAL dates: a run at 06:05 UTC (09:05
	// owner-local, same local day) closes the 06:30 UTC tick.
	ran := utc(6, 5)
	task.LastRunAt = &ran
	if s.dailyGateOpen(context.Background(), task, utc(6, 30)) {
		t.Error("06:30 UTC after a 06:05 UTC dispatch: same owner-local day, gate must be closed")
	}
	// 21:30 UTC July 8 = 00:30 July 9 owner-local — a NEW local day, but
	// outside the 9 o'clock window, so still closed.
	if s.dailyGateOpen(context.Background(), task, utc(21, 30)) {
		t.Error("21:30 UTC: new owner-local day but outside the window, gate must be closed")
	}
}

// TestDailyGateServerFallback: without a ResolveTimezone hook the gate
// documented fallback is the configured process (server) timezone.
func TestDailyGateServerFallback(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Moscow"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	cfg := &core.Config{Timezone: "Europe/Moscow"} // server at UTC+3, no hook
	s := &Scheduler{
		deps:   &core.Deps{Config: cfg},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	task := dailyTask(`{"daily_at": {"hour": 9}}`, nil)

	sixThirtyUTC := time.Date(2026, 7, 8, 6, 30, 0, 0, time.UTC) // 09:30 server-local
	if !s.dailyGateOpen(context.Background(), task, sixThirtyUTC) {
		t.Error("06:30 UTC = 09:30 server-local: gate must be open on the server-tz fallback")
	}
	nineThirtyUTC := time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC) // 12:30 server-local
	if s.dailyGateOpen(context.Background(), task, nineThirtyUTC) {
		t.Error("09:30 UTC = 12:30 server-local: gate must be closed on the server-tz fallback")
	}
}

// TestDailyAtMalformedWarnsOnce: a bad hour is re-parsed every tick, but
// the WARN must fire once per task, not every 60 seconds.
func TestDailyAtMalformedWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	s := newDailyGateScheduler(nil, &buf)
	task := dailyTask(`{"daily_at": {"hour": 24}}`, nil)

	for i := 0; i < 3; i++ {
		if _, ok := s.dailyAtHour(task); ok {
			t.Fatal("malformed hour must parse as absent")
		}
	}
	if n := strings.Count(buf.String(), "invalid daily_at hour"); n != 1 {
		t.Fatalf("want exactly 1 warning after 3 parses, got %d:\n%s", n, buf.String())
	}

	// A different task with its own bad config still gets its one warning.
	other := dailyTask(`{"daily_at": {}}`, nil)
	if _, ok := s.dailyAtHour(other); ok {
		t.Fatal("missing hour must parse as absent")
	}
	if n := strings.Count(buf.String(), "invalid daily_at hour"); n != 2 {
		t.Fatalf("want a second warning for a second task, got %d total", n)
	}
}
