package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CompactDuration formats d in the compact form SPEC.md uses across both
// the IPC status response (line 725: "60s", "24h") and `config show` (line
// 588: same). The rule is:
//
//   - whole hours (value % 1h == 0 && value > 0): emit as "<N>h"
//   - everything else: emit as "<N>s" (seconds)
//
// Deliberately doesn't auto-promote 60 seconds to "1m" or 5 minutes to
// "5m" - the SPEC examples show "60s" for the default poll_interval, and
// promoting whole minutes would round-trip the user's "60s" config to
// "1m" on the wire. Promoting whole hours IS done because SPEC shows
// "24h" (not "86400s" or "1440m") for the default full_sync_interval.
//
// Sub-second precision falls back to time.Duration.String. The settings
// validator clamps poll_interval >= 15s and full_sync_interval >= 1h so
// the fallback is unreachable in production for those two; horizon (and
// the bare config.Duration values) can in principle be sub-second.
func CompactDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

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

// Compact returns the SPEC's compact wire format ("60s", "24h"). See
// CompactDuration for the precise rule. Used by `config show` and the
// daemon's IPC status response.
func (d Duration) Compact() string {
	return CompactDuration(time.Duration(d))
}
