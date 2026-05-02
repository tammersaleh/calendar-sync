package mirror

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

func TestBuildPayload_HappyPath(t *testing.T) {
	source := &gws.Event{
		ID:          "evt-abc",
		Summary:     "Lunch",
		Description: "with bob",
		Location:    "Office cafe",
		Start:       &gws.EventDateTime{DateTime: "2026-04-30T12:00:00Z", TimeZone: "UTC"},
		End:         &gws.EventDateTime{DateTime: "2026-04-30T13:00:00Z", TimeZone: "UTC"},
		Updated:     "2026-04-29T23:00:00.000Z",
		HTMLLink:    "https://www.google.com/calendar/event?eid=ABC",
	}

	got := BuildPayload("alice@example.com", source)

	wantID := DeterministicID("alice@example.com", "evt-abc")
	if got.ID != wantID {
		t.Errorf("ID = %q, want %q", got.ID, wantID)
	}
	if got.Summary != "Lunch" {
		t.Errorf("Summary = %q, want Lunch", got.Summary)
	}
	if got.Location != "Office cafe" {
		t.Errorf("Location = %q, want %q", got.Location, "Office cafe")
	}
	if got.Transparency != gws.TransparencyOpaque {
		t.Errorf("Transparency = %q, want opaque", got.Transparency)
	}
	if got.Visibility != gws.VisibilityPrivate {
		t.Errorf("Visibility = %q, want private", got.Visibility)
	}
	if got.Reminders == nil || got.Reminders.UseDefault {
		t.Errorf("Reminders.UseDefault must be false; got %#v", got.Reminders)
	}
	wantDescription := "with bob\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC"
	if got.Description != wantDescription {
		t.Errorf("Description = %q\nwant %q", got.Description, wantDescription)
	}
	// Start/End passed through by reference.
	if got.Start != source.Start {
		t.Errorf("Start should be the same pointer as source.Start")
	}
}

func TestBuildPayload_LocationCopiedFromSource(t *testing.T) {
	// Location is a managed field as of v3 (SPEC.md "Managed fields and the
	// checksum"). BuildPayload copies it verbatim from source onto the
	// mirror.
	source := &gws.Event{
		ID:       "evt-loc",
		Location: "1234 Main St, Springfield",
		HTMLLink: "https://www.google.com/calendar/event?eid=LOC",
	}
	got := BuildPayload("c@example.com", source)
	if got.Location != source.Location {
		t.Errorf("Location = %q, want %q (copied verbatim from source)", got.Location, source.Location)
	}
}

func TestBuildPayload_LocationEmptyStaysEmpty(t *testing.T) {
	// Source with no location -> mirror with no location. Don't synthesize.
	source := &gws.Event{
		ID:       "evt-noloc",
		HTMLLink: "https://www.google.com/calendar/event?eid=N",
	}
	got := BuildPayload("c@example.com", source)
	if got.Location != "" {
		t.Errorf("Location = %q, want empty", got.Location)
	}
}

func TestSchemaVersion_IsThree(t *testing.T) {
	// Pin the v3 bump (SPEC.md "Schema version migration"). v3 added
	// Location to the managed-fields set; bumping the constant routes
	// existing v1 and v2 mirrors through the migration path.
	if SchemaVersion != "3" {
		t.Errorf("SchemaVersion = %q, want %q", SchemaVersion, "3")
	}
}

func TestBuildPayload_EmptySourceDescription(t *testing.T) {
	// Per SPEC.md "Mirror event payload (insert and patch)": "If source
	// description is empty, just the trailer." The trailer's literal form
	// is "\n\n---\nSource: <htmlLink>" so the empty-source description
	// becomes that exact string. This keeps StripTrailer's anchored-to-
	// end regex able to recover the trailer cleanly.
	source := &gws.Event{
		ID:       "e",
		HTMLLink: "https://www.google.com/calendar/event?eid=X",
	}
	got := BuildPayload("c@example.com", source)
	want := "\n\n---\nSource: https://www.google.com/calendar/event?eid=X"
	if got.Description != want {
		t.Errorf("Description = %q, want %q", got.Description, want)
	}

	// And the trailer regex should match (proving propagate/strip works).
	stripped, ok := StripTrailer(got.Description)
	if !ok {
		t.Errorf("StripTrailer did not recognize the empty-source trailer")
	}
	if stripped != "" {
		t.Errorf("stripped = %q, want empty string", stripped)
	}
}

