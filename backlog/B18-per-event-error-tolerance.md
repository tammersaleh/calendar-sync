# B18 — per-event error tolerance in the classify loop

Pending. Surfaced live during the v2.1.4 daemon's first hour: a single transient HTTP 500 on one recurring event triggered a 24-minute fast-track full re-sync, then another, then another. The daemon was correct (zero writes, zero data risk) but ran nothing but back-to-back full syncs because of one flaky event.

## Problem

`internal/sync/reconciler.go:runClassifyLoop` accumulates every per-event `Classify` error into `errs`, then returns `errors.Join(errs...)`. A non-nil return sets `PDirResult.Err`, which gates conditional token advancement (SPEC §"Conditional advancement"). The pdir's source syncToken doesn't advance. The next tick sees an empty token, sets `NeedsFullResync`, and the daemon's scheduler runs a fast-track FullSync. If the same flake recurs during the new FullSync, the cycle repeats indefinitely.

Observed live with this error during the daemon's tick:

```
{"level":"WARN","msg":"sync.runClassifyLoop: classify error",
 "pair":"work-personal",
 "source_event":"0lcf7loe2ua309p4j0rh1srhmo_20270326T210000Z",
 "recurring_event_id":"0lcf7loe2ua309p4j0rh1srhmo_R20260501T210000",
 "summary":"TARS Office Hours (Central/PT)",
 "error":"backend_error during events.get (HTTP 500): connect timeout"}
```

