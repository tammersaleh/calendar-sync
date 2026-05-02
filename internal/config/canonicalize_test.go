package config_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// stubLister is the in-process CalendarLister used by canonicalize tests.
// Each entry is a canned response keyed by the literal calendar reference
// the caller passes in (matches the TOML field). Tests that need a
// failure case set Err.
type stubLister struct {
	responses map[string]*gws.CalendarListEntry
	err       error
	calls     []string
}

func (s *stubLister) CalendarListGet(ctx context.Context, calendarID string) (*gws.CalendarListEntry, error) {
	s.calls = append(s.calls, calendarID)
	if s.err != nil {
		return nil, s.err
	}
	entry, ok := s.responses[calendarID]
	if !ok {
		return nil, fmt.Errorf("stubLister: no response for %q", calendarID)
	}
	return entry, nil
}

func enabled(b bool) *bool { return &b }

func baseConfig() config.Config {
	c := config.Config{
		Settings: config.Settings{
			PollInterval:     config.Duration(60 * 1e9),         // 60s
			Horizon:          config.Duration(365 * 24 * 3.6e12), // 365d in ns
			FullSyncInterval: config.Duration(24 * 3.6e12),       // 24h
			LogLevel:         config.LogLevelInfo,
			LogFormat:        config.LogFormatJSON,
		},
		Pairs: []config.Pair{},
	}
	return c
}

func TestCanonicalize_PrimaryAliasResolved(t *testing.T) {
	c := baseConfig()
	c.Pairs = append(c.Pairs, config.Pair{
		Name:   "wp",
		Source: "alice@example.com",
		Target: "primary",
	})

	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"alice@example.com": {ID: "alice@example.com", AccessRole: "writer"},
			"primary":           {ID: "alice.canonical@example.com", AccessRole: "owner"},
		},
	}

	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if got := can.Calendars["alice.canonical@example.com"].CanonicalID; got != "alice.canonical@example.com" {
		t.Errorf("primary not canonicalized: got %q", got)
	}
	if can.Calendars["alice.canonical@example.com"].OriginalRef != "primary" {
		t.Errorf("OriginalRef = %q, want primary", can.Calendars["alice.canonical@example.com"].OriginalRef)
	}
	if len(can.PDirs) != 1 {
		t.Errorf("len(PDirs) = %d, want 1 (every pair is one pdir post-v2.0.0)", len(can.PDirs))
	}
}

// TestCanonicalize_AlwaysOnePDirPerPair pins the v2.0.0 invariant: every
// enabled pair produces exactly one PDir, in the source→target direction.
// Bidirectional setups must declare two pairs with swapped source/target
// (which then produce two pdirs total).
func TestCanonicalize_AlwaysOnePDirPerPair(t *testing.T) {
	c := baseConfig()
	c.Pairs = append(c.Pairs, config.Pair{
		Name:   "p",
		Source: "a@example.com",
		Target: "b@example.com",
	})
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if len(can.PDirs) != 1 {
		t.Fatalf("len(PDirs) = %d, want 1", len(can.PDirs))
	}
	if can.PDirs[0].Direction != config.PDirAtoB {
		t.Errorf("PDir.Direction = %q, want %q (every pdir is a_to_b post-v2.0.0)",
			can.PDirs[0].Direction, config.PDirAtoB)
	}
	if can.PDirs[0].SourceCalendar != "a@example.com" {
		t.Errorf("SourceCalendar = %q, want a@example.com", can.PDirs[0].SourceCalendar)
	}
	if can.PDirs[0].TargetCalendar != "b@example.com" {
		t.Errorf("TargetCalendar = %q, want b@example.com", can.PDirs[0].TargetCalendar)
	}
}

// TestCanonicalize_RejectsDirectionField is the canonicalize-level
// integration check: Canonicalize calls Validate first, so any TOML that
// still sets `direction` must fail the canonicalize step too. Pinned so a
// future refactor can't accidentally bypass Validate.
func TestCanonicalize_RejectsDirectionField(t *testing.T) {
	c := baseConfig()
	c.Pairs = append(c.Pairs, config.Pair{
		Name:      "p",
		Direction: "source_to_target",
		Source:    "a@example.com",
		Target:    "b@example.com",
	})
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
		},
	}
	_, err := c.Canonicalize(context.Background(), lister)
	if !errors.Is(err, config.ErrInvalid) {
		t.Errorf("err = %v; want ErrInvalid", err)
	}
}

