package mirror

import (
	"sort"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// DriftedFieldNames compares a live mirror Event to the desired-from-source
// payload and returns the names of managed fields that differ.
//
// The comparison is on ManagedFields, with one description-specific
// adjustment: the trailer is stripped from the live description before
// comparing. Without this adjustment a mirror whose description differs
// only in the trailer (calendar-sync's own append) would produce a
// false-positive description-drift signal.
//
// Recurrence is compared with order-insensitive equality (matching
// Checksum's sort-before-hash behavior) so the field-level diff agrees
// with the mirror_drifted signal. For instance overrides both sides are
// nil per BuildInstancePayload, so the comparison is naturally equal and
// no drift is reported.
//
// Field name strings match SPEC.md's "fields" array ordering convention:
// they are returned alphabetically sorted so callers (tests, log emitters)
// can compare slices directly without ordering noise.
//
// Both the recurring-instance handler (per-instance drift) and the sync-layer
// classifier (per-parent drift) consume this helper; it lives in the mirror
// package to keep both layers free of an import cycle.
func DriftedFieldNames(live, desired *gws.Event) []string {
	liveFields := ManagedFieldsFromEvent(live)
	desiredFields := ManagedFieldsFromEvent(desired)

	// Strip the trailer from the live description before comparing so that
	// a mirror with only-trailer drift doesn't false-positive.
	stripped, _ := StripTrailer(liveFields.Description)
	desiredStripped, _ := StripTrailer(desiredFields.Description)
	liveFields.Description = stripped
	desiredFields.Description = desiredStripped

	var fields []string
	if liveFields.Summary != desiredFields.Summary {
		fields = append(fields, "summary")
	}
	if liveFields.Description != desiredFields.Description {
		fields = append(fields, "description")
	}
	if liveFields.Location != desiredFields.Location {
		fields = append(fields, "location")
	}
	if liveFields.Start != desiredFields.Start {
		fields = append(fields, "start")
	}
	if liveFields.End != desiredFields.End {
		fields = append(fields, "end")
	}
	if normalizeTransparency(liveFields.Transparency) != normalizeTransparency(desiredFields.Transparency) {
		fields = append(fields, "transparency")
	}
	if normalizeVisibility(liveFields.Visibility) != normalizeVisibility(desiredFields.Visibility) {
		fields = append(fields, "visibility")
	}
	if !recurrenceEqual(liveFields.Recurrence, desiredFields.Recurrence) {
		fields = append(fields, "recurrence")
	}
	sort.Strings(fields)
	return fields
}

// recurrenceEqual reports equality for recurrence arrays, ignoring order.
// Matches Checksum's sort-before-hash behavior so a parent whose checksum
// matches the stored value also has no recurrence drift in the field-level
// diff. Treats nil and an empty slice as equivalent (both serialize the
// same way thanks to omitempty).
func recurrenceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	aSorted := make([]string, len(a))
	bSorted := make([]string, len(b))
	copy(aSorted, a)
	copy(bSorted, b)
	sort.Strings(aSorted)
	sort.Strings(bSorted)
	for i := range aSorted {
		if aSorted[i] != bSorted[i] {
			return false
		}
	}
	return true
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

// BuildPropagatePatchBody constructs the events.patch body sent to the source
// when propagating mirror drift back. The body carries only the drifted
// fields per SPEC.md "Field-level propagate" (so untouched source fields
// like attendees / conferenceData are preserved).
//
// The description value is the LIVE mirror's description with the trailer
// stripped, per SPEC.md "Drift handling" step 3. If the trailer is the
// entire description (i.e. source body was empty), the stripped result is
// the empty string, which is the correct value to send to source.
//
// Both the recurring-instance handler and the sync-layer classifier use
// this helper; it lives in the mirror package alongside DriftedFieldNames.
func BuildPropagatePatchBody(live *gws.Event, drifted []string) *gws.Event {
	body := &gws.Event{}
	for _, f := range drifted {
		switch f {
		case "summary":
			body.Summary = live.Summary
		case "description":
			stripped, _ := StripTrailer(live.Description)
			body.Description = stripped
		case "location":
			body.Location = live.Location
		case "start":
			body.Start = live.Start
		case "end":
			body.End = live.End
		case "transparency":
			body.Transparency = live.Transparency
		case "visibility":
			body.Visibility = live.Visibility
		case "recurrence":
			body.Recurrence = live.Recurrence
		}
	}
	return body
}
