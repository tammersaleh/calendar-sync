package sync

import (
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/recurring"
)

// catalogReadiness records whether a source calendar's exception index was
// built from a collection we can prove complete.
//
// Readiness is per CALENDAR while membership is per occurrence, and that
// asymmetry is deliberate. A failed, truncated or 410'd list means an
// exception ANYWHERE in the calendar might be missing from the index, so no
// individual ID may be reported Absent on the strength of the stale set.
// The zero value is Unknown, so a source with no catalog at all fails safe.
type catalogReadiness int

const (
	readinessUnknown catalogReadiness = iota
	readinessReady
)

// sourceCatalog indexes the recurring-instance exceptions that one source
// calendar's unexpanded events.list(singleEvents=false) contained. It is the
// B29 discriminator: events.get answers 200 with a synthesized virtual
// occurrence for any slot the parent's RRULE produces, but a complete
// unexpanded list contains only real exceptions.
type sourceCatalog struct {
	readiness catalogReadiness

	// coverageMin and coverageMax bound the window the snapshot proved -
	// the timeMin / timeMax the full source-list was issued with. A zero
	// value means that side was unbounded. Absence is authoritative only
	// inside this window: the source list is horizon-bounded while the
	// target delta is not.
	coverageMin time.Time
	coverageMax time.Time

	// byID maps an exception's instance ID to its parent's ID. Cancelled
	// exceptions are indexed too. A cancelled exception is real source
	// intent - dropping it would let the reverse path treat a deliberately
	// removed occurrence as virtual and resurrect it.
	byID map[string]string

	// byParent is the reverse index. A parent tombstone in a source delta
	// has to evict every child indexed under it; without this index that
	// cascade would need a full scan of byID on every delta.
	byParent map[string]map[string]struct{}
}

// newSourceCatalog returns an empty, not-yet-ready catalog for the given
// coverage window.
func newSourceCatalog(coverageMin, coverageMax time.Time) *sourceCatalog {
	return &sourceCatalog{
		coverageMin: coverageMin,
		coverageMax: coverageMax,
		byID:        map[string]string{},
		byParent:    map[string]map[string]struct{}{},
	}
}

// addException indexes one recurring-instance exception. Callers must have
// checked ev.RecurringEventID is non-empty.
func (c *sourceCatalog) addException(ev *gws.Event) {
	c.byID[ev.ID] = ev.RecurringEventID
	children := c.byParent[ev.RecurringEventID]
	if children == nil {
		children = map[string]struct{}{}
		c.byParent[ev.RecurringEventID] = children
	}
	children[ev.ID] = struct{}{}
}

// removeSeries drops a parent and every exception indexed under it. Driven
// by a parent tombstone (status=cancelled with no recurringEventId) in a
// source delta: Google reports the series deletion once, not once per
// exception, so the children would otherwise linger as phantom Present
// answers forever.
//
// The parent's own ID is deliberately NOT evicted from byID. Only exceptions
// live there, and the one way a tombstone's ID could appear is a delta entry
// for a cancelled EXCEPTION that arrived without its recurringEventId -
// exactly the entry that must stay indexed, since a cancelled exception is
// real source intent.
func (c *sourceCatalog) removeSeries(parentID string) {
	for childID := range c.byParent[parentID] {
		delete(c.byID, childID)
	}
	delete(c.byParent, parentID)
}

// covers reports whether the snapshot's window would have listed an
// exception at this occurrence. The predicate mirrors the Calendar API's own
// timeMin / timeMax semantics (an event is listed when its end is after
// timeMin and its start is before timeMax) so a borderline occurrence is not
// mistakenly reported Absent.
//
// An occurrence with no parseable time is never covered: we cannot prove the
// snapshot would have caught an exception there, so absence proves nothing.
func (c *sourceCatalog) covers(inst *gws.Event) bool {
	start, ok := parseEventStart(inst.Start)
	if !ok {
		start, ok = parseEventStart(inst.OriginalStartTime)
	}
	if !ok {
		return false
	}
	end, endOK := parseEventStart(inst.End)
	if !endOK {
		end = start
	}
	if !c.coverageMin.IsZero() && !end.After(c.coverageMin) {
		return false
	}
	if !c.coverageMax.IsZero() && !start.Before(c.coverageMax) {
		return false
	}
	return true
}

