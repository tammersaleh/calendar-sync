# bugs.md

Running list of bugs surfaced during the v1.0.0 install + test session. Add a new section as each new bug is identified. Move a section to "Fixed" when the fix lands and is verified. Don't delete entries - the trail is part of the artifact.

## Open

### B3 - gws subprocess timeouts cascade to `partial_failure`

A 365-day-horizon dry-run produced ~200 `gws subprocess: context deadline exceeded` errors. The outer `--timeout` default is 5m; per-event `events.instances` lookups serialize and queue past it.

Possible mitigations:
- Increase the per-event gws subprocess timeout independently of the outer wall-clock cap.
- Reduce per-event API calls (e.g. cache the target instances list rather than one call per instance).
- Parallelize per-event processing.

### B5 - gws stderr bleeds into the error message

A captured failure cause read:

```
horizon check for recurring parent 2on8pejh4aunu1nqco5hi7ut88: api_invalid_request during events.instances: Using keyring backend: keyring
error: HTTP request failed
```

`Using keyring backend: keyring` is gws's own informational stderr leaking into the formatted error string. Not safety-critical but obscures the real error.

### B8 - `config.FindPath` can return relative paths; launchd `WatchPaths` is undefined for them

`config.FindPath` returns `--config` / `$CALENDAR_SYNC_CONFIG` values verbatim. A user who runs `calendar-sync install --config ./config.toml` would land a relative-path entry in the plist's `WatchPaths` array. launchd's documentation calls relative paths there undefined behavior; in practice it appears not to fire kqueue events for them, defeating B7's auto-reload.

Pre-existing in `internal/config/loader.go:25-44`; not introduced by B7. Normal install path (XDG default, `~/.config/...`) is always absolute, so default users don't hit it.

Fix sketch: `filepath.Abs` the result of `config.FindPath` (or just inside the launchd plist generator path before stamping into WatchPaths). Add a unit test that drives FindPath with a relative `--config` and asserts the returned path is absolute.

### B9 - plist generator does not XML-escape the config path

`internal/launchd/install.go` uses `text/template` (not `html/template`) to render the plist. A config path containing `&`, `<`, or `>` would produce malformed XML and `launchctl load` would reject it. Pre-existing limitation; unlikely to bite default users (`~/.config/calendar-sync/config.toml` has no special chars), but a user with a path like `/tmp/sync&backup/config.toml` gets a broken plist.

Fix sketch: switch to `html/template` (handles XML-class escaping for content), or wrap the path in `xml.EscapeText` before stamping. Test should generate a plist with `&`, `<`, `>` in the config path and verify the output parses as valid XML.

## Fixed

### B16 - inherited recurring-instance mirror shadows parent in inventory, propagate clobbers source parent (CRITICAL)

When calendar-sync writes a recurring parent mirror, Google auto-materializes the parent's instances and copies the parent's `extendedProperties.private` (including `calendar-sync:source`) to each materialized instance. If the user then edits an instance on the target side, `events.list` returns it as a real entry with the inherited `calendar-sync:source` still pointing at the source PARENT's tuple - the same value the actual parent mirror carries.

