package daemon

import (
	"sync"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// stateSnapshot is the in-memory state the IPC handler serializes for
// `calendar-sync status`. Per SPEC §"calendar-sync status" lines 722-728
// the wire shape is one global header line followed by one line per pdir
// then a `_meta` trailer. The struct mirrors that shape; serialization
// lives in socket.go where it's adjacent to the protocol details.
type stateSnapshot struct {
	PID              int
	StartedAt        time.Time
	PollInterval     time.Duration
	FullSyncInterval time.Duration
	LastFullSyncAt   time.Time // zero if no FullSync has completed yet

	// PDirs is the per-pdir status, ordered to match c.Canonical.PDirs.
	PDirs []pdirState
}

// pdirState mirrors SPEC's per-pdir status line. The `last_tick_*` fields
// hold the most recent FullSync OR Tick result (whichever ran most
// recently), so a daemon that just finished startup but has yet to tick
// shows the FullSync's counts; subsequent ticks update them.
type pdirState struct {
	Pair             string
	Direction        string
	SourceCalendar   string
	TargetCalendar   string
	Mirrors          int          // current inventory size for the target
	LastTickAt       time.Time    // zero if no pass has run yet
	LastTickStatus   string       // "ok" or "failed" (empty when LastTickAt zero)
	Counts           syncpkg.Counts
}

// stateStore wraps the snapshot with a mutex so the main loop can record
// pass results while the accept loop reads them via snapshot().
type stateStore struct {
	mu   sync.Mutex
	snap stateSnapshot
}

// newStateStore returns a stateStore preloaded from the canonical config:
// pid, intervals, and one pdirState entry per Canonical.PDirs entry. Per-
// pdir LastTick* fields stay zero until the first FullSync or Tick records
// into them.
func newStateStore(
	pid int,
	startedAt time.Time,
	settings config.Settings,
	pdirs []config.PDir,
) *stateStore {
	pds := make([]pdirState, len(pdirs))
	for i, pd := range pdirs {
		pds[i] = pdirState{
			Pair:           pd.PairName,
			Direction:      pd.Direction,
			SourceCalendar: pd.SourceCalendar,
			TargetCalendar: pd.TargetCalendar,
		}
	}
	return &stateStore{
		snap: stateSnapshot{
			PID:              pid,
			StartedAt:        startedAt,
			PollInterval:     settings.PollInterval.Duration(),
			FullSyncInterval: settings.FullSyncInterval.Duration(),
			PDirs:            pds,
		},
	}
}

// recordFullSync folds a FullSync result into the snapshot. Sets
// LastFullSyncAt and updates each pdir's LastTickAt + Counts + Status.
// Inventory size is taken from inventorySizes (a map[target]int the
// caller computes from the Reconciler's in-memory inventories).
func (s *stateStore) recordFullSync(at time.Time, res syncpkg.FullSyncResult, inventorySizes map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.LastFullSyncAt = at
	s.applyResults(at, res.PDirs, inventorySizes)
}

// recordTick folds a Tick result into the snapshot. Same as recordFullSync
// minus the LastFullSyncAt update (per SPEC, the timestamp is specifically
// for FullSync passes; Tick is incremental and doesn't refresh it).
func (s *stateStore) recordTick(at time.Time, res syncpkg.TickResult, inventorySizes map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyResults(at, res.PDirs, inventorySizes)
}

// applyResults updates the per-pdir entries from a slice of PDirResults.
// Lookup is by (pair, direction); pdirs that don't appear in the result
// (shouldn't happen) keep their previous values. Status is "ok" when
// PDirResult.Err is nil, "failed" otherwise.
//
// Caller must hold s.mu.
func (s *stateStore) applyResults(at time.Time, pdirs []syncpkg.PDirResult, inventorySizes map[string]int) {
	for _, pr := range pdirs {
		idx := s.findPDir(pr.Pair, pr.Direction)
		if idx < 0 {
			continue
		}
		pd := &s.snap.PDirs[idx]
		pd.LastTickAt = at
		pd.Counts = pr.Counts
		if pr.Err != nil {
			pd.LastTickStatus = "failed"
		} else {
			pd.LastTickStatus = "ok"
		}
		if size, ok := inventorySizes[pd.TargetCalendar]; ok {
			pd.Mirrors = size
		}
	}
}

// findPDir returns the index of the pdir with the given pair and
// direction, or -1 if not found.
//
// Caller must hold s.mu.
func (s *stateStore) findPDir(pair, direction string) int {
	for i := range s.snap.PDirs {
		if s.snap.PDirs[i].Pair == pair && s.snap.PDirs[i].Direction == direction {
			return i
		}
	}
	return -1
}

// snapshot returns a deep copy of the current state. The caller (the IPC
// handler) is free to mutate the returned value without affecting the
// store. The PDirs slice is duplicated so the accept loop never holds a
// reference into the main loop's working state.
func (s *stateStore) snapshot() stateSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	out.PDirs = make([]pdirState, len(s.snap.PDirs))
	copy(out.PDirs, s.snap.PDirs)
	return out
}
