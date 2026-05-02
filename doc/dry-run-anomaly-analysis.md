# Dry-run anomaly analysis (calendar-sync v1.0.0)

Two anomalies surfaced in the install-test dry-run captured at `doc/dry-run.out` + `doc/dry-run.err`. Both are diagnosed below against the v1.0.0 source as it stood before this branch's instrumentation diff. File:line references are pinned to v1.0.0; the instrumentation in commit `0a3869a` adds debug logging at every dispatch point named here, so a future debug run lights up the same execution path.

## Anomaly #1: bogus `migration_source_won` patches with no v1 mirrors present

### Observed

14 `patch` outcomes carry `conflict: migration_source_won`. Every one of them sits next to an `insert` outcome on the SAME `source_event`. Example:

```
{"action":"insert", "source_event":"22si2itigr2gho9abp8dk4kh8v_R20260323T163000", "target_event":"cs284...", "reason":"source_updated"}
{"action":"patch",  "source_event":"22si2itigr2gho9abp8dk4kh8v_R20260323T163000", "target_event":"cs284...", "reason":"source_updated", "conflict":"migration_source_won"}
```

The user verified that the target calendar (`me@tammersaleh.com`) returns zero events for the `BuildInventory` queries (`privateExtendedProperty=calendar-sync:version=1` and `=2`) and zero events for any `calendar-sync:source=tsaleh@coreweave.com` query. So at run-start there are no v1 (or v2) mirrors. Yet the dry-run reports v1-migration patches.

### Root cause

A combination of two contributing factors. Both are bugs - the migration_source_won outcomes are incorrect.

#### Cause A: `dryRunAPI.EventsPatch` echoes only the request body, losing the original mirror's extended properties

`cmd/run.go` line 278-287 (v1.0.0):

```go
func (d *dryRunAPI) EventsPatch(_ context.Context, _ string, eventID string, body *gws.Event) (*gws.Event, error) {
    if body == nil {
        return &gws.Event{ID: eventID}, nil
    }
    out := *body
    if out.ID == "" {
        out.ID = eventID
    }
    return &out, nil
}
```

The wrapper has no memory of the prior insert's resource. It returns only what the caller passed in.

The followup-checksum patch (`internal/sync/helpers.go:43-55`, `followUpChecksum`) passes a body containing ONLY the checksum:

```go
follow := &gws.Event{
    ExtendedProperties: &gws.ExtendedProperties{
        Private: map[string]string{mirror.ExtKeyChecksum: checksum},
    },
}
return c.API.EventsPatch(ctx, calendarID, eventID, follow)
```

In production, Calendar API merges the patch into the existing resource and returns the merged result, which carries `calendar-sync:version=2`, `calendar-sync:source`, etc. In dry-run, the wrapper returns only the patch body. The result of `followUpChecksum` therefore has extended properties limited to `{checksum: "..."}` - no version, no source.

`completeInsert` (`internal/sync/insert.go:55-71`) then writes that broken event into the in-memory inventory:

```go
final, err := c.followUpChecksum(ctx, c.TargetCalendarID, inserted.ID, inserted)
...
c.Inventory.Set(tuple, final)   // final has version="" (broken)
```

Test that pins this behavior: `cmd/run_test.go:TestDryRunAPI_PatchReturnsBodyOnlyAndDropsOriginalExtProps`.

#### Cause B: the source-list returns the same source-tuple twice

Every one of the 14 events showing `migration_source_won` has a corresponding INSERT in the same dry-run pass with the same target event ID. That's only possible if the source list returned the same event twice, OR if a single event got routed through `c.Classify` twice in the same `runClassifyLoop` pass. We don't have direct evidence of which (the v1.0.0 code emits no debug log on per-event entry); the instrumentation in this branch adds it. A future debug run will pin which.

The dispatch point: `internal/sync/reconciler.go:689-715` `runClassifyLoop` iterates `events []gws.Event` straightforwardly:

```go
for i := range events {
    ev := events[i]
    ...
    if err := c.Classify(ctx, &ev); err != nil { ... }
}
```

So if `events` contains a duplicate, the same source ID enters Classify twice. Possible reasons the source list could double-deliver:

- gws `--page-all` overlap on page boundaries (gws bug; not yet ruled in or out).
- Calendar API `events.list?singleEvents=false` returning a recurring exception twice in some edge case (the `_R<timestamp>` suffix in every problematic ID hints at a recurring-exception edge case; SPEC §"Recurring Events" addresses materialized exceptions, not the singleEvents=false wire shape pinned by Calendar API).

