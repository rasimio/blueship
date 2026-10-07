package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/provider/anthropic"
	"github.com/rasimio/blueship/internal/provider/gemini"
	"github.com/rasimio/blueship/runtime/handler"
	"github.com/rasimio/blueship/runtime/session"
	"github.com/rasimio/blueship/tool"
)

// Snapshot credentials only: never rotate or write the production token pair.
type liveAccessToken struct {
	value   string
	invalid atomic.Bool
}

func (t *liveAccessToken) AccessToken() (string, error) {
	if t.invalid.Load() || t.value == "" {
		return "", fmt.Errorf("live OAuth access token unavailable or rejected")
	}
	return t.value, nil
}
func (t *liveAccessToken) Invalidate() { t.invalid.Store(true) }

type liveCompletionMeter struct {
	provider core.CompletionProvider
	mu       sync.Mutex
	calls    []map[string]any
}

func (m *liveCompletionMeter) Complete(ctx context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	start := time.Now()
	response, err := m.provider.Complete(ctx, req)
	finished := time.Now()
	systemHash := sha256.Sum256([]byte(req.System))
	m.mu.Lock()
	failure := ""
	if err != nil {
		failure = err.Error()
		for _, key := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "BLUESHIP_LIVE_OAUTH_ACCESS_TOKEN"} {
			if value := os.Getenv(key); value != "" {
				failure = strings.ReplaceAll(failure, value, "[credential]")
			}
		}
	}
	call := map[string]any{"started_at": start.UTC(), "finished_at": finished.UTC(), "duration_ms": finished.Sub(start).Milliseconds(), "max_output_tokens": req.MaxTokens, "effort": req.Effort, "thinking_mode": req.ThinkingMode, "system_sha256": fmt.Sprintf("%x", systemHash), "model": req.Model, "tools": len(req.Tools), "failed": err != nil, "error": failure, "response": response}
	// Retain exact public-source audit input for focused replay after the
	// isolated database is deleted. No credentials or transport config.
	if strings.HasPrefix(req.System, "You are a citation auditor") {
		call["audit_system"] = req.System
		call["audit_messages"] = req.Messages
	}
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	return response, err
}

