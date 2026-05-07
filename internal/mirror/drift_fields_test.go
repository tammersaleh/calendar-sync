package mirror

import (
	"reflect"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

func TestDriftedFieldNames_TrailerOnlyDifferenceIsNotDrift(t *testing.T) {
	source := &gws.Event{
		Summary:      "Standup",
		Description:  "real body",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}
	desired := BuildInstancePayload("src-cal", source)

	// The live mirror has the trailer; comparing strict descriptions would
	// flag drift. Stripping the trailer first must dissolve that false signal.
	live := *desired
	got := DriftedFieldNames(&live, desired)
	if len(got) != 0 {
		t.Errorf("expected no drift when only the trailer differs; got %v", got)
	}
}

func TestDriftedFieldNames_UserEditsAreDrift(t *testing.T) {
	source := &gws.Event{
		Summary:      "Standup",
		Description:  "real body",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}
	desired := BuildInstancePayload("src-cal", source)

	live := *desired
	live.Summary = "Edited summary"
	live.Description = "Edited body\n\n---\nSource: " + source.HTMLLink
	live.Start = &gws.EventDateTime{DateTime: "2026-05-01T11:00:00Z"}

	got := DriftedFieldNames(&live, desired)
	wantSet := map[string]bool{"summary": true, "description": true, "start": true}
	gotSet := map[string]bool{}
	for _, f := range got {
		gotSet[f] = true
	}
	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Errorf("got drifted fields %v, want %v", got, []string{"description", "start", "summary"})
	}
}

func TestDriftedFieldNames_ResultIsAlphabeticallySorted(t *testing.T) {
	// Multiple fields drift; assert the returned slice is sorted alphabetically
	// so callers can compare slices directly without ordering noise.
	source := &gws.Event{
		Summary:      "Standup",
		Description:  "body",
		Location:     "Office",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}
	desired := BuildInstancePayload("src-cal", source)

	live := *desired
	live.Visibility = "public"
	live.Transparency = gws.TransparencyTransparent
	live.End = &gws.EventDateTime{DateTime: "2026-05-01T14:00:00Z"}
	live.Summary = "Different"
	live.Location = "Coffee shop"

	got := DriftedFieldNames(&live, desired)
	want := []string{"end", "location", "summary", "transparency", "visibility"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DriftedFieldNames = %v, want %v (alphabetical)", got, want)
	}
}

func TestDriftedFieldNames_LocationDrift(t *testing.T) {
	// Location is a managed field as of v3; a live mirror with a different
	// location than the source must report "location" as drifted.
	source := &gws.Event{
		Summary:      "Lunch",
		Description:  "with bob",
		Location:     "Office",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}
	desired := BuildInstancePayload("src-cal", source)
	live := *desired
	live.Location = "Cafe across the street"

	got := DriftedFieldNames(&live, desired)
	want := []string{"location"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DriftedFieldNames = %v, want %v", got, want)
	}
}

func TestBuildPropagatePatchBody_IncludesLocation(t *testing.T) {
	// When location drifts and SourceWritable=true, propagate carries the
	// live mirror's location to the source via events.patch.
	live := &gws.Event{
		Summary:  "Lunch",
		Location: "User-edited venue",
	}
	body := BuildPropagatePatchBody(live, []string{"location"})
	if body.Location == nil || *body.Location != "User-edited venue" {
		t.Errorf("Location = %v, want %q", body.Location, "User-edited venue")
	}
	if body.Summary != nil {
		t.Errorf("Summary should not be set (not in drifted list); got %v", body.Summary)
	}
}

func TestBuildPropagatePatchBody_StripsTrailerFromDescription(t *testing.T) {
	live := &gws.Event{
		Summary:     "Real summary",
		Description: "user-typed body\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC",
		Start:       &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:         &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
	}
	body := BuildPropagatePatchBody(live, []string{"summary", "description", "start", "end"})
	if body.Summary == nil || *body.Summary != "Real summary" {
		t.Errorf("Summary = %v", body.Summary)
	}
	if body.Description == nil || *body.Description != "user-typed body" {
		t.Errorf("Description = %v, want trailer stripped", body.Description)
	}
	if body.Start == nil || body.Start.DateTime != live.Start.DateTime {
		t.Errorf("Start passthrough wrong; got %+v", body.Start)
	}
	if body.End == nil || body.End.DateTime != live.End.DateTime {
		t.Errorf("End passthrough wrong; got %+v", body.End)
	}
}

