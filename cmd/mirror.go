package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
	"github.com/tammersaleh/calendar-sync/internal/output"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// MirrorCmd is the kong group for `mirror list` / `mirror prune`.
type MirrorCmd struct {
	List  MirrorListCmd  `cmd:"" help:"List mirror events on a calendar."`
	Prune MirrorPruneCmd `cmd:"" help:"Delete mirror events from a calendar."`
}

// MirrorListCmd implements `calendar-sync mirror list`. SPEC §"calendar-sync
// mirror list" lines 653-682.
type MirrorListCmd struct {
	Calendar  string `arg:"" name:"calendar" help:"Calendar ID to list mirrors on."`
	Pair      string `name:"pair" placeholder:"<name>" help:"Only mirrors created by this pair (any direction)."`
	Direction string `name:"direction" placeholder:"<dir>" help:"With --pair, limit to a_to_b or b_to_a."`
	Orphaned  bool   `name:"orphaned" help:"Only mirrors whose source no longer exists. Triggers per-mirror source lookup."`
	Limit     int    `name:"limit" placeholder:"<n>" default:"250" help:"Max items to return."`
	All       bool   `name:"all" help:"Fetch all pages."`
}

// Run loads config, builds the inventory for the named calendar, applies
// pair/direction/orphaned filters, and emits one JSON line per mirror.
func (c *MirrorListCmd) Run(rt *Runtime) error {
	canonical, err := loadCanonical(rt)
	if err != nil {
		return err
	}

	pdLookup, err := buildPDirLookup(canonical, c.Pair, c.Direction)
	if err != nil {
		return err
	}

	api := rt.gwsClient()
	mirrors, err := listMirrors(rt.Ctx, api, c.Calendar)
	if err != nil {
		return err
	}

	rows := []mirrorRow{}
	for _, m := range mirrors {
		row, ok := buildMirrorRow(m, pdLookup, c.Pair, c.Direction)
		if !ok {
			continue
		}
		if c.Orphaned {
			alive, lookupErr := sourceAlive(rt.Ctx, api, row.tuple)
			if lookupErr != nil {
				return lookupErr
			}
			if alive {
				continue
			}
		}
		rows = append(rows, row)
	}

	limit := c.Limit
	if limit <= 0 {
		limit = 250
	}
	hasMore := false
	if !c.All && len(rows) > limit {
		rows = rows[:limit]
		hasMore = true
	}

	p := rt.printer()
	for _, r := range rows {
		p.Emit(r)
	}
	p.Meta(mirrorListMeta{Count: len(rows), HasMore: hasMore})
	return nil
}

// MirrorPruneCmd implements `calendar-sync mirror prune`. SPEC §"calendar-sync
// mirror prune" lines 684-712.
type MirrorPruneCmd struct {
	Calendar     string        `arg:"" name:"calendar" help:"Calendar ID to prune."`
	Pair         string        `name:"pair" placeholder:"<name>" help:"Only delete mirrors created by this pair."`
	Direction    string        `name:"direction" placeholder:"<dir>" help:"With --pair, limit to a_to_b or b_to_a."`
	Orphaned     bool          `name:"orphaned" help:"Only delete mirrors whose source no longer exists."`
	All          bool          `name:"all" help:"Delete every mirror calendar-sync has ever created on this calendar."`
	PruneHorizon time.Duration `name:"prune-horizon" placeholder:"<dur>" help:"Only delete mirrors whose start falls in [now, now+dur]. Inclusive on both edges. Distinct from sync horizon."`
	DryRun       bool          `name:"dry-run" help:"List what would be deleted, do nothing."`
	Yes          bool          `short:"y" name:"yes" help:"Skip the interactive confirmation."`

	// now is a test-injection hook for the prune-horizon window. Production
	// leaves it nil (currentTime falls back to time.Now). kong ignores
	// unexported fields.
	now func() time.Time
}

