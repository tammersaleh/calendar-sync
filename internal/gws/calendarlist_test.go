package gws_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/testhelpers"
)

func TestCalendarListGet_HappyPath(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"id":"alice@example.com","summary":"Alice","accessRole":"writer"}`, Exit: 0},
		},
	}

	var (
		got    *gws.CalendarListEntry
		gotErr error
	)
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		client := gws.New()
		got, gotErr = client.CalendarListGet(context.Background(), "alice@example.com")
	})

	if gotErr != nil {
		t.Fatalf("CalendarListGet returned error: %v", gotErr)
	}
	want := &gws.CalendarListEntry{
		ID:         "alice@example.com",
		Summary:    "Alice",
		AccessRole: "writer",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CalendarListGet = %#v, want %#v", got, want)
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 gws invocation, got %d", len(calls))
	}
	call := calls[0]

	wantPrefix := []string{"calendar", "calendarList", "get"}
	if len(call.Args) < len(wantPrefix) {
		t.Fatalf("call.Args = %v, too short for %v", call.Args, wantPrefix)
	}
	for i, want := range wantPrefix {
		if call.Args[i] != want {
			t.Errorf("call.Args[%d] = %q, want %q", i, call.Args[i], want)
			break
		}
	}

	if !hasFlag(call.Args, "--params") {
		t.Errorf("argv missing --params: %v", call.Args)
	}
	if !hasFlag(call.Args, "--format") {
		t.Errorf("argv missing --format: %v", call.Args)
	}

	if calID, _ := call.Params["calendarId"].(string); calID != "alice@example.com" {
		t.Errorf("--params calendarId = %v, want alice@example.com", call.Params["calendarId"])
	}
}

func TestCalendarListGet_PrimaryAlias(t *testing.T) {
	// SPEC.md: at config-load time, "primary" is resolved to its canonical
	// ID via this very call. The wrapper just passes the literal "primary"
	// through to gws; gws resolves server-side and Google returns the
	// canonical id field on the entry.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"id":"alice.canonical@example.com","accessRole":"owner"}`, Exit: 0},
		},
	}

	var (
		got    *gws.CalendarListEntry
		gotErr error
	)
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		client := gws.New()
		got, gotErr = client.CalendarListGet(context.Background(), "primary")
	})
	if gotErr != nil {
		t.Fatalf("CalendarListGet returned error: %v", gotErr)
	}

	if got.ID != "alice.canonical@example.com" {
		t.Errorf("entry.ID = %q, want canonical id from response", got.ID)
	}
	if calID, _ := calls[0].Params["calendarId"].(string); calID != "primary" {
		t.Errorf("--params calendarId = %v, want literal 'primary'", calls[0].Params["calendarId"])
	}
}

func TestCalendarListGet_NonZeroExitReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				Stdout: "",
				Stderr: `{"error":"not found"}`,
				Exit:   1,
			},
		},
	}

	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().CalendarListGet(context.Background(), "missing@example.com")
	})

	if gotErr == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
}

func TestCalendarListGet_InvalidJSONReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `not json`, Exit: 0},
		},
	}

	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().CalendarListGet(context.Background(), "x@example.com")
	})

	if gotErr == nil {
		t.Fatal("expected error on unparseable stdout, got nil")
	}
}

func TestCalendarListGet_ContextCancellation(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"id":"x","accessRole":"reader"}`, Exit: 0},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before invoking

	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().CalendarListGet(ctx, "x@example.com")
	})

	if gotErr == nil {
		t.Fatal("expected error from canceled ctx, got nil")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("error does not wrap context.Canceled: %v", gotErr)
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
