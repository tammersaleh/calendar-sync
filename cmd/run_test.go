package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
)

func TestRunCmd_EmptySourceListEmitsMetaOnly(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	path := writeConfigFixture(t, validConfigTOML)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&RunCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want only meta line, got %d:\n%s", len(lines), stdout.String())
	}
	var meta struct {
		Meta runMetaPayload `json:"_meta"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.Meta.PDirs != 1 {
		t.Errorf("pdirs = %d, want 1 (every pair is one pdir post-v2.0.0)", meta.Meta.PDirs)
	}
}

func TestRunCmd_DaemonRunningReturnsCode5(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	sockPath := filepath.Join(tmp, "calendar-sync.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	path := writeConfigFixture(t, validConfigTOML)
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	err = (&RunCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected daemon_already_running")
	}
	code, _, _ := MapError(err)
	if code != "daemon_already_running" {
		t.Errorf("code = %q, want daemon_already_running", code)
	}
}

func TestRunCmd_PairFilterNoMatchMapsToPairNotFound(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	path := writeConfigFixture(t, validConfigTOML)
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	err := (&RunCmd{Pair: []string{"nope"}}).Run(rt)
	if err == nil {
		t.Fatalf("expected pair_not_found")
	}
	code, _, _ := MapError(err)
	if code != "pair_not_found" {
		t.Errorf("code = %q, want pair_not_found", code)
	}
}

// TestRunCmd_PartialFailureCollectsAllAndEmitsFailuresList exercises SPEC
// §"Partial failure semantics" (lines 1287-1303): every pdir runs even if
// one fails; final exit is partial_failure with `_meta.failures` listing
// the failed `<pair>:<direction>` identifiers.
//
// We trigger the single-pair fixture's lone pdir to fail by failing the
// inventory-rebuild for the target calendar. The inventory-rebuild call
// is distinguishable from the source-list call by the
// PrivateExtendedProperty filter (calendar-sync:version=...).
// validConfigTOML is a one-pair fixture, so this is exactly one failure
// out of one pdir; partial_failure still fires and the failures list
// carries the lone "work-personal:a_to_b" identifier.
func TestRunCmd_PartialFailureCollectsAllAndEmitsFailuresList(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	path := writeConfigFixture(t, validConfigTOML)

	stub := &failingInventoryGws{
		stubGws: stubGws{},
		failInventoryFor: map[string]error{
			"personal@example.com": errStubEventsList,
		},
	}
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     stub,
	}
	err := (&RunCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected partial_failure error")
	}
	code, _, _ := MapError(err)
	if code != "partial_failure" {
		t.Errorf("code = %q, want partial_failure", code)
	}

	// _meta line should carry failures. The Run wrapper always prints meta,
	// even on the partial-failure path.
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("expected at least the _meta line, got: %q", stdout.String())
	}
	last := lines[len(lines)-1]
	var meta struct {
		Meta runMetaPayload `json:"_meta"`
	}
	if err := json.Unmarshal([]byte(last), &meta); err != nil {
		t.Fatalf("unmarshal _meta line %q: %v", last, err)
	}
	if len(meta.Meta.Failures) != 1 {
		t.Fatalf("failures = %v, want exactly 1 entry", meta.Meta.Failures)
	}
	if meta.Meta.Failures[0] != "work-personal:a_to_b" {
		t.Errorf("failures[0] = %q, want work-personal:a_to_b", meta.Meta.Failures[0])
	}
}

// TestRunCmd_PartialFailureSurfacesUnderlyingErrorViaHandleErr:
// Anomaly #2 fix. A partial_failure exit must include the underlying gws
// error in the stderr ErrorEnvelope.Cause field; otherwise an operator
// sees only "1 pdir(s) failed: foo:a_to_b" with no clue what failed.
//
// The Run-method test path (TestRunCmd_PartialFailure...) only asserts on
// the cmdError MapError surface. This test drives the full handleErr path
// to verify the JSON envelope on stderr carries the cause.
func TestRunCmd_PartialFailureSurfacesUnderlyingErrorViaHandleErr(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	path := writeConfigFixture(t, validConfigTOML)

	stub := &failingInventoryGws{
		stubGws: stubGws{},
		failInventoryFor: map[string]error{
			"personal@example.com": errStubEventsList,
		},
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  stderr,
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     stub,
	}
	err := (&RunCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected partial_failure error")
	}
	// Drive handleErr to populate stderr.
	exitCode := handleErr(stderr, err)
	if exitCode == 0 {
		t.Fatalf("handleErr returned 0; expected non-zero")
	}

	var envelope struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
		Cause  string `json:"cause"`
	}
	last := strings.TrimSpace(stderr.String())
	if err := json.Unmarshal([]byte(last), &envelope); err != nil {
		t.Fatalf("unmarshal stderr %q: %v", last, err)
	}
	if envelope.Error != "partial_failure" {
		t.Errorf("error = %q, want partial_failure", envelope.Error)
	}
	if envelope.Cause == "" {
		t.Errorf("cause must be populated so operators can see WHY a pdir failed; got empty. envelope=%+v", envelope)
	}
	if !strings.Contains(envelope.Cause, "EventsList") {
		t.Errorf("cause = %q, want it to mention the underlying gws operation", envelope.Cause)
	}
}

// TestRunCmd_AllPDirsFailEmitsAllFailures: when every pdir fails, the
// failures list mirrors the full pdir list. Ensures the loop never
// short-circuits on the first failure. Uses a local 2-pair fixture so
// the test still has more than one pdir post-v2.0.0 (validConfigTOML
// declares only one pair = one pdir).
func TestRunCmd_AllPDirsFailEmitsAllFailures(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	const twoPairTOML = `
[settings]
poll_interval      = "60s"
horizon            = "365d"
full_sync_interval = "24h"
log_level          = "info"
log_format         = "json"

[[pairs]]
name      = "work-personal"
source    = "work@example.com"
target    = "personal@example.com"

[[pairs]]
name      = "personal-work"
source    = "personal@example.com"
target    = "work@example.com"
`
	path := writeConfigFixture(t, twoPairTOML)

	// Fail every inventory rebuild → both pdirs fail.
	stub := &failingInventoryGws{
		stubGws: stubGws{},
		failInventoryFor: map[string]error{
			"work@example.com":     errStubEventsList,
			"personal@example.com": errStubEventsList,
		},
	}
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     stub,
	}
	err := (&RunCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected partial_failure error")
	}
	code, _, _ := MapError(err)
	if code != "partial_failure" {
		t.Errorf("code = %q, want partial_failure", code)
	}

	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	last := lines[len(lines)-1]
	var meta struct {
		Meta runMetaPayload `json:"_meta"`
	}
	if err := json.Unmarshal([]byte(last), &meta); err != nil {
		t.Fatalf("unmarshal _meta line %q: %v", last, err)
	}
	if len(meta.Meta.Failures) != 2 {
		t.Fatalf("failures = %v, want exactly 2 entries (both pdirs failed)", meta.Meta.Failures)
	}
	// Pin the specific identifiers, not just the count: both pdirs must
	// surface in the failures list, in the SPEC `<pair>:<direction>`
	// shape. Order is unspecified; use slices.Contains.
	for _, want := range []string{"work-personal:a_to_b", "personal-work:a_to_b"} {
		if !slices.Contains(meta.Meta.Failures, want) {
			t.Errorf("failures missing %q; got %v", want, meta.Meta.Failures)
		}
	}
}

// errStubEventsList is the canned error failingInventoryGws raises.
var errStubEventsList = stubError("EventsList: test failure")

type stubError string

func (s stubError) Error() string { return string(s) }

// failingInventoryGws extends stubGws with per-target inventory-rebuild
// errors. Only EventsList calls that carry the PrivateExtendedProperty
// filter (i.e., the inventory rebuild path - not the source-list path)
// are intercepted, so source-list calls for the same calendar stay
// successful.
type failingInventoryGws struct {
	stubGws
	failInventoryFor map[string]error
}

func (f *failingInventoryGws) EventsList(ctx context.Context, params gws.EventsListParams) ([]gws.Event, string, error) {
	if len(params.PrivateExtendedProperty) > 0 {
		if err, ok := f.failInventoryFor[params.CalendarID]; ok {
			return nil, "", err
		}
	}
	return f.stubGws.EventsList(ctx, params)
}

func TestDryRunAPI_EventsInsertSuppressesWrite(t *testing.T) {
	called := 0
	inner := &countingGws{stubGws: stubGws{}, insertCount: &called}
	api := newDryRunAPI(inner)
	body := &gws.Event{ID: "abc"}
	got, err := api.EventsInsert(context.Background(), "cal", body)
	if err != nil {
		t.Fatalf("EventsInsert: %v", err)
	}
	if called != 0 {
		t.Errorf("inner.EventsInsert was called %d times, want 0", called)
	}
	if got == nil || got.ID != "abc" {
		t.Errorf("got = %+v, want event with ID abc", got)
	}
}

// TestDryRunAPI_PatchMergesIntoCachedInsertResource pins B2 cause A
// (doc/dry-run-anomaly-analysis.md anomaly #1). The dryRunAPI must
// remember what each EventsInsert echoed back so a subsequent
// EventsPatch returns the merged result, matching production's
// JSON Merge Patch semantics.
//
// Before the fix, EventsPatch returned only the request body. The
// follow-up checksum patch sends `{extendedProperties.private:
// {checksum: ...}}` ONLY, so the cached inventory entry lost
// calendar-sync:version + calendar-sync:source from the prior insert.
// On the next Classify pass for the same source-tuple,
// ComputeDriftSignal saw a missing version and reported
// NeedsMigration=true - bogus migration_source_won outcomes.
//
// After the fix:
//   - EventsInsert caches a copy of the echoed body keyed by
//     (calendarID, body.ID).
//   - EventsPatch merges patch body into the cached snapshot per
//     Calendar API JSON Merge Patch:
//   - top-level fields in body REPLACE the cached value
//   - ExtendedProperties.Private is merged at the KEY level
//     (existing keys preserved unless body provides a new value)
//   - The merged result is cached AND returned, so subsequent patches
//     see the post-merge state.
func TestDryRunAPI_PatchMergesIntoCachedInsertResource(t *testing.T) {
	api := newDryRunAPI(&stubGws{})
	ctx := context.Background()

	// First Insert: typical mirror payload with version + source.
	insertBody := &gws.Event{
		ID:      "evt",
		Summary: "Standup",
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				"calendar-sync:source":         "alice@example.com:src-evt",
				"calendar-sync:source_updated": "2026-04-29T20:00:00Z",
				"calendar-sync:version":        "2",
			},
		},
	}
	if _, err := api.EventsInsert(ctx, "cal", insertBody); err != nil {
		t.Fatalf("EventsInsert: %v", err)
	}

	// Follow-up checksum patch: body carries ONLY {checksum}, the
	// production shape (internal/sync/helpers.go:followUpChecksum).
	patchBody := &gws.Event{
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				"calendar-sync:checksum": "deadbeef",
			},
		},
	}
	got, err := api.EventsPatch(ctx, "cal", "evt", patchBody)
	if err != nil {
		t.Fatalf("EventsPatch: %v", err)
	}

	if got.ID != "evt" {
		t.Errorf("ID = %q, want evt", got.ID)
	}
	if got.Summary != "Standup" {
		t.Errorf("Summary = %q, want %q (cached from Insert)", got.Summary, "Standup")
	}
	if got.ExtendedProperties == nil || got.ExtendedProperties.Private == nil {
		t.Fatalf("ExtendedProperties/Private nil; got %+v", got)
	}
	priv := got.ExtendedProperties.Private
	wantPrivate := map[string]string{
		"calendar-sync:source":         "alice@example.com:src-evt",
		"calendar-sync:source_updated": "2026-04-29T20:00:00Z",
		"calendar-sync:version":        "2",
		"calendar-sync:checksum":       "deadbeef",
	}
	for k, want := range wantPrivate {
		if got := priv[k]; got != want {
			t.Errorf("Private[%q] = %q, want %q (full map: %v)", k, got, want, priv)
		}
	}
}

