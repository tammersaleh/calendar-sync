package gws_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/testhelpers"
)

func TestEventsGet_HappyPath(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				Stdout: `{
					"id":"abc123",
					"status":"confirmed",
					"summary":"Lunch",
					"start":{"dateTime":"2026-04-30T12:00:00Z","timeZone":"UTC"},
					"end":{"dateTime":"2026-04-30T13:00:00Z","timeZone":"UTC"},
					"updated":"2026-04-29T23:00:00.000Z",
					"htmlLink":"https://www.google.com/calendar/event?eid=ABC",
					"transparency":"opaque",
					"visibility":"private"
				}`,
				Exit: 0,
			},
		},
	}

	var (
		got    *gws.Event
		gotErr error
	)
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		got, gotErr = gws.New().EventsGet(context.Background(), "alice@example.com", "abc123")
	})

	if gotErr != nil {
		t.Fatalf("EventsGet returned error: %v", gotErr)
	}
	if got.ID != "abc123" {
		t.Errorf("Event.ID = %q, want abc123", got.ID)
	}
	if got.Summary != "Lunch" {
		t.Errorf("Event.Summary = %q, want Lunch", got.Summary)
	}
	if got.Start == nil || got.Start.DateTime != "2026-04-30T12:00:00Z" {
		t.Errorf("Event.Start = %#v, want DateTime=2026-04-30T12:00:00Z", got.Start)
	}
	if got.Status != "confirmed" {
		t.Errorf("Event.Status = %q, want confirmed", got.Status)
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 gws call, got %d", len(calls))
	}
	args := calls[0].Args
	if !argvHasPrefix(args, "calendar", "events", "get") {
		t.Errorf("args = %v, want prefix [calendar events get]", args)
	}
	if cal, _ := calls[0].Params["calendarId"].(string); cal != "alice@example.com" {
		t.Errorf("calendarId = %v, want alice@example.com", calls[0].Params["calendarId"])
	}
	if ev, _ := calls[0].Params["eventId"].(string); ev != "abc123" {
		t.Errorf("eventId = %v, want abc123", calls[0].Params["eventId"])
	}
}

func TestEventsGet_NonZeroExitReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stderr: `{"error":"not found"}`, Exit: 1},
		},
	}

	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, gotErr = gws.New().EventsGet(context.Background(), "x@example.com", "missing")
	})

	if gotErr == nil {
		t.Fatal("expected error on non-zero exit")
	}
}

func TestEventsList_SinglePageHappyPath(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				Stdout: `{"items":[{"id":"e1","summary":"A"},{"id":"e2","summary":"B"}],"nextSyncToken":"TOKEN_END"}` + "\n",
				Exit:   0,
			},
		},
	}

	var (
		gotEvents []gws.Event
		gotToken  string
		gotErr    error
	)
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		gotEvents, gotToken, gotErr = gws.New().EventsList(context.Background(), gws.EventsListParams{
			CalendarID:   "alice@example.com",
			TimeMin:      "2026-04-30T00:00:00Z",
			TimeMax:      "2027-04-30T00:00:00Z",
			SingleEvents: false,
			ShowDeleted:  true,
			EventTypes:   []string{"default", "outOfOffice", "focusTime"},
			MaxResults:   250,
		})
	})

	if gotErr != nil {
		t.Fatalf("EventsList returned error: %v", gotErr)
	}
	if len(gotEvents) != 2 {
		t.Fatalf("len(events) = %d, want 2; got %#v", len(gotEvents), gotEvents)
	}
	if gotEvents[0].ID != "e1" || gotEvents[1].ID != "e2" {
		t.Errorf("event IDs = [%q,%q], want [e1,e2]", gotEvents[0].ID, gotEvents[1].ID)
	}
	if gotToken != "TOKEN_END" {
		t.Errorf("nextSyncToken = %q, want TOKEN_END", gotToken)
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 gws call, got %d", len(calls))
	}
	args := calls[0].Args
	if !argvHasPrefix(args, "calendar", "events", "list") {
		t.Errorf("args prefix = %v", args)
	}
	if !contains(args, "--page-all") {
		t.Errorf("argv missing --page-all: %v", args)
	}

	// Param shape - SPEC.md required keys for the startup full-sync call.
	// Note: singleEvents is omitempty + the caller passes false, so the key
	// is omitted (Calendar API's default is false, matching SPEC's intent).
	p := calls[0].Params
	for _, k := range []string{"calendarId", "timeMin", "timeMax", "showDeleted", "eventTypes", "maxResults"} {
		if _, ok := p[k]; !ok {
			t.Errorf("--params missing %q: %#v", k, p)
		}
	}
	if _, present := p["singleEvents"]; present {
		t.Errorf("singleEvents should be omitted when caller passes false; got %#v", p["singleEvents"])
	}
	if v, ok := p["showDeleted"].(bool); !ok || v != true {
		t.Errorf("showDeleted = %#v, want bool true", p["showDeleted"])
	}
	if eventTypes, ok := p["eventTypes"].([]any); !ok || len(eventTypes) != 3 {
		t.Errorf("eventTypes = %#v, want 3 strings", p["eventTypes"])
	}
	if maxResults, ok := p["maxResults"].(float64); !ok || maxResults != 250 {
		t.Errorf("maxResults = %#v, want 250", p["maxResults"])
	}
}

