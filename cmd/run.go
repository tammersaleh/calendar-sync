package cmd

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/output"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// runDialTimeout is the deadline for the daemon-running detection probe.
// Quick enough to avoid a noticeable startup delay; long enough to handle
// a slow daemon's accept goroutine.
const runDialTimeout = 200 * time.Millisecond

// RunCmd implements `calendar-sync run`. SPEC §"calendar-sync run" lines
// 436-547. One-shot reconcile against the canonicalized config; refuses
// to start if the daemon is already running on the IPC socket.
type RunCmd struct {
	Pair      []string      `name:"pair" placeholder:"<name>" help:"Reconcile only the named pair. May be repeated. Default: all enabled pairs."`
	Direction string        `name:"direction" placeholder:"<dir>" help:"Limit to one direction within each pair. Only a_to_b is currently meaningful."`
	DryRun    bool          `name:"dry-run" help:"Plan and print actions but make no API writes. Reads still happen."`
	Timeout   time.Duration `name:"timeout" placeholder:"<dur>" default:"5m" help:"Wall-clock cap for the entire command. Default: 5m."`
}

// Run is the kong entry point. It wraps the shared run logic with timeout
// + daemon-running detection + the canonicalize step.
//
// SPEC: --timeout is the "wall-clock cap for the entire command", so the
// timeout context is derived BEFORE canonicalize (which calls gws
// calendarList and can hang). Both canonicalize and the run loop share
// the same deadline.
func (c *RunCmd) Run(rt *Runtime) error {
	if err := refuseIfDaemonRunning(); err != nil {
		return err
	}
	cfg, err := loadConfig(rt)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, cancel := c.timeoutContext(rt.Ctx)
	defer cancel()

	canonical, err := cfg.Canonicalize(ctx, rt.gwsClient())
	if err != nil {
		return err
	}
	count := 0
	return c.run(ctx, rt, canonical, &count)
}

// timeoutContext derives a child context from parent that bounds the
// entire command per SPEC's --timeout semantics. Zero / negative timeouts
// fall back to the 5m default.
func (c *RunCmd) timeoutContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return context.WithTimeout(parent, timeout)
}

