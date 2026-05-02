package sync

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// ---------- helpers ----------

// makeTestPDir builds a config.PDir with a simple naming convention so a
// test can wire two pdirs against shared or distinct sources/targets
// without verbose struct literals at every call site.
func makeTestPDir(pair, source, target string, sourceWritable bool) config.PDir {
	return config.PDir{
		PairName:       pair,
		Direction:      config.PDirAtoB,
		SourceCalendar: source,
		TargetCalendar: target,
		SourceWritable: sourceWritable,
	}
}

// makeCanonical wraps PDirs in a *config.Canonical with an empty Settings
// + Calendars. The reconciler doesn't read the Calendars map directly; only
// PDirs is consulted by uniqueSources / uniqueTargets / per-pdir loops.
func makeCanonical(pdirs ...config.PDir) *config.Canonical {
	return &config.Canonical{
		Settings:  config.Settings{},
		Calendars: map[string]config.Calendar{},
		PDirs:     pdirs,
	}
}

// newTestReconciler returns a Reconciler with sane defaults for the
// reconciler tests. Callers override fields on the returned value
// (Output, Horizon, OrphanConcurrency, etc.) before calling FullSync /
// Tick.
func newTestReconciler(api API, canonical *config.Canonical) *Reconciler {
	return New(api, canonical,
		WithNow(fixedNow(must2026())),
		WithHorizon(30*24*time.Hour),
		WithOrphanConcurrency(1),
	)
}

// pdirByPair is a lookup helper: map a pair name -> PDirResult.
func pdirByPair(results []PDirResult, pair string) (PDirResult, bool) {
	for _, pr := range results {
		if pr.Pair == pair {
			return pr, true
		}
	}
	return PDirResult{}, false
}

// queueEmptyInventory enqueues two empty inventory list responses (v=2 + v=1)
// for the given target. Use when a test doesn't care about pre-existing
// mirrors for the target; an empty inventory means every source event
// either inserts or skips.
func queueEmptyInventory(api *stubAPI, target string) {
	api.queueListInventory(target, mirror.SchemaVersion, nil)
	api.queueListInventory(target, "1", nil)
}

// ---------- FullSync: empty source list ----------

func TestFullSync_Empty_SourceListAdvancesToken(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	api.queueListFull("src-A", nil, "token-1")
	queueEmptyInventory(api, "tgt-A")

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	if len(res.PDirs) != 1 {
		t.Fatalf("expected 1 PDirResult; got %d", len(res.PDirs))
	}
	if res.PDirs[0].Err != nil {
		t.Errorf("pdir should succeed; got %v", res.PDirs[0].Err)
	}
	if res.PDirs[0].Counts.EventsProcessed != 0 {
		t.Errorf("EventsProcessed = %d, want 0", res.PDirs[0].Counts.EventsProcessed)
	}
	if got := r.syncTokens["src-A"]; got != "token-1" {
		t.Errorf("syncTokens[src-A] = %q, want token-1", got)
	}
	if !res.PerSource["src-A"].SyncTokenChanged {
		t.Errorf("SyncTokenChanged should be true")
	}
	if res.PerSource["src-A"].NeedsFullResync {
		t.Errorf("empty-but-successful should not need full resync")
	}
}

// ---------- FullSync: a single insert ----------

func TestFullSync_OneNewEvent_InsertAdvancesToken(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeNonRecurringSource("evt-1", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueListFull("src-A", []gws.Event{*src}, "token-1")
	queueEmptyInventory(api, "tgt-A")
	// Insert + checksum followup.
	insertedID := "cs2DEADBEEF"
	inserted := &gws.Event{ID: insertedID, Summary: src.Summary, Updated: "2026-04-29T20:00:01Z"}
	api.queueInsert(inserted)
	api.queuePatch(inserted) // checksum followup

	sink, captured := captureOutputs()
	r := newTestReconciler(api, canonical)
	r.Output = sink

	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	pr, ok := pdirByPair(res.PDirs, "p1")
	if !ok {
		t.Fatalf("missing pdir result")
	}
	if pr.Err != nil {
		t.Errorf("pdir should succeed; got %v", pr.Err)
	}
	if pr.Counts.Inserts != 1 || pr.Counts.EventsProcessed != 1 {
		t.Errorf("Counts = %+v, want Inserts=1 EventsProcessed=1", pr.Counts)
	}
	if res.Aggregated.Inserts != 1 {
		t.Errorf("Aggregated.Inserts = %d, want 1", res.Aggregated.Inserts)
	}
	if got := r.syncTokens["src-A"]; got != "token-1" {
		t.Errorf("token did not advance; got %q", got)
	}
	if len(*captured) != 1 || (*captured)[0].Action != mirror.ActionInsert {
		t.Errorf("expected one insert outcome; got %+v", *captured)
	}
	// Inventory should now contain the inserted mirror.
	tup := mirror.SourceTuple{CalendarID: "src-A", EventID: "evt-1"}
	if got, ok := r.inventories["tgt-A"].Lookup(tup); !ok || got.ID != insertedID {
		t.Errorf("inventory missing inserted mirror; got=%v ok=%v", got, ok)
	}
}

// ---------- FullSync: source-list error blocks token advancement ----------

func TestFullSync_SourceListError_TokenStaysEmpty(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	api.queueListFullErr("src-A", errors.New("boom"))
	queueEmptyInventory(api, "tgt-A")

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail when source-list errors")
	}
	if got, ok := r.syncTokens["src-A"]; ok {
		t.Errorf("token should remain unset; got %q", got)
	}
	if !res.PerSource["src-A"].NeedsFullResync {
		t.Errorf("NeedsFullResync should be true after source-list failure")
	}
	if res.PerSource["src-A"].SyncTokenChanged {
		t.Errorf("SyncTokenChanged should be false after source-list failure")
	}
}

