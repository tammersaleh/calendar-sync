package gws_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/testhelpers"
)

// B28: `gws --page-all` stops at --page-limit, which defaults to 10. The
// wrapper never passed the flag, so any list exceeding 10 pages (2500
// events at maxResults=250) came back as a silent partial prefix with an
// empty nextSyncToken and no error. In production that killed B17's
// target-delta phase outright: seedTargetSyncTokens stored "" for both
// targets (3470 and 10244 events) and runTargetDeltaPhase skipped them on
// every tick for months.
//
// The wrapper's contract is now: a successful return means the collection
// is COMPLETE. Incomplete pagination is an error and yields no data.

func TestEventsList_ArgvCarriesExplicitPageLimit(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"items":[],"nextSyncToken":"T"}` + "\n", Exit: 0},
		},
	}

	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		_, _, _ = gws.New().EventsList(context.Background(), gws.EventsListParams{
			CalendarID: "x", ShowDeleted: true,
		})
	})

	if len(calls) != 1 {
		t.Fatalf("expected 1 gws call, got %d", len(calls))
	}
	assertPageLimit(t, calls[0].Args)
}

func TestCalendarListList_ArgvCarriesExplicitPageLimit(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"items":[]}` + "\n", Exit: 0},
		},
	}

	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		_, _ = gws.New().CalendarListList(context.Background())
	})

	if len(calls) != 1 {
		t.Fatalf("expected 1 gws call, got %d", len(calls))
	}
	assertPageLimit(t, calls[0].Args)
}

// assertPageLimit checks argv carries --page-all plus an explicit
// --page-limit matching the exported constant. Pinning the value (not just
// its presence) is deliberate: a regression that passes --page-limit 10
// would reintroduce B28 while still satisfying a presence-only check.
func assertPageLimit(t *testing.T, args []string) {
	t.Helper()
	if !contains(args, "--page-all") {
		t.Errorf("argv missing --page-all: %v", args)
	}
	idx := -1
	for i, a := range args {
		if a == "--page-limit" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("argv missing --page-limit: %v", args)
	}
	if idx+1 >= len(args) {
		t.Fatalf("--page-limit has no value: %v", args)
	}
	if got, want := args[idx+1], gws.MaxPagesPerList; got != want {
		t.Errorf("--page-limit = %q, want %q", got, want)
	}
}

func TestEventsList_TruncatedPaginationIsAnError(t *testing.T) {
	// Final emitted page still advertises nextPageToken: gws hit its page
	// cap and stopped. The collection is incomplete.
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				Stdout: `{"items":[{"id":"e1"}],"nextPageToken":"PG2"}` + "\n" +
					`{"items":[{"id":"e2"}],"nextPageToken":"PG3_SECRET"}` + "\n",
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
			CalendarID: "x", ShowDeleted: true,
		})
	})

	if err == nil {
		t.Fatal("expected an error when pagination is truncated")
	}
	if !errors.Is(err, gws.ErrIncompletePagination) {
		t.Errorf("errors.Is(err, ErrIncompletePagination) = false; err = %v", err)
	}
	// Fail closed: callers must never receive a partial prefix. Returning
	// events alongside the error invites a caller that logs-and-continues
	// to reconcile against half a calendar.
	if events != nil {
		t.Errorf("expected nil events on truncation, got %d", len(events))
	}
	if token != "" {
		t.Errorf("expected empty token on truncation, got %q", token)
	}
	// The page token is opaque cursor state, not diagnostics. Keep it out
	// of error strings that reach logs.
	if strings.Contains(err.Error(), "PG3_SECRET") {
		t.Errorf("error leaks the opaque page token: %v", err)
	}
}

func TestCalendarListList_TruncatedPaginationIsAnError(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{Stdout: `{"items":[{"id":"c1"}],"nextPageToken":"MORE"}` + "\n", Exit: 0},
		},
	}

	var (
		entries []gws.CalendarListEntry
		err     error
	)
	testhelpers.WithFakeGWS(t, scenario, func() {
		entries, err = gws.New().CalendarListList(context.Background())
	})

	if err == nil {
		t.Fatal("expected an error when pagination is truncated")
	}
	if !errors.Is(err, gws.ErrIncompletePagination) {
		t.Errorf("errors.Is(err, ErrIncompletePagination) = false; err = %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries on truncation, got %d", len(entries))
	}
}

// A complete run longer than gws's old 10-page default must succeed. This
// is the production shape: me@tammersaleh.com needed 41 pages.
func TestEventsList_ManyPagesCompleteSuccessfully(t *testing.T) {
	var sb strings.Builder
	const pages = 41
	for i := 1; i < pages; i++ {
		fmt.Fprintf(&sb, "{\"items\":[{\"id\":\"e%d\"}],\"nextPageToken\":\"PG\"}\n", i)
	}
	fmt.Fprintf(&sb, "{\"items\":[{\"id\":\"e%d\"}],\"nextSyncToken\":\"FINAL\"}\n", pages)

	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{{Stdout: sb.String(), Exit: 0}},
	}

	var (
		events []gws.Event
		token  string
		err    error
	)
	testhelpers.WithFakeGWS(t, scenario, func() {
		events, token, err = gws.New().EventsList(context.Background(), gws.EventsListParams{
			CalendarID: "x", ShowDeleted: true,
		})
	})

	if err != nil {
		t.Fatalf("EventsList: %v", err)
	}
	if len(events) != pages {
		t.Errorf("got %d events, want %d", len(events), pages)
	}
	if token != "FINAL" {
		t.Errorf("token = %q, want FINAL", token)
	}
}

