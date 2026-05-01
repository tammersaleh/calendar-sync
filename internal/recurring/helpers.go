package recurring

import (
	"errors"
	"sort"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// computeOriginalStart picks the originalStart query string for SPEC.md
// "Step 2": prefer DateTime (timezone-aware RFC 3339) over Date (all-day
// "YYYY-MM-DD"). Returns an error when both are missing - the caller
// routed a non-exception event here, which is a programmer error in the
// classification logic.
func computeOriginalStart(t *gws.EventDateTime) (string, error) {
	if t == nil {
		return "", errors.New("recurring: source.OriginalStartTime is nil")
	}
	if t.DateTime != "" {
		return t.DateTime, nil
	}
	if t.Date != "" {
		return t.Date, nil
	}
	return "", errors.New("recurring: source.OriginalStartTime has neither DateTime nor Date")
}

// sourceOwnerResponseStatus returns the source-owner attendee's
// responseStatus per SPEC.md "Filtering". An empty return string is the
// SPEC's "no rejection signal" - either the event has no attendees array
// at all (owner is implicitly accepted), or no attendee has Self=true,
// or that attendee carries no responseStatus.
func sourceOwnerResponseStatus(e *gws.Event) string {
	for _, a := range e.Attendees {
		if a.Self {
			return a.ResponseStatus
		}
	}
	return ""
}

// driftedFieldNames compares a live mirror Event to the desired-from-source
// payload and returns the names of managed fields that differ.
//
// The comparison is on mirror.ManagedFields, with one description-specific
// adjustment: the trailer is stripped from the live description before
// comparing. Without this adjustment a mirror whose description differs
// only in the trailer (calendar-sync's own append) would produce a
// false-positive description-drift signal.
//
// Recurrence is intentionally not compared - mirror instances always have
// nil recurrence per mirror.BuildInstancePayload, so a difference there
// would be a programmer error, not user drift.
//
// Field name strings match SPEC.md's "fields" array ordering convention:
// they are returned alphabetically sorted so callers (tests, log emitters)
// can compare slices directly without ordering noise.
func driftedFieldNames(live, desired *gws.Event) []string {
	liveFields := mirror.ManagedFieldsFromEvent(live)
	desiredFields := mirror.ManagedFieldsFromEvent(desired)

	// Strip the trailer from the live description before comparing so that
	// a mirror with only-trailer drift doesn't false-positive.
	stripped, _ := mirror.StripTrailer(liveFields.Description)
	desiredStripped, _ := mirror.StripTrailer(desiredFields.Description)
	liveFields.Description = stripped
	desiredFields.Description = desiredStripped

	var fields []string
	if liveFields.Summary != desiredFields.Summary {
		fields = append(fields, "summary")
	}
	if liveFields.Description != desiredFields.Description {
		fields = append(fields, "description")
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
	sort.Strings(fields)
	return fields
}

// buildPropagateBody constructs the events.patch body sent to the source
// when propagating mirror drift back. The body carries only the drifted
// fields per SPEC.md "Field-level propagate" (so untouched source fields
// like attendees / location / conferenceData are preserved).
//
// The description value is the LIVE mirror's description with the trailer
// stripped, per SPEC.md "Drift handling" step 3. If the trailer is the
// entire description (i.e. source body was empty), the stripped result is
// the empty string, which is the correct value to send to source.
func buildPropagateBody(live *gws.Event, drifted []string) *gws.Event {
	body := &gws.Event{}
	for _, f := range drifted {
		switch f {
		case "summary":
			body.Summary = live.Summary
		case "description":
			stripped, _ := mirror.StripTrailer(live.Description)
			body.Description = stripped
		case "start":
			body.Start = live.Start
		case "end":
			body.End = live.End
		case "transparency":
			body.Transparency = live.Transparency
		case "visibility":
			body.Visibility = live.Visibility
		}
	}
	return body
}
