package config

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// PDir is the unit of sync calendar-sync operates on: one (pair, direction)
// pair with calendar IDs resolved to canonical form. SPEC.md "The unit of
// sync: pair-direction" explains why state lives at this granularity rather
// than per-pair.
//
// Direction is always "a_to_b" (source→target) post-v2.0.0; the field is
// retained for output stability (SPEC's `<pair>:<direction>` failure
// identifier and the per-Outcome `direction` JSONL field) and to keep the
// pdir-collision triple meaningful. Bidirectional setups declare two pairs.
type PDir struct {
	PairName       string
	Direction      string
	SourceCalendar string // canonical ID
	TargetCalendar string // canonical ID
	SourceWritable bool   // derived from source.AccessRole
	TimeZone       string

	// Horizon is the resolved effective horizon for this pdir: the
	// per-pair Pair.Horizon override if set, otherwise the fallback
	// Settings.Horizon. Resolved at canonicalization so downstream
	// consumers (sync/classify/orphan) read a single, non-nil value.
	Horizon time.Duration

	// PropagateTargetEdits is the resolved effective two-way-sync gate for
	// this pdir: the per-pair Pair.PropagateTargetEdits override if set,
	// otherwise the fallback Settings.PropagateTargetEdits. Resolved at
	// canonicalization so the sync layer's drift-handling gate reads a
	// single, plain-bool value rather than chasing the *bool fallback at
	// each call site. ANDed with SourceWritable in the sync layer; a
	// read-only source can never propagate regardless.
	PropagateTargetEdits bool
}

// Direction value for PDir. Pre-v2.0.0 there was also PDirBtoA for the
// reverse direction of a bidirectional pair; that's gone. Every pdir is
// a_to_b now.
const (
	PDirAtoB = "a_to_b"
)

// Calendar holds the resolved view of one calendar referenced in config.
// CanonicalID is the email or group-calendar ID Google considers
// authoritative; OriginalRef preserves whatever the user wrote in TOML
// (often "primary", which Google resolves server-side).
type Calendar struct {
	CanonicalID string
	AccessRole  string
	OriginalRef string
}

// Canonical is the post-resolution view of a Config: every calendar
// reference resolved to its canonical ID and accessRole, every pair
// expanded into one or two PDirs, and SPEC's canonicalize-time validation
// rules already enforced. The daemon and CLI commands work against this,
// not the raw Config.
type Canonical struct {
	Settings  Settings
	Calendars map[string]Calendar // keyed by canonical ID
	PDirs     []PDir
}

// CalendarLister is the gws-subprocess capability Canonicalize needs.
// Defined here so tests can provide a stub without spinning up the
// fake-gws harness for unit-level coverage; the production caller passes
// (*gws.Client).CalendarListGet directly.
type CalendarLister interface {
	CalendarListGet(ctx context.Context, calendarID string) (*gws.CalendarListEntry, error)
}

// Canonicalize resolves every calendar reference in c via lister, captures
// the accessRole each one carries, expands pairs into PDirs, and applies
// the SPEC validation rules that need canonical state:
//
//   - After canonicalization, source != target on every pair.
//   - Across all pdirs, no two share (canonical_source, canonical_target,
//     direction).
//   - Source's accessRole >= reader (freeBusyReader rejected).
//   - Target's accessRole >= writer.
//
// Returns errors wrapping ErrInvalid for validation failures. Subprocess
// errors from lister bubble up unchanged so callers can errors.Is them
// against the gws sentinels (e.g. ErrAPIAuthFailed) for correct exit-code
// propagation per SPEC.
func (c Config) Canonicalize(ctx context.Context, lister CalendarLister) (*Canonical, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	calendars, refToCanonical, err := resolveCalendars(ctx, c, lister)
	if err != nil {
		return nil, err
	}

	pdirs, err := expandPDirs(c, calendars, refToCanonical)
	if err != nil {
		return nil, err
	}

	if err := checkPDirCollisions(pdirs); err != nil {
		return nil, err
	}

	return &Canonical{
		Settings:  c.Settings,
		Calendars: calendars,
		PDirs:     pdirs,
	}, nil
}