func TestBuildPropagatePatchBody_OmitsUndriftedFields(t *testing.T) {
	live := &gws.Event{
		Summary:      "Real summary",
		Description:  "body\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
	}
	body := BuildPropagatePatchBody(live, []string{"summary"})
	if body.Summary == nil || *body.Summary != "Real summary" {
		t.Errorf("Summary = %v", body.Summary)
	}
	if body.Description != nil {
		t.Errorf("Description should not be set; got %v", body.Description)
	}
	if body.Start != nil {
		t.Errorf("Start should not be set; got %+v", body.Start)
	}
	if body.End != nil {
		t.Errorf("End should not be set; got %+v", body.End)
	}
	if body.Transparency != nil {
		t.Errorf("Transparency should not be set; got %v", body.Transparency)
	}
	if body.Visibility != nil {
		t.Errorf("Visibility should not be set; got %v", body.Visibility)
	}
}

func TestBuildPropagatePatchBody_EmptyDriftedFieldsYieldsEmptyBody(t *testing.T) {
	// Defensive: a no-drifted-fields call (which the sync layer would never
	// actually make) returns an empty *gws.PatchEvent without panicking.
	live := &gws.Event{Summary: "anything"}
	body := BuildPropagatePatchBody(live, nil)
	if body == nil {
		t.Fatal("body should never be nil")
	}
	if body.Summary != nil {
		t.Errorf("body should be empty; got %+v", body)
	}
}

func TestBuildPropagatePatchBody_ClearsSummary(t *testing.T) {
	// SPEC: when the user erases the summary on the mirror and propagate
	// runs, the source's summary must be CLEARED, not left untouched. The
	// patch body must carry summary as a non-nil pointer to "" so Calendar
	// API's merge-patch semantics overwrite the existing value rather than
	// preserve it via omitempty.
	live := &gws.Event{Summary: ""}
	body := BuildPropagatePatchBody(live, []string{"summary"})
	if body.Summary == nil {
		t.Fatal("Summary must be present in patch body even when empty (clear-intent); got nil")
	}
	if *body.Summary != "" {
		t.Errorf("Summary = %q, want empty string", *body.Summary)
	}
}

func TestBuildPropagatePatchBody_ClearsRecurrenceToEmptyArray(t *testing.T) {
	// When the user converts a recurring mirror back to a single-instance
	// event (clears the RRULE) the propagate body must instruct Calendar
	// API to clear the source's recurrence array. PatchRecurrenceClear
	// produces a non-nil pointer to []string{}, which marshals to
	// "recurrence":[] - the clear form. Without this, omitempty would drop
	// the field and source would keep its recurrence forever.
	live := &gws.Event{Recurrence: nil}
	body := BuildPropagatePatchBody(live, []string{"recurrence"})
	if body.Recurrence == nil {
		t.Fatal("Recurrence must be present in patch body even when empty (clear-intent); got nil")
	}
	if len(*body.Recurrence) != 0 {
		t.Errorf("Recurrence = %v, want empty slice", *body.Recurrence)
	}
}

func TestDriftedFieldNames_EmptyTransparencyTreatedAsOpaque(t *testing.T) {
	// Google omits transparency from events.list responses when its value
	// equals the default ("opaque"). The drift comparison must treat an empty
	// live transparency as equivalent to the explicit "opaque" that
	// BuildPayload writes; otherwise every round-tripped mirror would
	// false-positive on transparency.
	desired := &gws.Event{
		Summary:      "Standup",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
	}
	live := *desired
	live.Transparency = "" // Google omitted the default

	got := DriftedFieldNames(&live, desired)
	for _, f := range got {
		if f == "transparency" {
			t.Errorf("expected no transparency drift when live is empty (Google's default); got %v", got)
		}
	}
}

func TestDriftedFieldNames_RealTransparencyChangeStillDrifts(t *testing.T) {
	// A genuine non-default value (transparent) must still be detected.
	desired := &gws.Event{
		Summary:      "Standup",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
	}
	live := *desired
	live.Transparency = gws.TransparencyTransparent

	got := DriftedFieldNames(&live, desired)
	found := false
	for _, f := range got {
		if f == "transparency" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected transparency drift for opaque vs transparent; got %v", got)
	}
}

func TestDriftedFieldNames_EmptyVisibilityTreatedAsDefault(t *testing.T) {
	// Mirror visibility="default" against live visibility="" (Google's
	// omitted-default form); the comparison must normalize and produce no
	// drift. Symmetric to the transparency case above.
	desired := &gws.Event{
		Summary:      "Standup",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityDefault,
	}
	live := *desired
	live.Visibility = "" // Google omitted the default

	got := DriftedFieldNames(&live, desired)
	for _, f := range got {
		if f == "visibility" {
			t.Errorf("expected no visibility drift when live is empty (Google's default); got %v", got)
		}
	}
}

func TestDriftedFieldNames_RealVisibilityChangeStillDrifts(t *testing.T) {
	// A genuine value mismatch (public vs private) must still be detected.
	desired := &gws.Event{
		Summary:      "Standup",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
	}
	live := *desired
	live.Visibility = gws.VisibilityPublic

	got := DriftedFieldNames(&live, desired)
	found := false
	for _, f := range got {
		if f == "visibility" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected visibility drift for private vs public; got %v", got)
	}
}

