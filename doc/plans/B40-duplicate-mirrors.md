# B40 - duplicate mirror parents for one source tuple are silently dropped from inventory

## Context

Live report (2026-10-09): the 🧘/🏃 weekly series on `me@tammersaleh.com` (`k6dsed6sp8hj6jkiovu6tg5bro`) had TWO mirror parents on `tsaleh@coreweave.com`, both carrying `calendar-sync:source = me@tammersaleh.com:k6dsed6sp8hj6jkiovu6tg5bro`: the daemon-inserted deterministic one (`cs2u9fon…`) and a Google-random-ID clone (`tmcjn7bk…`, identical `created` timestamp and identical extendedProperties, so Google cloned it server-side; origin unknown, first seen in logs 2026-09-23). `Inventory` is `map[SourceTuple]*gws.Event`; `BuildInventory` calls `inv.Set` per event, so the last one in Google's list order wins and the other is dropped. The loser is invisible to classify, drift, the target-delta phase and the orphan walk (which iterates inventory tuples). Result: one copy follows source edits, the other is frozen forever. The user sees a duplicate that doesn't move when rescheduled.

The stale `cs2u9fon…` parent was deleted by hand on 2026-10-09; no other duplicate pair exists on either calendar (scanned all v3 mirrors).

SPEC "Deterministic mirror event IDs" promises "No duplicate mirrors, ever", so this is `fix:`.

## Design

Keep `BuildInventory` side-effect free (it is a read step). Detect the collision there, choose a winner deterministically, remember the losers, warn. Let the FullSync orphan walk - the existing mirror-delete path - delete the losers.

### Winner selection (`BuildInventory`, pass 2)

When `inv.Lookup(tuple)` already holds a live event for the tuple being indexed:

1. Newest parseable `Updated` (RFC3339 parsed, never string-compared; unparseable counts as oldest). The copy the daemon has been maintaining is the one that moved most recently - in the live case that was the random-ID clone, not the deterministic one.
2. Else the event whose `ID == mirror.DeterministicID(tuple)`.
3. Else the lexically smaller `ID`.

Codex review (2026-10-09) flipped 1 and 2: once two live mirrors exist the deterministic ID says nothing about which copy is current, and preferring it would have deleted the active copy in the reported incident.

Losers go into `Inventory.duplicates map[SourceTuple][]*gws.Event`. Collisions are resolved as events stream in; the WARN (`sync.BuildInventory: duplicate mirrors for source tuple`, with target, tuple, winner ID, sorted loser IDs) is logged once per tuple after pass 2 so three-way collisions report the final winner. Add a `duplicates` count to the `sync.BuildInventory complete` line.

Only parents, non-recurring events and managed instances reach `inv.index`; the inherited-instance filter already runs first, so this never fires on B16's inherited instances.

Pass 2 runs parents/non-recurring first, then instances. An instance whose `RecurringEventID` is a LOSING parent is demoted: it loses any collision against an instance under a surviving parent regardless of `Updated` (Codex finding: otherwise the parent and instance tuples can pick winners from opposite series and the orphan walk deletes the losing parent, cascading to the "winning" instance, then deletes the surviving parent's instance as the recorded loser - no copy left).

### Deletion (`OrphanWalker.Walk`)

New first step before the visited filter: for each duplicate loser whose tuple references `w.SourceCalendarID`, `events.delete` it on the target (404/410 swallowed as today), emit `Outcome{Action: delete, Reason: "duplicate_mirror", SourceEventID: tuple.EventID, TargetEventID: loser.ID}`, and drop it from `duplicates` via `Inventory.dropDuplicate` (NOT `Inventory.Delete`, which removes the winner). Only drop on success/404/410 so a real delete failure stays recorded for the next FullSync. Ignore `visited` - a visited tuple is the normal duplicate case. No `events.get` needed - the loser is redundant regardless of source state. Deleting a parent cascades to its instances on Google's side.

Rationale for the orphan walk rather than per-tick: the walk runs on FullSync only (startup + every 24h), it already owns mirror deletion with the right error handling and outcome plumbing, and duplicates are rare. A mirror-side edit pending on the loser is lost, but it is already lost today (nothing tracks the loser).

### Not in scope

- `mirror list` surfacing duplicates (the orphan walk clears them within one FullSync).
- Finding out what makes Google clone an event.

## Files

- `internal/sync/inventory.go`: `duplicates` field, `addDuplicate`/`dropDuplicate`/`loserIDs` (unexported; orphan.go is the same package), two-phase pass 2 with demotion, `preferMirror`, once-per-tuple warn, `duplicate_tuples` on the complete line.
- `internal/sync/inventory_test.go`: winner table (both orders), three-way with log assertions, same-ID guard, hierarchy test (parents + instances, three orders, walk deletes only the losing series), B16 test asserts no duplicate recorded.
- `internal/sync/orphan.go`: `ReasonDuplicateMirror`, `deleteDuplicates` step 0 in `Walk`.
- `internal/sync/orphan_test.go`: losers deleted for own source only, no `events.get`, winner untouched, retry on failure.
- `SPEC.md`, `doc/bugs.md` B40, `CLAUDE.md` note, `next.md`.

## Steps

- [x] Codex plan review (thread in scratchpad codex/b40; accepted: newest-Updated-first, once-per-tuple log, separate drop op, retry-on-failure; rejected: dropping CLAUDE.md/next.md updates - project workflow requires them)
- [x] Tests (red) for inventory + orphan
- [x] Implement
- [x] `mise run check`
- [ ] code-reviewer pass, fix, second pass (first pass done: vacuous fractional-seconds case, missing log coverage, same-ID guard - all fixed; Codex: hierarchy demotion - fixed)
- [ ] SPEC/bugs/CLAUDE/next docs
- [ ] Commit `fix:`, push, wait for release, install, restart daemon, verify logs
