// Package config loads, validates, and canonicalizes calendar-sync's TOML
// configuration. The on-disk schema lives in SPEC.md "Configuration"; this
// package's types mirror it field-for-field. Parsing produces a *Config
// with defaults filled in; Validate enforces the schema rules; the
// canonicalization step (separate file) resolves "primary" aliases to
// canonical Calendar IDs and captures each calendar's accessRole via the
// gws subprocess.
package config

// Config is the top-level TOML document. An entirely empty file produces a
// Config with default Settings and no Pairs (which then fails validation
// because at least one pair is implied by usage).
type Config struct {
	Settings Settings `toml:"settings"`
	Pairs    []Pair   `toml:"pairs"`
}

// Settings is the [settings] block. Every field has a default applied
// after parsing so an absent [settings] block produces a usable Config.
type Settings struct {
	PollInterval         Duration `toml:"poll_interval"`
	Horizon              Duration `toml:"horizon"`
	FullSyncInterval     Duration `toml:"full_sync_interval"`
	LogLevel             string   `toml:"log_level"`
	LogFormat            string   `toml:"log_format"`
	DryRun               bool     `toml:"dry_run"`
	PropagateTargetEdits bool     `toml:"propagate_target_edits"`
}

// Pair is one [[pairs]] entry. Enabled is a *bool so an unset value
// (the common case) defaults to true; only an explicit `enabled = false`
// disables a pair.
//
// Direction is the deprecated v1 field, removed in v2.0.0. The struct
// still binds it so toml.Unmarshal can populate the value and validation
// can reject any config that still sets it (toml.Unmarshal silently
// ignores unknown keys, so a non-bound field would produce a confusing
// "no effect" rather than a clear migration hint). New configs must omit
// the field entirely; bidirectional setups declare two pairs with
// swapped source/target.
type Pair struct {
	Name      string `toml:"name"`
	Direction string `toml:"direction"`
	Source    string `toml:"source"`
	Target    string `toml:"target"`
	Enabled   *bool  `toml:"enabled"`
	TimeZone  string `toml:"time_zone"`
}

// IsEnabled returns whether the pair should be processed. Enabled defaults
// to true when not set in TOML.
func (p Pair) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// Log levels per SPEC.md.
const (
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

// Log formats per SPEC.md.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Calendar API access roles. Used by validation and the per-pdir
// source_writable flag once canonicalization has populated them.
const (
	AccessRoleFreeBusyReader = "freeBusyReader"
	AccessRoleReader         = "reader"
	AccessRoleWriter         = "writer"
	AccessRoleOwner          = "owner"
)

// accessRoleRank maps each role to a comparable integer so callers can
// say "min role >= writer" without an enum library.
var accessRoleRank = map[string]int{
	AccessRoleFreeBusyReader: 1,
	AccessRoleReader:         2,
	AccessRoleWriter:         3,
	AccessRoleOwner:          4,
}

// AccessRoleAtLeast reports whether actual >= minimum on Calendar API's
// implicit ordering (freeBusyReader < reader < writer < owner). An
// unrecognized actual role ranks 0 and never satisfies any known
// minimum. An unrecognized MINIMUM also returns false: the function
// fails closed on programmer error or future-API surprises rather than
// silently approving every request.
func AccessRoleAtLeast(actual, minimum string) bool {
	minRank, ok := accessRoleRank[minimum]
	if !ok {
		return false
	}
	return accessRoleRank[actual] >= minRank
}
