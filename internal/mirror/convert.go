package mirror

import "github.com/tammersaleh/calendar-sync/internal/gws"

// ManagedFieldsFromEvent extracts the subset of fields Checksum hashes
// from a gws.Event resource. Used by the drift-detection step to recompute
// a mirror's checksum from its current state and compare to the stored
// calendar-sync:checksum value.
//
// Per SPEC.md "Computing the checksum from the post-write event", callers
// must pass the post-write Event resource (the one Google returns after
// insert/patch), not the outbound request payload. Pre-write payloads
// haven't been through Google's normalization (timezone canonicalization,
// RRULE re-formatting, etc.) and would produce a checksum that disagrees
// with the next read.
func ManagedFieldsFromEvent(e *gws.Event) ManagedFields {
	return ManagedFields{
		Summary:      e.Summary,
		Description:  e.Description,
		Location:     e.Location,
		Start:        eventDateTimeFromGWS(e.Start),
		End:          eventDateTimeFromGWS(e.End),
		Recurrence:   e.Recurrence,
		Transparency: e.Transparency,
		Visibility:   e.Visibility,
	}
}

// eventDateTimeFromGWS converts gws.EventDateTime (pointer) to mirror's
// value-type EventDateTime. A nil input becomes the zero value (which
// serializes to {} - identical to Google omitting an unset start/end).
func eventDateTimeFromGWS(p *gws.EventDateTime) EventDateTime {
	if p == nil {
		return EventDateTime{}
	}
	return EventDateTime{
		Date:     p.Date,
		DateTime: p.DateTime,
		TimeZone: p.TimeZone,
	}
}
