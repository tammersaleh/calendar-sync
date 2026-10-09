package sync

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// makeMirrorWithSource builds a mirror Event with the calendar-sync extended
// properties enough for inventory parsing to succeed. Tests that need the
// full payload (checksum, source_updated, etc.) build mirrors directly.
func makeMirrorWithSource(id, source string, version string) *gws.Event {
	return &gws.Event{
		ID:     id,
		Status: gws.EventStatusConfirmed,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:  source,
				mirror.ExtKeyVersion: version,
			},
		},
	}
}

func TestBuildInventory_CurrentVersionOnly(t *testing.T) {
	api := newStubAPI()
	m1 := makeMirrorWithSource("m1", "src-cal:src-evt-A", mirror.SchemaVersion)
	m2 := makeMirrorWithSource("m2", "src-cal:src-evt-B", mirror.SchemaVersion)
	api.queueList([]gws.Event{*m1, *m2}, "")
	api.queueList(nil, "") // no v2 legacy mirrors
	api.queueList(nil, "") // no v1 legacy mirrors

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-A"}); !ok || got.ID != "m1" {
		t.Errorf("expected m1 indexed by src-cal:src-evt-A; got=%v ok=%v", got, ok)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-B"}); !ok || got.ID != "m2" {
		t.Errorf("expected m2 indexed by src-cal:src-evt-B; got=%v ok=%v", got, ok)
	}
	// One events.list call per known schema version: current first, then
	// each legacy version still in the wild. The order is current -> "2"
	// -> "1" so a re-bump in the future stays self-documenting.
	listCalls := api.callsByOp("EventsList")
	if len(listCalls) != 3 {
		t.Fatalf("expected 3 EventsList calls (current + 2 legacy); got %d", len(listCalls))
	}
	wantFilters := [][]string{
		{mirror.ExtKeyVersion + "=" + mirror.SchemaVersion},
		{mirror.ExtKeyVersion + "=2"},
		{mirror.ExtKeyVersion + "=1"},
	}
	for i, want := range wantFilters {
		if !reflect.DeepEqual(listCalls[i].ListParams.PrivateExtendedProperty, want) {
			t.Errorf("list[%d] filter = %v, want %v", i, listCalls[i].ListParams.PrivateExtendedProperty, want)
		}
		if !listCalls[i].ListParams.ShowDeleted {
			t.Errorf("list[%d] must request showDeleted=true", i)
		}
	}
}

func TestBuildInventory_V1Only(t *testing.T) {
	api := newStubAPI()
	api.queueList(nil, "") // current empty
	api.queueList(nil, "") // v=2 empty
	v1 := makeMirrorWithSource("v1mirror", "src-cal:src-evt-X", "1")
	api.queueList([]gws.Event{*v1}, "")

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-X"}); !ok || got.ID != "v1mirror" {
		t.Errorf("expected v1 mirror indexed; got=%v ok=%v", got, ok)
	}
}

func TestBuildInventory_V2Only(t *testing.T) {
	// v2 mirrors lack the location field but still carry a checksum + source
	// tuple. They get indexed and are routed through the migration path at
	// reconciliation time.
	api := newStubAPI()
	api.queueList(nil, "") // current empty
	v2 := makeMirrorWithSource("v2mirror", "src-cal:src-evt-Y", "2")
	api.queueList([]gws.Event{*v2}, "")
	api.queueList(nil, "") // v1 empty

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-Y"}); !ok || got.ID != "v2mirror" {
		t.Errorf("expected v2 mirror indexed; got=%v ok=%v", got, ok)
	}
}

func TestBuildInventory_MixedCurrentAndLegacy(t *testing.T) {
	api := newStubAPI()
	cur := makeMirrorWithSource("mcur", "src-cal:evtA", mirror.SchemaVersion)
	api.queueList([]gws.Event{*cur}, "")
	v2 := makeMirrorWithSource("mv2", "src-cal:evtB", "2")
	api.queueList([]gws.Event{*v2}, "")
	v1 := makeMirrorWithSource("mv1", "src-cal:evtC", "1")
	api.queueList([]gws.Event{*v1}, "")

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "evtA"}); !ok {
		t.Errorf("current-version mirror missing")
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "evtB"}); !ok {
		t.Errorf("v2 mirror missing")
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "evtC"}); !ok {
		t.Errorf("v1 mirror missing")
	}
}

