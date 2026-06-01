# Getting Started

This walks you from zero to a running agent, then to one with its own
personality and tools. It takes about 5 minutes for the first bot.

## What you'll build

A Telegram bot backed by Claude that remembers conversations, compacts long
context automatically, and can use tools. BlueShip handles the runtime; you
bring an API key, a bot token, and (optionally) a personality and tools.

## 1. Run the example agent

**Prerequisites:** Go 1.26+, Docker (for Postgres), an
[Anthropic API key](https://console.anthropic.com/), and a Telegram bot token
from [@BotFather](https://t.me/BotFather) (`/newbot`, then copy the token).

```bash
git clone https://github.com/rasimio/blueship && cd blueship
make setup     # starts Postgres in Docker, fetches deps, creates .env from .env.example
```

Open `.env` and paste your two secrets:

```dotenv
ANTHROPIC_API_KEY=sk-ant-...
TELEGRAM_BOT_TOKEN=123456789:ABCdef...
# DATABASE_URL already points at the Docker Postgres make setup started.
```

Then:

```bash
make run
```

You'll see `telegram gateway started`. Message your bot — it replies as Claude.
BlueShip auto-created its tables in a dedicated `blueship` schema on first run;
there is no manual migration step. Stop with Ctrl-C; `make down` stops Postgres.

> No Docker? Point `DATABASE_URL` in `.env` at any Postgres 14+ you control —
> BlueShip migrates its own tables on start. Everything else is the same.

## 2. Give your agent a personality

By default the agent runs with an empty system prompt (it warns you). To give
it character, point `Config.Prompts` at a directory of `<key>.md` files. The
keys come from `Config.SystemPromptKeys` (default `preamble`, `soul`, `agents`),
concatenated into the system prompt.

The simplest setup is a single `soul` file. In your own `main.go`:

```go
ship := blueship.New(blueship.Config{
    LLM:              blueship.Anthropic(os.Getenv("ANTHROPIC_API_KEY")),
    Transport:        blueship.Telegram(os.Getenv("TELEGRAM_BOT_TOKEN")),
    DB:               os.Getenv("DATABASE_URL"),
    Prompts:          "prompts",                 // directory of <key>.md files
    SystemPromptKeys: []string{"soul"},          // just one layer, for now
})
```

Create `prompts/soul.md`:

```markdown
You are Aria, a concise and friendly assistant. You answer in plain language,
admit when you don't know something, and keep replies short unless asked for
detail.
```

Run again — your bot now has a personality. (Multi-layer setups split shared
platform text into `preamble.md` / `agents.md` and per-persona text into
`soul.md`; see the README's *Prompt Files* section.)

## 3. Add a tool

Tools are how the agent *does* things. A tool is a name + JSON-schema + handler,
grouped into a **module**. Here's a complete weather tool:

```go
package main

import (
    "context"
    "encoding/json"

    "github.com/rasimio/blueship"
)

type weatherModule struct{}

func (weatherModule) Name() string { return "weather" }

func (weatherModule) RegisterTools(r *blueship.ToolRegistry, d *blueship.Deps) {
    r.Register(
        "get_weather",
        "Get the current weather for a city.",
        json.RawMessage(`{
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"]
        }`),
        func(ctx context.Context, input json.RawMessage) (any, error) {
            var args struct{ City string `json:"city"` }
            if err := json.Unmarshal(input, &args); err != nil {
                return nil, err
            }
            // ... call a real weather API here ...
            return map[string]any{"city": args.City, "tempC": 21, "sky": "clear"}, nil
        },
    )
}
```

Register it before `Run`:

```go
ship.RegisterModule(weatherModule{})
```

Now ask your bot "what's the weather in Tokyo?" and it will call `get_weather`.
Modules can also provide background jobs (`JobProvider`) and CLI commands
(`CLIProvider`) — see [Writing Modules](../README.md#writing-modules).

## Where to go next

- [README](../README.md) — full configuration, providers (OpenAI/Gemini/local),
  background jobs, migrations, the complete API.
- [docs/ARCHITECTURE.md](ARCHITECTURE.md) — how the pieces fit (the S0/S1/S2
  transport → reflex → cortex model, providers, federation).
- [`examples/minimal`](../examples/minimal/main.go) — the smallest runnable agent.
