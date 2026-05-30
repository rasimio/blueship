# BlueShip Architecture

BlueShip is a framework for building production AI agents. A host application
configures a `Ship`, registers feature modules, and calls `Run`; the framework
owns transport, the reflex/cortex turn pipeline, providers, memory plumbing,
agent-task scheduling, and federation.

This document maps the codebase so a newcomer can navigate it by layer.

## The mental model: S0 / S1 / S2

Every inbound message flows through three layers:

| Layer | Name | Responsibility | Where it lives |
|-------|------|----------------|----------------|
| **S0** | Transport | How a user reaches a ship (Telegram, WebSocket voice, HTTP/SSE). Bytes in, bytes out. | `internal/transport/` |
| **S1** | Reflex | Fast tier: classify intent, match behavioural rules, optionally answer or pre-fetch before the heavy turn. | `internal/gateway` (reflex pipeline) + `internal/core` reflex types |
| **S2** | Cortex | The full agent turn: build context, run the LLM tool loop, persist, deliver. | `internal/gateway` (process pipeline) + `agent/` (the loop) |

Providers (LLM, embeddings, search, voice) sit beneath all three. Federation
(A2A + Fleet) lets one ship call another's tools as if local.

## Public surface (what a host imports)

Everything a host needs is reachable from the **root `blueship` package** — the
canonical types live in `internal/core` and are re-exported through
`facade.go`, so the whole API is `blueship.X` with no second package to learn.

```
blueship                      root: Ship, New, Run, Register*, provider
                              constructors (Anthropic/OpenAI/…), and the
                              facade re-exporting all core types as blueship.X
├── module.go                 Module / ToolProvider / JobProvider / CLIProvider
├── facade.go                 public re-exports over internal/core (by layer)
│
├── agent/                    the LLM tool loop (S2 execution)
├── handler/                  background + plan-executor agent-task handlers
├── session/                  chat session + message store
├── tool/                     built-in tools + registration helpers
├── attachment/               attachment value types + URL helpers
├── mcp/                      Model Context Protocol client
├── tts/                      text-to-speech helpers
├── pdf/                      markdown → PDF rendering
├── telemetry/                tracing + alerting
└── version/                  build stamp
```

## Internal structure (Go-enforced private)

`internal/` has no external importers; it is the framework's machine room,
organized to mirror the S0/S1/S2 model.

```
internal/
├── core/            canonical types behind the facade: Config, Deps (DI),
│                    Message, providers/ports, reflex/agent DTOs, stores,
│                    request-context helpers. Split by concern:
│                      config.go / config_transport.go / config_gateway.go
│                      deps.go / reqctx.go / cliout.go / dbpool (in deps)
│                      llm + capabilities (provider.go) / router
│                      transport.go / reflex.go / agent.go / registry.go
│                      *_store.go (model/prompt/role/session/user/agent)
│
├── transport/       S0 — how users reach a ship
│   ├── telegram/    Telegram Bot API client + multi-bot polling
│   ├── ws/          WebSocket server (voice / desktop)
│   └── httpchat/    HTTP + SSE chat server (web platform)
│
├── gateway/         S1+S2 — the conversation runtime (split by concern):
│                      gateway.go            struct, NewGateway, prompts,
│                                            handleUpdate, user-state
│                      gateway_process.go    ProcessInbound + the cortex turn
│                      gateway_reflex.go     reflex pipeline + interaction tier
│                      gateway_voice.go      voice synthesis + telegramSink
│                      gateway_attachments.go attachment + link handling
│                      gateway_helpers.go    post-actions, sessions, debouncer
│
├── provider/        the model layer (one home for every provider)
│   ├── anthropic/   Messages API (+ OAuth via anthropicoauth)
│   ├── openai/      completion + embedding + transcription
│   ├── gemini/  ollama/  openaicodex/
│   └── anthropicoauth/   subscription-billed OAuth token store
│
├── agenttask/       autonomous agent-task scheduler + dispatch
│   ├── scheduler.go         lifecycle, strategy dispatch
│   ├── evaluator.go         generic acceptance-criteria checking
│   └── grounding.go         research citation auditor (Gate C/D)
│
├── federation/      ship-to-ship
│   ├── a2a/         Agent-to-Agent protocol (client / server / store)
│   └── fleet/       BlueFleet directory integration
│
├── webaccess/       web access, two mechanisms
│   ├── web/         raw HTTP fetch + Serper search
│   └── browser/     headless Chrome (JS-heavy pages, PDF capture)
│
├── store/user/      user-profile DB helpers
├── looprunner/      periodic job loop runner (RunLoop / RunLoopWithTrigger)
├── toolcatalog/     publishes the native tool catalog for host cabinets
└── migrate/         embedded SQL auto-migration for framework tables
```

## Startup sequence

`Ship.Run` (see `run.go`) wires the layers in order:

1. **`InitDeps`** — DB pool, Redis, providers (`internal/core`).
2. **`migrate.Run`** — auto-apply embedded framework SQL.
3. **Stores + owner** — model/prompt/role/user/session stores; resolve owner.
4. **A2A + Fleet** — optional federation (`internal/federation`, see `a2a.go` / `fleet.go`).
5. **Agent-task scheduler** — if handlers/strategies are registered (`internal/agenttask`).
6. **Gateway** — the inbound router for every transport (`internal/gateway`).
7. **Transports** — Telegram fan-in, WebSocket, HTTP/SSE (`internal/transport`).

## Root files (package `blueship`)

The root package is intentionally split into small files by responsibility:

| File | Holds |
|------|-------|
| `ship.go` | `Ship` struct, `New`, `Register*`, `fleetAuth` |
| `run.go` | the `Run` boot sequence |
| `registry.go` | module registry + remote-tool fan-out |
| `providers.go` | convenience constructors (`Anthropic`, `OpenAI`, `Serper`, …) |
| `fleet.go` | Fleet integration plumbing |
| `a2a.go` | A2A startup, adapters, delegate callback |
| `metrics.go` | Prometheus ship metrics |
| `facade.go` | public re-exports over `internal/core` |
| `module.go` | the `Module` contracts |

## Why core is internal

`internal/core` holds the canonical types, but importing applications never
import it directly — Go's `internal/` rule forbids it. Instead `facade.go`
re-exports everything as `blueship.X`. This keeps the public surface to a
single package while letting the framework refactor core freely behind the
facade.