func TestBuildInventory_ListErrorPropagates(t *testing.T) {
	api := newStubAPI()
	api.queueListErr(errors.New("calendar API kaboom"))

	_, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err == nil {
		t.Fatal("expected error from EventsList failure")
	}
}

func TestBuildInventory_SkipsMirrorsMissingSourceTuple(t *testing.T) {
	// Mirrors with no source tuple, malformed source tuple, or no extended
	// properties at all are silently dropped from the inventory.
	api := newStubAPI()
	missing := &gws.Event{ID: "no-ext", Status: gws.EventStatusConfirmed}
	emptySource := &gws.Event{
		ID: "empty-src",
		ExtendedProperties: &gws.ExtendedProperties{Private: map[string]string{
			mirror.ExtKeyVersion: mirror.SchemaVersion,
		}},
	}
	malformed := &gws.Event{
		ID: "malformed",
		ExtendedProperties: &gws.ExtendedProperties{Private: map[string]string{
			mirror.ExtKeySource:  "no-colon-here",
			mirror.ExtKeyVersion: mirror.SchemaVersion,
		}},
	}
	good := makeMirrorWithSource("good", "src-cal:src-evt-A", mirror.SchemaVersion)
	api.queueList([]gws.Event{*missing, *emptySource, *malformed, *good}, "")
	api.queueList(nil, "") // v=2 empty
	api.queueList(nil, "") // v=1 empty

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if len(inv.All()) != 1 {
		t.Errorf("expected only the parseable mirror to be indexed; got %d entries", len(inv.All()))
	}
}

// TestBuildInventory_SkipsCancelledTombstones pins B11: mirrors with
// status=cancelled (Calendar API tombstones from a previous events.delete)
// must not enter the inventory. They reach the listing because we pass
// ShowDeleted:true to surface them for the cancelled-and-revived path,
// but indexing them here would have downstream effects: the orphan walk
// would try to delete them again (Calendar API responds with
// api_invalid_request "Resource has been deleted", which the walker
// surfaces as a partial_failure), and the standard reconcile path
// would treat them as live mirrors needing drift checks.
//
// SPEC's "Cancelled-and-revived" flow doesn't depend on the inventory
// holding tombstones - revival is triggered by 409 on insert, then a
// per-event events.get to inspect status.
func TestBuildInventory_SkipsCancelledTombstones(t *testing.T) {
	api := newStubAPI()
	live := makeMirrorWithSource("live", "src-cal:src-evt-A", mirror.SchemaVersion)
	tomb := makeMirrorWithSource("tomb", "src-cal:src-evt-B", mirror.SchemaVersion)
	tomb.Status = gws.EventStatusCancelled
	api.queueList([]gws.Event{*live, *tomb}, "")
	api.queueList(nil, "") // v=2 empty
	api.queueList(nil, "") // v=1 empty

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if got := len(inv.All()); got != 1 {
		t.Errorf("expected only the live mirror to be indexed; got %d entries", got)
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-B"}); ok {
		t.Errorf("cancelled tombstone src-evt-B must NOT be in inventory")
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-A"}); !ok {
		t.Errorf("live mirror src-evt-A must remain in inventory")
	}
}

