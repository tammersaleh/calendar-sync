package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
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
	Feeds     []CanonicalFeed
}

// CanonicalFeed is the post-resolution view of one enabled [[feeds]] entry:
// the target ref resolved to a canonical (writable) calendar ID and the URL
// secret resolved from either the inline url or the named env var.
//
// URL is a bearer secret; never log it directly. RedactedURL is the only
// form safe for logs and `config show`.
type CanonicalFeed struct {
	Name            string
	URL             string // resolved secret (from Feed.URL or os.Getenv(Feed.URLEnv)); MarshalJSON redacts it - see below
	TargetCalendar  string // canonical ID
	ForceAllDayBusy bool   // force imported all-day events Busy regardless of TRANSP
}

// RedactedURL returns a log-safe rendering of the feed URL: "scheme://host/
// <redacted>" with the path, query, and fragment (which carry the secret
// token) dropped entirely. If the URL fails to parse or has no host, it
// returns "<redacted>" so a malformed secret can't leak through the error
// path either.
func (f CanonicalFeed) RedactedURL() string {
	u, err := url.Parse(f.URL)
	if err != nil || u.Host == "" {
		return "<redacted>"
	}
	return u.Scheme + "://" + u.Host + "/<redacted>"
}

// MarshalJSON makes CanonicalFeed fail-safe to serialize: it emits the
// REDACTED url, never the raw secret. Without this, a future `config show`
// wiring or a stray %+v/json.Marshal of a CanonicalFeed would leak the bearer
// token in Field.URL. Callers that need the live URL read the field directly
// (in memory); anything that serializes gets the redacted form.
func (f CanonicalFeed) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name            string `json:"name"`
		URL             string `json:"url"`
		TargetCalendar  string `json:"target_calendar"`
		ForceAllDayBusy bool   `json:"force_all_day_busy"`
	}{
		Name:            f.Name,
		URL:             f.RedactedURL(),
		TargetCalendar:  f.TargetCalendar,
		ForceAllDayBusy: f.ForceAllDayBusy,
	})
}

// CalendarLister is the gws-subprocess capability Canonicalize needs.
// Defined here so tests can provide a stub without spinning up the
// fake-gws harness for unit-level coverage. *gws.Client implements both
// methods natively; production callers pass it directly.
//
// CalendarListGet resolves a single calendar ID to its CalendarListEntry
// (used for the pre-F1 ID-form refs and for the "primary" alias).
//
// CalendarListList returns every calendar visible to the gws-authenticated
// account. Used to back F1 summary lookups: a single call suffices to
// build the case-insensitive summary->entries index that resolves every
// summary-form ref across all enabled pairs.
type CalendarLister interface {
	CalendarListGet(ctx context.Context, calendarID string) (*gws.CalendarListEntry, error)
	CalendarListList(ctx context.Context) ([]gws.CalendarListEntry, error)
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

	feeds, err := expandFeeds(c, calendars, refToCanonical)
	if err != nil {
		return nil, err
	}

	return &Canonical{
		Settings:  c.Settings,
		Calendars: calendars,
		PDirs:     pdirs,
		Feeds:     feeds,
	}, nil
}

