package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration wraps time.Duration with TOML text-unmarshaling that supports a
// "d" (days) suffix on top of Go's standard duration syntax (60s, 5m, 24h).
// SPEC.md "Settings" calls this out explicitly: "Duration strings follow
// Go's time.ParseDuration syntax plus d (days) which calendar-sync adds."
type Duration time.Duration

// Duration returns the underlying time.Duration. Use this everywhere
// outside the parser - downstream code shouldn't care that we wrapped it.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// UnmarshalText parses TOML strings into a Duration. Accepts:
//   - "<n>d" forms ("365d", "1d") - converted to N*24h.
//   - Anything time.ParseDuration accepts ("60s", "5m", "24h", "1h30m").
//
// Returns a wrapped error pointing at the offending text on failure so
// the validator can surface a useful detail.
func (d *Duration) UnmarshalText(text []byte) error {
	s := string(text)
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(rest)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(time.Duration(days) * 24 * time.Hour)
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// String formats a Duration. Used in error messages, not for round-tripping
// the original TOML text - we don't preserve "365d" notation through a
// parse-emit cycle.
func (d Duration) String() string {
	return time.Duration(d).String()
}
