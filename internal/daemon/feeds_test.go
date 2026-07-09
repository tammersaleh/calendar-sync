package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/feedimport"
)

// fakeFeedRunner records the moment RunOnce is called into the shared stubAPI
// call log, so the daemon-phase ordering (feeds BEFORE the mirror reconcile)
// is observable in one ordered sequence alongside the reconciler's list calls.
type fakeFeedRunner struct {
	api *stubAPI
}

func (f *fakeFeedRunner) RunOnce(_ context.Context) []feedimport.FeedResult {
	f.api.mu.Lock()
	f.api.calls = append(f.api.calls, "feeds")
	f.api.mu.Unlock()
	return nil
}

// TestDaemon_FeedsRunBeforeReconcile pins the CRITICAL ordering: the feed
// importer runs before the mirror reconcile in BOTH the startup FullSync and
// each periodic Tick. Both the fake feed runner and the stubAPI append to the
// same ordered call log; asserting "feeds" precedes the reconciler's list
// calls in each pass proves a feed change can reach the target calendar and
// propagate downstream within the same pass.
func TestDaemon_FeedsRunBeforeReconcile(t *testing.T) {
	api := newStubAPI()
	api.listTokens["src"] = "tok-1"
	clock := newFakeClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	d, _ := makeDaemon(t, api, clock)
	d.Feeds = &fakeFeedRunner{api: api}

	cancel, done := runDaemonAsync(d)
	defer func() {
		cancel()
		<-done
	}()

	// Startup FullSync: source-list + 2 inventory rebuilds.
	waitFor(t, "startup FullSync done + After registered", 2*time.Second, func() bool {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		return api.listCalls.Load() >= 3 && len(clock.pending) > 0
	})

	// Drive one Tick.
	clock.advance(120 * time.Second)
	waitFor(t, "tick fired", 2*time.Second, func() bool {
		return api.tickListCalls.Load() >= 1
	})

	api.mu.Lock()
	calls := append([]string(nil), api.calls...)
	api.mu.Unlock()

	// The very first recorded call must be a feed run (before the startup
	// FullSync touched the API at all).
	if len(calls) == 0 || calls[0] != "feeds" {
		t.Fatalf("first call = %v, want the feed phase to run first; full log: %v", firstOrEmpty(calls), calls)
	}

	// Both passes ran the feed phase: at least two "feeds" markers.
	if n := count(calls, "feeds"); n < 2 {
		t.Errorf("feed phase ran %d times, want >= 2 (startup FullSync + one Tick); log: %v", n, calls)
	}

	// The tick's feed run precedes the tick's incremental list call.
	feedsBeforeIncr := false
	sawIncr := false
	for _, c := range calls {
		switch {
		case c == "feeds" && !sawIncr:
			feedsBeforeIncr = true
		case len(c) >= 10 && c[:10] == "list-incr:":
			sawIncr = true
			if !feedsBeforeIncr {
				t.Errorf("incremental list ran before any feed phase; log: %v", calls)
			}
		}
	}
	if !sawIncr {
		t.Errorf("no incremental list call recorded; tick did not run as expected; log: %v", calls)
	}
}

func count(s []string, want string) int {
	n := 0
	for _, v := range s {
		if v == want {
			n++
		}
	}
	return n
}

func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return "<empty>"
	}
	return s[0]
}