A single transient `events.get` failure (the recurring parent's eligibility check timed out at the network layer) flagged the pdir failed. The daemon ran 4 full syncs in ~50 minutes. No writes, no propagates - just CPU and quota.

The user instinct is right that fix-the-event-by-deletion (option 1) doesn't generalize: any future event that triggers the same Google API flakiness would re-introduce the loop. We need a daemon-policy fix (option 2) instead.

## Discrimination

The fix is to discriminate per-event errors into "transient read" (skip + advance token) vs "fatal" (fail the pdir, gate the token).

### Errors that SHOULD continue failing the pdir

- `gws.ErrAPIGone` (410) at the source-list level. Already handled separately in `incrementalListSources`; this branch invalidates the source token and the per-pdir source-list errs map. Untouched by B18.
- Source-list (`events.list`) errors for any reason. The pdir literally has no events to process this tick - the syncToken should NOT advance.
- **Write failures** during classify: `events.patch`, `events.insert`, `events.delete` on either source or mirror. A partially-succeeded write leaves mirror state uncertain; retrying the whole pdir cleanly on a stable token is the right move.

### Errors that SHOULD be logged + skipped (token advances)

- `gws.CodeBackendError` (HTTP 5xx) on `events.get` / `events.instances` during read-side calls. Read-side flake, no state mutation. Next tick or FullSync re-evaluates the affected event.
- gws subprocess timeouts on read-side calls (`gws subprocess: context canceled`, `context deadline exceeded`).
- `gws.CodeAPIInvalidRequest` from the recurring-instance handler when the underlying `events.instances` call goes wrong. Observed live on TARS Office Hours and Mistral AI + CoreWeave Weekly Sync. Probably a Google indexing quirk on recurring exception parents whose recurringEventId itself has the `_R<UTC>` suffix. Read-side; safe to skip.
- Per-event `events.get` 404 from horizon checks for recurring parents (the parent disappeared between source-list and the eligibility lookup; orphan walk and the next FullSync handle the cleanup).

## Where to discriminate

Two options. Codex review should weigh in.

### Option A: Discriminate in runClassifyLoop

Add a helper at `internal/sync/reconciler.go`:

```go
func isTransientReadError(err error) bool {
    return errors.Is(err, gws.ErrAPIBackend) ||
        errors.Is(err, gws.ErrSubprocessCanceled) ||
        errors.Is(err, gws.ErrAPIInvalidRequest) ||
        errors.Is(err, gws.ErrAPINotFound)
}
```

(`gws.ErrSubprocessCanceled` may need to be added; today `context canceled` errors are returned raw from `internal/gws/client.go`.)

Then in `runClassifyLoop`:

```go
if err := c.Classify(ctx, &ev); err != nil {
    r.warn("sync.runClassifyLoop: classify error", ...)
    if isTransientReadError(err) {
        continue
    }
    errs = append(errs, fmt.Errorf("classify %s/%s: %w", ...))
}
```

Pros: small change, lives at the existing decision point.

Cons: the Classifier and recurring handler wrap their internal errors heavily; `errors.Is` may or may not penetrate every wrap. Have to audit each error path.

### Option B: Discriminate inside the producers (Classifier / recurring handler)

Have `Classify` and `recurring.Handler.Handle` emit `Outcome{Action: skip, Reason: "transient_read_error"}` (or similar) for the read-side flakes locally, and only return errors for genuinely fatal cases (write failures, programmer bugs).

Pros: clean separation - by the time runClassifyLoop sees an error, it's already known-fatal. SPEC's "Partial failure semantics" stays simple.

Cons: bigger surgery on the producers. Both `Classify` and `recurring.Handler` need to know which ops are reads vs writes and which errors are transient.

### Lean

Option B is cleaner long-term. Option A is the ship-it-this-week version. Codex's review should pick one based on the test/spec impact tradeoff.

## SPEC sections to update

- §"Partial failure semantics" (line 1287). Currently: any per-event classify error fails the pdir. Update: enumerate the transient-read class and clarify that those don't gate token advancement.
- §"Conditional advancement" (line 910 / line 934). Same paragraph. Reference the transient class.
- §"Daemon lifecycle: per-tick reconciliation" line 932. The 410-handling path stays as-is; this is per-event errors only.
- §Errors table (the actions/reasons/codes tables). If Option B, add a `transient_read_error` skip reason.

## Test plan

In `internal/sync/reconciler_test.go`:

1. `TestTick_TransientReadError_AdvancesToken`: stage a source list with three events. Classify on the second one returns `gws.ErrAPIBackend` (HTTP 500 on a horizon-check `events.get`). Expect: pdir succeeds, syncToken advances, the other two events processed normally, no fast-track FullSync requested.
2. `TestTick_WriteError_DoesNotAdvanceToken`: stage three events; a write (`events.patch` for a propagate) returns 5xx. Expect: pdir fails, token stays, NeedsFullResync true.
3. `TestTick_MultipleTransientReadErrors`: every recurring-handler `events.get` flakes. Expect: token advances, no fast-track FullSync.
4. `TestTick_MixedTransientAndFatal`: one transient read, one write failure. Expect: token does NOT advance (any genuine failure trumps the leniency).
5. `TestTick_TransientReadErrorLogged`: assert the warn log fires per skipped event so an operator can spot the underlying flakiness.

In `internal/recurring/handler_test.go` (if Option B):

6. `TestHandle_EventsGetTransient5xx_ReturnsSkipNotError`: pin that the recurring handler swallows the API 5xx during eligibility-check `events.get` and returns `Result{Action: skip, Reason: transient_read_error}` instead of an error.
7. Same for `events.instances` flakiness.

## Risks

- **False-negative classification**: a genuinely persistent issue (e.g. one specific event always 5xx's) gets reclassified as "transient" forever. The pdir advances its token without ever fully processing that event. Mitigation: the next FullSync re-encounters the same event and either hits a clean response, or hits the same 5xx and skips again. The mirror for that event sits stale until the underlying issue clears or the user manually intervenes via `mirror prune`. This is strictly better than the current behavior (entire pdir in 100% FullSync mode).
- **Hiding real bugs**: a programmer error that surfaces as `gws.ErrAPIBackend` (it shouldn't, but defensively) would silently skip rather than failing loudly. Mitigation: keep the warn log; if the user sees the same warn line every tick, that's the signal.
- **Write detection**: Option A relies on the producer never returning a write error wrapped as a backend error. Currently `internal/sync/drift.go` wraps with `"propagate to source %s/%s: %w"`. If the underlying `events.patch` returns `gws.ErrAPIBackend`, `errors.Is` would still match it. We need to separate "this 5xx came from a read" vs "this 5xx came from a write" - probably by introducing distinct sentinels or by checking the operation context in the wrapper.

## Concrete file change list (sketch, Option A)

- `internal/gws/errors.go`: add `ErrSubprocessCanceled` sentinel for context-canceled subprocess errors. Currently those return raw `context.Canceled`-wrapped errors.
- `internal/sync/reconciler.go`: add `isTransientReadError`, modify `runClassifyLoop`.
- `internal/sync/reconcile.go` / `internal/sync/insert.go` / `internal/recurring/handler.go`: review every error wrap point. Confirm that `events.patch` / `events.insert` / `events.delete` failures wrap with a distinct shape that `isTransientReadError` won't false-positive on. (Probably need to introduce `ErrWriteFailed` and have the writer methods wrap with that.)
- `SPEC.md`: §Partial failure semantics, §Conditional advancement, §Errors table.
- `doc/bugs.md`: B18 entry under Fixed once landed.

(Option B is bigger - touches Classifier and recurring.Handler.)

## When this file becomes useless

When B18 ships, tests pass, Codex review is clean, and the daemon has run a full week with at least one of TARS Office Hours / Mistral AI / similar known-flaky events still in the source set without re-entering back-to-back FullSync mode, delete this file. The new per-event error policy lives in SPEC.md and `doc/bugs.md` from then on.
