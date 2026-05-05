package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		Settings: Settings{
			PollInterval:     Duration(60 * time.Second),
			Horizon:          Duration(365 * 24 * time.Hour),
			FullSyncInterval: Duration(24 * time.Hour),
			LogLevel:         LogLevelInfo,
			LogFormat:        LogFormatJSON,
		},
		Pairs: []Pair{
			{
				Name:   "work-personal",
				Source: CalendarRef{ID: "alice@example.com"},
				Target: CalendarRef{ID: "alice.personal@example.org"},
			},
		},
	}
}

func TestValidate_AcceptsValidConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate returned error on valid config: %v", err)
	}
}

func TestValidate_PollIntervalMinimum(t *testing.T) {
	c := validConfig()
	c.Settings.PollInterval = Duration(10 * time.Second)
	err := c.Validate()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v; want ErrInvalid", err)
	}
}

func TestValidate_HorizonRange(t *testing.T) {
	tests := []struct {
		name string
		dur  time.Duration
		ok   bool
	}{
		{"below 1d", 12 * time.Hour, false},
		{"at 1d", 24 * time.Hour, true},
		{"at 730d", 730 * 24 * time.Hour, true},
		{"above 730d", 731 * 24 * time.Hour, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Settings.Horizon = Duration(tc.dur)
			err := c.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate returned error: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate err = %v; want ErrInvalid", err)
			}
		})
	}
}

// TestValidate_PairHorizonBounds pins the per-pair horizon override:
// when [[pairs]].horizon is set, it's validated against the same 1d..730d
// bounds as [settings].horizon. nil (the unset case) is allowed and
// canonicalizes to the settings default - validation must NOT reject nil
// even though the bare zero-value Duration would be below the minimum.
func TestValidate_PairHorizonBounds(t *testing.T) {
	atDur := func(d time.Duration) *Duration {
		v := Duration(d)
		return &v
	}
	tests := []struct {
		name string
		dur  *Duration
		ok   bool
	}{
		{"nil falls back to settings", nil, true},
		{"at 1d", atDur(24 * time.Hour), true},
		{"at 730d", atDur(730 * 24 * time.Hour), true},
		{"below 1d", atDur(12 * time.Hour), false},
		{"above 730d", atDur(731 * 24 * time.Hour), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Pairs[0].Horizon = tc.dur
			err := c.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate returned error: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate err = %v; want ErrInvalid", err)
			}
		})
	}
}

func TestValidate_FullSyncIntervalRange(t *testing.T) {
	tests := []struct {
		name string
		dur  time.Duration
		ok   bool
	}{
		{"below 1h", 30 * time.Minute, false},
		{"at 1h", 1 * time.Hour, true},
		{"at 30d", 30 * 24 * time.Hour, true},
		{"above 30d", 31 * 24 * time.Hour, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Settings.FullSyncInterval = Duration(tc.dur)
			err := c.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate err: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v; want ErrInvalid", err)
			}
		})
	}
}

func TestValidate_LogLevel(t *testing.T) {
	for _, lvl := range []string{LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError} {
		t.Run("ok/"+lvl, func(t *testing.T) {
			c := validConfig()
			c.Settings.LogLevel = lvl
			if err := c.Validate(); err != nil {
				t.Errorf("rejected valid log level %q: %v", lvl, err)
			}
		})
	}
	for _, bad := range []string{"trace", "INFO", "verbose", ""} {
		t.Run("bad/"+bad, func(t *testing.T) {
			c := validConfig()
			c.Settings.LogLevel = bad
			if !errors.Is(c.Validate(), ErrInvalid) {
				t.Errorf("accepted bad log level %q", bad)
			}
		})
	}
}

func TestValidate_LogFormat(t *testing.T) {
	for _, fmt := range []string{LogFormatJSON, LogFormatText} {
		t.Run("ok/"+fmt, func(t *testing.T) {
			c := validConfig()
			c.Settings.LogFormat = fmt
			if err := c.Validate(); err != nil {
				t.Errorf("rejected valid log format %q: %v", fmt, err)
			}
		})
	}
	for _, bad := range []string{"yaml", "JSON", "plain", ""} {
		t.Run("bad/"+bad, func(t *testing.T) {
			c := validConfig()
			c.Settings.LogFormat = bad
			if !errors.Is(c.Validate(), ErrInvalid) {
				t.Errorf("accepted bad log format %q", bad)
			}
		})
	}
}

func TestValidate_PairNameRegex(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"work-personal", true},
		{"a", true},
		{"123", true},
		{"work-personal-2", true},
		{"a" + repeat("b", 62), true}, // 63 total - max length
		{"a" + repeat("b", 63), false},
		{"-leading-hyphen", false},
		{"Has-Uppercase", false},
		{"under_score", false},
		{"with space", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Pairs[0].Name = tc.name
			err := c.Validate()
			ok := err == nil
			if ok != tc.want {
				t.Errorf("name %q: ok = %v, want %v (err: %v)", tc.name, ok, tc.want, err)
			}
		})
	}
}

// TestValidate_RejectsDirectionField pins the v2.0.0 breaking change:
// every non-empty direction value is rejected with a migration hint
// pointing at the two-pair pattern. Callers who set the field on any
// pair (regardless of value) must update their config.
func TestValidate_RejectsDirectionField(t *testing.T) {
	for _, dir := range []string{
		"source_to_target",
		"target_to_source",
		"bidirectional",
		"a_to_b",
		"left_to_right",
		"BIDIRECTIONAL",
	} {
		t.Run(dir, func(t *testing.T) {
			c := validConfig()
			c.Pairs[0].Direction = dir
			err := c.Validate()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v; want ErrInvalid for direction=%q", err, dir)
			}
			if !strings.Contains(err.Error(), "removed in v2.0.0") {
				t.Errorf("err message %q missing migration hint about v2.0.0", err.Error())
			}
			if !strings.Contains(err.Error(), "declare two pairs") {
				t.Errorf("err message %q missing two-pair migration hint", err.Error())
			}
		})
	}
}

