# BlueShip

BlueShip is a Go 1.24 framework for building production AI agents. It provides the orchestration layer — LLM loop, tool dispatch, session persistence, Telegram transport, and background jobs — so that your application only needs to implement domain-specific modules.

**Import path:** `github.com/rasimio/blueship`

---

## Features

- **Multi-provider LLM support** — Anthropic (Claude), OpenAI, and Gemini out of the box
- **Module system** — opt-in interfaces for tools, background jobs, and CLI commands
- **Persistent sessions** — per-user chat sessions stored in PostgreSQL with automatic daily reset
- **Conversation compaction** — automatic summarization when context exceeds token budget
- **Extended thinking** — native support for Anthropic's extended thinking (ThinkingBudget)
- **Telegram transport** — full-featured: text, voice (Whisper), photos, file attachments, reply context, debouncing
- **Built-in tools** — `current_time`, `web_search` (Serper), `web_fetch` (HTML→text)
- **Background jobs** — panic-safe scheduler with heartbeat and autonomous thinking jobs
- **Auto-migrations** — embedded SQL migrations applied on startup
- **Redis optional** — non-fatal if unavailable; used for caching
- **Provider-based DI** — all AI services injected as interfaces, fully replaceable

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                         Your App                            │
│  main.go: blueship.New(cfg) → RegisterModule → Run(ctx)     │
└───────────────────────────┬─────────────────────────────────┘
                            │
┌───────────────────────────▼─────────────────────────────────┐
│                         Ship                                 │
│  1. InitDeps (DB pool, Redis)                                │
│  2. migrate.Run (auto-apply embedded SQL)                    │
│  3. user.ResolveOwner                                        │
│  4. Module JobProviders → scheduler.RunLoop (goroutines)     │
│  5. Gateway.Run → Telegram Poller                            │
└──────────┬────────────────────────────────┬─────────────────┘
           │ updates                        │ heartbeat / thinking