// ---------- FullSync: classify error on one event keeps others going ----------

// errorOnInsertAPI wraps a stubAPI but causes EventsInsert for a specific
// source event ID to fail. Other inserts succeed normally. Used to verify
// that one Classify error inside a pdir doesn't abort the loop.
type errorOnInsertAPI struct {
	*stubAPI
	failOnSummary string
}

func (e *errorOnInsertAPI) EventsInsert(ctx context.Context, calendarID string, body *gws.Event) (*gws.Event, error) {
	if body.Summary == e.failOnSummary {
		// Record the call so test assertions can still see attempted writes.
		e.stubAPI.mu.Lock()
		e.stubAPI.calls = append(e.stubAPI.calls,
			recordedCall{Op: "EventsInsert", CalendarID: calendarID, Body: body})
		e.stubAPI.mu.Unlock()
		return nil, errors.New("synthetic insert failure")
	}
	return e.stubAPI.EventsInsert(ctx, calendarID, body)
}

func TestFullSync_ClassifyErrorOnOneEvent_OthersProcessed(t *testing.T) {
	stub := newStubAPI()
	api := &errorOnInsertAPI{stubAPI: stub, failOnSummary: "fail-me"}
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	good1 := makeNonRecurringSource("good-1", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	good1.Summary = "Good1"
	bad := makeNonRecurringSource("bad-1", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	bad.Summary = "fail-me"
	good2 := makeNonRecurringSource("good-2", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	good2.Summary = "Good2"

	stub.queueListFull("src-A", []gws.Event{*good1, *bad, *good2}, "token-1")
	queueEmptyInventory(stub, "tgt-A")

	// Two successful inserts (good-1 + good-2). The bad insert returns an
	// error which is intercepted by errorOnInsertAPI before reaching the
	// stub queue.
	post1 := &gws.Event{ID: "m-good-1", Summary: "Good1", Updated: "2026-04-29T20:00:01Z"}
	post2 := &gws.Event{ID: "m-good-2", Summary: "Good2", Updated: "2026-04-29T20:00:01Z"}
	stub.queueInsert(post1)
	stub.queuePatch(post1) // checksum
	stub.queueInsert(post2)
	stub.queuePatch(post2) // checksum

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should be marked failed when one event errors")
	}
	if pr.Counts.Inserts != 2 {
		t.Errorf("Inserts = %d, want 2 (the two successful ones)", pr.Counts.Inserts)
	}
	// Token must NOT advance because pdir failed.
	if _, ok := r.syncTokens["src-A"]; ok {
		t.Errorf("token must not advance when a pdir errored")
	}
}

// ---------- FullSync: two pdirs sharing one source ----------

func TestFullSync_TwoPdirsSharedSource_OneFails_TokenStays(t *testing.T) {
	api := newStubAPI()
	// pdirA and pdirB share source src-A but have different targets.
	pdA := makeTestPDir("pA", "src-A", "tgt-A", true)
	pdB := makeTestPDir("pB", "src-A", "tgt-B", true)
	canonical := makeCanonical(pdA, pdB)

	src := makeNonRecurringSource("evt-1", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueListFull("src-A", []gws.Event{*src}, "token-1")

	// tgt-A inventory rebuild succeeds; tgt-B inventory rebuild errors.
	queueEmptyInventory(api, "tgt-A")
	api.queueListInventoryErr("tgt-B", mirror.SchemaVersion, errors.New("inventory boom"))

	// pdirA's classify will run successfully; queue insert + checksum.
	post := &gws.Event{ID: "m1", Summary: src.Summary, Updated: "2026-04-29T20:00:01Z"}
	api.queueInsert(post)
	api.queuePatch(post) // checksum

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}

	prA, _ := pdirByPair(res.PDirs, "pA")
	prB, _ := pdirByPair(res.PDirs, "pB")
	if prA.Err != nil {
		t.Errorf("pdirA should succeed; got %v", prA.Err)
	}
	if prB.Err == nil {
		t.Errorf("pdirB should fail (inventory rebuild errored)")
	}
	// Even though pdirA succeeded, its source's token must NOT advance
	// because pdirB (sharing the source) failed.
	if _, ok := r.syncTokens["src-A"]; ok {
		t.Errorf("shared-source token must not advance when ANY dependent pdir fails")
	}
	if res.PerSource["src-A"].SyncTokenChanged {
		t.Errorf("SyncTokenChanged should be false")
	}
}

// ---------- FullSync: two pdirs with different sources ----------

func TestFullSync_TwoSources_IndependentAdvancement(t *testing.T) {
	api := newStubAPI()
	pdA := makeTestPDir("pA", "src-A", "tgt-A", true)
	pdB := makeTestPDir("pB", "src-B", "tgt-B", true)
	canonical := makeCanonical(pdA, pdB)

	api.queueListFull("src-A", nil, "token-A")
	api.queueListFull("src-B", nil, "token-B")
	queueEmptyInventory(api, "tgt-A")
	queueEmptyInventory(api, "tgt-B")

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	if r.syncTokens["src-A"] != "token-A" || r.syncTokens["src-B"] != "token-B" {
		t.Errorf("tokens = %v, want both advanced", r.syncTokens)
	}
	if !res.PerSource["src-A"].SyncTokenChanged || !res.PerSource["src-B"].SyncTokenChanged {
		t.Errorf("both sources should be SyncTokenChanged=true")
	}
}

// ---------- FullSync: independent token advancement when one source fails ----------

func TestFullSync_OneSourceFails_OtherStillAdvances(t *testing.T) {
	api := newStubAPI()
	pdA := makeTestPDir("pA", "src-A", "tgt-A", true)
	pdB := makeTestPDir("pB", "src-B", "tgt-B", true)
	canonical := makeCanonical(pdA, pdB)

	api.queueListFullErr("src-A", errors.New("A boom"))
	api.queueListFull("src-B", nil, "token-B")
	queueEmptyInventory(api, "tgt-A")
	queueEmptyInventory(api, "tgt-B")

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	if _, ok := r.syncTokens["src-A"]; ok {
		t.Errorf("src-A token must not advance after error")
	}
	if r.syncTokens["src-B"] != "token-B" {
		t.Errorf("src-B token must advance independently")
	}
	if !res.PerSource["src-A"].NeedsFullResync {
		t.Errorf("src-A should need full resync after list error")
	}
	if !res.PerSource["src-B"].SyncTokenChanged {
		t.Errorf("src-B should be SyncTokenChanged=true")
	}
}

// ---------- FullSync: inventory rebuild error blocks dependent pdirs ----------

func TestFullSync_InventoryRebuildError_BlocksDependentPdir(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	api.queueListFull("src-A", nil, "token-1")
	api.queueListInventoryErr("tgt-A", mirror.SchemaVersion, errors.New("inventory kaboom"))

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail when inventory rebuild errors")
	}
	// Token must not advance because dependent pdir failed.
	if _, ok := r.syncTokens["src-A"]; ok {
		t.Errorf("token must not advance when inventory rebuild fails")
	}
}

// ---------- FullSync: orphan walk error marks pdir failed ----------

func TestFullSync_OrphanWalkError_PdirFails(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	// Source-list returns NO events; inventory has one mirror so the orphan
	// walk runs. The events.get for that orphaned mirror returns a non-404
	// error which the walker propagates.
	api.queueListFull("src-A", nil, "token-1")
	mirrorEv := makeOrphanMirror("m-orphan", "src-A:orphaned-evt")
	api.queueListInventory("tgt-A", mirror.SchemaVersion, []gws.Event{*mirrorEv})
	api.queueListInventory("tgt-A", "1", nil)
	api.queueGetErr("src-A", "orphaned-evt", errors.New("orphan get boom"))

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail when orphan walk errors")
	}
	if _, ok := r.syncTokens["src-A"]; ok {
		t.Errorf("token must not advance when orphan walk fails")
	}
}

