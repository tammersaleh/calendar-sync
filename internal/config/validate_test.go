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
				Source: "alice@example.com",
				Target: "alice.personal@example.org",
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
	c.Pairs[0].Source = "alice@example.com"
	c.Pairs[0].Target = "alice@example.com"
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
		Source: "x@example.com",
		Target: "y@example.com",
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
		{"source empty", func(p *Pair) { p.Source = "" }},
		{"target empty", func(p *Pair) { p.Target = "" }},
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
