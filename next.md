# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`. Previous sessions shipped:

- v2.0.0: per-pair config scoping, `direction` field removed (BREAKING)
- v2.1.0: location managed-field (v3 schema)
- v2.1.1: 5 fix commits during real-calendar migration
- v2.1.2: nothing user-facing
- v2.1.3: B15 (inherited recurring-instance source-wins bootstrap)
- v2.1.4: B16 (BuildInventory two-pass to skip inherited instances)

## Current state

### Daemon

Running on v2.1.4 via launchd. Two pairs in bidirectional sync at `horizon=365d`:

- `work-personal`: source = `tsaleh@coreweave.com`, target = `me@tammersaleh.com`
- `personal-to-work`: source = `me@tammersaleh.com`, target = `tsaleh@coreweave.com`

Functional and idempotent on the steady-state path. **No data corruption risk.** But currently CPU/quota-heavy: one flaky recurring event (TARS Office Hours, possibly the parent-of-recurring-exception shape) throws transient HTTP 500s on `events.get`, which fails the pdir, invalidates the source syncToken, and triggers a fast-track FullSync. The next FullSync hits the same flake. The daemon has been running back-to-back ~24-minute FullSyncs since startup. Zero writes, zero propagates. See B18 below.

### Recent live writes worth knowing about

- A B16 recurrence-propagate bug clobbered the personal `Lunch & Reading` parent during a manual `calendar-sync run` earlier in the session (moved its anchor from 2026-02-23 to 2026-05-20). Recovered manually by patching the source back. Source data is intact.
- Bidirectional sync is exercising correctly: source-side and target-side edits both reconcile (target-side at FullSync rate today, not tick rate - see B17).

## Backlog

Three queued items. All in `backlog/` with full design docs:

### B18 - per-event error tolerance (`backlog/B18-per-event-error-tolerance.md`)

The daemon's CPU/quota loop. One flaky event = endless FullSyncs. Fix is to discriminate transient read errors (HTTP 5xx on `events.get`, gws subprocess timeouts, recurring-handler 5xx) from fatal ones (write failures) inside `runClassifyLoop`, so the former skip + advance the token while the latter keep gating it. Two implementation options sketched. Recommended next: this one. Smallest surgery, biggest immediate UX win.

### B17 - target-syncToken for sub-tick target-edit propagation (`backlog/B17-target-syncToken.md`)

Target-side edits propagate at FullSync rate (24h default) rather than tick rate (60s default). Architect's design + Codex's review of must-fix items already captured. Phase 1 covers ~80% of cases; Phase 2 needed for mirror-only-override propagation (the user's specific Lunch & Reading 5/20 case).

### Codex full-codebase correctness review

Queued for after B17 and B18 land. Catches anything the recent B15/B16/B17/B18 fixes might have missed.

## Optional knob without backlog work

`[settings].poll_interval = "15s"` and `[settings].full_sync_interval = "1h"` validate fine and have headroom (idle ticks are ~1.1s; full sync is 3-24min depending on syncToken state). Doesn't fix B18 but reduces the worst-case target-edit-propagation window from 24h to 1h without any code change.

## Repo state

Pushed through HEAD. Two backlog docs committed (B17, B18). The `next.md` and `backlog/` are the only "in-flight" surface; main is clean.

## What's NOT in scope right now

- B3 (gws timeouts at 365d horizon) — workaround is `--timeout=30m` on `calendar-sync run`. Daemon doesn't hit this.
- B5 (gws stderr leak into error messages) — cosmetic.
- B8/B9 (launchd plist edge cases) — non-default paths only.

## When this file becomes useless

When B17 + B18 ship, Codex full review is clean, and the daemon has run a quiet week (no back-to-back FullSyncs, no propagate-related anomalies), delete this file. The fixed inventory + per-target syncToken + per-event error tolerance + bidirectional rollout become the new normal in `SPEC.md` and `doc/bugs.md`.
