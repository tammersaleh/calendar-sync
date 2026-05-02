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
	if body.Location != "User-edited venue" {
		t.Errorf("Location = %q, want %q", body.Location, "User-edited venue")
	}
	if body.Summary != "" {
		t.Errorf("Summary should not be set (not in drifted list); got %q", body.Summary)
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
	if body.Summary != "Real summary" {
		t.Errorf("Summary = %q", body.Summary)
	}
	if body.Description != "user-typed body" {
		t.Errorf("Description = %q, want trailer stripped", body.Description)
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
	if body.Summary != "Real summary" {
		t.Errorf("Summary = %q", body.Summary)
	}
	if body.Description != "" {
		t.Errorf("Description should not be set; got %q", body.Description)
	}
	if body.Start != nil {
		t.Errorf("Start should not be set; got %+v", body.Start)
	}
	if body.End != nil {
		t.Errorf("End should not be set; got %+v", body.End)
	}
	if body.Transparency != "" {
		t.Errorf("Transparency should not be set; got %q", body.Transparency)
	}
	if body.Visibility != "" {
		t.Errorf("Visibility should not be set; got %q", body.Visibility)
	}
}

func TestBuildPropagatePatchBody_EmptyDriftedFieldsYieldsEmptyBody(t *testing.T) {
	// Defensive: a no-drifted-fields call (which the sync layer would never
	// actually make) returns an empty *gws.Event without panicking.
	live := &gws.Event{Summary: "anything"}
	body := BuildPropagatePatchBody(live, nil)
	if body == nil {
		t.Fatal("body should never be nil")
	}
	if body.Summary != "" {
		t.Errorf("body should be empty; got %+v", body)
	}
}