The 14 problematic source IDs all use the `_R<timestamp>` suffix shape (e.g. `22si2itigr2gho9abp8dk4kh8v_R20260323T163000`). The standard exception-event ID format is `<parent>_<YYYYMMDDTHHMMSSZ>`; the `_R<timestamp>` form is what Calendar emits for certain "anchored" override shapes whose semantics aren't well-documented externally. The `_<timestamp>Z` form (e.g. `22si2itigr2gho9abp8dk4kh8v_20260504T163000Z` - showing up in the dry-run output as `instance_unmaterializable`) does flow through the recurring handler, which suggests its `recurringEventId` field is set; the `_R<timestamp>` form's INSERT outcome (which can only come from `internal/sync/reconcile.go:reconcileNormal`, not from `internal/sync/classify.go:classifyRecurringInstance`) implies its `recurringEventId` is empty. That is: the source list returns these exception-shaped events as if they were standalone non-recurring events.

### Trace (what happens on the second pass)

Given a duplicate source-tuple in the list and an empty inventory at startup:

1. **First Classify call** for the source event: routes through `internal/sync/classify.go:Classify` step 8 → `reconcileNormal` (`internal/sync/reconcile.go:11-55`) → inventory miss → `doInsert` (`internal/sync/insert.go:19-47`).
2. `doInsert` calls `c.API.EventsInsert` (the dry-run wrapper). The wrapper echoes body + ID. Body has `calendar-sync:version=2` and `calendar-sync:source` set by `mirror.BuildPayload` (`internal/mirror/payload.go:46-72`).
3. `completeInsert` → `followUpChecksum`. Per Cause A above, the returned `final` event has only `{checksum}` in its extended properties.
4. `c.Inventory.Set(tuple, final)`. The in-memory mirror for this source tuple now has `calendar-sync:version = ""` (missing). Outcome emitted: `insert(source_updated)`.
5. **Second Classify call** for the same source event: `reconcileNormal` → inventory **hit** (we just set it) → `mirror.ComputeDriftSignal` (`internal/mirror/drift.go:89-102`):
   - `storedVersion = ""` → `NeedsMigration = true`.
   - `storedSourceUpdated = ""` → `compareTimestamps(source.Updated, "") = 1` → `SourceChanged = true`.
   - `storedChecksum = checksum_just_written`. Source-passed ManagedFields differ from the dry-run-cached mirror's ManagedFields (the mirror only has `{checksum}` in its extended properties; everything else is zero). `MirrorDrifted = true`.
6. `reconcileMigration` (`internal/sync/migration.go:33-75`) recomputes `MirrorDrifted` via direct managed-field comparison (still `true`), then matches the `signal.SourceChanged && signal.MirrorDrifted` cell and routes to `doMigrationSourceWon` (`internal/sync/migration.go:114-136`).
7. Outcome: `patch(source_updated, conflict=migration_source_won)`.

End-to-end test reproducing the chain: `cmd/run_test.go:TestRunCmd_DryRun_DuplicateSourceEventTriggersBogusMigrationSourceWon`. Two copies of the same source event in the list, empty inventory, dry-run mode → both INSERT and migration_source_won outcomes are observed.

### Verdict

Two cooperating bugs. The user-visible symptom (`migration_source_won` on a freshly-minted v2 mirror) only manifests in dry-run mode AND when the source list double-delivers a tuple.

### Proposed fix sketch

The cleanest fix targets Cause A:

1. Make `dryRunAPI` track a per-(calendarID, eventID) map of "what we last echoed". `EventsPatch` then merges body into the cached resource (matching production's JSON Merge Patch semantics) before returning.

2. `EventsInsert` cache-keys the body by `(calendarID, body.ID)` so the patch followups see a coherent base.

3. Optionally: `EventsList` and `EventsGet` consult the cache so a re-list mid-run returns the current cached state. This mirrors how production deltas propagate.

Cause B is independent and worth fixing separately - the runClassifyLoop should de-dupe by source-tuple (which is unambiguous; SPEC §"In-memory state" already keys the inventory by tuple). A second-occurrence outcome can never be useful: the first call already drove the desired state; the second is at best a no-op and at worst (as here) a bogus migration. Adding a `seen[mirror.SourceTuple]bool` check at the top of the per-event loop is a one-line change with a unit test that fails today.

Recommended order: ship the de-dupe fix first (it's a real bug regardless of dry-run), then revisit the dry-run wrapper fidelity if the de-dupe doesn't fully cover.

### Out of scope but worth flagging

The Calendar API's actual behavior for `_R<timestamp>`-shaped IDs needs separate investigation. The current source-list handling assumes either "non-recurring event" or "exception with `recurringEventId` populated". Calendar API may return exceptions in a third shape (no `recurringEventId`, exception-shaped ID) that the classifier currently treats as "standalone non-recurring event" - the source-of-truth for that distinction is whichever Google API doc covers `events.list?singleEvents=false`'s return shape under "phantom" / past-UNTIL exceptions. Worth confirming.

## Anomaly #2: `partial_failure` exit drops underlying error context

### Observed

```
{"error":"partial_failure","detail":"1 pdir(s) failed: work-personal:a_to_b"}
```

Operator can see WHICH pdir failed but not WHY. The actual gws/Calendar error that triggered the failure never reaches stderr.

### Root cause

The error chain is preserved correctly in code right up until the JSON envelope is assembled.

`cmd/run.go:103-132` (v1.0.0) collects per-pdir errors:

```go
var failures []string
var firstErr error
for _, pr := range res.PDirs {
    if pr.Err == nil { continue }
    failures = append(failures, pr.Pair+":"+pr.Direction)
    if firstErr == nil { firstErr = pr.Err }
}
...
if len(failures) > 0 {
    return newCmdError(output.CodePartialFailure,
        fmt.Sprintf("%d pdir(s) failed: %s", len(failures), strings.Join(failures, ", ")),
        "", firstErr)
}
```

`firstErr` is wrapped as the cmdError's `cause` field. `cmdError.Unwrap()` returns `e.cause`, so the wrapped error is reachable via `errors.Unwrap`.

The drop happens at `cmd/cli.go:128-142` (v1.0.0):

```go
func handleErr(stderr io.Writer, err error) int {
    var parseErr *kong.ParseError
    if errors.As(err, &parseErr) { ... return 64 }
    code, detail, hint := MapError(err)
    output.EmitError(stderr, output.ErrorEnvelope{
        Error:  code,
        Detail: detail,
        Hint:   hint,
    })
    return output.ExitCodeFor(code)
}
```

`output.ErrorEnvelope` (`internal/output/error.go:15-20`) HAS a `Cause` field with `omitempty`. handleErr just doesn't fill it. `MapError` is structured to return `(code, detail, hint)` - no fourth slot - so the cause was orphaned.

### Verdict

Code bug. SPEC line 384 documents the `cause` field; the implementation never populated it.

### Fix

Landed in commit `5a412e6` on this branch. `handleErr` now calls `errors.Unwrap(err)` and attaches the result as `ErrorEnvelope.Cause`. cmdError's Unwrap returns the cause field directly, so the partial_failure path now includes the underlying gws/sync error in stderr.

Test pin: `cmd/errors_test.go:TestUnwrapCause` (unit), `cmd/run_test.go:TestRunCmd_PartialFailureSurfacesUnderlyingErrorViaHandleErr` (end-to-end).

The unwrap is a single-step (`errors.Unwrap` once, not loop). `errors.Unwrap` returns the immediate parent; for a `cmdError` wrapping a classify error wrapping a gws error, `Unwrap()` returns the classify error. That's what we want - the classify error's `Error()` reads `"classify <calendar>/<eventID>: <gws-error>"` which is exactly the bottom-line context.

For non-cmdError fall-throughs, `errors.Unwrap` may return a `fmt.Errorf` chain element. If that's noisy, the omitempty rule on the field still keeps the JSON envelope tidy. No additional handling is necessary.

## Where the new instrumentation lights up

For the next debug run (with `--log-level=debug`):

- gws subprocess invocations: `internal/gws/{client.go,events.go,events_write.go,calendarlist.go}` log on entry.
- `runClassifyLoop`: `internal/sync/reconciler.go:runClassifyLoop` debug log per source event with id, recurring_event_id, status, transparency.
- `reconcileNormal`: `internal/sync/reconcile.go:reconcileNormal` logs inventory hit/miss + drift signal + chosen action.
- Recurring handler: `internal/recurring/handler.go:Handle` / `resolveMirrorParent` / `locateMirrorInstance` / `applyDriftMatrix` log resolve + locate + matrix-branch decisions.
- BuildInventory: `internal/sync/inventory.go:BuildInventory` logs target + per-version count at info level.
- Per-pdir failure: `cmd/run.go` warn-level log carries source/target + underlying error, complementing the new envelope `cause` field.

Together these will pin down on the live calendar whether the duplicate source-tuple comes from gws pagination, Calendar API double-delivery, or something else entirely. The proposed de-dupe fix in §"Anomaly #1" should be implemented after we've seen one good debug run.