// TestValidate_AcceptsEmptyDirection: post-2.0.0 the field is implicit;
// callers MUST leave it unset and the validator must not complain.
func TestValidate_AcceptsEmptyDirection(t *testing.T) {
	c := validConfig()
	c.Pairs[0].Direction = ""
	if err := c.Validate(); err != nil {
		t.Errorf("rejected empty direction (the new default): %v", err)
	}
}

func TestValidate_RawSourceEqualsTarget(t *testing.T) {
	c := validConfig()
	c.Pairs[0].Source = CalendarRef{ID: "alice@example.com"}
	c.Pairs[0].Target = CalendarRef{ID: "alice@example.com"}
	if !errors.Is(c.Validate(), ErrInvalid) {
		t.Errorf("expected ErrInvalid for source==target")
	}
}

func TestValidate_ZeroPairsIsAllowed(t *testing.T) {
	// SPEC.md "Validation rules" does not require at least one pair;
	// a config that disables every pair (or has none) is technically
	// valid - it just produces a daemon with nothing to do. Document
	// the current behavior so any future tightening is intentional.
	c := validConfig()
	c.Pairs = nil
	if err := c.Validate(); err != nil {
		t.Errorf("zero-pair config should validate; got %v", err)
	}
}

func TestValidate_DuplicatePairNames(t *testing.T) {
	c := validConfig()
	c.Pairs = append(c.Pairs, Pair{
		Name:   c.Pairs[0].Name,
		Source: CalendarRef{ID: "x@example.com"},
		Target: CalendarRef{ID: "y@example.com"},
	})
	if !errors.Is(c.Validate(), ErrInvalid) {
		t.Errorf("expected ErrInvalid for duplicate pair name")
	}
}

func TestValidate_RequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Pair)
	}{
		{"name empty", func(p *Pair) { p.Name = "" }},
		{"source empty", func(p *Pair) { p.Source = CalendarRef{} }},
		{"target empty", func(p *Pair) { p.Target = CalendarRef{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c.Pairs[0])
			if !errors.Is(c.Validate(), ErrInvalid) {
				t.Errorf("expected ErrInvalid")
			}
		})
	}
}

// TestValidate_CalendarRefAcceptsSummaryForm pins the F1 happy path: a
// pair with an inline-table source and a string target validates cleanly
// (canonicalize-time resolution then routes the summary lookup).
func TestValidate_CalendarRefAcceptsSummaryForm(t *testing.T) {
	c := validConfig()
	c.Pairs[0].Source = CalendarRef{Summary: "TripIt"}
	c.Pairs[0].Target = CalendarRef{ID: "primary"}
	if err := c.Validate(); err != nil {
		t.Errorf("rejected summary-form source: %v", err)
	}
}

// TestValidate_CalendarRefRejectsAccountWithoutSummary covers the new
// rule from the F1 plan: account without summary is malformed.
// UnmarshalTOML is permissive on the shape; validate.go surfaces the
// required-field error so the user gets a uniform JSON error envelope.
func TestValidate_CalendarRefRejectsAccountWithoutSummary(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Pair)
	}{
		{
			name: "source account without summary",
			mutate: func(p *Pair) {
				p.Source = CalendarRef{Account: "alice@example.com"}
			},
		},
		{
			name: "target account without summary",
			mutate: func(p *Pair) {
				p.Target = CalendarRef{Account: "alice@example.com"}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c.Pairs[0])
			err := c.Validate()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v; want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), "account") {
				t.Errorf("err message %q should mention account", err.Error())
			}
			if !strings.Contains(err.Error(), "summary") {
				t.Errorf("err message %q should mention summary", err.Error())
			}
		})
	}
}

// TestValidate_CalendarRefRejectsEmptyTable covers the {} table case
// (no summary key at all). UnmarshalTOML accepts it; validatePair must
// surface it as missing-required-field.
func TestValidate_CalendarRefRejectsEmptyTable(t *testing.T) {
	c := validConfig()
	c.Pairs[0].Source = CalendarRef{}
	if !errors.Is(c.Validate(), ErrInvalid) {
		t.Errorf("expected ErrInvalid for fully-empty CalendarRef")
	}
}

func TestAccessRoleAtLeast(t *testing.T) {
	tests := []struct {
		actual string
		min    string
		want   bool
	}{
		{AccessRoleOwner, AccessRoleWriter, true},
		{AccessRoleWriter, AccessRoleWriter, true},
		{AccessRoleReader, AccessRoleWriter, false},
		{AccessRoleFreeBusyReader, AccessRoleReader, false},
		{AccessRoleReader, AccessRoleReader, true},
		{AccessRoleOwner, AccessRoleReader, true},
		{"unknown", AccessRoleReader, false},
		// Unknown MINIMUM returns false: fail closed on programmer error
		// or future-API surprise so an unrecognized requirement isn't
		// silently approved.
		{AccessRoleReader, "unknown", false},
		{AccessRoleOwner, "unknown", false},
	}
	for _, tc := range tests {
		got := AccessRoleAtLeast(tc.actual, tc.min)
		if got != tc.want {
			t.Errorf("AccessRoleAtLeast(%q, %q) = %v, want %v",
				tc.actual, tc.min, got, tc.want)
		}
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
