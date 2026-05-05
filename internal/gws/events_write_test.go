package gws_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/testhelpers"
)

func TestEventsInsert_HappyPath(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				// Calendar API returns the post-write resource. Note that
				// the response's "updated" timestamp is what the caller
				// will store as calendar-sync:source_updated.
				Stdout: `{"id":"cs2abc","summary":"Lunch","updated":"2026-04-30T12:00:00.000Z","status":"confirmed"}`,
				Exit:   0,
			},
		},
	}

	body := &gws.Event{
		ID:      "cs2abc",
		Summary: "Lunch",
		Start:   &gws.EventDateTime{DateTime: "2026-04-30T12:00:00Z"},
		End:     &gws.EventDateTime{DateTime: "2026-04-30T13:00:00Z"},
	}

	var (
		got    *gws.Event
		gotErr error
	)
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		got, gotErr = gws.New().EventsInsert(context.Background(), "alice@example.com", body)
	})

	if gotErr != nil {
		t.Fatalf("EventsInsert returned error: %v", gotErr)
	}
	if got.ID != "cs2abc" || got.Updated == "" {
		t.Errorf("response = %#v, want id=cs2abc and non-empty updated", got)
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 gws call, got %d", len(calls))
	}
	args := calls[0].Args
	if !argvHasPrefix(args, "calendar", "events", "insert") {
		t.Errorf("args prefix = %v, want [calendar events insert]", args)
	}
	if !contains(args, "--json") {
		t.Errorf("argv missing --json flag: %v", args)
	}

	if cal, _ := calls[0].Params["calendarId"].(string); cal != "alice@example.com" {
		t.Errorf("--params calendarId = %v, want alice@example.com", calls[0].Params["calendarId"])
	}

	gotBody, ok := calls[0].Body.(map[string]any)
	if !ok {
		t.Fatalf("--json body was not parsed as object: %#v", calls[0].Body)
	}
	if id, _ := gotBody["id"].(string); id != "cs2abc" {
		t.Errorf("--json id = %v, want cs2abc (caller-supplied deterministic ID)", gotBody["id"])
	}
	if summary, _ := gotBody["summary"].(string); summary != "Lunch" {
		t.Errorf("--json summary = %v, want Lunch", gotBody["summary"])
	}
}

func TestEventsInsert_NilBodyRejected(t *testing.T) {
	// No scenario calls expected; the wrapper must reject before subprocess.
	var gotErr error
	testhelpers.WithFakeGWS(t, testhelpers.Scenario{}, func() {
		_, gotErr = gws.New().EventsInsert(context.Background(), "x", nil)
	})
	if gotErr == nil {
		t.Fatal("expected error when body is nil")
	}
}

func TestEventsInsert_NonZeroExitReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stderr: `{"error":{"code":409,"message":"duplicate"}}`, Exit: 1},
		},
	}
	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().EventsInsert(context.Background(), "x", &gws.Event{ID: "cs2dup"})
	})
	if gotErr == nil {
		t.Fatal("expected error on non-zero exit")
	}
}

func TestEventsInsert_409SurfacesAsTypedAPIConflict(t *testing.T) {
	// SPEC.md "Mirror identification" / cancelled-and-revived: the sync
	// layer must distinguish 409 from other errors so it can fetch the
	// existing event and revive it. The wrapper exposes 409 via the
	// ErrAPIConflict sentinel.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				Stderr: `{"error":{"code":409,"message":"duplicate","errors":[{"reason":"duplicate"}]}}`,
				Exit:   1,
			},
		},
	}
	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().EventsInsert(context.Background(), "alice@example.com",
			&gws.Event{ID: "cs2dup", Summary: "x"})
	})

	if !errors.Is(gotErr, gws.ErrAPIConflict) {
		t.Fatalf("expected errors.Is(err, ErrAPIConflict) to be true; got %v", gotErr)
	}
	var typed *gws.Error
	if !errors.As(gotErr, &typed) {
		t.Fatalf("error not *gws.Error: %v", gotErr)
	}
	if typed.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", typed.HTTPStatus)
	}
	if typed.Op != "events.insert" {
		t.Errorf("Op = %q, want events.insert", typed.Op)
	}
}