// resolveCalendars walks every distinct calendar reference across all pairs
// and resolves it via lister exactly once. Returns the resolved map (keyed
// by canonical ID) plus a "ref-key → canonical ID" lookup so the pdir-
// expansion step can map a Pair's literal Source/Target back to a resolved
// Calendar in O(1).
//
// The first OriginalRef wins when multiple TOML references resolve to the
// same canonical ID (e.g. one pair uses "primary" and another uses the
// canonical email); subsequent references are recorded in the lookup map
// but don't replace the canonical entry. This is fine: callers only need
// the canonical ID and accessRole, both of which are identical regardless
// of which alias was first.
func resolveCalendars(ctx context.Context, c Config, lister CalendarLister) (
	resolved map[string]Calendar,
	refToCanonical map[string]string,
	err error,
) {
	resolved = make(map[string]Calendar, len(c.Pairs)*2)
	refToCanonical = make(map[string]string, len(c.Pairs)*2)

	resolve := func(ref CalendarRef) error {
		key := refKey(ref)
		if _, ok := refToCanonical[key]; ok {
			return nil
		}
		// Summary-form refs are added in a follow-up commit; for now,
		// every supported ref is ID-form and routes through CalendarListGet
		// exactly as before.
		entry, err := lister.CalendarListGet(ctx, ref.ID)
		if err != nil {
			return fmt.Errorf("resolve %q: %w", ref.ID, err)
		}
		if entry.ID == "" {
			return fmt.Errorf("%w: gws calendarList.get(%q) returned an empty id",
				ErrInvalid, ref.ID)
		}
		if _, exists := resolved[entry.ID]; !exists {
			resolved[entry.ID] = Calendar{
				CanonicalID: entry.ID,
				AccessRole:  entry.AccessRole,
				OriginalRef: ref.ID,
			}
		}
		refToCanonical[key] = entry.ID
		return nil
	}

	for _, p := range c.Pairs {
		// Disabled pairs are "skipped entirely" per SPEC.md "[[pairs]]";
		// don't waste a CalendarListGet call on a calendar nothing
		// references, and don't surface a typo'd or now-inaccessible
		// calendar as an error when the user has explicitly turned the
		// pair off.
		if !p.IsEnabled() {
			continue
		}
		if err := resolve(p.Source); err != nil {
			return nil, nil, err
		}
		if err := resolve(p.Target); err != nil {
			return nil, nil, err
		}
	}
	return resolved, refToCanonical, nil
}

// refKey returns a stable lookup key for a CalendarRef. ID-form refs key
// on the literal ID string (preserving the pre-F1 behavior where multiple
// pairs sharing a calendar dedupe to one CalendarListGet call). Summary-form
// refs key on a "summary:<lower-summary>\x00<lower-account>" sentinel so two
// pairs that name the same calendar by summary share one lookup.
func refKey(r CalendarRef) string {
	if r.IsSummaryRef() {
		return "summary:" + strings.ToLower(r.Summary) + "\x00" + strings.ToLower(r.Account)
	}
	return r.ID
}

// refDescription returns a human-readable description of a CalendarRef
// for error messages. ID-form refs render as the bare ID in quotes;
// summary-form refs render with the summary (and optional account) so
// the error names the form the user actually wrote.
func refDescription(r CalendarRef) string {
	if r.IsSummaryRef() {
		if r.Account != "" {
			return fmt.Sprintf("{summary=%q, account=%q}", r.Summary, r.Account)
		}
		return fmt.Sprintf("{summary=%q}", r.Summary)
	}
	return fmt.Sprintf("%q", r.ID)
}

