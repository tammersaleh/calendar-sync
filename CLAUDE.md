# calendar-sync

Google Calendar event mirroring tool. Replaces the calendar-syncing piece of Reclaim.ai. Go, kong, `gws` subprocess for Calendar API access. JSONL output, launchd-driven polling.

## Workflow

Work is driven by `SPEC.md`. Every change - feature, bug fix, perf fix, refactor - follows the same workflow. No shortcuts for "small" fixes:

0. **Check CI before starting any work.** Run `gh run list --workflow=ci.yml --limit 3` and `gh run list --workflow=release.yml --limit 3` (or the equivalent `gh pr checks` for an open release PR). If a recent run is failing or in progress with errors, STOP and ask the user whether to fix that first before proceeding with the new task. Don't stack new work on top of a broken pipeline - a failing release workflow blocks shipment, and a failing CI signals a regression worth understanding before adding more changes.
1. Read `SPEC.md` for the relevant command/feature. Also check `doc/progress.md` if it exists; it captures handoff state from prior autonomous sessions.
2. Create a feature branch off main (or work directly on main for hot fixes - still follow every other step).
3. Red-green-refactor: write failing tests first, then implement, then clean up. See "Implementation strategy" below for how to spawn the implementation work.
4. Run both `mise run test` and `mise run lint` after every change. Both must pass before committing.
5. Keep commits small and conventional. Commit types drive releases - see "Release versioning" below.
6. **MANDATORY code review before push** (code changes only - skip for docs-only changes where every modified file is Markdown or plain-text documentation): spawn a `feature-dev:code-reviewer` sub-agent on the pending changes (`git diff main...HEAD` for a branch, or on the commits about to push for direct-to-main work). Tell the reviewer to scrutinize tests: look for tests that don't actually test what they claim, useless tests, and missing test coverage. Address every important or critical finding. **Re-run the reviewer after addressing feedback** to confirm the fixes are clean - "before and after" reviews are both required. Never push code without a clean review pass. Skipping is not acceptable regardless of how small a code change looks; the docs carve-out is the only exception.
7. Merge to main and **push** - this is the standing default, do not ask. After pushing a release-triggering commit (`feat:`/`fix:`), **wait for the release to cut, then install it**: watch release-please open and auto-merge the release PR, watch GoReleaser build and update the Homebrew tap, then `brew upgrade calendar-sync` and confirm `calendar-sync version` reports the new tag. The job isn't done at "pushed" - it's done when the released binary is installed and the daemon is running it. The ONLY exception is the SSH agent being unreachable: then leave the commit stacked locally and tell the user - never block waiting for auth. See "Git" below.
8. Retrospective: review your approach and these instructions. Update CLAUDE.md with anything you learned that would help future sessions. Update `doc/progress.md` so the next session can pick up.
9. Move on to the next feature.

## Implementation strategy

Implementation work for each `next.md`-layer-sized chunk runs in a per-layer subagent to keep main-context low. The pattern:

1. **Plan in main.** Identify files to create, types/interfaces to define, which prior packages to import. Read the relevant `SPEC.md` sections.
2. **Spawn a `general-purpose` subagent** with: the SPEC excerpt, the interfaces from prior layers it'll consume, the types it should produce, the test conventions from this CLAUDE.md, and the explicit list of files to write. The subagent returns implementation + tests in the working tree.
3. **In main: run `mise run check`** to verify tests + lint pass. If they don't, address in main or send back to the subagent.
4. **Spawn `feature-dev:code-reviewer`** on the diff (per "Workflow" step 6). Mandatory.
5. **Address findings.** Small fixes in main; substantial ones via another subagent. Re-run code-reviewer for the second pass.
6. **Commit and continue** to the next layer.

Main retains SPEC.md fluency and prior-layer architecture decisions. Subagents do the heavy lifting of implementation. Code review always runs in a subagent (the reviewer needs an independent read of the diff anyway).

The reviewer subagent's UUID is reusable: continue the conversation via `SendMessage` for the second-pass review instead of spawning a fresh one each time. That preserves the reviewer's context of the original findings.

## Release versioning