func TestEventsInsert_PartialPayloadOmitsZeroFields(t *testing.T) {
	// SPEC's mirror payload uses Reminders.UseDefault=false explicitly. The
	// Event type's omitempty tags must NOT drop the Reminders sub-struct
	// just because UseDefault is false. Verify by sending an Event with a
	// non-nil Reminders pointer.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"id":"e1"}`, Exit: 0},
		},
	}

	body := &gws.Event{
		ID:        "e1",
		Summary:   "x",
		Reminders: &gws.Reminders{UseDefault: false},
	}

	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		_, _ = gws.New().EventsInsert(context.Background(), "cal@example.com", body)
	})

	gotBody := calls[0].Body.(map[string]any)
	rem, ok := gotBody["reminders"].(map[string]any)
	if !ok {
		t.Fatalf("reminders missing or wrong type: %#v", gotBody["reminders"])
	}
	// Key-presence check: the SPEC mirror payload requires useDefault to
	// be PRESENT and false. A bare type-assertion on the bool would also
	// pass if the key were absent (zero-value bool is false too), so check
	// presence explicitly first.
	raw, present := rem["useDefault"]
	if !present {
		t.Fatalf("reminders.useDefault missing from payload; want present with value false")
	}
	v, ok := raw.(bool)
	if !ok {
		t.Fatalf("reminders.useDefault wrong type: %#v", raw)
	}
	if v != false {
		t.Errorf("reminders.useDefault = %v, want false", v)
	}
}

func TestEventsPatch_NilFieldsProduceEmptyBody(t *testing.T) {
	// PatchEvent semantics: a nil pointer field means "leave alone" and
	// must not appear on the wire. Verify that an entirely-nil PatchEvent
	// marshals to {} so omitempty does its job.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"id":"e1"}`, Exit: 0},
		},
	}

	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		_, _ = gws.New().EventsPatch(context.Background(), "cal", "e1", &gws.PatchEvent{})
	})

	gotBody, ok := calls[0].Body.(map[string]any)
	if !ok {
		t.Fatalf("body did not parse as object: %#v", calls[0].Body)
	}
	if len(gotBody) != 0 {
		t.Errorf("nil-fields patch produced non-empty body %#v; want {} (nil pointers are omitted)", gotBody)
	}
}

func TestEventsPatch_NonNilEmptyStringClearsField(t *testing.T) {
	// PatchEvent's primary motivation: a non-nil pointer to "" must reach
	// the wire as "summary":"" so Calendar API clears the existing value.
	// A bare *gws.Event with Summary="" would silently drop the key via
	// omitempty; the patch type's pointer encoding fixes that.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"id":"e1"}`, Exit: 0},
		},
	}

	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		_, _ = gws.New().EventsPatch(context.Background(), "cal", "e1",
			&gws.PatchEvent{Summary: gws.PatchStrClear()})
	})

	gotBody, ok := calls[0].Body.(map[string]any)
	if !ok {
		t.Fatalf("body did not parse as object: %#v", calls[0].Body)
	}
	raw, present := gotBody["summary"]
	if !present {
		t.Fatalf("clear-summary patch missing summary key; want present with empty string. body=%#v", gotBody)
	}
	got, ok := raw.(string)
	if !ok {
		t.Fatalf("summary wrong type: %#v", raw)
	}
	if got != "" {
		t.Errorf("summary = %q, want empty string", got)
	}
}

func TestEventsPatch_ClearRecurrenceProducesEmptyArray(t *testing.T) {
	// PatchRecurrenceClear must produce "recurrence":[] - the explicit
	// clear-form Calendar API requires to remove a recurrence rule from
	// an event. nil-Recurrence (the omitempty drop) would leave the
	// existing rule in place.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"id":"e1"}`, Exit: 0},
		},
	}

	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		_, _ = gws.New().EventsPatch(context.Background(), "cal", "e1",
			&gws.PatchEvent{Recurrence: gws.PatchRecurrenceClear()})
	})

	gotBody, ok := calls[0].Body.(map[string]any)
	if !ok {
		t.Fatalf("body did not parse as object: %#v", calls[0].Body)
	}
	raw, present := gotBody["recurrence"]
	if !present {
		t.Fatalf("clear-recurrence patch missing recurrence key. body=%#v", gotBody)
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("recurrence wrong type: %#v", raw)
	}
	if len(arr) != 0 {
		t.Errorf("recurrence = %v, want empty array", arr)
	}
}