// currentTime returns the wall-clock used to anchor --prune-horizon's
// [now, now+dur] window. Tests assign c.now to a fixed function;
// production uses time.Now.
func (c *MirrorPruneCmd) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Run validates the selector, builds the deletion candidate list, prompts
// (unless --yes), and emits one delete line per processed mirror.
func (c *MirrorPruneCmd) Run(rt *Runtime) error {
	if err := validatePruneSelector(c); err != nil {
		return err
	}
	canonical, err := loadCanonical(rt)
	if err != nil {
		return err
	}
	pdLookup, err := buildPDirLookup(canonical, c.Pair, c.Direction)
	if err != nil {
		return err
	}

	api := rt.gwsClient()
	mirrors, err := listMirrors(rt.Ctx, api, c.Calendar)
	if err != nil {
		return err
	}

	candidates := []mirrorRow{}
	var horizonNow, horizonEnd time.Time
	if c.PruneHorizon > 0 {
		horizonNow = c.currentTime()
		horizonEnd = horizonNow.Add(c.PruneHorizon)
	}
	for _, m := range mirrors {
		row, ok := buildMirrorRow(m, pdLookup, c.Pair, c.Direction)
		if !ok {
			continue
		}
		if c.PruneHorizon > 0 && !inPruneHorizonWindow(m.Start, horizonNow, horizonEnd) {
			continue
		}
		if c.Orphaned {
			alive, lookupErr := sourceAlive(rt.Ctx, api, row.tuple)
			if lookupErr != nil {
				return lookupErr
			}
			if alive {
				continue
			}
		}
		candidates = append(candidates, row)
	}

	if !c.Yes && !c.DryRun {
		if !isTerminal(os.Stdin) {
			return newCmdError(output.CodeConfirmationRequired,
				"refusing to prune without --yes on a non-TTY", "", nil)
		}
		if !confirm(rt.Stderr, len(candidates)) {
			return newCmdError(output.CodeConfirmationRequired,
				"prune aborted by user", "", nil)
		}
	}

	p := rt.printer()
	count := 0
	for _, row := range candidates {
		if !c.DryRun {
			if err := api.EventsDelete(rt.Ctx, c.Calendar, row.ID); err != nil {
				if errors.Is(err, gws.ErrAPINotFound) {
					// Already gone; carry on.
				} else {
					return err
				}
			}
		}
		p.Emit(prunedRow{
			Action:  pruneAction(c.DryRun),
			ID:      row.ID,
			Summary: row.Summary,
			Source:  row.Source,
		})
		count++
	}
	p.Meta(metaCount{Count: count})
	return nil
}

// listMirrors performs SPEC's two-pass inventory build (v2 then v1) on the
// target calendar and returns the merged event slice. We don't reuse
// sync.BuildInventory directly because we want the raw events, not a
// SourceTuple-keyed map.
func listMirrors(ctx context.Context, api syncpkg.API, calendar string) ([]gws.Event, error) {
	var all []gws.Event
	for _, version := range []string{mirror.SchemaVersion, "1"} {
		params := gws.EventsListParams{
			CalendarID:              calendar,
			ShowDeleted:             true,
			PrivateExtendedProperty: []string{mirror.ExtKeyVersion + "=" + version},
		}
		events, _, err := api.EventsList(ctx, params)
		if err != nil {
			return nil, err
		}
		all = append(all, events...)
	}
	return all, nil
}

// pdirInfo is the resolved pdir context attached to each mirror row.
type pdirInfo struct {
	Pair      string
	Direction string
}

// buildPDirLookup returns a map keyed by (sourceCalendar, targetCalendar)
// to the matching pdir info. Used by buildMirrorRow to attach Pair /
// Direction fields client-side per SPEC line 674. If pairFilter is set and
// no pdir matches, returns pair_not_found.
func buildPDirLookup(canonical *config.Canonical, pairFilter, direction string) (map[pdirKey]pdirInfo, error) {
	lookup := map[pdirKey]pdirInfo{}
	matched := false
	for _, pd := range canonical.PDirs {
		if pairFilter != "" && pd.PairName != pairFilter {
			continue
		}
		if direction != "" && pd.Direction != direction {
			continue
		}
		matched = true
		lookup[pdirKey{Source: pd.SourceCalendar, Target: pd.TargetCalendar}] = pdirInfo{
			Pair:      pd.PairName,
			Direction: pd.Direction,
		}
	}
	if pairFilter != "" && !matched {
		return nil, newCmdError(output.CodePairNotFound,
			fmt.Sprintf("pair %q (direction=%q) not found in config", pairFilter, direction),
			"", nil)
	}
	return lookup, nil
}

