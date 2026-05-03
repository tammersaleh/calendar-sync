package mirror

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// ErrInvalidSourceTuple is returned by ParseSourceTuple when the input is not
// in "<calendar_id>:<event_id>" form with both halves non-empty.
var ErrInvalidSourceTuple = errors.New("invalid calendar-sync:source value")

// SourceTuple is the value of the calendar-sync:source extended property on
// every mirror event: the canonical source calendar ID joined to the source
// event ID by a colon. It identifies which source event a mirror is tracking
// and is the lookup key for the daemon's per-target mirror inventory.
type SourceTuple struct {
	CalendarID string
	EventID    string
}

// String returns the "<calendar_id>:<event_id>" wire form stored on each
// mirror's calendar-sync:source extended property.
func (s SourceTuple) String() string {
	return s.CalendarID + ":" + s.EventID
}

// ParseSourceTuple is the inverse of (SourceTuple).String. It splits on the
// first colon (calendar IDs are emails or "<hash>@group.calendar.google.com",
// neither of which contains a colon; Google event IDs are [a-v0-9]). Both
// halves must be non-empty.
func ParseSourceTuple(s string) (SourceTuple, error) {
	cal, event, ok := strings.Cut(s, ":")
	if !ok || cal == "" || event == "" {
		return SourceTuple{}, fmt.Errorf("%w: %q", ErrInvalidSourceTuple, s)
	}
	return SourceTuple{CalendarID: cal, EventID: event}, nil
}

// IsInheritedRecurringInstance reports whether mirrorEvent is an
// auto-materialized recurring-instance mirror that has not been explicitly
// written by calendar-sync.
//
// When calendar-sync writes a recurring parent mirror, Google Calendar
// materializes the parent's instances using its recurrence rule and copies
// the parent's extendedProperties.private to each materialized instance. The
// inherited copy carries a calendar-sync:source value pointing at the source
// PARENT - the EventID portion equals sourceParentID. By contrast, when the
// recurring handler explicitly writes an instance, BuildInstancePayload sets
// calendar-sync:source to the source-instance tuple (EventID = the exception's
// own ID with the "_<UTC>" suffix), so inherited and managed forms are
// distinguishable by string compare.
//
// sourceParentID is the source event's RecurringEventID (its parent ID).
// Returns false for nil events, missing/malformed source extended properties,
// empty sourceParentID, or any mismatch with the parent ID.
func IsInheritedRecurringInstance(mirrorEvent *gws.Event, sourceParentID string) bool {
	if mirrorEvent == nil || sourceParentID == "" {
		return false
	}
	tuple, err := ParseSourceTuple(extPropPrivate(mirrorEvent, ExtKeySource))
	if err != nil {
		return false
	}
	return tuple.EventID == sourceParentID
}