// run is the shared core invoked by both RunCmd.Run (above) and PairTestCmd.Run.
// `count` is incremented (or zeroed) by the printed Outcomes so PairTestCmd
// can fold this command's count into its own meta. ctx must already carry
// the --timeout deadline; see RunCmd.Run / PairTestCmd.Run for the
// derivation.
func (c *RunCmd) run(ctx context.Context, rt *Runtime, canonical *config.Canonical, count *int) error {
	pdirs := filterPDirs(canonical.PDirs, c.Pair, c.Direction)
	if len(c.Pair) > 0 && len(pdirs) == 0 {
		return newCmdError(output.CodePairNotFound,
			fmt.Sprintf("no pdirs match pair=%v direction=%s", c.Pair, c.Direction),
			"Check `calendar-sync pair list` for available pairs.", nil)
	}

	// SPEC line 253: `[settings].dry_run = true` must suppress writes the
	// same way `--dry-run` does. Either source of truth flips the wrapper.
	api := rt.gwsClient()
	if c.DryRun || canonical.Settings.DryRun {
		api = newDryRunAPI(api)
	}

	// Build a per-command Reconciler scoped to the filtered pdir list. We
	// don't reuse a daemon-style long-lived Reconciler because `run` is
	// fire-and-forget; FullSync rebuilds the inventory from scratch.
	scopedCanonical := &config.Canonical{
		Settings:  canonical.Settings,
		Calendars: canonical.Calendars,
		PDirs:     pdirs,
	}

	p := rt.printer()
	opts := []syncpkg.Option{
		syncpkg.WithOutput(func(o syncpkg.Outcome) {
			p.Emit(outcomeRow{Outcome: o})
			*count++
		}),
	}
	if rt.Logger != nil {
		opts = append(opts, syncpkg.WithLogger(rt.Logger))
	}
	rec := syncpkg.New(api, scopedCanonical, opts...)

	res, err := rec.FullSync(ctx)
	if err != nil {
		return err
	}

	// Per SPEC §"Partial failure semantics" (lines 1287-1303): every pdir
	// runs to completion; the final exit is partial_failure if ANY pdir
	// failed, with `_meta.failures` listing them by `<pair>:<direction>`
	// (the same identifier shape SPEC uses for pdirs throughout).
	var failures []string
	var firstErr error
	for _, pr := range res.PDirs {
		if pr.Err == nil {
			continue
		}
		failures = append(failures, pr.Pair+":"+pr.Direction)
		if firstErr == nil {
			firstErr = pr.Err
		}
		// SPEC line 1295: a pdir failure must surface the underlying error
		// somewhere visible. The stderr partial_failure envelope only carries
		// a list of pdir names; the actual cause lives in stderr's warn-level
		// log so an operator running with --log-level=warn or above can
		// always see why a pdir failed without re-running with debug.
		// Logger.Warn is nil-safe at the receiver (see internal/output/logger.go).
		rt.Logger.Warn("cmd.run: pdir failed",
			"pair", pr.Pair,
			"direction", pr.Direction,
			"source", pr.Source,
			"target", pr.Target,
			"events_processed", pr.Counts.EventsProcessed,
			"error", pr.Err.Error(),
		)
	}

	p.Meta(runMetaPayload{
		PDirs:           len(res.PDirs),
		EventsProcessed: res.Aggregated.EventsProcessed,
		Inserts:         res.Aggregated.Inserts,
		Patches:         res.Aggregated.Patches,
		Propagates:      res.Aggregated.Propagates,
		Reverts:         res.Aggregated.Reverts,
		Deletes:         res.Aggregated.Deletes,
		Skips:           res.Aggregated.Skips,
		DurationMS:      res.DurationMS,
		Failures:        failures,
	})

	if len(failures) > 0 {
		return newCmdError(output.CodePartialFailure,
			fmt.Sprintf("%d pdir(s) failed: %s", len(failures), strings.Join(failures, ", ")),
			"", firstErr)
	}
	return nil
}

// filterPDirs returns the subset of pdirs that match --pair and
// --direction. Empty pairFilter returns every enabled pdir; non-empty
// applies an OR over the names. Empty direction returns both directions
// for each matched pair.
func filterPDirs(all []config.PDir, pairFilter []string, direction string) []config.PDir {
	if len(pairFilter) == 0 && direction == "" {
		return all
	}
	pairSet := map[string]bool{}
	for _, n := range pairFilter {
		pairSet[n] = true
	}
	out := []config.PDir{}
	for _, pd := range all {
		if len(pairSet) > 0 && !pairSet[pd.PairName] {
			continue
		}
		if direction != "" && pd.Direction != direction {
			continue
		}
		out = append(out, pd)
	}
	return out
}

// refuseIfDaemonRunning probes the IPC socket. If a daemon is reachable,
// returns daemon_already_running (exit 5). Stale or missing socket -
// proceed.
func refuseIfDaemonRunning() error {
	path := defaultSocketPath()
	conn, err := net.DialTimeout("unix", path, runDialTimeout)
	if err != nil {
		// Stale or missing - proceed.
		return nil
	}
	_ = conn.Close()
	return newCmdError(output.CodeDaemonAlreadyRunning,
		"calendar-sync watch is reachable on "+path,
		"Stop the running daemon first (`calendar-sync uninstall`).", nil)
}

// outcomeRow converts an internal sync.Outcome to the SPEC-stable JSON
// shape. Keeps the wire format (snake_case, omitempty rules) decoupled
// from the internal sync.Outcome struct.
type outcomeRow struct {
	syncpkg.Outcome
}