func TestEventsList_MultiPageMergesItemsAndUsesLastSyncToken(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				// gws --page-all emits one JSON object per line. The last
				// page carries nextSyncToken; intermediate pages may carry
				// nextPageToken.
				Stdout: `{"items":[{"id":"e1"}],"nextPageToken":"PG2"}` + "\n" +
					`{"items":[{"id":"e2"},{"id":"e3"}],"nextPageToken":"PG3"}` + "\n" +
					`{"items":[{"id":"e4"}],"nextSyncToken":"FINAL_TOKEN"}` + "\n",
				Exit: 0,
			},
		},
	}

	var (
		events []gws.Event
		token  string
		err    error
	)
	testhelpers.WithFakeGWS(t, scenario, func() {
		events, token, err = gws.New().EventsList(context.Background(), gws.EventsListParams{
			CalendarID:   "x",
			SingleEvents: false,
			ShowDeleted:  true,
		})
	})
	if err != nil {
		t.Fatalf("EventsList: %v", err)
	}
	if got := eventIDs(events); !reflect.DeepEqual(got, []string{"e1", "e2", "e3", "e4"}) {
		t.Errorf("merged event ids = %v, want [e1 e2 e3 e4]", got)
	}
	if token != "FINAL_TOKEN" {
		t.Errorf("nextSyncToken = %q, want FINAL_TOKEN (from last page)", token)
	}
}

func TestEventsList_EmptyDeltaReturnsEmptySliceAndToken(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"items":[],"nextSyncToken":"EMPTY_DELTA_TOKEN"}` + "\n", Exit: 0},
		},
	}

	var (
		events []gws.Event
		token  string
	)
	testhelpers.WithFakeGWS(t, scenario, func() {
		events, token, _ = gws.New().EventsList(context.Background(), gws.EventsListParams{
			CalendarID:   "x",
			SyncToken:    "PRIOR_TOKEN",
			SingleEvents: false,
			ShowDeleted:  true,
		})
	})

	if len(events) != 0 {
		t.Errorf("expected empty events on empty delta, got %d", len(events))
	}
	if token != "EMPTY_DELTA_TOKEN" {
		t.Errorf("token = %q, want EMPTY_DELTA_TOKEN", token)
	}
}

func TestEventsList_SyncTokenPassedThroughParams(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"items":[],"nextSyncToken":"NEW_TOKEN"}` + "\n", Exit: 0},
		},
	}

	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		_, _, _ = gws.New().EventsList(context.Background(), gws.EventsListParams{
			CalendarID:   "x",
			SyncToken:    "OLD_TOKEN",
			SingleEvents: false,
			ShowDeleted:  true,
		})
	})

	if got, _ := calls[0].Params["syncToken"].(string); got != "OLD_TOKEN" {
		t.Errorf("syncToken in params = %v, want OLD_TOKEN", calls[0].Params["syncToken"])
	}
	// SPEC.md: timeMin/timeMax are rejected alongside syncToken; the
	// wrapper must not silently inject them when the caller hasn't set them.
	if _, ok := calls[0].Params["timeMin"]; ok {
		t.Errorf("timeMin should not be in params when caller didn't set it")
	}
	// singleEvents is similarly absent from SPEC's incremental wire shape:
	// omitempty + caller's false zero-value drops the key.
	if _, ok := calls[0].Params["singleEvents"]; ok {
		t.Errorf("singleEvents should be omitted on incremental calls; got %#v", calls[0].Params["singleEvents"])
	}
}

func TestEventsList_PageWithSyncTokenAndNoItemsField(t *testing.T) {
	// Calendar API can emit a syncToken-advancing page that omits "items"
	// entirely (vs an empty array). The merge loop must treat the missing
	// key the same as an empty list. This is a real shape we'll see in
	// production.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"nextSyncToken":"TOKEN_NO_ITEMS"}` + "\n", Exit: 0},
		},
	}

	var (
		events []gws.Event
		token  string
		err    error
	)
	testhelpers.WithFakeGWS(t, scenario, func() {
		events, token, err = gws.New().EventsList(context.Background(), gws.EventsListParams{
			CalendarID:  "x",
			ShowDeleted: true,
		})
	})
	if err != nil {
		t.Fatalf("EventsList: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(events))
	}
	if token != "TOKEN_NO_ITEMS" {
		t.Errorf("nextSyncToken = %q, want TOKEN_NO_ITEMS", token)
	}
}

