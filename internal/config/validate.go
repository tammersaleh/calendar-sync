package config

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrInvalid is the sentinel returned for any config validation failure.
// Callers do errors.Is(err, config.ErrInvalid) to map to SPEC's
// config_invalid error code; the returned error wraps a detail message
// that names the offending field.
var ErrInvalid = errors.New("config invalid")

// nameRegex enforces SPEC.md's pair name pattern: ^[a-z0-9][a-z0-9-]{0,62}$.
// 63 characters max, lowercase alphanumeric plus hyphens (not at start).
var nameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Validate runs every SPEC.md "Validation rules" check that does NOT
// require Calendar API access. Canonicalization-dependent rules (after-
// canonicalization source != target, no two pdirs sharing a triple,
// accessRole minimums) live in the canonicalize step; running this
// function is enough to reject malformed TOML before any subprocess work.
//
// Returns errors wrapping ErrInvalid; the wrapped message names the
// offending field for SPEC's user-facing 'detail' output. Value receiver
// because Validate does not mutate the Config.
func (c Config) Validate() error {
	if err := validateSettings(c.Settings); err != nil {
		return err
	}

	seenNames := make(map[string]struct{}, len(c.Pairs))
	for i, p := range c.Pairs {
		if err := validatePair(i, p); err != nil {
			return err
		}
		if _, dup := seenNames[p.Name]; dup {
			return fmt.Errorf("%w: duplicate pair name %q", ErrInvalid, p.Name)
		}
		seenNames[p.Name] = struct{}{}
	}
	return nil
}

func validateSettings(s Settings) error {
	const (
		minPoll     = 15 * time.Second
		minHorizon  = 24 * time.Hour
		maxHorizon  = 730 * 24 * time.Hour
		minFullSync = 1 * time.Hour
		maxFullSync = 30 * 24 * time.Hour
	)

	if d := s.PollInterval.Duration(); d < minPoll {
		return fmt.Errorf("%w: settings.poll_interval %s is below minimum %s",
			ErrInvalid, d, minPoll)
	}
	if d := s.Horizon.Duration(); d < minHorizon || d > maxHorizon {
		return fmt.Errorf("%w: settings.horizon %s outside allowed range %s..%s",
			ErrInvalid, d, minHorizon, maxHorizon)
	}
	if d := s.FullSyncInterval.Duration(); d < minFullSync || d > maxFullSync {
		return fmt.Errorf("%w: settings.full_sync_interval %s outside allowed range %s..%s",
			ErrInvalid, d, minFullSync, maxFullSync)
	}

	switch s.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		return fmt.Errorf("%w: settings.log_level %q not one of debug/info/warn/error",
			ErrInvalid, s.LogLevel)
	}

	switch s.LogFormat {
	case LogFormatJSON, LogFormatText:
	default:
		return fmt.Errorf("%w: settings.log_format %q not one of json/text",
			ErrInvalid, s.LogFormat)
	}
	return nil
}

func validatePair(idx int, p Pair) error {
	if p.Name == "" {
		return fmt.Errorf("%w: pairs[%d].name is required", ErrInvalid, idx)
	}
	if !nameRegex.MatchString(p.Name) {
		return fmt.Errorf("%w: pairs[%d].name %q does not match ^[a-z0-9][a-z0-9-]{0,62}$",
			ErrInvalid, idx, p.Name)
	}

	if p.Direction != "" {
		return fmt.Errorf("%w: pair %q has direction = %q; the direction field was removed in v2.0.0. Remove the field (every pair is now source-to-target); for bidirectional sync, declare two pairs with swapped source/target",
			ErrInvalid, p.Name, p.Direction)
	}

	if p.Source == "" {
		return fmt.Errorf("%w: pairs[%q].source is required", ErrInvalid, p.Name)
	}
	if p.Target == "" {
		return fmt.Errorf("%w: pairs[%q].target is required", ErrInvalid, p.Name)
	}
	// Pre-canonicalization source==target catches the typo case early.
	// The canonicalize step also re-checks after primary-resolution.
	if p.Source == p.Target {
		return fmt.Errorf("%w: pairs[%q] cannot mirror a calendar to itself (source == target)",
			ErrInvalid, p.Name)
	}
	return nil
}
