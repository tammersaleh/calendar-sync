# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`. Previous sessions shipped:

- v2.0.0: per-pair config scoping, `direction` field removed (BREAKING)
- v2.1.0: location managed-field (v3 schema)
- v2.1.1: 5 fix commits during real-calendar migration
- v2.1.2: nothing user-facing
- v2.1.3: B15 (inherited recurring-instance source-wins bootstrap)
- v2.1.4: B16 (BuildInventory two-pass to skip inherited instances)
- v2.1.5 (pending release): B18 (per-event transient read error tolerance)

## Current state

### Daemon

Still running on v2.1.4 via launchd at the time of writing. The B18 fix landed on `main` as commit `d66b12e`; the release-please PR will pick it up and ship v2.1.5 once CI is green. After the release lands, run `brew upgrade calendar-sync && calendar-sync uninstall && calendar-sync install` to pick it up. Until then the daemon will keep doing back-to-back FullSyncs.

Two pairs in bidirectional sync at `horizon=365d`:

- `work-personal`: source = `tsaleh@coreweave.com`, target = `me@tammersaleh.com`
- `personal-to-work`: source = `me@tammersaleh.com`, target = `tsaleh@coreweave.com`

Functional and idempotent on the steady-state path. **No data corruption risk.** Once v2.1.5 is in place, the TARS Office Hours flake (and any other persistently-flaky recurring read) will surface as a `transient=true` warn line on each tick instead of pinning the source's syncToken.

### Recent live writes worth knowing about

- A B16 recurrence-propagate bug clobbered the personal `Lunch & Reading` parent during a manual `calendar-sync run` earlier in the session (moved its anchor from 2026-02-23 to 2026-05-20). Recovered manually by patching the source back. Source data is intact.
- Bidirectional sync is exercising correctly: source-side and target-side edits both reconcile (target-side at FullSync rate today, not tick rate - see B17).

## Backlog

### B20 - cancelled mirror with confirmed source classifies `unchanged` forever (CRITICAL, NEW)

Surfaced 2026-05-03 evening when the user noticed missing future Lunch & Reading instances on the work mirror. 5 mirror instances (5/4 - 5/8) sat at status=cancelled while the source was confirmed; daemon reported `unchanged` every tick because Status isn't a managed field and the stored checksum still matched. Manually revived the 5 instances via direct gws patch (status=confirmed). Full design + fix sketch in `doc/bugs.md` Open. Lean: Option A (revive branch in classify before mirror.Classify), avoids forcing a re-checksum across every existing mirror that Option B (add Status to ManagedFields) would require.

Side note in bugs.md: an active drift on 5/11 (source 11:30, mirror 11:00) was found during the same investigation. Daemon reports `unchanged` for that instance too. Awaiting user input on which side is the source of truth before deciding whether it's part of B20 or a separate issue.

### B19 - stale inventory after partial recurring-instance repair-path failure (NEW)

Surfaced during B18 code review (pre-existing; B18's transient tolerance makes it observable). When `recurring/handler.go` `locateMirrorInstance`'s repair path succeeds at `forceRewriteMirrorParent` (two `events.patch` writes) but the subsequent `events.instances` flakes transiently, `Handle` discards the post-rewrite mirror parent on the error return path. The next tick's classify loop sees stale inventory and may re-fire the force-rewrite. Bounded by `full_sync_interval` (FullSync rebuilds inventory). Spurious double-writes only - no data loss, no source-side effect. Full design + fix sketch in `doc/bugs.md` Open. Small fix; touches `recurring.Handler`'s Result/error contract.

### B17 - target-syncToken for sub-tick target-edit propagation (`backlog/B17-target-syncToken.md`)

Target-side edits propagate at FullSync rate (24h default) rather than tick rate (60s default). Architect's design + Codex's review of must-fix items already captured. Phase 1 covers ~80% of cases; Phase 2 needed for mirror-only-override propagation (the user's specific Lunch & Reading 5/20 case).

### Codex full-codebase correctness review

Queued for after B17 lands. Catches anything the recent B15/B16/B18 fixes might have missed.

## Optional knob without backlog work

`[settings].poll_interval = "15s"` and `[settings].full_sync_interval = "1h"` validate fine and have headroom (idle ticks are ~1.1s; full sync is 3-24min depending on syncToken state). Reduces worst-case target-edit-propagation window from 24h to 1h without any code change.

## Repo state

Pushed through HEAD. Main is clean. Backlog docs: B17, B18 (B18 design doc can be deleted after release-please ships v2.1.5 since the spec + bugs.md now carry the canonical version).

## What's NOT in scope right now

- B3 (gws timeouts at 365d horizon) — workaround is `--timeout=30m` on `calendar-sync run`. Daemon doesn't hit this.
- B5 (gws stderr leak into error messages) — cosmetic.
- B8/B9 (launchd plist edge cases) — non-default paths only.

## When this file becomes useless

When B17 ships, B19 ships, the Codex full review is clean, and the daemon has run a quiet week (no back-to-back FullSyncs, no propagate-related anomalies), delete this file. The fixed inventory + per-target syncToken + per-event error tolerance + bidirectional rollout become the new normal in `SPEC.md` and `doc/bugs.md`.
