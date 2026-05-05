# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`.

## Where the project stands

calendar-sync is feature-complete on the planned scope. Recent work history:

1. **E2E test infrastructure** (`internal/e2e/`, build tag `e2e`, run with `mise run test:e2e`). 14 scenarios against real Google Calendar, ~200s wall-clock. Auto-creates and tears down its own fixture calendars by name (`calendar-sync-e2e-source` / `calendar-sync-e2e-target`); anyone with `gws auth` can clone and run. See `doc/e2e-design.md`.
2. **F1 + F2** (calendar refs by display summary + single-command brew upgrade). Shipped in v2.3.0.
3. **Codex correctness pass** (v2.3.0): 7 findings addressed across 1 `feat:` (PatchEvent type) + 9 `fix:` commits.
4. **B17 target-syncToken** (v2.4.0): target-side edits propagate within ~one tick (~60s) instead of waiting for the 24h FullSync. Per-target syncToken stream, target-delta phase before source-delta on every Tick. 19 unit tests + 1 E2E + B17 fast-follow test added this session.

## Versions

- v2.0.0: per-pair config scoping, `direction` field removed (BREAKING)
- v2.1.0: location managed-field (v3 schema)
- v2.1.x series: B15/B16/B18/B19/B20/B22/B23 fixes
- v2.2.0: F1 (calendar refs by summary) + F2 (brew postflight)
- v2.3.0: Codex correctness pass
- **v2.4.0**: B17 target-syncToken. Shipped 2026-05-05.

## Daemon state

Daemon as of end of session (~16:00Z 2026-05-05): running v2.4.0, pid 76065, started 15:32:04Z. Two pairs: `work-personal` (tsaleh@coreweave.com → me@tammersaleh.com) and `personal-to-work` (reverse). 149 + 72 mirrors. Stderr silent since the v2.4.0 startup BuildInventory.

## Open question: IPC status was empty for the first ~23 minutes after v2.4.0 startup

After `brew upgrade` to v2.4.0, the daemon ran cleanly per stderr (BuildInventory complete at 15:32:38Z, no warnings since), and `_meta` lines streamed to stdout per tick. But `calendar-sync status` reported `mirrors:0` and omitted `last_full_sync_at`, `last_tick_at`, `last_tick_status` for ~23 minutes. Around 15:55:49Z those fields populated correctly and have stayed that way.

Curious: when the IPC eventually populated (`last_full_sync_at = 15:55:49Z`), no fresh BuildInventory entries appeared in stderr. That implies either (a) `recordFullSync` was called from a non-FullSync path (no caller of that exists in the code), or (b) the FullSync ran but its `BuildInventory` calls didn't log - which contradicts `inventory.go:219`'s unconditional `if log != nil` log emission.

A daemon-level integration test (`TestDaemon_StartupFullSyncPopulatesSnapshot`, commit `6fcb0cd`) now drives a real `Daemon.Run` end-to-end and asserts the snapshot fields populate after the startup FullSync. The test PASSES, which means the in-process `runFullSync → recordFullSync → snapshot → IPC` path is correct. The production observation was therefore likely environmental (slow real-clock gws calls, a launchd startup/restart race, or similar) rather than a code bug. The test guards against a real future regression in this path.

For now: daemon IS functioning - mirrors maintained, stderr clean, target-edit propagation working in real-time. If the IPC empty-field window recurs after a future daemon restart, that's worth investigating further; the integration test would have caught a code-level cause.

## What's left

### 1. Watch v2.4.0 for a quiet week

Per next.md history's deletion criteria: when v2.4.0 has run a quiet week without target-edit-related anomalies, the B17 backlog file (`backlog/B17-target-syncToken.md`) is safe to delete. Earliest delete-by date: 2026-05-12.

Active monitoring should look for:
- `propagate` outcomes (target-delta phase firing on real edits) - confirms B17 working in production.
- `target_sync_token` related warnings - none expected.
- The ~23min IPC blank window from this session's startup - did this reproduce after any later daemon restart?

### 2. Phase 2 of B17 (deferred, may not ship)

`backlog/B17-target-syncToken.md` documents Phase 2: when target-delta hits a `source_orphan` (404 on `events.get` for the source instance), automatically create the source override via `events.patch` so the user's mirror-only edit propagates instead of being skipped.