func TestEventsInsert_MalformedStdoutReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `not-json`, Exit: 0},
		},
	}
	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().EventsInsert(context.Background(), "x", &gws.Event{ID: "e1"})
	})
	if gotErr == nil {
		t.Fatal("expected parse error on malformed stdout, got nil")
	}
}

func TestEventsPatch_MalformedStdoutReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `not-json`, Exit: 0},
		},
	}
	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().EventsPatch(context.Background(), "x", "e1",
			&gws.PatchEvent{Summary: gws.PatchStr("x")})
	})
	if gotErr == nil {
		t.Fatal("expected parse error on malformed stdout, got nil")
	}
}

func TestEventsPatch_HappyPath(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"id":"cs2abc","summary":"Lunch (updated)","updated":"2026-04-30T13:00:00.000Z"}`, Exit: 0},
		},
	}

	patch := &gws.PatchEvent{Summary: gws.PatchStr("Lunch (updated)")}

	var (
		got    *gws.Event
		gotErr error
	)
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		got, gotErr = gws.New().EventsPatch(context.Background(), "alice@example.com", "cs2abc", patch)
	})

	if gotErr != nil {
		t.Fatalf("EventsPatch returned error: %v", gotErr)
	}
	if got.Summary != "Lunch (updated)" {
		t.Errorf("response.Summary = %q, want updated", got.Summary)
	}

	args := calls[0].Args
	if !argvHasPrefix(args, "calendar", "events", "patch") {
		t.Errorf("args prefix = %v, want [calendar events patch]", args)
	}
	if !contains(args, "--json") {
		t.Errorf("argv missing --json: %v", args)
	}
	if cal, _ := calls[0].Params["calendarId"].(string); cal != "alice@example.com" {
		t.Errorf("--params calendarId = %v", calls[0].Params["calendarId"])
	}
	if ev, _ := calls[0].Params["eventId"].(string); ev != "cs2abc" {
		t.Errorf("--params eventId = %v", calls[0].Params["eventId"])
	}

	gotBody := calls[0].Body.(map[string]any)
	want := map[string]any{"summary": "Lunch (updated)"}
	// Patch sends only the fields the caller set; omitempty drops nil
	// pointer fields.
	if !reflect.DeepEqual(gotBody, want) {
		t.Errorf("--json body = %#v, want %#v (only the changed fields)", gotBody, want)
	}
}

func TestEventsPatch_NilBodyRejected(t *testing.T) {
	var gotErr error
	testhelpers.WithFakeGWS(t, testhelpers.Scenario{}, func() {
		_, gotErr = gws.New().EventsPatch(context.Background(), "x", "y", nil)
	})
	if gotErr == nil {
		t.Fatal("expected error when body is nil")
	}
}

func TestEventsDelete_HappyPath(t *testing.T) {
	// 204 No Content -> empty stdout, exit 0.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: "", Exit: 0},
		},
	}

	var gotErr error
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		gotErr = gws.New().EventsDelete(context.Background(), "alice@example.com", "cs2abc")
	})

	if gotErr != nil {
		t.Fatalf("EventsDelete returned error: %v", gotErr)
	}
	args := calls[0].Args
	if !argvHasPrefix(args, "calendar", "events", "delete") {
		t.Errorf("args prefix = %v, want [calendar events delete]", args)
	}
	if contains(args, "--json") {
		t.Errorf("delete must not send --json (no body)")
	}
	if ev, _ := calls[0].Params["eventId"].(string); ev != "cs2abc" {
		t.Errorf("--params eventId = %v", calls[0].Params["eventId"])
	}
}

func TestEventsDelete_NonZeroExitReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stderr: `{"error":{"code":404}}`, Exit: 1},
		},
	}
	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		gotErr = gws.New().EventsDelete(context.Background(), "x", "missing")
	})
	if gotErr == nil {
		t.Fatal("expected error on non-zero exit")
	}
}
