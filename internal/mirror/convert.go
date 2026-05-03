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
//
// Transparency and Visibility are normalized here so that the same Event
// produces the same ManagedFields regardless of whether it came from a
// patch response (where Google echoes back BuildPayload's explicit
// "opaque"/"default") or a list response (where Google omits both fields
// when their value equals the default). Without normalization, the stored
// post-write checksum would disagree with the next read's live recompute
// and fire MirrorDrifted on every sync cycle for unmodified mirrors.
func ManagedFieldsFromEvent(e *gws.Event) ManagedFields {
	return ManagedFields{
		Summary:      e.Summary,
		Description:  e.Description,
		Location:     e.Location,
		Start:        eventDateTimeFromGWS(e.Start),
		End:          eventDateTimeFromGWS(e.End),
		Recurrence:   e.Recurrence,
		Transparency: normalizeTransparency(e.Transparency),
		Visibility:   normalizeVisibility(e.Visibility),
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

// normalizeTransparency treats Google's omission of the default ("opaque")
// the same as an explicit "opaque" so a managed-field comparison doesn't
// report drift on a mirror whose value just round-tripped through the API.
// events.list responses omit transparency when the value equals the default;
// BuildPayload writes the explicit form, so a freshly round-tripped mirror
// would otherwise false-positive on every drift check.
func normalizeTransparency(t string) string {
	if t == "" {
		return gws.TransparencyOpaque
	}
	return t
}

// normalizeVisibility treats Google's omission of the default ("default")
// the same as an explicit "default". Calendar-sync mirrors force
// visibility="private" which Google preserves, so this normalization rarely
// matters in practice; included for symmetry and to handle the case where
// a source event's visibility comes back omitted on the propagate path.
func normalizeVisibility(v string) string {
	if v == "" {
		return gws.VisibilityDefault
	}
	return v
}
