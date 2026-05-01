package sync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// Reason values the orphan walk can emit. SPEC.md "Daemon lifecycle:
// periodic full re-sync" step 5 enumerates four delete cells; three need
// fresh reasons (orphaned, outside_horizon, source_filtered) and the
// fourth (already-classified, alive-and-eligible) is intentionally silent.
//
// ReasonOutsideHorizon already exists in classify.go for the per-event
// pre-step-7 horizon prune; the orphan walk reuses the same string so
// callers see one vocabulary regardless of which path produced the delete.
const (
	ReasonOrphaned       mirror.Reason = "orphaned"
	ReasonSourceFiltered mirror.Reason = "source_filtered"
)

// defaultOrphanConcurrency mirrors SPEC.md §"Concurrency" line 1107:
// "Orphan-detection lookups [...] fan out with a semaphore of 5". An
// OrphanWalker with ConcurrencyLimit<=0 falls back to this value.
const defaultOrphanConcurrency = 5

// OrphanWalker traverses an Inventory and deletes mirrors whose source is
// no longer eligible per SPEC.md "Daemon lifecycle: periodic full re-sync"
// step 5. Construct one per (source-calendar, target-calendar) pair per
// FullSync pass: the caller iterates over the distinct (source, target)
// tuples among its pdirs and runs one OrphanWalker for each.
//
// Walk respects the visited set the caller computed during the just-
// completed full classify pass for this pair. Every inventory entry whose
// source-tuple was visited is skipped (its source was on the wire, so
// classify already had its turn); entries NOT visited are checked via
// events.get to determine why they're missing.
//
// A walk for one (source, target) pair never touches inventory entries
// whose source-tuple references a different source calendar - the walker
// silently skips them. The caller still has to run a separate walker
// against the other source.
type OrphanWalker struct {
	API              API
	Now              func() time.Time
	Horizon          time.Duration
	Pair             string
	Direction        string
	SourceCalendarID string
	TargetCalendarID string
	Inventory        *Inventory
	Output           Output

	// ConcurrencyLimit caps simultaneous events.get fan-out per SPEC
	// §"Concurrency". A non-positive value falls back to
	// defaultOrphanConcurrency (5).
	ConcurrencyLimit int
}

// orphanResult is the per-tuple events.get result piped from the
// concurrent fan-out back to the serial mutation loop.
type orphanResult struct {
	tuple  mirror.SourceTuple
	mirror *gws.Event
	source *gws.Event
	err    error
}

