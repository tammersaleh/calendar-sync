package daemon

import "time"

// scheduler holds the wall-clock-driven timing state SPEC §"Sleep and wake"
// (lines 1090-1101) describes. It's a plain struct rather than a goroutine
// so the main loop drives it deterministically: ask for nextDelay, sleep
// (or wake on signal), then ask which event fires next.
//
// The scheduler is single-goroutine by contract. Per SPEC §"Concurrency"
// line 1108, "a tick won't fire while the previous tick is still running" -
// the daemon's main loop completes each FullSync/Tick before re-querying
// the scheduler.
type scheduler struct {
	// pollInterval is SPEC's `settings.poll_interval`; the scheduler fires a
	// Tick at the wall-clock multiple of this duration past Truncate(0).
	pollInterval time.Duration

	// fullSyncInterval is SPEC's `settings.full_sync_interval`; a FullSync
	// fires at startedAt + fullSyncInterval after the prior FullSync.
	fullSyncInterval time.Duration

	// nextTickAt is the wall-clock time of the next Tick. Computed via
	// SPEC's formula: now.Truncate(pollInterval).Add(pollInterval).
	nextTickAt time.Time

	// nextFullSyncAt is the wall-clock time of the next periodic FullSync.
	// Anchored to startedAt for the initial value; reset to "now +
	// fullSyncInterval" after each completed FullSync.
	nextFullSyncAt time.Time

	// fastTrack carries Tick's NeedsFullResync signal. When true, the next
	// loop iteration runs FullSync immediately (delay=0) without waiting
	// for the regular cadence. Cleared after the FullSync runs.
	fastTrack bool
}

// schedulerEvent names which operation a scheduled wake-up should run.
type schedulerEvent int

const (
	// eventTick runs Reconciler.Tick.
	eventTick schedulerEvent = iota
	// eventFullSync runs Reconciler.FullSync.
	eventFullSync
)

// newScheduler constructs a scheduler from the settings + a wall-clock
// startedAt anchor. Initial nextTickAt is computed from startedAt so the
// daemon's first sleep aligns to the next pollInterval boundary;
// nextFullSyncAt is startedAt + fullSyncInterval so the first periodic
// FullSync fires one interval after process start (SPEC's "every
// `full_sync_interval`" cadence).
func newScheduler(startedAt time.Time, pollInterval, fullSyncInterval time.Duration) *scheduler {
	return &scheduler{
		pollInterval:     pollInterval,
		fullSyncInterval: fullSyncInterval,
		nextTickAt:       nextTickBoundary(startedAt, pollInterval),
		nextFullSyncAt:   startedAt.Add(fullSyncInterval),
	}
}

// requestFastTrackFullSync flags the scheduler to fire a FullSync on the
// next iteration regardless of the regular cadence. SPEC §"per-tick
// reconciliation" step 2 ("410 GONE recovery: schedule an immediate full
// re-sync") and step 4 (any source whose token doesn't advance) feed this.
func (s *scheduler) requestFastTrackFullSync() {
	s.fastTrack = true
}

// hasFastTrack reports whether a fast-track FullSync is queued.
func (s *scheduler) hasFastTrack() bool {
	return s.fastTrack
}

// nextEvent returns the operation to run next and the delay until it
// should fire (relative to now). A fast-track FullSync always fires
// immediately (delay=0). When both timers are already past on wake (the
// "laptop slept overnight" case from SPEC §"Sleep and wake" line 1097),
// FullSync wins because it subsumes Tick's work - running Tick first
// would just be wasted effort. When neither is past, the soonest wins;
// FullSync also wins on an exact-tie or earlier-or-equal future time
// for the same reason.
func (s *scheduler) nextEvent(now time.Time) (schedulerEvent, time.Duration) {
	if s.fastTrack {
		return eventFullSync, 0
	}

	tickDelay := s.nextTickAt.Sub(now)
	if tickDelay < 0 {
		tickDelay = 0
	}
	fullDelay := s.nextFullSyncAt.Sub(now)
	if fullDelay < 0 {
		fullDelay = 0
	}

	// FullSync wins when:
	//   - it's also already past on a sleep-wake catch-up (both delays 0
	//     and nextFullSyncAt is past), OR
	//   - its delay is <= tickDelay in the normal future-event case.
	// The first condition catches SPEC's "if the gap since the last
	// completed full re-sync exceeds full_sync_interval, the periodic
	// full re-sync fires immediately on wake".
	fullSyncDue := !s.nextFullSyncAt.After(now)
	if fullSyncDue || fullDelay <= tickDelay {
		return eventFullSync, fullDelay
	}
	return eventTick, tickDelay
}

// recordTickRan updates nextTickAt after a Tick completes. The new
// boundary is computed from "now" rather than "old nextTickAt + interval"
// so a Tick that ran late (e.g., the previous tick took longer than the
// interval) doesn't double-fire on the next pass.
func (s *scheduler) recordTickRan(now time.Time) {
	s.nextTickAt = nextTickBoundary(now, s.pollInterval)
}

// recordFullSyncRan updates nextFullSyncAt and nextTickAt after a FullSync
// completes. The fast-track flag clears regardless of whether this run
// satisfied a fast-track request - the FullSync just ran, so any
// outstanding NeedsFullResync signal has been served.
func (s *scheduler) recordFullSyncRan(now time.Time) {
	s.fastTrack = false
	s.nextFullSyncAt = now.Add(s.fullSyncInterval)
	s.nextTickAt = nextTickBoundary(now, s.pollInterval)
}

// nextTickBoundary computes SPEC's wall-clock-aligned next-tick time:
// `now.Truncate(interval).Add(interval)`. With interval=60s and now=
// 12:00:23, the next tick fires at 12:01:00 (and after a sleep that
// crossed multiple 60-second boundaries, the next tick still fires at
// the immediately-upcoming 60s mark - SPEC line 1096 pins this to "the
// wall-clock-derived next-tick time is already in the past").
//
// A zero or negative interval is treated as "no scheduling": the
// returned boundary is now itself, which makes the scheduler fire
// immediately. Production wiring validates the config has positive
// values; this guard is for tests that exercise edge cases.
func nextTickBoundary(now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return now
	}
	return now.Truncate(interval).Add(interval)
}