func TestEventsList_NonZeroExitReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stderr: `{"error":"500"}`, Exit: 1},
		},
	}

	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, _, gotErr = gws.New().EventsList(context.Background(), gws.EventsListParams{
			CalendarID:   "x",
			SingleEvents: false,
			ShowDeleted:  true,
		})
	})
	if gotErr == nil {
		t.Fatal("expected error on non-zero exit")
	}
}

func TestEventsList_UnparseableLineReturnsError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			// First page valid, second is garbage. Don't silently skip.
			{Stdout: `{"items":[{"id":"e1"}]}` + "\n" + "not-json\n", Exit: 0},
		},
	}

	var gotErr error
	testhelpers.WithFakeGWS(t, scenario, func() {
		_, _, gotErr = gws.New().EventsList(context.Background(), gws.EventsListParams{
			CalendarID:   "x",
			SingleEvents: false,
			ShowDeleted:  true,
		})
	})
	if gotErr == nil {
		t.Fatal("expected parse error on garbage NDJSON line")
	}
	if !strings.Contains(gotErr.Error(), "parse") && !strings.Contains(gotErr.Error(), "page") {
		t.Errorf("error message = %v; want it to mention parse or page", gotErr)
	}
}

func TestEventsInstances_HappyPath(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				Stdout: `{"items":[{"id":"i1","recurringEventId":"r1"},{"id":"i2","recurringEventId":"r1"}]}` + "\n",
				Exit:   0,
			},
		},
	}

	var (
		instances []gws.Event
		gotErr    error
	)
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		instances, gotErr = gws.New().EventsInstances(context.Background(), gws.EventsInstancesParams{
			CalendarID:  "alice@example.com",
			EventID:     "r1",
			TimeMin:     "2026-04-30T00:00:00Z",
			TimeMax:     "2027-04-30T00:00:00Z",
			MaxResults:  1,
			ShowDeleted: false,
		})
	})

	if gotErr != nil {
		t.Fatalf("EventsInstances returned error: %v", gotErr)
	}
	if len(instances) != 2 {
		t.Errorf("len(instances) = %d, want 2", len(instances))
	}
	if !argvHasPrefix(calls[0].Args, "calendar", "events", "instances") {
		t.Errorf("args prefix = %v", calls[0].Args)
	}
	if !contains(calls[0].Args, "--page-all") {
		t.Errorf("argv missing --page-all: %v", calls[0].Args)
	}
	for _, k := range []string{"calendarId", "eventId", "timeMin", "timeMax", "maxResults"} {
		if _, ok := calls[0].Params[k]; !ok {
			t.Errorf("--params missing %q: %#v", k, calls[0].Params)
		}
	}
}

func TestEventsInstances_MultiPageMergesItems(t *testing.T) {
	// EventsInstances also goes through --page-all; verify the merge path
	// works for instances too, not just events.list.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				Stdout: `{"items":[{"id":"i1"},{"id":"i2"}],"nextPageToken":"PG2"}` + "\n" +
					`{"items":[{"id":"i3"}]}` + "\n",
				Exit: 0,
			},
		},
	}

	var (
		instances []gws.Event
		err       error
	)
	testhelpers.WithFakeGWS(t, scenario, func() {
		instances, err = gws.New().EventsInstances(context.Background(), gws.EventsInstancesParams{
			CalendarID: "x",
			EventID:    "r1",
			TimeMin:    "2026-04-30T00:00:00Z",
			TimeMax:    "2027-04-30T00:00:00Z",
		})
	})
	if err != nil {
		t.Fatalf("EventsInstances: %v", err)
	}
	if got := eventIDs(instances); !reflect.DeepEqual(got, []string{"i1", "i2", "i3"}) {
		t.Errorf("instance IDs = %v, want [i1 i2 i3]", got)
	}
}

func TestEventsInstances_EmptyResultIsNotAnError(t *testing.T) {
	// The horizon-eligibility check is the canonical "0 instances" use:
	// a recurring source whose only instances are past or beyond horizon.
	// The wrapper must return an empty slice, not an error.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"items":[]}` + "\n", Exit: 0},
		},
	}

	var (
		instances []gws.Event
		gotErr    error
	)
	testhelpers.WithFakeGWS(t, scenario, func() {
		instances, gotErr = gws.New().EventsInstances(context.Background(), gws.EventsInstancesParams{
			CalendarID:  "x",
			EventID:     "r1",
			TimeMin:     "2026-04-30T00:00:00Z",
			TimeMax:     "2027-04-30T00:00:00Z",
			MaxResults:  1,
			ShowDeleted: false,
		})
	})
	if gotErr != nil {
		t.Fatalf("EventsInstances on empty result returned error: %v", gotErr)
	}
	if len(instances) != 0 {
		t.Errorf("len(instances) = %d, want 0", len(instances))
	}
}

func argvHasPrefix(args []string, want ...string) bool {
	if len(args) < len(want) {
		return false
	}
	for i, w := range want {
		if args[i] != w {
			return false
		}
	}
	return true
}

func contains(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}

func eventIDs(events []gws.Event) []string {
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	return ids
}