// ---------- FullSync: orphan walk runs and prunes a missing source ----------

func TestFullSync_OrphanWalk_PrunesMissingSource(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	// Source-list is empty (the orphaned source vanished from Google).
	api.queueListFull("src-A", nil, "token-1")
	// Inventory still has a mirror for that vanished source.
	mirrorEv := makeOrphanMirror("m-ghost", "src-A:ghost")
	api.queueListInventory("tgt-A", mirror.SchemaVersion, []gws.Event{*mirrorEv})
	api.queueListInventory("tgt-A", "1", nil)
	// Orphan walk does events.get, which returns 404.
	api.queueGetErr("src-A", "ghost", &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	sink, captured := captureOutputs()
	r := newTestReconciler(api, canonical)
	r.Output = sink

	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should succeed; got %v", pr.Err)
	}
	if pr.Counts.Deletes != 1 {
		t.Errorf("expected 1 delete from orphan walk; got %d", pr.Counts.Deletes)
	}
	if r.syncTokens["src-A"] != "token-1" {
		t.Errorf("token must advance after successful orphan walk")
	}
	// Verify the orphan walk emitted exactly one delete outcome.
	deletes := 0
	for _, o := range *captured {
		if o.Action == mirror.ActionDelete {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("expected one delete outcome; got %d (%v)", deletes, *captured)
	}
}

// ---------- FullSync: source-list wire shape ----------

// Tick has no orphan walk (per SPEC). This test queues an inventory with
// an orphan-eligible mirror and then runs a Tick, asserting that
// EventsGet was NOT called. The inventory survives from a previous
// FullSync.
func TestTick_NoOrphanWalk(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	// Pre-seed the inventory + token as if a previous FullSync had run.
	r.syncTokens["src-A"] = "tok-pre"
	inv := NewInventory("tgt-A")
	tup := mirror.SourceTuple{CalendarID: "src-A", EventID: "ghost"}
	inv.Set(tup, makeOrphanMirror("m-ghost", tup.String()))
	r.inventories["tgt-A"] = inv

	// Tick: empty incremental delta.
	api.queueListIncr("src-A", nil, "tok-next")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should succeed; got %v", pr.Err)
	}
	// EventsGet must NOT be called - that's only the orphan walk's job.
	if calls := api.callsByOp("EventsGet"); len(calls) > 0 {
		t.Errorf("Tick must not run orphan walk; got %d EventsGet calls", len(calls))
	}
	// Inventory entry must remain (orphan walk would have pruned it).
	if _, ok := inv.Lookup(tup); !ok {
		t.Errorf("inventory entry pruned during Tick - unexpected")
	}
	if r.syncTokens["src-A"] != "tok-next" {
		t.Errorf("token = %q, want tok-next", r.syncTokens["src-A"])
	}
}