// TestBuildInventory_InheritedInstanceDoesNotOverwriteParent pins the fix for
// B16: when calendar-sync writes a recurring parent mirror, Google
// auto-materializes its instances and copies the parent's
// extendedProperties.private to each instance verbatim. So an instance the
// user has overridden on the target lands in events.list with
// `calendar-sync:source` pointing at the source PARENT - the same value the
// real parent mirror carries. Both events parse to the same source-tuple key
// and one would overwrite the other in inventory. Last-writer-wins lets the
// instance's drift state masquerade as the parent's, which produces a
// catastrophic propagate that rewrites the source parent with the
// instance's per-occurrence start/end/recurrence.
//
// The fix builds a parent-id -> source-tuple map in a first pass, then in a
// second pass uses mirror.IsInheritedRecurringInstance to skip any instance
// whose parsed source-tuple matches its mirror parent's source-tuple.
func TestBuildInventory_InheritedInstanceDoesNotOverwriteParent(t *testing.T) {
	api := newStubAPI()
	parent := makeMirrorWithSource("mp1", "src-cal:src-parent-1", mirror.SchemaVersion)
	// Auto-materialized instance the user has overridden on the target. Google
	// echoes back the inherited extended properties exactly.
	inherited := makeMirrorWithSource("mp1_20260520T183000Z", "src-cal:src-parent-1", mirror.SchemaVersion)
	inherited.RecurringEventID = "mp1"
	api.queueList([]gws.Event{*parent, *inherited}, "")
	api.queueList(nil, "") // v=2 empty
	api.queueList(nil, "") // v=1 empty

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent-1"})
	if !ok {
		t.Fatal("parent missing from inventory at its source-tuple key")
	}
	if got.ID != "mp1" {
		t.Errorf("inventory at parent's source-tuple = %q, want %q (the parent ID); inherited instance must not overwrite parent",
			got.ID, "mp1")
	}
	// Also try the reverse list order - the bug surfaces on iteration order.
	api2 := newStubAPI()
	api2.queueList([]gws.Event{*inherited, *parent}, "")
	api2.queueList(nil, "")
	api2.queueList(nil, "")
	inv2, err := BuildInventory(context.Background(), api2, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error (reverse order): %v", err)
	}
	got2, ok := inv2.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent-1"})
	if !ok || got2.ID != "mp1" {
		t.Errorf("reverse-order inventory at parent's tuple: got=%v ok=%v, want parent mp1", got2, ok)
	}
	// B40: an inherited instance is filtered BEFORE duplicate detection, so
	// it must never be recorded as a duplicate of its parent.
	for name, i := range map[string]*Inventory{"forward": inv, "reverse": inv2} {
		if d := i.duplicates; len(d) != 0 {
			t.Errorf("%s: inherited instance must not be recorded as a duplicate; got %v", name, d)
		}
	}
}

// TestBuildInventory_ManagedInstanceStillIndexed pins that the inherited-
// instance filter does NOT skip explicitly-managed instances. A managed
// instance carries its own per-instance source-tuple (EventID = source
// instance ID with the `_<UTC>` suffix), which is different from the parent's
// source-tuple, so there's no collision and no reason to drop it.
func TestBuildInventory_ManagedInstanceStillIndexed(t *testing.T) {
	api := newStubAPI()
	parent := makeMirrorWithSource("mp1", "src-cal:src-parent-1", mirror.SchemaVersion)
	managed := makeMirrorWithSource("mp1_20260520T183000Z",
		"src-cal:src-parent-1_20260520T183000Z", // explicit per-instance source
		mirror.SchemaVersion)
	managed.RecurringEventID = "mp1"
	api.queueList([]gws.Event{*parent, *managed}, "")
	api.queueList(nil, "")
	api.queueList(nil, "")

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent-1"}); !ok || got.ID != "mp1" {
		t.Errorf("parent missing or wrong; got=%v ok=%v", got, ok)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent-1_20260520T183000Z"}); !ok || got.ID != "mp1_20260520T183000Z" {
		t.Errorf("managed instance missing from inventory; got=%v ok=%v", got, ok)
	}
}

// TestBuildInventory_OrphanInheritedInstance covers the edge case where the
// parent isn't in the listing (e.g. a partial response, or the parent has
// somehow been removed without the instance being cancelled). With the
// parent absent, IsInheritedRecurringInstance can't fire (no parent map
// entry), so the instance falls through and is indexed. That keeps the
// inventory non-empty even in pathological data states - the orphan walk
// will discover the orphan and clean up. The test pins that we don't
// silently drop entries when we can't prove inheritance.
func TestBuildInventory_OrphanInheritedInstance(t *testing.T) {
	api := newStubAPI()
	orphanInstance := makeMirrorWithSource("mp_missing_20260520T183000Z",
		"src-cal:src-parent-missing",
		mirror.SchemaVersion)
	orphanInstance.RecurringEventID = "mp_missing"
	api.queueList([]gws.Event{*orphanInstance}, "")
	api.queueList(nil, "")
	api.queueList(nil, "")

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent-missing"}); !ok {
		t.Error("orphan instance with no parent in listing should still be indexed (orphan walk cleans up later)")
	}
}

