# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`.

## Where the project stands

calendar-sync is feature-complete on the planned scope. Three sessions of work just shipped:

1. **E2E test infrastructure** (`internal/e2e/`, build tag `e2e`, run with `mise run test:e2e`). 14 scenarios against real Google Calendar, ~200s wall-clock. Auto-creates and tears down its own fixture calendars by name (`calendar-sync-e2e-source` / `calendar-sync-e2e-target`); anyone with `gws auth` can clone and run. See `doc/e2e-design.md`.
2. **F1 + F2** (calendar refs by display summary + single-command brew upgrade). Shipped in v2.3.0 alongside the Codex correctness pass.
3. **Codex correctness pass** (v2.3.0): 7 findings addressed across 1 `feat:` (PatchEvent type) + 9 `fix:` commits.
4. **B17 target-syncToken** (just merged to main, awaiting release): target-side edits now propagate within ~one tick (~60s) instead of waiting for the 24h FullSync. New per-target syncToken stream, target-delta phase runs BEFORE source-delta on every Tick. 19 unit tests + 1 E2E (`TestE2E_B17_TargetEditPropagatesNextTick`).

## Versions and current daemon state

- v2.0.0: per-pair config scoping, `direction` field removed (BREAKING)
- v2.1.0: location managed-field (v3 schema)
- v2.1.x series: B15/B16/B18/B19/B20/B22/B23 fixes
- v2.2.0: F1 (calendar refs by summary) + F2 (brew postflight)
- v2.3.0: Codex correctness pass (PatchEvent, retry layer, time_zone plumbing, syncToken-empty fix, --timeout placement, plist safety, empty-propagate degrade)
- **v2.4.0 (pending):** B17 target-syncToken. Just pushed; release-please will cut it shortly. Once the cask updates, `brew upgrade calendar-sync` will bounce the daemon automatically (F2 is already in place since v2.3.0).

Daemon as of last check (~14h ago):
- Running on v2.3.0, pid 12119, started 2026-05-05T04:53Z
- Stderr has been silent since the v2.3.0 startup BuildInventory - 9+ hours of clean operation. Pre-v2.3.0 stderr was full of HTTP 410/404/500 warns every tick; the retry layer plus carry-on-through-410/404 (B22) absorbs them now.
- Two pairs: `work-personal` (tsaleh@coreweave.com → me@tammersaleh.com) and `personal-to-work` (reverse). Both at horizon=365d.
- 45 `instance_unmaterializable` outcomes observed across 4 distinct events ("🧘/🏃", "Global Engineering Operations Review", "Mistral AI + CoreWeave Weekly Sync", "Product Engineering Demos") - SPEC notes this as rare. Not a regression, predates v2.3.0; worth investigating eventually if it stays steady.

When the next session starts:
```bash
brew update && brew upgrade calendar-sync && calendar-sync version
calendar-sync status
```
The brew postflight bounces the daemon automatically. No `uninstall && install` dance.

## What's left

### 1. Verify v2.4.0 ships cleanly (small)

After release-please cuts v2.4.0 (~5 min after the push that just happened), `brew upgrade` should pick it up and the daemon's pid should roll. Then watch the logs for ~an hour to make sure B17 is healthy in production:
- Look for `propagate` outcomes (target-delta phase firing on real edits).
- Look for `target_sync_token` related warns - none expected.
- A 410 on the target side should trigger `NeedsFullResync` for that target and the scheduler should fast-track a FullSync.

If you want to actively test it: edit one of your mirror events on `me@tammersaleh.com` (e.g. tweak the summary on a Lunch & Reading instance). Within ~1 minute, the change should propagate back to the work calendar. Pre-B17 this would have waited up to 24h.

### 2. Fast-follow test gap (low priority)