// ---------- Tick: empty token triggers NeedsFullResync ----------

func TestTick_EmptyToken_NeedsFullResync(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	// No syncTokens set, no inventory - cold-start scenario.

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	if !res.PerSource["src-A"].NeedsFullResync {
		t.Errorf("first-tick should signal NeedsFullResync")
	}
	if len(api.callsByOp("EventsList")) != 0 {
		t.Errorf("first-tick must NOT call events.list (no token to use); got %d",
			len(api.callsByOp("EventsList")))
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should not be marked failed on first-tick; got %v", pr.Err)
	}
}

// ---------- Tick: empty delta advances token ----------

func TestTick_EmptyDelta_TokenAdvances(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	api.queueListIncr("src-A", nil, "tok-new")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	if r.syncTokens["src-A"] != "tok-new" {
		t.Errorf("token = %q, want tok-new", r.syncTokens["src-A"])
	}
	if !res.PerSource["src-A"].SyncTokenChanged {
		t.Errorf("SyncTokenChanged should be true")
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Counts.EventsProcessed != 0 {
		t.Errorf("EventsProcessed = %d, want 0", pr.Counts.EventsProcessed)
	}
}

// ---------- Tick: delta with events runs classify ----------

func TestTick_DeltaWithEvents_ClassifyRuns(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	src := makeNonRecurringSource("evt-1", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	post := &gws.Event{ID: "m1", Summary: src.Summary, Updated: "2026-04-29T20:00:01Z"}
	api.queueInsert(post)
	api.queuePatch(post) // checksum

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Counts.Inserts != 1 {
		t.Errorf("Inserts = %d, want 1", pr.Counts.Inserts)
	}
	if r.syncTokens["src-A"] != "tok-new" {
		t.Errorf("token did not advance")
	}
}

// ---------- Tick: 410 GONE clears token, signals NeedsFullResync ----------

func TestTick_410Gone_ClearsToken(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-stale"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	api.queueListIncrErr("src-A", &gws.Error{Code: gws.CodeAPIGone, ExitCode: 1})

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	if !res.PerSource["src-A"].NeedsFullResync {
		t.Errorf("NeedsFullResync should be true after 410")
	}
	if _, ok := r.syncTokens["src-A"]; ok {
		t.Errorf("token must be cleared after 410")
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("410 is recoverable; pdir should not be marked failed; got %v", pr.Err)
	}
}

// ---------- Tick: non-410 error fails dependent pdirs ----------

func TestTick_NonGoneError_PdirFails_TokenStays(t *testing.T) {
	api := newStubAPI()
	pdA := makeTestPDir("pA", "src-A", "tgt-A", true)
	pdB := makeTestPDir("pB", "src-A", "tgt-B", true)
	canonical := makeCanonical(pdA, pdB)

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.inventories["tgt-B"] = NewInventory("tgt-B")

	// 500 backend error - not 410, so SPEC says: leave token, mark pdirs failed.
	api.queueListIncrErr("src-A", &gws.Error{Code: gws.CodeBackendError, ExitCode: 1})

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	prA, _ := pdirByPair(res.PDirs, "pA")
	prB, _ := pdirByPair(res.PDirs, "pB")
	if prA.Err == nil || prB.Err == nil {
		t.Errorf("both pdirs sharing src-A should be marked failed")
	}
	if r.syncTokens["src-A"] != "tok-old" {
		t.Errorf("token should remain unchanged after non-410 error")
	}
	if res.PerSource["src-A"].NeedsFullResync {
		t.Errorf("non-410 error should NOT trigger NeedsFullResync")
	}
}

// ---------- Tick: two sources, one 410 + one success ----------

func TestTick_TwoSources_410PlusSuccess_Independent(t *testing.T) {
	api := newStubAPI()
	pdA := makeTestPDir("pA", "src-A", "tgt-A", true)
	pdB := makeTestPDir("pB", "src-B", "tgt-B", true)
	canonical := makeCanonical(pdA, pdB)

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-A-stale"
	r.syncTokens["src-B"] = "tok-B-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.inventories["tgt-B"] = NewInventory("tgt-B")

	api.queueListIncrErr("src-A", &gws.Error{Code: gws.CodeAPIGone, ExitCode: 1})
	api.queueListIncr("src-B", nil, "tok-B-new")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	if _, ok := r.syncTokens["src-A"]; ok {
		t.Errorf("src-A token should be cleared after 410")
	}
	if r.syncTokens["src-B"] != "tok-B-new" {
		t.Errorf("src-B token must advance independently after 410 on src-A")
	}
	if !res.PerSource["src-A"].NeedsFullResync {
		t.Errorf("src-A NeedsFullResync should be true")
	}
	if !res.PerSource["src-B"].SyncTokenChanged {
		t.Errorf("src-B SyncTokenChanged should be true")
	}
}

// ---------- Counts and aggregation ----------

func TestCounts_Observe_PerActionCounters(t *testing.T) {
	tests := []struct {
		action mirror.Action
		want   func(c Counts) bool
	}{
		{mirror.ActionInsert, func(c Counts) bool { return c.Inserts == 1 && c.EventsProcessed == 1 }},
		{mirror.ActionPatch, func(c Counts) bool { return c.Patches == 1 && c.EventsProcessed == 1 }},
		{mirror.ActionDelete, func(c Counts) bool { return c.Deletes == 1 && c.EventsProcessed == 1 }},
		{mirror.ActionPropagate, func(c Counts) bool { return c.Propagates == 1 && c.EventsProcessed == 1 }},
		{mirror.ActionRevert, func(c Counts) bool { return c.Reverts == 1 && c.EventsProcessed == 1 }},
		{mirror.ActionSkip, func(c Counts) bool { return c.Skips == 1 && c.EventsProcessed == 1 }},
	}
	for _, tc := range tests {
		t.Run(string(tc.action), func(t *testing.T) {
			var c Counts
			c.observe(Outcome{Action: tc.action})
			if !tc.want(c) {
				t.Errorf("after observe(%s): %+v", tc.action, c)
			}
		})
	}
}

func TestFullSync_AggregatedCountsSumPerPdir(t *testing.T) {
	api := newStubAPI()
	pdA := makeTestPDir("pA", "src-A", "tgt-A", true)
	pdB := makeTestPDir("pB", "src-B", "tgt-B", true)
	canonical := makeCanonical(pdA, pdB)

	srcA := makeNonRecurringSource("evt-A", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	srcB := makeNonRecurringSource("evt-B", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueListFull("src-A", []gws.Event{*srcA}, "tok-A")
	api.queueListFull("src-B", []gws.Event{*srcB}, "tok-B")
	queueEmptyInventory(api, "tgt-A")
	queueEmptyInventory(api, "tgt-B")
	postA := &gws.Event{ID: "m-A", Summary: srcA.Summary, Updated: "2026-04-29T20:00:01Z"}
	postB := &gws.Event{ID: "m-B", Summary: srcB.Summary, Updated: "2026-04-29T20:00:01Z"}
	api.queueInsert(postA)
	api.queuePatch(postA) // checksum
	api.queueInsert(postB)
	api.queuePatch(postB) // checksum

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	if res.Aggregated.Inserts != 2 || res.Aggregated.EventsProcessed != 2 {
		t.Errorf("Aggregated = %+v, want Inserts=2 EventsProcessed=2", res.Aggregated)
	}
}

// ---------- FullSync: source-list wire shape pinned ----------

func TestFullSync_SourceListWireShape(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)
	api.queueListFull("src-A", nil, "tok-1")
	queueEmptyInventory(api, "tgt-A")

	r := New(api, canonical,
		WithNow(fixedNow(must2026())),
		WithHorizon(7*24*time.Hour),
	)
	if _, err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync error: %v", err)
	}

	// Find the full source-list call (label "list:src-A:full"). It's the
	// only top-level events.list with no SyncToken and no
	// PrivateExtendedProperty.
	var found *recordedCall
	for i := range api.calls {
		c := &api.calls[i]
		if c.Op != "EventsList" {
			continue
		}
		if c.ListParams.SyncToken != "" {
			continue
		}
		if len(c.ListParams.PrivateExtendedProperty) > 0 {
			continue
		}
		if c.CalendarID == "src-A" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("no full source-list call recorded")
	}
	p := found.ListParams
	if p.MaxResults != MaxResultsPerPage {
		t.Errorf("MaxResults = %d, want %d", p.MaxResults, MaxResultsPerPage)
	}
	if p.ShowDeleted != true {
		t.Errorf("ShowDeleted = %v, want true", p.ShowDeleted)
	}
	if p.SingleEvents != false {
		t.Errorf("SingleEvents = %v, want false", p.SingleEvents)
	}
	if !reflect.DeepEqual(p.EventTypes, sourceListEventTypes) {
		t.Errorf("EventTypes = %v, want %v", p.EventTypes, sourceListEventTypes)
	}
	want := must2026()
	if p.TimeMin != want.Format(time.RFC3339) {
		t.Errorf("TimeMin = %q, want %q", p.TimeMin, want.Format(time.RFC3339))
	}
	if p.TimeMax != want.Add(7*24*time.Hour).Format(time.RFC3339) {
		t.Errorf("TimeMax = %q, want %q", p.TimeMax, want.Add(7*24*time.Hour).Format(time.RFC3339))
	}
}

// ---------- Tick: incremental wire shape pinned ----------

func TestTick_IncrementalWireShape(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)
	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-x"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	api.queueListIncr("src-A", nil, "tok-y")
	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick error: %v", err)
	}

	calls := api.callsByOp("EventsList")
	if len(calls) != 1 {
		t.Fatalf("expected 1 EventsList call; got %d", len(calls))
	}
	p := calls[0].ListParams
	if p.SyncToken != "tok-x" {
		t.Errorf("SyncToken = %q, want tok-x", p.SyncToken)
	}
	if p.MaxResults != MaxResultsPerPage {
		t.Errorf("MaxResults = %d, want %d", p.MaxResults, MaxResultsPerPage)
	}
	if !p.ShowDeleted {
		t.Errorf("ShowDeleted = false, want true")
	}
	if !reflect.DeepEqual(p.EventTypes, sourceListEventTypes) {
		t.Errorf("EventTypes = %v, want %v", p.EventTypes, sourceListEventTypes)
	}
	// Per SPEC's incremental wire shape (line 922), no TimeMin / TimeMax /
	// SingleEvents are sent. Verify they're zeroed (omitempty drops them
	// from the JSON marshal).
	if p.TimeMin != "" {
		t.Errorf("TimeMin = %q, want empty for incremental", p.TimeMin)
	}
	if p.TimeMax != "" {
		t.Errorf("TimeMax = %q, want empty for incremental", p.TimeMax)
	}
	if p.SingleEvents {
		t.Errorf("SingleEvents = true, want false for incremental")
	}
}

