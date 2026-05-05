package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// validConfigTOML is the smallest config that passes Validate. Post-v2.0.0
// every pair is implicitly source-to-target, so this fixture produces a
// single pdir (work→personal). Tests that need a second pdir declare a
// second [[pairs]] block locally.
const validConfigTOML = `
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
`

// stubGws is a hand-rolled GwsClient that satisfies sync.API +
// config.CalendarLister with zero-value responses. Subcommand tests that
// don't exercise the gws path inject this to avoid spawning the real
// binary.
type stubGws struct {
	calendars map[string]*gws.CalendarListEntry
}

func (s *stubGws) CalendarListGet(_ context.Context, id string) (*gws.CalendarListEntry, error) {
	if s.calendars == nil {
		return &gws.CalendarListEntry{ID: id, AccessRole: "owner"}, nil
	}
	if e, ok := s.calendars[id]; ok {
		return e, nil
	}
	return &gws.CalendarListEntry{ID: id, AccessRole: "owner"}, nil
}

// CalendarListList returns the calendars map flattened to a slice. Unused
// in the existing cmd-level fixtures (which all use ID-form refs and
// route through CalendarListGet); keeps the GwsClient interface satisfied
// after F1 widened it.
func (s *stubGws) CalendarListList(context.Context) ([]gws.CalendarListEntry, error) {
	if s.calendars == nil {
		return nil, nil
	}
	out := make([]gws.CalendarListEntry, 0, len(s.calendars))
	for _, e := range s.calendars {
		out = append(out, *e)
	}
	return out, nil
}

func (s *stubGws) EventsList(context.Context, gws.EventsListParams) ([]gws.Event, string, error) {
	return nil, "", nil
}

func (s *stubGws) EventsGet(context.Context, string, string) (*gws.Event, error) {
	return nil, nil
}

func (s *stubGws) EventsInstances(context.Context, gws.EventsInstancesParams) ([]gws.Event, error) {
	return nil, nil
}

func (s *stubGws) EventsInsert(context.Context, string, *gws.Event) (*gws.Event, error) {
	return nil, nil
}

func (s *stubGws) EventsPatch(context.Context, string, string, *gws.PatchEvent) (*gws.Event, error) {
	return nil, nil
}

func (s *stubGws) EventsDelete(context.Context, string, string) error {
	return nil
}

func writeConfigFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestConfigShowCmd_EmitsResolvedSettings(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&ConfigShowCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var payload configShowPayload
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &payload); err != nil {
		t.Fatalf("unmarshal: %v\nstdout=%s", err, stdout.String())
	}
	if payload.Settings.PollInterval != "60s" {
		t.Errorf("poll_interval = %q, want 60s", payload.Settings.PollInterval)
	}
	if payload.Settings.FullSyncInterval != "24h" {
		t.Errorf("full_sync_interval = %q, want 24h", payload.Settings.FullSyncInterval)
	}
	// propagate_target_edits defaults to false (one-way sync); the wire
	// shape MUST surface it so operators can confirm at a glance.
	if payload.Settings.PropagateTargetEdits {
		t.Errorf("propagate_target_edits = true; default must be false until opted in")
	}
	if len(payload.Pairs) != 1 || payload.Pairs[0].Name != "work-personal" {
		t.Errorf("pairs = %+v, want one pair named work-personal", payload.Pairs)
	}
}

// TestConfigShowCmd_DoesNotEmitDirectionField pins the v2.0.0 wire-format
// change: the `direction` field was removed from the per-pair JSON output.
// Decoding into map[string]any (rather than the typed pairPayload) catches
// regressions where someone re-adds the field to the struct - struct-typed
// unmarshal would silently ignore an unexpected key.
func TestConfigShowCmd_DoesNotEmitDirectionField(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&ConfigShowCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	var raw map[string]any
	if err := json.Unmarshal([]byte(first), &raw); err != nil {
		t.Fatalf("unmarshal: %v\nstdout=%s", err, stdout.String())
	}
	pairs, ok := raw["pairs"].([]any)
	if !ok {
		t.Fatalf("pairs not a JSON array; raw=%+v", raw)
	}
	if len(pairs) == 0 {
		t.Fatalf("pairs is empty; want at least one entry")
	}
	for i, p := range pairs {
		entry, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("pairs[%d] not a JSON object; got %T", i, p)
		}
		if _, present := entry["direction"]; present {
			t.Errorf("pairs[%d] contains `direction` key; v2.0.0 removed the field. entry=%+v",
				i, entry)
		}
	}
}

