# bugs.md

Running list of bugs surfaced during the v1.0.0 install + test session. Add a new section as each new bug is identified. Move a section to "Fixed" when the fix lands and is verified. Don't delete entries - the trail is part of the artifact.

## Open

### B38 - a single recurring-instance edit moved the entire source series anchor and cancelled the edited occurrence (CRITICAL)

Surfaced from a live user report: the user edited one occurrence of the "Breakfast and drive Mads to school" series on the CoreWeave mirror ("this morning's event"). The occurrence vanished from both calendars and the whole weekly series shifted 15 minutes. This is the B16 failure class - an instance-level interaction rewriting the source recurring parent - but B16's fix does NOT cover it, because the mirror instance here is managed form, not inherited form.

Evidence (2026-08-13, all writes at the 07:22 tick; source `me@tammersaleh.com` [personal], mirror `tsaleh@coreweave.com` [work], pair `personal-to-work`):

- Source parent `ij3p7feqdeku8i1nncht48hf3k`: `RRULE:FREQ=WEEKLY;WKST=MO;UNTIL=20261221T075959Z;BYDAY=MO,TU,WE`, start moved 07:25 -> 07:40, sequence 4, updated `2026-08-13T14:22:10.346Z`.
- Mirror parent `cs22oh9u867c0j1bi1sa7ddleoralr5gjsnpdiuj38nfm0tamvqdq`: same RRULE, start 07:40, sequence 1, updated `2026-08-13T14:22:12.881Z`.
- Source Thursday exception `ij3p7…_20260812T142500Z` (occurrence key on the old 07:25 grid, moved to Thu 2026-08-13 07:25): cancelled, sequence 5, updated `14:22:10.346Z`.
- Mirror Thursday exception `cs22oh9…_20260812T142500Z`: managed form - `calendar-sync:source = me@tammersaleh.com:ij3p7feqdeku8i1nncht48hf3k_20260812T142500Z` (note the trailing `_<UTC>` suffix); cancelled, sequence 2, updated `14:21:59.440Z`.

Action log (`calendar-sync.out.log`, in order): `propagate personal-to-work ij3p7→cs22oh9 target_edited fields:[start]`, then `skip …_142500Z target_cancelled`, then `delete …_142500Z source_cancelled`. `calendar-sync.err.log` at `14:22:12` WARN `sync.targetDelta: target-side deletion is not propagated to source` for `cs22oh9…_142500Z` <- `ij3p7…_142500Z`. An earlier round the same morning propagated `fields:[end,start]` on BOTH the parent and the instance, so there were at least two edit/reconcile rounds.

Confirmed:

- Neither parent was user-edited. Both parents carry the daemon's tick time and low sequence numbers; an "All events" / "this and following" scope edit would stamp the user's time and bump the parent sequence. The only user action was to a single occurrence.
- The daemon nevertheless propagated a bare-parent `start` change to the source parent (`target_edited fields:[start]`), shifting all of MO/TU/WE from 07:25 to 07:40. The moved Thursday exception then fell off the new grid and was cancelled on both sides. Net user-visible effect: the edited occurrence disappeared and the series moved.

Why B16's fix does not catch it: B16's two-pass `BuildInventory` filter (`internal/sync/inventory.go:160-217`) skips only INHERITED-form instances - those whose `calendar-sync:source` equals the parent's tuple. This instance is MANAGED form (source tuple `ij3p7…_20260812T142500Z`, with the suffix), so it is indexed under its own per-instance key and never shadows the parent. B16's guard is never engaged.

Open question for the fix session - the exact path that produced the bare-parent `start` propagate is not yet pinned. Inventory shadowing is ruled out (managed key differs from the parent key). Candidates to investigate:

- Target-delta processing the mirror PARENT `cs22oh9` as an edited event (`internal/sync/target_delta.go:275` `processTargetDeltaEvent`, non-instance branch -> fetch bare source parent -> `Classify` -> drift on `start` -> propagate). This requires the mirror parent to have appeared in the target delta with a changed start, which contradicts its sequence-1/daemon-only update. Reconcile that contradiction first - it is the crux.
- Drift cross-talk from duplicate series (below).
- The source-classify path computing a spurious `start` drift on the source parent.

Duplicate-series context (likely contributing, own cleanup needed): "Breakfast and drive Mads to school" exists as ~4-5 overlapping recurring masters across both calendars - personal `ij3p7`, `6fh25ms4tqrl8ju7t1i4gmmrrl`, `raea2qb0fqjlv86had0jjhrn04`, `u1e7g45jkh0770gt55gqiahudi`; work-native `e9im6r31…` splits - all named identically, all MO,TU,WE, several propagated/deleted in the same 07:22 tick. This tangle can make one series' mirror reconcile against another's and manufacture drift. Determine whether the duplication is a precondition for the bug or independent of it.

Repro to run first (throwaway calendars, `propagate_target_edits = true`, writable source): create a weekly recurring event on the source, let it mirror, move a single occurrence on the mirror to a new time with "This event only", wait one tick. Correct outcome: the source gets a per-instance override, parent untouched. Bug repro: the source PARENT start moves and the exception is cancelled. Capture the out.log action lines and the pre/post `sequence` + `updated` on both parents.

Recovery for the live event would be a direct `events.patch` restoring the source parent's original 07:25 start (as in B16's manual recovery) then un-cancelling the exception; the user does not need this occurrence restored, so it is left as-is.

### B31 - no FullSync recovery for inherited target exceptions

`BuildInventory` drops recurring mirror instances whose `calendar-sync:source` is the inherited (parent) form, so a target-side override that the target-delta stream never delivered has no second chance. There is no automatic repair for: edits made while the daemon was stopped, edits predating process startup, anything queued when a target token 410s, occurrences quarantined by B32, or occurrences that only enter the horizon later.

Safe to defer only because B28's fix stopped FullSync from reseeding valid target tokens - without that, every periodic FullSync would open a fresh loss window and this would be mandatory rather than a recovery gap.

The fix is to keep inherited target exceptions in a separate inventory collection instead of discarding them, and run them through the same B29 membership decision during FullSync. Because the target inventory is unexpanded, those candidates are real target exceptions, not every virtual occurrence.