// ---------- FullSync: Recurring delegation routes through handler ----------

// TestFullSync_RecurringDelegation_RoutesToHandler ensures that when a
// source-list returns a recurring instance (RecurringEventID set), the
// reconciler's classifier delegates to the recurring.Handler. This pins
// the wiring of MirrorParentLookup + ParentReconciler done in
// buildClassifier. The scenario: an existing mirror parent in inventory,
// the recurring instance Handler.Handle runs, locates the mirror
// instance via events.instances, and patches it.
func TestFullSync_RecurringDelegation_RoutesToHandler(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	// Source: a recurring-instance exception.
	source := &gws.Event{
		ID:                "src-evt",
		Status:            gws.EventStatusConfirmed,
		Summary:           "Updated",
		Updated:           "2026-04-30T10:00:00Z",
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
		RecurringEventID:  "src-parent",
		OriginalStartTime: &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		Start:             &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		End:               &gws.EventDateTime{DateTime: "2026-05-01T14:00:00Z"},
		Transparency:      gws.TransparencyOpaque,
	}
	api.queueListFull("src-A", []gws.Event{*source}, "tok-1")

	// Inventory rebuild returns the mirror parent (so the handler's
	// step-1 LookupMirrorParent finds it without reconcileParent fallback)
	// AND the corresponding mirror instance. The mirror parent is keyed by
	// SOURCE-tuple (src-A:src-parent); the mirror instance by
	// (src-A:src-evt).
	mirrorParent := &gws.Event{
		ID:         "mp-1",
		Status:     gws.EventStatusConfirmed,
		Summary:    "Standup",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:  "src-A:src-parent",
				mirror.ExtKeyVersion: mirror.SchemaVersion,
			},
		},
	}
	api.queueListInventory("tgt-A", mirror.SchemaVersion, []gws.Event{*mirrorParent})
	api.queueListInventory("tgt-A", "1", nil)

	// Step 2: events.instances on the mirror parent returns the mirror
	// instance.
	mirrorInst := makeCleanV2Mirror("mi-1", "src-A:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T08:00:00Z",
		"Standup", source.Start, source.End,
	)
	api.queueInstances([]gws.Event{*mirrorInst})

	// Step 3 drift: source.Updated > stored source_updated, mirror clean -
	// patch + checksum followup.
	postMain := *mirrorInst
	postMain.Summary = "Updated"
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	// The orphan walk runs after classify on FullSync. The mirror parent
	// (src-A:src-parent) was NOT in the source-list (only the instance
	// was), so the walker will events.get it. Queue an alive recurring
	// parent that has an instance in horizon to satisfy the
	// alive-and-eligible cell (no delete).
	parentForOrphan := makeAliveSource("src-parent",
		&gws.EventDateTime{DateTime: "2025-01-01T12:00:00Z"})
	parentForOrphan.Recurrence = []string{"RRULE:FREQ=WEEKLY"}
	api.queueGet("src-A", "src-parent", parentForOrphan)
	api.queueInstances([]gws.Event{{ID: "any-instance"}})

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should succeed; got %v", pr.Err)
	}
	if pr.Counts.Patches != 1 {
		t.Errorf("expected 1 patch (recurring-instance delegation); got Counts=%+v", pr.Counts)
	}
}

