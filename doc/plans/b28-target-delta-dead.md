# B28 - target-delta phase dead in production; recurring target edits clobbered

Plan doc. Read this, then `doc/bugs.md` B28/B29/B30, then `CLAUDE.md`.

Branch: `fix/target-token-pagination`.

## How this started

User reported: "why isn't the exercise event on my personal calendar updated
after I updated the coreweave version?"

The 🧘/🏃 series lives on `me@tammersaleh.com` (weekly TU, 08:45-09:30). The
CoreWeave copy is the mirror. The user dragged the Jul 28 occurrence on the
mirror to 09:00. With `propagate_target_edits = true` and a writable source,
B17's target-delta phase should have pushed that back to the source. Nothing
happened - for hours, across many ticks.

## Root cause chain

Three independent defects, found in this order.

### B28 - `gws --page-all` silently truncates at 10 pages

`internal/gws/events.go:EventsList` ran `gws calendar events list --page-all`
and never passed `--page-limit`. **gws defaults `--page-limit` to 10.** At
`maxResults=250` that caps any list at 2500 events.

`seedTargetSyncTokens` lists the ENTIRE target calendar with no time bounds
(sync tokens are incompatible with `timeMin`/`timeMax`). Production sizes:

| calendar | events | pages needed |
|---|---:|---:|
| `tsaleh@coreweave.com` | 3470 | 14 |
| `me@tammersaleh.com` | 10244 | 41 |

Both exceed 10. gws stopped at page 10; that page carried `nextPageToken` and
no `nextSyncToken`. `EventsList` returned `("", nil)` - **a partial prefix
reported as success**. `seedTargetSyncTokens` stored `""`, and
`runTargetDeltaPhase`'s `if !ok || token == ""` skipped the target on every
tick.

**B17's entire target-delta phase has been dead since v2.4.0 shipped**, for
both targets, silently.

Evidence: daemon debug log shows `sync.seedTargetSyncTokens: seeded ...
token_present:false` for both targets, and every tick makes exactly two
`gws.EventsList` calls (the two source deltas) and zero target lists.

Why some target edits still worked: the source-side classify path also detects
mirror drift and propagates. That covers any target edit whose source event
appears in the source list. It does NOT cover a recurring occurrence with no
materialized source exception - the source list (`singleEvents=false`) never
contains it, at any FullSync, forever.

### B29 - inherited recurring mirror instances clobber genuine target edits

Even with the token fixed, this specific event would still be reverted.

`processTargetDeltaEvent` takes the B17-Phase-2 `materializeSourceOverride`
path only on a **404** from `events.get` on the constructed source-instance ID.
But Google returns **200** with a synthesized *virtual* occurrence when the
series has no exception at that slot. So the code falls through to `Classify`
→ `recurring.applyDriftMatrix`, where `IsInheritedRecurringInstance` is true
(Google copies the parent's extendedProperties onto a user-created override, so
`calendar-sync:source` carries the source PARENT id), the recomputed
`MirrorDrifted` is true, and the `case signal.MirrorDrifted:` cell **source-wins**
- patching the mirror back to 08:45 with `conflict: inherited_source_won`.

Confirmed by observation test: `action=patch reason=source_updated
conflict=inherited_source_won`, zero writes to the source.

`isInherited` cannot distinguish:
- (a) auto-materialized bootstrap instance whose fields are junk → source must win;
- (b) user-created override on the target → target intent, should propagate.

The guard exists for (a) and that hazard is real (B15). It must be preserved.

#### Discriminators that do NOT work

Verified empirically against the live calendars:

| signal | why it fails |
|---|---|
| `events.get` 200 vs 404 | Google returns 200 for virtual occurrences. This is the bug. |
| `etag` instance vs parent | A real override and its parent share the SAME etag. |
| `sequence` | A virtual instance can have `sequence` 2 while its parent has 1. |
| `start != originalStartTime` | Only detects moved exceptions; misses title/end/status changes. |
| `SourceChanged` | Pinned false for a genuine source exception by an existing test (`handler_test.go`). |
| compare source instance's managed fields to the parent's projection | **Killed by real data.** Jul 14 is a *cancelled* exception whose `start == originalStartTime`. `status` is deliberately not a managed field (adding it would force a checksum migration across every v3 mirror), so this reports "no source intent" and would resurrect a deliberately-cancelled occurrence. |

#### The sound discriminator: collection membership

A complete unexpanded `events.list(singleEvents=false)` contains real
exceptions and does not manufacture virtual ones. Live proof for this series:

```
k6dsed...                    start=2026-06-16T08:45  ost=-      confirmed  (parent)
k6dsed..._20260616T154500Z   start=07:15  ost=08:45  cancelled
k6dsed..._20260623T154500Z   start=09:15  ost=08:45  confirmed
k6dsed..._20260630T154500Z   start=09:15  ost=08:45  confirmed
k6dsed..._20260707T154500Z   start=10:45  ost=08:45  confirmed
k6dsed..._20260714T154500Z   start=08:45  ost=08:45  cancelled
k6dsed..._20260721T154500Z   start=10:15  ost=08:45  confirmed
k6dsed..._20260804T154500Z   start=09:15  ost=08:45  confirmed
k6dsed..._20260811T154500Z   start=07:00  ost=08:45  confirmed
```