func TestDriftedFieldNames_RecurrenceChangeOnParentDrifts(t *testing.T) {
	// A user editing a recurring mirror parent's RRULE must enter the drifted
	// set; otherwise propagate runs with an empty body and the user's edit
	// silently reverts when the mirror is re-written from BuildPayload(source).
	desired := &gws.Event{
		Summary:      "Standup",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Recurrence:   []string{"RRULE:FREQ=DAILY;COUNT=3"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
	}
	live := *desired
	live.Recurrence = []string{"RRULE:FREQ=WEEKLY;COUNT=4"}

	got := DriftedFieldNames(&live, desired)
	found := false
	for _, f := range got {
		if f == "recurrence" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected recurrence drift for parent edit; got %v", got)
	}
}

func TestDriftedFieldNames_RecurrenceMatchesIgnoringOrder(t *testing.T) {
	// Mirrors Checksum's sort-before-hash behavior: two recurrence arrays
	// with the same content in different order must be treated as equal so
	// the field-level diff agrees with the checksum signal.
	desired := &gws.Event{
		Summary:      "Standup",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Recurrence:   []string{"RRULE:FREQ=DAILY", "EXDATE;TZID=UTC:20260507T120000"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
	}
	live := *desired
	live.Recurrence = []string{"EXDATE;TZID=UTC:20260507T120000", "RRULE:FREQ=DAILY"}

	got := DriftedFieldNames(&live, desired)
	for _, f := range got {
		if f == "recurrence" {
			t.Errorf("expected no recurrence drift when contents match (order-insensitive); got %v", got)
		}
	}
}

func TestDriftedFieldNames_RecurrenceBothNilNoChange(t *testing.T) {
	// Instance overrides have nil recurrence on both sides per
	// BuildInstancePayload; the comparison must treat that as equal.
	desired := &gws.Event{
		Summary:      "Override",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
	}
	live := *desired

	got := DriftedFieldNames(&live, desired)
	for _, f := range got {
		if f == "recurrence" {
			t.Errorf("expected no recurrence drift when both nil; got %v", got)
		}
	}
}

func TestDriftedFieldNames_RecurrenceEmptySliceVsNil(t *testing.T) {
	// An empty []string and a nil slice are both "no recurrence" on the wire
	// (omitempty erases both); the field-level comparison must not flag
	// drift between them.
	desired := &gws.Event{
		Summary:      "Override",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Recurrence:   []string{},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
	}
	live := *desired
	live.Recurrence = nil

	got := DriftedFieldNames(&live, desired)
	for _, f := range got {
		if f == "recurrence" {
			t.Errorf("expected no recurrence drift when one side is empty and the other nil; got %v", got)
		}
	}
}

func TestBuildPropagatePatchBody_IncludesRecurrence(t *testing.T) {
	// When recurrence is in the drifted set, the propagate body carries the
	// LIVE mirror's recurrence verbatim so the user's edit reaches source.
	live := &gws.Event{
		Summary:    "Standup",
		Recurrence: []string{"RRULE:FREQ=WEEKLY;COUNT=4"},
	}
	body := BuildPropagatePatchBody(live, []string{"recurrence"})
	if body.Recurrence == nil {
		t.Fatal("Recurrence missing from patch body")
	}
	if len(*body.Recurrence) != 1 || (*body.Recurrence)[0] != "RRULE:FREQ=WEEKLY;COUNT=4" {
		t.Errorf("Recurrence = %v, want [RRULE:FREQ=WEEKLY;COUNT=4]", *body.Recurrence)
	}
	if body.Summary != nil {
		t.Errorf("Summary should not be set (not in drifted list); got %v", body.Summary)
	}
}

// ---------- BuildSourceOverridePatchBody (B17 Phase 2) ----------
//
// BuildSourceOverridePatchBody is the per-instance variant of
// BuildPropagatePatchBody used when target-delta phase hits a 404 on the
// source instance lookup (Phase 2's mirror-only override path). Unlike
// BuildPropagatePatchBody, the body is built from the mirror's full managed
// fields (we're materializing the user's complete state to the source's
// previously-auto-materialized occurrence), and the recurrence field is
// NEVER included by construction.
//
// The recurrence omission is the B16 guardrail: Google reinterprets a
// per-instance patch with recurrence as a parent-level update, silently
// corrupting every future occurrence of the meeting. The helper omitting
// recurrence by construction (rather than conditionally) makes that class
// of bug structurally impossible.

func TestBuildSourceOverridePatchBody_IncludesAllManagedFieldsExceptRecurrence(t *testing.T) {
	// Pin every managed field appears in the body output, EXCEPT recurrence.
	// This is what the helper has to do to materialize the user's full state
	// onto the source's instance override.
	live := &gws.Event{
		Summary:      "Lunch-edited",
		Description:  "real body\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC",
		Location:     "Cafe",
		Start:        &gws.EventDateTime{DateTime: "2026-05-20T16:30:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-20T17:30:00Z"},
		Transparency: gws.TransparencyTransparent,
		Visibility:   gws.VisibilityPublic,
		Recurrence:   []string{"RRULE:FREQ=WEEKLY;COUNT=4"}, // must NOT propagate
	}
	body := BuildSourceOverridePatchBody(live)

	if body.Summary == nil || *body.Summary != "Lunch-edited" {
		t.Errorf("Summary = %v, want %q", body.Summary, "Lunch-edited")
	}
	if body.Description == nil || *body.Description != "real body" {
		t.Errorf("Description = %v, want trailer-stripped 'real body'", body.Description)
	}
	if body.Location == nil || *body.Location != "Cafe" {
		t.Errorf("Location = %v, want %q", body.Location, "Cafe")
	}
	if body.Start == nil || body.Start.DateTime != "2026-05-20T16:30:00Z" {
		t.Errorf("Start = %+v", body.Start)
	}
	if body.End == nil || body.End.DateTime != "2026-05-20T17:30:00Z" {
		t.Errorf("End = %+v", body.End)
	}
	if body.Transparency == nil || *body.Transparency != gws.TransparencyTransparent {
		t.Errorf("Transparency = %v", body.Transparency)
	}
	if body.Visibility == nil || *body.Visibility != gws.VisibilityPublic {
		t.Errorf("Visibility = %v", body.Visibility)
	}
	if body.Recurrence != nil {
		t.Errorf("Recurrence MUST be omitted (B16 guardrail); got %v", body.Recurrence)
	}
}

func TestBuildSourceOverridePatchBody_NeverIncludesRecurrence(t *testing.T) {
	// Belt-and-suspenders: regardless of what shape the live mirror's
	// recurrence is in (nil, empty, populated, multi-line), the output body's
	// recurrence field stays nil. This guards against a future change adding
	// recurrence handling to the helper - which would resurrect the B16 bug
	// class (per-instance patch with recurrence -> Google reinterprets as
	// parent update -> corrupts every future occurrence).
	tests := []struct {
		name       string
		recurrence []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"single RRULE", []string{"RRULE:FREQ=DAILY"}},
		{"RRULE+EXDATE", []string{"RRULE:FREQ=DAILY", "EXDATE;TZID=UTC:20260507T120000"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live := &gws.Event{
				Summary:    "x",
				Recurrence: tt.recurrence,
			}
			body := BuildSourceOverridePatchBody(live)
			if body.Recurrence != nil {
				t.Errorf("Recurrence must be nil; got %v (recurrence in: %v)",
					body.Recurrence, tt.recurrence)
			}
		})
	}
}

func TestBuildSourceOverridePatchBody_StripsTrailerFromDescription(t *testing.T) {
	// Same trailer-stripping behavior as BuildPropagatePatchBody: the user's
	// edit on the mirror description is the live value with the calendar-sync
	// trailer appended; sending that to the source would snowball the trailer
	// on the next round-trip.
	live := &gws.Event{
		Summary:     "Real summary",
		Description: "user-typed body\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC",
		Start:       &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:         &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
	}
	body := BuildSourceOverridePatchBody(live)
	if body.Description == nil || *body.Description != "user-typed body" {
		t.Errorf("Description = %v, want trailer-stripped 'user-typed body'", body.Description)
	}
}

func TestBuildSourceOverridePatchBody_ClearsEmptyFields(t *testing.T) {
	// Same explicit-clear semantics as BuildPropagatePatchBody: a user who
	// erased the description on the mirror (or any string field) must reach
	// the source as a non-nil pointer to "" so Calendar API's merge-patch
	// semantics overwrite the existing value rather than preserve it via
	// omitempty. Without this, a cleared mirror description would round-trip
	// the source's stale description back onto the mirror via the post-patch
	// rewrite.
	live := &gws.Event{
		Summary:     "",
		Description: "",
		Location:    "",
		Start:       &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:         &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
	}
	body := BuildSourceOverridePatchBody(live)
	if body.Summary == nil || *body.Summary != "" {
		t.Errorf("Summary must be present in patch body even when empty (clear-intent); got %v", body.Summary)
	}
	if body.Description == nil || *body.Description != "" {
		t.Errorf("Description must be present in patch body even when empty (clear-intent); got %v", body.Description)
	}
	if body.Location == nil || *body.Location != "" {
		t.Errorf("Location must be present in patch body even when empty (clear-intent); got %v", body.Location)
	}
}