// lookup answers the four-state membership question for one source
// instance. Order matters: readiness gates everything, then coverage, then
// the exact-ID index.
func (c *sourceCatalog) lookup(inst *gws.Event) recurring.Membership {
	if c == nil || c.readiness != readinessReady {
		return recurring.MembershipUnknown
	}
	if !c.covers(inst) {
		return recurring.MembershipOutOfScope
	}
	if _, ok := c.byID[inst.ID]; ok {
		return recurring.MembershipPresent
	}
	return recurring.MembershipAbsent
}

// rebuildSourceCatalog installs a fresh catalog for a source from a full
// unexpanded source-list that completed with proven pagination. The swap is
// atomic: the new catalog is built off to the side and only assigned once
// every event has been indexed, so no reader can observe a half-filled set.
//
// coverageMin / coverageMax are the timeMin / timeMax the list was issued
// with; a zero coverageMax means the list was unbounded.
func (r *Reconciler) rebuildSourceCatalog(source string, events []gws.Event, coverageMin, coverageMax time.Time) {
	fresh := newSourceCatalog(coverageMin, coverageMax)
	for i := range events {
		ev := &events[i]
		if ev.RecurringEventID == "" {
			continue
		}
		// Cancelled exceptions are indexed deliberately - see byID.
		fresh.addException(ev)
	}
	fresh.readiness = readinessReady
	r.sourceCatalogs[source] = fresh
	r.debug("sync.sourceCatalog: rebuilt",
		"source", source,
		"exceptions", len(fresh.byID),
		"coverage_min", coverageMin.Format(time.RFC3339),
		"coverage_max", coverageMax.Format(time.RFC3339),
	)
}

// applySourceDeltaToCatalog folds one successful source delta into the
// existing catalog. Callers must not call it for a failed, truncated or
// 410'd delta: an incomplete delta cannot keep the index authoritative.
//
// The delta itself does not widen coverage. Absence from a delta proves
// nothing about a time range the original snapshot never reached.
func (r *Reconciler) applySourceDeltaToCatalog(source string, events []gws.Event) {
	cat, ok := r.sourceCatalogs[source]
	if !ok || cat.readiness != readinessReady {
		// Nothing complete to fold into. The next FullSync builds a fresh
		// catalog; until then the calendar stays Unknown.
		return
	}
	for i := range events {
		ev := &events[i]
		switch {
		case ev.RecurringEventID != "":
			// An exception, whatever its status. A cancelled exception is
			// still an exception the source deliberately holds.
			cat.addException(ev)
		case ev.Status == gws.EventStatusCancelled:
			// Parent tombstone: the whole series is gone, so every
			// exception under it is gone with it.
			cat.removeSeries(ev.ID)
		}
	}
	r.debug("sync.sourceCatalog: delta applied",
		"source", source,
		"delta_events", len(events),
		"exceptions", len(cat.byID),
	)
}

// markSourceCatalogUnknown flags a source calendar's catalog as unusable.
// The indexed data is left in place for diagnostics; every lookup gates on
// readiness first, so a stale set can never answer Absent.
func (r *Reconciler) markSourceCatalogUnknown(source string) {
	cat, ok := r.sourceCatalogs[source]
	if !ok {
		r.sourceCatalogs[source] = newSourceCatalog(time.Time{}, time.Time{})
		return
	}
	if cat.readiness == readinessReady {
		r.warn("sync.sourceCatalog: source read incomplete; membership unknown for this calendar",
			"source", source,
		)
	}
	cat.readiness = readinessUnknown
}

// noteSourceException records an exception the reverse-materialization path
// just created, so a retry after a failed mirror rewrite sees Present rather
// than materializing a duplicate. A no-op when the calendar is not Ready:
// that calendar's batches are blocked at preflight anyway.
func (r *Reconciler) noteSourceException(source string, inst *gws.Event) {
	cat, ok := r.sourceCatalogs[source]
	if !ok || cat.readiness != readinessReady || inst == nil || inst.RecurringEventID == "" {
		return
	}
	cat.addException(inst)
}

// lookupSourceException is the MembershipLookup the target-delta classifier
// hands to recurring.Handler.
func (r *Reconciler) lookupSourceException(source string, inst *gws.Event) recurring.Membership {
	return r.sourceCatalogs[source].lookup(inst)
}

// sourceCatalogReady reports whether a source calendar's exception index can
// currently answer Absent. Drives the target-batch preflight.
func (r *Reconciler) sourceCatalogReady(source string) bool {
	cat, ok := r.sourceCatalogs[source]
	return ok && cat.readiness == readinessReady
}