// TestDryRunAPI_PatchOverwritesExistingPrivateKey verifies merge
// semantics for the case where a patch body re-writes a key already
// present in the cached snapshot. JSON Merge Patch semantics: the
// patch wins on key-level collision (the key's value is replaced).
func TestDryRunAPI_PatchOverwritesExistingPrivateKey(t *testing.T) {
	api := newDryRunAPI(&stubGws{})
	ctx := context.Background()

	insertBody := &gws.Event{
		ID:      "evt",
		Summary: "v1",
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{"k": "old"},
		},
	}
	if _, err := api.EventsInsert(ctx, "cal", insertBody); err != nil {
		t.Fatalf("EventsInsert: %v", err)
	}
	patchBody := &gws.Event{
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{"k": "new"},
		},
	}
	got, err := api.EventsPatch(ctx, "cal", "evt", patchBody)
	if err != nil {
		t.Fatalf("EventsPatch: %v", err)
	}
	if got.ExtendedProperties.Private["k"] != "new" {
		t.Errorf("Private[k] = %q, want %q (patch should overwrite)",
			got.ExtendedProperties.Private["k"], "new")
	}
}

// TestDryRunAPI_PatchWithoutPriorInsertReturnsBody covers the edge
// case where EventsPatch is called for an ID that was never inserted
// (e.g. tests that don't drive doInsert first). With no cached
// snapshot, the wrapper falls back to the body-echo behavior: the
// request body is returned with ID populated. This preserves the
// prior contract for callers that don't go through Insert.
func TestDryRunAPI_PatchWithoutPriorInsertReturnsBody(t *testing.T) {
	api := newDryRunAPI(&stubGws{})
	body := &gws.Event{
		Summary: "patched",
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{"k": "v"},
		},
	}
	got, err := api.EventsPatch(context.Background(), "cal", "no-insert", body)
	if err != nil {
		t.Fatalf("EventsPatch: %v", err)
	}
	if got.ID != "no-insert" {
		t.Errorf("ID = %q, want no-insert", got.ID)
	}
	if got.Summary != "patched" {
		t.Errorf("Summary = %q, want patched", got.Summary)
	}
	if got.ExtendedProperties.Private["k"] != "v" {
		t.Errorf("Private[k] = %q, want v", got.ExtendedProperties.Private["k"])
	}
}

