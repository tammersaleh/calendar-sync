# calendar-sync

Google Calendar event mirroring tool. Replaces the calendar-syncing piece of Reclaim.ai. Go, kong, `gws` subprocess for Calendar API access. JSONL output, launchd-driven polling.

## Workflow

Work is driven by `SPEC.md`. Every change - feature, bug fix, perf fix, refactor - follows the same workflow. No shortcuts for "small" fixes:

1. Read `SPEC.md` for the relevant command/feature. Also check `doc/progress.md` if it exists; it captures handoff state from prior autonomous sessions.
2. Create a feature branch off main (or work directly on main for hot fixes - still follow every other step).
3. Red-green-refactor: write failing tests first, then implement, then clean up. See "Implementation strategy" below for how to spawn the implementation work.
4. Run both `mise run test` and `mise run lint` after every change. Both must pass before committing.
5. Keep commits small and conventional. Commit types drive releases - see "Release versioning" below.
6. **MANDATORY code review before push** (code changes only - skip for docs-only changes where every modified file is Markdown or plain-text documentation): spawn a `feature-dev:code-reviewer` sub-agent on the pending changes (`git diff main...HEAD` for a branch, or on the commits about to push for direct-to-main work). Tell the reviewer to scrutinize tests: look for tests that don't actually test what they claim, useless tests, and missing test coverage. Address every important or critical finding. **Re-run the reviewer after addressing feedback** to confirm the fixes are clean - "before and after" reviews are both required. Never push code without a clean review pass. Skipping is not acceptable regardless of how small a code change looks; the docs carve-out is the only exception.
7. Merge to main. Push if the SSH agent is reachable; otherwise leave the commit stacked locally - see "Git" below.
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

## Sandbox

`mise run` commands work fine in sandboxed processes. Network access required during `go mod tidy` (first run) and during `go test` for any test that pulls a module.

The `gws` subprocess uses the user's keyring; running tests against a real `gws` outside the fake-binary harness needs the sandbox disabled (Netskope proxy + keyring access).
