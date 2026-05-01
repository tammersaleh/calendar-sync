package config_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
		Name:      "wp",
		Direction: config.DirectionBidirectional,
		Source:    "alice@example.com",
		Target:    "primary",
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
	if len(can.PDirs) != 2 {
		t.Errorf("len(PDirs) = %d, want 2 (bidirectional)", len(can.PDirs))
	}
}

func TestCanonicalize_DirectionExpansion(t *testing.T) {
	tests := []struct {
		direction string
		wantPDirs int
	}{
		{config.DirectionBidirectional, 2},
		{config.DirectionSourceToTarget, 1},
		{config.DirectionTargetToSource, 1},
	}
	for _, tc := range tests {
		t.Run(tc.direction, func(t *testing.T) {
			c := baseConfig()
			c.Pairs = append(c.Pairs, config.Pair{
				Name:      "p",
				Direction: tc.direction,
				Source:    "a@example.com",
				Target:    "b@example.com",
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
			if len(can.PDirs) != tc.wantPDirs {
				t.Errorf("len(PDirs) = %d, want %d", len(can.PDirs), tc.wantPDirs)
			}
		})
	}
}

func TestCanonicalize_DisabledPairSkipped(t *testing.T) {
	c := baseConfig()
	c.Pairs = []config.Pair{
		{
			Name:      "active",
			Direction: config.DirectionSourceToTarget,
			Source:    "a@example.com",
			Target:    "b@example.com",
		},
		{
			Name:      "off",
			Direction: config.DirectionBidirectional,
			Source:    "c@example.com",
			Target:    "d@example.com",
			Enabled:   enabled(false),
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
				Name:      "p",
				Direction: config.DirectionSourceToTarget,
				Source:    "src@example.com",
				Target:    "dst@example.com",
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
	// sourceRole/targetRole are the access roles the lister returns for
	// the pair's literal Source/Target fields; the direction determines
	// which side actually plays the read vs write role.
	tests := []struct {
		name        string
		direction   string
		sourceRole  string
		targetRole  string
		wantInvalid bool
	}{
		// source_to_target: TOML source reads, TOML target writes.
		{"s2t source freeBusyReader rejected", config.DirectionSourceToTarget, "freeBusyReader", "owner", true},
		{"s2t source reader OK", config.DirectionSourceToTarget, "reader", "owner", false},
		{"s2t target reader rejected", config.DirectionSourceToTarget, "owner", "reader", true},
		{"s2t target writer OK", config.DirectionSourceToTarget, "owner", "writer", false},

		// target_to_source: roles swap. TOML target is the actual read
		// source (needs >= reader); TOML source is the actual write
		// target (needs >= writer). Verify the swap is wired correctly.
		{"t2s TOML-target=freeBusyReader rejected (it's the read source)", config.DirectionTargetToSource, "owner", "freeBusyReader", true},
		{"t2s TOML-target=reader OK (read source)", config.DirectionTargetToSource, "owner", "reader", false},
		{"t2s TOML-source=reader rejected (it's the write target)", config.DirectionTargetToSource, "reader", "owner", true},
		{"t2s TOML-source=writer OK", config.DirectionTargetToSource, "writer", "owner", false},

		// bidirectional: both calendars play both roles, both must be writer+.
		{"bidi with source=reader rejected", config.DirectionBidirectional, "reader", "writer", true},
		{"bidi with target=reader rejected", config.DirectionBidirectional, "writer", "reader", true},
		{"bidi with both writer OK", config.DirectionBidirectional, "writer", "writer", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			c.Pairs = []config.Pair{{
				Name:      "p",
				Direction: tc.direction,
				Source:    "src@example.com",
				Target:    "dst@example.com",
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
		Name:      "off",
		Direction: config.DirectionBidirectional,
		Source:    "fr@example.com",
		Target:    "fr2@example.com",
		Enabled:   enabled(false),
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
		Name:      "selfsync",
		Direction: config.DirectionSourceToTarget,
		Source:    "primary",
		Target:    "alice@example.com",
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
		{Name: "one", Direction: config.DirectionSourceToTarget, Source: "a@example.com", Target: "b@example.com"},
		{Name: "two", Direction: config.DirectionSourceToTarget, Source: "a@example.com", Target: "b@example.com"},
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
		Name:      "p",
		Direction: config.DirectionSourceToTarget,
		Source:    "src@example.com",
		Target:    "dst@example.com",
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
		{Name: "one", Direction: config.DirectionSourceToTarget, Source: "a@example.com", Target: "b@example.com"},
		{Name: "two", Direction: config.DirectionSourceToTarget, Source: "a@example.com", Target: "c@example.com"},
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

func TestCanonicalize_PreservesTimeZoneOnPDir(t *testing.T) {
	c := baseConfig()
	c.Pairs = []config.Pair{{
		Name:      "tz",
		Direction: config.DirectionSourceToTarget,
		Source:    "a@example.com",
		Target:    "b@example.com",
		TimeZone:  "America/New_York",
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
		{Name: "dup", Direction: config.DirectionSourceToTarget, Source: "a@example.com", Target: "b@example.com"},
		{Name: "dup", Direction: config.DirectionSourceToTarget, Source: "c@example.com", Target: "d@example.com"},
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
