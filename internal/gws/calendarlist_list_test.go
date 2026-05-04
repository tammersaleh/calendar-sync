package gws_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/testhelpers"
)

func TestCalendarListList_HappyPath_SinglePage(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"items":[{"id":"a@example.com","summary":"A","accessRole":"owner"},{"id":"b@example.com","summary":"B","accessRole":"writer"}]}`, Exit: 0},
		},
	}

	var (
		got    []gws.CalendarListEntry
		gotErr error
	)
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		got, gotErr = gws.New().CalendarListList(context.Background())
	})
	if gotErr != nil {
		t.Fatalf("CalendarListList returned error: %v", gotErr)
	}
	want := []gws.CalendarListEntry{
		{ID: "a@example.com", Summary: "A", AccessRole: "owner"},
		{ID: "b@example.com", Summary: "B", AccessRole: "writer"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CalendarListList = %#v, want %#v", got, want)
	}

	wantPrefix := []string{"calendar", "calendarList", "list"}
	for i, w := range wantPrefix {
		if calls[0].Args[i] != w {
			t.Errorf("call.Args[%d] = %q, want %q", i, calls[0].Args[i], w)
		}
	}
	if !hasFlag(calls[0].Args, "--page-all") {
		t.Errorf("argv missing --page-all (caller expects auto-pagination): %v", calls[0].Args)
	}
}

func TestCalendarListList_HappyPath_MultiPage(t *testing.T) {
	// gws --page-all emits NDJSON: one JSON object per line, each one a
	// page. The wrapper merges items across pages.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				Stdout: `{"items":[{"id":"a@example.com","accessRole":"owner"}],"nextPageToken":"p1"}` + "\n" +
					`{"items":[{"id":"b@example.com","accessRole":"writer"}]}` + "\n",
				Exit: 0,
			},
		},
	}

	var (
		got    []gws.CalendarListEntry
		gotErr error
	)
	testhelpers.WithFakeGWS(t, scenario, func() {
		got, gotErr = gws.New().CalendarListList(context.Background())
	})
	if gotErr != nil {
		t.Fatalf("CalendarListList returned error: %v", gotErr)
	}
	if len(got) != 2 {
		t.Fatalf("merged items = %d, want 2", len(got))
	}
	if got[0].ID != "a@example.com" || got[1].ID != "b@example.com" {
		t.Errorf("merged ids = [%q, %q], want [a, b]", got[0].ID, got[1].ID)
	}
}

func TestCalendarListList_EmptyResultIsValid(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"items":[]}`, Exit: 0},
		},
	}

	var (
		got    []gws.CalendarListEntry
		gotErr error
	)
	testhelpers.WithFakeGWS(t, scenario, func() {
		got, gotErr = gws.New().CalendarListList(context.Background())
	})
	if gotErr != nil {
		t.Fatalf("CalendarListList returned error: %v", gotErr)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0 for empty calendar list", len(got))
	}
}

func TestCalendarListList_NonZeroExitReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stderr: `{"error":"unauthorized"}`, Exit: 2},
		},
	}
	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().CalendarListList(context.Background())
	})
	if gotErr == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
}
