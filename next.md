# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`. Previous sessions shipped:

- v2.0.0: per-pair config scoping, `direction` field removed (BREAKING)
- v2.1.0: location managed-field (v3 schema)
- v2.1.1: 5 fix commits during real-calendar migration (transparency normalization, recurrence on parents, migration drift recompute, etc.)
- v2.1.2: nothing user-facing (release-please picked up the fix backlog)
- v2.1.3: B15 (inherited recurring-instance source-wins bootstrap)
- v2.1.4 (in flight): B16 (BuildInventory two-pass to skip inherited instances)

The B17 target-syncToken work is captured in detail in `backlog/B17-target-syncToken.md` and is deliberately not in this version.

## Where we are

### Repo state

- main pushed through `af7dda7`. v2.1.4 release-please cycle in flight.
- Two pairs running bidirectional sync at `horizon=365d` per `~/.config/calendar-sync/config.toml`.

### Daemon state

Currently STOPPED via `launchctl unload` so the user can recover from a B16-induced corruption (recovered manually). After v2.1.4 lands and the homebrew formula updates, the steps to resume are:

```
brew upgrade --cask tammersaleh/tap/calendar-sync
launchctl load ~/Library/LaunchAgents/org.calendar-sync.agent.plist
calendar-sync status
```

The first tick after restart will be a full sync (~24min based on prior measurements). Subsequent ticks are sub-second incremental.

### Known limitation

Per `backlog/B17-target-syncToken.md`: target-side edits propagate at the next FULL re-sync (`[settings].full_sync_interval`, default 24h), not at tick rate. Workaround: `launchctl unload && calendar-sync run --pair <name> && launchctl load`.

### Bug list

`doc/bugs.md` is the canonical record. Open bugs unchanged from prior sessions (B3 timeouts, B5 stderr leak, B8/B9 launchd plist edge cases). All non-blocking.

## What's left

In rough priority:

1. **B17 (target-syncToken)**: full design + Codex's review live in `backlog/B17-target-syncToken.md`. Phase 1 (target-delta phase, dispatch through Classifier) addresses ~80% of cases. Phase 2 (source-override creation for mirror-only edits) is needed for the user's specific Lunch & Reading style case to propagate at tick rate. Estimate one focused session.
2. **Codex full-codebase correctness review**: deferred until B17 lands. Run after Phase 1 to catch any regressions or missed edge cases that the recent B15/B16/B17 fixes might have introduced.
3. **B3/B5/B8/B9**: cosmetic / edge cases per `doc/bugs.md`. Skip unless asked.

## What this session did

- Confirmed B14 (one-cycle convergence) was correctly diagnosed - run #5/#6 after the d723624 normalization fix were idempotent.
- Rolled out personal→cw bidirectionally up to 365d. 22 mirrors landed on cw.
- Surfaced B15 (inherited recurring-instance bootstrap source-wins) at 14d via dry-run; fixed and shipped as v2.1.3.
- Daemon installed via Homebrew; ran cleanly for ~30 minutes including the first 24-min full sync.
- User edited Lunch & Reading 5/20 mirror to validate target-edit propagation.
- Bug B16 surfaced: inventory parent/instance collision caused a manual `calendar-sync run` to clobber the source recurring parent's anchor (moved Lunch & Reading from Feb 23 11:30 to May 20 12:00). Recovered manually via `events.patch`.
- B17 (the original UX issue motivating the user's edit-to-test) deferred to `backlog/B17-target-syncToken.md`.
- B16 fix shipped as v2.1.4.

## When this file becomes useless

When B17 ships and the daemon has run a full week without target-edit-related anomalies, delete this file. The fixed inventory + per-target syncToken + bidirectional rollout will be the new normal in SPEC.md and `doc/bugs.md` from then on.
