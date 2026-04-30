# next.md

Instructions for the session that starts implementing calendar-sync. Delete this file once it's been read and acted on.

## Read order before writing any code

1. `SPEC.md` end-to-end. Every command, every flag, every error code, the daemon lifecycle, the drift detection model, the recurring-event handler, the mirror identification scheme, the deterministic-ID design. Don't skim. The spec went through seven Codex review rounds and is the source of truth.
2. `CLAUDE.md` for the workflow rules. Note especially the mandatory-code-review-before-push rule (twice: once on the change, once after addressing feedback). Don't skip it.
3. `doc/plans/requirements-questions.md` for the round-1 decisions and the rationale behind them.
4. `doc/measurements/2026-04-30-event-list-timing.md` for the cost analysis that drove the long-running daemon architecture. Re-run `scripts/measure-list-cost.py` if you want to validate.
5. `/Users/tammersaleh/src/github.com/tammersaleh/slack-cli` is the adjacent reference project. Patterns to reuse listed below.

## Suggested implementation order

Bottom-up. Each layer's tests pass before moving to the next.

1. **Pure-logic primitives.** Deterministic mirror ID derivation, the canonical-field hash for the checksum, description-trailer strip regex, scope-string formatting helpers. These are pure functions. Tests are trivial. Get them right early - the rest of the codebase depends on them.
2. **`internal/gws` subprocess wrapper.** Wraps `gws calendar ...` invocations. One method per Calendar API call we use (`events.list`, `events.get`, `events.insert`, `events.patch`, `events.delete`, `events.instances`, `calendarList.get`). Each method takes typed parameters, marshals to the gws `--params`/`--json` form, runs the subprocess, parses the response. Error mapping (per SPEC's "gws subprocess error mapping" table). This is the only layer that knows the gws CLI exists.
3. **`internal/config`.** TOML loader, validation rules from the spec's "Validation rules" section, calendar-ID canonicalization (calls into `internal/gws` for `calendarList.get`), accessRole population, pdir expansion.
4. **`internal/mirror`.** Mirror payload construction, extended-property layout, the field-set used for the canonical-field hash, the drift-handling decision matrix from the spec.
5. **`internal/recurring`.** Recurring-instance handler, originalStart `dateTime`-vs-`date` branching, the four-way matrix per instance, the parent-recurrence-changes path.
6. **`internal/sync`.** Classification logic. Takes a source event, a pdir, and the in-memory mirror inventory; returns the action and the API operation to perform. Pure-ish (mocks `internal/gws` for the few API lookups it needs - `events.instances` for horizon checks, `events.get` for parent fetches).
7. **`internal/daemon`.** Lifecycle (cold start, per-tick scheduler, periodic full re-sync), in-memory inventory management, syncToken-per-source with conditional advancement, signal handling (SIGTERM clean exit), the IPC socket server.
8. **`internal/launchd`.** Plist generation, launchctl wrappers.
9. **`cmd/`.** kong CLI struct, all the subcommands. Each subcommand is thin: it parses flags, calls into the internal packages, formats output. The output formatting is centralized in `internal/output` (see "Reuse" below).

## TDD and mocking plan

### Test boundary

The single mockable boundary is the `gws` subprocess. calendar-sync never talks HTTP directly, so there's no httptest server. There's no Calendar Go SDK in scope, so there's no library-level mock either. Every test that exercises a Calendar API call drives a fake `gws` binary.

This is symmetric to slack-cli's pattern, but at the subprocess boundary instead of `httptest.Server`.

### Fake gws binary

Build a small Go program at `go test` setup time. The fake:

- Reads its argv to identify which Calendar API call is requested.
- Looks up the canned response from a per-test scenario (passed in via env var, file, or a side-channel socket).
- Writes JSON to stdout, exits with the configured code.

Two viable shapes for the side-channel:

- **File-based**: each test writes a scenarios JSON to a temp dir. The fake reads `$CALENDAR_SYNC_FAKE_GWS_SCENARIO=<path>` to find it. Simple, debuggable.
- **Socket-based**: the test runs a tiny in-process HTTP/UDS server; the fake makes a request to ask "what should I return for these args?". More flexible (lets the test set assertions on the wire) but more moving parts.

Go with file-based first. Move to socket-based if a test class genuinely needs request-time decision-making (e.g., simulating per-page latency).

### Test helper

`internal/testhelpers.RunWithFakeGWS(t, scenario, args...)`:

1. Build the fake gws binary into a temp dir (cached across tests via `sync.Once`).
2. Prepend the temp dir to `PATH`.
3. Write the scenario JSON to a temp file; set `$CALENDAR_SYNC_FAKE_GWS_SCENARIO` to its path.
4. `isolateTestEnv(t)` to clear any leaking env vars (`CALENDAR_SYNC_*`, `XDG_CONFIG_HOME`, `TMPDIR`).
5. Set up a temp `XDG_CONFIG_HOME` so config and IPC socket are isolated.
6. Run the calendar-sync CLI subcommand via `os/exec` (or call the same entry function the binary calls).
7. Return stdout, stderr, exit code, plus a captured list of the fake gws invocations for assertion.

The same helper is used for daemon tests and one-shot run tests. For daemon tests, send SIGTERM after the test's actions complete and assert clean shutdown.

### Test categories

- **Unit tests** for pure logic: ID derivation, canonical hashing, trailer regex, classification decisions with hand-built input. No subprocess. Live next to their code.
- **Subprocess tests** for `internal/gws` only: drive the fake gws to verify the wrapper builds the right argv and parses responses correctly.
- **Integration tests** for `internal/sync`, `internal/daemon`, and the commands: full path through classification + gws wrapper + daemon scheduler, with the fake gws returning canned responses. Table-driven, one row per scenario (e.g., "source event added, no mirror exists, expects insert with deterministic ID").
- **No live-API tests in CI.** A separate `mise run test:live` task can run against a real test calendar for manual smoke tests; it stays out of the standard `mise run check`.

### Bootstrap order for tests

The same as the implementation order. For each layer:

1. Write the failing test first.
2. Implement until it passes.
3. Refactor.
4. Don't move to the next layer until the current one's tests are green AND a `feature-dev:code-reviewer` subagent has approved (per CLAUDE.md).

The first test you write is `TestDeterministicMirrorID` - the simplest pure function in the spec. Get the green-bar muscle memory before tackling anything that needs the fake gws.

## Reuse from slack-cli

The slack-cli repo is at `/Users/tammersaleh/src/github.com/tammersaleh/slack-cli`. Patterns to lift directly:

- **`internal/output/`**: JSONL `Printer` with `_meta` trailer, `Error` and `ExitError` types with exit-code mapping, the stderr error format. Identical contract.
- **`cmd/root.go`** for kong CLI struct shape, global flags wiring, the `NewPrinter` / `NewClient` factory pattern.
- **`internal/api/`** structure (split into client + paginator + error classifier) - the `internal/gws/` package can mirror the same shape with subprocess primitives in place of HTTP.
- **Test helper patterns** in `cmd/channel_test.go` (`runWithMock`) and `cmd/saved_test.go` (`runWithMockSession`). Same idea, different boundary.
- **`isolateTestEnv(t)`** in `cmd/channel_test.go` for env-var hygiene. Lift verbatim, adjust the var list.
- **kong setup**: `kong.Parse(&cli, kong.Name("calendar-sync"), ...)` with the same error-handling shape (`output.ExitError`, `output.Error`, fallback general_error JSON to stderr).
- **CI workflow** (`.github/workflows/ci.yml`) is already cloned; the slack-cli release.yml is the template for ours and is already cloned too.
- **`mise.toml` task names**: `build`, `test`, `lint`, `check`, `setup-hooks`, `skill`. Already cloned. Keep them stable.

Don't reuse:

- `slack-go/slack` SDK (no Calendar SDK; we shell out).
- HTTP mocking (`httptest.Server`) and the surrounding `runWithMock` body. The pattern is the same; the implementation differs.
- LevelDB, utls, SQLite, sqlite-modernc - all slack-Desktop-specific.
- The `internal/api/Paginate[T]` helper - gws handles pagination via `--page-all`; we just trust it.
- The whole xoxc/xoxd Desktop-extraction layer - irrelevant.

## Setup before first commit

- Run `git config core.hooksPath .githooks` (already done in this session, but verify - the pre-push hook is essential).
- Run `mise trust .mise.toml` (already done).
- Run `mise install` to make sure go 1.25 + golangci-lint are present.
- Run `mise run check` to confirm the empty stub still passes.

## Operational gotchas to watch for during implementation

- **`gws` subprocess startup cost**: Rust binary, keyring access, OAuth token refresh check. Roughly 100-300ms per invocation per measurements. Don't shell out in a hot loop; batch where possible (the daemon already does this via `--page-all`).
- **Google's HTTP 409 on deterministic ID**: per the cancelled-and-revived path in the spec, `events.insert` may 409 against a previously-deleted mirror. Handle in the insert path, not as a generic retry.
- **`events.instances` requires bounded `timeMin`/`timeMax`**: don't call it without them. Unbounded is the original bug Codex caught.
- **`gws calendar events list` parameter shape**: pass a JSON object via `--params`, not separate flags. The gws CLI is a thin Discovery-API shim.
- **launchd's `KeepAlive=true`**: a buggy daemon that crashes on startup will be restarted by launchd in a tight loop until launchd's own backoff kicks in (which can take several restart cycles). Test cold-start failure paths carefully.

## Don't

- Don't add backward-compat shims for the slack-cli architecture - this is a separate codebase with separate concerns.
- Don't introduce new dependencies beyond what `CLAUDE.md` already lists. If you need something else, push back to the spec/CLAUDE.md before pulling it in.
- Don't skip the code-reviewer subagent before push. Per CLAUDE.md it's mandatory; the docs carve-out is the only exception.
- Don't write code in `cmd/` before the `internal/` packages it depends on are tested and green. The `cmd/` layer should be thin.
- Don't write integration tests against a real Google account in CI. The fake gws is the boundary; live tests are a manual-only path.

When you've read this and acted on it, delete this file and add a `chore: remove next.md` commit. The implementation that lands afterward is a fresh start.