Releases are fully automated via release-please + GoReleaser. Release-please watches main; when a commit with a version-bumping type lands, it opens a release PR which auto-merges once CI passes. The merged PR cuts a tag + GitHub Release; GoReleaser builds binaries and pushes an updated Formula to `tammersaleh/homebrew-tap`. Nobody runs `git tag` by hand - and nobody clicks Merge on the release PR either.

This means commit type is not a style choice. It is the entire release trigger. A `feat:` or `fix:` commit pushed to main ships as a new Homebrew release within minutes. A `chore:` or `docs:` commit pushed to main ships nothing. Pick the type based on user-facing impact, not diff size:

- `feat:` - minor bump, listed under "Features". New commands, flags, outputs, or API surface.
- `fix:` - patch bump, listed under "Bug Fixes". Behavior that was already promised but broken.
- `feat!:` (or `BREAKING CHANGE:` footer) - minor bump pre-1.0, major post-1.0, listed under "BREAKING CHANGES". Anything that can break an existing caller: removed/renamed flags, changed output shape, changed exit codes, changed command behavior, changed config schema, changed extended-property layout, changed state.json shape.
- `chore:`, `docs:`, `test:`, `refactor:`, `perf:`, `style:` - no release, not in changelog. Internal only.

Rules:

- If a commit contains both a feat and a fix, split it into two commits.
- Dependency bumps: `fix:` if the bump reaches users, otherwise `chore:`.
- Don't downgrade a type to avoid a release. If it's user-facing, it's `feat:` or `fix:`.
- Don't upgrade a type to force a release. Internal refactors are `refactor:` even if the change is large.
- Write the subject in imperative mood and keep it under ~70 chars.

## Autonomy

Work through features independently. Never stop to ask "should I continue?" or "want me to keep going?" - the answer is always yes. After giving a status summary, keep working. Only escalate when:

- A design decision isn't covered by `SPEC.md`.
- Something feels wrong (scope creep, gws/Calendar API limitation, etc.).

## Project structure

The intended layout once code lands:

```
cmd/
  calendar-sync/
    main.go        # entrypoint; wires kong, calls into cmd
  root.go          # CLI struct, global flags, NewRunner
  run.go           # `run` subcommand
  watch.go         # `watch` subcommand (the long-running daemon)
  init.go          # `init` subcommand
  config.go        # `config show`, `config validate`
  pair.go          # `pair list`, `pair test`
  mirror.go        # `mirror list`, `mirror prune`
  status.go        # `status` subcommand (queries the daemon's IPC socket)
  install.go       # `install`, `uninstall`
  skill.go         # `skill`
  version.go       # `version`
internal/
  config/          # TOML loader, validation, canonicalization
  gws/             # gws subprocess wrapper (events.list, events.insert, etc.) + typed errors
  sync/            # core algorithm: list, classify, reconcile, prune
  mirror/          # mirror payload construction, extended-property layout, drift handling
  recurring/       # recurring-instance handler, originalStart helpers
  daemon/          # lifecycle, scheduler, IPC socket
  output/          # JSONL printer, _meta trailer, error stderr writer
  launchd/         # plist generation, launchctl wrappers
  testhelpers/     # fake-gws harness (test binary re-entry)
```

This is the target. The autonomous implementation session is allowed to deviate where it makes sense, but the boundaries between `gws` (subprocess), `sync` (orchestration), and `mirror`/`recurring` (per-event reconciliation) are load-bearing for testability.

Note: SPEC.md (round 7) eliminated the on-disk state.json file - everything except `config.toml` lives in process memory. There's no `internal/state/` package and no `state` subcommand; daemon-state queries go through `calendar-sync status` over the IPC socket.

## Testing

Tests live next to the code they test (`foo_test.go`). Use table-driven tests.

The `gws` subprocess is the only test boundary. The fake-gws harness lives in `internal/testhelpers/` and uses the **test-binary-as-fake** pattern: the test binary is symlinked as `gws` onto a per-test PATH, and an env-var sentinel re-routes its `TestMain` into the fake-gws code path that reads a JSON scenario file and emits canned responses. No separate `go build` of a fake binary, no HTTP mocking.

Each test package that uses the harness wires it into TestMain:

```go
func TestMain(m *testing.M) {
    testhelpers.MaybeRunFakeGWS()
    os.Exit(m.Run())
}
```