The final B17 review flagged one remaining test gap: no unit test for the `nextToken == ""` branch in `runTargetDeltaPhase`. The code is correct (mirrors source-delta's `TestAdvanceTokens_StaleTokenClearedWhenStagedEmpty` pattern) but the path is untested. Confidence 82 from the reviewer - "not blocking, but obvious to add".

To add: a unit test that primes a target-delta call where Google returns events but no `nextSyncToken`, asserts the in-memory token is cleared and `NeedsFullResync=true`. Pattern: look at how `TestAdvanceTokens_StaleTokenClearedWhenStagedEmpty` constructs that scenario for the source path.

### 3. Phase 2 of B17 (deferred, may not ship)

`backlog/B17-target-syncToken.md` documents Phase 2: when target-delta hits a `source_orphan` (404 on `events.get` for the source instance), automatically create the source override via `events.patch` so the user's mirror-only edit propagates instead of being skipped.

Today's Phase 1 emits `skip(reason=mirror_only_override)` for this case. The user's specific motivating bug (Lunch & Reading 5/20 instance edit on cw target) was a Phase 2 scenario - they edited an inherited recurring instance that had no source counterpart.

Per the design doc: "Phase 2 introduces source-override creation which is a new write path that the existing tests don't cover; risk it ships with edge cases (the B16-style recurrence inclusion bug being the obvious one to guard)." Phase 2 is genuinely optional - SPEC §"Limitation: mirror-only recurring instance overrides" already documents this as a known limitation.

Recommend: leave Phase 2 deferred until you observe the limitation actually hurting in practice. The B17 Phase 1 + the existing 24h FullSync catches mirror-only edits on the daily boundary; that may be good enough.

### 4. Upstream blocker for retry-after honoring

`internal/gws/retry.go` has a comment pointing at `googleworkspace/cli#777` (filed this session). The gws CLI doesn't expose `Retry-After` in its error envelope, so calendar-sync's retry layer can't honor it. Two-line change in retry.go once the upstream lands the field.

### 5. Backlog files

`backlog/B17-target-syncToken.md` is now stale (B17 shipped). Per the design doc's last section: "When B17 ships (Phase 1 minimum, Phase 2 if pursued), tests pass, Codex review is clean, daemon has run a full week without target-edit-related anomalies, delete this file." First three conditions met; "full week" is the gate to delete. Will be safe to delete around 2026-05-12 if no anomalies.

`backlog/` is otherwise empty.

## Out of scope (per next.md history; nothing changed)

- B3 (gws timeouts at 365d horizon) - workaround is `--timeout=30m` on `calendar-sync run`; daemon doesn't hit this.
- B5 (gws stderr leak into error messages) - cosmetic.
- B8/B9 (launchd plist edge cases) - non-default paths only.

## Architecture / decisions worth knowing for the next session

These are recent additions to mention because they shape what's possible:

- **`gws.PatchEvent`** (`internal/gws/patch_event.go`, shipped in 2.3.0): explicit pointer-typed fields for merge patches. Use `gws.PatchStr("")` to clear a field; `nil` to leave it alone. The `mirror.BuildPatchPayload(*gws.Event) *gws.PatchEvent` converter bridges full-state desired payloads to the patch wire format with explicit-clear semantics. EventsInsert still takes `*gws.Event` (omit-empty is correct for inserts).

- **`gws.Client` retry layer** (`internal/gws/retry.go`, shipped in 2.3.0): every gws subprocess call goes through `executeTyped` which wraps `withRetry`. 5 attempts, exponential backoff (1s, 2s, 4s, 8s) with 25% jitter. Retries `CodeRateLimited` and `CodeBackendError` only; everything else fails fast. Per-call timeout (from `cmd/timeout_api.go`'s wrapper) bounds the total retry budget.

- **`Reconciler.targetSyncTokens`** (B17, just shipped): per-target syncToken map alongside the existing per-source one. Seeded BEFORE inventory rebuild on FullSync (critical ordering). Target-delta phase runs BEFORE source-delta on every Tick (critical ordering). Both invariants pinned by tests.

- **E2E harness conventions** (`internal/e2e/harness_test.go`): `Setup(t, SetupOptions{...})` returns a `*Harness` with calendars provisioned. `t.Cleanup` wipes events + tears down. `startWatch(t, h)` for daemon-mode scenarios; `h.Run(ctx, ...)` for one-shot. `OutcomeMatch` filtered by `SourceEvent` to ignore tombstone noise from prior tests in the same run.

- **`CalendarRef`** (`internal/config/types.go`, shipped in 2.2.0): `source` and `target` accept either a TOML string OR an inline table `{summary = "...", account = "..."}`. Match against `summaryOverride` (user-visible name) with fallback to raw `summary`. Account disambiguation prefers `dataOwner` equality, falls back to ID-substring.

## When this file becomes useless

When v2.4.0 has shipped and run a quiet week without target-edit-related anomalies, AND the gws Retry-After upstream issue is closed, delete this file. The history lives in `doc/bugs.md`; design context in `SPEC.md` and `CLAUDE.md`; backlog architecture in `backlog/B17-target-syncToken.md` (until that's deleted too).

## Commit history of this session (for context)

```
228d449 test: E2E scenario for B17 target-edit-propagates-next-tick
4353e8d fix: don't advance target syncToken when inventory is missing
a25544f fix: tolerate transient read errors in target-delta classify
792ea07 fix: don't shadow recurring parent in inventory on inherited target-delta
2bdef04 docs: add mirror_only_override and source_orphan to SPEC reason table
cda0f3a test: pin self-write suppression outcome shape
5853c8b fix: emit skip(source_orphan) for non-recurring target-delta 404
8868a25 fix: trigger fast-track FullSync on target-token 410 GONE
2818ea1 chore: record gws retry-after upstream issue link
8a7bdc3 docs: SPEC and bugs.md updates for B17
547032b feat: B17 target-syncToken for sub-tick target-edit propagation
01d0232 docs: drop stale Codex-queued and Cleanup notes from next.md
b34770e chore(main): release 2.3.0
507ce54 fix: address final-review findings on correctness pass
977a6c0 test: migrate E2E scenarios to gws.PatchEvent
8a1b6e5 fix: degrade empty-fields propagate to stale_bookkeeping in recurring handler
c13452f fix: retry rate-limited and backend gws calls per SPEC
91337a5 fix: degrade empty-fields propagate to stale_bookkeeping
f7db671 feat: PatchEvent type for explicit-clear merge patches
f4f7fc8 fix: install plist handles relative paths and XML metacharacters
9f82a67 fix: --timeout bounds the full command, not just the run loop
083a170 fix: clear stale syncToken on FullSync when Google omits nextSyncToken
e8f6d39 fix: respect [[pairs]].time_zone for all-day mirrored events
81fa109 chore: drop stale backlog/B18 doc
5f78d3f chore(main): release 2.2.0
6ae1b43 docs: update next.md - F1 and F2 shipped
23dc765 fix: address F2 review findings
66646af feat: bounce launchd agent on brew upgrade via cask postflight
8baf219 fix: honor account on single-summary-match for CalendarRef
dcc9797 fix: clear stale union state in CalendarRef unmarshalers
95b5b93 fix: type-assert inline-table fields in CalendarRef.UnmarshalTOML
a00ee0e feat: match calendar refs against summaryOverride and dataOwner
86274f6 test: E2E scenario for F1 summary-form calendar refs
d8fa0dc docs: document inline-table source/target for summary lookups
840e680 test: cover summary ref still-ambiguous-after-account-filter case
fd3863f feat: resolve summary-form calendar refs via CalendarListList
6a7e55a feat: validate CalendarRef summary/account union rules
ea238e0 feat: add CalendarRef type for string-or-table source/target
```
