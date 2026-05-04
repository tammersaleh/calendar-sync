# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`. Previous sessions shipped:

- v2.0.0: per-pair config scoping, `direction` field removed (BREAKING)
- v2.1.0: location managed-field (v3 schema)
- v2.1.1: 5 fix commits during real-calendar migration
- v2.1.2: nothing user-facing
- v2.1.3: B15 (inherited recurring-instance source-wins bootstrap)
- v2.1.4: B16 (BuildInventory two-pass to skip inherited instances)
- v2.1.5: B18 (per-event transient read error tolerance)
- v2.1.6 (pending release): B22 (carry-on through 410/404 in classify-path delete)
- v2.1.7 (pending release): B20 (revive cancelled mirror when source is syncable)

## Current state

### Daemon

Running on v2.1.5 (B18 in place; verified 435 transient skips today). Pair `work-personal` was failing every tick from B22's already-deleted-mirror 410, but the v2.1.6 fix is committed - waiting on release-please. After v2.1.7 cuts, run `brew upgrade calendar-sync && calendar-sync uninstall && calendar-sync install` to pick up both B22 and B20.

Two pairs in bidirectional sync at `horizon=365d`:

- `work-personal`: source = `tsaleh@coreweave.com`, target = `me@tammersaleh.com`
- `personal-to-work`: source = `me@tammersaleh.com`, target = `tsaleh@coreweave.com`

Functional and idempotent on the steady-state path. **No data corruption risk.**

### Recent live writes worth knowing about

- B20 recovery: 5 Lunch & Reading mirror instances on work calendar (5/4-5/8) were stuck at status=cancelled with source confirmed. Manually patched all 5 to status=confirmed; daemon now sees them as `unchanged` (correctly). With v2.1.7 in place, future occurrences of this class of stuck-state will self-heal on the next tick.
- 5/11 Lunch & Reading: source was at 11:30, mirror at 11:00. User confirmed 11am is correct. Patched source to 11:00; daemon reconciled successfully (`action: patch, reason: source_updated`).
- Earlier: A B16 recurrence-propagate bug clobbered the personal `Lunch & Reading` parent (moved anchor from 2026-02-23 to 2026-05-20). Recovered manually. Source data is intact.

### Push state

2 commits stacked locally on `main` waiting for SSH key reload:

- `bcaebce fix: revive cancelled mirror when source is syncable` (B20)
- `a50a47f docs: queue B23 (drift signal blind spot) in backlog`

Earlier B22 fix (`985ddc7`) already pushed.

## Backlog

### B23 - drift signal never compares source-now to mirror-now directly (CRITICAL, NEW)

Surfaced during B20 investigation. The drift signal correctly answers "did either side change since the last write?" but never asks "are the two sides in sync right now?" Once stored bookkeeping locks into a state where both signals match, any subsequent divergence in managed fields is invisible. B20 is a specific symptom on the Status field. B23 is the structural fix: add a `fields_disagree` signal that compares `hash(source.managed)` to `hash(mirror.managed)` directly. SPEC-level change, requires Codex review on the four-way matrix expansion. Full design in `doc/bugs.md` Open.

### B19 - stale inventory after partial recurring-instance repair-path failure

Pre-existing; B18 makes it observable. Spurious double-writes bounded by `full_sync_interval`. Small fix touching `recurring.Handler`'s Result/error contract. Full design in `doc/bugs.md` Open.

### B17 - target-syncToken for sub-tick target-edit propagation (`backlog/B17-target-syncToken.md`)

Target-side edits propagate at FullSync rate (24h default) rather than tick rate (60s default). Architect's design + Codex's review of must-fix items already captured. Phase 1 covers ~80% of cases; Phase 2 needed for mirror-only-override propagation.

### Codex full-codebase correctness review

Queued for after B17/B19/B23 land. Catches anything the recent B15/B16/B18/B20/B22 fixes might have missed.

## Optional knob without backlog work

`[settings].poll_interval = "15s"` and `[settings].full_sync_interval = "1h"` validate fine and have headroom. Reduces worst-case target-edit-propagation window from 24h to 1h without any code change.

## Repo state

Local main: 2 commits ahead of origin (B20 + B23 docs). Backlog docs: `B17-target-syncToken.md` is the only relevant one - `B18-per-event-error-tolerance.md` can be deleted (B18 shipped; the canonical doc lives in SPEC.md and bugs.md).

## What's NOT in scope right now

- B3 (gws timeouts at 365d horizon) — workaround is `--timeout=30m` on `calendar-sync run`. Daemon doesn't hit this.
- B5 (gws stderr leak into error messages) — cosmetic.
- B8/B9 (launchd plist edge cases) — non-default paths only.

## When this file becomes useless

When B17, B19, B23 ship, the Codex full review is clean, and the daemon has run a quiet week (no back-to-back FullSyncs, no stuck-cancelled-mirror anomalies, no propagate-related anomalies), delete this file. Everything in here will be normal then.
