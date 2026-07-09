# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`.

## Where the project stands

calendar-sync is feature-complete on the planned scope. Recent work history:

000. **iCal feed importer (`[[feeds]]`)** - SHIPPED as **v2.6.0** (merged to main, released, installed; daemon running it). Six layers (`internal/ical` parser, `internal/feed` fetcher, `internal/feedimport` importer + Runner, `[[feeds]]` config, daemon/cmd wiring, docs), each red-green with before+after code review plus a final full-diff review. Design in `doc/plans/ical-importer.md`.

    **Why it exists:** the travel events on the calendars came from Navan's OAuth Google-Calendar integration, which is DISCONNECTED (16 stale `navan*` events on CoreWeave, frozen at a pre-rebooking itinerary, mirrored to Personal). Navan's OAuth calendar write appears org-disabled (only the read-only feed link shows in settings). The importer polls the TripIt iCal feed and writes into Personal; the `personal-to-work` pair mirrors onward to CoreWeave.

    **Live config:** a `[[feeds]]` entry (`name = "tripit"`, inline `url`, `target = me@tammersaleh.com`) is in `~/.config/calendar-sync/config.toml` (local, untracked - the URL is a bearer secret and fine there). Verified: the feed imports 26 events to Personal (stable, `unchanged` on re-poll, no churn), and the rebooked Jul-13 flights (AS3050 STS→LAX, AS233 LAX→EWR) mirrored to CoreWeave.

    **STILL TO DO:** (1) the DEFERRED cleanup of the 16 stale `navan*` events on `tsaleh@coreweave.com` - until then both calendars show BOTH the fresh TripIt flights and the stale navan flights (the expected duplicate window). Delete the `navan*` originals on CoreWeave; the daemon auto-prunes their Personal mirrors (same mechanism as the Reclaim cleanup below). (2) The **B26** follow-up (see `doc/bugs.md`): a FullSync that races a fresh feed import can strand the imported events until the next FullSync/restart; steady-state incremental ticks are fine. Self-heals on restart; a real fix is a small syncToken-handling change.

00. **B25 - daemon `events.delete` always failed on read-only cwd** (merged + pushed to main, commit `ad15273`, ships as the next patch release). Surfaced while clearing ~660 stale Reclaim leftovers off the work calendar (see "Reclaim cleanup" below). The daemon's launchd cwd is `/` (read-only on macOS); gws writes a stray `download.html` there on every 204 (`events.delete`), so each prune logged HTTP 500 + 5 retries (the API delete still landed). Fix wired the already-existing `gws.WithWorkDir` into the production constructor `cmd/gws.go:gwsClient()` via `gwsScratchDir()` (cache→temp→inherit). See `doc/bugs.md` B25 and the CLAUDE.md gotcha.

    **Reclaim cleanup (one-time op, not code):** deleted 443 work-calendar Reclaim leftover events (621 incl. recurring instances), each verified via its `reclaim.personalSync.eventId` backlink to still have a live original on the source calendar. The daemon then auto-pruned ~138 spurious personal-side mirrors (138→0). **40 past TripIt events were intentionally KEPT** on the work calendar - their originals had aged out of the read-only TripIt feed, so the Reclaim copy is the only remaining calendar record; all are in the past. If the user later wants them gone, they're the only `reclaim.*`-tagged events left on `tsaleh@coreweave.com`.

0. **B24 - moved recurring instances were permanently unsyncable** (branch `fix/locate-moved-exception`, NOT yet merged/pushed at end of session). The recurring handler located mirror instances with Google's `events.instances?originalStart=...` filter, which silently fails to return an instance once it's been moved off its native slot - so any recurring instance ever moved (e.g. the `Lunch & Reading` overrides) froze against future source edits with `skip(instance_unmaterializable)`. Now locates by constructing the deterministic instance ID and `events.get`. See `doc/bugs.md` B24 + `doc/plans/moved-exception-locate-fix.md`. A `fix:` commit is stacked on the branch; pushing to main auto-ships a release, so it's left for the user to merge. Follow-up: the pre-existing `patchMirrorWithChecksum` B19 gap noted in B24.
1. **E2E test infrastructure** (`internal/e2e/`, build tag `e2e`, run with `mise run test:e2e`). 14 scenarios against real Google Calendar, ~200s wall-clock. Auto-creates and tears down its own fixture calendars by name (`calendar-sync-e2e-source` / `calendar-sync-e2e-target`); anyone with `gws auth` can clone and run. See `doc/e2e-design.md`.
2. **F1 + F2** (calendar refs by display summary + single-command brew upgrade). Shipped in v2.3.0.
3. **Codex correctness pass** (v2.3.0): 7 findings addressed across 1 `feat:` (PatchEvent type) + 9 `fix:` commits.
4. **B17 target-syncToken** (v2.4.0): target-side edits propagate within ~one tick (~60s) instead of waiting for the 24h FullSync. Per-target syncToken stream, target-delta phase before source-delta on every Tick.
5. **B17 Phase 2** (v2.5.0): mirror-only recurring instance overrides now propagate. When a user edits a single occurrence of a recurring mirror with no source counterpart, the daemon creates the source-side override via `events.patch` and updates the mirror's checksum, all within a tick. Closes the last known limitation in two-way sync. New helper `mirror.BuildSourceOverridePatchBody` is structurally guarded against the B16-class trap (the helper signature can't carry recurrence; pinned by helper-level + integration-level tests).

