# B17 — target-syncToken for sub-tick target-edit propagation

Pending. Surfaced in the same session as B16 but deferred so B16 (data corruption) could ship clean.

## Problem

`calendar-sync` mirrors events from a source calendar to a target calendar. With `[settings].propagate_target_edits = true` the gate is open: edits to the mirror on the TARGET propagate back to the SOURCE.

The one-shot `run` command does this correctly. It performs a full source-list, computes drift on each (source, mirror) pair, and routes target-only edits through the propagate path.

The `watch` daemon's per-tick reconciliation is driven by Google's `events.list` `syncToken` on the SOURCE calendar. A pure target-side edit produces zero change on the source, so the source's syncToken delta returns nothing. Drift is never detected. The user's target edit sits undetected until the next FULL re-sync (default `[settings].full_sync_interval = 24h`).

Observed live during the personal→cw rollout: user edited the `Lunch & Reading` 5/20 instance on the cw target by +30 min. The daemon's incremental ticks were idle for hours because nothing on personal had changed. A manual `calendar-sync run --pair personal-to-work` (which does a full source-list) caught it.

The user's expectation: a target edit should propagate within ~one tick (~60s) of being made, not 24h.

## Architect's design (medium scope, accepted as the right primitive)

Add a separate per-target syncToken stream and run a target-delta phase on every Tick.

### State

`Reconciler` gains:

```go
targetSyncTokens map[string]string  // canonical target -> latest token
```

Parallel to the existing source `syncTokens`. Maintained only for targets where some pdir has `effectiveSourceWritable=true` (`pd.SourceWritable && pd.PropagateTargetEdits`); idle targets that can't propagate target edits never list.

### FullSync changes

Seed the per-target token BEFORE rebuilding the inventory:

1. For each writable-source target, issue `events.list` with empty `SyncToken` and consume `nextSyncToken`. Capture into `targetSyncTokens[T]`.
2. Then run the existing `BuildInventory` pass.

Seeding before inventory build is essential. If we seed after, an edit landing in the gap is invisible to inventory (already snapshotted) and to the seeded token (which starts after the edit) — missed forever.

### Tick changes

Run the target-delta phase BEFORE the existing source-delta classify:

1. **Target-delta phase**: for each writable-source target T:
    - `events.list(CalendarID=T, SyncToken=targetSyncTokens[T], ShowDeleted=true, MaxResults=250)`.
    - 410 GONE: `delete(targetSyncTokens, T)` and surface `NeedsFullResync` for that target. Skip rest of the target-delta phase for T.
    - For each delta event E:
        - Skip if `E.extendedProperties.private["calendar-sync:source"]` is unset (not a mirror; defensive).
        - Parse source-tuple from `calendar-sync:source`. Find the **single** owning pdir P where `P.target == T && P.source == tuple.CalendarID && P.SourceWritable && P.PropagateTargetEdits`. Skip if no match.
        - Dispatch to the source-side reconcile path (see "Dispatch routing" below).
    - Advance `targetSyncTokens[T] = nextSyncToken` only if the delta processing had no errors.
2. **Source-delta phase**: existing `incrementalListSources` + `runPDirTick` logic, unchanged.

The phase order is critical. If source-delta runs first, a source-side patch from that phase can rewrite the mirror before the target-delta phase notices the user's target edit, silently clobbering it.

### Dispatch routing

For each target-delta event E with parsed source-tuple `(source_cal, source_event_id)`:

- If `E.recurringEventId == ""` (parent or non-recurring):
    - `events.get(source_cal, source_event_id)`.
    - 404: skip with reason `source_orphan` (orphan walk handles).
    - 200: dispatch through `Classifier.Classify(ctx, source)`. The 8-step switch handles non-recurring events at step 8, recurring parents at step 8 (parent path), etc.