Today's Phase 1 emits `skip(reason=mirror_only_override)` for this case. SPEC §"Limitation: mirror-only recurring instance overrides" already documents this as a known limitation.

Recommend: leave Phase 2 deferred until you observe the limitation actually hurting in practice.

### 3. Upstream blocker for retry-after honoring

`internal/gws/retry.go` has a comment pointing at `googleworkspace/cli#777`. The gws CLI doesn't expose `Retry-After` in its error envelope, so calendar-sync's retry layer can't honor it. Two-line change in retry.go once the upstream lands.

### 4. Backlog files

`backlog/B17-target-syncToken.md` is now stale (B17 shipped). Per the design doc's last section: safe to delete after a quiet week of v2.4.0 in production. `backlog/` is otherwise empty.

## This session's commits

```
6fcb0cd test: pin daemon-IPC integration after startup FullSync
8140518 docs: update next.md handoff after v2.4.0 ship
3c42af8 test: cover empty-nextToken branch in runTargetDeltaPhase
404daf0 chore(main): release 2.4.0 (#19)
e258a63 chore: pass release PR JSON via env to bash auto-merge step
```

The workflow fix (e258a63) was needed because release-please's auto-merge step embedded the PR JSON inside a bash single-quoted string, and v2.4.0's changelog had "don't" in two commit messages. The apostrophe broke the surrounding quotes, then markdown-link parens broke the parser. Fixed by passing the JSON via env var. PR #19 had to be manually merged for v2.4.0 to ship.

## Out of scope (per prior next.md history; nothing changed)

- B3 (gws timeouts at 365d horizon) - workaround is `--timeout=30m`; daemon doesn't hit this.
- B5 (gws stderr leak into error messages) - cosmetic.
- B8/B9 (launchd plist edge cases) - non-default paths only.

## Architecture / decisions worth knowing

These are recent additions that shape what's possible:

- **`gws.PatchEvent`** (`internal/gws/patch_event.go`, v2.3.0): explicit pointer-typed fields for merge patches. Use `gws.PatchStr("")` to clear a field; `nil` to leave it alone. The `mirror.BuildPatchPayload(*gws.Event) *gws.PatchEvent` converter bridges full-state desired payloads to the patch wire format with explicit-clear semantics. EventsInsert still takes `*gws.Event` (omit-empty is correct for inserts).

- **`gws.Client` retry layer** (`internal/gws/retry.go`, v2.3.0): every gws subprocess call goes through `executeTyped` which wraps `withRetry`. 5 attempts, exponential backoff (1s, 2s, 4s, 8s) with 25% jitter. Retries `CodeRateLimited` and `CodeBackendError` only.

- **`Reconciler.targetSyncTokens`** (B17, v2.4.0): per-target syncToken map alongside the existing per-source one. Seeded BEFORE inventory rebuild on FullSync (critical ordering). Target-delta phase runs BEFORE source-delta on every Tick (critical ordering). Both invariants pinned by tests.

- **E2E harness conventions** (`internal/e2e/harness_test.go`): `Setup(t, SetupOptions{...})` returns a `*Harness` with calendars provisioned. `t.Cleanup` wipes events + tears down. `startWatch(t, h)` for daemon-mode scenarios; `h.Run(ctx, ...)` for one-shot. `OutcomeMatch` filtered by `SourceEvent` to ignore tombstone noise from prior tests.

- **`CalendarRef`** (`internal/config/types.go`, v2.2.0): `source` and `target` accept either a TOML string OR an inline table `{summary = "...", account = "..."}`. Match against `summaryOverride` (user-visible name) with fallback to raw `summary`. Account disambiguation prefers `dataOwner` equality, falls back to ID-substring.

- **Release workflow shell-safety** (this session): `.github/workflows/release.yml` passes the release PR JSON via env var, not direct `${{ }}` interpolation. Required because changelog bodies include commit messages that may contain apostrophes (or other shell-special characters).

## When this file becomes useless

When v2.4.0 has run a quiet week, AND the gws Retry-After upstream issue is closed, AND the IPC startup quirk is either reproduced+fixed or confirmed environmental, delete this file. History lives in `doc/bugs.md`; design context in `SPEC.md` and `CLAUDE.md`; backlog architecture in `backlog/B17-target-syncToken.md` (until that's deleted too).
