package cmd

import (
	"context"
	"fmt"
	"net"
	"strings"
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
	Direction string        `name:"direction" placeholder:"<dir>" help:"Limit to one direction within each pair. One of a_to_b, b_to_a."`
	DryRun    bool          `name:"dry-run" help:"Plan and print actions but make no API writes. Reads still happen."`
	Timeout   time.Duration `name:"timeout" placeholder:"<dur>" default:"5m" help:"Wall-clock cap for the entire command. Default: 5m."`
}

// Run is the kong entry point. It wraps the shared run logic with timeout
// + daemon-running detection + the canonicalize step.
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
	canonical, err := cfg.Canonicalize(rt.Ctx, rt.gwsClient())
	if err != nil {
		return err
	}
	count := 0
	return c.run(rt, canonical, &count)
}

// run is the shared core invoked by both RunCmd.Run (above) and PairTestCmd.Run.
// `count` is incremented (or zeroed) by the printed Outcomes so PairTestCmd
// can fold this command's count into its own meta.
func (c *RunCmd) run(rt *Runtime, canonical *config.Canonical, count *int) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(rt.Ctx, timeout)
	defer cancel()

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
		syncpkg.WithHorizon(canonical.Settings.Horizon.Duration()),
		syncpkg.WithPropagateTargetEdits(canonical.Settings.PropagateTargetEdits),
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
// The wrapper returns minimal-but-valid Event values for write paths so
// downstream code that consults the response (e.g. mirror.BuildPayload's
// follow-up checksum patch) doesn't trip on nil. In particular:
//
//   - Insert returns a copy of the body with ID populated if missing.
//   - Patch returns body unchanged.
//   - Delete returns nil (success).
type dryRunAPI struct{ inner GwsClient }

func newDryRunAPI(inner GwsClient) GwsClient { return &dryRunAPI{inner: inner} }

func (d *dryRunAPI) CalendarListGet(ctx context.Context, id string) (*gws.CalendarListEntry, error) {
	return d.inner.CalendarListGet(ctx, id)
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

func (d *dryRunAPI) EventsInsert(_ context.Context, _ string, body *gws.Event) (*gws.Event, error) {
	if body == nil {
		return &gws.Event{}, nil
	}
	out := *body
	if out.ID == "" {
		out.ID = "dry-run"
	}
	return &out, nil
}

func (d *dryRunAPI) EventsPatch(_ context.Context, _ string, eventID string, body *gws.Event) (*gws.Event, error) {
	if body == nil {
		return &gws.Event{ID: eventID}, nil
	}
	out := *body
	if out.ID == "" {
		out.ID = eventID
	}
	return &out, nil
}

func (d *dryRunAPI) EventsDelete(_ context.Context, _, _ string) error {
	return nil
}