// Walk implements SPEC.md "Daemon lifecycle: periodic full re-sync" step 5.
//
// For each inventory entry whose source-tuple references w.SourceCalendarID
// AND was not in visited, fetch the source via events.get and decide
// (branch order matches the implementation in classifyOrphan):
//
//   - 404 or status=cancelled              -> delete(orphaned)
//   - recurring parent, no instance in win -> delete(outside_horizon)
//   - non-recurring, start > now+horizon   -> delete(outside_horizon)
//   - alive in horizon but filtered (eventType not in
//     {default, outOfOffice, focusTime}, transparency=transparent,
//     declined, tentative)                 -> delete(source_filtered)
//   - alive in horizon, NOT filtered       -> no delete (would have been
//     visited by the classify pass; this is a programmer-bug shape, not
//     emitted as an outcome - see "alive-and-eligible" comment below).
//
// Concurrency: events.get fan-out is bounded by w.ConcurrencyLimit
// (default 5). Mutations to Inventory and Output happen serially after
// the concurrent fan-out drains, so callers don't need an external lock.
//
// Error semantics: a non-404 events.get failure on one entry does not
// abort the walk; other entries still run. The collected per-entry errors
// are joined and returned at the end via errors.Join, mirroring the SPEC's
// "partial_failure" pattern (run does not abort on the first error). A
// 404 from events.delete on a mirror is swallowed: another process (or a
// previous orphan walk that crashed) already cleaned it up.
func (w *OrphanWalker) Walk(ctx context.Context, visited map[mirror.SourceTuple]bool) error {
	concurrency := w.ConcurrencyLimit
	if concurrency <= 0 {
		concurrency = defaultOrphanConcurrency
	}

	// 1. Filter inventory to the entries this walker is responsible for:
	//    same source calendar, not visited.
	toCheck := w.candidateTuples(visited)
	if len(toCheck) == 0 {
		return nil
	}

	// 2. Fan out events.get with a buffered-channel semaphore. The
	//    concurrent path is read-only against the inventory; only the
	//    serial drain below mutates it.
	results := w.fanOutGet(ctx, toCheck, concurrency)

	// 3. Drain results, classify, and apply mutations serially. Errors
	//    on individual entries are accumulated; the walk continues so
	//    other orphans get cleaned up.
	var errs []error
	for r := range results {
		if r.err != nil && !errors.Is(r.err, gws.ErrAPINotFound) {
			errs = append(errs, fmt.Errorf("orphan walk events.get %s/%s: %w",
				w.SourceCalendarID, r.tuple.EventID, r.err))
			continue
		}
		if err := w.classifyAndDelete(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// candidateTuples returns the inventory entries this walker should
// inspect: same source calendar AND not in visited.
func (w *OrphanWalker) candidateTuples(visited map[mirror.SourceTuple]bool) []mirror.SourceTuple {
	tuples := w.Inventory.Tuples() // alphabetical for deterministic-ish ordering
	out := make([]mirror.SourceTuple, 0, len(tuples))
	for _, t := range tuples {
		if t.CalendarID != w.SourceCalendarID {
			continue
		}
		if visited[t] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// fanOutGet runs concurrent events.get calls bounded by a semaphore of
// size concurrency. The returned channel is closed after all goroutines
// finish; callers drain it sequentially. Per SPEC §"Concurrency", only
// the get fan-out is concurrent; downstream mutations stay serial.
func (w *OrphanWalker) fanOutGet(ctx context.Context, tuples []mirror.SourceTuple, concurrency int) <-chan orphanResult {
	results := make(chan orphanResult, len(tuples))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, t := range tuples {
		t := t
		mirrorEvent, ok := w.Inventory.Lookup(t)
		if !ok {
			// Inventory shrunk between candidateTuples and now (impossible
			// in the current single-threaded caller pattern, but cheap to
			// be defensive about). Skip; the next full sync will revisit.
			continue
		}
		wg.Add(1)
		go func(tuple mirror.SourceTuple, mirrorEv *gws.Event) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			source, err := w.API.EventsGet(ctx, w.SourceCalendarID, tuple.EventID)
			results <- orphanResult{tuple: tuple, mirror: mirrorEv, source: source, err: err}
		}(t, mirrorEvent)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	return results
}

// classifyAndDelete runs the SPEC step-5 four-cell decision on one
// orphanResult and applies the resulting delete (if any). Mutations to
// Inventory and Output happen here, serially.
//
// A nil r.err with a 404-equivalent r.source value can't actually happen
// because the gws wrapper returns a typed *Error on 404; we always reach
// this function with either err=ErrAPINotFound (already handled) or a
// non-nil source.
func (w *OrphanWalker) classifyAndDelete(ctx context.Context, r orphanResult) error {
	reason, shouldDelete := w.classifyOrphan(ctx, r)
	if !shouldDelete {
		return nil
	}
	return w.deleteMirror(ctx, r, reason)
}

// classifyOrphan runs the SPEC step-5 cell decision and returns the
// reason + whether to delete. Branch order matches the code below:
//
//  1. 404 OR status=cancelled                   -> orphaned
//  2. recurring parent, no instance in window   -> outside_horizon
//     (recurring parents fall through to the filter check when in window)
//  3. non-recurring start > now+horizon         -> outside_horizon
//  4. alive in horizon, filtered (eventType /
//     transparency / declined / tentative)      -> source_filtered
//
// SPEC step 5 enumerates these four cells; we evaluate recurring before
// non-recurring because the source.Recurrence presence is what splits
// them. A fifth case (alive and eligible) is the "shouldn't happen"
// branch: any inventory entry not visited by classify and not matching
// the four cells has slipped through both paths. We skip without
// deleting and without emitting an outcome - a future warn-sink hookup
// will surface this as a diagnostic. Doing nothing is the safer choice;
// the next full sync will revisit.
func (w *OrphanWalker) classifyOrphan(ctx context.Context, r orphanResult) (mirror.Reason, bool) {
	// Cell 1: source is gone (404) or cancelled.
	if errors.Is(r.err, gws.ErrAPINotFound) || (r.source != nil && r.source.Status == gws.EventStatusCancelled) {
		return ReasonOrphaned, true
	}

	// At this point r.source is non-nil and not cancelled.
	src := r.source

	// Cell 3: recurring parent. Use events.instances over the horizon
	// window; empty -> outside_horizon. The events.instances 404 case
	// is unlikely (we just got a non-404 from events.get on the parent)
	// and is treated as "no instance in window" if it ever happens.
	if len(src.Recurrence) > 0 {
		inWindow, err := w.recurringHasInstance(ctx, src.ID)
		if err != nil {
			// A failed instances call leaves us unable to decide; conservative
			// behavior is to NOT delete and let the next full sync retry.
			return "", false
		}
		if !inWindow {
			return ReasonOutsideHorizon, true
		}
		// Recurring source IS in horizon. Fall through to filter checks
		// below: a recurring event can still be transparent/declined/etc.
		// SPEC step 5 doesn't enumerate this combo explicitly, but the
		// safer reading of the four cells is "the order is 404 -> horizon
		// -> filtered", so a recurring-parent that's in horizon AND
		// filtered should be source_filtered, not silently retained.
	} else {
		// Cell 2: non-recurring start > now+horizon.
		if w.outsideHorizon(src) {
			return ReasonOutsideHorizon, true
		}
	}

	// Cell 4: alive in horizon. Did the source-list query filter it out?
	if w.sourceFilteredOut(src) {
		return ReasonSourceFiltered, true
	}

	// Cell 5 (the shouldn't-happen): alive, in horizon, NOT filtered. The
	// classify pass should have visited this; we don't know why it didn't.
	// Emit nothing, document the gap. See classifyOrphan doc.
	return "", false
}

// recurringHasInstance issues SPEC step-5's events.instances probe for a
// recurring parent: maxResults=1 over the [now, now+horizon] window. A
// non-empty response means at least one occurrence falls in horizon.
//
// Horizon=0 (the test/default convenience) skips the API call and reports
// in-window=true so the recurring branch falls through to the filter
// checks like a non-recurring in-horizon event would.
func (w *OrphanWalker) recurringHasInstance(ctx context.Context, sourceID string) (bool, error) {
	if w.Horizon == 0 {
		return true, nil
	}
	now := w.now()
	params := gws.EventsInstancesParams{
		CalendarID:  w.SourceCalendarID,
		EventID:     sourceID,
		TimeMin:     now.Format(time.RFC3339),
		TimeMax:     now.Add(w.Horizon).Format(time.RFC3339),
		MaxResults:  1,
		ShowDeleted: false,
	}
	instances, err := w.API.EventsInstances(ctx, params)
	if err != nil {
		return false, err
	}
	return len(instances) > 0, nil
}

// outsideHorizon returns whether a non-recurring source's start is
// strictly after now+horizon. Mirrors classify.go's parseEventStart so
// orphan and classify paths agree on "in horizon".
func (w *OrphanWalker) outsideHorizon(source *gws.Event) bool {
	if w.Horizon == 0 {
		return false
	}
	start, ok := parseEventStart(source.Start)
	if !ok {
		// Source has no parseable start. Mirrors classify's choice of
		// failing open: don't delete on a malformed start; let a
		// subsequent reconcile-or-prune surface the issue.
		return false
	}
	return start.After(w.now().Add(w.Horizon))
}

// sourceFilteredOut returns whether a live source event would be filtered
// by SPEC.md §"Filtering" - i.e. why the source-list query at step 2 of
// the full re-sync didn't return it. The four signals are:
//
//   - eventType NOT in {default, outOfOffice, focusTime} (the source-list
//     query passes those three explicitly via the eventTypes parameter,
//     so any other value - documented birthday/fromGmail/workingLocation
//     OR any future Google type - is treated as filtered).
//   - transparency=transparent.
//   - source-owner attendee responseStatus=declined.
//   - source-owner attendee responseStatus=tentative.
//
// The eventType check is an allowlist deliberately: matching the SPEC's
// positive set means new Google event types default to "filter, prune
// the mirror" rather than "retain the mirror", which is the safer
// behavior given the orphan walk only runs when the source was already
// missing from the source-list response.
//
// We don't check the loop-prevention is_mirror filter here: a mirror of
// a mirror is impossible (the inventory query already filtered to events
// carrying calendar-sync:source), so it can't reach this branch.
func (w *OrphanWalker) sourceFilteredOut(source *gws.Event) bool {
	switch source.EventType {
	case gws.EventTypeDefault, gws.EventTypeOutOfOffice, gws.EventTypeFocusTime:
		// Allowed type; fall through to the non-eventType filters.
	default:
		return true
	}
	if source.Transparency == gws.TransparencyTransparent {
		return true
	}
	switch mirror.SourceOwnerResponseStatus(source) {
	case gws.ResponseStatusDeclined, gws.ResponseStatusTentative:
		return true
	}
	return false
}

// deleteMirror issues events.delete on the target, removes the entry
// from the in-memory inventory, and emits an outcome. A 404 on
// events.delete is treated as success: the mirror was already gone
// (concurrent prune, manual deletion, etc.), so the inventory and
// downstream consumers should still see the cleanup.
func (w *OrphanWalker) deleteMirror(ctx context.Context, r orphanResult, reason mirror.Reason) error {
	err := w.API.EventsDelete(ctx, w.TargetCalendarID, r.mirror.ID)
	if err != nil && !errors.Is(err, gws.ErrAPINotFound) {
		return fmt.Errorf("orphan walk events.delete %s/%s: %w",
			w.TargetCalendarID, r.mirror.ID, err)
	}

	w.Inventory.Delete(r.tuple)
	w.emit(Outcome{
		Action:        mirror.ActionDelete,
		Reason:        reason,
		SourceEventID: r.tuple.EventID,
		TargetEventID: r.mirror.ID,
		Summary:       r.mirror.Summary,
	})
	return nil
}

// emit centralizes Pair/Direction enrichment so call sites stay terse,
// matching Classifier.emit in classify.go.
func (w *OrphanWalker) emit(o Outcome) {
	if w.Output == nil {
		return
	}
	o.Pair = w.Pair
	o.Direction = w.Direction
	w.Output(o)
}

// now returns w.Now() if set, otherwise time.Now. Mirrors Classifier.now.
func (w *OrphanWalker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
