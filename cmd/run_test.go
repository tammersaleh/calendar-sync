package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

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
	if meta.Meta.PDirs != 2 {
		t.Errorf("pdirs = %d, want 2 (bidirectional expansion)", meta.Meta.PDirs)
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
// We trigger a single-pdir failure by failing the inventory-rebuild for
// one target calendar. The inventory-rebuild call is distinguishable from
// the source-list call by the PrivateExtendedProperty filter
// (calendar-sync:version=...). For the bidirectional pair work-personal,
// failing inventory rebuild for personal@example.com kills the a_to_b
// pdir (target=personal) but leaves b_to_a (target=work) intact.
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
// short-circuits on the first failure.
func TestRunCmd_AllPDirsFailEmitsAllFailures(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	path := writeConfigFixture(t, validConfigTOML)

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

// TestDryRunAPI_PatchReturnsBodyWithoutOriginalExtProps documents the
// dry-run wrapper's body-echo behavior that drives Anomaly #1 in
// doc/dry-run-anomaly-analysis.md.
//
// The follow-up checksum patch sends body={extendedProperties.private:
// {checksum: ...}} only. The dry-run wrapper returns this body as the
// post-write resource, so the caller (sync.completeInsert) caches an
// "inserted" mirror in inventory whose extended properties contain ONLY
// the checksum - no calendar-sync:version, no calendar-sync:source. On the
// NEXT Classify pass for the same source event, ComputeDriftSignal sees
// a missing version and reports NeedsMigration=true even though the mirror
// was just minted with version=2 in the same dry-run pass.
//
// This test pins the wire shape so a future fix to dryRunAPI.EventsPatch
// (e.g. merging body into a remembered prior snapshot) breaks an explicit
// assertion rather than silently changing behavior.
func TestDryRunAPI_PatchReturnsBodyOnlyAndDropsOriginalExtProps(t *testing.T) {
	api := newDryRunAPI(&stubGws{})
	body := &gws.Event{
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{"calendar-sync:checksum": "abc123"},
		},
	}
	got, err := api.EventsPatch(context.Background(), "cal", "evt", body)
	if err != nil {
		t.Fatalf("EventsPatch: %v", err)
	}
	if got.ID != "evt" {
		t.Errorf("ID = %q, want evt", got.ID)
	}
	// The returned body has ONLY {checksum}. There is no calendar-sync:source
	// or calendar-sync:version - because the wrapper has no memory of the
	// prior insert. Downstream `mirror.ComputeDriftSignal` will read these
	// missing keys and return NeedsMigration=true.
	if got.ExtendedProperties == nil || got.ExtendedProperties.Private == nil {
		t.Fatalf("ExtendedProperties/Private nil; got %+v", got)
	}
	if _, has := got.ExtendedProperties.Private["calendar-sync:version"]; has {
		t.Errorf("post-checksum-patch body must NOT carry calendar-sync:version - "+
			"the wrapper echoes body verbatim. got=%+v", got.ExtendedProperties.Private)
	}
	if _, has := got.ExtendedProperties.Private["calendar-sync:source"]; has {
		t.Errorf("post-checksum-patch body must NOT carry calendar-sync:source. "+
			"got=%+v", got.ExtendedProperties.Private)
	}
}

// countingGws extends stubGws with insert counter so the dryRunAPI test
// can assert no underlying write occurred.
type countingGws struct {
	stubGws
	insertCount *int
}

func (c *countingGws) EventsInsert(ctx context.Context, cal string, body *gws.Event) (*gws.Event, error) {
	*c.insertCount++
	return c.stubGws.EventsInsert(ctx, cal, body)
}

// TestRunCmd_DryRun_DuplicateSourceEventTriggersBogusMigrationSourceWon is
// the end-to-end reproduction of doc/dry-run-anomaly-analysis.md anomaly
// #1. The user's dry-run output had 14 patches with conflict=
// migration_source_won despite zero v1/v2 mirrors existing on the target.
// This test wires the same code path with two copies of the same source
// event (a stand-in for whatever caused the actual duplication on Tammer's
// calendar - phantom recurring exception, paginated overlap, etc.) and
// asserts the bogus outcome is reproducible.
//
// Expected to FAIL on a future fix that either:
//   - rejects duplicate source-tuples in the source list (sync layer), or
//   - has dryRunAPI.EventsPatch echo a snapshot rather than only the patch
//     body (so the cached mirror retains version=2).
//
// When that fix lands, change t.Errorf to t.Logf and update the comment.
func TestRunCmd_DryRun_DuplicateSourceEventTriggersBogusMigrationSourceWon(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	path := writeConfigFixture(t, validConfigTOML)

	// Two identical copies of the same source event in the source list.
	// In the user's actual dry-run output the duplication appeared as
	// "_R<timestamp>" recurring-exception IDs being returned twice; the
	// shape that drives the bug is duplication, not recurring-ness.
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
		t.Fatalf("Run: %v", err)
	}

	// First pass should be insert(source_updated), second should be
	// patch(source_updated) with conflict=migration_source_won. That second
	// outcome is the bug: the mirror was JUST minted with version=2 in the
	// same dry-run pass.
	out := stdout.String()
	if !strings.Contains(out, `"action":"insert"`) {
		t.Errorf("expected first pass to insert; stdout:\n%s", out)
	}
	if !strings.Contains(out, `"conflict":"migration_source_won"`) {
		t.Errorf("expected second pass to emit migration_source_won; stdout:\n%s", out)
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
