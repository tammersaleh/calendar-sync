package mirror

import (
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

func TestBuildPatchPayload_SetsAllManagedFieldsExplicitly(t *testing.T) {
	// The whole point of BuildPatchPayload over a raw *gws.Event patch:
	// every managed field must be set explicitly, even when zero, so
	// merge-patch semantics overwrite the mirror's existing value with the
	// source's empty value when source has cleared it.
	source := &gws.Event{
		Summary:      "Standup",
		Description:  "body\n\n---\nSource: https://link",
		Location:     "Office",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		Recurrence:   []string{"RRULE:FREQ=DAILY"},
		Reminders:    &gws.Reminders{UseDefault: false},
	}
	got := BuildPatchPayload(source)
	if got.Summary == nil || *got.Summary != "Standup" {
		t.Errorf("Summary = %v, want Standup", got.Summary)
	}
	if got.Description == nil || *got.Description != source.Description {
		t.Errorf("Description = %v, want trailer copy", got.Description)
	}
	if got.Location == nil || *got.Location != "Office" {
		t.Errorf("Location = %v, want Office", got.Location)
	}
	if got.Start == nil || got.Start.DateTime != source.Start.DateTime {
		t.Errorf("Start = %+v, want passthrough", got.Start)
	}
	if got.End == nil || got.End.DateTime != source.End.DateTime {
		t.Errorf("End = %+v, want passthrough", got.End)
	}
	if got.Transparency == nil || *got.Transparency != gws.TransparencyOpaque {
		t.Errorf("Transparency = %v, want opaque", got.Transparency)
	}
	if got.Visibility == nil || *got.Visibility != gws.VisibilityPrivate {
		t.Errorf("Visibility = %v, want private", got.Visibility)
	}
	if got.Recurrence == nil || len(*got.Recurrence) != 1 {
		t.Errorf("Recurrence = %v, want one RRULE", got.Recurrence)
	}
	if got.Reminders != source.Reminders {
		t.Errorf("Reminders pointer should pass through")
	}
}

func TestBuildPatchPayload_DoesNotSetStatusOrID(t *testing.T) {
	// Status is intentionally not set: revive paths populate it themselves;
	// non-revive callers must not stomp the server's current status.
	// ID isn't on PatchEvent at all (URL-bound), so just check Status.
	source := &gws.Event{
		ID:     "deterministic-id",
		Status: gws.EventStatusConfirmed,
	}
	got := BuildPatchPayload(source)
	if got.Status != nil {
		t.Errorf("Status should be nil; got %v", got.Status)
	}
}

func TestBuildPatchPayload_ClearsEmptyStrings(t *testing.T) {
	// A source whose summary/description/location are empty must produce
	// non-nil pointers to "" - the explicit clear-intent. Without this,
	// json:",omitempty" on *gws.Event.Summary would drop the field and the
	// mirror would keep its old value.
	source := &gws.Event{
		Summary:     "",
		Description: "",
		Location:    "",
	}
	got := BuildPatchPayload(source)
	if got.Summary == nil || *got.Summary != "" {
		t.Errorf("Summary must be non-nil pointer to \"\" for clear-intent; got %v", got.Summary)
	}
	if got.Description == nil || *got.Description != "" {
		t.Errorf("Description must be non-nil pointer to \"\" for clear-intent; got %v", got.Description)
	}
	if got.Location == nil || *got.Location != "" {
		t.Errorf("Location must be non-nil pointer to \"\" for clear-intent; got %v", got.Location)
	}
}

func TestBuildPatchPayload_ClearsRecurrenceWhenEmpty(t *testing.T) {
	// Source with no recurrence (nil or []) means "this is not a recurring
	// event" - the patch should instruct Calendar API to clear any
	// recurrence the mirror still has from a previous state.
	source := &gws.Event{Recurrence: nil}
	got := BuildPatchPayload(source)
	if got.Recurrence == nil {
		t.Fatal("Recurrence must be non-nil for clear-intent; got nil")
	}
	if len(*got.Recurrence) != 0 {
		t.Errorf("Recurrence should be empty slice; got %v", *got.Recurrence)
	}
}

func TestBuildPatchPayload_PassesNilStartEndThrough(t *testing.T) {
	// nil Start/End is a degenerate input but must not panic. The pointer
	// passes through as nil ("leave alone"), which is the most defensible
	// behavior - we don't have a clear-form for date/time fields.
	source := &gws.Event{}
	got := BuildPatchPayload(source)
	if got.Start != nil {
		t.Errorf("Start should be nil; got %+v", got.Start)
	}
	if got.End != nil {
		t.Errorf("End should be nil; got %+v", got.End)
	}
}