## Versions

- v2.0.0: per-pair config scoping, `direction` field removed (BREAKING)
- v2.1.0: location managed-field (v3 schema)
- v2.1.x series: B15/B16/B18/B19/B20/B22/B23 fixes
- v2.2.0: F1 (calendar refs by summary) + F2 (brew postflight)
- v2.3.0: Codex correctness pass
- v2.4.0: B17 Phase 1 target-syncToken. Shipped 2026-05-05.
- **v2.5.0**: B17 Phase 2 mirror-only override propagation. Shipped 2026-05-07.

## Daemon state

Daemon as of end of session: running v2.5.0, pid 64943, started 2026-05-07T21:58:27Z. Two pairs: `work-personal` (tsaleh@coreweave.com → me@tammersaleh.com) and `personal-to-work` (reverse). Just bounced via the F2 postflight; FullSync still warming up at end-of-session, so the IPC `mirrors` count was still 0 in the snapshot — expected, populates within minutes of the first FullSync completing per the integration test pinned in `TestDaemon_StartupFullSyncPopulatesSnapshot`.

## v2.4.0 startup IPC observation (still open, but lower priority)

After the v2.4.0 upgrade earlier this week, the daemon ran cleanly per stderr but `calendar-sync status` reported empty `last_full_sync_at`/`last_tick_at`/`mirrors` for ~23 minutes before populating. The follow-up daemon-level integration test (`TestDaemon_StartupFullSyncPopulatesSnapshot`, commit `6fcb0cd`) drives a real `Daemon.Run` end-to-end and the snapshot fields populate correctly — so the in-process path is sound. The production observation was likely environmental (slow real-clock gws calls, a launchd startup race, or similar).

If the empty-window pattern recurs after the v2.5.0 bounce or any future upgrade, that's the signal to dig deeper. Otherwise, treat as a one-off.

## What's left

### 1. Watch v2.5.0 for a quiet week

After a quiet week of v2.4.0 + v2.5.0 in production with no target-edit-related anomalies, `backlog/B17-target-syncToken.md` is safe to delete. Earliest delete-by date (counting from v2.5.0): 2026-05-14.

Active monitoring:
- `propagate(mirror_only_override)` outcomes — confirms Phase 2 working in production. Most users won't see any until they actually drag a recurring instance on the mirror; the existing 5/20 Lunch & Reading instance is presumably stable now.
- `target_sync_token` related warnings — none expected.
- Any patch on the source side that produces a recurrence-bearing body for a per-instance ID — would be the canary for a B16-class regression in the new helper. The `BuildSourceOverridePatchBody` signature makes this structurally impossible, but worth watching if the daemon does anything weird around recurring instances.

### 2. Upstream blocker for retry-after honoring

`internal/gws/retry.go` has a comment pointing at `googleworkspace/cli#777`. The gws CLI doesn't expose `Retry-After` in its error envelope, so calendar-sync's retry layer can't honor it. Two-line change in retry.go once the upstream lands.

### 3. Backlog files

`backlog/B17-target-syncToken.md` is now stale (B17 Phase 1 + Phase 2 both shipped). Per the design doc's last section: safe to delete after a quiet week. `backlog/` is otherwise empty.

## This session's commits (across two days)

```
7d7ac7f docs: drop Phase 1 references from target_delta.go comments
6fd6ca1 test: assert inventory write in B17 Phase 2 happy path
84c7aff docs: SPEC and bugs.md updates for B17 Phase 2
0e38824 feat: B17 Phase 2 propagate mirror-only recurring instance overrides
1a2a21b docs: next.md - integration test gap closed; remove dry-run.err artifact
6fcb0cd test: pin daemon-IPC integration after startup FullSync
8140518 docs: update next.md handoff after v2.4.0 ship
3c42af8 test: cover empty-nextToken branch in runTargetDeltaPhase
404daf0 chore(main): release 2.4.0 (#19)
e258a63 chore: pass release PR JSON via env to bash auto-merge step
```