Jul 28 is absent. Genuinely virtual.

### B30 - `[settings].log_level` never reaches the logger

`cmd/cli.go:133` builds the logger from CLI globals only, before config is
loaded. `[settings].log_level` / `log_format` are parsed, defaulted, and
validated - then ignored. launchd passes no log flags, so production always
ran at the built-in default. This is why the dead phase stayed invisible for
months.

Also: invalid values (`--log-level=trace`) currently parse and silently fall
back inside `NewLogger` instead of failing as usage errors.

## Rollout hazard - READ BEFORE SHIPPING ANY PART

Fixing B28 alone **wakes a phase that has been dead for months** against two
large live calendars. Two paths become newly reachable and are destructive:

1. **B29** - the next genuine recurring target edit gets clobbered.
2. **Target cancellations** - target deltas use `ShowDeleted: true`.
   `recurring/handler.go` (B20 revive cell) and `sync/reconcile.go` both
   **revive** a cancelled mirror whose source is live, and the reverse patch
   body omits `status`. So waking the phase means user-deleted occurrences get
   resurrected.

Therefore B28's fix must NOT ship alone. Ship the whole cut below, or leave
`propagate_target_edits` off.

## The cut (reviewed with Codex; it rejected the first, smaller version)

### Ship together

1. **Logging precedence** (B30). Independent, but land it first - the rest
   needs visible diagnostics.
   - Precedence per field, resolved independently: CLI flag > env > config >
     loader default. `--log-level=warn` must keep `settings.log_format`.
   - Globals become optional (`*string`) with kong `enum` so absent ≠ empty
     and invalid values are usage errors. No kong `default:` (that would
     shadow config again).
   - Keep a bootstrap logger for configless commands and config-load failures;
     rebuild `rt.Logger` in the shared `loadConfig` path after a successful
     load, before any gws/reconciler/feed object captures it.
   - Do NOT mutate `cfg.Settings` with CLI values.

2. **Pagination completeness** (B28). **DONE** - see Status.
   - Explicit `--page-limit` on `EventsList` and `CalendarListList` only.
   - Fail closed when the terminal page still carries `nextPageToken`; return
     no partial data.
   - Leave `EventsInstances` alone: its callers are `MaxResults=1` existence
     probes where truncation is the expected outcome.

3. **Target-token seeding invariant.**
   - `seedTargetSyncTokens` seeds **only when the token is missing**. A valid
     existing token is never overwritten by FullSync.
     Without this, every periodic FullSync re-seeds past an unconsumed target
     edit and loses it - which would make item 5 mandatory rather than
     deferrable.
   - A successful list returning an empty token is an error, not a stored `""`.
     Map invariant: `entry present ⇒ nonempty and usable`.
   - Warn on state transition only, never per-tick.
   - Do NOT set `NeedsFullResync` when the *source* membership read failed -
     the target token is fine; reseeding would lose the event.

4. **Source-exception membership catalog** (B29).
   - Shape: per source calendar `{readiness: Ready|Unknown, coverage,
     byID: instanceID→parentID, byParent: parentID→set(instanceID)}`.
   - Lookup returns four states: `Present | Absent | Unknown | OutOfScope`.
     Readiness is per-calendar; membership is per-occurrence.
   - Cancelled source exceptions count as **Present** (the Jul 14 case).
   - Built at FullSync from the complete source list; swapped atomically only
     after pagination is proven complete. Updated from each successful source
     delta via a staged overlay.
   - `Absent` is only authoritative inside the snapshot's horizon coverage;
     outside it, return `OutOfScope` and make no source write.
   - After a successful reverse materialization, insert the new instance ID
     into the catalog **immediately**, before the mirror metadata rewrite, so a
     retry after a failed rewrite sees `Present` instead of duplicating.
   - Decision table in the inherited branch:

     ```
     Present                        -> keep inherited source-wins bootstrap
     Absent + GET 200 + drift       -> materialize source override (target wins)
     Absent + GET 200 + no drift    -> metadata bootstrap only, no source write
     Absent + GET 404               -> terminal skip, no source write
     Unknown                        -> zero writes for the batch, pin the token
     OutOfScope                     -> no source write, horizon policy
     ```

     The old "404 means no source override, therefore materialize" assumption
     is wrong and its test must change.

5. **Staged read ordering + batch preflight.**
   - Order: stage target-delta **reads** → source-delta reads + catalog overlay
     → preflight the whole target batch → target writes → source writes →
     commit tokens.
   - Target-read before source-read is deliberate: it puts the source view as
     close as possible to the reverse write, narrowing the window where a
     just-created source override is missed. (The reverse order is strictly
     worse.)
   - Preflight the entire batch: if ANY event depends on an `Unknown` source
     calendar, perform **zero** writes for that target and leave the token
     unadvanced. Without batch preflight the safe prefix gets rewritten every
     60s while one later event pins the token.
   - This preserves B17's invariant that target writes precede source-driven
     mirror rewrites.

