package recurring

import "github.com/tammersaleh/calendar-sync/internal/gws"

// Membership reports whether a source calendar's unexpanded event collection
// actually contains a given recurring-instance exception.
//
// events.get on a constructed source-instance ID cannot answer the question:
// Google returns 200 with a synthesized VIRTUAL occurrence for every slot the
// parent's RRULE produces, whether or not a real exception exists there. That
// 200-instead-of-404 shape is B29. A complete
// events.list(singleEvents=false) never manufactures virtual instances, so
// collection membership is the only sound discriminator between "the source
// already holds an override here, so the mirror's differing fields are
// bootstrap state" and "the mirror's differing fields are the user's own
// edit". etag, sequence, start-vs-originalStartTime and managed-field
// comparison were all tried against live calendars and all fail.
type Membership int

const (
	// MembershipPresent means the collection contains this exact instance
	// ID. It is the zero value so a Handler with no membership callback
	// keeps the pre-B29 behaviour: correct for the source-delta path, where
	// the source event came out of the collection and is therefore present
	// by construction.
	MembershipPresent Membership = iota

	// MembershipAbsent means the collection is authoritative here and does
	// not contain the instance. The occurrence is virtual.
	MembershipAbsent

	// MembershipUnknown means the collection could not be proven complete
	// (a failed, truncated or 410'd list), so no claim can be made about any
	// occurrence in that calendar.
	MembershipUnknown

	// MembershipOutOfScope means the occurrence falls outside the time
	// window the collection snapshot covered. Absence proves nothing there.
	MembershipOutOfScope
)

// String renders a Membership for log lines.
func (m Membership) String() string {
	switch m {
	case MembershipPresent:
		return "present"
	case MembershipAbsent:
		return "absent"
	case MembershipUnknown:
		return "unknown"
	case MembershipOutOfScope:
		return "out_of_scope"
	}
	return "invalid"
}

// MembershipLookup reports the collection membership of one source recurring
// instance. sourceInstance is the resource events.get returned for the
// constructed instance ID; the sync layer matches on its ID and uses its
// start/end for the coverage check.
//
// Injected as a function value for the same reason LookupMirrorParent is:
// internal/recurring cannot import internal/sync without a cycle.
type MembershipLookup func(sourceInstance *gws.Event) Membership

// SourceExceptionNoter records a source exception the handler has just
// materialized so a later lookup reports MembershipPresent for it.
//
// The handler calls it immediately after the source patch succeeds and
// before the mirror rewrite. If the rewrite then fails, the target
// syncToken stays pinned and the next tick re-delivers the same edit; a
// catalog that still said Absent would materialize a second override.
type SourceExceptionNoter func(sourceInstance *gws.Event)