Tests then use `testhelpers.WithFakeGWS(t, scenario, fn)` which returns a `[]RecordedCall` for assertions. The harness's caller contract:

- Tests must NOT call `t.Parallel()` - env vars are process-wide and would clash between concurrent fake invocations.
- `fn` MUST NOT return before all gws calls it triggers have completed - the calls log is read after `fn` returns.

For unit-level coverage of internal logic that takes a small interface (e.g. `config.CalendarLister`), prefer a hand-rolled in-process stub over the harness; reserve the harness for end-to-end command tests and per-method gws-wrapper coverage where the gws argv shape itself is what's under test.

For functions only accessible within an internal package, put unit tests in a file that uses `package <pkg>` rather than `package <pkg>_test`.

## Git

This is a personal project. The workflow is entirely local: commit on a branch (or directly on main for small fixes), merge to main, push. Don't open pull requests. `gh pr create` is not part of any flow here. The only PR that exists is the release-please automation PR, and that's created and merged without human involvement.

### Push policy

Long autonomous sessions can run while the user is away or asleep. SSH agents disconnect, network blips happen, push fails. That is expected and not a blocker.

- Continue committing locally after each layer's clean review pass.
- Never block on a push failure. Never wait for user authentication.
- Don't sleep, retry, or ping the user on a push error - just keep working through more layers.
- The user pushes the accumulated stack manually when they return to the keyboard.
- The pre-push hook still runs `mise run check`, so each commit must remain green even if push isn't actually attempted in this session.

## Output

JSONL to stdout. Every command emits one JSON object per line, ending with a `_meta` trailer. Errors as JSON to stderr. See `SPEC.md` for the full output model.

## Architecture decisions and gotchas

Add notes here as decisions are made. The bar is "would this surprise a future implementer reading just SPEC.md".

### Type duplication: `mirror.EventDateTime` and `gws.EventDateTime`

Both packages declare the same 3-field struct (Date / DateTime / TimeZone) with identical JSON tags. The duplication is intentional: `mirror.ManagedFields` is the canonical-hash input shape, `gws.Event` is the wire format. `mirror.ManagedFieldsFromEvent` (in `convert.go`) bridges the two; `convert_test.go` pins round-trip checksum equality so a divergence between the two definitions fails a test rather than silently shifting the hash.

### `DriftSignal.NeedsMigration`

Pre-v2 mirrors lack a `calendar-sync:checksum` extended property. Naively, the standard sha256-mismatch check would always fire `MirrorDrifted=true` for them, which is misleading - those mirrors haven't necessarily drifted; they just predate the schema bump. `ComputeDriftSignal` reads `calendar-sync:version` and surfaces `NeedsMigration` so the sync layer routes v1 mirrors through SPEC's "Schema version migration" path (compare managed fields directly) rather than the standard four-way matrix.

### `gws.ErrAPIGone` for 410 syncToken expiry

SPEC's user-facing exit-code table doesn't enumerate 410, but the per-tick reconciliation step requires a typed sentinel so the sync layer can detect "syncToken invalid → trigger full re-sync" via `errors.Is(err, gws.ErrAPIGone)`. Both documented Calendar API 410 reasons (`fullSyncRequired`, `updatedMinTooLongAgo`) map to ErrAPIGone with `ExitCode: 1`.

### `config.AccessRoleAtLeast` fails closed on unknown roles

Both an unknown actual role AND an unknown minimum return `false`. This is a security-style gate ("is this calendar readable/writable for the role this pdir requires?") and failing open on programmer error or a future-API surprise would silently approve unauthorized access.

### Disabled pairs are skipped entirely

SPEC's `[[pairs]]` entry says `enabled=false` skips the pair. `Canonicalize` honors that literally: no calendar resolution, no validation (including accessRole), no PDir emitted. A typo'd or now-inaccessible calendar in a disabled pair never blocks startup - the failure surfaces only when the user re-enables the pair.

### `BuildPayload` does NOT include `calendar-sync:checksum`

