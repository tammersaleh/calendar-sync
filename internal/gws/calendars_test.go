package gws_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/testhelpers"
)

func TestCalendarsInsert_HappyPath(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"id":"c_abc@group.calendar.google.com","summary":"calendar-sync-e2e-source","description":"fixture","timeZone":"UTC"}`, Exit: 0},
		},
	}

	var (
		got    *gws.Calendar
		gotErr error
	)
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		client := gws.New()
		got, gotErr = client.CalendarsInsert(context.Background(), &gws.Calendar{
			Summary:     "calendar-sync-e2e-source",
			Description: "fixture",
			TimeZone:    "UTC",
		})
	})
	if gotErr != nil {
		t.Fatalf("CalendarsInsert returned error: %v", gotErr)
	}
	want := &gws.Calendar{
		ID:          "c_abc@group.calendar.google.com",
		Summary:     "calendar-sync-e2e-source",
		Description: "fixture",
		TimeZone:    "UTC",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CalendarsInsert = %#v, want %#v", got, want)
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 gws invocation, got %d", len(calls))
	}
	wantPrefix := []string{"calendar", "calendars", "insert"}
	for i, w := range wantPrefix {
		if calls[0].Args[i] != w {
			t.Errorf("call.Args[%d] = %q, want %q", i, calls[0].Args[i], w)
		}
	}
	if !hasFlag(calls[0].Args, "--json") {
		t.Errorf("argv missing --json: %v", calls[0].Args)
	}
}

func TestCalendarsInsert_NilBodyReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{Calls: []testhelpers.ScenarioCall{}}

	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().CalendarsInsert(context.Background(), nil)
	})
	if gotErr == nil {
		t.Fatal("expected error on nil body, got nil")
	}
}

func TestCalendarsInsert_NonZeroExitReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: "", Stderr: `{"error":"forbidden"}`, Exit: 1},
		},
	}
	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().CalendarsInsert(context.Background(), &gws.Calendar{Summary: "x"})
	})
	if gotErr == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
}

func TestCalendarsDelete_HappyPath(t *testing.T) {
	// Calendar API returns 204 No Content; gws produces empty stdout
	// with exit 0. The wrapper must not treat the empty stdout as a
	// parse error - delete returns nil error on success without
	// inspecting stdout.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: "", Exit: 0},
		},
	}
	var gotErr error
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		gotErr = gws.New().CalendarsDelete(context.Background(), "c_abc@group.calendar.google.com")
	})
	if gotErr != nil {
		t.Fatalf("CalendarsDelete returned error: %v", gotErr)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 gws invocation, got %d", len(calls))
	}
	wantPrefix := []string{"calendar", "calendars", "delete"}
	for i, w := range wantPrefix {
		if calls[0].Args[i] != w {
			t.Errorf("call.Args[%d] = %q, want %q", i, calls[0].Args[i], w)
		}
	}
	if calID, _ := calls[0].Params["calendarId"].(string); calID != "c_abc@group.calendar.google.com" {
		t.Errorf("--params calendarId = %v, want c_abc@group.calendar.google.com", calls[0].Params["calendarId"])
	}
}

func TestCalendarsDelete_NonZeroExitReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stderr: `{"error":"not found"}`, Exit: 1},
		},
	}
	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		gotErr = gws.New().CalendarsDelete(context.Background(), "c_abc@group.calendar.google.com")
	})
	if gotErr == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
}