// pdirKey indexes the lookup map by canonical source + target calendar.
type pdirKey struct {
	Source string
	Target string
}

// buildMirrorRow extracts SPEC's wire shape from one inventory event. The
// mirror's calendar-sync:source extended property is parsed for the source
// tuple; if it's malformed (no parseable value), the row is skipped.
//
// When pairFilter / direction filtering is active, we also drop any mirror
// whose source-calendar doesn't match a pdir in the lookup. Unfiltered
// listings retain every mirror, including those whose source-calendar
// belongs to a pdir that's been removed from config since the mirror was
// created (the user wants to see those so they can prune them).
func buildMirrorRow(m gws.Event, lookup map[pdirKey]pdirInfo, pairFilter, direction string) (mirrorRow, bool) {
	tuple, ok := mirrorSourceTuple(&m)
	if !ok {
		return mirrorRow{}, false
	}
	row := mirrorRow{
		ID:            m.ID,
		Summary:       m.Summary,
		Source:        tuple.String(),
		SourceUpdated: extPropValue(&m, mirror.ExtKeySourceUpdated),
		tuple:         tuple,
	}
	if m.Start != nil {
		row.Start = formatEventTime(m.Start)
	}
	if m.End != nil {
		row.End = formatEventTime(m.End)
	}

	// We don't know the target calendar a-priori from a mirror; the API
	// caller did, so tag every row with the pdir info matching tuple.Source
	// against the lookup. If multiple pdirs share the same source, prefer
	// the one whose direction matches the filter.
	for k, info := range lookup {
		if k.Source != tuple.CalendarID {
			continue
		}
		if direction != "" && info.Direction != direction {
			continue
		}
		row.Pair = info.Pair
		row.Direction = info.Direction
		break
	}

	if pairFilter != "" || direction != "" {
		// If a filter is active and the row didn't resolve to a pdir, drop
		// it. (An unfiltered listing keeps every mirror including
		// orphans-from-removed-pdirs.)
		if row.Pair == "" {
			return mirrorRow{}, false
		}
	}

	return row, true
}

// mirrorSourceTuple extracts the SourceTuple from a mirror's
// extended-property bag. Mirrors of mirrors are filtered out by SPEC's
// loop guard, so the value is always either present-and-parseable or the
// row is unmanageable and dropped.
func mirrorSourceTuple(m *gws.Event) (mirror.SourceTuple, bool) {
	if m.ExtendedProperties == nil || m.ExtendedProperties.Private == nil {
		return mirror.SourceTuple{}, false
	}
	raw, ok := m.ExtendedProperties.Private[mirror.ExtKeySource]
	if !ok || raw == "" {
		return mirror.SourceTuple{}, false
	}
	tuple, err := mirror.ParseSourceTuple(raw)
	if err != nil {
		return mirror.SourceTuple{}, false
	}
	return tuple, true
}

// extPropValue returns the value of an extended-property private key, or
// empty string if absent.
func extPropValue(m *gws.Event, key string) string {
	if m.ExtendedProperties == nil || m.ExtendedProperties.Private == nil {
		return ""
	}
	return m.ExtendedProperties.Private[key]
}

// formatEventTime returns the canonical text form of a Calendar API
// EventDateTime: dateTime preferred over date.
func formatEventTime(t *gws.EventDateTime) string {
	if t == nil {
		return ""
	}
	if t.DateTime != "" {
		return t.DateTime
	}
	return t.Date
}

// sourceAlive returns whether the source event tuple still resolves on
// Google's side. Used by --orphaned filtering. A 404 maps to alive=false;
// any other error bubbles up.
func sourceAlive(ctx context.Context, api syncpkg.API, tuple mirror.SourceTuple) (bool, error) {
	src, err := api.EventsGet(ctx, tuple.CalendarID, tuple.EventID)
	if err != nil {
		if errors.Is(err, gws.ErrAPINotFound) {
			return false, nil
		}
		return false, err
	}
	if src == nil || src.Status == gws.EventStatusCancelled {
		return false, nil
	}
	return true, nil
}

