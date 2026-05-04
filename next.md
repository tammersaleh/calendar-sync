# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`.

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

## NEXT TASK: build a true end-to-end testing system

The user's explicit ask for the next session.

### Why this matters

Five bugs shipped in this session (B18, B19, B20, B22, B23) - all caught only after they manifested in production. Unit tests with the fake-gws harness are fast but don't catch:

- Bugs that depend on Google's actual API semantics (e.g., does Google bump `updated` on a managed-field-no-op patch? does cascading a parent edit bump instance overrides' `updated`?).
- Bugs that surface only with multi-tick state evolution against a real backend.
- Cross-pair race conditions, concurrent edit handling, real recurrence projection behavior.
- Latency-sensitive paths and real timeout behavior.

E2E tests against real Google Calendar would have caught most of B15/B16's bugs and at least the diagnosis path for B23.

### Test calendars

The user has set up dedicated test calendars in their Google account:

- `calendar-sync-test-A`
- `calendar-sync-test-B`
- (potentially more: `-C`, `-D` if multi-pair scenarios are needed)

These are real Google Calendars but isolated from any personal/work data. Tests can create, modify, and delete events freely.

### Design constraints

- **Real `gws` subprocess.** No fake harness. Uses the user's keyring credentials (already configured for the production daemon).
- **Opt-in only.** Must NOT run during `mise run test` since:
  1. Tests hit real Google API and burn quota
  2. They require valid credentials (CI doesn't have them)
  3. They have network dependency
  4. They take orders of magnitude longer than unit tests

  Recommended: Go build tag `//go:build e2e` and a `mise run test:e2e` task that explicitly enables it. Or a `--tags=e2e` flag to `go test`.

- **Idempotent.** Each test cleans up after itself - delete every event it created, even on test failure. Use `t.Cleanup(...)` aggressively.

- **Unique events.** Tests should timestamp event titles or use UUIDs so concurrent test runs don't collide. Same for the calendars themselves: a stale event from a prior run shouldn't cause a flake.

- **Reasonable timeouts.** Real API calls take 100ms-2s each. A full E2E test suite of 20-30 scenarios shouldn't take more than a few minutes.

### What to test (priority order)

1. **Happy-path source-to-mirror sync.** Create source event, run sync, verify mirror exists with correct managed fields, correct extended properties, correct checksum.

2. **Modify source, sync, verify mirror updated.** Patch path. Critical: verify `calendar-sync:source_updated` and `calendar-sync:checksum` get refreshed correctly (the data flow B23's drift-signal blind spot was about).

3. **Delete source, sync, verify mirror deleted.** SPEC step 3 (cancelled).

4. **Recurring parent + source-side instance override.** Verify the mirror has both the parent and the explicit instance override. Then verify modifying the source's instance override propagates to the mirror's instance.

5. **B20 revive.** Create source, sync, manually patch mirror to status=cancelled, verify next sync revives it (`action: insert, reason: source_updated`). This is the test that would have caught the original 5/4-5/8 Lunch & Reading bug.

6. **B23 stale-bookkeeping.** Trickier - need to construct the state where stored bookkeeping says clean but managed fields disagree. One approach: manually patch the mirror's `start` field via direct gws (NOT through calendar-sync), which bumps mirror.updated but doesn't recompute the stored checksum. Then run sync and verify the new `stale_bookkeeping` cell fires.

7. **Bidirectional propagate.** With `propagate_target_edits=true`: edit mirror, run sync, verify source got patched.

8. **Decline / tentative / transparency filtering.** Each one creates the mirror first, then changes source state, verifies the mirror is cancelled (B20-style) or skipped.

9. **Outside-horizon.** Create source event past horizon, verify no mirror. Move it into horizon, verify mirror appears.

10. **409 collision recovery.** Pre-create a mirror with the deterministic ID, then run sync. Verify it's detected and reconciled (or revived if cancelled).

### What's hard or impossible to test E2E

- **B18 transient read tolerance.** Hard to inject a 5xx from Google deliberately. Skip - rely on unit tests.
- **B19 partial-repair-error.** Same - need a mid-flow API failure. Skip.
- **B22 410-on-delete.** Could provoke by deleting the mirror externally between two ticks, but timing-dependent. Maybe leave to unit.
- **Schema migration.** No way to write a v1 mirror via current code. Could be tested by manually constructing the extended properties via direct gws.

### Architecture notes (for the implementer)

- **Where to put the code.** Suggest `internal/e2e/` as a new test package, or a top-level `e2e/` directory. Keeps the build tag and the helpers grouped.
- **Helper structure.** Need a `Setup` that resolves test calendar IDs (probably via gws + matching by summary), creates a config.toml pointing at them, starts the binary in `run --once` mode. Need `Teardown` that lists every event in test calendars and deletes them all.
- **Run mode.** `calendar-sync run` (one-shot) is more amenable to test than `watch` (daemon). Tests should orchestrate: setup → events → run → assert → teardown.
- **State assertions.** After running sync, query the calendar via gws directly and assert on event presence/absence, managed field values, extended properties, status, etc.
- **Avoid the production daemon.** The production daemon must NOT be running against the test calendars (and shouldn't be configured to anyway, since they're separate calendar IDs). Explicit check at test setup that no daemon is running on the test config socket.

### What this would have caught

- B23's 5/11 11:30/11:00 mismatch: scenario "user manually edits mirror.start without bumping source.updated" hits the stale-bookkeeping cell. E2E test would have surfaced the divergence before production.
- B20's stuck-cancelled mirrors: scenario "source flips transparent → opaque" leaves mirror cancelled forever. E2E test detects.
- B16's parent-clobber: write a test that exercises the inherited-instance + propagate flow against real Google. The bug would have surfaced during the test setup, not at user runtime.

## Queued AFTER the E2E system lands

Two features the user wants the next session to tackle as soon as the E2E test infrastructure is in place. Both benefit hugely from being able to assert behavior against real Google calendars.

### F1 - Sync non-primary calendars (the user has more than one calendar per email)

Today the config's `[[calendars]]` block effectively addresses **only the primary calendar** for a given Google account. The resolution path in `internal/config/canonicalize.go` calls `gws calendar calendarList get` and matches by email - which returns the primary. There's no way to say "sync the calendar named TripIt under tsaleh@coreweave.com" or "sync the secondary calendar with id `c_abc123@group.calendar.google.com`."

The user wants to be able to sync any calendar visible in `gws calendar calendarList get` output, not just the primary.

#### Design surface

Two reasonable shapes for the config:

**Option A: explicit calendar ID.** TOML carries the canonical Google Calendar ID directly:

```toml
[[calendars]]
name = "tripit"
id = "c_abc123def@group.calendar.google.com"
```

Pros: unambiguous, no resolution step needed.
Cons: user has to look up the cryptic ID. Most calendars don't have human-readable IDs.

**Option B: name-based lookup within an account.** TOML names a calendar by its display summary, optionally scoped to an account email:

```toml
[[calendars]]
name = "tripit"
account = "tsaleh@coreweave.com"
summary = "TripIt"
```

The canonicalize step calls `calendarList` for the account and finds the entry whose `summary` matches. Errors if zero or multiple match.

Pros: human-friendly. Cons: brittle if user renames calendars; ambiguous if duplicate summaries.

**Option C: hybrid.** Accept either an `id` (preferred) or a `summary` lookup with `account`. Validation rejects providing both.

The user's preference matters here - ask them which they prefer, or implement Option C and let usage decide.

#### Required changes (rough sketch)

- `internal/config/types.go`: extend `Calendar` struct with optional `ID` / `Summary` / `Account` fields. Today it's just `Email string`.
- `internal/config/canonicalize.go`: `resolveCalendar` updated to handle the new shapes. Today it resolves email → primary calendar ID via `CalendarList.Get(email)`. Need a new path that lists all calendars for the user and picks the one matching the supplied id/summary.
- `internal/gws/calendarlist.go`: may need a `CalendarListList()` method (returns the full list) since today only `CalendarListGet(id)` exists.
- `SPEC.md` §"Calendars and pdirs": document the new fields.
- E2E test: create a non-primary calendar via gws, configure it, run sync, verify mirror lands on the right target.
- Backwards compatibility: the existing `email = "..."` form must keep working unchanged. Adding `id` or `summary` is additive.

#### Watch out for

- **Read-only special calendars.** Google has built-in subscribed calendars (holidays, birthdays, weather, sports). They show up in calendarList but are read-only. The existing accessRole check in `config.AccessRoleAtLeast` should handle this, but verify.
- **Subscribed calendars from other accounts.** A calendar can be shared with you but owned by someone else. accessRole tells you what you can do with it, but the calendar's `id` is opaque (`<owner>:<calendar_id>` shape). Make sure the resolution is consistent.
- **TripIt-style read-only feeds.** Likely the canonical use case: TripIt publishes a public iCal feed which you've subscribed to. The resulting calendar is read-only, so calendar-sync can only mirror FROM it (one-way), not propagate edits. The existing `propagate_target_edits` gate should handle this naturally.

### F2 - Single-command upgrade via `brew upgrade calendar-sync`

Today the upgrade dance is:

```bash
brew upgrade calendar-sync   # binary updated at /opt/homebrew/bin/calendar-sync
calendar-sync uninstall      # removes plist + unloads from launchd
calendar-sync install        # installs new plist + loads new daemon
```

Steps 2-3 are necessary because launchd does NOT restart the daemon when the binary file is replaced. The running pid keeps executing the old binary code (cached by the kernel via mmap, even though the on-disk file is replaced).

The user wants `brew upgrade calendar-sync` to "just work" - bouncing the daemon automatically.

#### The mechanism: cask postflight hook

Homebrew casks support a `postflight` Ruby block that runs after the cask install/upgrade. This is the natural hook for restarting the daemon. Sketch:

```ruby
cask "calendar-sync" do
  # ... existing fields ...

  postflight do
    uid = Process.uid
    label = "org.calendar-sync.agent"
    # Only restart if the agent is currently loaded - skip the cold-install case.
    print = system_command "/bin/launchctl",
      args: ["print", "gui/#{uid}/#{label}"],
      must_succeed: false
    if print.success?
      system_command "/bin/launchctl",
        args: ["kickstart", "-k", "gui/#{uid}/#{label}"],
        must_succeed: false
    end
  end
end
```

`launchctl kickstart -k <service>` kills the running process and restarts it under launchd. The `-k` flag forces a clean restart even if KeepAlive would have allowed the running pid to continue.

#### The release-tooling change

The cask is generated by GoReleaser. The repo's `.goreleaser.yml` has a `casks:` block (or similar) that produces the formula in `tammersaleh/homebrew-tap`. To add the postflight, modify the GoReleaser config so each new release ships a cask with the postflight.

Look in:
- `.goreleaser.yml` or `.goreleaser.yaml` in this repo
- The `tammersaleh/homebrew-tap` repo for the current cask file shape

GoReleaser's docs: <https://goreleaser.com/customization/homebrew_casks/> - `custom_block` or similar lets you inject Ruby blocks into the generated cask.

#### Edge cases

- **Cold install.** First-time `brew install calendar-sync` runs the postflight, but no daemon is loaded yet. The `must_succeed: false` and the `print` check above handle this - kickstart only fires if launchctl print succeeds.
- **Multiple users.** The cask runs as the installing user. `Process.uid` gives that user. If the user installs as themselves and the daemon runs as themselves, this is correct. Cross-user scenarios (admin install, regular user runs daemon) probably break - flag for the user.
- **Failed restart.** `must_succeed: false` means a kickstart failure won't fail the brew upgrade. Trade-off: brew succeeds, daemon stays old. Better than blocking the upgrade. The next `calendar-sync status` would catch it.
- **launchd debouncing.** If the user upgrades multiple casks at once, multiple kickstarts could fire. Idempotent - kickstart on an already-restarted daemon is fine.

#### Required changes (rough sketch)

- `.goreleaser.yml`: add a `custom_block` or `postflight` field to the `casks` block.
- Test the generated cask locally: install a v2.1.X release, manually edit the cask file with the postflight, run `brew upgrade` and verify the daemon's `started_at` advances.
- Document the upgrade flow in `doc/install.md` (if it exists) or `SPEC.md`.
- E2E test? Hard - requires a real cask install. Probably skip and rely on manual verification.

#### Out-of-scope alternatives considered

- **calendar-sync self-detects stale binary.** The daemon could check at startup or periodically whether `/opt/homebrew/bin/calendar-sync` differs from its own running binary, and exit gracefully if so (letting KeepAlive respawn it). Adds complexity; the postflight approach is simpler and brew-native.
- **launchd-managed `OnDemand` instead of KeepAlive.** Restructure so the daemon exits frequently and launchd respawns it. Major architecture change for marginal benefit.

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