- If `E.recurringEventId != ""` (recurring instance):
    - Determine source instance ID:
        - **Managed form**: `tuple.EventID` already has the `_<UTC>` suffix. Use it directly.
        - **Inherited form** (auto-materialized instance — the case that bit us in B16): `tuple.EventID` is the source PARENT's id without suffix. Compute `source_instance_id = tuple.EventID + suffix_from_E.id`, where `suffix_from_E.id` is the `_<UTC>` portion of the mirror instance's id (deterministic match to the source's RRULE projection time).
    - `events.get(source_cal, source_instance_id)`.
    - 200: dispatch through `Classifier.Classify(ctx, sourceInstance)`. Step 2 routes to the recurring handler.
    - 404: source has no override at this occurrence. **This is "mirror-only override" territory** — see Phase 2 below.

### Self-write suppression

Architect's claim: free, because the post-write checksum on the mirror matches what we'd recompute. Codex partially agreed:

> For normal explicit mirrors the architect is right: if checksum is computed from the post-write Event resource, the next target delta classifies as `unchanged`.
> But this is not universally true. The big exception is recurring auto-materialized instances. A parent rewrite can cause child instance deltas whose inherited `calendar-sync:checksum` is the parent's checksum, so they look drifted even with no user edit.

The B15 inherited-instance branch already routes that case through the bootstrap source-wins path (`inherited_upgrade` / `inherited_source_won`). Verify in tests that the target-delta phase for an inherited instance correctly hits the bootstrap path and that subsequent ticks suppress the self-write.

### 410 GONE recovery

Same shape as the existing source-syncToken recovery: clear the token, mark the target as needing a fresh seed, the next FullSync re-seeds. Add this branch to the target-delta phase.

## Codex's review — must-fix items

Thread `019def25-bb67-70f3-adc8-00283b6a4cb2` (likely expired; full body archived in next.md commit message history). Six issues:

1. **Phase ordering** (must-fix). Target-delta MUST run before source-delta within a Tick, OR both deltas must be staged before any writes. Otherwise source-driven mirror rewrites can clobber target edits before target-delta lists them.

2. **Recurring-instance dispatch** (must-fix). For target-delta events with `recurringEventId` set, the dispatch can't stay flat. The path must reuse the recurring-handler semantics. Architect's design accommodates this via "dispatch through Classifier"; the Classifier's step 2 already routes recurring instances. Pin in tests that this branch fires.

3. **Token seeding before inventory rebuild** (must-fix). Detailed above.

4. **Routing dedup** (must-fix). Target-delta event must route to **one** owning pdir, not all pdirs sharing the target. Match by parsed source tuple == pdir source.

5. **Self-write suppression** (caveat). Normal mirrors are fine; recurring auto-materialized instances need the B15 bootstrap path verification.

6. **Other gaps to cover**:
    - **410 GONE recovery for target syncTokens**.
    - **Target-side cancelled/deleted mirror events policy**. Tombstones come back via `ShowDeleted:true`. SPEC's existing limitation says target-only deletes aren't reconciled (mirror-only overrides). Confirm or change.
    - Missing tests for: simultaneous source+target edit in one tick, inherited recurring instance target edit, shared-target routing/dedup, target-token 410 recovery.

## Mirror-only override (Phase 2)

The `events.get(source_instance_id)` 404 path means the user's target edit is a "mirror-only override" — they edited an occurrence on the mirror that has no source counterpart. Per SPEC §"Limitation: mirror-only recurring instance overrides", the existing daemon does not reconcile these.

The user's specific Lunch & Reading 5/20 case is exactly this scenario. The cw mirror was an auto-materialized instance with `calendar-sync:source` inherited from the parent (no instance suffix); editing it didn't create a source-side override automatically.

Two options:

- **Phase 1 (deferred)**: continue documenting as a SPEC limitation. User's instance edits propagate at the 24h boundary via the FullSync source-list pass (which sees the source parent and indirectly catches drift via the recurring handler's drift matrix on each instance).
- **Phase 2 (full fix)**: when target-delta phase hits a 404 on the source instance lookup, `events.patch(source_cal, source_instance_id, mirror_managed_fields_minus_recurrence)` to create the source override, then re-fetch and dispatch normally. This is what the user is asking for when they enable `propagate_target_edits=true` and edit a single occurrence.

