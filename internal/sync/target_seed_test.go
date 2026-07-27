package sync

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// B28 target-token invariants.
//
// The map's contract is now: an entry exists only when it holds a usable
// token. Absent means "this target is not delta-capable right now"; there
// is no third "present but empty" state, because runTargetDeltaPhase's
// `token == ""` guard turned that state into a silently dead phase for
// months.

// A successful seed that returns no token is a protocol violation, not a
// normal long-calendar outcome. Before B28's pagination fix this is exactly
// what production saw on both targets, and storing "" is what disabled the
// phase.
func TestSeedTargetSyncTokens_EmptyTokenIsNotStored(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))

	queueWritableTargetSeed(api, "tgt-A", "")

	errs := r.seedTargetSyncTokens(context.Background())

	if _, present := r.targetSyncTokens["tgt-A"]; present {
		t.Errorf("empty token must not be stored; map = %#v", r.targetSyncTokens)
	}
	if errs["tgt-A"] == nil {
		t.Error("expected a seed error for the empty-token response")
	}
}

// FullSync must not move a valid target cursor forward. If it did, a
// periodic FullSync landing between a user's target edit and the next tick
// would seed past the edit and lose it permanently - and it would do that
// on every FullSync, not just once.
func TestSeedTargetSyncTokens_PreservesExistingToken(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))
	r.targetSyncTokens["tgt-A"] = "tok-existing"

	// Queue a response that would overwrite the cursor if consumed.
	queueWritableTargetSeed(api, "tgt-A", "tok-newer")

	r.seedTargetSyncTokens(context.Background())

	if got := r.targetSyncTokens["tgt-A"]; got != "tok-existing" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-existing (unchanged)", got)
	}
	// Skipping should also skip the API call: seeding an already-seeded
	// target is a full unbounded list of the whole calendar (41 pages in
	// production), so this is a real cost, not just tidiness.
	if n := len(api.callsByOp("EventsList")); n != 0 {
		t.Errorf("expected no EventsList call for an already-seeded target, got %d", n)
	}
}

func TestSeedTargetSyncTokens_SeedsWhenMissing(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))

	queueWritableTargetSeed(api, "tgt-A", "tok-fresh")

	if errs := r.seedTargetSyncTokens(context.Background()); len(errs) != 0 {
		t.Fatalf("unexpected seed errors: %v", errs)
	}
	if got := r.targetSyncTokens["tgt-A"]; got != "tok-fresh" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-fresh", got)
	}
}

// A failed seed leaves the target absent rather than storing a sentinel.
func TestSeedTargetSyncTokens_ErrorLeavesTargetAbsent(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))

	api.queueListFullErr("tgt-A", errors.New("boom"))

	errs := r.seedTargetSyncTokens(context.Background())

	if errs["tgt-A"] == nil {
		t.Error("expected a seed error")
	}
	if _, present := r.targetSyncTokens["tgt-A"]; present {
		t.Errorf("failed seed must leave the target absent; map = %#v", r.targetSyncTokens)
	}
}

// Truncated pagination reaches the seed as a plain error from EventsList.
// It must be treated like any other seed failure - never as a usable token.
func TestSeedTargetSyncTokens_IncompletePaginationLeavesTargetAbsent(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))

	api.queueListFullErr("tgt-A", gws.ErrIncompletePagination)

	r.seedTargetSyncTokens(context.Background())

	if _, present := r.targetSyncTokens["tgt-A"]; present {
		t.Errorf("truncated seed must leave the target absent; map = %#v", r.targetSyncTokens)
	}
}

// A tokenless target warns once, not once per tick. B28 hid for months
// because this state logged nothing above DEBUG; a per-tick warning would
// be filtered back out just as effectively.
func TestTargetDeltaPhase_TokenlessTargetWarnsOnce(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))
	logger := &captureLogger{}
	r.Log = logger
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	// No entry in targetSyncTokens: the target is not delta-capable.

	const ticks = 3
	for range ticks {
		api.queueListIncr("src-A", nil, "tok-src")
		r.syncTokens["src-A"] = "tok-src-old"
		if _, err := r.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}

	n := 0
	for _, w := range logger.warns {
		if msg, _ := w["msg"].(string); strings.Contains(msg, "no target syncToken") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly 1 tokenless warning across %d ticks, got %d (%v)",
			ticks, n, logger.warns)
	}
}

// Once a target recovers a usable token, a later loss must warn again -
// the once-only latch has to reset, or a second outage goes unreported.
func TestTargetDeltaPhase_WarnLatchResetsAfterRecovery(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))
	logger := &captureLogger{}
	r.Log = logger
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	// Tick 1: no token -> warn.
	api.queueListIncr("src-A", nil, "tok-src")
	mustTick(t, r)

	// Tick 2: token present -> phase runs, latch clears.
	r.targetSyncTokens["tgt-A"] = "tok-tgt"
	queueTargetIncrDelta(api, "tgt-A", nil, "tok-tgt-2")
	api.queueListIncr("src-A", nil, "tok-src")
	mustTick(t, r)

	// Tick 3: token gone again -> warn again.
	delete(r.targetSyncTokens, "tgt-A")
	api.queueListIncr("src-A", nil, "tok-src")
	mustTick(t, r)

	n := 0
	for _, w := range logger.warns {
		if msg, _ := w["msg"].(string); strings.Contains(msg, "no target syncToken") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("expected 2 tokenless warnings (loss, recovery, loss), got %d (%v)", n, logger.warns)
	}
}

func mustTick(t *testing.T, r *Reconciler) {
	t.Helper()
	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// A FullSync running while a target token is pinned (target-delta left it
// unadvanced so the next tick re-delivers) must not reseed over it.
func TestFullSync_DoesNotReseedPinnedTargetToken(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))
	r.targetSyncTokens["tgt-A"] = "tok-pinned"

	api.queueListFull("src-A", nil, "tok-src")
	queueWritableTargetSeed(api, "tgt-A", "tok-would-clobber")
	api.queueListInventory("tgt-A", "3", nil)

	if _, err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	if got := r.targetSyncTokens["tgt-A"]; got != "tok-pinned" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-pinned (FullSync must not reseed)", got)
	}
}
