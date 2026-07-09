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

// TestDaemon_FeedsRunOnTickNotFullSync pins the B26 ordering: the feed importer
// runs before each Tick's mirror reconcile, but NOT on the startup FullSync.
// FullSync resets source syncTokens from a full list that can leap past a
// just-written feed event; Tick's incremental delta is self-consistent, so it's
// the safe place to import. Both the fake feed runner and the stubAPI append to
// one ordered call log: we assert the startup FullSync ran with NO preceding
// feed phase, and that the tick's feed run precedes its incremental list.
func TestDaemon_FeedsRunOnTickNotFullSync(t *testing.T) {
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

	// Startup FullSync: source-list + 2 inventory rebuilds. No feed phase.
	waitFor(t, "startup FullSync done + After registered", 2*time.Second, func() bool {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		return api.listCalls.Load() >= 3 && len(clock.pending) > 0
	})

	// Snapshot the startup log BEFORE any tick: it must contain no "feeds".
	api.mu.Lock()
	startupCalls := append([]string(nil), api.calls...)
	api.mu.Unlock()
	if n := count(startupCalls, "feeds"); n != 0 {
		t.Fatalf("feed phase ran %d times during the startup FullSync, want 0 (B26); log: %v", n, startupCalls)
	}
	if len(startupCalls) == 0 || startupCalls[0] == "feeds" {
		t.Fatalf("first call = %v, want a FullSync list call, not a feed phase; log: %v", firstOrEmpty(startupCalls), startupCalls)
	}

	// Drive one Tick.
	clock.advance(120 * time.Second)
	waitFor(t, "tick fired", 2*time.Second, func() bool {
		return api.tickListCalls.Load() >= 1
	})

	api.mu.Lock()
	calls := append([]string(nil), api.calls...)
	api.mu.Unlock()

	// Exactly one feed run so far: the tick's (the startup FullSync ran none).
	if n := count(calls, "feeds"); n != 1 {
		t.Errorf("feed phase ran %d times, want exactly 1 (the Tick only); log: %v", n, calls)
	}

	// The tick's feed run precedes the tick's incremental list call.
	feedIdx, incrIdx := -1, -1
	for i, c := range calls {
		if c == "feeds" && feedIdx == -1 {
			feedIdx = i
		}
		if len(c) >= 10 && c[:10] == "list-incr:" && incrIdx == -1 {
			incrIdx = i
		}
	}
	if incrIdx == -1 {
		t.Fatalf("no incremental list call recorded; tick did not run as expected; log: %v", calls)
	}
	if feedIdx == -1 || feedIdx > incrIdx {
		t.Errorf("feed phase did not precede the tick's incremental list (feedIdx=%d incrIdx=%d); log: %v", feedIdx, incrIdx, calls)
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