// ---------- visited-set excludes mirrors that ARE returned by source-list ----------

// SPEC §"periodic full re-sync" step 5 only walks orphans (entries NOT in
// visited). When a source event maps to an existing mirror (i.e. the
// classify pass touches it), the orphan walk must NOT re-walk it. This
// test puts one mirror in inventory, has the source-list return that
// source event, and verifies the orphan walk doesn't issue any
// EventsGet (the visited filter blocked the entry).
func TestFullSync_OrphanWalk_SkipsVisitedEntries(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeNonRecurringSource("evt-1", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueListFull("src-A", []gws.Event{*src}, "tok-1")

	// Inventory has a mirror for that exact source. Classify should run a
	// patch (matching by source-tuple); orphan walk skips it.
	mirrorEv := makeCleanV2Mirror("m1", "src-A:evt-1",
		"2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z",
		src.Summary, src.Start, src.End)
	api.queueListInventory("tgt-A", mirror.SchemaVersion, []gws.Event{*mirrorEv})
	api.queueListInventory("tgt-A", "1", nil)
	// Classify is idempotent: source.Updated == stored, mirror clean -> skip.

	r := newTestReconciler(api, canonical)
	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should succeed; got %v", pr.Err)
	}
	if pr.Counts.Skips != 1 {
		t.Errorf("expected 1 skip (idempotent path); got %+v", pr.Counts)
	}
	// Orphan walk must NOT call events.get for the visited entry.
	if calls := api.callsByOp("EventsGet"); len(calls) != 0 {
		t.Errorf("orphan walk should skip visited entries; got %d EventsGet calls (%v)",
			len(calls), calls)
	}
}

// ---------- staged-but-empty-token: leave existing token unchanged ----------