6. **Target-cancellation quarantine.** While reverse cancellation is deferred,
   every target-delta event with `status=cancelled` is: not classified, not
   materialized, not revived, no source write, structured warning, and
   **consumed** for token purposes. Consumed rather than pinned - otherwise the
   first deletion head-of-line blocks every later target delta.

### Deferred, each needs a `doc/bugs.md` entry

1. **No FullSync recovery scan for inherited target exceptions.**
   `BuildInventory` drops them. So there is no automatic repair for: edits made
   while the daemon was stopped, edits predating startup, anything after a
   target-token 410, quarantined cancellations, or occurrences that enter the
   horizon later. Safe to defer ONLY because of the seed-only-when-missing
   change above.
2. **Reverse target cancellation** (the quarantine's permanent fix).
3. **`EventsInstances` first-page vs exhaustive split.**
4. **General FullSync concurrent target-edit/write race** (pre-existing, not
   observed).
5. **Prolonged source-catalog outage** pins the target cursor; backlog grows
   and the token can eventually 410, at which point deferred item 1 bites.

## Status

All six shipped items are DONE, reviewed clean, and merged to local `main`
(6 commits, unpushed - the SSH agent was unreachable).

- [x] Diagnosis, all three defects confirmed against live calendars.
- [x] User's Jul 28 event repaired. Both sides 09:00-09:45, and the mirror's
      `calendar-sync:source` upgraded to per-instance form, so that
      occurrence is out of the inherited state entirely.
- [x] Item 1 (logging precedence, B30).
- [x] Item 2 (pagination, B28). Verified against real gws: `--page-limit
      1000` is accepted and the terminal page has no `nextPageToken` and a
      populated `nextSyncToken`.
- [x] Item 3 (target-token seeding invariant, B28).
- [x] Item 4 (membership catalog, B29) - `internal/sync/catalog.go`,
      `internal/recurring/membership.go`.
- [x] Item 5 (staged reads + batch preflight).
- [x] Item 6 (cancellation quarantine).
- [x] `doc/bugs.md`: B28/B29/B30 fixed; B31-B36 open.
- [x] SPEC.md + CLAUDE.md corrections.
- [x] Code review - clean, no critical or data-loss findings. It verified
      the quarantine specifically (no path from a target tombstone reaches a
      revive cell), the readiness gating (a stale index can never answer
      Absent), the `MembershipPresent` zero-value default (only
      `buildTargetDeltaClassifier` wires the real lookup; the source-delta
      path correctly defaults to Present), and that the modified tests were
      strengthened rather than weakened. One observation became B36.
- [x] Temporary local daemon changes reverted (see below).
- [ ] **PUSH** - blocked on the SSH agent, nothing else.
- [ ] After push: wait for the release, `brew upgrade --cask calendar-sync`,
      `launchctl kickstart -k "gui/$(id -u)/org.calendar-sync.agent"`, then
      watch the first ticks (see "What to watch after shipping").

## What to watch after shipping

This release wakes a phase that has been dead since v2.4.0, so the first
hours are the interesting ones.

The startup FullSync seeds both target tokens fresh, so the first delta
contains only changes made after the seed. There is no stampede over
history - but every subsequent mirror-side edit now acts.

Expect in `~/Library/Logs/calendar-sync/calendar-sync.err.log`:

- No more `token_present:false`. The seed should now produce real tokens.
- `sync.targetDelta: no target syncToken` should NOT appear. If it does,
  the seed is still failing and two-way sync is still off.
- `sync.sourceCatalog: rebuilt` per source on each FullSync, with a
  plausible exception count.

Watch stdout (`calendar-sync.out.log`) for the newly-reachable outcomes:
`propagate(mirror_only_override)`, `skip(target_cancelled)`,
`skip(instance_not_in_series)`, `skip(outside_catalog_coverage)`. The first
two are the ones to sanity-check against what you actually did in the UI.

Red flags: any `conflict=inherited_source_won` on an occurrence you edited
on the mirror side (that would mean membership said Present when it should
have said Absent), or a mirror-side deletion coming back.

## Temporary local changes to revert before finishing

All reverted and verified:

- `~/.config/calendar-sync/config.toml`: `log_level` back to `info`.
- `~/Library/LaunchAgents/org.calendar-sync.agent.plist`:
  `CALENDAR_SYNC_LOG_LEVEL` removed, daemon re-bootstrapped, confirmed
  emitting INFO again.
- `internal/sync/scratch_b28_test.go`: deleted; the real regression test is
  `TestTargetDeltaPhase_VirtualOccurrence_MaterializesSourceOverride`.

Note the installed daemon is still v2.6.1, which predates B30 and therefore
ignores `settings.log_level`. To get debug out of the CURRENT binary you
need the env var in the plist. After this release ships, `log_level` in
config.toml works on its own.