The one known instance of this damage (the Jul 28 exercise event) was repaired by hand: the source override was materialized directly at 09:00.

### B36 - a transient read error on the target-delta path silently drops the edit

`applyTargetDeltas` reuses B18's transient-read carve-out: a well-understood flake (`events.get` 5xx/400/404) inside `processTargetDeltaEvent` is logged, skipped, and does NOT pin the target token. The carve-out exists so one flaky read cannot replay the same delta forever.

That tradeoff is sound on the source-delta path, where the next FullSync re-lists the source event anyway. It is worse on the target-delta path, because B31 means there is no recovery scan: the token advances past the user's edit and nothing ever re-delivers it. A single transient flake at the wrong moment silently loses a mirror-side edit.

Pre-existing behaviour extended to a newly-live path rather than a new regression, but the consequence is different here and worth choosing deliberately. Options: pin the token for transient errors on the target path only (accepting a possible replay loop, bounded the same way B35's deferred batches are), or fix B31 so FullSync recovers dropped edits. B31 is the better fix because it also covers the offline-edit and 410 cases.

### B32 - reverse target cancellation is quarantined, not implemented

Deleting one occurrence of a recurring event on the MIRROR side does not propagate to the source. Target deltas carry `ShowDeleted: true`, and the revive cells in `internal/recurring/handler.go` (B20) and `internal/sync/reconcile.go` would resurrect a cancelled mirror whose source is still live. The reverse patch body omits `status`, so routing cancellations through materialization would not delete anything either.

Until this ships, every target-delta event with `status == cancelled` is quarantined: not classified, not materialized, not revived, no source write, structured warning, and consumed for token purposes. Consumed rather than pinned, so the first deletion does not head-of-line block every later target delta.

Consequences while deferred: the cancellation does not propagate; it will not retry automatically once this ships (B31 would be the recovery path); and a later source-driven FullSync may still revive it, since `BuildInventory` drops cancelled resources.

The fix is to cancel or delete the constructed source occurrence - which materializes a cancelled source exception - rather than sending an ordinary field patch, and to reconstruct the source parent mapping via the target's recurring parent plus `OriginalStartTime` for tombstones that arrive without inherited private properties.

### B33 - `EventsInstances` conflates existence probes with exhaustive listing

Both callers (`classify.go` horizon eligibility, `orphan.go`) pass `MaxResults=1` and only test `len(result) > 0`, but the method still passes `--page-all`. With `maxResults=1` that is one item per page. It is harmless today because gws's default 10-page cap bounds it, which is precisely why B28's explicit `--page-limit` was NOT applied here - doing so would turn a yes/no question into up to 1000 round-trips.

The clean shape is two methods: a bounded first-page existence probe with no `--page-all`, and a separately named exhaustive one if a caller ever needs the full instance set. Do not "fix the inconsistency" by giving this method the same treatment as `EventsList`.

### B34 - FullSync can race a concurrent target edit

Pre-existing and not observed in the wild. FullSync snapshots the source, snapshots the target inventory, then issues source-driven writes. A target edit landing between the inventory snapshot and the write can be overwritten, and whether the following target delta observes it depends on timing. B29's staged reads narrow the equivalent race on the tick path but do not address FullSync.

Closing it would need target deltas staged before FullSync writes, or conditional writes with version preconditions.

### B35 - a prolonged source-catalog outage pins the target cursor

By design, B29 refuses to make inherited-instance decisions while a source calendar's exception catalog is `Unknown`, and leaves the target token unadvanced so the edit is redelivered. Correct for a transient failure, but a sustained source-list outage means the target batch grows, one source calendar head-of-line blocks a whole target, and Google may eventually expire the pinned target token with a 410. At that point the queued edits have no recovery path until B31 exists.

Not a hot loop - preflight guarantees zero writes while blocked, and retries follow the normal tick cadence. Blocked duration should be made observable.

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

### B27 - a fast-track FullSync can still strand a just-imported feed event (B26 residual)

B26's fix moved feed imports off the startup/periodic FullSync onto the per-tick path. One path remains: `runTick` runs `runFeeds` then `Reconciler.Tick`; if that Tick sets `NeedsFullResync` (a 410 GONE on any source/target), `scheduler.requestFastTrackFullSync` fires an immediate FullSync on the next loop iteration, which full-lists the calendar the feed just wrote to seconds earlier - the same not-read-your-writes race, so the token can leap past the fresh event and strand it until the next successful full re-sync.

Rare: needs a `Changed` feed import AND a 410 in the same tick, plus Google's full list lagging that one event. Self-heals on the next full re-sync.

Fix options: (a) if `runFeeds` reported a `Changed` import this tick, defer any fast-track FullSync by one `poll_interval` so the write settles before the full list (needs `runFeeds` to report changed, and `runTick` to tell the scheduler); (b) hold/skip the syncToken reset in FullSync for any source written to within the last few seconds (touches the token-advancement path). (a) is smaller and localizes the change to the daemon. Needs a test driving feed-changed-tick + NeedsFullResync and asserting the fast-track doesn't strand the event.

## Fixed

### B28 - gws truncated every long list at 10 pages; two-way sync was dead in production (CRITICAL)

Surfaced from a user report: "why isn't the exercise event on my personal calendar updated after I updated the coreweave version?" The 🧘/🏃 series lives on Personal (weekly TU 08:45-09:30); the user dragged the Jul 28 occurrence on the CoreWeave MIRROR to 09:00. With `propagate_target_edits = true` and a writable source, B17's target-delta phase should have pushed that back. Nothing happened, across hours of ticks.

Root cause: `gws --page-all` stops at `--page-limit`, which **defaults to 10**. `internal/gws/events.go:EventsList` never passed the flag. At `maxResults=250` that silently capped any list at 2500 events, and the wrapper returned the partial prefix as SUCCESS - `nextSyncToken` empty, no error.

`seedTargetSyncTokens` lists the entire target calendar with no time bounds (sync tokens are incompatible with `timeMin`/`timeMax`). Production sizes: `tsaleh@coreweave.com` 3470 events / 14 pages, `me@tammersaleh.com` 10244 events / 41 pages. Both blew past 10. gws stopped at page 10; that page carried `nextPageToken` and no `nextSyncToken`. The seed stored `""`, and `runTargetDeltaPhase`'s `if !ok || token == ""` skipped the target on every tick.

**B17's entire target-delta phase had been dead since v2.4.0 shipped**, for both targets, silently. Evidence: daemon debug log showing `sync.seedTargetSyncTokens: seeded ... token_present:false` for both targets, and tick traces making exactly two `gws.EventsList` calls (the two source deltas) and zero target lists. Confirmed independently by capturing a sync token by hand with the daemon's exact parameters, editing the mirror, and watching Google return the edit in the delta - the API was fine, the wrapper was not.

Why it wasn't obvious sooner: the source-side classify path ALSO detects mirror drift and propagates, which covers any target edit whose source event appears in the source list. It does not cover a recurring occurrence with no materialized source exception - that never appears in a `singleEvents=false` source list, at any FullSync, ever. So most target edits still worked, eventually.

Fix, in three parts:

1. `EventsList` and `CalendarListList` pass an explicit `--page-limit` (`gws.MaxPagesPerList`, 1000 = 250k events) and run `assertPaginationComplete`: if the terminal page still advertises `nextPageToken`, return `ErrIncompletePagination` **with no data**. A successful return now means the collection is complete. The check is deliberately about pagination completeness only, not sync-token presence - whether Google issues a token depends on the query. `EventsInstances` is excluded on purpose: both callers are `MaxResults=1` existence probes where truncation is the expected outcome and an exhaustive walk would cost one round-trip per instance.

2. `seedTargetSyncTokens` seeds **only when a target has no usable token**. It previously reseeded unconditionally, so a periodic FullSync landing between a mirror-side edit and the next tick seeded past the edit and lost it - on every FullSync, not just once. Skipping also drops a full unbounded re-list of every target calendar per FullSync.

3. An empty token from a successful seed is now a seed error, never stored. Map invariant: an entry in `targetSyncTokens` exists only when it holds a usable token; there is no "present but empty" state. A tokenless target warns once on the transition (latch reset on recovery) instead of staying silent - silence is what let this hide for months.

Note the old SPEC/CLAUDE.md claim that Google "omitted nextSyncToken on a long delta" was wrong. A terminal page without a sync token means truncation, not a large calendar.

### B29 - inherited recurring mirror instances clobbered genuine target edits (CRITICAL)

Found while fixing B28, and the reason B28's fix could not ship alone. Even with the target token working, the exercise event above would have been REVERTED rather than propagated.

`processTargetDeltaEvent` took the B17 Phase 2 `materializeSourceOverride` path only on a **404** from `events.get` on the constructed source-instance ID. But Google returns **200 with a synthesized virtual occurrence** when the series has no exception at that slot. So control fell through to `Classify` -> `recurring.applyDriftMatrix`, where `IsInheritedRecurringInstance` is true (Google copies the parent's extendedProperties onto a user-created override, so `calendar-sync:source` carries the source PARENT id), the recomputed `MirrorDrifted` is true, and the `case signal.MirrorDrifted:` cell source-wins. Reproduced in a test before fixing: `action=patch reason=source_updated conflict=inherited_source_won`, zero writes to the source.

The `isInherited` guard itself is correct and stays - it exists for B15, where a freshly auto-materialized mirror instance's bootstrap fields must not be propagated over a pre-existing source override. The defect is that `isInherited` alone cannot separate bootstrap junk from user intent.

Discriminators evaluated and rejected, all against live data:

- `events.get` 200 vs 404 - Google returns 200 for virtual occurrences. This is the bug itself.
- `etag` - a real override and its parent share the SAME etag.
- `sequence` - a virtual instance reported `sequence` 2 while its parent reported 1.
- `start != originalStartTime` - catches only moved exceptions; misses title/end/status changes.
- `signal.SourceChanged` - an existing test deliberately pins a genuine source exception with `SourceChanged == false`.
- Comparing the source instance's managed fields to the parent's projection for that occurrence. This was the near-miss: it fixes the "only detects moves" objection. Killed by real data on this very series - the Jul 14 exception is **cancelled** with `start == originalStartTime`, and `status` is deliberately not a managed field (adding it would force a checksum migration across every v3 mirror). The comparison reports "no source intent" and would reverse-propagate onto a deliberately-cancelled occurrence, resurrecting it.

The sound signal is **membership in a complete unexpanded `events.list(singleEvents=false)`**: it contains real exceptions and never manufactures virtual ones. Live proof on the exercise series - exceptions exist for Jun 16, Jun 23, Jun 30, Jul 7, Jul 14, Jul 21, Aug 4 and Aug 11, and Jul 28 is absent.

Fix: a per-source-calendar exception catalog (`readiness: Ready|Unknown`, proven coverage window, `byID`, `byParent`) built from the complete source list at FullSync and maintained by a staged overlay from each successful source delta. Cancelled exceptions count as Present. Lookup returns `Present | Absent | Unknown | OutOfScope`; readiness is per-calendar because a failed or truncated source list means any exception anywhere in it might be unknown. The inherited branch then decides:

```
Present                            -> inherited source-wins bootstrap (unchanged, B15 preserved)
Absent + GET 200 virtual + drift   -> materialize the source override (target wins)
Absent + GET 200 + no drift        -> metadata bootstrap only, no source write
Absent + GET 404                   -> terminal skip, no source write
Unknown                            -> zero writes for the batch, target token pinned
OutOfScope                         -> no source write, horizon policy
```

Tick was restaged to target-delta READS -> source-delta reads + catalog overlay -> whole-batch preflight -> target writes -> source writes -> token commit. Target-read before source-read is load-bearing: it puts the source view as close as possible to the reverse write, so any source override that existed at target-read time is guaranteed to be in the catalog. Batch preflight means one `Unknown` blocks the whole target with zero writes rather than rewriting the safe prefix every 60 seconds while the token stays pinned.

Design reviewed with Codex, which rejected a first, smaller version of the fix and supplied the seed-preservation and cancellation-quarantine guards.

### B30 - `[settings].log_level` and `log_format` never reached the logger

`cmd/cli.go` built the logger from CLI globals only, before config was loaded. Both settings were parsed, defaulted and validated, then ignored. launchd passes no log flags, so the production daemon always ran at the built-in default no matter what config.toml said. The `--log-level` help text already claimed "Overrides settings.log_level", so config-as-fallback was the documented intent all along.

This is why B28 stayed invisible: the one state that disabled two-way sync logged at DEBUG, and DEBUG was unreachable in production.

Secondary defect in the same code: invalid values (`--log-level=trace`, `--log-format=yaml`) parsed fine and silently fell back inside `output.NewLogger` instead of failing as usage errors.

Fix: `Globals.LogLevel` / `LogFormat` become `*string` with kong `enum` and no `default:` - nil means "the invocation said nothing", so absent is distinguishable from empty and a bad value is a usage error. Level and format resolve independently (`--log-level=warn` still honors `settings.log_format`). A bootstrap logger still exists for configless commands and config-load failures; `loadConfig` replaces `rt.Logger` after a successful load, before any gws client, reconciler or feed runner captures it. `cfg.Settings` is not mutated with CLI values - the config represents the file, the logger represents the invocation.

### B37 - reverse patches sent `"transparency": ""` and got HTTP 400 (CRITICAL)

Caught by driving the real bug end-to-end against live calendars immediately after v2.6.2 installed, which is the only reason it was found: no unit test covered it and the code path had never executed in production.

Test: the exercise series has no source exception on Aug 18, and its mirror instance was inherited-form - exactly the B29 shape. Moving the mirror to 10:00 should have created a source override. Instead every tick logged:

```
sync.targetDelta: process failed ... target-delta classify
me@tammersaleh.com/k6dsed..._20260818T154500Z:
api_invalid_request during events.patch (HTTP 400): Bad Request
```

Cause: `BuildSourceOverridePatchBody` and `BuildPropagatePatchBody` read `live.Transparency` / `live.Visibility` RAW, while `DriftedFieldNames` reads the same fields through `ManagedFieldsFromEvent`, which runs `normalizeTransparency` / `normalizeVisibility`. Google OMITS these fields when the value equals the default, so a raw read yields `""` and the patch body carried `"transparency": ""` - not a member of the enum. Reproduced by hand: the identical body with `transparency` omitted succeeds, with `"transparency": ""` returns 400 `badRequest`.

The comparison layer and the write layer disagreed about what an omitted enum means. The write layer was wrong.

Latent since v2.5.0 (`BuildSourceOverridePatchBody` shipped with B17 Phase 2) but never reachable, because B28 had the target-delta phase switched off for that entire period. B28/B29 made it live on the first tick.

Fix: both builders send the NORMALIZED value. Sending the explicit default rather than omitting the field is required, not cosmetic - on the propagate path the field is in the drifted set by definition, so omitting it would leave the drift unresolved and every tick would retry forever. `TestPatchBuilders_AgreeWithDriftComparison` pins the two layers against each other so they cannot drift apart again.

Behaviour while broken was correct-but-stuck rather than destructive: the 400 is non-transient, so the batch pinned the target token and retried, and no calendar data was corrupted. That is the designed failure mode working.

### B26 - a FullSync that races a fresh feed import stranded the imported events

Surfaced live during the v2.6.0 install: the tripit feed imported the rebooked Jul-13 flights onto Personal, but they didn't mirror to CoreWeave for ~10 minutes (and in principle up to `full_sync_interval`, 24h).

Cause: FullSync does a FULL `events.list` per source and resets that source's in-memory syncToken to the list's `nextSyncToken`. Google's full `events.list` is not strictly read-your-writes - an event the importer wrote seconds earlier (into Personal, which is also a pdir source) can be absent from the full-list body while the returned token already sits past it. The token advances past the event, and since incremental deltas only move forward from the token, it never reappears until the next FullSync/restart. Incremental ticks don't have this: a tick's delta and its `nextSyncToken` are mutually consistent, so a briefly-lagged write is caught by the next delta. The install's several daemon restarts (cask relink, config WatchPaths, manual kickstart) made the collision easy to hit.

Fix (v2.6.1): run the feed importer on the per-tick path ONLY, never on the startup or periodic FullSync. `daemon.runFullSync` no longer calls `runFeeds`; `runTick` still does (before the tick's reconcile, preserving single-tick propagation). Feed writes now happen on the incremental path, whose token can't leap past them. At startup the feed import lands on the first tick (~one `poll_interval` later) instead of the startup FullSync - a trivial delay for a race-free path. Pinned by `TestDaemon_FeedsRunOnTickNotFullSync` (asserts the startup FullSync records zero feed runs, and the tick's feed run precedes its incremental list). The live workaround before the fix shipped was a daemon restart once the feed events had settled.

Residual (tracked as B27, Open): a FAST-TRACK FullSync still can. If a tick's `runFeeds` makes a `Changed` import and that same tick's `Tick()` sets `NeedsFullResync` (a 410 GONE on any source/target), the scheduler fires an immediate FullSync on the next iteration, which full-lists the just-written source seconds later - reopening the same race. Much rarer than the fixed path (needs a Changed import AND a 410 in the same tick), and self-heals on the next successful full re-sync, but not fully closed.

### B25 - daemon `events.delete` always failed HTTP 500 on read-only cwd

Surfaced live while cleaning up ~660 stale Reclaim leftover events on the source calendar: the daemon dutifully pruned the corresponding mirrors, but every prune logged `backend_error during events.delete (HTTP 500): Failed to create output file: Read-only file system (os error 30)` and retried it the full 5 attempts. 148 such 500s in one prune pass.

Root cause: gws renders a 204 No Content (what `events.delete` / `calendars.delete` get on success) as an empty "downloaded file" and writes `download.html` into the subprocess cwd. The launchd plist sets no `WorkingDirectory`, so the daemon's cwd defaults to `/`, which is read-only on modern macOS - the file write fails and gws exits non-zero. The Calendar API delete itself lands first (server-side), so the prune actually completed; the error was cosmetic-but-noisy and made every cancellation/orphan delete look like a failure.

The gws client already had `WithWorkDir` built and tested (`internal/gws/client_test.go:TestWithWorkDir_HonoredByExecute`) for exactly this stray-file problem, and the e2e harness passed it - but the production constructor `cmd/gws.go:gwsClient()` never did. The bug was a missing option, not missing capability.

Fix: `gwsClient()` now builds one options slice and passes `WithWorkDir(gwsScratchDir())`. `gwsScratchDir` prefers the user cache dir (`~/Library/Caches/calendar-sync`), falls back to a fixed subdir under the system temp dir, and returns "" (inherit-cwd) only if neither is creatable. Collapsing the old logger/no-logger return-branch split into a single accumulated slice removes the shape that let the option go missing. Covers every gws subprocess (daemon + any CLI invocation run from a read-only cwd), so no plist change was needed.

Verified by `cmd/gws_test.go`: `TestGwsClient_WiresScratchWorkDir` (fake gws on PATH reports its cwd; asserts production `gwsClient()` runs it in the cache scratch dir - the cross-layer wiring guard), `TestGwsScratchDir_PrefersCacheDir`, and `TestGwsScratchDir_FallsBackToTempWhenCacheUnusable` (deterministic: plants a file at the preferred path so MkdirAll fails and the temp fallback fires; uid-independent).

Operational note for bulk gws work: running `gws` writes (delete/insert/patch) from a **backgrounded/detached** shell hangs indefinitely - the keyring is unreachable without the foreground session, so the first call blocks with no gws process and no error. Run gws mutations in the foreground (small batches if needed to stay under the harness's auto-background threshold).

### B24 - moved recurring instances became permanently unsyncable (CRITICAL)

Surfaced live: user rescheduled the `Lunch & Reading` 6/10 instance on personal (source) back to 11:30; the cw mirror stayed at 12:00. The daemon was healthy and ticking, the source change WAS delivered in the incremental delta, but every tick logged `skip(instance_unmaterializable)` for that instance.

Root cause: the recurring handler located the mirror instance via `events.instances?eventId=<mirrorParent>&originalStart=<src.originalStartTime>&...`. Google's `originalStart` filter does NOT return an instance once it has been moved off its native recurrence slot (`start != originalStartTime`). Verified live: the un-moved 6/9 instance matched the filter; the moved 6/10 instance returned zero in every tz format, while a plain timeMin/timeMax window (no filter) returned it. The repair path re-fetched the source parent, force-rewrote the mirror parent recurrence, and retried the SAME filtered lookup - still zero - so it skipped forever.

Self-perpetuating: a mirror instance becomes a moved exception precisely because a prior sync moved it successfully. So any recurring instance ever moved was permanently frozen against future source edits.

SPEC.md encoded the wrong mental model ("empty = the mirror parent's recurrence is stale"). The recurrence was fine; the filter just doesn't match moved exceptions.

Fix: locate by constructing the deterministic instance ID `<mirrorParent.id>_<occurrenceKey>` (occurrenceKey = the substring after the last `_` of the source instance ID; identical across both series because the mirror parent copies the source DTSTART grid) and fetching via `events.get`. A 404 (not an empty list) is the repair trigger; retry GET against the repaired parent; a second 404 is the genuine `instance_unmaterializable`. `events.get` returns moved exceptions, cancelled instances, and 404s out-of-series occurrences cleanly. A post-get sanity check aborts if the located instance names a different `RecurringEventID` (constructed-ID collision). Live-verified against the real calendars before and after. See `doc/plans/moved-exception-locate-fix.md`.

Known follow-up (pre-existing, not introduced here): `patchMirrorWithChecksum` drops the post-main parent resource if the checksum follow-up patch fails, so the force-rewrite repair path can still lose B19 inventory propagation on that narrow sub-path. Shared by every checksum write path; deserves its own fix.

### B17 - target-edit propagation lagged 24h instead of one tick

Surfaced live during the personal->cw rollout: user edited the `Lunch & Reading` 5/20 instance on the cw target by +30 min. The daemon's incremental ticks were idle for hours because nothing on personal had changed, and a manual `calendar-sync run --pair personal-to-work` (which does a full source-list) was needed to catch it.

The watch daemon's per-tick reconciliation was driven by Google's `events.list` `syncToken` on the SOURCE calendar only. A pure target-side edit produces zero change on the source, so the source's syncToken delta returns nothing - drift is never detected until the next FULL re-sync (default 24h).

Phase 1 fix adds a per-target `targetSyncToken` stream and a target-delta phase that runs BEFORE the source-delta phase on every tick:

- New state: `Reconciler.targetSyncTokens map[string]string`. Maintained only for targets that have at least one pdir with the effective two-way-sync gate open (`pd.SourceWritable && pd.PropagateTargetEdits`).
- FullSync seeds the token via `seedTargetSyncTokens` BEFORE rebuildInventories. Seeding-after would leave an edit landing in the gap invisible to both inventory (already snapshotted) and the seeded token (which starts after the edit).
- Tick runs `runTargetDeltaPhase` BEFORE the source-delta classify. Phase ordering matters: a source-delta-driven mirror rewrite running first could clobber a target edit before target-delta lists it.
- For each delta event, dispatch parses `calendar-sync:source` to find the SINGLE owning pdir (matched by source-tuple == pd.source AND target == event.target AND effective gate open). Stray mirrors and non-mirrors skip silently.
- Recurring-instance dispatch handles both managed form (calendar-sync:source already has `_<UTC>` suffix) and inherited form (parent's id, no suffix - the case that bit B16). Inherited form computes source_instance_id by appending the suffix parsed from the mirror's own id via regex `_\d{8}T\d{6}Z$`.
- 410 GONE clears the per-target token and surfaces `NeedsFullResync` so the next FullSync re-seeds.

Phase 1 (v2.4.0) deferred the mirror-only-override case: when the source-instance `events.get` returned 404, the dispatch emitted `skip(reason=mirror_only_override)` and stopped. Phase 2 (v2.5.0) promotes that branch to a real propagate.

Phase 2 fix:

- New helper `mirror.BuildSourceOverridePatchBody(*gws.Event) *gws.PatchEvent`. Carries the mirror's full managed fields (summary/description/location/start/end/transparency/visibility) with the description trailer stripped and explicit-clear semantics for empty strings. Recurrence is omitted by construction - the helper takes `*gws.Event` (not a "drifted fields" slice) so there's no mechanism for a future change to opt recurrence in. This is the structural B16 guardrail: a per-instance patch carrying recurrence gets reinterpreted by Google as a parent-level update and silently corrupts every future occurrence.
- `Reconciler.materializeSourceOverride` replaces the recurring-instance 404 skip branch. Calls `events.patch(source_calendar, source_instance_id, BuildSourceOverridePatchBody(mirror))` to create the override, then rewrites the mirror via `BuildInstancePayloadWithTimeZone(post-patch source)` + checksum follow-up so subsequent ticks classify as `unchanged`. Inventory updates at the per-instance source tuple (the override now exists at `(tuple.CalendarID, source_instance_id)`; the mirror's `calendar-sync:source` upgrades from inherited form to managed form as part of the rewrite).
- Outcome shape changed: `skip(mirror_only_override)` -> `propagate(mirror_only_override)`. Reason name preserved for stream continuity.
- Source-side patch failures keep the `targetSyncToken` pinned (write errors are never transient per the B18 matrix), so the next tick re-delivers the user's edit.

Test pins (`internal/sync/target_delta_test.go`):
- Phase 1 (v2.4.0):
  - `TestTargetDeltaPhase_NonRecurringEdit` - non-recurring target edit propagates.
  - `TestTargetDeltaPhase_RecurringParentEdit` - recurring parent edit propagates.
  - `TestTargetDeltaPhase_ManagedInstanceEdit` - managed-form recurring instance edit propagates via the recurring handler.
  - `TestTargetDeltaPhase_NonRecurringSourceOrphanEmitsSkip` - non-recurring 404 still emits `skip(source_orphan)`.
  - `TestTargetDeltaPhase_SelfWriteSuppression` - post-write delta of an unchanged mirror classifies as `unchanged`, no extra patches.
  - `TestTargetDeltaPhase_InheritedAutoMaterializedNoUserEdit` - inherited auto-materialized instance routes through B15's bootstrap path (`inherited_upgrade`), no source-side patch.
  - `TestTargetDeltaPhase_NotMirror` / `TestTargetDeltaPhase_NoOwningPdir` - skips silently.
  - `TestTargetDeltaPhase_NonWritableTarget` - target with no writable-source pdir is never listed.
  - `TestTargetDeltaPhase_410Recovery` - clears the target token, surfaces NeedsFullResync.
  - `TestTargetDeltaPhase_TokenAdvancementOnSuccess` / `TestTargetDeltaPhase_TokenStaysOnError` - conditional advancement.
  - `TestSeedTargetSyncTokens_RunsBeforeInventoryRebuild` - phase ordering pinned via the recorded-call log.
  - `TestTickPhaseOrdering_TargetBeforeSource` - target-delta runs before source-delta in `Tick`.
  - `TestTick_PropagatesTargetEdit_OneTick` - regression: full-sync, then user edit on target, then Tick - propagate within a single tick.
- Phase 2 (v2.5.0):
  - `TestTargetDeltaPhase_MirrorOnlyOverride_PromotesToPropagate` - inherited-form recurring instance edit, source 404, daemon materializes the override and emits `propagate(mirror_only_override)`. Pins the source-patch body's `Recurrence=nil` (B16 guardrail).
  - `TestTargetDeltaPhase_MirrorOnlyOverride_DoesNotIncludeRecurrenceInPatch` - integration-level B16 guardrail: regardless of the mirror's live `Recurrence`, the source-side patch body's `Recurrence` field stays nil.
  - `TestTargetDeltaPhase_MirrorOnlyOverride_PatchFailureDoesNotAdvanceToken` - source-patch failure pins `targetSyncToken` so the next tick re-delivers.
- Helper-level tests (`internal/mirror/drift_fields_test.go`):
  - `TestBuildSourceOverridePatchBody_IncludesAllManagedFieldsExceptRecurrence`
  - `TestBuildSourceOverridePatchBody_NeverIncludesRecurrence` (table: nil / empty / single RRULE / RRULE+EXDATE)
  - `TestBuildSourceOverridePatchBody_StripsTrailerFromDescription`
  - `TestBuildSourceOverridePatchBody_ClearsEmptyFields`

SPEC §"In-memory state", §"Daemon lifecycle: startup" (step 5a), §"Daemon lifecycle: per-tick reconciliation" (step 0), §"What full re-sync catches", §"Mirror-only recurring instance override propagation" (replaces the prior "Limitation" section), and the §"Edits flow both ways" carve-out updated with Phase 2's behavior. The "Out of scope" Phase 2 deferral bullet was removed.

### B19 - stale inventory after partial recurring-instance repair-path failure

Surfaced during B18 code review (pre-existing; B18's transient tolerance turned the failure mode from "syncToken pinned forever" to "spurious double-writes per tick until next FullSync"). The recurring handler's `locateMirrorInstance` repair path fires up to 3 API calls: source-parent `events.get`, `forceRewriteMirrorParent` (2 `events.patch` writes), then a retry `events.instances`. If step 1 and 2 succeeded but step 3 returned a transient error, `Handle` returned `Result{}, err` and `classifyRecurringInstance` skipped the inventory update on the error path. The post-rewrite mirror parent was dropped on the floor; the next tick saw the stale inventory entry and re-fired the force-rewrite.

Fixed by propagating the post-write mirror parent through the Result even on the error path, and having the sync layer apply the inventory updates before returning the error:

- `internal/recurring/handler.go` `Handle`: capture `parentAfterRepair` BEFORE the `locateMirrorInstance` error check. On error, return `Result{PostWriteMirrorParent: postWriteMirrorParent}, err`. Also on the `reconcileInstance` error path: surface `postWriteMirrorParent` if `res.PostWriteMirrorParent` is nil. The behavior of the success path is unchanged.

- `internal/sync/classify.go` `classifyRecurringInstance`: apply inventory updates from `res.PostWriteMirrorParent` and `res.PostWriteMirrorInstance` BEFORE the err check. The Outcome emit only fires on success; only the inventory state mutation moved earlier.

The error itself still bubbles up to `runClassifyLoop` where B18's transient-read classifier decides whether to log+skip (advancing the token) or fail the pdir (gating the token). Either way the inventory is now consistent with the writes that did complete.

Test pins:
- `internal/recurring/handler_test.go`: `TestHandle_Step2_RepairSucceedsThenRetryFails_ReturnsParentAfterRepair` - asserts Result.PostWriteMirrorParent is non-nil with the post-rewrite Recurrence value when the retry events.instances errors.
- `internal/sync/classify_test.go`: `TestClassify_Step2_RecurringDelegation_PartialRepairOnError_UpdatesInventory` - integration test asserting the inventory entry reflects the post-rewrite mirror parent (new Recurrence rule from source) after Classify returns the underlying error.

SPEC §"Zero-result instance lookup" updated with the partial-repair-error contract.

### B23 - drift signal never compared source-now to mirror-now directly

Surfaced 2026-05-04 during B20 investigation. The 5/11 Lunch & Reading instance sat in a stable state where source.start = 11:30, mirror.start = 11:00, and the daemon classified every tick as `reason: unchanged`.

`mirror.ComputeDriftSignal` built two signals from stored bookkeeping on the mirror:

- `source_changed = source.Updated > mirror.calendar-sync:source_updated`
- `mirror_drifted = sha256(canonical(mirror.<managed fields>)) != mirror.calendar-sync:checksum`

Both fire false when "neither side changed since the last write." But that's not the same as "source and mirror agree right now." The daemon could end up at a state where stored bookkeeping says clean (e.g., a managed-field-no-op patch updated `source_updated` and recomputed `checksum` over the post-write event whose managed fields hadn't changed) and a separate later edit drove the source's actual fields away from the mirror's. With only the two stored-bookkeeping signals, that divergence was invisible. The mirror sat stale forever.

B20 was a specific symptom on the Status field (not in `ManagedFields`, so flipping confirmed↔cancelled produced no checksum drift). B23 is the structural fix.

Fix: added `FieldsDisagree` to `mirror.DriftSignal` and a `desired *gws.Event` param to `mirror.ComputeDriftSignal`. The new signal compares the source's current managed fields to the mirror's current managed fields directly via the existing `mirror.DriftedFieldNames` helper, which already implements the canonical-form rules used by the checksum. Both production callers (`sync/reconcile.go`, `recurring/handler.go`) build `desired` anyway and now pass it.

`mirror.Classify` got one new behavioral cell: `!SourceChanged && !MirrorDrifted && FieldsDisagree` -> `ActionPatch / ReasonStaleBookkeeping`. The patch path is identical to the existing source_updated cell (rewrite mirror from source, run checksum follow-up). No `Conflict` label - the daemon doesn't have evidence of a user-edit conflict, just evidence of bookkeeping divergence. New reason gives operators a machine-readable signal that something in the mirror's history left stored bookkeeping inconsistent. Other matrix cells are unchanged: when `MirrorDrifted=true` the existing target_edited / newer-wins-conflict cells handle the case regardless of `FieldsDisagree` (the user-edit signal is the authoritative cause); when `SourceChanged=true` the existing source_updated cell is correct (a real source bump trumps stale-bookkeeping inference).

Migration semantics unchanged: `reconcileMigration` still overrides `signal.MirrorDrifted` with its live-vs-desired check before falling through to `Classify`, so the migration cells continue to behave per SPEC. The fallthrough cell (SC=T, MD-overridden=F) routes through the existing `source_updated` branch.

Test pins:

- `internal/mirror/drift_test.go`: `TestComputeDriftSignal_FieldsDisagreeWithCleanBookkeeping` and `_FieldsAgreeOnAlignedState` and `_NilDesiredKeepsFieldsDisagreeFalse` for the signal; `TestClassify_StaleBookkeepingCell`, `_FieldsDisagreeWithSourceChanged`, `_FieldsDisagreeWithMirrorDrifted` for the matrix.
- `internal/sync/classify_test.go`: `TestClassify_Step8_StaleBookkeeping_PatchesFromSource` integration test.
- `internal/recurring/handler_test.go`: `TestHandle_Step3_StaleBookkeeping_PatchesFromSource` integration test.

A test fixture inconsistency was unmasked along the way: `makeNonRecurringSource` set source.Description="" while `makeCleanCurrentMirror` constructed mirror.Description="<summary>\n\n---\nSource:..." - the mirror's body had a `<summary>` prefix that BuildPayload would never produce from an empty source description. Pre-B23 the checksum-based signal missed this divergence; post-B23 the new FieldsDisagree caught it. Aligned the fixture by setting `Description: "Standup"` on `makeNonRecurringSource`.

Codex flagged a `MirrorDrifted=true && FieldsDisagree=false` quadrant (mirror's checksum is stale but its current fields happen to match source's) - the existing target_edited path produces an empty propagate body in that case, which Calendar API may reject. Out of scope for B23; documented as a future refinement.

### B20 - cancelled mirror with confirmed source classifies as `unchanged` forever (CRITICAL)

Surfaced 2026-05-03 when the user reported missing future "Lunch & Reading" instances on the work mirror. Investigation found 5 mirror instances at `status=cancelled` while their source instances were `status=confirmed`. The daemon was emitting `reason: unchanged` for each instance every tick, never reviving them.

Root cause: `Status` is not in `mirror.ManagedFields` (Description / End / Location / Recurrence / Start / Summary / Transparency / Visibility). The drift checksum hashes only managed fields, so a cancelled mirror with the same managed fields as its source produces the same checksum the daemon stored when the mirror was last written. Combined with `source.Updated == calendar-sync:source_updated`, the drift signal fires `source_changed=false && mirror_drifted=false` -> `mirror.Classify` returns `ActionSkip / ReasonUnchanged`. The same hole existed in both the recurring-instance handler and the non-recurring `reconcileNormal` path.

How a mirror got stuck cancelled:

1. Source had `transparency=transparent` OR `responseStatus=declined/tentative` OR `status=cancelled` at some past time.
2. `recurring.Handler.cancelMirrorInstance` patched the mirror with `status=cancelled`. Per the function's intentional design, NO checksum follow-up ran ("the cancellation does not write a managed field, so the existing checksum remains accurate").
3. Source flipped back to a syncable state. Source's `updated` bumped.
4. Daemon's next reconcile fired the patch path which writes managed fields, recomputes checksum, updates `source_updated`. Status was never touched because the patch body only carried managed fields.
5. From there on: managed fields matched stored checksum, source_updated matched source.Updated, drift signal said everything was clean. Mirror was cancelled forever.

Fixed by adding an explicit revive cell ahead of the four-way drift matrix in both `recurring.Handler.applyDriftMatrix` and `sync.Classifier.reconcileNormal`: when `mirrorInstance.Status == EventStatusCancelled` (recurring) or `mirrorEvent.Status == EventStatusCancelled` (non-recurring) and the source has reached classify - meaning steps 3-7 already cleared it as syncable - patch with the full managed-field payload plus `status=confirmed`, then run the standard checksum follow-up. Outcome: `Action=insert / Reason=source_updated`, symmetric with `insert.go`'s post-409 `reviveCancelledMirror`. Status stays out of `ManagedFields` (option B in the original sketch was rejected because it would force a checksum migration across every existing v3 mirror).

SPEC §"Step 8 / Mirror exists, status=cancelled" documents the new cell. Tests in `internal/recurring/handler_test.go` (`TestHandle_Step3_ConfirmedSourceCancelledMirror_Revives`) and `internal/sync/classify_test.go` (`TestClassify_Step8_CancelledMirrorWithSyncableSource_Revives`) pin the revive shape and post-write inventory state. Existing tests for the cancellation cells (source-cancelled-with-cancelled-mirror returns skip-unchanged) still pass - the revive only fires when the source is syncable.

The structural follow-up - making drift detection compare source's current managed fields to mirror's current managed fields directly, rather than relying on stored bookkeeping - is queued as B23. B20's fix handles the Status-specific symptom; B23 catches the same class of drift on any other field.

### B22 - HTTP 410/404 on classify-path events.delete fails the pdir every tick

Surfaced 2026-05-04 during B18 monitoring. The work-personal pair was failing every tick with:

```
delete mirror me@tammersaleh.com/cs23qvnk...: api_gone during events.delete (HTTP 410): Resource has been deleted
```

Root cause: `internal/sync/classify.go`'s `deleteOrSkip` issues `events.delete` against a mirror that was already deleted server-side - typically a cascade from a parent delete, a user-side manual delete that the daemon hadn't yet observed, or a mirror that was tombstoned by a prior cleanup. Calendar API returns HTTP 410 (`api_gone`) or HTTP 404 (`api_not_found`) for these. The classify path bubbled both up as fatal errors, failing the pdir, gating the source's syncToken, and (with B18 in place) producing a steady stream of failed ticks rather than a fast-track FullSync loop.

This is the same bug shape as B14 - which fixed the orphan walker's `events.delete` to carry on for both 404 and 410 - but the classify path's `deleteOrSkip` was never updated. SPEC's intent for the delete-or-skip cells (source_cancelled / declined / tentative / transparency_transparent / outside_horizon) is "the mirror should not exist after this operation"; if the mirror is already gone, the intent is satisfied.

Fixed by mirroring B14's pattern in `classify.go:257`: catch `gws.ErrAPINotFound` and `gws.ErrAPIGone` from `EventsDelete`, prune the inventory entry, emit the standard delete Outcome. Test pin: `TestClassify_DeleteOrSkip_AlreadyGoneCarriesOn` (parameterized over both error shapes). Verified pre-fix the test fails with the exact error string the live daemon was logging.

### B18 - one flaky source event pins the syncToken, daemon falls into back-to-back FullSyncs

A single recurring source event (TARS Office Hours) intermittently failed its horizon-eligibility lookup with HTTP 500 from `events.get`. Each tick: classify errored on that event, the pdir was marked failed, the conditional-advancement gate kept the source's syncToken pinned, the next tick saw an empty token and triggered `NeedsFullResync`, the scheduler ran a fast-track ~24-minute FullSync, the FullSync re-hit the same flake. The daemon ran 4 back-to-back FullSyncs in ~50 minutes with zero writes and zero data risk - just CPU and quota burn.

Surfaced live during the v2.1.4 daemon's first hour. Root cause is the strict "any per-event classify error fails the pdir" rule conflating two error classes: write failures (mirror state may be partially updated) versus read flakes that don't represent state mutation. The strict rule is correct for writes and necessary to preserve idempotency. For read flakes it's overkill: the next tick or FullSync re-evaluates the event, and Calendar API hiccups on `events.get`/`events.instances` are common enough that one flaky event shouldn't pin the entire pdir.

Fixed by carving out a narrow transient-read class in the classify loop. `internal/sync/transient.go` defines `isTransientClassifyReadError` over a (op, code) matrix:

- `events.instances` + {`backend_error`, `api_invalid_request`, `api_not_found`}
- `events.get` + {`backend_error`, `api_not_found`}

`events.get` + `api_invalid_request` (400) intentionally stays fatal: a request-shape rejection there is more likely a programmer bug than a Google quirk. Write ops, rate-limit, auth, forbidden, 410-gone, network, context-canceled, and the post-409 `events.get` inside insert recovery (marked via `errInsertCollisionRead` so the helper can distinguish it from a standalone read) are all explicitly fatal. Any fatal error in a loop still pins the token; transient errors only count as "skip and continue" when nothing else broke.

`runClassifyLoop` (`internal/sync/reconciler.go`) checks the helper after the warn log; the log line carries a `transient` field so an operator can grep for the underlying flakiness. SPEC.md §"Per-event transient read tolerance" documents the matrix and reasoning. Test pins live in `internal/sync/transient_test.go`: helper unit tests for every matrix cell, integration tests at the Tick level for transient skip + token advance, write failure stays fatal, post-409 lookup stays fatal, context errors stay fatal, mixed transient+fatal still fails the pdir, rate_limited stays fatal.

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
