# BlueShip — Improvement Backlog

A categorized checklist of audit findings. Items are improvement candidates, not all blockers — pick by impact and current priorities. Fold completed work into git history rather than ticking and keeping it here forever.

## Tests

- [ ] Add tests for `core/` (provider contracts, config parsing, errors).
- [ ] Add tests for `agent/loop.go` (compaction trigger, tool dispatch, cancellation).
- [ ] Add tests for `handler/` (`plan_executor.go`, `delegate_executor.go`).
- [ ] Add tests for `internal/gateway/` (debounce, user resolution, tool registration).
- [ ] Add tests for `internal/scheduler/` (panic-safety, heartbeat cadence).
- [ ] Add tests for `a2a/` (server, client, dispatcher, store).
- [ ] Add tests for `session/`, `tool/`, `migrate/`, `internal/openai/`.
- [ ] Wire `go test ./...` (with `-race` and coverage) into CI.

## Refactor / Architecture

- [ ] Split `ship.go` (~1180 lines, 28 receiver methods) — extract Fleet bootstrap, A2A bootstrap, delegate-callback firing, scheduler init into focused files.
- [ ] Split `internal/gateway/gateway.go` (~2100 LOC) — separate transport polling, user resolution, tool registration, and dispatch.
- [ ] Split `handler/plan_executor.go` (~770 LOC, 14 functions) — separate plan parsing, step execution, and result emission.
- [ ] Audit `_ = ...` ignored-error sites in `a2a/`, `handler/`, dispatcher — decide per-site: log, return, or assert. The pattern currently masks state-mutation failures.

## Documentation

- [ ] Add `// Package …` doc comments to public packages missing them (agent, core, tool, session, handler, internal/gateway, internal/scheduler, migrate, a2a/dispatcher, …).
- [ ] Add a top-of-file godoc to `ship.go` describing the orchestration entry point.
- [ ] Document the Fleet federation and A2A subsystems (purpose, lifecycle, message flow) — currently only inferable from code.
- [ ] Consider an `docs/adr/` directory for design decisions (federation model, delegate callback protocol, compaction strategy).

## CI / Tooling

- [ ] Extend `.github/workflows/` beyond `agent-isolation.yml`: run `go test ./... -race`, `go vet`, `golangci-lint`, and report coverage.
- [ ] Add `.golangci.yml` with a baseline ruleset (errcheck, govet, staticcheck, gofmt, goimports, ineffassign, unused).
- [ ] Add a `Makefile` (or `justfile`) with `test`, `lint`, `build`, `migrate` targets to standardize local workflow.
- [ ] Add a pre-commit hook (or document one) running `gofmt`/`goimports` and `go vet`.

## Dependencies

- [ ] Periodically run `go list -m -u all` and apply security-relevant updates; pin a cadence.
- [ ] Re-evaluate `lib/pq` (low activity) vs. `pgx` for the Postgres driver.
- [ ] Add a test-helper dependency (e.g., `testify` or `go-cmp`) once test coverage grows.

## Error Handling

- [ ] Replace silent `_ = json.Marshal(...)` / `_ = emit.EmitTerminal(...)` with structured logging at minimum.
- [ ] Standardize error wrapping (`fmt.Errorf("…: %w", err)`) and consider `errors.Is/As` checks where call sites currently compare strings.
- [ ] Audit goroutines (scheduler, A2A, gateway) for panic propagation — confirm the panic-safe wrapper is applied uniformly.

## Security

- [ ] Confirm OAuth client secret in `internal/fleet/client.go` is never logged at info level (verify log levels in production config).
- [ ] Add a basic `gosec` run to CI to catch obvious smells early.
- [ ] Document the secret-injection contract (env vars only, no fallback to files) in the README config section.

## Misc

- [ ] Resolve the lone `TODO` in `internal/agenttask/scheduler.go:313` (cron expression support) or convert to a tracked issue.
- [ ] Consider a short `CONTRIBUTING.md` covering branch naming, test expectations, and the agent-isolation lint rule.
- [ ] Add a `CHANGELOG.md` once the project tags releases.