Phase 2 introduces a new write path that creates source overrides. Risk: the patch body must NOT include `recurrence` (instance overrides don't have recurrence; the B16 trigger was exactly this — recurrence in a body sent to an instance ID that Google reinterpreted as a parent-level update). The patch body is essentially `mirror.BuildPropagatePatchBody(E, drifted_fields)` minus recurrence, scoped to the per-instance ID.

Decision deferred. Phase 1 ships first; Phase 2 lands as a follow-up if user observation supports it.

## Concrete file change list

- `internal/sync/reconciler.go`:
    - Add `targetSyncTokens map[string]string` field; initialize in `New`.
    - Add `uniqueWritableTargets() []string` helper.
    - Add `seedTargetSyncTokens(ctx) map[string]error` method called from `FullSync` BEFORE `rebuildInventories`.
    - Add target-delta phase invocation in `Tick` BEFORE the source-delta classify pass.
- `internal/sync/target_delta.go` (new):
    - `runTargetDeltaPhase(ctx) (map[string]Counts, error)` — lists target deltas, dispatches per event.
    - `processTargetDeltaEvent(ctx, target, event, counts)` — owning-pdir lookup, source-fetch, dispatch through Classifier.
    - `extractInstanceSuffix(mirrorID) (string, bool)` — parse the `_<UTC>` suffix from a mirror instance id (regex `_\d{8}T\d{6}Z$`).
- `internal/sync/target_delta_test.go` (new): see test plan.
- `internal/recurring/handler.go`: probably no changes; the existing inherited-instance branch (B15) handles the bootstrap case for self-write suppression.
- `cmd/run.go`: in `WatchCmd` runtime, propagate target-delta counts into the daemon's `_meta` trailer. May be a separate `target_delta` block to keep source-delta counts unchanged.
- `internal/daemon/socket.go`: optional — surface `target_sync_token_age` in IPC for debugging.
- `SPEC.md`:
    - §"In-memory state": add `targetSyncToken` per writable-source target.
    - §"Daemon lifecycle: per-tick reconciliation": add a phase 1 (target-delta) ordered before the existing source-delta.
    - §"Daemon lifecycle: startup" / §"periodic full re-sync": add the seed step.
    - §"Limitation: mirror-only recurring instance overrides": update with reference to B17 Phase 2 status.
    - §"What full re-sync catches": "Mirror drift on currently-eligible source events" bullet now also at tick granularity for non-mirror-only edits.
- `doc/bugs.md`: B17 entry under Fixed.

## Test plan

Unit tests in `internal/sync/target_delta_test.go`, using the existing `stubAPI` harness:

1. `TestTargetDeltaPhase_NonRecurringEdit`: target edit on non-recurring mirror -> propagate to source, source patched, mirror rewritten with fresh checksum, inventory updated.
2. `TestTargetDeltaPhase_RecurringParentEdit`: target edit on a recurring parent mirror -> propagate to source parent.
3. `TestTargetDeltaPhase_ManagedInstanceEdit`: target edit on a recurring instance with `_<UTC>` suffix in source-tuple -> events.get source instance succeeds, dispatch through recurring handler, propagate.
4. `TestTargetDeltaPhase_InheritedInstanceEdit_Phase1Skip`: target edit on a recurring instance with inherited (parent-form) source-tuple, source has no override at that occurrence -> events.get source instance returns 404, skip with reason `mirror_only_override`. (Phase 2 changes this to actually propagate via source override creation.)
5. `TestTargetDeltaPhase_SelfWriteSuppression`: post-write delta returns the freshly-rewritten mirror -> `skip(reason=unchanged)`, no extra patches.
6. `TestTargetDeltaPhase_InheritedAutoMaterializedNoUserEdit`: parent insert auto-materializes instances which appear in the target delta; the inherited-instance path should hit B15's bootstrap (`inherited_upgrade` -> upgrades the instance to managed form), not propagate.
7. `TestTargetDeltaPhase_NotMirror`: target-delta returns a user-created event with no `calendar-sync:source` -> skipped silently.
8. `TestTargetDeltaPhase_NoOwningPdir`: target-delta returns a stray mirror whose source-tuple doesn't match any pdir's source -> skipped (defensive).
9. `TestTargetDeltaPhase_NonWritableTarget`: target with no writable-source pdir -> events.list NOT called for that target.
10. `TestTargetDeltaPhase_410Recovery`: ErrAPIGone clears `targetSyncTokens[T]` and surfaces NeedsFullResync.
11. `TestTargetDeltaPhase_TokenAdvancementOnSuccess`: clean delta processing -> token advances.
12. `TestTargetDeltaPhase_TokenStaysOnError`: classify error during delta processing -> token does NOT advance, next tick re-delivers.
13. `TestSeedTargetSyncTokens_RunsBeforeInventoryRebuild`: ordering pinned via call sequence on the stubAPI's recorded-call log.
14. `TestTickPhaseOrdering_TargetBeforeSource`: target-delta phase runs before source-delta phase in `Tick`.

Plus a regression test in `internal/sync/reconciler_test.go`:

15. `TestTick_PropagatesTargetEdit_OneTick`: full-sync, then user edit on target, then Tick — expect propagate within that single tick. This is the user-observable scenario the whole feature exists for.

## Risks

- New write paths increase blast radius. Phase 1 only writes to source via the existing propagate path which we just trust-but-verified after the B15 + B16 fixes; should be safe. Phase 2 introduces source-override creation which is a new write path that the existing tests don't cover; risk it ships with edge cases (the B16-style recurrence inclusion bug being the obvious one to guard).
- Extra `events.get` per drifted target-delta event. On idle ticks the delta is empty; only the target-delta `events.list` fires (one per writable target) plus zero gets. On a user-edit tick the per-event get is unavoidable. Quota cost minimal.
- Target `events.list` with `syncToken` rejects `TimeMin`/`TimeMax` filters (Calendar API constraint, same as source). Out-of-horizon target edits could surface; `processTargetDeltaEvent` should re-apply the same `outside_horizon` check Classifier does so a stale edit on an event past horizon doesn't propagate.
- Concurrent pair runs (same target serving two pdirs in different directions): not a hazard if Tick stays single-threaded, but route delta events to ONE owning pdir per Codex's must-fix #4.

## When this file becomes useless

When B17 ships (Phase 1 minimum, Phase 2 if pursued), tests pass, Codex review is clean, daemon has run a full week without target-edit-related anomalies, delete this file. The feature lives in SPEC.md and `doc/bugs.md` from then on.
