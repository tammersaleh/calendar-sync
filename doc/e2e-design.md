# E2E test infrastructure - design

Catches bugs that depend on Google Calendar's actual API semantics, multi-tick state evolution, and recurrence projection - the class that drove this session's B15/B16/B18/B20/B22/B23 fixes. Unit tests against the fake-gws harness stay primary for the classifier matrix, drift signals, and argv-shape contracts.

## Constraints

Real `gws` subprocess against the user's keyring. Opt-in only; default `mise run test` and the pre-push `check` task do not run E2E. Tests are idempotent and clean up regardless of pass/fail. No CI integration - manual `mise run test:e2e` only.

## Test calendars

Test calendars are created and destroyed by the harness itself. There are no per-machine env vars, no checked-in calendar IDs, and no manual provisioning. Anyone who clones the repo and has authenticated `gws` can run `mise run test:e2e`.

Per run, the harness:

1. Lists the user's calendars via `gws calendar calendarList list`.
2. For each fixture name (`calendar-sync-e2e-source`, `calendar-sync-e2e-target`), if a calendar with that summary exists AND its description matches the harness's safety marker (`calendar-sync E2E test fixture; safe to delete`), delete it. If a calendar matches the name but NOT the marker, fail loudly - we won't nuke a user's real calendar that happens to share the name.
3. Create fresh calendars via `gws calendar calendars insert`.
4. Run scenarios.
5. At end-of-run (TestMain teardown), delete the fixture calendars.

Cost: one create + one delete per `mise run test:e2e` invocation per fixture. ~5-10s amortized over the full suite.

`gws calendar calendarList get` accepts group calendar IDs as-is, and `internal/config/canonicalize.go` passes `source`/`target` straight to `CalendarListGet` without transformation - so the auto-created calendars work in the harness's TOML config today, no F1 dependency.

## Layout

```
internal/e2e/
  doc.go                # package comment + build-tag rationale
  harness.go            # Setup/Teardown, exec wrapper, gws client
  helpers.go            # event constructors, list/get/patch/delete passthroughs
  cleanup.go            # wholesale-wipe of test calendars
  watch.go              # watch-mode harness (start daemon, observe ticks, shutdown)
  *_test.go             # one file per scenario group
```

All files: `//go:build e2e`. New mise task:

```toml
[tasks."test:e2e"]
description = "Run E2E tests against real Google Calendar (slow, requires gws auth)"
run = "go test -tags=e2e -timeout=15m -count=1 ./internal/e2e/..."
env = { CALENDAR_SYNC_E2E = "1" }
```

`-count=1` defeats Go's test cache. The pre-push `check` task is unchanged.

## Harness API

```go
type Harness struct {
    SourceCalID string  // resolved at TestMain time (created or reused)
    TargetCalID string  // same
    ConfigPath  string  // per-test config.toml
    Binary      string  // built calendar-sync binary
    GWS         *gws.Client  // for direct API access (assertions, setup, cleanup)
    Run         func(args ...string) RunResult  // wraps `calendar-sync run`
}

type RunResult struct {
    Outcomes []Outcome
    Meta     Meta
    Stderr   string
    ExitCode int
}

func Setup(t *testing.T, opts ...Option) *Harness
```

TestMain responsibilities (once per `go test` invocation):

1. Refuse to run unless `os.Getenv("CALENDAR_SYNC_E2E")=="1"` - belt-and-suspenders against an accidental fixture wipe from running `go test -tags=e2e` by hand.
2. Build the calendar-sync binary into a process-wide temp dir.
3. Resolve fixture calendars: list-by-summary, verify the safety marker, delete-and-recreate fresh.
4. Run all tests.
5. Delete the fixture calendars on exit (success or failure).

Per-test Setup:

1. `t.Setenv("TMPDIR", t.TempDir())` so the run's daemon-detection probe doesn't see the production daemon's socket. This lets E2E run while the production daemon is up.
2. Wipe events from both fixture calendars (cheap insurance against pollution from a prior test in the same run).
3. Write `config.toml` to `t.TempDir()` referencing the fixture calendar IDs.
4. `t.Cleanup` re-runs the event wipe.

`Options`: pair direction, `propagate_target_edits`, horizon, `dry_run`. Default = one pair source→target, 365d, propagate=false.

## Avoiding the production daemon

Two protections:

- The production daemon's config doesn't reference the test calendar IDs. Its sync loop never touches them regardless.
- Per-test `TMPDIR` isolates the IPC-socket detection. The production daemon's socket lives in the user's real `$TMPDIR`; the test sees an empty per-test dir.

## Watch-mode coverage