// MarshalJSON renders an Outcome as SPEC's per-action wire shape (lines
// 453-458). We re-implement here rather than reusing daemon's outcomeRow
// (unexported) so the command package owns its output contract.
func (r outcomeRow) MarshalJSON() ([]byte, error) {
	type payload struct {
		Action        string   `json:"action"`
		Pair          string   `json:"pair"`
		Direction     string   `json:"direction"`
		SourceEvent   string   `json:"source_event,omitempty"`
		TargetEvent   string   `json:"target_event,omitempty"`
		Summary       string   `json:"summary,omitempty"`
		Reason        string   `json:"reason,omitempty"`
		Fields        []string `json:"fields,omitempty"`
		Conflict      string   `json:"conflict,omitempty"`
		SourceUpdated string   `json:"source_updated,omitempty"`
		MirrorUpdated string   `json:"mirror_updated,omitempty"`
	}
	o := r.Outcome
	row := payload{
		Action:        string(o.Action),
		Pair:          o.Pair,
		Direction:     o.Direction,
		SourceEvent:   o.SourceEventID,
		TargetEvent:   o.TargetEventID,
		Summary:       o.Summary,
		Reason:        string(o.Reason),
		Fields:        o.Fields,
		Conflict:      string(o.Conflict),
		SourceUpdated: o.SourceUpdated,
		MirrorUpdated: o.MirrorUpdated,
	}
	return jsonMarshal(row)
}

// runMetaPayload is SPEC line 459's `_meta` body. Wrapped by the printer
// as `{"_meta":{...}}`. Failures is omitempty so successful runs match
// SPEC's example shape exactly; on any pdir failure SPEC line 526
// requires the field to list the failed `<pair>:<direction>` identifiers.
type runMetaPayload struct {
	PDirs           int      `json:"pdirs"`
	EventsProcessed int      `json:"events_processed"`
	Inserts         int      `json:"inserts"`
	Patches         int      `json:"patches"`
	Propagates      int      `json:"propagates"`
	Reverts         int      `json:"reverts"`
	Deletes         int      `json:"deletes"`
	Skips           int      `json:"skips"`
	DurationMS      int64    `json:"duration_ms"`
	Failures        []string `json:"failures,omitempty"`
}

// dryRunAPI wraps a real GwsClient so reads (List, Get, Instances) are
// unchanged but writes (Insert, Patch, Delete) become no-ops. The
// reconciler still emits Outcomes for would-be writes via its Output
// callback; the wire effect is suppressed.
//
// The wrapper holds a per-(calendarID, eventID) cache of "what was
// last echoed back" so repeated writes against the same event behave
// like Calendar API's JSON Merge Patch:
//
//   - Insert caches a clone of the body (with ID populated if missing).
//   - Patch merges the patch body into the cached snapshot - top-level
//     fields replace; ExtendedProperties.Private/Shared merges at the
//     key level - and returns the merged event. Without a prior Insert,
//     Patch falls back to body-echo with ID populated.
//   - Delete clears the cache entry and returns nil.
//
// The cache + merge are what make B2 cause A go away. Before, Patch
// echoed body verbatim, so a checksum-only follow-up Patch dropped the
// version + source the Insert had populated. The next Classify pass
// for the same source-tuple read the broken cached event and fired
// NeedsMigration - bogus migration_source_won outcomes.
//
// The wrapper IS thread-safe (Reconciler's orphan walk fans events.get
// out across goroutines, and fake test stubs that wrap dryRunAPI may
// also fan out); the inner mutex serializes cache reads and writes.
type dryRunAPI struct {
	inner GwsClient
	mu    sync.Mutex
	cache map[dryRunCacheKey]*gws.Event
}

type dryRunCacheKey struct {
	calendarID string
	eventID    string
}

func newDryRunAPI(inner GwsClient) GwsClient {
	return &dryRunAPI{
		inner: inner,
		cache: make(map[dryRunCacheKey]*gws.Event),
	}
}

