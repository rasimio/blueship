# BlueShip — Improvement Backlog

A categorized checklist of improvement candidates — not all blockers; pick by impact and current priorities. Fold completed work into git history rather than ticking and keeping it here forever.

## Tests

- [ ] Add tests for `internal/core/` (provider contracts, config parsing, errors).
- [ ] Add tests for `runtime/agent/` (compaction trigger, tool dispatch, cancellation, streaming).
- [ ] Add tests for `runtime/handler/` (`plan_executor.go`, `delegate_executor.go`).
- [ ] Add tests for `internal/gateway/` (debounce, user resolution, tool registration).
- [ ] Add tests for `internal/looprunner/` (panic-safety, loop cadence).
- [ ] Add tests for `internal/federation/a2a/` (server, client, dispatcher, store).
- [ ] Add tests for `runtime/session/`, `tool/`, `internal/migrate/`, `internal/provider/openai/`.
- [ ] Wire `go test ./...` (with `-race` and coverage) into CI.

## Refactor / Architecture

- [ ] Apply interface-first to the last concrete stores: `Deps.ModelStore`/`RoleTools` hold concrete types while `Users`/`Prompts`/`Sessions` are interfaces — extract `ModelConfigQuerier`/`RoleToolQuerier`.
- [ ] Lift framework-owned user-facing strings (refusal/interrupt placeholders, onboarding copy) and the TTS `language_code` out of the framework into host-supplied config — text is a tenant concern.
- [ ] (Decision) Genericize the `vaelum.*` platform schema behind host-supplied interfaces (schema/prompt/tool-catalog seams) — the largest lever toward a truly tenant-agnostic framework, but it touches prod read-paths. Phased + shadowed.
- [ ] Audit `_ = ...` ignored-error sites in `internal/federation/a2a/`, `runtime/handler/`, dispatcher — decide per-site: log, return, or assert.

## Documentation

- [ ] Document the Fleet federation and A2A subsystems (purpose, lifecycle, message flow) in `docs/`.
- [ ] Add a `docs/adr/` directory for design decisions (federation model, delegate-callback protocol, compaction strategy). `docs/ARCHITECTURE.md` is in place.

## CI / Tooling

- [ ] Extend CI beyond `agent-isolation.yml`: run `go test ./... -race`, `golangci-lint`, and report coverage.
- [ ] Add `.golangci.yml` with a baseline ruleset (errcheck, govet, staticcheck, gofmt, goimports, ineffassign, unused).
- [ ] Add a `Makefile` (or `justfile`) with `test`, `lint`, `build`, `migrate` targets.
- [ ] Add a pre-commit hook (or document one) running `gofmt`/`goimports` and `go vet`.

## Dependencies

- [ ] Periodically run `go list -m -u all` and apply security-relevant updates; pin a cadence.
- [ ] Re-evaluate `lib/pq` (low activity) vs. `pgx` for the Postgres driver.
- [ ] Add a test-helper dependency (e.g., `testify` or `go-cmp`) once test coverage grows.

## Error Handling

- [ ] Replace silent `_ = json.Marshal(...)` / `_ = emit.EmitTerminal(...)` with structured logging at minimum.
- [ ] Standardize error wrapping (`fmt.Errorf("…: %w", err)`) and use `errors.Is/As` where call sites currently compare strings.
- [ ] Audit goroutines (scheduler, A2A, gateway) for panic propagation — confirm the panic-safe wrapper is applied uniformly.

## Security

- [ ] Confirm the OAuth client secret in `internal/federation/fleet/client.go` is never logged at info level.
- [ ] Add a basic `gosec` run to CI.
- [ ] Document the secret-injection contract (env vars only, no fallback to files) in the README config section.

## Misc

- [ ] Resolve the lone `TODO` in `internal/agenttask/scheduler.go` (cron expression support) or convert to a tracked issue.
- [ ] Add a short `CONTRIBUTING.md` covering branch naming, test expectations, and the agent-isolation lint rule.
- [ ] Add a `CHANGELOG.md` once the project tags releases.