The workflow fix (`e258a63`) was needed because release-please's auto-merge step embedded the PR JSON inside a bash single-quoted string, and v2.4.0's changelog had "don't" in two commit messages. The apostrophe broke the surrounding quotes, then markdown-link parens broke the parser. Fixed by passing the JSON via env var. PR #19 had to be manually merged for v2.4.0 to ship; PR #20 (v2.5.0) auto-merged cleanly, confirming the fix.

## Out of scope (per prior next.md history; nothing changed)

- B3 (gws timeouts at 365d horizon) - workaround is `--timeout=30m`; daemon doesn't hit this.
- B5 (gws stderr leak into error messages) - cosmetic.
- B8/B9 (launchd plist edge cases) - non-default paths only.

## Architecture / decisions worth knowing

These are recent additions that shape what's possible:

- **`gws.PatchEvent`** (`internal/gws/patch_event.go`, v2.3.0): explicit pointer-typed fields for merge patches. Use `gws.PatchStr("")` to clear a field; `nil` to leave it alone. The `mirror.BuildPatchPayload(*gws.Event) *gws.PatchEvent` converter bridges full-state desired payloads to the patch wire format with explicit-clear semantics. EventsInsert still takes `*gws.Event` (omit-empty is correct for inserts).

- **`gws.Client` retry layer** (`internal/gws/retry.go`, v2.3.0): every gws subprocess call goes through `executeTyped` which wraps `withRetry`. 5 attempts, exponential backoff (1s, 2s, 4s, 8s) with 25% jitter. Retries `CodeRateLimited` and `CodeBackendError` only.

- **`Reconciler.targetSyncTokens`** (B17, v2.4.0): per-target syncToken map alongside the existing per-source one. Seeded BEFORE inventory rebuild on FullSync (critical ordering). Target-delta phase runs BEFORE source-delta on every Tick (critical ordering). Both invariants pinned by tests.

- **`mirror.BuildSourceOverridePatchBody`** (B17 Phase 2, v2.5.0): produces the per-instance source-override patch body. Takes `*gws.Event`, not a `driftedFields []string` slice — recurrence is omitted by construction so a future change can't opt it in. This is the B16 guardrail; pinned by `TestBuildSourceOverridePatchBody_NeverIncludesRecurrence` (helper-level) + `TestTargetDeltaPhase_MirrorOnlyOverride_DoesNotIncludeRecurrenceInPatch` (integration-level, inspects the actual `gws.PatchEvent` reaching the stub).

- **`Reconciler.materializeSourceOverride`** (B17 Phase 2, v2.5.0): the new write path that creates source-side instance overrides. Reuses the Classifier-scoped `patchMirrorWithChecksum` helper rather than duplicating it. Flow: patch source instance → rewrite mirror via `BuildInstancePayload(post-patch source)` + checksum follow-up → `inv.Set` at the per-instance source tuple → emit `propagate(mirror_only_override)`. The inventory write IS load-bearing — without it, the next tick would re-enter Phase 2 and create duplicate source overrides every tick. Pinned by the inventory assertion in `TestTargetDeltaPhase_MirrorOnlyOverride_PromotesToPropagate`.

- **E2E harness conventions** (`internal/e2e/harness_test.go`): `Setup(t, SetupOptions{...})` returns a `*Harness` with calendars provisioned. `t.Cleanup` wipes events + tears down. `startWatch(t, h)` for daemon-mode scenarios; `h.Run(ctx, ...)` for one-shot. `OutcomeMatch` filtered by `SourceEvent` to ignore tombstone noise from prior tests.

- **`CalendarRef`** (`internal/config/types.go`, v2.2.0): `source` and `target` accept either a TOML string OR an inline table `{summary = "...", account = "..."}`. Match against `summaryOverride` (user-visible name) with fallback to raw `summary`. Account disambiguation prefers `dataOwner` equality, falls back to ID-substring.

- **Release workflow shell-safety**: `.github/workflows/release.yml` passes the release PR JSON via env var, not direct `${{ }}` interpolation. Required because changelog bodies include commit messages that may contain apostrophes (or other shell-special characters). v2.5.0's auto-merge of PR #20 confirmed the fix.

## When this file becomes useless

When v2.5.0 has run a quiet week, AND the gws Retry-After upstream issue is closed, AND the v2.4.0 IPC startup observation has not recurred after any subsequent daemon restart, delete this file. History lives in `doc/bugs.md`; design context in `SPEC.md` and `CLAUDE.md`; backlog architecture in `backlog/B17-target-syncToken.md` (until that's deleted too).