func TestBuildPayload_ExtendedProperties(t *testing.T) {
	source := &gws.Event{
		ID:       "evt-1",
		Updated:  "2026-04-29T23:00:00.000Z",
		HTMLLink: "https://www.google.com/calendar/event?eid=X",
	}
	got := BuildPayload("alice@example.com", source)

	if got.ExtendedProperties == nil {
		t.Fatal("ExtendedProperties is nil")
	}
	priv := got.ExtendedProperties.Private
	if priv == nil {
		t.Fatal("ExtendedProperties.Private is nil")
	}
	wantSource := "alice@example.com:evt-1"
	if priv[ExtKeySource] != wantSource {
		t.Errorf("private[%q] = %q, want %q", ExtKeySource, priv[ExtKeySource], wantSource)
	}
	if priv[ExtKeySourceUpdated] != "2026-04-29T23:00:00.000Z" {
		t.Errorf("private[%q] = %q", ExtKeySourceUpdated, priv[ExtKeySourceUpdated])
	}
	if priv[ExtKeyVersion] != SchemaVersion {
		t.Errorf("private[%q] = %q, want %q", ExtKeyVersion, priv[ExtKeyVersion], SchemaVersion)
	}
	// Checksum is intentionally NOT set on the initial payload; it's a
	// follow-up patch using the post-write resource per SPEC.md
	// "Computing the checksum from the post-write event".
	if _, present := priv[ExtKeyChecksum]; present {
		t.Errorf("private[%q] should be absent on initial payload; got %q",
			ExtKeyChecksum, priv[ExtKeyChecksum])
	}
}

func TestBuildPayload_PreservesRecurrence(t *testing.T) {
	rrule := []string{"RRULE:FREQ=WEEKLY", "EXDATE;TZID=UTC:20260507T120000"}
	source := &gws.Event{
		ID:         "rec-1",
		Recurrence: rrule,
	}
	got := BuildPayload("c", source)
	if !reflect.DeepEqual(got.Recurrence, rrule) {
		t.Errorf("Recurrence = %v, want %v", got.Recurrence, rrule)
	}
}

func TestBuildInstancePayload_OmitsRecurrence(t *testing.T) {
	// SPEC.md "Recurring Events" / "Step 3": instance payloads omit
	// recurrence (it belongs to the parent, not the override).
	source := &gws.Event{
		ID:               "exc-1",
		RecurringEventID: "parent-1",
		Recurrence:       []string{"RRULE:FREQ=WEEKLY"}, // bogus on an instance
	}
	got := BuildInstancePayload("c@example.com", source)
	if got.Recurrence != nil {
		t.Errorf("Recurrence on instance payload = %v, want nil", got.Recurrence)
	}
}

func TestBuildInstancePayload_SourceTuplePointsAtException(t *testing.T) {
	// SPEC.md: the instance mirror's calendar-sync:source carries the
	// EXCEPTION's ID, not the parent's, so a direct lookup later finds
	// the right instance.
	source := &gws.Event{
		ID:               "exc-1",
		RecurringEventID: "parent-1",
	}
	got := BuildInstancePayload("c@example.com", source)
	want := "c@example.com:exc-1"
	if got.ExtendedProperties.Private[ExtKeySource] != want {
		t.Errorf("private[%q] = %q, want %q", ExtKeySource,
			got.ExtendedProperties.Private[ExtKeySource], want)
	}
}

func TestBuildPayload_TrailerStripsCleanly(t *testing.T) {
	// End-to-end check: the description we build must round-trip through
	// StripTrailer cleanly. This guards against trailer-format drift
	// between BuildPayload and trailerPattern.
	source := &gws.Event{
		ID:          "e1",
		Description: "Some body text",
		HTMLLink:    "https://www.google.com/calendar/event?eid=ABC123_-=",
	}
	got := BuildPayload("c", source)
	stripped, ok := StripTrailer(got.Description)
	if !ok {
		t.Fatalf("StripTrailer did not recognize the trailer in %q", got.Description)
	}
	if stripped != "Some body text" {
		t.Errorf("stripped body = %q, want 'Some body text'", stripped)
	}
}

func TestBuildPayload_NilSourcePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil source")
		}
	}()
	BuildPayload("c", nil)
}

func TestBuildPayload_DescriptionFormatStartsWithExpectedPrefix(t *testing.T) {
	source := &gws.Event{ID: "e", Description: "x", HTMLLink: "https://www.google.com/calendar/event?eid=Y"}
	got := BuildPayload("c", source)
	if !strings.Contains(got.Description, "\n\n---\nSource: https://www.google.com/calendar/event?eid=Y") {
		t.Errorf("description missing expected trailer fragment; got %q", got.Description)
	}
}
