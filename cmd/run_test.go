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