// TestDryRunAPI_DeleteClearsCache pins the cache-clearing semantics
// of EventsDelete. Without this, an Insert -> Delete -> Patch
// sequence would surface the deleted event's old payload via the
// post-merge return value, which doesn't match production semantics
// (a deleted event isn't there anymore; subsequent Patch on it would
// be a 404). Tests that exercise mirror.prune through dry-run rely
// on Delete clearing the cache so a subsequent Insert with the same
// ID re-hydrates fresh.
func TestDryRunAPI_DeleteClearsCache(t *testing.T) {
	api := newDryRunAPI(&stubGws{})
	ctx := context.Background()

	insertBody := &gws.Event{
		ID:      "evt",
		Summary: "v1",
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{"calendar-sync:version": "2"},
		},
	}
	if _, err := api.EventsInsert(ctx, "cal", insertBody); err != nil {
		t.Fatalf("EventsInsert: %v", err)
	}
	if err := api.EventsDelete(ctx, "cal", "evt"); err != nil {
		t.Fatalf("EventsDelete: %v", err)
	}

	// After Delete, a Patch with no body of its own should hit the
	// no-prior-insert fallback (body-echo). The Insert's Summary=v1
	// must NOT bleed through.
	patchBody := &gws.Event{Summary: "v2"}
	got, err := api.EventsPatch(ctx, "cal", "evt", patchBody)
	if err != nil {
		t.Fatalf("EventsPatch: %v", err)
	}
	if got.Summary != "v2" {
		t.Errorf("Summary = %q, want v2 (cache should have been cleared)", got.Summary)
	}
	// The Insert's calendar-sync:version must NOT be present - it was
	// in the cleared cache, not in the patch body.
	if got.ExtendedProperties != nil && got.ExtendedProperties.Private["calendar-sync:version"] != "" {
		t.Errorf("Private[version] = %q, want empty (cache should have been cleared); ext=%+v",
			got.ExtendedProperties.Private["calendar-sync:version"], got.ExtendedProperties)
	}
}