┌──────────▼──────────┐          ┌──────────▼──────────────────┐
│   Telegram Poller   │          │   Scheduler                  │
│   long-poll 30s     │          │   HeartbeatJob  (30 min)     │
│   voice → Whisper   │          │   ThinkingJob   (60 min)     │
│   photo → base64    │          │   Module Jobs   (configurable│
│   file → text       │          └──────────────────────────────┘
└──────────┬──────────┘
           │ per-user debounce (1.5s window)
┌──────────▼──────────────────────────────────────────────────┐
│   Gateway                                                    │
│   getOrInitUser → resolve UUID, is_owner                     │
│   RegisterBuiltinTools + Module.RegisterTools                │
│   cancel thinking loop if user speaks                        │
└──────────┬──────────────────────────────────────────────────┘
           │
┌──────────▼──────────────────────────────────────────────────┐
│   Agent Loop (agent/loop.go)                                 │
│   1. Append user message to session                          │
│   2. Compact if messages exceed threshold                    │
│   for turn in MaxTurns:                                      │
│     3. Build effective system (base + compact_summary)       │
│     4. Load messages within token budget                     │
│     5. provider.Complete(request)                            │
│     6. Persist assistant message                             │
│     7. If tool_use → dispatch → append results → continue   │
│     8. If end_turn / max_tokens → return text               │
└──────────┬──────────────────────────────────────────────────┘
           │
┌──────────▼──────────────────────────────────────────────────┐
│   Providers (interfaces)                                     │
│   CompletionProvider   EmbeddingProvider   TranscriptionProvider│
│   SearchEngine         WebFetcher          MessageSender     │
└─────────────────────────────────────────────────────────────┘
```

---

## Quick Start

### Prerequisites

- Go 1.24+
- PostgreSQL 14+
- A Telegram bot token (`@BotFather`)
- An Anthropic, OpenAI, or Gemini API key

### Install

```bash
go get github.com/rasimio/blueship
```

### Minimal agent

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/rasimio/blueship"
)

func main() {
    ship := blueship.New(blueship.Config{
        LLM:       blueship.Anthropic(os.Getenv("ANTHROPIC_API_KEY")),
        Transport: blueship.Telegram(os.Getenv("TELEGRAM_BOT_TOKEN")),
        DB:        os.Getenv("DATABASE_URL"),
    })

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    if err := ship.Run(ctx); err != nil {
        log.Fatal(err)
    }
}
```

This gives you a fully functional Telegram bot backed by Claude. It handles sessions, compaction, and the built-in `current_time` and `web_fetch` tools automatically.

---

## Configuration

Only three fields are required. Everything else has sensible defaults.

```go
type Config struct {
    // Required
    LLM       CompletionProvider  // LLM backend
    Transport TransportConfig     // messaging transport
    DB        string              // PostgreSQL DSN (app database)

    // Optional: separate DB for BlueShip's own tables
    // Defaults to DB if empty.
    ShipDB string

    // Optional providers (nil = feature disabled)
    Embedder    EmbeddingProvider     // vector embeddings
    Search      SearchEngine          // enables web_search tool
    Fetcher     WebFetcher            // custom web fetch (auto-created if nil)
    Calendar    CalendarProvider
    Transcriber TranscriptionProvider // enables voice → text
    Sender      MessageSender         // enables proactive messaging

    // Infrastructure
    Redis    string // Redis address, "" = no cache
    Prompts  string // path to .md workspace directory
    Timezone string // default "UTC"

    // Fine-tuning
    Models   ModelsConfig
    Limits   LimitsConfig
    Timeouts TimeoutsConfig
    Retry    RetryConfig
    Gateway  GatewayConfig
}
```

### GatewayConfig defaults

| Field | Default | Description |
|---|---|---|
| `DebounceWindow` | `1500ms` | Wait this long to batch rapid messages |
| `DebounceCap` | `10` | Fire immediately after N buffered messages |
| `SessionResetHour` | `4` | Hour (local time) to start a new daily session |
| `MaxTurns` | `15` | Max tool-call iterations per invocation |
| `DisableThinking` | `false` | Set `true` to disable the autonomous thinking job |

### LimitsConfig defaults

| Field | Default | Description |
|---|---|---|
| `MaxContext` | `180000` | Total token budget for context window |
| `CompactThreshold` | `40000` | Trigger compaction above this many tokens |
| `CompactKeep` | `30000` | Keep this many recent tokens uncompacted |
| `MaxOutputTokens` | `8192` | Max tokens per LLM response |
| `CompactOutput` | `2048` | Max tokens for compaction summary |
| `ThinkingBudget` | `0` | Extended thinking budget (0 = disabled) |

### ModelsConfig

```go
ship := blueship.New(blueship.Config{
    Models: blueship.ModelsConfig{
        Primary: blueship.ModelRef{Provider: "anthropic", Name: "claude-opus-4-6"},
        Compact: blueship.ModelRef{Provider: "anthropic", Name: "claude-haiku-4-5-20251001"},
    },
    // ...
})
```

`Primary` is used for the main agent loop. `Compact` is used for conversation summarization. Both default to `claude-haiku-4-5-20251001`.

---

## Providers

### Completion (LLM)

```go
// Anthropic Claude (standard API key)
blueship.Anthropic(apiKey)

// Anthropic with custom timeout and retry backoffs
blueship.AnthropicWithConfig(apiKey, 180*time.Second, []time.Duration{5*time.Second, 15*time.Second, 45*time.Second})

// OpenAI Chat Completions
blueship.OpenAI(apiKey)
blueship.OpenAIWithConfig(apiKey, 120*time.Second)

// Google Gemini
blueship.Gemini(apiKey)
blueship.GeminiWithConfig(apiKey, 120*time.Second)
```

**Anthropic notes:**
- Supports both standard API keys (`sk-ant-*`) and OAuth Bearer tokens (`sk-ant-oat01-*`)
- OAuth tokens require the `anthropic-beta: oauth-2025-04-20` header — handled automatically
- Retries on `rate_limit` and `overloaded` errors with configurable backoff
- Extended thinking enabled when `LimitsConfig.ThinkingBudget > 0`

### Embeddings

```go
// OpenAI text-embedding-3-small (default model)
blueship.OpenAIEmbedding(apiKey)

// Custom model and timeout
blueship.OpenAIEmbeddingWithModel(apiKey, "text-embedding-3-large", 30*time.Second)
```

### Voice Transcription

```go
// OpenAI Whisper
blueship.Whisper(apiKey)
blueship.WhisperWithModel(apiKey, "whisper-1", 30*time.Second)
```

When configured, voice messages sent to the Telegram bot are automatically transcribed before being passed to the agent loop.

### Web Search

```go
// Serper.dev (Google Search)
blueship.Serper(apiKey)
```

When configured, the `web_search` tool is automatically registered for all users.

### Transport

```go
// Telegram transport (required for bots)
blueship.Telegram(botToken)

// Proactive message sender (for heartbeats, notifications)
blueship.TelegramSender(botToken, 10*time.Second)
```

---

## Writing Modules

A module is any type implementing `Name() string`. It can additionally opt into three capability interfaces.

### ToolProvider — expose LLM tools

```go
type WeatherModule struct{}

func (m *WeatherModule) Name() string { return "weather" }

func (m *WeatherModule) RegisterTools(r *blueship.ToolRegistry, d *blueship.Deps) {
    r.Register(
        "get_weather",
        "Returns current weather for a given city.",
        json.RawMessage(`{
            "type": "object",
            "properties": {
                "city": {"type": "string", "description": "City name"}
            },
            "required": ["city"]
        }`),
        func(ctx context.Context, input json.RawMessage) (any, error) {
            var p struct {
                City string `json:"city"`
            }
            if err := json.Unmarshal(input, &p); err != nil {
                return nil, err
            }
            // Call your weather API here
            return map[string]any{"city": p.City, "temp": "22°C", "condition": "sunny"}, nil
        },
    )
}
```

### JobProvider — background jobs

```go
func (m *WeatherModule) Jobs(d *blueship.Deps) []blueship.Job {
    return []blueship.Job{
        {
            Name:     "weather-cache-refresh",
            Interval: 15 * time.Minute,
            Run: func(ctx context.Context) error {
                // Refresh cached weather data
                return nil
            },
        },
    }
}
```

Jobs run in a panic-safe loop via `scheduler.RunLoop`. Panics are caught and logged; the job is restarted on the next tick.

### CLIProvider — admin commands

```go
func (m *WeatherModule) RegisterCLI(cmd *cobra.Command, d *blueship.Deps) {
    weatherCmd := &cobra.Command{
        Use:   "weather",
        Short: "Weather module commands",
    }
    weatherCmd.AddCommand(&cobra.Command{
        Use:   "status",
        Short: "Print cache status",
        Run: func(cmd *cobra.Command, args []string) {
            blueship.OK(map[string]any{"cache_entries": 42})
        },
    })
    cmd.AddCommand(weatherCmd)
}
```

`blueship.OK(data)` writes `{"success":true,"data":...}` to stdout. `blueship.Fail(msg)` writes `{"success":false,"error":...}` and exits 1.

### Registering modules

```go
ship := blueship.New(cfg)
ship.RegisterModule(&WeatherModule{})
ship.RegisterModule(&CalendarModule{})
ship.Run(ctx)
```

---

## Deps — dependency injection

`Deps` is passed to all module methods. It carries everything the module needs:

```go
type Deps struct {
    Config  *Config
    Logger  *slog.Logger
    Redis   *redis.Client   // nil if Redis not configured
    UserID  uuid.UUID       // resolved per-invocation
    ChatID  string          // e.g. "telegram:123456789"
    IsOwner bool

    Embedder EmbeddingProvider  // nil if not configured
    LLM      CompletionProvider
    Sender   MessageSender      // nil if not configured
}

// DB returns a connection pool for the named database (lazy, cached).
func (d *Deps) DB(module string) (*sqlx.DB, error)
```

### Database routing

`DB(module)` derives the connection from the base DSN:

| Call | Connects to |
|---|---|
| `d.DB("core")` | Base DSN as-is (app's main database) |
| `d.DB("ship")` | BlueShip's own database (`ShipDB` config) |
| `d.DB("tasks")` | Base DSN with `_tasks` appended to the database name |
| `d.DB("finance")` | Base DSN with `_finance` appended |

Example: if `DB = "postgres://user:pass@host/myapp"`, then `d.DB("tasks")` connects to `myapp_tasks`.

---

## Prompt Files (Workspace)

Set `Config.Prompts` to a directory path. BlueShip reads the following files on startup:

| File | Used as |
|---|---|
| `PREAMBLE.md` | Prefix for all system prompts |
| `SOUL.md` | Agent persona and values |
| `AGENTS.md` | Operational instructions, tool usage guidelines |
| `HEARTBEAT.md` | System prompt for the 30-min heartbeat job |
| `THINKING.md` | System prompt for the 60-min thinking job (optional) |
| `prompts/compact.md` | System prompt for the compaction summarizer (optional) |

The main system prompt is: `PREAMBLE + SOUL + AGENTS`.
The heartbeat prompt is: `PREAMBLE + SOUL + AGENTS + HEARTBEAT`.

---

## Background Jobs

### Heartbeat (built-in, every 30 min)

The heartbeat job runs through the agent loop for every known user using `HEARTBEAT.md` as the system prompt. A reply is sent to the user only if:

- It is non-empty
- It is longer than 10 characters
- It does not contain `[no-op]`

The job is skipped entirely between 00:00 and 07:59 (local time).

Use `[no-op]` in your `HEARTBEAT.md` instructions to signal that the agent should remain silent when there is nothing actionable to report.

### Thinking (built-in, every 60 min)

An autonomous thinking job runs for the owner user only, using `THINKING.md`. It is cancelled immediately when the user sends a message, so it never blocks responsiveness. Disable it with `Gateway.DisableThinking = true`.

### Custom jobs

Any module implementing `JobProvider` can add jobs. They run in separate goroutines managed by `scheduler.RunLoop`, which catches panics, logs them, and restarts on the next interval.

---

## Migrations

BlueShip applies its own migrations automatically on `Run()`. You do not need to run anything manually for BlueShip's tables.

### BlueShip's own tables

| Table | Purpose |
|---|---|
| `user_profiles` | Chat ID, display name, trust level, owner flag, bio (JSONB), preferences (JSONB) |
| `chat_sessions` | Per-user sessions with daily reset |
| `chat_messages` | Session messages stored as JSONB `[]ContentBlock` |
| `blueship_migrations` | Migration tracking |

### For your application tables

Manage your own migrations separately (e.g., `golang-migrate`, raw SQL scripts, or your own tooling). Access your databases via `d.DB("core")`, `d.DB("tasks")`, etc.

---

## Telegram Features

The built-in Telegram transport handles:

| Input type | Behavior |
|---|---|
| Text messages | Passed directly to agent loop |
| Voice messages | Transcribed via Whisper, then passed as text |
| Photos | Downloaded, base64-encoded, passed as image content blocks |
| Text files | Downloaded (up to 512 KB), inlined as fenced code blocks |
| Reply context | Quoted as `[reply to: ...]` prefix |
| `/session` | Built-in command showing session stats, token usage, model |

**Debouncing:** rapid messages within the configured window (default 1.5s) are batched into a single agent loop invocation. This prevents wasted LLM calls when users send multiple messages in quick succession.

**Long messages:** responses exceeding Telegram's 4096-character limit are automatically split.

---

## Full Example

A complete agent with a custom todo module:

```go
package main

import (
    "context"
    "encoding/json"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/rasimio/blueship"
    "github.com/spf13/cobra"
)

// TodoModule stores and retrieves todos from the database.
type TodoModule struct{}

func (m *TodoModule) Name() string { return "todo" }

func (m *TodoModule) RegisterTools(r *blueship.ToolRegistry, d *blueship.Deps) {
    r.Register(
        "add_todo",
        "Add a todo item for the current user.",
        json.RawMessage(`{
            "type": "object",
            "properties": {
                "text": {"type": "string", "description": "The todo item text"}
            },
            "required": ["text"]
        }`),
        func(ctx context.Context, input json.RawMessage) (any, error) {
            var p struct {
                Text string `json:"text"`
            }
            if err := json.Unmarshal(input, &p); err != nil {
                return nil, err
            }
            db, err := d.DB("core")
            if err != nil {
                return nil, err
            }
            _, err = db.ExecContext(ctx,
                `INSERT INTO todos (user_id, text) VALUES ($1, $2)`,
                d.UserID, p.Text,
            )
            if err != nil {
                return nil, err
            }
            return map[string]any{"added": p.Text}, nil
        },
    )

    r.Register(
        "list_todos",
        "List all pending todo items for the current user.",
        json.RawMessage(`{"type":"object","properties":{}}`),
        func(ctx context.Context, input json.RawMessage) (any, error) {
            db, err := d.DB("core")
            if err != nil {
                return nil, err
            }
            var todos []string
            err = db.SelectContext(ctx, &todos,
                `SELECT text FROM todos WHERE user_id = $1 AND done = false ORDER BY created_at`,
                d.UserID,
            )
            return todos, err
        },
    )
}

func (m *TodoModule) Jobs(d *blueship.Deps) []blueship.Job {
    return []blueship.Job{
        {
            Name:     "todo-reminder",
            Interval: 1 * time.Hour,
            Run: func(ctx context.Context) error {
                // Check for overdue todos and notify via d.Sender
                return nil
            },
        },
    }
}

func (m *TodoModule) RegisterCLI(cmd *cobra.Command, d *blueship.Deps) {
    cmd.AddCommand(&cobra.Command{
        Use:   "todo list",
        Short: "List todos from CLI",
        Run: func(cmd *cobra.Command, args []string) {
            db, err := d.DB("core")
            if err != nil {
                blueship.Fail(err.Error())
            }
            var todos []string
            if err := db.Select(&todos, `SELECT text FROM todos WHERE done = false`); err != nil {
                blueship.Fail(err.Error())
            }
            blueship.OK(todos)
        },
    })
}

func main() {
    ship := blueship.New(blueship.Config{
        LLM:       blueship.Anthropic(os.Getenv("ANTHROPIC_API_KEY")),
        Transport: blueship.Telegram(os.Getenv("TELEGRAM_BOT_TOKEN")),
        DB:        os.Getenv("DATABASE_URL"),
        ShipDB:    "blueship",

        Embedder:    blueship.OpenAIEmbedding(os.Getenv("OPENAI_API_KEY")),
        Search:      blueship.Serper(os.Getenv("SERPER_API_KEY")),
        Transcriber: blueship.Whisper(os.Getenv("OPENAI_API_KEY")),
        Sender:      blueship.TelegramSender(os.Getenv("TELEGRAM_BOT_TOKEN"), 10*time.Second),

        Prompts:  "/app/workspace",
        Timezone: "America/New_York",

        Models: blueship.ModelsConfig{
            Primary: blueship.ModelRef{Provider: "anthropic", Name: "claude-opus-4-6"},
            Compact: blueship.ModelRef{Provider: "anthropic", Name: "claude-haiku-4-5-20251001"},
        },
        Limits: blueship.LimitsConfig{
            ThinkingBudget: 10000,
        },
        Gateway: blueship.GatewayConfig{
            SessionResetHour: 5,
            MaxTurns:         20,
        },
    })

    ship.RegisterModule(&TodoModule{})

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    if err := ship.Run(ctx); err != nil {
        log.Fatal(err)
    }
}
```

---

## Startup Sequence

When `ship.Run(ctx)` is called:

1. `InitDeps` — connects to Redis (non-fatal if unavailable), creates the DB connection pool
2. `migrate.Run` — applies any pending embedded SQL migrations to the BlueShip database
3. `user.ResolveOwner` — creates or resolves the owner user record
4. Module `JobProvider.Jobs()` — starts each module's background jobs in goroutines
5. `gateway.NewGateway` — loads prompt files, creates Telegram poller and client
6. `HeartbeatJob` — starts on a 30-minute scheduler
7. `ThinkingJob` — starts on a 60-minute scheduler (unless `DisableThinking = true`)
8. Block on `ctx.Done()`; graceful shutdown waits for all goroutines

---

## Provider Interfaces

Implement these interfaces to plug in custom providers:

```go
// LLM backend
type CompletionProvider interface {
    Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
}

// Vector embeddings
type EmbeddingProvider interface {
    Embed(ctx context.Context, text string) ([]float32, error)
}

// Web search
type SearchEngine interface {
    Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
}

// Web page fetching
type WebFetcher interface {
    Fetch(ctx context.Context, url string, maxChars int) (string, error)
}

// Voice-to-text
type TranscriptionProvider interface {
    Transcribe(ctx context.Context, audio []byte, filename string) (string, error)
}

// Proactive messaging (outbound notifications)
type MessageSender interface {
    SendMessage(ctx context.Context, chatID string, text string) (messageID int, err error)
}

// Calendar integration
type CalendarProvider interface {
    GetEvents(ctx context.Context, start, end time.Time) ([]CalendarEvent, error)
    CreateEvent(ctx context.Context, event CalendarEvent) (CalendarEvent, error)
    DeleteEvent(ctx context.Context, eventID string) error
}
```

---

## Roadmap

See [TODO.md](TODO.md) for the current improvement backlog (tests, refactors, CI, docs).

---

## License

MIT