// resolveCalendars walks every distinct calendar reference across all pairs
// and resolves it via lister exactly once. Returns the resolved map (keyed
// by canonical ID) plus a "ref-key → canonical ID" lookup so the pdir-
// expansion step can map a Pair's literal Source/Target back to a resolved
// Calendar in O(1).
//
// ID-form refs route through CalendarListGet exactly as before. Summary-
// form refs (F1) all share a single CalendarListList call that builds a
// case-insensitive summary->entries index; each summary-form ref then
// resolves against that index. The list call is skipped entirely when no
// enabled pair carries a summary-form ref, preserving the pre-F1 cost
// for existing configs.
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

	// Pre-fetch the full list ONCE if any enabled pair has a summary ref.
	// nil signals "no summary lookups needed" to the resolver below; the
	// list call is skipped in that case so existing ID-only configs keep
	// their pre-F1 cost profile.
	var summaryIndex map[string][]gws.CalendarListEntry
	if anyEnabledSummaryRef(c) {
		entries, err := lister.CalendarListList(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("calendarList.list: %w", err)
		}
		summaryIndex = buildSummaryIndex(entries)
	}

	resolve := func(ref CalendarRef) error {
		key := refKey(ref)
		if _, ok := refToCanonical[key]; ok {
			return nil
		}
		entry, err := resolveOne(ctx, ref, lister, summaryIndex)
		if err != nil {
			return err
		}
		if entry.ID == "" {
			return fmt.Errorf("%w: gws returned empty id resolving %s",
				ErrInvalid, refDescription(ref))
		}
		if _, exists := resolved[entry.ID]; !exists {
			resolved[entry.ID] = Calendar{
				CanonicalID: entry.ID,
				AccessRole:  entry.AccessRole,
				OriginalRef: originalRef(ref),
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

	// Enabled feed targets resolve in the SAME pass as pair refs so a single
	// CalendarListList still covers every summary lookup. Disabled feeds are
	// skipped entirely, like disabled pairs.
	for _, f := range c.Feeds {
		if !f.IsEnabled() {
			continue
		}
		if err := resolve(f.Target); err != nil {
			return nil, nil, err
		}
	}
	return resolved, refToCanonical, nil
}

// anyEnabledSummaryRef reports whether any enabled pair carries a summary-
// form ref on either side. Used to skip the CalendarListList call entirely
// for configs that only use ID-form refs.
func anyEnabledSummaryRef(c Config) bool {
	for _, p := range c.Pairs {
		if !p.IsEnabled() {
			continue
		}
		if p.Source.IsSummaryRef() || p.Target.IsSummaryRef() {
			return true
		}
	}
	for _, f := range c.Feeds {
		if !f.IsEnabled() {
			continue
		}
		if f.Target.IsSummaryRef() {
			return true
		}
	}
	return false
}

// buildSummaryIndex groups CalendarListList entries by their lowercased
// effective summary so summary lookups can match case-insensitively.
//
// "Effective summary" is summaryOverride when set, otherwise summary.
// SummaryOverride is the user's per-calendar display name in Google's UI:
// the underlying TripIt subscription has summary="Tammer Saleh (TripIt)"
// but the user only ever sees their override "TripIt" in the calendar
// list. Indexing by override-first matches what the user typed in TOML.
func buildSummaryIndex(entries []gws.CalendarListEntry) map[string][]gws.CalendarListEntry {
	out := make(map[string][]gws.CalendarListEntry, len(entries))
	for _, e := range entries {
		key := strings.ToLower(effectiveSummary(e))
		out[key] = append(out[key], e)
	}
	return out
}

// effectiveSummary returns the user-visible name: SummaryOverride when
// non-empty, falling back to Summary.
func effectiveSummary(e gws.CalendarListEntry) string {
	if e.SummaryOverride != "" {
		return e.SummaryOverride
	}
	return e.Summary
}

// resolveOne resolves a single CalendarRef. ID-form routes through
// CalendarListGet (same call shape as the pre-F1 path). Summary-form
// looks up against the pre-built summaryIndex; ambiguity is disambiguated
// via account-substring filtering, falling through to a clear ErrInvalid
// when zero or many calendars match.
func resolveOne(
	ctx context.Context,
	ref CalendarRef,
	lister CalendarLister,
	summaryIndex map[string][]gws.CalendarListEntry,
) (*gws.CalendarListEntry, error) {
	if !ref.IsSummaryRef() {
		entry, err := lister.CalendarListGet(ctx, ref.ID)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", ref.ID, err)
		}
		return entry, nil
	}
	return resolveSummaryRef(ref, summaryIndex)
}

// resolveSummaryRef performs the summary-lookup walk:
//   - 0 matches: ErrInvalid - the user typed a name that doesn't exist
//     under the gws-authenticated account.
//   - 1 match: success.
//   - N matches with no account: ErrInvalid listing every match so the
//     user can see exactly what to disambiguate against.
//   - N matches with account: filter by the two-step match (DataOwner
//     equality preferred, ID-substring fallback). Yield 1: success.
//     Yield 0 / 2+: ErrInvalid with the relevant detail.
//
// Account disambiguation prefers DataOwner because Google reports it
// authoritatively for secondary calendars and group calendars whose IDs
// (`c_<hash>@group.calendar.google.com`) carry no account information.
// When DataOwner is empty (the user's own primary, some shared
// calendars), fall back to the case-insensitive ID-substring heuristic.
// Both fallbacks still fail for "<random>@import.calendar.google.com"-
// shaped IDs whose hash carries no account context; those users fall
// back to bare ID refs.
func resolveSummaryRef(
	ref CalendarRef,
	summaryIndex map[string][]gws.CalendarListEntry,
) (*gws.CalendarListEntry, error) {
	matches := summaryIndex[strings.ToLower(ref.Summary)]
	if len(matches) == 0 {
		return nil, fmt.Errorf("%w: no calendar with summary %q visible to this account",
			ErrInvalid, ref.Summary)
	}

	// When account is set, the user's intent is "pick the calendar owned
	// by this account". Honor it even when only one calendar matches the
	// summary - returning a single match owned by someone else would
	// silently violate the user's stated constraint.
	if ref.Account != "" {
		accountLower := strings.ToLower(ref.Account)
		var filtered []gws.CalendarListEntry
		for _, e := range matches {
			if matchesAccount(e, ref.Account, accountLower) {
				filtered = append(filtered, e)
			}
		}
		switch len(filtered) {
		case 0:
			return nil, fmt.Errorf(
				"%w: summary %q matches %d calendars but none has DataOwner or ID matching account %q; matches were: %s",
				ErrInvalid, ref.Summary, len(matches), ref.Account, formatMatchIDs(matches))
		case 1:
			entry := filtered[0]
			return &entry, nil
		default:
			return nil, fmt.Errorf(
				"%w: summary %q with account %q is still ambiguous: %s",
				ErrInvalid, ref.Summary, ref.Account, formatMatchIDs(filtered))
		}
	}

	if len(matches) == 1 {
		entry := matches[0]
		return &entry, nil
	}

	return nil, fmt.Errorf(
		"%w: summary %q matches %d calendars: %s; add account = \"...\" to disambiguate",
		ErrInvalid, ref.Summary, len(matches), formatMatchIDs(matches))
}

// matchesAccount applies the two-step disambiguation match: prefer
// DataOwner equality when Google populated the field; otherwise fall
// back to the case-insensitive ID-substring heuristic.
func matchesAccount(e gws.CalendarListEntry, account, accountLower string) bool {
	if e.DataOwner != "" {
		return strings.EqualFold(e.DataOwner, account)
	}
	return strings.Contains(strings.ToLower(e.ID), accountLower)
}

// formatMatchIDs formats a list of CalendarListEntry IDs as a
// `[id1, id2, id3]` string for error messages.
func formatMatchIDs(entries []gws.CalendarListEntry) string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return "[" + strings.Join(ids, ", ") + "]"
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

// originalRef returns the value to store in Calendar.OriginalRef so the
// downstream "what did the user write?" lookup matches their TOML. ID-form
// refs preserve the bare ID; summary-form refs use the descriptive form
// since there's no single string that names what the user wrote.
func originalRef(r CalendarRef) string {
	if r.IsSummaryRef() {
		return refDescription(r)
	}
	return r.ID
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

// expandFeeds resolves each enabled [[feeds]] entry into a CanonicalFeed: the
// target ref resolved to a canonical (writable) calendar and the URL secret
// resolved from url or url_env. Disabled feeds produce nothing, mirroring
// disabled pairs. Refs were already resolved by resolveCalendars in the same
// pass as pair refs, so this step only looks them up and applies the
// writable-target rule (the importer writes to the target).
func expandFeeds(c Config, calendars map[string]Calendar, refToCanonical map[string]string) ([]CanonicalFeed, error) {
	var out []CanonicalFeed
	for _, f := range c.Feeds {
		if !f.IsEnabled() {
			continue
		}

		targetCal, ok := calendars[refToCanonical[refKey(f.Target)]]
		if !ok {
			return nil, fmt.Errorf("%w: feed %q target %s not resolved",
				ErrInvalid, f.Name, refDescription(f.Target))
		}
		if err := requireFeedTarget(targetCal, f.Name); err != nil {
			return nil, err
		}

		feedURL, err := resolveFeedURL(f)
		if err != nil {
			return nil, err
		}

		out = append(out, CanonicalFeed{
			Name:            f.Name,
			URL:             feedURL,
			TargetCalendar:  targetCal.CanonicalID,
			ForceAllDayBusy: f.ForceAllDayBusy,
		})
	}
	return out, nil
}

// requireFeedTarget enforces that a feed's target calendar is writable; the
// importer inserts, patches, and deletes events on it.
func requireFeedTarget(cal Calendar, feedName string) error {
	if !AccessRoleAtLeast(cal.AccessRole, AccessRoleWriter) {
		return fmt.Errorf("%w: feed %q target %q has accessRole %q; need at least writer",
			ErrInvalid, feedName, cal.OriginalRef, cal.AccessRole)
	}
	return nil
}

// resolveFeedURL returns the feed's URL secret from the inline url or, when
// url_env is set, from that env var. Validate has already enforced the
// exactly-one rule and that the env var is set/non-empty, but re-check here
// so canonicalization never emits a CanonicalFeed with an empty URL if it's
// ever called without a prior Validate. The error names only the env var,
// never its value.
func resolveFeedURL(f Feed) (string, error) {
	if f.URL != "" {
		return f.URL, nil
	}
	if v, ok := os.LookupEnv(f.URLEnv); ok && v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%w: feed %q url_env names environment variable %q which is unset or empty",
		ErrInvalid, f.Name, f.URLEnv)
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