// countingGws extends stubGws with per-method counters so dryRunAPI tests
// can assert no underlying write occurred. Each Counter is a *int so the
// test owns the storage and can assert directly without exposing extra
// accessors.
type countingGws struct {
	stubGws
	insertCount *int
	patchCount  *int
	deleteCount *int
}

func (c *countingGws) EventsInsert(ctx context.Context, cal string, body *gws.Event) (*gws.Event, error) {
	if c.insertCount != nil {
		*c.insertCount++
	}
	return c.stubGws.EventsInsert(ctx, cal, body)
}

func (c *countingGws) EventsPatch(ctx context.Context, cal, id string, body *gws.Event) (*gws.Event, error) {
	if c.patchCount != nil {
		*c.patchCount++
	}
	return c.stubGws.EventsPatch(ctx, cal, id, body)
}

func (c *countingGws) EventsDelete(ctx context.Context, cal, id string) error {
	if c.deleteCount != nil {
		*c.deleteCount++
	}
	return c.stubGws.EventsDelete(ctx, cal, id)
}

// TestDryRunAPI_EventsPatchSuppressesWrite is the patch-side counterpart to
// TestDryRunAPI_EventsInsertSuppressesWrite: every patch call through the
// wrapper must NOT reach the inner client. Pinned per-method because the
// production path emits more patches than inserts (every drift
// reconciliation, every checksum follow-up, every recurring instance
// materialization), so a regression that re-enables the inner call here is
// the most likely source of accidental writes.
func TestDryRunAPI_EventsPatchSuppressesWrite(t *testing.T) {
	patches := 0
	inner := &countingGws{stubGws: stubGws{}, patchCount: &patches}
	api := newDryRunAPI(inner)
	body := &gws.Event{Summary: "patched"}
	got, err := api.EventsPatch(context.Background(), "cal", "evt-1", body)
	if err != nil {
		t.Fatalf("EventsPatch: %v", err)
	}
	if patches != 0 {
		t.Errorf("inner.EventsPatch was called %d times, want 0", patches)
	}
	if got == nil || got.ID != "evt-1" {
		t.Errorf("got = %+v, want event with ID evt-1", got)
	}
}

// TestDryRunAPI_EventsDeleteSuppressesWrite verifies the orphan-walk delete
// path is gated. SPEC's "orphan cleanup" is the single largest write
// volume in a steady-state run; if delete bypasses the wrapper the user
// loses real mirror events on every dry-run.
func TestDryRunAPI_EventsDeleteSuppressesWrite(t *testing.T) {
	deletes := 0
	inner := &countingGws{stubGws: stubGws{}, deleteCount: &deletes}
	api := newDryRunAPI(inner)
	if err := api.EventsDelete(context.Background(), "cal", "evt-1"); err != nil {
		t.Fatalf("EventsDelete: %v", err)
	}
	if deletes != 0 {
		t.Errorf("inner.EventsDelete was called %d times, want 0", deletes)
	}
}

