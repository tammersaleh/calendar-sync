package mirror

import "github.com/tammersaleh/calendar-sync/internal/gws"

// SourceOwnerResponseStatus returns the source-owner attendee's
// responseStatus per SPEC.md "Filtering". An empty return string is the
// SPEC's "no rejection signal" - either the event has no attendees array
// at all (owner is implicitly accepted), or no attendee has Self=true,
// or that attendee carries no responseStatus.
//
// Lives in the mirror package so both the sync-layer classifier and the
// recurring-instance handler can share the implementation without
// duplicating it or introducing a cross-layer import.
func SourceOwnerResponseStatus(e *gws.Event) string {
	for _, a := range e.Attendees {
		if a.Self {
			return a.ResponseStatus
		}
	}
	return ""
}