func TestInventory_LookupSetDelete(t *testing.T) {
	inv := NewInventory("tgt-cal")
	if inv.Target() != "tgt-cal" {
		t.Errorf("Target = %q, want tgt-cal", inv.Target())
	}
	tuple := mirror.SourceTuple{CalendarID: "s", EventID: "e"}
	if _, ok := inv.Lookup(tuple); ok {
		t.Errorf("empty inventory shouldn't have %v", tuple)
	}
	e := &gws.Event{ID: "m1"}
	inv.Set(tuple, e)
	got, ok := inv.Lookup(tuple)
	if !ok || got.ID != "m1" {
		t.Errorf("Lookup after Set: got=%v ok=%v", got, ok)
	}
	inv.Delete(tuple)
	if _, ok := inv.Lookup(tuple); ok {
		t.Errorf("Delete didn't remove %v", tuple)
	}
}

func TestInventory_TuplesSortedAndAllAligned(t *testing.T) {
	inv := NewInventory("tgt-cal")
	tuples := []mirror.SourceTuple{
		{CalendarID: "calB", EventID: "ev2"},
		{CalendarID: "calA", EventID: "ev1"},
		{CalendarID: "calA", EventID: "ev0"},
	}
	for i, t0 := range tuples {
		inv.Set(t0, &gws.Event{ID: tuples[i].CalendarID + "-" + tuples[i].EventID})
	}
	got := inv.Tuples()
	want := []mirror.SourceTuple{
		{CalendarID: "calA", EventID: "ev0"},
		{CalendarID: "calA", EventID: "ev1"},
		{CalendarID: "calB", EventID: "ev2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tuples = %v, want %v", got, want)
	}
	all := inv.All()
	if len(all) != 3 {
		t.Fatalf("All length = %d, want 3", len(all))
	}
	// All must align with Tuples ordering.
	for i, ev := range all {
		expected := want[i].CalendarID + "-" + want[i].EventID
		if ev.ID != expected {
			t.Errorf("All[%d].ID = %q, want %q", i, ev.ID, expected)
		}
	}
}

