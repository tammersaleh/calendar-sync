package mirror

import "github.com/tammersaleh/calendar-sync/internal/gws"

// BuildPatchPayload converts a fully-specified desired Event (typically the
// output of BuildPayload / BuildInstancePayload) into a *gws.PatchEvent
// where every managed field is set EXPLICITLY, including zero-value strings.
//
// Use this when you have the full desired state and want a merge patch that
// clears any field the source has cleared. Without this, a managed-field
// patch built straight from *gws.Event would silently drop empty fields
// (json:",omitempty") and Calendar API's merge-patch semantics would
// preserve the old mirror values on a clear-by-source event.
//
// Mappings:
//   - Summary / Description / Location are always set via PatchStr, even
//     when empty. The empty string is the explicit clear-intent.
//   - Start / End / Reminders / ExtendedProperties pass through as
//     pointers; nil on the input means nil on the output (caller's
//     intent is "leave alone").
//   - Transparency / Visibility are always set via PatchStr. Mirrors are
//     forced opaque/private, so these are never empty in practice; the
//     blanket PatchStr keeps the contract consistent with the other
//     string fields.
//   - Recurrence: a non-empty source recurrence sets the array; nil or
//     empty source recurrence sets PatchRecurrenceClear() so the mirror's
//     recurrence is cleared when the source no longer has one (a recurring
//     event flipped to single-instance). Both nil and empty get the same
//     treatment because the wire form is identical (omitempty erases
//     both); the patch needs to express clear-intent unambiguously.
//
// Status is intentionally NOT set: revive paths set status=confirmed
// themselves before sending the patch, and a non-revive full-payload patch
// must not stomp the server's current status (e.g. a mirror that the
// recurring handler is mid-flight cancelling).
//
// ID is not set: events.patch carries the event ID in the URL.
func BuildPatchPayload(e *gws.Event) *gws.PatchEvent {
	p := &gws.PatchEvent{
		Summary:            gws.PatchStr(e.Summary),
		Description:        gws.PatchStr(e.Description),
		Location:           gws.PatchStr(e.Location),
		Start:              e.Start,
		End:                e.End,
		Transparency:       gws.PatchStr(e.Transparency),
		Visibility:         gws.PatchStr(e.Visibility),
		Reminders:          e.Reminders,
		ExtendedProperties: e.ExtendedProperties,
	}
	if len(e.Recurrence) > 0 {
		p.Recurrence = gws.PatchRecurrence(e.Recurrence)
	} else {
		p.Recurrence = gws.PatchRecurrenceClear()
	}
	return p
}