// TestRunCmd_DryRun_DuplicateSourceEventNoLongerEmitsBogusMigration is the
// end-to-end regression test for doc/dry-run-anomaly-analysis.md anomaly #1.
// The user's dry-run produced 14 patches with conflict=migration_source_won
// despite zero v1/v2 mirrors on the target. Two cooperating causes:
//
//   - Cause A: dryRunAPI.EventsPatch echoed only the request body, dropping
//     the prior Insert's extended properties (calendar-sync:version, source).
//   - Cause B: source-list duplication - _R<timestamp> recurring parents
//     appear both as a top-level event and as a recurring_event_id on their
//     instances, so runClassifyLoop processed the same source-tuple twice.
//
// Both fixes landed (the dedupe in runClassifyLoop and the cache-and-merge
// in dryRunAPI). Either alone makes the bogus outcome go away in the
// duplicate-source case; with both, the wire shape is also correct on
// other dry-run paths that consume the post-Patch event.
//
// This test feeds two copies of the same source event - the production
// shape - and asserts:
//  1. NO migration_source_won outcome appears in the output.
//  2. Exactly ONE outcome is emitted (the insert from the first occurrence).
//     The dedupe at runClassifyLoop short-circuits the second pass before
//     it reaches Classify.
func TestRunCmd_DryRun_DuplicateSourceEventNoLongerEmitsBogusMigration(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	path := writeConfigFixture(t, validConfigTOML)

	dupEvent := gws.Event{
		ID:           "evt-1",
		Status:       gws.EventStatusConfirmed,
		Summary:      "SA Office Hours",
		Updated:      "2026-04-29T20:00:00Z",
		Transparency: gws.TransparencyOpaque,
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T16:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T17:00:00Z"},
	}
	stub := &dupSourceListGws{events: []gws.Event{dupEvent, dupEvent}}

	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     stub,
	}
	if err := (&RunCmd{DryRun: true}).Run(rt); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	out := stdout.String()
	if strings.Contains(out, "migration_source_won") {
		t.Errorf("dry-run output contains migration_source_won; B2 fix regressed.\noutput:\n%s", out)
	}

	// Parse outcome lines (skip the _meta trailer). validConfigTOML
	// declares ONE pair, which expands to ONE pdir (a_to_b) post-v2.0.0.
	// dupSourceListGws returns the same event twice; per-pdir dedupe at
	// runClassifyLoop kills the second occurrence, leaving exactly ONE
	// outcome. The key post-fix invariant is that the outcome is NOT
	// migration_source_won AND the per-pdir count is 1 (not 2).
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var outcomes []map[string]interface{}
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		var row map[string]interface{}
		if err := json.Unmarshal([]byte(ln), &row); err != nil {
			t.Fatalf("unmarshal %q: %v", ln, err)
		}
		if _, isMeta := row["_meta"]; isMeta {
			continue
		}
		outcomes = append(outcomes, row)
	}
	// 1 outcome (single pdir, dedupe killed the duplicate). 2 outcomes
	// would mean dedupe regressed. 0 means a different regression.
	if len(outcomes) != 1 {
		t.Fatalf("expected 1 outcome (single insert; dedupe within the lone pdir); got %d:\n%s",
			len(outcomes), out)
	}
	for _, row := range outcomes {
		if action, _ := row["action"].(string); action != "insert" {
			t.Errorf("outcome action = %q, want insert; row=%+v", action, row)
		}
		if src, _ := row["source_event"].(string); src != "evt-1" {
			t.Errorf("outcome source_event = %q, want evt-1; row=%+v", src, row)
		}
		if conflict, _ := row["conflict"].(string); conflict != "" {
			t.Errorf("outcome conflict = %q, want empty (no conflict on insert); row=%+v",
				conflict, row)
		}
	}
}

// TestRunCmd_SettingsDryRunGatesWrites pins SPEC line 253: the
// `[settings].dry_run` config field, when true, must suppress writes the
// same way `--dry-run` does. Pre-fix this fails because the settings
// field is parsed and emitted in `config show` output but never wired to
// the dryRunAPI wrapper at the run/watch/pair-test sites.
//
// We use the panicWriteGws stub so any leaked write surfaces as a
// descriptive panic; the test asserts the run completes without panic.
func TestRunCmd_SettingsDryRunGatesWrites(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	// Same-shape config as validConfigTOML but with dry_run=true.
	const dryRunConfigTOML = `
[settings]
poll_interval      = "60s"
horizon            = "365d"
full_sync_interval = "24h"
log_level          = "info"
log_format         = "json"
dry_run            = true

[[pairs]]
name      = "work-personal"
source    = "work@example.com"
target    = "personal@example.com"
`
	path := writeConfigFixture(t, dryRunConfigTOML)

	// One confirmed source event: enough to drive the doInsert path which
	// is the most common write call site.
	stub := &panicWriteGws{events: []gws.Event{{
		ID:           "evt-1",
		Status:       gws.EventStatusConfirmed,
		Summary:      "Test event",
		Updated:      "2026-04-29T20:00:00Z",
		Transparency: gws.TransparencyOpaque,
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T16:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T17:00:00Z"},
	}}}

	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     stub,
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("settings.dry_run did not gate writes: %v\nstack:\n%s", r, debug.Stack())
		}
	}()

	// CRUCIAL: NO --dry-run flag. settings.dry_run must do the gating.
	if err := (&RunCmd{}).Run(rt); err != nil {
		t.Logf("RunCmd.Run returned %v (expected for synthetic stub; the panic-stub guarantee is what matters)", err)
	}
}

