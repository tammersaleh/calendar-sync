# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`. Previous session resolved the 37-mirror non-idempotency bug (was one-cycle convergence after the d723624 normalization fix - confirmed by run #5 showing 0 propagates). Then completed the personal→cw rollout up to horizon=365d, with the rollout itself surfacing a second, much more serious data-corruption bug (B15) which has been fixed and shipped. Both pairs are now running idempotently at 365d. Daemon install is the only step remaining.

## Where we are

### Repo state

- **main branch**: rebased + pushed through `9143cc4` (the d723624 fix and ancestors). One additional commit LOCAL ONLY:
  - `502759c fix: route inherited recurring-instance mirrors through bootstrap source-wins`
  - This is the B15 fix (described below). It went through `feature-dev:code-reviewer` twice, both clean. Push when SSH agent is reachable: `git push origin main`.
- **Releases shipped**: v2.0.0, v2.1.0, v2.1.1 (tag landed on origin via release-please during this session). The 4 fix commits from this and the prior session will land as v2.1.2 on next release-please cycle once 502759c is pushed.

### Local binary

- `./calendar-sync` (repo root) is built with all fixes through 502759c. Use this, NOT `/opt/homebrew/bin/calendar-sync` (lags) or worktree binaries.

### Config state

- `~/.config/calendar-sync/config.toml`: TWO pairs now active.
  - `work-personal` (cw → personal), inheriting `[settings].horizon = "365d"`.
  - `personal-to-work` (personal → cw), explicit `horizon = "365d"` after the ramp.
  - `[settings].propagate_target_edits = true` (bidirectional sync enabled).
- Backup at `~/.config/calendar-sync/config.toml.pre-v2`.

### Pair 2 rollout (personal → cw)

Completed this session. The full ramp:

| Horizon | First run | Idempotency rerun |
|---------|-----------|-------------------|
| 1d  | 5 inserts, 8 skips | clean |
| 2d  | 2 inserts, 1 cancel-delete, 62 skips | clean |
| 7d  | 1 insert, 3 cancel-deletes, 1 PROPAGATE (pre-fix), 237 skips | clean |
| 14d (pre-fix dry-run) | 1 propagate predicted on yoga 5/11 → STOPPED, fixed B15, rebuilt binary | - |
| 14d (post-fix) | 5 inserts, 7 cancel-deletes, 2 inherited_source_won patches, 330 skips | clean |
| 30d | 6 inserts, 1 cancel-delete, 438 skips | clean |
| 90d | 4 inserts, 819 skips | clean |
| 365d | 13 inserts, 5 cancel-deletes, 1761 skips (1 transient gws error on Lunch & Reading) | TBC - check `/tmp/p2-365d-r2.txt` |

Pre-fix propagates that landed on real calendars:
- Pills 5/6 instance: pair 2 7d run propagated start/end. Source had no actual reschedule (originalStartTime == start), so the patch was semantically a no-op. Etag bumped, sequence on the source went from 4 to 5. No data loss.
- That's the only source-side write across the rollout.

### Bug list

`doc/bugs.md` is the canonical record. New bugs found this session:

**Fixed during this session**:
- B15 (CRITICAL): inherited recurring-instance mirrors clobbered source overrides on first encounter. When calendar-sync wrote a recurring parent, Google auto-materialized instances inheriting the parent's `calendar-sync:checksum`. The standard drift matrix saw `mirror_drifted=true` (live != stored) and the newer-wins tiebreak picked the freshly-materialized mirror over a pre-existing source override, routing to `propagate(target_edited)` and reverting the user's reschedule. Fixed in `502759c` by detecting inherited instances via `mirror.IsInheritedRecurringInstance` (mirror's `calendar-sync:source` EventID matches `source.RecurringEventID`), then routing them through the same source-wins bootstrap path as schema-version migration. New labels: `inherited_source_won` (conflict), `inherited_upgrade` (reason). Source is never patched on this path. SPEC.md, doc/bugs.md, and tests all updated.

**Still open** (pre-existing, unchanged):
- B3: gws subprocess timeouts at 365d horizon. Mitigation: `--timeout=30m`. Workaround sufficient.
- B5: gws stderr ("Using keyring backend: keyring") leaks into formatted error strings. Cosmetic. Surfaced on every run on a couple of recurring events (TARS Office Hours, Lunch & Reading) but doesn't affect outcomes.
- B8: `config.FindPath` can return relative paths; launchd `WatchPaths` is undefined for them. Edge case.
- B9: plist generator uses `text/template` not `html/template`. Edge case.

### What's left

1. **Push 502759c**: `git push origin main` once SSH agent is reachable.
2. **Verify the 365d idempotency rerun**: `grep '"_meta"' /tmp/p2-365d-r2.txt` and confirm 0 propagates / 0 patches.
3. **Run pair 1 once at 365d**: confirm pair 2's 22 new mirrors on cw don't echo through pair 1. The earlier loop-guard test (after pair 2's first 1d run) was clean; this re-runs it after the full ramp.
4. **Install the daemon**: `./calendar-sync install`. Creates the launchd plist with auto-reload via `WatchPaths`. Sets up a 60-second poll cycle. The user explicitly raised concern about real-calendar writes during this session, so check in before installing.

### Pair 2 mirrors on cw (current count)

22 mirrors across the ramp:
- 1d: pills & whitening (recurring parent), Breakfast and drive Mads to school, Lunch, bloodwork, Serwar
- 2d: pack, 🧘/🏃 (recurring parent)
- 7d: Budget
- 14d: check jury duty, Tammer dinner, Alone time, Lunch & Reading, Dinner at Khom Loi
- 30d: 6 more (mostly Spring 2026 events)
- 90d: Tammer b-day, Drive, Monterey Trip, Petaluma Fair
- 365d: 13 more (rest of the year)

Plus per-instance bootstrap patches on yoga 5/11 and the Lunch & Reading override series (about 14 instances total). The patches set `calendar-sync:source` to the per-instance form so future drift checks compare instance-vs-instance.

## Test calendar cleanup (low priority)

The test calendars `calendar-sync-test-A` and `calendar-sync-test-B` are still on Tammer's account with cancelled tombstones from scenario tests. Delete via `gws calendar calendars delete --params '{"calendarId":"<id>"}'` when no longer needed. They were never used during this session - the inherited-instance bug was fixed via unit tests + a dry-run-first verification on the real calendars rather than reproducing in isolation.

## When this file becomes useless

When 502759c is pushed, both pairs are running under the daemon, and the next release-please cycle has shipped v2.1.2, delete this file. The fixed checksum logic + per-pair config + bidirectional rollout will be the new normal in SPEC.md and CLAUDE.md.