// SPEC line 912: "If the staged token is missing... leave the in-memory
// token empty so the next cycle re-runs a full source-list." We exercise
// this on FullSync: the source-list returns events but no nextSyncToken.
// The reconciler must NOT clobber an existing token with empty (which
// would force a useless full re-sync next tick). Existing-token preserved.
//
// lastFullSync, however, IS stamped: a successful full source-list scan
// is what the timestamp tracks, regardless of whether Google returned a
// token. Gating the stamp on token advancement would leave a no-token
// source permanently stale and the daemon would loop FullSync forever.
func TestFullSync_EmptyStagedToken_DoesNotClobber(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)
	api.queueListFull("src-A", nil, "") // Google omitted nextSyncToken
	queueEmptyInventory(api, "tgt-A")

	stamp := must2026()
	r := New(api, canonical, WithNow(fixedNow(stamp)),
		WithHorizon(30*24*time.Hour), WithOrphanConcurrency(1))
	r.syncTokens["src-A"] = "preexisting-tok"

	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	if r.syncTokens["src-A"] != "preexisting-tok" {
		t.Errorf("token should not be clobbered by empty staged token; got %q",
			r.syncTokens["src-A"])
	}
	if res.PerSource["src-A"].SyncTokenChanged {
		t.Errorf("SyncTokenChanged should be false when staged token is empty")
	}
	got, ok := r.lastFullSync["src-A"]
	if !ok {
		t.Fatalf("lastFullSync should be stamped on a successful full source-list, even with no token")
	}
	if !got.Equal(stamp) {
		t.Errorf("lastFullSync = %v, want %v", got, stamp)
	}
}

// ---------- lastFullSync stamp updates only on FullSync ----------

func TestFullSync_SetsLastFullSyncTimestamp(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)
	api.queueListFull("src-A", nil, "tok-1")
	queueEmptyInventory(api, "tgt-A")

	stamp := must2026()
	r := New(api, canonical, WithNow(fixedNow(stamp)),
		WithHorizon(30*24*time.Hour), WithOrphanConcurrency(1))
	if _, err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	got, ok := r.lastFullSync["src-A"]
	if !ok {
		t.Fatalf("lastFullSync should be set after successful FullSync")
	}
	if !got.Equal(stamp) {
		t.Errorf("lastFullSync = %v, want %v", got, stamp)
	}
}

func TestTick_DoesNotUpdateLastFullSync(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	preStamp := must2026().Add(-time.Hour)
	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.lastFullSync["src-A"] = preStamp

	api.queueListIncr("src-A", nil, "tok-new")
	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	got := r.lastFullSync["src-A"]
	if !got.Equal(preStamp) {
		t.Errorf("lastFullSync was modified by Tick; got %v, want %v", got, preStamp)
	}
}

// ---------- FullSync error preserves prior lastFullSync ----------

func TestFullSync_ErrorPreservesPriorLastFullSync(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	preStamp := must2026().Add(-time.Hour)
	r := newTestReconciler(api, canonical)
	r.lastFullSync["src-A"] = preStamp

	api.queueListFullErr("src-A", errors.New("source list boom"))
	queueEmptyInventory(api, "tgt-A")

	if _, err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	if got := r.lastFullSync["src-A"]; !got.Equal(preStamp) {
		t.Errorf("lastFullSync clobbered after error; got %v, want %v", got, preStamp)
	}
}

// ---------- New() option wiring ----------

func TestNew_OptionsApply(t *testing.T) {
	now := func() time.Time { return must2026() }
	out := Output(func(_ Outcome) {})
	r := New(nil, &config.Canonical{},
		WithNow(now),
		WithHorizon(time.Hour),
		WithOrphanConcurrency(7),
		WithOutput(out),
	)
	if r.Now == nil {
		t.Errorf("Now option not applied")
	}
	if r.Horizon != time.Hour {
		t.Errorf("Horizon = %v, want 1h", r.Horizon)
	}
	if r.OrphanConcurrency != 7 {
		t.Errorf("OrphanConcurrency = %d, want 7", r.OrphanConcurrency)
	}
	if r.Output == nil {
		t.Errorf("Output option not applied")
	}
	if r.syncTokens == nil || r.inventories == nil || r.lastFullSync == nil {
		t.Errorf("New must initialize internal maps")
	}
}

// ---------- InventorySize accessor (used by layer 7's IPC status surface) ----------

func TestInventorySize_UnknownTargetReturnsZero(t *testing.T) {
	r := New(nil, &config.Canonical{})
	if got := r.InventorySize("nonexistent-target"); got != 0 {
		t.Errorf("InventorySize on missing target = %d, want 0", got)
	}
}

func TestInventorySize_ReflectsInventoryEntries(t *testing.T) {
	r := New(nil, &config.Canonical{})
	inv := NewInventory("tgt-A")
	inv.Set(mirror.SourceTuple{CalendarID: "src-A", EventID: "e1"},
		&gws.Event{ID: "m1"})
	inv.Set(mirror.SourceTuple{CalendarID: "src-A", EventID: "e2"},
		&gws.Event{ID: "m2"})
	r.inventories["tgt-A"] = inv

	if got := r.InventorySize("tgt-A"); got != 2 {
		t.Errorf("InventorySize = %d, want 2", got)
	}

	inv.Delete(mirror.SourceTuple{CalendarID: "src-A", EventID: "e1"})
	if got := r.InventorySize("tgt-A"); got != 1 {
		t.Errorf("InventorySize after delete = %d, want 1", got)
	}
}