func TestCanonicalize_DisabledPairSkipped(t *testing.T) {
	c := baseConfig()
	c.Pairs = []config.Pair{
		{
			Name:   "active",
			Source: "a@example.com",
			Target: "b@example.com",
		},
		{
			Name:    "off",
			Source:  "c@example.com",
			Target:  "d@example.com",
			Enabled: enabled(false),
		},
	}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
			"c@example.com": {ID: "c@example.com", AccessRole: "owner"},
			"d@example.com": {ID: "d@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if len(can.PDirs) != 1 {
		t.Errorf("len(PDirs) = %d, want 1 (disabled pair skipped)", len(can.PDirs))
	}
	if can.PDirs[0].PairName != "active" {
		t.Errorf("emitted pdir for wrong pair: %q", can.PDirs[0].PairName)
	}
}

func TestCanonicalize_SourceWritableFromAccessRole(t *testing.T) {
	tests := []struct {
		role           string
		wantWritable   bool
	}{
		{"reader", false},
		{"writer", true},
		{"owner", true},
	}
	for _, tc := range tests {
		t.Run(tc.role, func(t *testing.T) {
			c := baseConfig()
			c.Pairs = []config.Pair{{
				Name:   "p",
				Source: "src@example.com",
				Target: "dst@example.com",
			}}
			lister := &stubLister{
				responses: map[string]*gws.CalendarListEntry{
					"src@example.com": {ID: "src@example.com", AccessRole: tc.role},
					"dst@example.com": {ID: "dst@example.com", AccessRole: "owner"},
				},
			}
			can, err := c.Canonicalize(context.Background(), lister)
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			if can.PDirs[0].SourceWritable != tc.wantWritable {
				t.Errorf("SourceWritable = %v, want %v", can.PDirs[0].SourceWritable, tc.wantWritable)
			}
		})
	}
}

func TestCanonicalize_AccessRoleValidation(t *testing.T) {
	// Every pair is implicitly source-to-target post-v2.0.0: the TOML
	// source side reads (needs >= reader); the TOML target side writes
	// (needs >= writer). The pre-v2.0.0 t2s and bidi cases collapse into
	// the s2t case: bidirectional setups now declare two pairs with
	// swapped source/target, so each row above is independently scoped.
	tests := []struct {
		name        string
		sourceRole  string
		targetRole  string
		wantInvalid bool
	}{
		{"source freeBusyReader rejected", "freeBusyReader", "owner", true},
		{"source reader OK", "reader", "owner", false},
		{"target reader rejected", "owner", "reader", true},
		{"target writer OK", "owner", "writer", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			c.Pairs = []config.Pair{{
				Name:   "p",
				Source: "src@example.com",
				Target: "dst@example.com",
			}}
			lister := &stubLister{
				responses: map[string]*gws.CalendarListEntry{
					"src@example.com": {ID: "src@example.com", AccessRole: tc.sourceRole},
					"dst@example.com": {ID: "dst@example.com", AccessRole: tc.targetRole},
				},
			}
			_, err := c.Canonicalize(context.Background(), lister)
			if tc.wantInvalid {
				if !errors.Is(err, config.ErrInvalid) {
					t.Errorf("err = %v; want ErrInvalid", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected err: %v", err)
				}
			}
		})
	}
}

func TestCanonicalize_DisabledPairAccessRoleNotValidated(t *testing.T) {
	// SPEC.md "[[pairs]]" says a disabled pair "is skipped entirely".
	// Pin that as: a disabled pair with insufficient access roles
	// must NOT cause Canonicalize to fail. Re-enabling the pair in
	// config will surface the failure at the next startup.
	c := baseConfig()
	c.Pairs = []config.Pair{{
		Name:    "off",
		Source:  "fr@example.com",
		Target:  "fr2@example.com",
		Enabled: enabled(false),
	}}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			// freeBusyReader on both - would fail if validated.
			"fr@example.com":  {ID: "fr@example.com", AccessRole: "freeBusyReader"},
			"fr2@example.com": {ID: "fr2@example.com", AccessRole: "freeBusyReader"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("disabled pair caused validation failure: %v", err)
	}
	if len(can.PDirs) != 0 {
		t.Errorf("len(PDirs) = %d, want 0", len(can.PDirs))
	}
	// Disabled pairs are skipped entirely - no calendar lookups should
	// happen for them.
	if len(lister.calls) != 0 {
		t.Errorf("expected 0 lister calls for fully-disabled config; got %d: %v",
			len(lister.calls), lister.calls)
	}
}

