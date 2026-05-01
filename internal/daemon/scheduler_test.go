package daemon

import (
	"testing"
	"time"
)

// SPEC §"Sleep and wake" line 1094: "next_tick = now.Truncate(poll_interval)
// .Add(poll_interval)". With poll_interval=60s and now at 12:00:23, the next
// tick should fire at 12:01:00.
func TestNextTickBoundary_Truncates(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 23, 0, time.UTC)
	got := nextTickBoundary(now, 60*time.Second)
	want := time.Date(2026, 4, 30, 12, 1, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextTickBoundary = %v, want %v", got, want)
	}
}

// A now exactly on a boundary returns the NEXT boundary, not the current
// instant. This avoids fire-twice-on-the-same-instant when the daemon
// checks just after a tick completed.
func TestNextTickBoundary_OnBoundaryAdvances(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	got := nextTickBoundary(now, 60*time.Second)
	want := time.Date(2026, 4, 30, 12, 1, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextTickBoundary = %v, want %v", got, want)
	}
}

// Zero/negative interval is treated as "fire immediately" (used as a
// defensive guard; production wiring validates positive values).
func TestNextTickBoundary_ZeroIntervalReturnsNow(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 23, 0, time.UTC)
	got := nextTickBoundary(now, 0)
	if !got.Equal(now) {
		t.Errorf("nextTickBoundary(zero) = %v, want %v", got, now)
	}
}

// Initial scheduler at startedAt computes nextTickAt to the next
// pollInterval boundary and nextFullSyncAt to startedAt + fullSyncInterval.
func TestScheduler_InitialState(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 12, 0, 23, 0, time.UTC)
	s := newScheduler(startedAt, 60*time.Second, 24*time.Hour)

	wantTick := time.Date(2026, 4, 30, 12, 1, 0, 0, time.UTC)
	if !s.nextTickAt.Equal(wantTick) {
		t.Errorf("nextTickAt = %v, want %v", s.nextTickAt, wantTick)
	}
	wantFull := startedAt.Add(24 * time.Hour)
	if !s.nextFullSyncAt.Equal(wantFull) {
		t.Errorf("nextFullSyncAt = %v, want %v", s.nextFullSyncAt, wantFull)
	}
}

// nextEvent picks the soonest of (tick, full-sync). Tick first when it's
// earlier.
func TestScheduler_NextEventPicksTickWhenSooner(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := newScheduler(startedAt, 60*time.Second, 24*time.Hour)

	now := startedAt.Add(15 * time.Second)
	event, delay := s.nextEvent(now)
	if event != eventTick {
		t.Errorf("event = %v, want eventTick", event)
	}
	if delay != 45*time.Second {
		t.Errorf("delay = %v, want 45s", delay)
	}
}

// FullSync wins when the FullSync timer is sooner.
func TestScheduler_NextEventPicksFullSyncWhenSooner(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	// Short fullSyncInterval so the very next event is FullSync.
	s := newScheduler(startedAt, 60*time.Second, 30*time.Second)

	// The next-tick boundary is at 12:01:00; nextFullSyncAt is at 12:00:30.
	now := startedAt.Add(5 * time.Second)
	event, delay := s.nextEvent(now)
	if event != eventFullSync {
		t.Errorf("event = %v, want eventFullSync", event)
	}
	if delay != 25*time.Second {
		t.Errorf("delay = %v, want 25s", delay)
	}
}

// Per SPEC line 1097, on tie the FullSync fires first. This isn't a
// strictly contractual SPEC requirement but it's the documented behavior
// when both timers are due simultaneously after a sleep.
func TestScheduler_NextEventTieFavorsFullSync(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := newScheduler(startedAt, 60*time.Second, 60*time.Second)
	// At t+60s both nextTickAt and nextFullSyncAt are equal (12:01:00).
	now := startedAt.Add(60 * time.Second)
	event, _ := s.nextEvent(now)
	if event != eventFullSync {
		t.Errorf("tie should favor FullSync; got %v", event)
	}
}

// Fast-track flag wins regardless of regular cadence: next event is
// FullSync with delay=0.
func TestScheduler_FastTrackFiresImmediately(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := newScheduler(startedAt, 60*time.Second, 24*time.Hour)
	s.requestFastTrackFullSync()

	now := startedAt.Add(5 * time.Second)
	event, delay := s.nextEvent(now)
	if event != eventFullSync {
		t.Errorf("fast-track event = %v, want eventFullSync", event)
	}
	if delay != 0 {
		t.Errorf("fast-track delay = %v, want 0", delay)
	}
}

