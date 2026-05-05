package gws_test

import (
	"context"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/testhelpers"
)

// TestEventsList_RetriesOnRateLimitedThenSucceeds proves the retry layer
// is wired end-to-end at the events.list call site: two consecutive 429s
// classify to CodeRateLimited and trigger retry; the third attempt
// succeeds and the wrapper returns the parsed events.
//
// SPEC §"Retry policy" (line 1393): 5-attempt ceiling, exponential
// backoff. We use NewWithFastRetryForTest to compress the schedule to
// nanoseconds so the test does not actually sleep through real seconds.
func TestEventsList_RetriesOnRateLimitedThenSucceeds(t *testing.T) {
	rateLimitEnvelope := `{"error":{"code":429,"message":"slow down"}}`
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			// Two retryable failures.
			{Stdout: rateLimitEnvelope, Exit: 1},
			{Stdout: rateLimitEnvelope, Exit: 1},
			// Third attempt succeeds.
			{
				Stdout: `{"items":[{"id":"e1","summary":"A"}],"nextSyncToken":"TOK"}` + "\n",
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
		c := gws.NewWithFastRetryForTest()
		gotEvents, gotToken, gotErr = c.EventsList(context.Background(), gws.EventsListParams{
			CalendarID: "alice@example.com",
		})
	})

	if gotErr != nil {
		t.Fatalf("EventsList returned err = %v, want nil after retry recovery", gotErr)
	}
	if len(gotEvents) != 1 || gotEvents[0].ID != "e1" {
		t.Errorf("events = %+v, want one event with id=e1", gotEvents)
	}
	if gotToken != "TOK" {
		t.Errorf("nextSyncToken = %q, want TOK", gotToken)
	}
	if len(calls) != 3 {
		t.Errorf("gws call count = %d, want 3 (two retries + success)", len(calls))
	}
}

// TestEventsList_NonRetryableErrorReturnsImmediately is the negative
// counterpart: a 404 must NOT be retried. The scenario only configures
// one response; if the wrapper retried we'd see the harness exit with
// "scenario exhausted" rather than the typed not-found error.
func TestEventsList_NonRetryableErrorReturnsImmediately(t *testing.T) {
	scenario := testhelpers.Scenario{
		Calls: []testhelpers.ScenarioCall{
			{
				Stdout: `{"error":{"code":404,"message":"not found"}}`,
				Exit:   1,
			},
		},
	}

	var gotErr error
	calls := testhelpers.WithFakeGWS(t, scenario, func() {
		c := gws.NewWithFastRetryForTest()
		_, _, gotErr = c.EventsList(context.Background(), gws.EventsListParams{
			CalendarID: "alice@example.com",
		})
	})

	if gotErr == nil {
		t.Fatal("EventsList returned nil err, want a typed not-found error")
	}
	if len(calls) != 1 {
		t.Errorf("gws call count = %d, want 1 (no retry on 404)", len(calls))
	}
}