// dupSourceListGws extends stubGws with a fixed source-list response. The
// inventory rebuild calls return empty (no mirrors exist on the target),
// which mirrors the production scenario we observed.
type dupSourceListGws struct {
	stubGws
	events []gws.Event
}

func (d *dupSourceListGws) EventsList(_ context.Context, params gws.EventsListParams) ([]gws.Event, string, error) {
	if len(params.PrivateExtendedProperty) > 0 {
		// Inventory rebuild: empty (no v1 or v2 mirrors).
		return nil, "", nil
	}
	// Source-list call: return the duplicated events.
	return d.events, "next-token", nil
}

// panicWriteGws is a stress-test stub: every read returns a canned response,
// every WRITE method panics. Tests wrap this in newDryRunAPI to assert that
// the dry-run wrapper short-circuits 100% of writes regardless of what the
// sync layer attempts (insert, patch, delete, recurring-handler patch, drift
// patch, orphan delete, etc.). A panic surfaces as a test failure with a
// clear "B1-class regression" message.
//
// Naming: Gws here means "gws subprocess wrapper" (the GwsClient interface),
// not literally the binary. The stub intercepts the wrapper layer.
//
// The stub's EventsGet / EventsInstances return synthetic events when the
// recurring path needs a parent or instances - they're read-only so the
// content matters less than the shape; the goal is only to keep the sync
// layer running long enough to attempt a write that would then panic.
type panicWriteGws struct {
	stubGws
	events []gws.Event

	// parentEvents is the canned response for EventsGet calls (typically
	// the recurring-instance handler fetching the source parent). Keyed by
	// event ID; missing key returns gws.ErrAPINotFound so the recurring
	// path bails out cleanly rather than nil-derefing the response.
	parentEvents map[string]gws.Event
}

func (p *panicWriteGws) EventsList(_ context.Context, params gws.EventsListParams) ([]gws.Event, string, error) {
	if len(params.PrivateExtendedProperty) > 0 {
		return nil, "", nil
	}
	return p.events, "next-token", nil
}

func (p *panicWriteGws) EventsGet(_ context.Context, _, eventID string) (*gws.Event, error) {
	if e, ok := p.parentEvents[eventID]; ok {
		return &e, nil
	}
	return nil, gws.ErrAPINotFound
}

func (p *panicWriteGws) EventsInstances(_ context.Context, _ gws.EventsInstancesParams) ([]gws.Event, error) {
	// Empty result keeps the recurring "instance lookup" path well-defined;
	// it'll trigger the repair branch which fetches the source parent via
	// EventsGet, and our parent fixture lives there.
	return nil, nil
}

func (p *panicWriteGws) EventsInsert(_ context.Context, calendarID string, body *gws.Event) (*gws.Event, error) {
	panic(fmt.Sprintf("panicWriteGws.EventsInsert called - dry-run wrapper failed to suppress write to %s/%s", calendarID, body.ID))
}

func (p *panicWriteGws) EventsPatch(_ context.Context, calendarID, eventID string, _ *gws.Event) (*gws.Event, error) {
	panic(fmt.Sprintf("panicWriteGws.EventsPatch called - dry-run wrapper failed to suppress patch to %s/%s", calendarID, eventID))
}

func (p *panicWriteGws) EventsDelete(_ context.Context, calendarID, eventID string) error {
	panic(fmt.Sprintf("panicWriteGws.EventsDelete called - dry-run wrapper failed to suppress delete of %s/%s", calendarID, eventID))
}