The checksum is a follow-up patch using the post-write Event resource Google returns (per SPEC's "Computing the checksum from the post-write event"). The initial insert sends `source` / `source_updated` / `version` only; the sync layer makes a second `events.patch` to write the checksum after reading the post-write resource.

### `gws.EventsListParams.SingleEvents` has `omitempty`; `ShowDeleted` does not

SPEC's startup wire shape includes explicit `singleEvents=false`; SPEC's incremental wire shape omits it. With omitempty, the caller's zero-value `false` drops the key from JSON, which matches SPEC's incremental shape exactly and is functionally equivalent to explicit-false on startup (Calendar API's default is false). One struct, both call shapes, no surprise.

`ShowDeleted` deliberately has NO omitempty: callers always want `true`, and explicit transmission surfaces caller bugs immediately rather than silently sending the API default.

### `CalendarLister` interface in `internal/config`

Single-method interface (`CalendarListGet`) the canonicalize step accepts. Tests use a hand-rolled stubLister (in-process map). Production passes `gws.Client.CalendarListGet` directly. Avoids spinning up the fake-gws harness for unit-level config tests where the gws argv shape isn't what's under test.

### `gws.Error` sentinel pattern

Each error code (`gws.CodeAPIConflict` etc.) has a paired sentinel value (`gws.ErrAPIConflict` etc.). The custom `Is` method matches by `Code` only, so callers do `errors.Is(err, gws.ErrAPIConflict)` without caring about the wrapped HTTP status, reason, or operation context. `Op` and `Cause` survive in the typed error for log/output enrichment.

### Sync token advancement is conditional

The wrapper exposes `nextSyncToken` from the LAST NDJSON page (or empty string if Google omitted it). Per SPEC, the in-memory syncToken must advance only when every dependent pdir successfully processed every event in the delta - that conditional-advancement invariant is the sync layer's responsibility, not the wrapper's.

### v1 migration cells live in callers, not in `mirror.Classify`

`mirror.Classify` does not consume `signal.NeedsMigration`. The two migration-specific cells (`migration_upgrade` for no-actual-drift, `migration_source_won` for both-changed) live in the caller. The recurring handler branches on `signal.NeedsMigration` BEFORE delegating to `Classify`; the sync layer (layer 6) will need the same branch when reconciling non-recurring/parent events. The other two cells (source-only, mirror-only) are identical between v1 and v2 mirrors so they fall through to `Classify` unchanged. This keeps `Classify` tidy at the cost of a small per-caller branch.

### `recurring.Handler` accepts callbacks, doesn't import sync

`Handler.LookupMirrorParent` (over the per-target inventory) and `Handler.ReconcileParent` (the classification path for source parents) are function-typed fields rather than interface methods on a sync-layer type. Layer 5 cannot import layer 6 without a cycle; the inversion lets the sync layer inject closures over its inventory map and classification logic. Tests pass plain closures over hand-rolled fixtures instead of mocking an interface.

### Cancellation patches skip the checksum follow-up

Source-cancelled / declined / tentative / transparent all patch the mirror instance with `{status: cancelled}` and stop. No follow-up `calendar-sync:checksum` write happens because no managed field changed; the existing checksum on the mirror is still accurate. This is one of two write paths in the recurring handler that bypass `patchMirrorWithChecksum` (the other is the source-side patch in `propagate`, which writes to the source, not a mirror).

### Revive cell sits before the four-way drift matrix

Both `Classifier.reconcileNormal` and `recurring.Handler.applyDriftMatrix` short-circuit to a revive path when `mirror.Status == EventStatusCancelled` at classify time. By the time those functions run, the source has already passed steps 3-7 (cancelled / declined / tentative / transparent / outside_horizon - all filtered upstream), so a cancelled mirror at this point is the leftover of an earlier cancellation cell whose source has flipped back. Status is intentionally NOT in `mirror.ManagedFields` (adding it would force a checksum migration across every existing v3 mirror), so the standard drift signal would emit `skip/unchanged` and leave the mirror cancelled forever.

The revive patches with the full managed-field payload plus `status=confirmed`, then runs the standard checksum follow-up. Outcome shape mirrors `insert.go`'s post-409 `reviveCancelledMirror`: `ActionInsert + ReasonSourceUpdated`. Two helpers exist in parallel: `Classifier.reviveCancelledMirror` (non-recurring, in `insert.go`) and `Handler.reviveCancelledMirrorInstance` (recurring, in `handler.go`). The non-recurring path is reused as-is for both 409-recovery and B20.

Ordering matters: the revive check fires BEFORE the inherited/migration branches in the recurring handler, and BEFORE the `NeedsMigration` branch in `reconcileNormal`. A cancelled mirror at any schema version or inheritance state takes the revive path - the rewrite at the current `SchemaVersion` IS the migration in those cases. `BuildPayload` always writes `mirror.SchemaVersion` so the revive doubles as a schema upgrade for legacy mirrors.

### `config.CompactDuration` is the single source of truth for wire durations

Both SPEC line 588 (`config show`) and SPEC line 725 (IPC `status`) format durations the same way: `60s` (whole seconds) or `24h` (whole hours), but never Go's verbose `1m0s` / `24h0m0s`. The implementation lives in `internal/config/duration.go` as `CompactDuration(time.Duration) string` with a `Duration.Compact()` method for the typed wrapper. The daemon's `compactDuration` in `internal/daemon/socket.go` is a thin delegate so the two layers can't drift.

### `gws.ErrGWSNotFound` is *gws.Error, not fs.ErrNotExist

When `gws` is not on PATH, `internal/gws/client.go` returns `&Error{Code: CodeGWSNotFound, ...}` directly rather than wrapping the underlying `fs.ErrNotExist`. This is intentional: MapError's `fs.ErrNotExist` branch routes config-load failures to `config_not_found` via a substring heuristic. If the gws-binary error were also `fs.ErrNotExist`-shaped it would either be misclassified or require a more brittle heuristic. The typed sentinel sidesteps the question - `errors.Is(err, gws.ErrGWSNotFound)` matches by Code only and gets its own branch in MapError before any fs.ErrNotExist matching happens.

### Partial-failure path always emits `_meta`

`run.go` no longer returns on the first PDirResult.Err. Per SPEC §"Partial failure semantics" (lines 1287-1303) every pdir runs to completion; failures are collected, the meta line is emitted with `failures: [...]`, and only THEN does `cmdError(partial_failure)` get returned. Tests should assert on the meta line (last stdout line) plus the MapError code, not on early termination.

### `propagate_target_edits` gates two-way sync per-pdir

`config.PDir.PropagateTargetEdits` (resolved from `Pair.PropagateTargetEdits` when set, else `Settings.PropagateTargetEdits`) is ANDed with `pd.SourceWritable` inside `Reconciler.buildClassifier` to produce the effective writability the Classifier and recurring.Handler see. The downstream drift-matrix code is unchanged - it still consumes `SourceWritable` as before. Per-pair scoping lets operators ramp two-way sync one direction at a time. A read-only source (`accessRole < writer`) can never propagate regardless of the setting; the gate is a SUBSET, not an override.

The gate lives in the Reconciler rather than in mirror.Classify so Classify stays a pure function. Tests that exercise the matrix directly (classify_test.go) bypass the gate by constructing a Classifier with explicit SourceWritable; tests that want to pin the gate (reconciler_test.go's TestBuildClassifier_Gate*) drive `Reconciler.buildClassifier` directly with `pd.PropagateTargetEdits` set on the test PDir.

### Per-pair horizon resolved at canonicalization

`Pair.Horizon *Duration` (TOML optional, nil = fall back to `Settings.Horizon`) is resolved during `expandPDirs` into `PDir.Horizon time.Duration` (the effective per-pdir value). Downstream consumers (`Classifier.Horizon`, `OrphanWalker.Horizon`) read the resolved per-pdir value. The Reconciler-level horizon is GONE - `WithHorizon` removed in commit fcf1bf3.

The source-list TimeMax uses `Reconciler.sourceMaxHorizon(source)` which returns the max effective horizon across all pdirs that share the source. Two pdirs sharing one source with different horizons get a single source-list call with the longer horizon's TimeMax; per-pdir filtering happens at classification time.

### `Pair.Direction` field exists only for v1->v2 migration rejection

Per commit 88d77d0, the `direction` config field is removed. The `Pair.Direction` Go field stays in `internal/config/types.go` SOLELY so `validatePair` can detect non-empty values and reject with a migration hint. `toml.Unmarshal` silently ignores unknown TOML keys, so dropping the Go field would mean a user's stale `direction = "..."` is silently inert rather than producing a clear migration error. The field can be removed in a future major version once any in-the-wild configs have migrated.

### Logger threading uses re-declared interfaces, not output package imports

`internal/sync` and `internal/recurring` each re-declare a 4-method `Logger` interface (Debug/Info/Warn/Error) rather than importing `internal/output`. This keeps the dependency direction one-way: output is a top-of-tree package, the lower layers don't depend on it. Production code passes `*output.Logger` through `WithLogger(...)` options on Reconciler / gws.Client; the same pointer satisfies all three interfaces structurally. Tests leave the field nil; every per-method log call does an `if r.Log != nil` short-circuit so silence has no formatting cost.

The same pattern (`internal/gws.Logger`) exists in the gws layer for the same reason. When extending, add new logger calls inside nil-safe wrappers (`r.debug`, `c.debug`, `h.debug`) on the receiver - direct `r.Log.Debug(...)` without the nil check breaks tests that don't set Log.

### `cmdError.cause` carries cause; `handleErr` populates `ErrorEnvelope.Cause`

The partial_failure path wraps the first PDir error as the cmdError's cause field via `newCmdError(code, detail, hint, cause)`. `cmd/cli.go:unwrapCause` reads `ce.cause.Error()` directly via type assertion when err is a *cmdError, then falls back to `errors.Unwrap(err)` for non-cmdError types.

Reading the field directly is required, not stylistic. `Reconciler.runClassifyLoop` aggregates per-event classify errors via `errors.Join`, and `errors.Unwrap` returns nil on the resulting joinError (it implements `Unwrap() []error`, not `Unwrap() error`). A single-step `errors.Unwrap(cmdErr)` would therefore drop the cause whenever the pdir failed with multiple events - which is the production partial_failure shape.

### `[settings].dry_run` is OR'd with `--dry-run`, not overridden by it

`run` and `watch` compute effective dry-run as `c.DryRun || canonical.Settings.DryRun`. If config.toml has `dry_run = true`, the CLI cannot turn it off - `--dry-run=false` (kong's negative boolean form) goes from `false || true = true` and writes are still suppressed. This is intentional: a user who set the config flag did so deliberately, and a typo'd CLI invocation shouldn't override the safer setting. The escape hatch is editing config.toml.

`mirror prune` is intentionally NOT gated by `[settings].dry_run`. It has its own `--dry-run` flag and SPEC scopes the settings field to the sync loop (run/watch). Pruning is a manual surgical operation; if you ran `mirror prune` without `--dry-run`, you meant it.

### launchd `WatchPaths` drives config.toml auto-reload

The plist generated by `calendar-sync install` includes a `WatchPaths` entry pointing at the resolved config.toml path. launchd watches the listed paths via kqueue and restarts the daemon when any file is modified, created, or deleted. Because the daemon's startup re-reads config from disk, a launchd-driven restart IS the config reload. SPEC lines 945/971 still say "Config edits require a daemon restart" - that statement is now satisfied automatically rather than requiring `calendar-sync uninstall && calendar-sync install` by hand.

The resolved path comes from `config.FindPath(rt.Globals.Config)`, the same precedence chain (`--config` flag, then `$CALENDAR_SYNC_CONFIG`, then `$XDG_CONFIG_HOME/calendar-sync/config.toml`) used everywhere else. A user who installed with `--config /custom/path` gets that exact path watched, not the default one.

Editor-save behavior: most editors (vim, neovim, VS Code) save-and-swap (write to a temp file, rename over the target). launchd handles the rename cleanly because it tracks the path, not the inode - the rename fires a single kqueue event. Editors that write-in-place without atomic rename (some legacy tools) may produce two events: one for the truncate, one for the write. launchd will fire WatchPaths twice in quick succession; KeepAlive's debouncer typically collapses these into one effective restart.

The feature is macOS-only by virtue of `launchd.Install` already returning `ErrNotMacOS` on non-Darwin platforms (`internal/launchd/install.go:86`). Linux users still don't get a daemon at all.

## Sandbox

`mise run` commands work fine in sandboxed processes. Network access required during `go mod tidy` (first run) and during `go test` for any test that pulls a module.

The `gws` subprocess uses the user's keyring; running tests against a real `gws` outside the fake-binary harness needs the sandbox disabled (Netskope proxy + keyring access).