At least one scenario exercises `calendar-sync watch` end-to-end. The watch harness:

1. Starts the binary as `calendar-sync watch --config <path>` with stdout/stderr piped.
2. Observes JSONL outcomes via stdout (one per action) until a per-tick `_meta`-equivalent (or N expected outcomes) appears.
3. Drives state changes against source via direct `gws` between ticks.
4. Sends SIGTERM and waits for clean exit on test cleanup.

A single watch scenario is enough proof-of-life for daemon orchestration. Most scenarios stay on `run --once` because it's simpler to reason about and faster. Watch tests with `poll_interval = "15s"` (the SPEC minimum) so two ticks fit in ~30s.

Initial watch scenario: `TestE2E_Watch_TickPropagatesSourceEdit`. Insert source, wait for tick → mirror created. Patch source, wait for tick → mirror patched. Stop daemon.

## Cleanup

`wipeCalendar(ctx, gws, calendarID)`:

1. `events.list` with `showDeleted=false`.
2. `events.delete` each.
3. Recurring parent delete cascades to instances. Standalone instance overrides without a parent: individually deleted.

Cancelled events (status=cancelled from prior deletes) don't appear without `showDeleted=true` - they're already gone from the alive view. Google's retention window may briefly cause 409 on re-insert with the same deterministic ID; the existing 409-revive path handles that.

## Event uniqueness

Events titled `e2e-<scenario>-<random>` where `<random>` is per-test 8-char hex. Wholesale-wipe at setup catches stale events with any name; the random suffix is a second line of defense if a wipe partially fails.

## Scenarios (initial)

| File | Test | Pins |
|------|------|------|
| `happy_path_test.go` | `TestE2E_HappyPath_Insert` | Insert → mirror with managed fields, version=3, valid checksum. |
| `happy_path_test.go` | `TestE2E_SourceModified_PatchMirror` | Patch source → mirror patched, source_updated/checksum refreshed. |
| `happy_path_test.go` | `TestE2E_SourceDeleted_DeleteMirror` | Source cancelled → mirror cancelled. |
| `recurring_test.go` | `TestE2E_RecurringParent_With_InstanceOverride` | Recurring parent + source-side override → mirror parent + instance present. |
| `recurring_test.go` | `TestE2E_InstanceOverridePropagates` | Modify source instance → mirror instance updates. |
| `b20_test.go` | `TestE2E_Revive_CancelledMirror` | Cancel mirror via gws bypass; sync revives with `insert/source_updated`. |
| `b23_test.go` | `TestE2E_StaleBookkeeping` | Bypass-patch source AND stored `calendar-sync:source_updated` to match (so the daemon sees source_changed=false, mirror_drifted=false), then sync; expect `patch/stale_bookkeeping`. The mechanism: source's managed fields differ from mirror's, but the daemon's stored bookkeeping reports clean. |
| `propagate_test.go` | `TestE2E_TargetEdit_Propagates` | propagate_target_edits=true; edit mirror; sync patches source. |
| `filtering_test.go` | `TestE2E_Transparency_Skipped` | Transparent source → no mirror. |
| `filtering_test.go` | `TestE2E_Declined_NoMirror` | Source.responseStatus=declined → no mirror. |
| `horizon_test.go` | `TestE2E_OutsideHorizon_NoMirror` | Source past horizon → skipped. Move into horizon → mirror created. |
| `collision_test.go` | `TestE2E_409_RecoversAndReconciles` | Pre-create mirror with deterministic ID; sync recovers via revive path. |
| `watch_test.go` | `TestE2E_Watch_TickPropagatesSourceEdit` | Daemon mode. Insert → tick → mirror. Patch → tick → mirror updated. SIGTERM clean. |

13 scenarios. Estimated ~3 minutes serial.

## Out of E2E scope

- B18 (transient 5xx tolerance) - can't deterministically inject 5xx from Google.
- B19 (mid-flow API failure) - same.
- B22 (410 on delete) - timing-dependent.
- Schema migration v1→v3 - no current code path produces a v1 mirror.

These remain unit-only.

## Test conventions

`t.Parallel()` is OFF - tests share the same two test calendars. Each setup wipes both (so leftover state from a prior test in the same run can't leak). Outcome assertions match by `(action, reason)` plus optional `source_event`/`target_event` predicates. The harness exposes `result.AssertOutcome(t, OutcomeMatch{Action: "insert", Reason: "source_updated"})`.

If the suite ever balloons past ~5 minutes, parallelism is straightforward to retrofit: add calendars C, D, ..., parameterize Setup to claim a calendar pair from a pool.