// TestConfigShowCmd_PerPairHorizonWireShape pins the per-pair-horizon
// scoping rollout's wire format: a pair with no override drops the
// `horizon` field via omitempty (the settings default isn't echoed back as
// a per-pair value); a pair with an explicit override emits the field in
// compact form.
func TestConfigShowCmd_PerPairHorizonWireShape(t *testing.T) {
	body := `
[settings]
poll_interval = "60s"
horizon = "365d"
full_sync_interval = "24h"
log_level = "info"
log_format = "json"

[[pairs]]
name = "fallback"
source = "a@example.com"
target = "b@example.com"

[[pairs]]
name = "override"
source = "c@example.com"
target = "d@example.com"
horizon = "1d"
`
	path := writeConfigFixture(t, body)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&ConfigShowCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	first := strings.SplitN(stdout.String(), "\n", 2)[0]

	// Decode into untyped map to assert presence/absence of the omitempty
	// field; struct-typed unmarshal would mask a missing key.
	var raw map[string]any
	if err := json.Unmarshal([]byte(first), &raw); err != nil {
		t.Fatalf("unmarshal: %v\nstdout=%s", err, stdout.String())
	}
	pairs, _ := raw["pairs"].([]any)
	byName := map[string]map[string]any{}
	for _, p := range pairs {
		entry := p.(map[string]any)
		byName[entry["name"].(string)] = entry
	}
	if h, present := byName["fallback"]["horizon"]; present {
		t.Errorf("fallback pair must drop horizon via omitempty; got %v", h)
	}
	if h := byName["override"]["horizon"]; h != "24h" {
		t.Errorf("override pair horizon = %v, want 24h", h)
	}
}

// TestConfigShowCmd_PerPairPropagateWireShape pins the per-pair-propagate
// scoping rollout's wire format, with a critical "explicit false" case:
//
//   - "fallback" pair has no override → omitempty drops the field entirely.
//   - "override-true" → key present, value true.
//   - "override-false" → key present, value false. This is the load-bearing
//     case: with a plain `bool` field + omitempty, Go's encoder would also
//     drop `false` and silently demote an explicit override to fallback.
//     The pairPayload's *bool pointer makes "absent" and "explicit false"
//     distinguishable; this test pins it.
func TestConfigShowCmd_PerPairPropagateWireShape(t *testing.T) {
	body := `
[settings]
poll_interval = "60s"
horizon = "365d"
full_sync_interval = "24h"
log_level = "info"
log_format = "json"
propagate_target_edits = false

[[pairs]]
name = "fallback"
source = "a@example.com"
target = "b@example.com"

[[pairs]]
name = "override-true"
source = "c@example.com"
target = "d@example.com"
propagate_target_edits = true

[[pairs]]
name = "override-false"
source = "e@example.com"
target = "f@example.com"
propagate_target_edits = false
`
	path := writeConfigFixture(t, body)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&ConfigShowCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	first := strings.SplitN(stdout.String(), "\n", 2)[0]

	var raw map[string]any
	if err := json.Unmarshal([]byte(first), &raw); err != nil {
		t.Fatalf("unmarshal: %v\nstdout=%s", err, stdout.String())
	}
	pairs, _ := raw["pairs"].([]any)
	byName := map[string]map[string]any{}
	for _, p := range pairs {
		entry := p.(map[string]any)
		byName[entry["name"].(string)] = entry
	}
	if v, present := byName["fallback"]["propagate_target_edits"]; present {
		t.Errorf("fallback pair must drop propagate_target_edits via omitempty; got %v", v)
	}
	if v, present := byName["override-true"]["propagate_target_edits"]; !present || v != true {
		t.Errorf("override-true propagate_target_edits = %v (present=%v), want true", v, present)
	}
	if v, present := byName["override-false"]["propagate_target_edits"]; !present || v != false {
		t.Errorf("override-false propagate_target_edits = %v (present=%v), want false (explicit false must NOT be dropped)",
			v, present)
	}
}

func TestConfigShowCmd_CanonicalizeResolvesSource(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws: &stubGws{
			calendars: map[string]*gws.CalendarListEntry{
				"work@example.com":     {ID: "work@example.com", AccessRole: "owner"},
				"personal@example.com": {ID: "personal@example.com", AccessRole: "owner"},
			},
		},
	}
	if err := (&ConfigShowCmd{Canonicalize: true}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var payload configShowPayload
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Pairs) != 1 {
		t.Fatalf("want 1 pair, got %d", len(payload.Pairs))
	}
	if payload.Pairs[0].Source.ID != "work@example.com" {
		t.Errorf("source = %q, want work@example.com", payload.Pairs[0].Source.ID)
	}
}

func TestConfigShowCmd_MissingFileMapsToConfigNotFound(t *testing.T) {
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: "/nonexistent/config.toml"},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	err := (&ConfigShowCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected error")
	}
	code, _, _ := MapError(err)
	if code != "config_not_found" {
		t.Errorf("code = %q, want config_not_found", code)
	}
}

func TestConfigValidateCmd_OK(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&ConfigValidateCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var payload configValidatePayload
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Status != "ok" {
		t.Errorf("status = %q, want ok", payload.Status)
	}
	if payload.Pairs != 1 {
		t.Errorf("pairs = %d, want 1", payload.Pairs)
	}
	if payload.PDirs != 1 {
		t.Errorf("pdirs = %d, want 1 (every pair is one pdir post-v2.0.0)", payload.PDirs)
	}
}

func TestConfigValidateCmd_BadConfigMapsToConfigInvalid(t *testing.T) {
	const bad = `
[settings]
poll_interval = "1s"
horizon = "365d"
full_sync_interval = "24h"
log_level = "info"
log_format = "json"
`
	path := writeConfigFixture(t, bad)
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	err := (&ConfigValidateCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected validation error")
	}
	code, _, _ := MapError(err)
	if code != "config_invalid" {
		t.Errorf("code = %q, want config_invalid", code)
	}
}