func TestCanonicalize_SourceTargetCollideAfterCanonicalization(t *testing.T) {
	// Two TOML refs that resolve to the same canonical ID via gws.
	// The pre-canonicalize Validate() doesn't catch this because the
	// strings differ; canonicalize must.
	c := baseConfig()
	c.Pairs = []config.Pair{{
		Name:   "selfsync",
		Source: "primary",
		Target: "alice@example.com",
	}}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"primary":           {ID: "alice@example.com", AccessRole: "owner"},
			"alice@example.com": {ID: "alice@example.com", AccessRole: "owner"},
		},
	}
	_, err := c.Canonicalize(context.Background(), lister)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v; want ErrInvalid for source==target after canonicalization", err)
	}
}

func TestCanonicalize_PDirCollisionRejected(t *testing.T) {
	// Two pairs writing the same canonical (source, target, direction)
	// triple is a configuration bug; we'd produce duplicate mirrors.
	c := baseConfig()
	c.Pairs = []config.Pair{
		{Name: "one", Source: "a@example.com", Target: "b@example.com"},
		{Name: "two", Source: "a@example.com", Target: "b@example.com"},
	}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
		},
	}
	_, err := c.Canonicalize(context.Background(), lister)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v; want ErrInvalid for pdir collision", err)
	}
}

func TestCanonicalize_ListerErrorPropagates(t *testing.T) {
	c := baseConfig()
	c.Pairs = []config.Pair{{
		Name:   "p",
		Source: "src@example.com",
		Target: "dst@example.com",
	}}
	wantErr := errors.New("simulated network failure")
	lister := &stubLister{err: wantErr}

	_, err := c.Canonicalize(context.Background(), lister)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v; want errors.Is to match the lister's error", err)
	}
}

func TestCanonicalize_DedupesCalendarLookups(t *testing.T) {
	// Two pairs that both reference the same calendar should produce
	// exactly one CalendarListGet call per distinct reference.
	c := baseConfig()
	c.Pairs = []config.Pair{
		{Name: "one", Source: "a@example.com", Target: "b@example.com"},
		{Name: "two", Source: "a@example.com", Target: "c@example.com"},
	}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
			"c@example.com": {ID: "c@example.com", AccessRole: "owner"},
		},
	}
	if _, err := c.Canonicalize(context.Background(), lister); err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	// Three distinct refs: a, b, c. Two pairs reference a. Want 3 calls,
	// not 4.
	if len(lister.calls) != 3 {
		t.Errorf("len(calls) = %d, want 3 (one per distinct calendar ref); calls = %v",
			len(lister.calls), lister.calls)
	}
}

// TestCanonicalize_PerPairHorizonOverridesSettings: a pair with an
// explicit horizon resolves to a PDir whose Horizon equals the override,
// not the settings default. Pins the per-pair scoping fallthrough.
func TestCanonicalize_PerPairHorizonOverridesSettings(t *testing.T) {
	c := baseConfig()
	override := config.Duration(24 * time.Hour)
	c.Pairs = []config.Pair{{
		Name:    "p",
		Source:  "a@example.com",
		Target:  "b@example.com",
		Horizon: &override,
	}}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if got, want := can.PDirs[0].Horizon, 24*time.Hour; got != want {
		t.Errorf("PDir.Horizon = %v, want %v (per-pair override)", got, want)
	}
}