// Opt-in live smoke through the real scheduler, PostgreSQL sessions and browser
// tools. This is not a speed benchmark: no baseline or human quality grading.
// The only allowed DB endpoint is the isolated local test service on port15433.
func TestLiveBackgroundGraph(t *testing.T) {
	if os.Getenv("BLUESHIP_LIVE_BACKGROUND") != "1" {
		t.Skip("set BLUESHIP_LIVE_BACKGROUND=1")
	}
	adminURL, err := url.Parse(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if err != nil || adminURL.Hostname() != "127.0.0.1" || adminURL.Port() != "15433" {
		t.Fatal("live probe requires isolated local port15433")
	}
	model, prompts := os.Getenv("BLUESHIP_LIVE_MODEL"), os.Getenv("BLUESHIP_LIVE_PROMPTS")
	if model == "" || prompts == "" {
		t.Fatal("explicit model and host prompt directory required")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	admin, err := sqlx.Connect("postgres", adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	database := "background_live_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(database)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(`DROP DATABASE ` + pq.QuoteIdentifier(database) + ` WITH (FORCE)`)
		if err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	// Keep the admin connection alive through cleanup, after all task pools close.
	adminURL.Path = "/" + database
	query := adminURL.Query()
	query.Set("search_path", "blueship,public")
	adminURL.RawQuery = query.Encode()
	cfg := &core.Config{DB: adminURL.String(), ShipSchema: "blueship"}
	cfg.ApplyDefaults()
	cfg.Models.Primary.Name = model
	cfg.Models.Primary.Effort = os.Getenv("BLUESHIP_LIVE_EFFORT")
	cfg.Models.Primary.ThinkingMode = os.Getenv("BLUESHIP_LIVE_THINKING_MODE")
	providerName := os.Getenv("BLUESHIP_LIVE_PROVIDER")
	if providerName == "" {
		providerName = "anthropic"
	}
	credential := "ANTHROPIC_API_KEY"
	var provider core.CompletionProvider
	switch providerName {
	case "anthropic":
		provider = anthropic.NewProvider(os.Getenv(credential), 90*time.Second, nil, logger)
	case "anthropic-oauth":
		credential = "BLUESHIP_LIVE_OAUTH_ACCESS_TOKEN"
		provider = anthropic.NewOAuthProvider(&liveAccessToken{value: os.Getenv(credential)}, 90*time.Second, nil, logger)
	case "gemini":
		credential = "GEMINI_API_KEY"
		provider = gemini.NewCompletionProvider(os.Getenv(credential), 90*time.Second)
	default:
		t.Fatal("unsupported live provider")
	}
	cfg.Models.Primary.Provider = providerName
	meter := &liveCompletionMeter{provider: core.NewLLMRouter(provider, map[string]core.CompletionProvider{providerName: provider}, nil)}
	cfg.LLM = meter
	deps, err := core.InitDeps(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deps.Close)
	db, err := deps.DB("ship")
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(db, logger); err != nil {
		t.Fatal(err)
	}
	// Optional phase-policy experiment: keep final acceptance/cortex and source
	// auditors at their supplied primary effort while varying background work.
	if effort := os.Getenv("BLUESHIP_LIVE_BACKGROUND_EFFORT"); effort != "" {
		for _, role := range []string{"background", "compact", "grounding_evaluator", "cortex"} {
			roleEffort, mode := cfg.Models.Primary.Effort, ""
			if role == "background" {
				roleEffort = effort
			}
			if role == "cortex" {
				mode = cfg.Models.Primary.ThinkingMode
			}
			if role == "grounding_evaluator" {
				if override := os.Getenv("BLUESHIP_LIVE_GROUNDING_EFFORT"); override != "" {
					roleEffort = override
				}
			}
			if _, err := db.Exec(`INSERT INTO model_config(role,provider,model_name,effort,thinking_mode) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(role) DO UPDATE SET provider=EXCLUDED.provider,model_name=EXCLUDED.model_name,effort=EXCLUDED.effort,thinking_mode=EXCLUDED.thinking_mode`, role, providerName, model, roleEffort, mode); err != nil {
				t.Fatal(err)
			}
		}
		models := core.NewModelConfigStore(db)
		if err := models.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		deps.ModelStore = models
	}
	// Host tenancy and session projection columns, absent from generic ship init.
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS soul_id uuid;
 ALTER TABLE agent_task_tool_outputs ADD COLUMN IF NOT EXISTS soul_id uuid;
 ALTER TABLE agent_task_iterations ADD COLUMN IF NOT EXISTS soul_id uuid;
 ALTER TABLE chat_sessions ADD COLUMN IF NOT EXISTS soul_id uuid,ADD COLUMN IF NOT EXISTS last_input_tokens integer;
 ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS soul_id uuid,ADD COLUMN IF NOT EXISTS visible_text text,
 ADD COLUMN IF NOT EXISTS projection_status text NOT NULL DEFAULT '',ADD COLUMN IF NOT EXISTS projection_reason text,
 ADD COLUMN IF NOT EXISTS projector_version text NOT NULL DEFAULT '',
 ADD COLUMN IF NOT EXISTS reply_to_message_id uuid,ADD COLUMN IF NOT EXISTS tg_message_id bigint,ADD COLUMN IF NOT EXISTS tg_message_ids bigint[];`); err != nil {
		t.Fatal(err)
	}
	preflight := session.NewStore(db)
	preflightCtx := core.WithSoulID(context.Background(), uuid.New())
	sessionID, err := preflight.CreateSessionWithSource(preflightCtx, uuid.NewString(), model, "agent_task", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if err := preflight.Append(preflightCtx, sessionID, core.Message{Role: "user", Content: "schema preflight"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM chat_sessions WHERE id=$1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("BLUESHIP_LIVE_SETUP_ONLY") == "1" {
		t.Log("isolated schema initialized")
		return
	}
	if os.Getenv(credential) == "" {
		t.Fatal("live provider credential unavailable")
	}
	deps.Prompts = core.NewFilePromptStore(prompts)
	deps.EnsureAutonomousHistory = func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }
	registry := core.NewToolRegistry()
	if err := tool.RegisterBrowserTools(registry, deps); err != nil {
		t.Fatal(err)
	}
	user, soul := uuid.New(), uuid.New()
	if _, err := db.Exec(`INSERT INTO user_profiles(id,chat_id,display_name) VALUES($1,'test:live-background','Isolated probe')`, user); err != nil {
		t.Fatal(err)
	}
	description := os.Getenv("BLUESHIP_LIVE_TASK")
	if description == "" {
		t.Fatal("explicit task description required")
	}
	criteria := os.Getenv("BLUESHIP_LIVE_CRITERIA")
	if criteria == "" {
		t.Fatal("explicit acceptance criteria required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	deadline := time.Now().Add(9 * time.Minute)
	store := core.NewAgentTaskStore(db)
	task, err := store.Create(core.WithSoulID(ctx, soul), core.AgentTask{UserID: user, Title: "Isolated live research", Description: &description, AcceptanceCriteria: &criteria, Strategy: core.StrategyDirect, Handler: "background", ExecutorVersion: 2, Deadline: &deadline})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := agenttask.NewScheduler(store, nil, nil, registry, session.NewStore(db), deps, nil, logger)
	scheduler.SetGraphHandler(handler.NewBackground(time.UTC, nil, nil, []string{"browser_fetch", "browser_search"}))
	start := time.Now()
	for ctx.Err() == nil {
		if err := scheduler.Run(ctx); err != nil {
			t.Fatal(err)
		}
		scheduler.Wait()
		task, err = store.Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == "done" || task.Status == "failed" || task.Status == "canceled" {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	steps, stepsErr := store.TaskSteps(context.Background(), task.ID)
	captureCtx, endCapture := context.WithTimeout(context.Background(), 15*time.Second)
	sources, sourcesErr := captureLiveSources(captureCtx, db, task.ID)
	endCapture()
	meter.mu.Lock()
	calls := append([]map[string]any(nil), meter.calls...)
	meter.mu.Unlock()
	report := map[string]any{"task": task, "steps": steps, "calls": calls, "elapsed_ms": time.Since(start).Milliseconds(), "model": model, "delivery": "disabled", "steps_error": fmt.Sprint(stepsErr)}
	report["sources"], report["sources_error"] = sources, fmt.Sprint(sourcesErr)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	output := os.Getenv("BLUESHIP_LIVE_REPORT")
	if output == "" {
		output = filepath.Join(t.TempDir(), "report.json")
	}
	if err := os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("task=%s status=%s elapsed=%s calls=%d report=%s", task.ID, task.Status, time.Since(start).Round(time.Second), len(calls), output)
	if sourcesErr != nil {
		t.Fatalf("source snapshot unavailable; failure recorded in %s: %v", output, sourcesErr)
	}
	if task.Status != "done" || task.Result == nil {
		t.Fatalf("live task did not produce a verified complete result: status=%s error=%v", task.Status, coreErrorString(task.ErrorMessage))
	}
}

// Preserve exact browser evidence before the isolated database is dropped,
// including windows' metadata and original observation times. This does not
// copy provider credentials or configuration into the task-scoped artifact.
func captureLiveSources(ctx context.Context, db *sqlx.DB, taskID uuid.UUID) (json.RawMessage, error) {
	var raw []byte
	err := db.GetContext(ctx, &raw, `SELECT COALESCE(jsonb_agg(jsonb_build_object(
	 'tool_name',tool_name,'tool_input',tool_input,'output',output,
	 'output_format',output_format,'metadata',metadata,'iteration',iteration,
	 'created_at',created_at) ORDER BY created_at),'[]'::jsonb)
	 FROM agent_task_tool_outputs WHERE task_id=$1 AND tool_name='browser_fetch'`, taskID)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

func coreErrorString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