// ---------- PropagateTargetEdits safety gate ----------

// buildClassifier ANDs PropagateTargetEdits with pd.SourceWritable so a
// freshly installed daemon never propagates mirror edits back to source.
// These tests pin the gate at the construction layer (the four-way matrix
// itself is tested in classify_test.go).
func TestBuildClassifier_GateOff_NeutralizesSourceWritable(t *testing.T) {
	r := New(nil, &config.Canonical{})
	r.PropagateTargetEdits = false
	pd := makeTestPDir("p1", "src-A", "tgt-A", true) // writable per accessRole
	c, _ := r.buildClassifier(pd, NewInventory("tgt-A"), &Counts{})
	if c.SourceWritable {
		t.Error("Classifier.SourceWritable must be false when gate is off, even with a writable source")
	}
	if c.Recurring.SourceWritable {
		t.Error("recurring.Handler.SourceWritable must be false when gate is off")
	}
}

func TestBuildClassifier_GateOn_PassesThroughSourceWritable(t *testing.T) {
	r := New(nil, &config.Canonical{})
	r.PropagateTargetEdits = true
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	c, _ := r.buildClassifier(pd, NewInventory("tgt-A"), &Counts{})
	if !c.SourceWritable {
		t.Error("Classifier.SourceWritable must be true when gate is on AND source is writable")
	}
	if !c.Recurring.SourceWritable {
		t.Error("recurring.Handler.SourceWritable must be true when gate is on AND source is writable")
	}
}

func TestBuildClassifier_GateOn_ReadOnlySourceStaysReadOnly(t *testing.T) {
	// Even with the gate flipped on, a source whose accessRole is below
	// writer can never be written to. The gate is a SUBSET, not an
	// override.
	r := New(nil, &config.Canonical{})
	r.PropagateTargetEdits = true
	pd := makeTestPDir("p1", "src-A", "tgt-A", false)
	c, _ := r.buildClassifier(pd, NewInventory("tgt-A"), &Counts{})
	if c.SourceWritable {
		t.Error("read-only source must stay read-only regardless of the gate")
	}
}

func TestWithPropagateTargetEdits_OptionApplies(t *testing.T) {
	r := New(nil, &config.Canonical{}, WithPropagateTargetEdits(true))
	if !r.PropagateTargetEdits {
		t.Error("WithPropagateTargetEdits(true) did not apply")
	}
}

// TestRunClassifyLoop_DedupesSourceTuple pins B2 Cause B
// (doc/dry-run-anomaly-analysis.md anomaly #1): when events.list returns
// the same source-tuple twice (typically because a `_R<timestamp>`-shaped
// recurring parent appears both as a top-level event and as a
// `recurring_event_id` on its child instances), the per-event classify
// loop must process it exactly once.
//
// The bug surfaces in dry-run as a bogus migration_source_won outcome on
// the second pass: the first call inserts the mirror with a broken
// extended-properties cache (Cause A), then the second call sees the
// broken cache and routes to the migration matrix.
//
// Production semantics aren't affected by Cause B alone (the second
// Classify on a real Calendar API would just emit `skip(unchanged)`),
// but the dedupe is a correctness safeguard regardless: a second pass
// on the same source-tuple is at best a wasted RTT and at worst (with
// the dryRunAPI defect) a misleading outcome.
//
// Behavior under the fix: only ONE outcome (the insert from the first
// occurrence). The duplicate is silently skipped - no `skip(reason=...)`
// emitted because SPEC's outcomes table doesn't define a reason for
// "duplicate within the same classify pass" and inventing one would
// surface as wire-format noise.
func TestRunClassifyLoop_DedupesSourceTuple(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	// Two copies of the same source event in the source-list response.
	// This is the production shape `_R<timestamp>` recurring parents
	// produce per the dry-run-anomaly-analysis.md doc.
	src := makeNonRecurringSource("evt-1", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueListFull("src-A", []gws.Event{*src, *src}, "token-1")
	queueEmptyInventory(api, "tgt-A")

	// Single insert + checksum patch from the first occurrence. We do NOT
	// queue a second insert; if the loop runs Classify twice for the same
	// source-tuple, the second call would consume the next queued response
	// (or fail if the queue is empty), and the assertion below catches it.
	insertedID := "cs2DEADBEEF"
	inserted := &gws.Event{ID: insertedID, Summary: src.Summary, Updated: "2026-04-29T20:00:01Z"}
	api.queueInsert(inserted)
	api.queuePatch(inserted)

	sink, captured := captureOutputs()
	r := newTestReconciler(api, canonical)
	r.Output = sink

	res, err := r.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir failed: %v", pr.Err)
	}
	// Exactly one outcome - the insert. The duplicate occurrence is
	// silently skipped at the top of the per-event loop.
	if len(*captured) != 1 {
		t.Errorf("got %d outcomes, want 1; outcomes=%+v", len(*captured), *captured)
	} else if (*captured)[0].Action != mirror.ActionInsert {
		t.Errorf("expected one insert outcome; got %+v", (*captured)[0])
	}
	if pr.Counts.Inserts != 1 {
		t.Errorf("Counts.Inserts = %d, want 1", pr.Counts.Inserts)
	}
	if pr.Counts.EventsProcessed != 1 {
		t.Errorf("Counts.EventsProcessed = %d, want 1 "+
			"(dedupe must NOT count the duplicate as processed)",
			pr.Counts.EventsProcessed)
	}
}