// inPruneHorizonWindow returns true iff the event's start falls in
// [now, end] (inclusive). Events without a parseable start are excluded -
// the user must drop --prune-horizon to clean those up.
func inPruneHorizonWindow(start *gws.EventDateTime, now, end time.Time) bool {
	if start == nil {
		return false
	}
	var t time.Time
	var err error
	switch {
	case start.DateTime != "":
		t, err = time.Parse(time.RFC3339, start.DateTime)
	case start.Date != "":
		t, err = time.Parse("2006-01-02", start.Date)
	default:
		return false
	}
	if err != nil {
		return false
	}
	return !t.Before(now) && !t.After(end)
}

// validatePruneSelector enforces SPEC's "exactly one of --pair, --orphaned,
// --all" rule. A direction filter is allowed only with --pair.
func validatePruneSelector(c *MirrorPruneCmd) error {
	count := 0
	if c.Pair != "" {
		count++
	}
	if c.Orphaned {
		count++
	}
	if c.All {
		count++
	}
	if count != 1 {
		return newCmdError(output.CodeSelectorRequired,
			"exactly one of --pair, --orphaned, --all is required", "", nil)
	}
	if c.Direction != "" && c.Pair == "" {
		return newCmdError(output.CodeSelectorRequired,
			"--direction requires --pair", "", nil)
	}
	return nil
}

// confirm prompts the user via stderr and reads "y" / "yes" from stdin.
// Used only when --yes is omitted and stdin is a TTY.
func confirm(stderr interface{ Write([]byte) (int, error) }, count int) bool {
	if stderr != nil {
		_, _ = stderr.Write([]byte(fmt.Sprintf("About to delete %d mirror(s). Proceed? [y/N] ", count)))
	}
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	resp := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return resp == "y" || resp == "yes"
}

// isTerminal reports whether the given file is a terminal. We do a
// best-effort os.File check: a non-character device (regular file, pipe,
// socket) is treated as non-TTY. Avoids pulling in golang.org/x/term for
// a single boolean.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// pruneAction returns the action string SPEC line 701 uses ("deleted" for
// real deletes, "would_delete" for dry-run). Custom rather than the
// generic mirror.ActionDelete because dry-run is unique to this command.
func pruneAction(dryRun bool) string {
	if dryRun {
		return "would_delete"
	}
	return "deleted"
}

// loadCanonical is the loadConfig + validate + canonicalize trio that
// every gws-using subcommand needs. Errors flow through MapError.
func loadCanonical(rt *Runtime) (*config.Canonical, error) {
	cfg, err := loadConfig(rt)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg.Canonicalize(rt.Ctx, rt.gwsClient())
}

// mirrorRow is SPEC line 670's wire shape for one mirror entry. tuple is
// unexported so it doesn't appear in the JSON; it's used by the orphan
// filter and prune paths.
type mirrorRow struct {
	ID            string `json:"id"`
	Summary       string `json:"summary,omitempty"`
	Start         string `json:"start,omitempty"`
	End           string `json:"end,omitempty"`
	Source        string `json:"source"`
	SourceUpdated string `json:"source_updated,omitempty"`
	Pair          string `json:"pair,omitempty"`
	Direction     string `json:"direction,omitempty"`

	tuple mirror.SourceTuple `json:"-"`
}

// mirrorListMeta is SPEC line 671's `_meta` body for `mirror list`:
// `{"count":N,"has_more":bool}`.
type mirrorListMeta struct {
	Count   int  `json:"count"`
	HasMore bool `json:"has_more"`
}

// prunedRow is SPEC line 701's wire shape for `mirror prune`. action is
// "deleted" or "would_delete".
type prunedRow struct {
	Action  string `json:"action"`
	ID      string `json:"id"`
	Summary string `json:"summary,omitempty"`
	Source  string `json:"source"`
}

