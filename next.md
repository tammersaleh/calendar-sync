# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`.

## E2E test system landed (this session)

The full E2E suite is in place at `internal/e2e/` (build tag `e2e`). 12 scenarios across happy-path, modify, delete, transparency-filter, outside-horizon, 409 collision, B20 revive, recurring parent + instance override, instance override propagation, B23 stale-bookkeeping, target-edit propagate, and watch-mode tick. Run with `mise run test:e2e`. Wall-clock ~160s against real Google Calendar.

The harness auto-creates and tears down its fixture calendars by name (`calendar-sync-e2e-source` / `-target`) so anyone with `gws auth` can clone and run. `mise run check` (the pre-push gate) is unaffected; default `go test ./...` doesn't see the package.

One scenario was deliberately dropped from the original plan: `TestE2E_Declined_NoMirror`. Google's API doesn't echo back `self: true` on attendees for events authored against a group calendar where the auth user is the data owner, so the harness can't construct an event the production classifier would identify as source-owner-declined. SPEC's declined / tentative paths are unit-tested in `internal/sync/classify_test.go` with synthetic events.

See `doc/e2e-design.md` for the architecture rationale.

## Version history

- v2.0.0: per-pair config scoping, `direction` field removed (BREAKING)
- v2.1.0: location managed-field (v3 schema)
- v2.1.1: 5 fix commits during real-calendar migration
- v2.1.2: nothing user-facing
- v2.1.3: B15 (inherited recurring-instance source-wins bootstrap)
- v2.1.4: B16 (BuildInventory two-pass to skip inherited instances)
- v2.1.5: B18 (per-event transient read error tolerance)
- v2.1.6: B22 (carry on through 410/404 in classify deleteOrSkip)
- v2.1.7: B20 (revive cancelled mirror when source is syncable) + repository docs (CLAUDE.md, next.md)
- v2.1.8: B23 (drift signal field-disagreement detection)
- v2.1.9 (pending): B19 (preserve post-write parent across recurring-handler errors) - committed and pushed; release-please will cut it shortly

## Current state

### Daemon

Running on v2.1.7 via launchd at the time of writing (pid 17773, started 2026-05-04T19:44:11Z). When the next session starts:

```bash
brew update && brew upgrade calendar-sync && calendar-sync version
calendar-sync uninstall && calendar-sync install
calendar-sync status
```

This will pick up B19 (and B23 if you're not yet on v2.1.8 either).

Two pairs in bidirectional sync at `horizon=365d`:

- `work-personal`: source = `tsaleh@coreweave.com`, target = `me@tammersaleh.com`
- `personal-to-work`: source = `me@tammersaleh.com`, target = `tsaleh@coreweave.com`

Functional and idempotent on the steady-state path. **No data corruption risk.** B18's transient-read tolerance is firing 400+ times/day on a known flaky event (CoreWeave Orientation 404s) - working as designed.

### Repo state

`main` matches `origin/main`. No local commits stacked. Untracked files (`.claude/`, `doc/dry-run.err`, `download.html`) are stale and unrelated to this work; can be cleaned up or ignored.

### Recent live writes worth knowing about

- 5 Lunch & Reading mirror instances (5/4-5/8) were stuck at status=cancelled. Manually revived via direct gws patch; the daemon now sees them as `unchanged` (correctly). Once v2.1.7 is in place, future occurrences of this class of stuck-state will self-heal on the next tick (B20).
- 5/11 Lunch & Reading: source patched to 11:00 to match the mirror's existing state per user preference; daemon reconciled cleanly via `action: patch, reason: source_updated`.
- Earlier: B16 recurrence-propagate bug clobbered the personal Lunch & Reading parent (anchor moved 2026-02-23 → 2026-05-20). Recovered manually. Source data is intact.

## F1 + F2 shipped (this session)

### F1 - Calendar refs by display summary

`source` and `target` in `[[pairs]]` now accept an inline TOML table in addition to the existing string forms:

```toml
[[pairs]]
name = "tripit-personal"
source = { summary = "TripIt" }
target = "primary"
```

Plus optional `account = "..."` for disambiguation when multiple calendars share a summary. Implementation went past the original spec on two points: matching is done against `summaryOverride` when set (the user-visible name in the Calendar UI) with fallback to raw `summary`, and account disambiguation prefers `dataOwner` equality with ID-substring as a fallback. Backwards compatible: every existing string form (`primary`, email, group calendar ID) keeps working unchanged.

E2E coverage: `TestE2E_F1_SummaryRefResolves` configures both source and target as summary refs and pins the full TOML→canonicalize→sync round trip.

See SPEC §"[[pairs]]" / "Validation rules" / "Examples" for the new schema.

### F2 - Single-command upgrade

`.goreleaser.yml` now injects a launchctl kickstart into the cask postflight. After release, `brew upgrade calendar-sync` automatically bounces the running daemon - no more `uninstall && install` dance.

Cold install (`brew install` with no agent yet loaded) is handled by a `launchctl print` guard that suppresses stderr so the cask doesn't spew "Could not find service" through brew. Kickstart failures are non-fatal so a transient launchctl issue doesn't break the upgrade transaction.

Limitation: only the default launchd label (`org.calendar-sync.agent`) is restarted. Users who installed with `calendar-sync install --label <custom>` need to keep using the manual dance after `brew upgrade`. Documented in SPEC §"calendar-sync install" / "Upgrades via Homebrew".

The first release that ships F2 will cut the new cask. After it lands, verify the bounce works by running `brew upgrade calendar-sync` against a non-current version, then comparing `calendar-sync version` with `launchctl print gui/$(id -u)/org.calendar-sync.agent | grep pid`.

## Other backlog (lower priority)

### B17 - target-syncToken for sub-tick target-edit propagation (`backlog/B17-target-syncToken.md`)

Target-side edits propagate at FullSync rate (24h default) rather than tick rate (60s default). Architect's design + Codex's review of must-fix items already captured. Phase 1 covers ~80% of cases; Phase 2 needed for mirror-only-override propagation.

This is independent of E2E testing but would benefit from it: target-edit propagation is exactly the kind of thing E2E tests catch.

### Codex full-codebase correctness review

Queued. Catches anything the recent B15/B16/B18/B19/B20/B22/B23 fixes might have missed. Probably worth running AFTER E2E testing infrastructure exists, so the review can include test coverage analysis.

### Cleanup

- `backlog/B18-per-event-error-tolerance.md` is stale (B18 shipped). Canonical doc lives in SPEC.md and `doc/bugs.md`. Safe to delete.
- `backlog/B17-target-syncToken.md` is still relevant.

## Out of scope

- B3 (gws timeouts at 365d horizon) — workaround is `--timeout=30m` on `calendar-sync run`. Daemon doesn't hit this.
- B5 (gws stderr leak into error messages) — cosmetic.
- B8/B9 (launchd plist edge cases) — non-default paths only.

## When this file becomes useless

When the E2E test system is in place, F1 + F2 + B17 all ship, and the daemon has run a quiet week, delete this file. The fix history will live in `doc/bugs.md`; design context in `SPEC.md` and `CLAUDE.md`.