// TestCanonicalize_NilHorizonFallsBackToSettings: a pair with no horizon
// resolves to a PDir whose Horizon equals the settings default. Pins the
// fallback path for configs that omit per-pair horizon.
func TestCanonicalize_NilHorizonFallsBackToSettings(t *testing.T) {
	c := baseConfig()
	// baseConfig() sets Settings.Horizon to 365d.
	c.Pairs = []config.Pair{{
		Name:   "p",
		Source: "a@example.com",
		Target: "b@example.com",
	}}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if got, want := can.PDirs[0].Horizon, 365*24*time.Hour; got != want {
		t.Errorf("PDir.Horizon = %v, want %v (settings fallback)", got, want)
	}
}

// TestCanonicalize_PerPairPropagateTargetEditsOverride: settings has
// propagate_target_edits=false but the pair sets &true; the resolved PDir
// flips to true. Pins the per-pair scoping fallthrough for the safety gate.
func TestCanonicalize_PerPairPropagateTargetEditsOverride(t *testing.T) {
	c := baseConfig()
	c.Settings.PropagateTargetEdits = false
	override := true
	c.Pairs = []config.Pair{{
		Name:                 "p",
		Source:               "a@example.com",
		Target:               "b@example.com",
		PropagateTargetEdits: &override,
	}}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if got, want := can.PDirs[0].PropagateTargetEdits, true; got != want {
		t.Errorf("PDir.PropagateTargetEdits = %v, want %v (per-pair override)", got, want)
	}
}

// TestCanonicalize_PerPairPropagateTargetEditsExplicitFalse: settings has
// propagate_target_edits=true but the pair sets &false; the resolved PDir
// is false. Pins the explicit-false case so a future bug that conflates
// nil-vs-&false (e.g. reading "absent" from "explicit false") gets caught.
func TestCanonicalize_PerPairPropagateTargetEditsExplicitFalse(t *testing.T) {
	c := baseConfig()
	c.Settings.PropagateTargetEdits = true
	override := false
	c.Pairs = []config.Pair{{
		Name:                 "p",
		Source:               "a@example.com",
		Target:               "b@example.com",
		PropagateTargetEdits: &override,
	}}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if got, want := can.PDirs[0].PropagateTargetEdits, false; got != want {
		t.Errorf("PDir.PropagateTargetEdits = %v, want %v (explicit false override)", got, want)
	}
}

// TestCanonicalize_NilPropagateFallsBackToSettings: a pair with no
// propagate_target_edits resolves to a PDir whose value equals the
// settings default. Pins the fallback path for configs that omit the
// per-pair field.
func TestCanonicalize_NilPropagateFallsBackToSettings(t *testing.T) {
	c := baseConfig()
	c.Settings.PropagateTargetEdits = true
	c.Pairs = []config.Pair{{
		Name:   "p",
		Source: "a@example.com",
		Target: "b@example.com",
	}}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if got, want := can.PDirs[0].PropagateTargetEdits, true; got != want {
		t.Errorf("PDir.PropagateTargetEdits = %v, want %v (settings fallback)", got, want)
	}
}

func TestCanonicalize_PreservesTimeZoneOnPDir(t *testing.T) {
	c := baseConfig()
	c.Pairs = []config.Pair{{
		Name:     "tz",
		Source:   "a@example.com",
		Target:   "b@example.com",
		TimeZone: "America/New_York",
	}}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"a@example.com": {ID: "a@example.com", AccessRole: "owner"},
			"b@example.com": {ID: "b@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if can.PDirs[0].TimeZone != "America/New_York" {
		t.Errorf("TimeZone = %q, want America/New_York", can.PDirs[0].TimeZone)
	}
}

func TestCanonicalize_PropagatesValidationFailure(t *testing.T) {
	// If parse-time Validate fails (e.g. duplicate pair name), Canonicalize
	// must short-circuit before making any gws calls.
	c := baseConfig()
	c.Pairs = []config.Pair{
		{Name: "dup", Source: "a@example.com", Target: "b@example.com"},
		{Name: "dup", Source: "c@example.com", Target: "d@example.com"},
	}
	lister := &stubLister{}

	_, err := c.Canonicalize(context.Background(), lister)
	if !errors.Is(err, config.ErrInvalid) {
		t.Errorf("err = %v; want ErrInvalid", err)
	}
	if len(lister.calls) != 0 {
		t.Errorf("Canonicalize made %d gws calls before validation; want 0", len(lister.calls))
	}
}
