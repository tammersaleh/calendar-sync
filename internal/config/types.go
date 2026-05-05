// Package config loads, validates, and canonicalizes calendar-sync's TOML
// configuration. The on-disk schema lives in SPEC.md "Configuration"; this
// package's types mirror it field-for-field. Parsing produces a *Config
// with defaults filled in; Validate enforces the schema rules; the
// canonicalization step (separate file) resolves "primary" aliases to
// canonical Calendar IDs and captures each calendar's accessRole via the
// gws subprocess.
package config

import (
	"encoding/json"
	"fmt"
)

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
	Name      string      `toml:"name"`
	Direction string      `toml:"direction"`
	Source    CalendarRef `toml:"source"`
	Target    CalendarRef `toml:"target"`
	Enabled   *bool       `toml:"enabled"`
	TimeZone  string      `toml:"time_zone"`

	// Horizon optionally overrides [settings].horizon for this pair. nil
	// (the unset case) means fall back to Settings.Horizon at canonicalization
	// time. Pointer-typed so absence is distinguishable from an explicit
	// zero. Same 1d..730d bounds as the settings field.
	Horizon *Duration `toml:"horizon"`

	// PropagateTargetEdits optionally overrides
	// [settings].propagate_target_edits for this pair. nil (the unset case)
	// means fall back to Settings.PropagateTargetEdits at canonicalization
	// time. Pointer-typed so absence is distinguishable from an explicit
	// `false` - the per-pair scoping rollout uses this distinction to ramp
	// two-way sync one direction at a time without disturbing the other.
	PropagateTargetEdits *bool `toml:"propagate_target_edits"`
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

// CalendarRef is the value of [[pairs]].source or [[pairs]].target. It
// accepts two TOML forms:
//
//   - String: the calendar ID directly (`source = "alice@example.com"`,
//     `source = "primary"`, or a group calendar ID). UnmarshalTOML stores
//     this in ID.
//   - Inline table with a `summary` key: name-based lookup
//     (`source = {summary = "TripIt"}`). The optional `account` key
//     disambiguates when multiple visible calendars share the same
//     summary (`source = {summary = "CoreWeave", account = "alice@example.com"}`).
//
// Backwards compatibility: every pre-F1 string config flows through the
// string case unchanged, producing CalendarRef{ID: ...}; downstream
// canonicalization routes ID-form refs to CalendarListGet exactly as
// before.
type CalendarRef struct {
	ID      string // set when the TOML value was a string
	Summary string // set when the TOML value was a table with key "summary"
	Account string // optional disambiguation when Summary is set
}

// IsSummaryRef reports whether this ref needs a summary-based lookup
// (vs. a direct CalendarListGet by ID).
func (r CalendarRef) IsSummaryRef() bool { return r.Summary != "" }

// UnmarshalTOML decodes either a string or an inline table into a
// CalendarRef. An empty string is rejected here; an empty summary on the
// table form is allowed at unmarshal time so validatePair can surface
// the missing-required-field error through the normal JSON envelope.
//
// Wipes the receiver up front so a reused struct (decode object, then
// re-decode as string) doesn't leave the prior union variant's field set
// alongside the new one - IsSummaryRef would otherwise flip true on a
// pure ID-form ref.
func (r *CalendarRef) UnmarshalTOML(data any) error {
	*r = CalendarRef{}
	switch v := data.(type) {
	case string:
		if v == "" {
			return fmt.Errorf("calendar ref must not be empty")
		}
		r.ID = v
		return nil
	case map[string]any:
		for k := range v {
			if k != "summary" && k != "account" {
				return fmt.Errorf("calendar ref table has unknown field %q; allowed: summary, account", k)
			}
		}
		// A missing key produces the zero value; a non-string value is a
		// user error worth a clear message. Silent coercion would surface
		// later as a confusing "required field missing" from validatePair.
		summary, err := stringField(v, "summary")
		if err != nil {
			return err
		}
		account, err := stringField(v, "account")
		if err != nil {
			return err
		}
		r.Summary = summary
		r.Account = account
		return nil
	default:
		return fmt.Errorf("calendar ref must be a string or inline table, got %T", data)
	}
}

// stringField returns m[key] as a string. Missing keys return "" with no
// error (the inline-table form's required-fields rule lives in
// validatePair). Non-string values are rejected at unmarshal time so a
// typo'd `summary = 42` surfaces as a clear type error.
func stringField(m map[string]any, key string) (string, error) {
	raw, ok := m[key]
	if !ok {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("calendar ref table field %q must be a string, got %T", key, raw)
	}
	return s, nil
}

// MarshalJSON emits the user-facing wire shape used by `pair list` /
// `config show`. ID-form refs marshal as plain strings so existing JSONL
// output (and the SPEC's documented examples) are unchanged. Summary-form
// refs marshal as objects so the output mirrors what the user wrote.
func (r CalendarRef) MarshalJSON() ([]byte, error) {
	if r.IsSummaryRef() {
		type wire struct {
			Summary string `json:"summary"`
			Account string `json:"account,omitempty"`
		}
		return json.Marshal(wire{Summary: r.Summary, Account: r.Account})
	}
	return json.Marshal(r.ID)
}

// UnmarshalJSON is the inverse of MarshalJSON: a JSON string becomes an
// ID-form ref, a JSON object becomes a summary-form ref. Required so test
// code (and any future caller) can decode the wire shape back into a typed
// CalendarRef.
//
// Wipes the receiver up front so a reused struct doesn't leave a stale
// union variant's field set; see UnmarshalTOML for the same reason.
func (r *CalendarRef) UnmarshalJSON(data []byte) error {
	*r = CalendarRef{}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		r.ID = s
		return nil
	}
	var obj struct {
		Summary string `json:"summary"`
		Account string `json:"account"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("calendar ref must be string or object: %w", err)
	}
	r.Summary = obj.Summary
	r.Account = obj.Account
	return nil
}
