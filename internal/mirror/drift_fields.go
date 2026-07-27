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
// Transparency and visibility are normalized at extraction time by
// ManagedFieldsFromEvent (see convert.go), so the comparison here is a
// straight equality check. Recurrence is compared with order-insensitive
// equality (matching Checksum's sort-before-hash behavior) so the
// field-level diff agrees with the mirror_drifted signal. For instance
// overrides both sides are nil per BuildInstancePayload, so the
// comparison is naturally equal and no drift is reported.
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
	if liveFields.Transparency != desiredFields.Transparency {
		fields = append(fields, "transparency")
	}
	if liveFields.Visibility != desiredFields.Visibility {
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

// BuildSourceOverridePatchBody constructs the events.patch body sent to a
// source recurring-instance ID when target-delta phase needs to materialize
// a mirror-only override onto the source (B17 Phase 2). Differs from
// BuildPropagatePatchBody in two structural ways:
//
//  1. The body carries the mirror's FULL managed fields rather than a
//     drift-filtered subset. The source instance doesn't exist yet (we got
//     here because events.get on the constructed source-instance ID
//     returned 404), so there's no source-side state to merge against - the
//     mirror's live managed fields ARE the desired source state.
//
//  2. The recurrence field is NEVER included by construction. This is the
//     B16 guardrail: per-instance patches that include recurrence get
//     reinterpreted by Google as parent-level updates, silently corrupting
//     every future occurrence of the meeting. Omitting recurrence by
//     construction (rather than conditionally) makes that bug class
//     structurally impossible. A future change that "just needs to add
//     recurrence here" must instead route through the parent's events.patch
//     endpoint via the standard propagate path.
//
// Description trailer-stripping and explicit-clear semantics for empty
// strings match BuildPropagatePatchBody so the two helpers are symmetric on
// the user-edit-side semantics. Status is intentionally NOT set: target
// edits don't carry status mutations through this path (a target-side
// status change is handled by the deleteOrSkip cancellation flow).
//
// Reminders and ExtendedProperties are NOT set: source-side reminders are
// the source's responsibility, and the source's extended-property namespace
// is not ours to write into. The helper is deliberately narrow.
func BuildSourceOverridePatchBody(live *gws.Event) *gws.PatchEvent {
	stripped, _ := StripTrailer(live.Description)
	body := &gws.PatchEvent{
		Summary:      gws.PatchStr(live.Summary),
		Description:  gws.PatchStr(stripped),
		Location:     gws.PatchStr(live.Location),
		Start: live.Start,
		End:   live.End,
		// Normalized, not raw. Google OMITS transparency/visibility when the
		// value equals the default, so a raw read yields "" and the patch
		// sends `"transparency": ""` - not a member of the enum, and a hard
		// 400 (B37). The explicit default is also what keeps this builder
		// consistent with DriftedFieldNames, which compares through the same
		// normalization.
		Transparency: gws.PatchStr(normalizeTransparency(live.Transparency)),
		Visibility:   gws.PatchStr(normalizeVisibility(live.Visibility)),
	}
	// body.Recurrence stays nil. Do NOT add recurrence handling here. See
	// the doc comment above for the B16 rationale.
	return body
}

// BuildPropagatePatchBody constructs the events.patch body sent to the source
// when propagating mirror drift back. The body carries only the drifted
// fields per SPEC.md "Field-level propagate" (so untouched source fields
// like attendees / conferenceData are preserved).
//
// The returned *gws.PatchEvent uses pointer fields so a cleared mirror
// value (e.g. user erased the description) reaches the source as an
// explicit clear rather than being dropped by json:",omitempty".
//
// The description value is the LIVE mirror's description with the trailer
// stripped, per SPEC.md "Drift handling" step 3. If the trailer is the
// entire description (i.e. source body was empty), the stripped result is
// the empty string, which is the correct value to send to source.
//
// Recurrence is special-cased: a nil or empty live.Recurrence in the
// drifted set means "clear recurrence" (a recurring event the user flipped
// to single-instance), so the body uses PatchRecurrenceClear() to send the
// empty-array clear form rather than dropping the field.
//
// Both the recurring-instance handler and the sync-layer classifier use
// this helper; it lives in the mirror package alongside DriftedFieldNames.
func BuildPropagatePatchBody(live *gws.Event, drifted []string) *gws.PatchEvent {
	body := &gws.PatchEvent{}
	for _, f := range drifted {
		switch f {
		case "summary":
			body.Summary = gws.PatchStr(live.Summary)
		case "description":
			stripped, _ := StripTrailer(live.Description)
			body.Description = gws.PatchStr(stripped)
		case "location":
			body.Location = gws.PatchStr(live.Location)
		case "start":
			body.Start = live.Start
		case "end":
			body.End = live.End
		case "transparency":
			// Normalized for the same reason as BuildSourceOverridePatchBody:
			// an omitted value reads as "" and Calendar API rejects that (B37).
			// Omitting the field instead is NOT an option - it is in the
			// drifted set, so leaving it unset would never resolve the drift
			// and every tick would retry.
			body.Transparency = gws.PatchStr(normalizeTransparency(live.Transparency))
		case "visibility":
			body.Visibility = gws.PatchStr(normalizeVisibility(live.Visibility))
		case "recurrence":
			if len(live.Recurrence) == 0 {
				body.Recurrence = gws.PatchRecurrenceClear()
			} else {
				body.Recurrence = gws.PatchRecurrence(live.Recurrence)
			}
		}
	}
	return body
}