// recordFullSyncRan clears the fast-track flag and resets both timers.
// Per the algorithm in CLAUDE.md / proposal, nextTickAt is recomputed from
// "now" (not from "old nextTickAt + interval") so a long FullSync doesn't
// double-fire the next tick.
func TestScheduler_RecordFullSyncResetsTimers(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := newScheduler(startedAt, 60*time.Second, 24*time.Hour)
	s.requestFastTrackFullSync()

	// FullSync ran at startedAt + 5 minutes (way past the original
	// nextTickAt at 12:01:00).
	doneAt := startedAt.Add(5 * time.Minute)
	s.recordFullSyncRan(doneAt)

	if s.fastTrack {
		t.Errorf("fast-track flag should clear after FullSync runs")
	}
	wantNextTick := time.Date(2026, 4, 30, 12, 6, 0, 0, time.UTC)
	if !s.nextTickAt.Equal(wantNextTick) {
		t.Errorf("nextTickAt = %v, want %v (recomputed from doneAt)",
			s.nextTickAt, wantNextTick)
	}
	wantNextFull := doneAt.Add(24 * time.Hour)
	if !s.nextFullSyncAt.Equal(wantNextFull) {
		t.Errorf("nextFullSyncAt = %v, want %v", s.nextFullSyncAt, wantNextFull)
	}
}

// recordTickRan recomputes nextTickAt from now without touching FullSync.
func TestScheduler_RecordTickAdvancesTickOnly(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := newScheduler(startedAt, 60*time.Second, 24*time.Hour)
	originalFullSyncAt := s.nextFullSyncAt

	s.recordTickRan(startedAt.Add(90 * time.Second))

	wantNextTick := time.Date(2026, 4, 30, 12, 2, 0, 0, time.UTC)
	if !s.nextTickAt.Equal(wantNextTick) {
		t.Errorf("nextTickAt = %v, want %v", s.nextTickAt, wantNextTick)
	}
	if !s.nextFullSyncAt.Equal(originalFullSyncAt) {
		t.Errorf("recordTickRan must not affect nextFullSyncAt; got %v, want %v",
			s.nextFullSyncAt, originalFullSyncAt)
	}
}

// SPEC line 1096: "The next tick fires immediately on wake (the
// wall-clock-derived next-tick time is already in the past)." Simulate
// this by setting now well past nextTickAt.
func TestScheduler_SleepAcrossBoundaryTriggersImmediateTick(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := newScheduler(startedAt, 60*time.Second, 24*time.Hour)

	// Wake up 10 minutes later. The original nextTickAt was 12:01:00,
	// long past; nextEvent should report delay=0.
	now := startedAt.Add(10 * time.Minute)
	event, delay := s.nextEvent(now)
	if event != eventTick {
		t.Errorf("event = %v, want eventTick", event)
	}
	if delay != 0 {
		t.Errorf("delay = %v, want 0 (catch-up)", delay)
	}
}

// SPEC line 1097: "The periodic full re-sync fires immediately on wake
// if the gap since the last completed full re-sync exceeds
// full_sync_interval." We model this by having the wake-up be past
// nextFullSyncAt.
func TestScheduler_SleepCrossesFullSyncTriggersImmediateFullSync(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	s := newScheduler(startedAt, 60*time.Second, 24*time.Hour)

	// 25 hours later - past nextFullSyncAt at startedAt+24h.
	now := startedAt.Add(25 * time.Hour)
	event, delay := s.nextEvent(now)
	if event != eventFullSync {
		t.Errorf("event = %v, want eventFullSync", event)
	}
	if delay != 0 {
		t.Errorf("delay = %v, want 0 (catch-up FullSync)", delay)
	}
}

// hasFastTrack reflects requestFastTrackFullSync's effect.
func TestScheduler_HasFastTrack(t *testing.T) {
	s := newScheduler(time.Now(), 60*time.Second, 24*time.Hour)
	if s.hasFastTrack() {
		t.Errorf("fresh scheduler should not have fast-track set")
	}
	s.requestFastTrackFullSync()
	if !s.hasFastTrack() {
		t.Errorf("after request, fast-track should be set")
	}
	s.recordFullSyncRan(time.Now())
	if s.hasFastTrack() {
		t.Errorf("after FullSync runs, fast-track should clear")
	}
}