// TestBuildInventory_DuplicateMirrors_WinnerSelection pins B40: two live
// mirrors carrying the same source-tuple must not silently last-writer-win.
// Newest parseable Updated wins; the deterministic ID breaks a tie; then
// the lexically smaller ID. Losers are recorded in the duplicates map so the orphan
// walk can delete them. Each case runs in both list orders because the
// pre-fix bug was order-dependent.
func TestBuildInventory_DuplicateMirrors_WinnerSelection(t *testing.T) {
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	detID := mirror.DeterministicID("src-cal", "src-evt")
	mk := func(id, updated string) *gws.Event {
		m := makeMirrorWithSource(id, tuple.String(), mirror.SchemaVersion)
		m.Updated = updated
		return m
	}
	cases := []struct {
		name       string
		a, b       *gws.Event
		wantWinner string
	}{
		{"newer random beats older deterministic", mk(detID, "2026-10-05T17:59:51.061Z"), mk("random1", "2026-10-06T05:07:46.229Z"), "random1"},
		{"newer deterministic beats older random", mk(detID, "2026-10-06T05:07:46.229Z"), mk("random1", "2026-10-05T17:59:51.061Z"), detID},
		// As strings "46.5Z" < "46Z" ('.' sorts before 'Z'); as times .5 is
		// newer. Lexical ID would pick aa. Only a real time compare picks zz.
		{"fractional seconds compared as time, not string", mk("zz", "2026-10-06T05:07:46.5Z"), mk("aa", "2026-10-06T05:07:46Z"), "zz"},
		// 05:00-07:00 is 12:00Z, later than 11:00Z, though the string is smaller.
		{"offsets compared as time, not string", mk("zz", "2026-10-06T05:00:00-07:00"), mk("aa", "2026-10-06T11:00:00Z"), "zz"},
		{"unparseable updated counts as oldest", mk("aa", "garbage"), mk("zz", "2026-01-01T00:00:00Z"), "zz"},
		{"empty updated counts as oldest", mk("aa", ""), mk("zz", "2026-01-01T00:00:00Z"), "zz"},
		{"equal updated: deterministic wins", mk("aaaa", "2026-10-06T05:07:46Z"), mk(detID, "2026-10-06T05:07:46Z"), detID},
		{"equal updated, neither deterministic: lexical", mk("zzz", "2026-10-06T05:07:46Z"), mk("aaa", "2026-10-06T05:07:46Z"), "aaa"},
	}
	for _, tc := range cases {
		for _, order := range [][]gws.Event{{*tc.a, *tc.b}, {*tc.b, *tc.a}} {
			api := newStubAPI()
			api.queueList(order, "")
			api.queueList(nil, "")
			api.queueList(nil, "")
			inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
			if err != nil {
				t.Fatalf("%s: BuildInventory error: %v", tc.name, err)
			}
			got, ok := inv.Lookup(tuple)
			if !ok || got.ID != tc.wantWinner {
				t.Errorf("%s (order %s,%s): winner = %v, want %s", tc.name, order[0].ID, order[1].ID, got, tc.wantWinner)
			}
			wantLoser := tc.a.ID
			if wantLoser == tc.wantWinner {
				wantLoser = tc.b.ID
			}
			dups := inv.duplicates
			if len(dups) != 1 || len(dups[tuple]) != 1 || dups[tuple][0].ID != wantLoser {
				t.Errorf("%s (order %s,%s): duplicates = %v, want exactly [%s] under %s", tc.name, order[0].ID, order[1].ID, dups, wantLoser, tuple)
			}
		}
	}
}

// TestBuildInventory_ThreeWayDuplicate pins that the winner is the global
// best across all copies, not just the last pairwise comparison, that both
// losers are recorded, and that the log reports the FINAL winner once per
// tuple (not an intermediate pairwise winner) plus the duplicate count on
// the complete line.
func TestBuildInventory_ThreeWayDuplicate(t *testing.T) {
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	mk := func(id, updated string) gws.Event {
		m := makeMirrorWithSource(id, tuple.String(), mirror.SchemaVersion)
		m.Updated = updated
		return *m
	}
	api := newStubAPI()
	api.queueList([]gws.Event{mk("mid", "2026-02-01T00:00:00Z"), mk("newest", "2026-03-01T00:00:00Z"), mk("oldest", "2026-01-01T00:00:00Z")}, "")
	api.queueList(nil, "")
	api.queueList(nil, "")
	logger := &captureLogger{}
	inv, err := BuildInventory(context.Background(), api, "tgt-cal", logger)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := inv.Lookup(tuple); got == nil || got.ID != "newest" {
		t.Errorf("winner = %v, want newest", got)
	}
	losers := inv.duplicates[tuple]
	ids := []string{}
	for _, l := range losers {
		ids = append(ids, l.ID)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"mid", "oldest"}) {
		t.Errorf("losers = %v, want [mid oldest]", ids)
	}

	if len(logger.warns) != 1 {
		t.Fatalf("want exactly one duplicate warning; got %d: %v", len(logger.warns), logger.warns)
	}
	w := logger.warns[0]
	if w["msg"] != "sync.BuildInventory: duplicate mirrors for source tuple" ||
		w["target"] != "tgt-cal" || w["source_tuple"] != tuple.String() ||
		w["winner"] != "newest" || w["losers"] != "mid,oldest" {
		t.Errorf("warning = %v, want final winner newest and losers mid,oldest", w)
	}
	var complete map[string]any
	for _, e := range logger.infos {
		if e["msg"] == "sync.BuildInventory complete" {
			complete = e
		}
	}
	if complete == nil || complete["duplicate_tuples"] != 1 || complete["total_mirrors"] != 1 {
		t.Errorf("complete line = %v, want duplicate_tuples=1 total_mirrors=1", complete)
	}
}