func (d *dryRunAPI) CalendarListGet(ctx context.Context, id string) (*gws.CalendarListEntry, error) {
	return d.inner.CalendarListGet(ctx, id)
}

func (d *dryRunAPI) CalendarListList(ctx context.Context) ([]gws.CalendarListEntry, error) {
	return d.inner.CalendarListList(ctx)
}

func (d *dryRunAPI) EventsList(ctx context.Context, params gws.EventsListParams) ([]gws.Event, string, error) {
	return d.inner.EventsList(ctx, params)
}

func (d *dryRunAPI) EventsGet(ctx context.Context, calendarID, eventID string) (*gws.Event, error) {
	return d.inner.EventsGet(ctx, calendarID, eventID)
}

func (d *dryRunAPI) EventsInstances(ctx context.Context, params gws.EventsInstancesParams) ([]gws.Event, error) {
	return d.inner.EventsInstances(ctx, params)
}

func (d *dryRunAPI) EventsInsert(_ context.Context, calendarID string, body *gws.Event) (*gws.Event, error) {
	if body == nil {
		return &gws.Event{}, nil
	}
	out := dryRunCloneEvent(body)
	if out.ID == "" {
		out.ID = "dry-run"
	}
	d.mu.Lock()
	d.cache[dryRunCacheKey{calendarID: calendarID, eventID: out.ID}] = out
	d.mu.Unlock()
	// Return an independent copy so callers mutating the response
	// don't scribble over the cache.
	return dryRunCloneEvent(out), nil
}

func (d *dryRunAPI) EventsPatch(_ context.Context, calendarID string, eventID string, body *gws.Event) (*gws.Event, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := dryRunCacheKey{calendarID: calendarID, eventID: eventID}
	cached := d.cache[key]

	if body == nil {
		// Body-less patch: return what's cached if any, else a minimal
		// stub matching the prior body-echo contract.
		if cached != nil {
			return dryRunCloneEvent(cached), nil
		}
		return &gws.Event{ID: eventID}, nil
	}

	if cached == nil {
		// No prior insert seen by the wrapper. Body-echo with ID
		// populated. Tests that don't drive doInsert (e.g. patch-only
		// stubs) rely on this contract.
		out := dryRunCloneEvent(body)
		if out.ID == "" {
			out.ID = eventID
		}
		d.cache[key] = out
		return dryRunCloneEvent(out), nil
	}

	merged := dryRunMergeEvent(cached, body)
	d.cache[key] = merged
	return dryRunCloneEvent(merged), nil
}

func (d *dryRunAPI) EventsDelete(_ context.Context, calendarID, eventID string) error {
	d.mu.Lock()
	delete(d.cache, dryRunCacheKey{calendarID: calendarID, eventID: eventID})
	d.mu.Unlock()
	return nil
}

// dryRunCloneEvent returns a fresh *gws.Event with deep copies of the
// pointer + slice + map fields the dry-run cache could otherwise leak
// callers' mutations into (or vice versa).
func dryRunCloneEvent(e *gws.Event) *gws.Event {
	if e == nil {
		return nil
	}
	out := *e
	if e.Start != nil {
		s := *e.Start
		out.Start = &s
	}
	if e.End != nil {
		s := *e.End
		out.End = &s
	}
	if e.OriginalStartTime != nil {
		s := *e.OriginalStartTime
		out.OriginalStartTime = &s
	}
	if e.Reminders != nil {
		r := *e.Reminders
		out.Reminders = &r
	}
	if e.ExtendedProperties != nil {
		ep := *e.ExtendedProperties
		if e.ExtendedProperties.Private != nil {
			priv := make(map[string]string, len(e.ExtendedProperties.Private))
			for k, v := range e.ExtendedProperties.Private {
				priv[k] = v
			}
			ep.Private = priv
		}
		if e.ExtendedProperties.Shared != nil {
			sh := make(map[string]string, len(e.ExtendedProperties.Shared))
			for k, v := range e.ExtendedProperties.Shared {
				sh[k] = v
			}
			ep.Shared = sh
		}
		out.ExtendedProperties = &ep
	}
	if e.Recurrence != nil {
		r := make([]string, len(e.Recurrence))
		copy(r, e.Recurrence)
		out.Recurrence = r
	}
	if e.Attendees != nil {
		a := make([]gws.Attendee, len(e.Attendees))
		copy(a, e.Attendees)
		out.Attendees = a
	}
	return &out
}