// TestDryRunAPI_ZeroWritesAcrossEventShapes is the load-bearing safety
// guarantee for the `--dry-run` flag: under any source-event mix the user
// throws at us, the dry-run wrapper short-circuits 100% of writes. This
// test wraps a panicWriteGws in newDryRunAPI; the panic stub fires on any
// EventsInsert/Patch/Delete the wrapper failed to intercept.
//
// We feed a deliberately-pathological source list - confirmed events, a
// recurring instance, a cancelled event, a duplicate (same-tuple appearing
// twice in source-list, the B2 production shape). Each shape exercises a
// different sync-layer write path:
//
//   - confirmed event with no mirror → doInsert → EventsInsert + checksum
//     EventsPatch
//   - cancelled event with no mirror → SPEC's "skip" cell, no writes
//   - recurring instance → recurring.Handler.handleConfirmed →
//     EventsPatch (instance materialization) + EventsPatch (checksum)
//   - duplicated event → second pass triggers either patch or
//     migration_source_won (B2); either way no underlying write should fire.
//
// If ANY of these reach the inner client, the test panics with a
// descriptive message identifying the leaking method + calendar + event ID.
func TestDryRunAPI_ZeroWritesAcrossEventShapes(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	path := writeConfigFixture(t, validConfigTOML)

	// Mix the four shapes. Calendar API treats event IDs as opaque so the
	// names don't matter; the wire-shape fields (Status, Recurring*, etc.)
	// drive sync-layer dispatch.
	events := []gws.Event{
		{
			ID:           "evt-confirmed-1",
			Status:       gws.EventStatusConfirmed,
			Summary:      "Confirmed event",
			Updated:      "2026-04-29T20:00:00Z",
			Transparency: gws.TransparencyOpaque,
			Start:        &gws.EventDateTime{DateTime: "2026-05-01T16:00:00Z"},
			End:          &gws.EventDateTime{DateTime: "2026-05-01T17:00:00Z"},
		},
		{
			ID:      "evt-cancelled-1",
			Status:  gws.EventStatusCancelled,
			Summary: "Cancelled event",
			Updated: "2026-04-29T20:00:00Z",
		},
		{
			ID:                "evt-recurring-instance-1",
			Status:            gws.EventStatusConfirmed,
			Summary:           "Recurring instance exception",
			RecurringEventID:  "evt-parent-1",
			OriginalStartTime: &gws.EventDateTime{DateTime: "2026-05-15T16:00:00Z"},
			Updated:           "2026-04-29T20:00:00Z",
			Transparency:      gws.TransparencyOpaque,
			Start:             &gws.EventDateTime{DateTime: "2026-05-15T17:00:00Z"},
			End:               &gws.EventDateTime{DateTime: "2026-05-15T18:00:00Z"},
		},
		{
			ID:           "evt-confirmed-1", // deliberate duplicate of [0]
			Status:       gws.EventStatusConfirmed,
			Summary:      "Confirmed event (duplicate)",
			Updated:      "2026-04-29T20:00:00Z",
			Transparency: gws.TransparencyOpaque,
			Start:        &gws.EventDateTime{DateTime: "2026-05-01T16:00:00Z"},
			End:          &gws.EventDateTime{DateTime: "2026-05-01T17:00:00Z"},
		},
	}
	parents := map[string]gws.Event{
		"evt-parent-1": {
			ID:           "evt-parent-1",
			Status:       gws.EventStatusConfirmed,
			Summary:      "Recurring parent",
			Updated:      "2026-04-29T20:00:00Z",
			Transparency: gws.TransparencyOpaque,
			Start:        &gws.EventDateTime{DateTime: "2026-05-01T16:00:00Z"},
			End:          &gws.EventDateTime{DateTime: "2026-05-01T17:00:00Z"},
			Recurrence:   []string{"RRULE:FREQ=WEEKLY;BYDAY=MO"},
		},
	}
	stub := &panicWriteGws{events: events, parentEvents: parents}

	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     stub,
	}

	// We don't assert on the error - some paths legitimately fail (e.g. the
	// recurring-instance path needs an EventsGet of the parent which our
	// stub returns nil for). The point is: ANY underlying write would
	// panic. As long as we return without panic, the dry-run wrapper held.
	//
	// Use `defer recover` so a panic is converted to a clear test failure
	// rather than tearing down the test binary. We print the stack so a
	// nil-deref panic from a malformed test event isn't misread as a
	// dry-run leak.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic during dry-run: %v\nstack:\n%s", r, debug.Stack())
		}
	}()

	_ = (&RunCmd{DryRun: true}).Run(rt)
}

// TestRunCmd_ConfigHorizonWiredToPDir pins B4 from doc/bugs.md and the
// per-pair horizon scoping rollout: `[settings].horizon` is the per-pdir
// fallback. Post-rollout, horizon lives on each canonical PDir (resolved
// during canonicalization to either the per-pair override or the settings
// default). A regression that drops the wire-through would leave PDir
// horizons at zero, classifying every event as outside_horizon and
// deleting every mirror.
//
// Two cases pin both ends of the SPEC's allowed range (1d-730d):
//   - "1d" → 24h: smallest horizon, the day-by-day rollout shape.
//   - "365d" → 8760h: SPEC's default.
//
// We replicate the exact wire pattern run.go uses (Load → Canonicalize)
// and assert on the canonical PDir's resolved horizon - the reconciler no
// longer carries a Horizon field; per-pdir consumers read it from the PDir.
func TestRunCmd_ConfigHorizonWiredToPDir(t *testing.T) {
	cases := []struct {
		name        string
		horizonTOML string
		want        time.Duration
	}{
		{"1d", `horizon = "1d"`, 24 * time.Hour},
		{"365d", `horizon = "365d"`, 365 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `
[settings]
poll_interval      = "60s"
` + tc.horizonTOML + `
full_sync_interval = "24h"
log_level          = "info"
log_format         = "json"

[[pairs]]
name      = "work-personal"
source    = "work@example.com"
target    = "personal@example.com"
`
			path := writeConfigFixture(t, body)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			canonical, err := cfg.Canonicalize(context.Background(), &stubGws{})
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			if len(canonical.PDirs) != 1 {
				t.Fatalf("len(PDirs) = %d, want 1", len(canonical.PDirs))
			}
			if got := canonical.PDirs[0].Horizon; got != tc.want {
				t.Errorf("PDir.Horizon = %v, want %v (config horizon=%q)",
					got, tc.want, tc.horizonTOML)
			}
		})
	}
}