// TestBuildInventory_SameIDTwiceIsNotADuplicate pins the guard against the
// same Google event ID appearing twice in the merged per-version lists:
// it is one mirror, not a collision, so recording it as its own loser
// would make the orphan walk delete the live mirror.
func TestBuildInventory_SameIDTwiceIsNotADuplicate(t *testing.T) {
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	m := makeMirrorWithSource("only", tuple.String(), mirror.SchemaVersion)
	api := newStubAPI()
	api.queueList([]gws.Event{*m}, "")
	api.queueList([]gws.Event{*m}, "")
	api.queueList(nil, "")
	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := inv.Lookup(tuple); !ok || got.ID != "only" {
		t.Errorf("winner = %v ok=%v, want only", got, ok)
	}
	if d := inv.duplicates; len(d) != 0 {
		t.Errorf("same ID twice must not be a duplicate; got %v", d)
	}
}

// TestBuildInventory_DuplicateParents_InstanceFollowsSurvivingParent pins
// the B40 hierarchy rule: when two recurring mirror parents collide AND
// their managed instances for the same occurrence collide, the instance
// under the SURVIVING parent must win even when the instance under the
// losing parent is newer. Otherwise the orphan walk deletes the losing
// parent (cascading to its instances, including the "winning" one) and
// then deletes the surviving parent's instance as the recorded loser,
// leaving no mirror occurrence at all. Both list orders.
func TestBuildInventory_DuplicateParents_InstanceFollowsSurvivingParent(t *testing.T) {
	parentTuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "sp"}
	instTuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "sp_20261013T154500Z"}
	mk := func(id, parent, tuple, updated string) gws.Event {
		m := makeMirrorWithSource(id, tuple, mirror.SchemaVersion)
		m.RecurringEventID = parent
		m.Updated = updated
		return *m
	}
	p1 := mk("P1", "", parentTuple.String(), "2026-10-06T00:00:00Z") // parent winner (newer)
	p2 := mk("P2", "", parentTuple.String(), "2026-10-01T00:00:00Z")
	p1x := mk("P1_20261013T154500Z", "P1", instTuple.String(), "2026-10-01T00:00:00Z") // older, but under the survivor
	p2x := mk("P2_20261013T154500Z", "P2", instTuple.String(), "2026-10-09T00:00:00Z") // newer, under the loser

	orders := [][]gws.Event{{p1, p1x, p2, p2x}, {p2x, p2, p1x, p1}, {p1x, p2x, p1, p2}}
	for _, order := range orders {
		api := newStubAPI()
		api.queueList(order, "")
		api.queueList(nil, "")
		api.queueList(nil, "")
		inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := inv.Lookup(parentTuple); got == nil || got.ID != "P1" {
			t.Errorf("parent winner = %v, want P1", got)
		}
		if got, _ := inv.Lookup(instTuple); got == nil || got.ID != "P1_20261013T154500Z" {
			t.Errorf("instance winner = %v, want P1's instance (under the surviving parent) despite being older", got)
		}
		if d := inv.duplicates[parentTuple]; len(d) != 1 || d[0].ID != "P2" {
			t.Errorf("parent losers = %v, want [P2]", d)
		}
		if d := inv.duplicates[instTuple]; len(d) != 1 || d[0].ID != "P2_20261013T154500Z" {
			t.Errorf("instance losers = %v, want [P2's instance]", d)
		}

		// The orphan walk must then never delete the surviving parent's
		// instance.
		sink, _ := captureOutputs()
		w := newOrphanWalker(t, api, inv, sink, orphanOptions{})
		if err := w.Walk(context.Background(), map[mirror.SourceTuple]bool{parentTuple: true, instTuple: true}); err != nil {
			t.Fatal(err)
		}
		deleted := map[string]bool{}
		for _, c := range api.callsByOp("EventsDelete") {
			deleted[c.EventID] = true
		}
		if !deleted["P2"] || !deleted["P2_20261013T154500Z"] || deleted["P1"] || deleted["P1_20261013T154500Z"] || len(deleted) != 2 {
			t.Errorf("deleted = %v, want exactly P2 and P2's instance", deleted)
		}
	}
}