// expandPDirs walks the pair list and emits one PDir per enabled pair.
// Every pair is implicitly source-to-target (v2.0.0 dropped the
// `direction` field; see SPEC.md "[[pairs]]"). For each emitted PDir it
// validates:
//   - source != target after canonicalization (catches the case where
//     two TOML refs resolve to the same canonical ID, e.g. "primary"
//     and the user's literal email).
//   - source.AccessRole >= reader.
//   - target.AccessRole >= writer.
//
// Disabled pairs (Enabled=false) are skipped entirely - they don't expand
// to any PDir and aren't validated.
func expandPDirs(c Config, calendars map[string]Calendar, refToCanonical map[string]string) ([]PDir, error) {
	var out []PDir
	for _, p := range c.Pairs {
		if !p.IsEnabled() {
			continue
		}

		sourceCal, ok := calendars[refToCanonical[refKey(p.Source)]]
		if !ok {
			return nil, fmt.Errorf("%w: pair %q source %s not resolved",
				ErrInvalid, p.Name, refDescription(p.Source))
		}
		targetCal, ok := calendars[refToCanonical[refKey(p.Target)]]
		if !ok {
			return nil, fmt.Errorf("%w: pair %q target %s not resolved",
				ErrInvalid, p.Name, refDescription(p.Target))
		}

		if sourceCal.CanonicalID == targetCal.CanonicalID {
			return nil, fmt.Errorf("%w: pair %q resolves source and target to the same canonical ID %q",
				ErrInvalid, p.Name, sourceCal.CanonicalID)
		}

		if err := requireSource(sourceCal, p.Name); err != nil {
			return nil, err
		}
		if err := requireTarget(targetCal, p.Name); err != nil {
			return nil, err
		}

		// Resolve effective horizon: per-pair override wins, settings
		// default is the fallback. Validation has already bounded both
		// against the 1d..730d range.
		horizon := c.Settings.Horizon.Duration()
		if p.Horizon != nil {
			horizon = p.Horizon.Duration()
		}
		// Resolve effective propagate-target-edits gate: per-pair override
		// wins (including an explicit `false`), settings default is the
		// fallback when the pair leaves it unset.
		propagate := c.Settings.PropagateTargetEdits
		if p.PropagateTargetEdits != nil {
			propagate = *p.PropagateTargetEdits
		}
		out = append(out, makePDir(p, sourceCal, targetCal, horizon, propagate))
	}
	return out, nil
}

func requireSource(cal Calendar, pairName string) error {
	if !AccessRoleAtLeast(cal.AccessRole, AccessRoleReader) {
		return fmt.Errorf("%w: pair %q source %q has accessRole %q; need at least reader",
			ErrInvalid, pairName, cal.OriginalRef, cal.AccessRole)
	}
	return nil
}

func requireTarget(cal Calendar, pairName string) error {
	if !AccessRoleAtLeast(cal.AccessRole, AccessRoleWriter) {
		return fmt.Errorf("%w: pair %q target %q has accessRole %q; need at least writer",
			ErrInvalid, pairName, cal.OriginalRef, cal.AccessRole)
	}
	return nil
}

func makePDir(p Pair, source, target Calendar, horizon time.Duration, propagate bool) PDir {
	return PDir{
		PairName:             p.Name,
		Direction:            PDirAtoB,
		SourceCalendar:       source.CanonicalID,
		TargetCalendar:       target.CanonicalID,
		SourceWritable:       AccessRoleAtLeast(source.AccessRole, AccessRoleWriter),
		TimeZone:             p.TimeZone,
		Horizon:              horizon,
		PropagateTargetEdits: propagate,
	}
}

// checkPDirCollisions enforces SPEC.md's "no two pdirs share the same
// (canonical_source, canonical_target, direction) triple" rule. Two pdirs
// writing identical mirrors to the same calendar is a configuration bug.
func checkPDirCollisions(pdirs []PDir) error {
	type triple struct {
		source, target, direction string
	}
	seen := make(map[triple]string, len(pdirs))
	for _, pd := range pdirs {
		k := triple{pd.SourceCalendar, pd.TargetCalendar, pd.Direction}
		if other, dup := seen[k]; dup {
			return fmt.Errorf("%w: pdirs %q and %q share (source=%q, target=%q, direction=%q)",
				ErrInvalid, other, pd.PairName, pd.SourceCalendar, pd.TargetCalendar, pd.Direction)
		}
		seen[k] = pd.PairName
	}
	return nil
}