// TestRunCmd_ConfigPropagateWiredToPDir pins the per-pair propagate scoping
// rollout: `[settings].propagate_target_edits` is the per-pdir fallback.
// Post-rollout, the gate value lives on each canonical PDir (resolved at
// canonicalization to either the per-pair override or the settings
// default). A regression that drops the wire-through would leave PDirs
// with PropagateTargetEdits=false even when the operator opted in via
// settings - silently neutralizing every two-way pdir.
//
// We replicate the exact wire pattern run.go uses (Load → Canonicalize)
// and assert on the canonical PDir's resolved value - the reconciler no
// longer carries a PropagateTargetEdits field; the gate reads pd.PropagateTargetEdits.
func TestRunCmd_ConfigPropagateWiredToPDir(t *testing.T) {
	body := `
[settings]
poll_interval         = "60s"
horizon               = "365d"
full_sync_interval    = "24h"
log_level             = "info"
log_format            = "json"
propagate_target_edits = true

[[pairs]]
name      = "work-personal"
source    = "work@example.com"
target    = "personal@example.com"
`
	path := writeConfigFixture(t, body)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	canonical, err := cfg.Canonicalize(context.Background(), &stubGws{})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if len(canonical.PDirs) != 1 {
		t.Fatalf("len(PDirs) = %d, want 1", len(canonical.PDirs))
	}
	if got, want := canonical.PDirs[0].PropagateTargetEdits, true; got != want {
		t.Errorf("PDir.PropagateTargetEdits = %v, want %v (settings opt-in)", got, want)
	}
}

// TestRunCmd_PerPairPropagateOverridesSettings: when a [[pairs]] block
// sets its own propagate_target_edits, that override flows through
// canonicalization to the PDir's resolved value. The settings field is
// the fallback for pairs without an explicit override; per-pair scoping
// is what enables ramping two-way sync one direction at a time.
//
// Three pairs cover the three resolution paths:
//   - settings=false, no override → false (fallback).
//   - settings=false, override=true → true (per-pair opt-in).
//   - settings=false, override=false → false (explicit-false override; pins
//     the *bool wire so omitempty / nil-vs-pointer-to-false confusion can't
//     silently downgrade to fallback).
func TestRunCmd_PerPairPropagateOverridesSettings(t *testing.T) {
	body := `
[settings]
poll_interval         = "60s"
horizon               = "365d"
full_sync_interval    = "24h"
log_level             = "info"
log_format            = "json"
propagate_target_edits = false

[[pairs]]
name    = "fallback"
source  = "a@example.com"
target  = "b@example.com"

[[pairs]]
name                   = "override-true"
source                 = "c@example.com"
target                 = "d@example.com"
propagate_target_edits = true

[[pairs]]
name                   = "override-false"
source                 = "e@example.com"
target                 = "f@example.com"
propagate_target_edits = false
`
	path := writeConfigFixture(t, body)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	canonical, err := cfg.Canonicalize(context.Background(), &stubGws{})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	pdirByName := map[string]config.PDir{}
	for _, pd := range canonical.PDirs {
		pdirByName[pd.PairName] = pd
	}
	if got, want := pdirByName["fallback"].PropagateTargetEdits, false; got != want {
		t.Errorf("fallback PDir.PropagateTargetEdits = %v, want %v (settings default)", got, want)
	}
	if got, want := pdirByName["override-true"].PropagateTargetEdits, true; got != want {
		t.Errorf("override-true PDir.PropagateTargetEdits = %v, want %v (per-pair override)", got, want)
	}
	if got, want := pdirByName["override-false"].PropagateTargetEdits, false; got != want {
		t.Errorf("override-false PDir.PropagateTargetEdits = %v, want %v (explicit-false per-pair override)", got, want)
	}
}

// TestRunCmd_PerPairHorizonOverridesSettings: when a [[pairs]] block sets
// its own horizon, that override flows through canonicalization to the
// PDir.Horizon. Settings.Horizon is the fallback for pairs without an
// explicit override; per-pair scoping is what enables the gradual rollout
// shape (one direction at horizon=1d while the other stays at 365d).
func TestRunCmd_PerPairHorizonOverridesSettings(t *testing.T) {
	body := `
[settings]
poll_interval      = "60s"
horizon            = "365d"
full_sync_interval = "24h"
log_level          = "info"
log_format         = "json"

[[pairs]]
name    = "fallback"
source  = "a@example.com"
target  = "b@example.com"

[[pairs]]
name    = "override"
source  = "c@example.com"
target  = "d@example.com"
horizon = "1d"
`
	path := writeConfigFixture(t, body)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	canonical, err := cfg.Canonicalize(context.Background(), &stubGws{})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	pdirByName := map[string]config.PDir{}
	for _, pd := range canonical.PDirs {
		pdirByName[pd.PairName] = pd
	}
	if got, want := pdirByName["fallback"].Horizon, 365*24*time.Hour; got != want {
		t.Errorf("fallback PDir.Horizon = %v, want %v (settings default)", got, want)
	}
	if got, want := pdirByName["override"].Horizon, 24*time.Hour; got != want {
		t.Errorf("override PDir.Horizon = %v, want %v (per-pair override)", got, want)
	}
}