`BuildInventory` indexed events keyed by parsed source-tuple, so the inherited instance and the parent both landed at `{source_cal, parent_id}`. Last-writer-wins; whichever pass returned the instance after the parent (depends on Google's iteration order) shadowed the parent in inventory.

Then on the next sync, `reconcileNormal` for the source PARENT looked up that key, got the mirror INSTANCE (with its per-occurrence start/end and possibly a recurrence inherited from the API response shape), computed drift against the parent's stored checksum (always drifts because instance fields differ from parent fields), and routed to `propagate(target_edited)`. The propagate body included start/end/recurrence from the instance, sent to the source PARENT via events.patch. The source parent's recurrence anchor moved to the user's edited time, breaking the entire series.

Surfaced after the user edited a recurring-mirror instance on cw with `propagate_target_edits=true` and a manual `calendar-sync run` triggered the propagate. The personal source's `Lunch & Reading` recurring parent was moved from 2026-02-23T11:30 to 2026-05-20T12:00; recovered manually via a direct `events.patch` back to the original anchor.

Fixed by switching `BuildInventory` to a two-pass shape: pass 1 builds a (mirror parent ID -> parsed source-tuple) map across all schema-version queries, pass 2 indexes each event but skips any instance whose parsed source-tuple matches its parent's recorded source-tuple (`mirror.IsInheritedRecurringInstance`). Explicitly-managed instances (whose `calendar-sync:source` carries the `_<UTC>` per-instance suffix) have a different source-tuple than the parent and are kept. SPEC.md and tests updated.

### B15 - recurring-instance handler clobbers source overrides on first encounter (CRITICAL)

When calendar-sync writes a recurring parent mirror, Google auto-materializes the parent's instances using the parent's RRULE. The auto-materialized instances inherit the parent's `extendedProperties.private` (including `calendar-sync:checksum`) but their live managed fields differ from the parent's (each instance has its own start/end). The standard drift matrix saw `mirror_drifted=true` (live checksum != stored), and when source had a per-instance override (e.g. a user rescheduled one occurrence), `source_changed=true` as well. The newer-wins tiebreak picked the freshly-materialized mirror over the pre-existing source override and routed to `propagate(target_edited, conflict_target_won)`, sending the mirror's RRULE-projected start/end back to the source - reverting the user's reschedule.

Surfaced during the personal→cw rollout dry-run at horizon=14d. A real source override (yoga 5/11 rescheduled from 9am to 10am) was about to be clobbered. Caught before the actual run.

Fixed by adding an inherited-instance branch in `applyDriftMatrix`: detect via `mirror.IsInheritedRecurringInstance` (mirror's `calendar-sync:source` EventID equals `source.RecurringEventID`), then route through the same source-wins bootstrap path as the schema-version migration. New conflict label `inherited_source_won`, new reason `inherited_upgrade`. Source is never patched on this path. SPEC.md updated with the new branch and decision table. Tests pin all four cells (no-drift upgrade, real-drift source-wins, both-changed source-wins, managed-instance regression).

### B14 - orphan walker errors on HTTP 410 from events.delete

After B13 the orphan walker started seeing legitimate 410 responses on `events.delete` ("Resource has been deleted") - typically a cancelled exception instance of a recurring event whose parent was just deleted in the same pass, triggering a server-side cascade. The walker only handled `ErrAPINotFound` (404), so the 410 bubbled up as `partial_failure`.

Surfaced on the rollout's drop-and-restart pivot: dropping horizon from 14d to 1d made the orphan walker classify ~63 mirrors as outside_horizon. Many of those were recurring instances; deleting their parents cascaded the children, and the deletes-of-already-cascaded-children returned 410.

Fixed by adding `gws.ErrAPIGone` to the carry-on branch alongside `gws.ErrAPINotFound`. Test pin: `TestOrphanWalk_DeleteReturns410_TreatedAsSuccess` (`internal/sync/orphan_test.go`).

### B13 - gws error JSON parsed from wrong stream; masks 409/410/404 (CRITICAL)

`gws` emits the Calendar API error envelope on **stdout** (the JSON: `{"error":{"code":409,...}}`). stderr gets only the human-readable summary plus keyring noise. `internal/gws/errors.go:classifyError` parsed **stderr** for the JSON, found nothing parseable, and fell through to `api_invalid_request` for every API error. This masked every meaningful HTTP-status sentinel - including 409 (which broke SPEC's cancelled-and-revived flow that triggers on `errors.Is(err, ErrAPIConflict)`).

The bug was the proximate root cause of B10, B11, AND B12. With B13 fixed, the 409 from inserting against a tombstone correctly routes through `doInsert`'s 409 handler → `reviveCancelledMirror` → mirror is revived in-place. Without B13, the same insert appeared as `api_invalid_request` and the revival path never ran.

Surfaced during the day-by-day rollout's Day 2: with horizon=2d the 24-event source list included the "CoreWeave P&E Offsite" tombstone from yesterday's cleanup. Insert returned 409, classifyError mismapped to `api_invalid_request`, the run errored as `partial_failure` with cause "The requested identifier already exists." - clearly a 409 misrendered.

Fixed by parsing stdout first, falling back to stderr if stdout doesn't contain a parseable envelope (preserves backwards compat for tests/edge cases that route the envelope to stderr). Test pin: `TestClassifyError_ParsesAPIErrorFromStdout` (`internal/gws/errors_test.go`) covers 404, 409, 410, and 400 with realistic stdout+stderr pairs.

### B11 - sync orphan walk errors on already-cancelled mirrors

Same root cause as B10 but in a different code path. `BuildInventory` uses `ShowDeleted:true` to surface tombstones for the cancelled-and-revived flow, then indexes them into the inventory. The orphan walk iterates the inventory and (correctly, given the visited set) classifies tombstones as orphaned, then tries `events.delete` on them - which Calendar API rejects with `api_invalid_request: Resource has been deleted`. The walker only catches `ErrAPINotFound`, so the error bubbles up as `partial_failure` and the entire sync tick errors out.

Triggered on the very first `calendar-sync run` of the day-by-day rollout: 74 tombstones from the prior cleanup were indexed, all 74 looked orphaned (no source on a 1-day horizon), all 74 errored on delete.

Fixed by skipping `ev.Status == gws.EventStatusCancelled` events at the inventory-build step. SPEC's cancelled-and-revived flow uses a per-event `events.get` triggered by 409 on insert - it doesn't depend on the inventory holding tombstones, so the skip is safe. Test pin: `TestBuildInventory_SkipsCancelledTombstones` (`internal/sync/inventory_test.go`) constructs a confirmed + cancelled mix and asserts only the confirmed reaches the inventory.

### B10 - `mirror prune` errors on already-cancelled events

`listMirrors` queries `ShowDeleted:true` so it returns tombstones (events previously deleted via `events.delete`, which Calendar API stores with `status=cancelled` rather than physically removing). The candidate loop didn't filter these out, so prune would call `events.delete` on a tombstone, which returns `api_invalid_request: Resource has been deleted` - NOT the `NotFound` the existing carry-on branch catches. The function then returned the error early, skipping any remaining live mirrors and emitting no `_meta` trailer. Discovered during phased cleanup of the B1 leak: Phase 3 silently truncated after deleting 4 of 19 candidates because the 5th was a tombstone from Phase 2.

Fixed by skipping `status=cancelled` events at the top of the candidate loop. They're already deleted; nothing to do. Test pin: `TestMirrorPruneCmd_SkipsAlreadyCancelledEvents` (`cmd/mirror_test.go`) constructs a mix of confirmed and cancelled mirrors and asserts only the confirmed ones reach `EventsDelete`.

### F1 - `partial_failure` envelope dropped underlying error

Initially fixed in `5a412e6 fix: surface underlying error in partial_failure stderr envelope` but that pass missed the joinError case. Followup `803317c fix: surface joinError cause in partial_failure stderr envelope` reads `cmdError.cause` directly via type assertion. Verified empirically against the live calendar - the envelope's `cause` field now carries the joined classify errors (~32KB).

### B1 - `--help` triggered a live run on subcommands (CRITICAL)

`./calendar-sync run --help` printed kong's usage text AND then executed a real, non-dry-run sync. 37 mirror events landed on `me@tammersaleh.com` before the process was killed.

Root cause: `cmd/cli.go` constructed the kong parser with `kong.Exit(func(int) {})` so kong's `helpFlag.BeforeReset` (which calls `ctx.Kong.Exit(0)` after printing help) became a no-op. Parse returned successfully and `kctx.Run(rt)` dispatched the subcommand. Same mechanism would have triggered for kong `VersionFlag` and any future kong-builtin terminator.

Fixed in `9705671 fix: short-circuit kong --help / --version before subcommand dispatch`. The kong-Exit callback now writes the code into a sentinel; if Parse called Exit, `Run()` returns that code without dispatching. `TestRun_HelpFlagDoesNotDispatchSubcommand` (`cmd/cli_test.go`) pins the invariant for every subcommand including --help mixed with other flags and --quiet. Subcommands with required positional args (`mirror prune`, `mirror list`, `pair test`, top-level) still hit kong's pre-hook positional validation, so they exit 64 with "expected <X>" - the subcommand's Run is still NOT dispatched, which is the load-bearing safety guarantee.

### B6 - `[settings].dry_run = true` did not suppress writes

SPEC line 253 promised `[settings].dry_run = true` would suppress writes. The field was parsed into `config.Settings.DryRun` and emitted in `config show` output but never threaded into `newDryRunAPI()`. A user with `dry_run = true` in config.toml got live writes anyway from `run` and `watch`.

Fixed in `aa01edf fix: wire [settings].dry_run to dryRunAPI wrapper in run / watch`. `RunCmd.run` now ORs `c.DryRun` with `canonical.Settings.DryRun`. `WatchCmd.Run` (no `--dry-run` flag) gates solely on the settings field. `pair test` inherits via the `RunCmd{DryRun: true}` it constructs. `mirror prune` is intentionally NOT gated by settings - it has its own `--dry-run` flag and SPEC scopes settings.dry_run to the sync loop. Verified by `TestRunCmd_SettingsDryRunGatesWrites` and `TestWatchCmd_SettingsDryRunGatesWrites`, both using `panicWriteGws` so any leaked write surfaces as a descriptive panic.

### B4 - No `--horizon` CLI flag (misframed; config-file is the design)

The bug as originally written claimed `config.toml` had `horizon` but there was no `--horizon` flag override for day-by-day rollout. The framing was wrong: SPEC line 249 makes `[settings].horizon` the single configurable surface; there is no CLI flag, by design. Day-by-day rollout means editing the config file between runs (or using `CALENDAR_SYNC_CONFIG=/path/to/horizon-1d.toml` to swap fixtures).

The wire-through (`canonical.Settings.Horizon.Duration()` → `syncpkg.WithHorizon(...)`) was already in place at `cmd/run.go:88` and `cmd/watch.go:41`. The verification gap was that no test exercised the load → canonicalize → Reconciler chain to confirm `Reconciler.Horizon` matched the config value.

Closed by `TestRunCmd_ConfigHorizonWiredToReconciler` (`cmd/run_test.go`). The test loads two configs (`horizon = "1d"` and `horizon = "365d"`), runs the same `Load → Validate → Canonicalize → New + WithHorizon` chain `cmd/run.go:run` does, and asserts `Reconciler.Horizon` is `24h` and `8760h` respectively. A regression that drops the `WithHorizon(...)` call in run.go or watch.go would leave Horizon at zero and the test catches it.

### B7 - launchd plist did not auto-reload on config edits

Editing `~/.config/calendar-sync/config.toml` while the daemon was running had no effect. SPEC lines 945/971 documented the requirement to manually `calendar-sync uninstall && calendar-sync install` to pick up changes - awkward for any config tweak, especially during a day-by-day horizon rollout where `[settings].horizon` is the only thing changing.

Fix: add a `WatchPaths` directive to the generated plist listing the resolved config.toml path. launchd watches those paths via kqueue and restarts the daemon on any modification. Because the daemon's startup re-reads config from disk, a launchd-driven restart IS the config reload SPEC needs.

Resolved path comes from `config.FindPath(rt.Globals.Config)` so `--config` / `$CALENDAR_SYNC_CONFIG` overrides at install time get watched, not just the XDG default.

Verified by `TestRenderPlist_WatchPathsContainsConfig` and `TestRenderPlist_HappyPath` (rendered plist asserts `<key>WatchPaths</key>` plus the config-path `<string>` entry inside the array) and `TestInstall_HappyPath` (end-to-end through `Install`).

Macos-only by virtue of `launchd.Install` already returning `ErrNotMacOS` on non-Darwin platforms; the WatchPaths feature inherits that gate.

### B2 - Bogus `migration_source_won` outcomes in dry-run

15 patches reported with `conflict: migration_source_won` despite zero v1/v2 mirrors on the target. Two cooperating causes per `doc/dry-run-anomaly-analysis.md`:

- Cause A: `dryRunAPI.EventsPatch` echoes only the request body, dropping any extended properties the prior insert wrote. The follow-up checksum patch sends `body={Private:{checksum}}` ONLY, so the cached event lost `calendar-sync:version` and `calendar-sync:source` from the prior Insert. On a second pass `ComputeDriftSignal` saw a missing version and routed through the migration matrix.
- Cause B: source-list duplication - `_R<timestamp>` recurring parents appear both as a top-level event and as a `recurring_event_id` on their instances, so `runClassifyLoop` processed the same source-tuple twice.

Dry-run cosmetic only - production semantics weren't affected (real Calendar API merges patches correctly), but the wrong outcomes obscured the actual mirror plan.

Both causes fixed:

- Cause B dedupe in `e1e1f4d fix: dedupe source-tuples in runClassifyLoop (B2 cause B)`. A per-call `seen[SourceTuple]bool` short-circuits subsequent occurrences with no outcome emitted (SPEC's outcomes table doesn't define a "duplicate" reason). The `visited` set still records every occurrence so the orphan walk's "saw this on the wire" semantics are preserved.
- Cause A cache-and-merge in `f6d9f1c fix: dryRunAPI EventsPatch merges into cached Insert resource (B2 cause A)`. `dryRunAPI` now keeps a per-(calendarID, eventID) cache populated by `EventsInsert`. `EventsPatch` merges body into the cached snapshot per Calendar API JSON Merge Patch semantics: top-level fields replace, ExtendedProperties.Private/Shared merge at the key level, pointer/slice fields replace as a whole when non-nil. A patch on an event that was never Inserted falls back to body-echo, preserving the prior contract for tests that don't drive `doInsert`.

Verified by:

- `TestRunClassifyLoop_DedupesSourceTuple` (`internal/sync/reconciler_test.go`) feeds two copies of the same source event and asserts exactly one outcome plus `Counts.Inserts/EventsProcessed = 1`.
- `TestDryRunAPI_PatchMergesIntoCachedInsertResource` etc. (`cmd/run_test.go`) drive Insert(version=2 + source) → Patch({checksum}) and assert the merged result carries all three keys.
- `TestRunCmd_DryRun_DuplicateSourceEventNoLongerEmitsBogusMigration` (the un-skipped end-to-end regression) feeds duplicate events through `RunCmd.Run` and asserts no `migration_source_won` in the output and exactly one outcome per pdir.