// dryRunMergeEvent applies Calendar API JSON Merge Patch semantics:
// non-zero top-level fields in patch REPLACE base; ExtendedProperties
// (both Private and Shared) merge at the key level so a checksum-only
// follow-up patch doesn't drop the version + source the Insert wrote.
//
// Pointer + slice fields in patch (Start, Recurrence, Attendees, ...)
// replace as a whole when non-nil. Wholesale-replacement matches
// Calendar API's wire behavior for these fields - a patch with a new
// `start` replaces the prior start, it doesn't merge sub-fields.
//
// Empty-string scalars in patch are NOT applied; calendar-sync's
// patches never explicitly clear a field, and the wire test suite
// already exercises the "drop empty in body" pattern via omitempty.
func dryRunMergeEvent(base, patch *gws.Event) *gws.Event {
	out := dryRunCloneEvent(base)
	if patch.ID != "" {
		out.ID = patch.ID
	}
	if patch.Status != "" {
		out.Status = patch.Status
	}
	if patch.Summary != "" {
		out.Summary = patch.Summary
	}
	if patch.Description != "" {
		out.Description = patch.Description
	}
	if patch.Updated != "" {
		out.Updated = patch.Updated
	}
	if patch.Transparency != "" {
		out.Transparency = patch.Transparency
	}
	if patch.Visibility != "" {
		out.Visibility = patch.Visibility
	}
	if patch.HTMLLink != "" {
		out.HTMLLink = patch.HTMLLink
	}
	if patch.RecurringEventID != "" {
		out.RecurringEventID = patch.RecurringEventID
	}
	if patch.EventType != "" {
		out.EventType = patch.EventType
	}
	if patch.Start != nil {
		s := *patch.Start
		out.Start = &s
	}
	if patch.End != nil {
		s := *patch.End
		out.End = &s
	}
	if patch.OriginalStartTime != nil {
		s := *patch.OriginalStartTime
		out.OriginalStartTime = &s
	}
	if patch.Reminders != nil {
		r := *patch.Reminders
		out.Reminders = &r
	}
	if patch.Recurrence != nil {
		r := make([]string, len(patch.Recurrence))
		copy(r, patch.Recurrence)
		out.Recurrence = r
	}
	if patch.Attendees != nil {
		a := make([]gws.Attendee, len(patch.Attendees))
		copy(a, patch.Attendees)
		out.Attendees = a
	}
	if patch.ExtendedProperties != nil {
		if out.ExtendedProperties == nil {
			out.ExtendedProperties = &gws.ExtendedProperties{}
		}
		if patch.ExtendedProperties.Private != nil {
			if out.ExtendedProperties.Private == nil {
				out.ExtendedProperties.Private = make(map[string]string, len(patch.ExtendedProperties.Private))
			}
			for k, v := range patch.ExtendedProperties.Private {
				out.ExtendedProperties.Private[k] = v
			}
		}
		if patch.ExtendedProperties.Shared != nil {
			if out.ExtendedProperties.Shared == nil {
				out.ExtendedProperties.Shared = make(map[string]string, len(patch.ExtendedProperties.Shared))
			}
			for k, v := range patch.ExtendedProperties.Shared {
				out.ExtendedProperties.Shared[k] = v
			}
		}
	}
	return out
}
