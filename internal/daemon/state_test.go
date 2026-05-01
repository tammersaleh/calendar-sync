package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// makePDirs returns one a_to_b pdir for testing.
func makePDirs() []config.PDir {
	return []config.PDir{
		{
			PairName:       "p1",
			Direction:      config.PDirAtoB,
			SourceCalendar: "src",
			TargetCalendar: "tgt",
		},
	}
}

// TestStateStore_InitializesPDirs: newStateStore seeds one pdirState per
// canonical pdir.
func TestStateStore_InitializesPDirs(t *testing.T) {
	startedAt := time.Now()
	settings := config.Settings{
		PollInterval:     mustDuration("60s"),
		FullSyncInterval: mustDuration("24h"),
	}
	store := newStateStore(42, startedAt, settings, makePDirs())
	snap := store.snapshot()
	if snap.PID != 42 {
		t.Errorf("PID = %d, want 42", snap.PID)
	}
	if !snap.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt = %v, want %v", snap.StartedAt, startedAt)
	}
	if snap.PollInterval != 60*time.Second {
		t.Errorf("PollInterval = %v, want 60s", snap.PollInterval)
	}
	if len(snap.PDirs) != 1 {
		t.Fatalf("PDirs len = %d, want 1", len(snap.PDirs))
	}
	if snap.PDirs[0].Pair != "p1" {
		t.Errorf("Pair = %q, want p1", snap.PDirs[0].Pair)
	}
	if !snap.PDirs[0].LastTickAt.IsZero() {
		t.Errorf("LastTickAt should be zero before any pass; got %v", snap.PDirs[0].LastTickAt)
	}
	if snap.PDirs[0].LastTickStatus != "" {
		t.Errorf("LastTickStatus should be empty before any pass; got %q", snap.PDirs[0].LastTickStatus)
	}
}

// TestStateStore_RecordFullSyncUpdatesAll: recordFullSync sets
// LastFullSyncAt + per-pdir fields.
func TestStateStore_RecordFullSyncUpdatesAll(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 8, 0, 0, 0, time.UTC)
	settings := config.Settings{
		PollInterval:     mustDuration("60s"),
		FullSyncInterval: mustDuration("24h"),
	}
	store := newStateStore(1, startedAt, settings, makePDirs())

	at := startedAt.Add(time.Second)
	store.recordFullSync(at, syncpkg.FullSyncResult{
		PDirs: []syncpkg.PDirResult{
			{
				Pair:      "p1",
				Direction: config.PDirAtoB,
				Counts:    syncpkg.Counts{Patches: 3, EventsProcessed: 3},
			},
		},
	}, map[string]int{"tgt": 100})

	snap := store.snapshot()
	if !snap.LastFullSyncAt.Equal(at) {
		t.Errorf("LastFullSyncAt = %v, want %v", snap.LastFullSyncAt, at)
	}
	pd := snap.PDirs[0]
	if !pd.LastTickAt.Equal(at) {
		t.Errorf("LastTickAt = %v, want %v", pd.LastTickAt, at)
	}
	if pd.LastTickStatus != "ok" {
		t.Errorf("LastTickStatus = %q, want ok", pd.LastTickStatus)
	}
	if pd.Counts.Patches != 3 {
		t.Errorf("Counts.Patches = %d, want 3", pd.Counts.Patches)
	}
	if pd.Mirrors != 100 {
		t.Errorf("Mirrors = %d, want 100", pd.Mirrors)
	}
}

// TestStateStore_RecordTickDoesNotTouchLastFullSync: Tick updates per-pdir
// fields but leaves LastFullSyncAt alone.
func TestStateStore_RecordTickDoesNotTouchLastFullSync(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 8, 0, 0, 0, time.UTC)
	settings := config.Settings{
		PollInterval:     mustDuration("60s"),
		FullSyncInterval: mustDuration("24h"),
	}
	store := newStateStore(1, startedAt, settings, makePDirs())

	// First a FullSync.
	fullAt := startedAt
	store.recordFullSync(fullAt, syncpkg.FullSyncResult{
		PDirs: []syncpkg.PDirResult{
			{Pair: "p1", Direction: config.PDirAtoB, Counts: syncpkg.Counts{Inserts: 1}},
		},
	}, nil)

	// Then a Tick at a later time.
	tickAt := startedAt.Add(time.Minute)
	store.recordTick(tickAt, syncpkg.TickResult{
		PDirs: []syncpkg.PDirResult{
			{Pair: "p1", Direction: config.PDirAtoB, Counts: syncpkg.Counts{Patches: 5}},
		},
	}, nil)

	snap := store.snapshot()
	if !snap.LastFullSyncAt.Equal(fullAt) {
		t.Errorf("LastFullSyncAt = %v, want %v (Tick must not touch it)",
			snap.LastFullSyncAt, fullAt)
	}
	if !snap.PDirs[0].LastTickAt.Equal(tickAt) {
		t.Errorf("LastTickAt = %v, want %v", snap.PDirs[0].LastTickAt, tickAt)
	}
	if snap.PDirs[0].Counts.Patches != 5 {
		t.Errorf("Counts.Patches = %d, want 5", snap.PDirs[0].Counts.Patches)
	}
}

// TestStateStore_FailedPDirRendersFailedStatus: PDirResult.Err populates
// LastTickStatus="failed".
func TestStateStore_FailedPDirRendersFailedStatus(t *testing.T) {
	store := newStateStore(1, time.Now(), config.Settings{}, makePDirs())
	store.recordTick(time.Now(), syncpkg.TickResult{
		PDirs: []syncpkg.PDirResult{
			{Pair: "p1", Direction: config.PDirAtoB, Err: errors.New("boom")},
		},
	}, nil)
	snap := store.snapshot()
	if snap.PDirs[0].LastTickStatus != "failed" {
		t.Errorf("LastTickStatus = %q, want failed", snap.PDirs[0].LastTickStatus)
	}
}

// TestStateStore_SnapshotIsDeepCopy: mutating the returned snapshot does
// not affect future snapshots.
func TestStateStore_SnapshotIsDeepCopy(t *testing.T) {
	store := newStateStore(1, time.Now(), config.Settings{}, makePDirs())
	snap1 := store.snapshot()
	snap1.PDirs[0].Mirrors = 9999
	snap2 := store.snapshot()
	if snap2.PDirs[0].Mirrors == 9999 {
		t.Errorf("snapshot returned shared slice; mutating snap1 affected snap2")
	}
}

// TestStateStore_UnknownPDirInResultsIgnored: a PDirResult that doesn't
// match any seeded pdir is silently skipped (defensive; shouldn't happen).
func TestStateStore_UnknownPDirInResultsIgnored(t *testing.T) {
	store := newStateStore(1, time.Now(), config.Settings{}, makePDirs())
	store.recordTick(time.Now(), syncpkg.TickResult{
		PDirs: []syncpkg.PDirResult{
			{Pair: "p2", Direction: config.PDirAtoB, Counts: syncpkg.Counts{Inserts: 99}},
		},
	}, nil)
	snap := store.snapshot()
	if snap.PDirs[0].Counts.Inserts != 0 {
		t.Errorf("seeded pdir mutated by unknown result; got %+v", snap.PDirs[0].Counts)
	}
}
