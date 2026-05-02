package sync

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
	"github.com/tammersaleh/calendar-sync/internal/recurring"
)

// MaxResultsPerPage is the events.list page size SPEC.md mandates for both
// the startup full source-list (line 904) and the per-tick incremental delta
// (line 928). Exposed as a constant so tests can assert on it.
const MaxResultsPerPage = 250

// sourceListEventTypes is the SPEC's filter (line 851 / line 903 / line 928).
// Order matters: SPEC's wire shape lists default first, then outOfOffice,
// then focusTime - tests pin the exact slice contents.
var sourceListEventTypes = []string{
	gws.EventTypeDefault,
	gws.EventTypeOutOfOffice,
	gws.EventTypeFocusTime,
}

// Logger is the slog-style interface the sync layer consumes for structured
// per-step diagnostics. Re-declared here (rather than imported from
// internal/output) to keep the dependency direction one-way: output → sync,
// not sync → output. Production code passes *output.Logger which satisfies
// this interface naturally.
//
// A nil Logger is valid: every log call short-circuits before formatting.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Reconciler is the public-facing orchestrator for layer 6. It owns the
// in-memory per-source syncTokens and per-target inventories that survive
// across ticks per SPEC.md §"In-memory state". The daemon (layer 7)
// constructs one Reconciler at process start and drives it via FullSync
// (at startup + periodic full re-sync) and Tick (every poll_interval).
//
// All exported state mutation flows through FullSync / Tick. The Reconciler
// itself is not safe for concurrent calls - SPEC.md §"Concurrency" line 1108
// guarantees the daemon's scheduler holds the next tick until the current
// one returns, so a mutex would only mask programmer error.
type Reconciler struct {
	API       API
	Now       func() time.Time
	Canonical *config.Canonical
	Output    Output
	Log       Logger

	// OrphanConcurrency caps the orphan walk's events.get fan-out. SPEC's
	// "semaphore of 5" (line 1107) is the default when this is <= 0.
	OrphanConcurrency int

	// PropagateTargetEdits gates the SPEC §"Drift detection model" propagate
	// path. When false, drift on a writable-source pdir routes to revert
	// instead of propagate; the source is never modified. When true,
	// SPEC's two-way behavior is in effect. Defaults to false so a fresh
	// install runs one-way until the operator opts in via
	// `[settings].propagate_target_edits = true`.
	PropagateTargetEdits bool

	// In-memory state. SPEC §"In-memory state" lists these as the only
	// pieces that survive across ticks. A cold start (daemon restart, system
	// reboot) starts with all three empty and re-derives them via FullSync.
	syncTokens   map[string]string     // canonical source -> latest token
	inventories  map[string]*Inventory // canonical target -> live inventory
	lastFullSync map[string]time.Time  // canonical source -> last full-sync ts
}

// debug is a nil-safe wrapper around r.Log.Debug. Centralizing the nil
// check keeps the per-method log call sites uniform.
func (r *Reconciler) debug(msg string, args ...any) {
	if r.Log != nil {
		r.Log.Debug(msg, args...)
	}
}

// warn is a nil-safe wrapper around r.Log.Warn. Used by the per-pdir
// classify loop to surface per-event errors without dropping them on the
// floor (the SPEC's partial_failure path only carries pdir names; the
// underlying error needs its own log line so an operator running with
// --log-level=warn still gets the context).
func (r *Reconciler) warn(msg string, args ...any) {
	if r.Log != nil {
		r.Log.Warn(msg, args...)
	}
}

// Option mutates a Reconciler at construction time. Reserved for daemon-
// wiring knobs that don't belong in the type's required fields. Tests
// today only ever set fields directly; the option pattern is here for
// layer-7 ergonomics ("WithOrphanConcurrency", etc.).
type Option func(*Reconciler)

// WithNow injects a fixed clock. Used by tests. Production callers omit it
// and time.Now is used.
func WithNow(now func() time.Time) Option {
	return func(r *Reconciler) { r.Now = now }
}

// WithOrphanConcurrency overrides SPEC's default-5 events.get cap.
func WithOrphanConcurrency(n int) Option {
	return func(r *Reconciler) { r.OrphanConcurrency = n }
}

// WithOutput sets the Outcome sink.
func WithOutput(out Output) Option {
	return func(r *Reconciler) { r.Output = out }
}

// WithPropagateTargetEdits flips the safety gate. See
// Reconciler.PropagateTargetEdits for semantics.
func WithPropagateTargetEdits(enabled bool) Option {
	return func(r *Reconciler) { r.PropagateTargetEdits = enabled }
}

// WithLogger wires a structured logger that propagates into every Classifier
// and recurring.Handler the reconciler builds. Nil silences logging.
func WithLogger(l Logger) Option {
	return func(r *Reconciler) { r.Log = l }
}

// New constructs a Reconciler with empty in-memory state. Required fields
// are passed positionally; everything else is set via Option or by direct
// field access (the Reconciler is exported; both styles are supported).
func New(api API, canonical *config.Canonical, opts ...Option) *Reconciler {
	r := &Reconciler{
		API:          api,
		Canonical:    canonical,
		syncTokens:   map[string]string{},
		inventories:  map[string]*Inventory{},
		lastFullSync: map[string]time.Time{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Counts is the per-pdir or aggregated tally of outcomes a reconciliation
// produced. Tests assert against these; the daemon may surface them via
// `_meta` trailers in the JSONL output.
type Counts struct {
	EventsProcessed int
	Inserts         int
	Patches         int
	Deletes         int
	Propagates      int
	Reverts         int
	Skips           int
}

// add accumulates other into c. Used to fold per-pdir counts into the
// FullSyncResult.Aggregated total.
func (c *Counts) add(other Counts) {
	c.EventsProcessed += other.EventsProcessed
	c.Inserts += other.Inserts
	c.Patches += other.Patches
	c.Deletes += other.Deletes
	c.Propagates += other.Propagates
	c.Reverts += other.Reverts
	c.Skips += other.Skips
}

// observe increments the appropriate counter based on the action and
// always bumps EventsProcessed. The wrapped Output passes the outcome
// through to the daemon's sink unchanged.
func (c *Counts) observe(o Outcome) {
	c.EventsProcessed++
	switch o.Action {
	case mirror.ActionInsert:
		c.Inserts++
	case mirror.ActionPatch:
		c.Patches++
	case mirror.ActionDelete:
		c.Deletes++
	case mirror.ActionPropagate:
		c.Propagates++
	case mirror.ActionRevert:
		c.Reverts++
	case mirror.ActionSkip:
		c.Skips++
	}
}

// PDirResult is one pdir's per-pass tally and failure signal. Err is nil on
// success. Per SPEC §"Partial failure semantics" (line 1287), a non-nil Err
// here means "this pdir couldn't process every event, so its source's
// syncToken must NOT advance" - the conditional-advancement gate uses Err
// to decide.
type PDirResult struct {
	Pair      string
	Direction string
	Source    string // canonical
	Target    string // canonical
	Counts    Counts
	Err       error
}

// SourceStatus is the per-source bookkeeping the daemon needs after a pass.
//
//   - SyncTokenChanged is true iff the in-memory token for this source was
//     advanced (every dependent pdir succeeded and Google returned a token).
//   - NeedsFullResync is true iff a 410 GONE was received (Tick) or the
//     token was empty when Tick ran. The daemon should schedule a FullSync
//     for this source.
type SourceStatus struct {
	SyncTokenChanged bool
	NeedsFullResync  bool
}

// FullSyncResult is what FullSync returns. Per-pdir + per-source signals,
// total wall-clock duration, and an aggregated Counts across all pdirs.
type FullSyncResult struct {
	PDirs      []PDirResult
	PerSource  map[string]SourceStatus
	DurationMS int64
	Aggregated Counts
}

// TickResult is what Tick returns. Same shape as FullSyncResult; the orphan
// walk doesn't run on per-tick (SPEC §"per-tick reconciliation" lines 917-
// 936 don't include it).
type TickResult struct {
	PDirs      []PDirResult
	PerSource  map[string]SourceStatus
	DurationMS int64
	Aggregated Counts
}

// InventorySize returns the current number of mirrors in the in-memory
// inventory for the given target calendar. Used by the layer-7 daemon's
// IPC status surface (per SPEC §"calendar-sync status" line 726, the
// `mirrors` field). Returns 0 when no inventory exists yet for the target
// (e.g. before the first FullSync, or for a target whose rebuild
// errored).
func (r *Reconciler) InventorySize(target string) int {
	inv, ok := r.inventories[target]
	if !ok {
		return 0
	}
	return len(inv.Tuples())
}

// sourceMaxHorizon returns the max effective horizon across all pdirs that
// use the given source. Source-list TimeMax must reach the longest-horizon
// pdir's window so its classifier sees every event in scope; the
// shorter-horizon pdir's classifier filters per its own horizon at apply
// time.
//
// Returns 0 if no pdir matches the source. uniqueSources() is derived from
// r.Canonical.PDirs, so a "no match" return is a programmer-bug shape (the
// caller passed in a source that's not in the canonical) - the safe
// default of 0 means "no TimeMax", which simply widens the window.
func (r *Reconciler) sourceMaxHorizon(source string) time.Duration {
	var max time.Duration
	for _, pd := range r.Canonical.PDirs {
		if pd.SourceCalendar != source {
			continue
		}
		if pd.Horizon > max {
			max = pd.Horizon
		}
	}
	return max
}

// uniqueSources returns the canonical source IDs from r.Canonical.PDirs in
// alphabetical order. Sorted so tests can pin the wire order without
// depending on map iteration.
func (r *Reconciler) uniqueSources() []string {
	seen := map[string]struct{}{}
	for _, pd := range r.Canonical.PDirs {
		seen[pd.SourceCalendar] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// uniqueTargets returns the canonical target IDs in alphabetical order.
func (r *Reconciler) uniqueTargets() []string {
	seen := map[string]struct{}{}
	for _, pd := range r.Canonical.PDirs {
		seen[pd.TargetCalendar] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// now returns the configured clock or time.Now. Mirrors Classifier.now /
// OrphanWalker.now for consistency.
func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// FullSync runs SPEC.md §"Daemon lifecycle: startup" (lines 887-913) plus
// the §"periodic full re-sync" orphan walk (line 950 step 5).
//
// Algorithm:
//
//  1. Full source-list per unique source. Capture events + nextSyncToken into
//     staging maps; do NOT install tokens yet.
//  2. Mirror inventory rebuild per unique target via BuildInventory.
//  3. For each pdir, construct a Classifier + recurring.Handler and walk the
//     staged source list, emitting outcomes via a wrapped Output that tracks
//     per-pdir Counts.
//  4. After classify, run an OrphanWalker per (source, target) pair using the
//     visited set populated in step 3.
//  5. Conditionally commit each source's staged token: only if every pdir
//     whose source matches it succeeded (SPEC §"Conditional advancement"
//     line 910 + line 934).
//
// Per-pdir failures don't abort the pass; SPEC §"Partial failure semantics"
// (line 1287) requires every pdir to get its chance.
func (r *Reconciler) FullSync(ctx context.Context) (FullSyncResult, error) {
	start := r.now()

	res := FullSyncResult{
		PerSource: map[string]SourceStatus{},
	}

	// 1. Full source-list per unique source.
	stagedEvents, stagedTokens, sourceErrs := r.fullListSources(ctx)

	// 2. Inventory rebuild per unique target. Errors are tracked per target
	//    so we can mark dependent pdirs as failed and skip token advancement
	//    where appropriate.
	inventoryErrs := r.rebuildInventories(ctx)

	// 3 + 4. Reconcile per pdir, then orphan walk per (source, target).
	for _, pd := range r.Canonical.PDirs {
		pr := r.runPDirFullSync(ctx, pd, stagedEvents, sourceErrs, inventoryErrs)
		res.PDirs = append(res.PDirs, pr)
		res.Aggregated.add(pr.Counts)
	}

	// 5. Conditional token advancement.
	r.advanceTokens(stagedTokens, res.PDirs, res.PerSource)

	// Stamp lastFullSync for every source whose full source-list call
	// succeeded - regardless of whether a syncToken came back (Google can
	// omit nextSyncToken on very long full lists; SPEC line 912). The
	// timestamp tracks "when did we last complete a full source-list scan
	// for this source", not "when did the token advance". Gating on token
	// advancement would leave lastFullSync permanently stale for a source
	// whose full list keeps coming back without a token, causing the
	// daemon's scheduler to re-run FullSync immediately and indefinitely.
	//
	// fullListSources only adds to sourceErrs on failure, so a source that
	// is absent from the map succeeded.
	for _, source := range r.uniqueSources() {
		if sourceErrs[source] == nil {
			r.lastFullSync[source] = start
		}
	}

	// Record sources that hit a list error so the daemon's status surface can
	// flag NeedsFullResync (the next tick wouldn't have a token to use, and
	// even FullSync didn't get past the wire). This matches Tick's behavior:
	// any source whose token wasn't advanced is "needs full re-sync next."
	for s, err := range sourceErrs {
		if err == nil {
			continue
		}
		st := res.PerSource[s]
		st.NeedsFullResync = true
		res.PerSource[s] = st
	}

	res.DurationMS = r.now().Sub(start).Milliseconds()
	return res, nil
}

// Tick runs SPEC.md §"Daemon lifecycle: per-tick reconciliation" (lines
// 917-936). Incremental events.list per unique source via the in-memory
// syncToken; reconcile the delta to every dependent pdir; conditional
// token advancement. The orphan walk does NOT run on a per-tick path -
// SPEC's lines 917-936 don't include it; only the periodic full re-sync
// (FullSync) does.
//
// 410 GONE handling per SPEC line 932: clear the in-memory token for that
// source and surface NeedsFullResync=true so the daemon can schedule a
// FullSync. Other source-list errors leave the token unchanged and mark
// every pdir for that source as failed.
//
// First-tick / empty-token handling: a source with no in-memory token (no
// FullSync has populated it yet, or a previous tick's 410 cleared it) is
// silently skipped with NeedsFullResync=true. Per SPEC line 932 "Schedule
// an immediate full re-sync for S".
func (r *Reconciler) Tick(ctx context.Context) (TickResult, error) {
	start := r.now()

	res := TickResult{
		PerSource: map[string]SourceStatus{},
	}

	stagedEvents, stagedTokens, sourceErrs := r.incrementalListSources(ctx, res.PerSource)

	for _, pd := range r.Canonical.PDirs {
		pr := r.runPDirTick(ctx, pd, stagedEvents, sourceErrs, res.PerSource)
		res.PDirs = append(res.PDirs, pr)
		res.Aggregated.add(pr.Counts)
	}

	r.advanceTokens(stagedTokens, res.PDirs, res.PerSource)

	res.DurationMS = r.now().Sub(start).Milliseconds()
	return res, nil
}

// fullListSources runs SPEC §"Daemon lifecycle: startup" step 5: full
// events.list per unique source with TimeMin=now, TimeMax=now+horizon,
// SingleEvents=false, ShowDeleted=true, EventTypes=[default,outOfOffice,
// focusTime], MaxResults=250. Returns staged events + tokens + per-source
// errors.
func (r *Reconciler) fullListSources(ctx context.Context) (
	events map[string][]gws.Event,
	tokens map[string]string,
	errs map[string]error,
) {
	events = map[string][]gws.Event{}
	tokens = map[string]string{}
	errs = map[string]error{}

	now := r.now()
	timeMin := now.Format(time.RFC3339)

	for _, source := range r.uniqueSources() {
		// Per-source TimeMax: max horizon across pdirs sharing this source.
		// Multiple pdirs can share a source with different horizons (e.g.
		// 1d on one, 365d on another); the source-list needs events out to
		// the longer window so the longer-horizon pdir's classifier sees
		// them. The shorter-horizon pdir's classifier filters per its own
		// horizon.
		horizon := r.sourceMaxHorizon(source)
		timeMax := ""
		if horizon > 0 {
			timeMax = now.Add(horizon).Format(time.RFC3339)
		}

		params := gws.EventsListParams{
			CalendarID:   source,
			TimeMin:      timeMin,
			TimeMax:      timeMax,
			SingleEvents: false,
			ShowDeleted:  true,
			EventTypes:   sourceListEventTypes,
			MaxResults:   MaxResultsPerPage,
		}
		evs, token, err := r.API.EventsList(ctx, params)
		if err != nil {
			errs[source] = err
			continue
		}
		events[source] = evs
		tokens[source] = token
	}
	return events, tokens, errs
}

// incrementalListSources runs SPEC §"per-tick reconciliation" step 1: an
// events.list per unique source using the in-memory syncToken. 410 GONE
// clears the token and sets NeedsFullResync; missing-token sources are
// skipped (NeedsFullResync only); other errors propagate to errs so the
// dependent pdirs are marked failed.
func (r *Reconciler) incrementalListSources(
	ctx context.Context,
	perSource map[string]SourceStatus,
) (
	events map[string][]gws.Event,
	tokens map[string]string,
	errs map[string]error,
) {
	events = map[string][]gws.Event{}
	tokens = map[string]string{}
	errs = map[string]error{}

	for _, source := range r.uniqueSources() {
		token, ok := r.syncTokens[source]
		if !ok || token == "" {
			st := perSource[source]
			st.NeedsFullResync = true
			perSource[source] = st
			continue
		}

		params := gws.EventsListParams{
			CalendarID:  source,
			SyncToken:   token,
			ShowDeleted: true,
			EventTypes:  sourceListEventTypes,
			MaxResults:  MaxResultsPerPage,
		}
		evs, nextToken, err := r.API.EventsList(ctx, params)
		if err != nil {
			if errors.Is(err, gws.ErrAPIGone) {
				delete(r.syncTokens, source)
				st := perSource[source]
				st.NeedsFullResync = true
				perSource[source] = st
				continue
			}
			errs[source] = err
			continue
		}
		events[source] = evs
		tokens[source] = nextToken
	}
	return events, tokens, errs
}

// rebuildInventories runs BuildInventory once per unique target and
// installs the resulting inventories atomically into r.inventories.
// Per SPEC §"periodic full re-sync" line 970 ("After each full re-sync the
// in-memory inventories are replaced atomically"), we build into a fresh
// map, then assign to r.inventories at the end so a partial pass doesn't
// leave the daemon with a half-rebuilt view.
func (r *Reconciler) rebuildInventories(ctx context.Context) map[string]error {
	errs := map[string]error{}
	fresh := map[string]*Inventory{}

	for _, target := range r.uniqueTargets() {
		inv, err := BuildInventory(ctx, r.API, target, r.Log)
		if err != nil {
			errs[target] = err
			continue
		}
		fresh[target] = inv
	}
	// Atomic-ish swap: assign successful targets in one go. Targets whose
	// rebuild errored retain whatever value (or absence) was there before;
	// per SPEC, a target whose rebuild failed should leave dependent pdirs
	// failed, so the conditional-token-advancement gate downstream blocks
	// any progress until the next FullSync succeeds.
	for target, inv := range fresh {
		r.inventories[target] = inv
	}
	return errs
}

// runPDirFullSync runs one pdir's classify pass + orphan walk during a
// FullSync. The pdir is marked failed (PDirResult.Err non-nil) when:
//
//   - the source-list for this pdir's source errored;
//   - the inventory rebuild for this pdir's target errored;
//   - any Classify call returned an error;
//   - the orphan walk returned an error.
//
// Per SPEC §"Partial failure semantics", a single Classify error within a
// pdir does NOT abort the pdir's loop - other events still get their
// chance, only the pdir's overall failure flag is set so its source's
// token won't advance.
func (r *Reconciler) runPDirFullSync(
	ctx context.Context,
	pd config.PDir,
	stagedEvents map[string][]gws.Event,
	sourceErrs map[string]error,
	inventoryErrs map[string]error,
) PDirResult {
	pr := PDirResult{
		Pair:      pd.PairName,
		Direction: pd.Direction,
		Source:    pd.SourceCalendar,
		Target:    pd.TargetCalendar,
	}

	// Pre-check: source-list or inventory failure means this pdir can't run.
	if err := sourceErrs[pd.SourceCalendar]; err != nil {
		pr.Err = fmt.Errorf("source list %s: %w", pd.SourceCalendar, err)
		return pr
	}
	if err := inventoryErrs[pd.TargetCalendar]; err != nil {
		pr.Err = fmt.Errorf("inventory rebuild %s: %w", pd.TargetCalendar, err)
		return pr
	}

	inv, ok := r.inventories[pd.TargetCalendar]
	if !ok {
		// Target's inventory wasn't rebuilt and there's no prior version.
		// Treat as failure; the conditional-advancement gate blocks the
		// source from advancing.
		pr.Err = fmt.Errorf("missing inventory for target %s", pd.TargetCalendar)
		return pr
	}

	classifier, output := r.buildClassifier(pd, inv, &pr.Counts)

	visited := map[mirror.SourceTuple]bool{}
	classifyErrs := r.runClassifyLoop(ctx, classifier, stagedEvents[pd.SourceCalendar], visited)
	if classifyErrs != nil {
		pr.Err = classifyErrs
	}

	// Orphan walk runs on FullSync only. Per SPEC §"periodic full re-sync"
	// step 5 (line 950): "For each mirror inventory entry whose corresponding
	// source event ID was not visited in step 4..."
	if err := r.runOrphanWalk(ctx, pd, inv, visited, output); err != nil {
		// Combine with any classify errors so the pdir's failure context
		// is preserved end-to-end.
		if pr.Err != nil {
			pr.Err = errors.Join(pr.Err, err)
		} else {
			pr.Err = err
		}
	}
	return pr
}

// runPDirTick runs one pdir's classify pass during Tick (no orphan walk).
// Pre-checks mirror runPDirFullSync but the inventory must already exist
// from a prior FullSync; on Tick we don't rebuild inventories from
// scratch.
func (r *Reconciler) runPDirTick(
	ctx context.Context,
	pd config.PDir,
	stagedEvents map[string][]gws.Event,
	sourceErrs map[string]error,
	perSource map[string]SourceStatus,
) PDirResult {
	pr := PDirResult{
		Pair:      pd.PairName,
		Direction: pd.Direction,
		Source:    pd.SourceCalendar,
		Target:    pd.TargetCalendar,
	}

	// First-tick / empty-token: NeedsFullResync was already set by
	// incrementalListSources; the pdir is "skipped" in the sense that no
	// classify runs, but it's not a failure - the daemon will run FullSync
	// next.
	if perSource[pd.SourceCalendar].NeedsFullResync &&
		sourceErrs[pd.SourceCalendar] == nil {
		// No events staged, no error - this pdir runs zero classifies.
		// Don't flag it as failed; the source already has NeedsFullResync.
		return pr
	}

	if err := sourceErrs[pd.SourceCalendar]; err != nil {
		pr.Err = fmt.Errorf("source list %s: %w", pd.SourceCalendar, err)
		return pr
	}

	inv, ok := r.inventories[pd.TargetCalendar]
	if !ok {
		// No prior inventory for this target - shouldn't happen on Tick if
		// FullSync ran first, but defensive: mark failed so the token
		// doesn't advance.
		pr.Err = fmt.Errorf("missing inventory for target %s; FullSync required",
			pd.TargetCalendar)
		return pr
	}

	classifier, _ := r.buildClassifier(pd, inv, &pr.Counts)

	// Tick has no orphan walk so the visited set isn't needed.
	if err := r.runClassifyLoop(ctx, classifier, stagedEvents[pd.SourceCalendar], nil); err != nil {
		pr.Err = err
	}
	return pr
}

// buildClassifier wires a per-pdir Classifier and recurring.Handler over the
// shared inventory. The returned Output is the same one we set on the
// classifier (a count-tracking wrapper around r.Output); orphan walks need
// it too so callers can pass it through.
func (r *Reconciler) buildClassifier(
	pd config.PDir,
	inv *Inventory,
	counts *Counts,
) (*Classifier, Output) {
	wrapped := wrapOutput(r.Output, counts)

	// Effective writability gates SPEC's drift-handling propagate path.
	// pd.SourceWritable reflects the calendar's accessRole (the API
	// permission); r.PropagateTargetEdits is the operator's safety toggle.
	// Both must be true for drift to flow back to source. When the gate is
	// off, drift routes to revert even on a writable source - matching the
	// SPEC's read-only-source behavior so the operator can verify the
	// one-way path before opting into bidirectional sync.
	effectiveSourceWritable := pd.SourceWritable && r.PropagateTargetEdits

	c := &Classifier{
		API:              r.API,
		Now:              r.Now,
		Horizon:          pd.Horizon,
		Pair:             pd.PairName,
		Direction:        pd.Direction,
		SourceCalendarID: pd.SourceCalendar,
		TargetCalendarID: pd.TargetCalendar,
		SourceWritable:   effectiveSourceWritable,
		Inventory:        inv,
		Output:           wrapped,
		Log:              r.Log,
	}

	// Wire the recurring.Handler. The closures capture the Classifier so
	// the parent-reconcile path delegates back through the standard
	// classification logic; SPEC's recurring instance handler step 1
	// (line 1130) requires that fallback when the inventory lacks the
	// mirror parent.
	c.Recurring = &recurring.Handler{
		API:              r.API,
		SourceCalendarID: pd.SourceCalendar,
		TargetCalendarID: pd.TargetCalendar,
		SourceWritable:   effectiveSourceWritable,
		Log:              r.Log,
		LookupMirrorParent: func(s mirror.SourceTuple) (*gws.Event, bool) {
			return inv.Lookup(s)
		},
		ReconcileParent: func(ctx context.Context, sourceParent *gws.Event) (*gws.Event, error) {
			if err := c.Classify(ctx, sourceParent); err != nil {
				return nil, err
			}
			tuple := mirror.SourceTuple{
				CalendarID: pd.SourceCalendar,
				EventID:    sourceParent.ID,
			}
			mp, _ := inv.Lookup(tuple)
			return mp, nil
		},
	}

	return c, wrapped
}

// runClassifyLoop iterates the staged source list for this pdir, calling
// Classify on each event and tracking visited source-tuples for the orphan
// walk. Per-event errors are accumulated; the loop continues so other
// events still get processed (SPEC §"Partial failure semantics").
//
// `visited` may be nil; Tick passes nil because there's no orphan walk on
// per-tick reconciliation. FullSync passes a non-nil map for the orphan
// walk that follows.
//
// Source-tuple dedupe (B2 cause B): events.list can return the same
// source-tuple twice in a single response - the documented case is a
// `_R<timestamp>`-shaped recurring parent that appears both as a
// top-level event and as a `recurring_event_id` on its own child
// instances. A second Classify on the same tuple is at best a wasted
// no-op and at worst (with the dryRunAPI defect) routes to the
// migration matrix and emits a bogus migration_source_won outcome.
// We track seen tuples per call and silently skip subsequent
// occurrences. SPEC's outcomes table doesn't define a "duplicate"
// reason; emitting one would surface as wire-format noise.
func (r *Reconciler) runClassifyLoop(
	ctx context.Context,
	c *Classifier,
	events []gws.Event,
	visited map[mirror.SourceTuple]bool,
) error {
	r.debug("sync.runClassifyLoop start",
		"pair", c.Pair,
		"direction", c.Direction,
		"source_calendar", c.SourceCalendarID,
		"target_calendar", c.TargetCalendarID,
		"events", len(events),
	)
	seen := make(map[mirror.SourceTuple]bool, len(events))
	var errs []error
	for i := range events {
		ev := events[i]
		tuple := mirror.SourceTuple{
			CalendarID: c.SourceCalendarID,
			EventID:    ev.ID,
		}

		// Track visited BEFORE Classify - the orphan walk needs to know we
		// SAW this source-tuple even if Classify errored on it. (The
		// alternative - skip on error - would treat a transient API
		// failure as "this source vanished" and delete the mirror.)
		// Tracking happens regardless of dedupe state; recording the
		// tuple a second time is a no-op on the visited set.
		if visited != nil {
			visited[tuple] = true
		}

		if seen[tuple] {
			r.debug("sync.runClassifyLoop duplicate",
				"pair", c.Pair,
				"direction", c.Direction,
				"source_event", ev.ID,
				"recurring_event_id", ev.RecurringEventID,
			)
			continue
		}
		seen[tuple] = true

		r.debug("sync.runClassifyLoop event",
			"pair", c.Pair,
			"direction", c.Direction,
			"source_event", ev.ID,
			"recurring_event_id", ev.RecurringEventID,
			"summary", ev.Summary,
			"status", ev.Status,
			"transparency", ev.Transparency,
		)

		if err := c.Classify(ctx, &ev); err != nil {
			r.warn("sync.runClassifyLoop: classify error",
				"pair", c.Pair,
				"direction", c.Direction,
				"source_event", ev.ID,
				"recurring_event_id", ev.RecurringEventID,
				"summary", ev.Summary,
				"error", err.Error(),
			)
			errs = append(errs, fmt.Errorf("classify %s/%s: %w",
				c.SourceCalendarID, ev.ID, err))
		}
	}
	return errors.Join(errs...)
}

// runOrphanWalk runs the per-pdir orphan walk. Per SPEC §"periodic full
// re-sync" step 5, this prunes mirror entries whose source wasn't seen in
// the visited set. Errors propagate up to be folded into PDirResult.Err.
//
// Concurrency: each pdir's walk uses r.OrphanConcurrency (default 5).
// SPEC §"Concurrency" line 1107.
func (r *Reconciler) runOrphanWalk(
	ctx context.Context,
	pd config.PDir,
	inv *Inventory,
	visited map[mirror.SourceTuple]bool,
	out Output,
) error {
	walker := &OrphanWalker{
		API:              r.API,
		Now:              r.Now,
		Horizon:          pd.Horizon,
		Pair:             pd.PairName,
		Direction:        pd.Direction,
		SourceCalendarID: pd.SourceCalendar,
		TargetCalendarID: pd.TargetCalendar,
		Inventory:        inv,
		Output:           out,
		ConcurrencyLimit: r.OrphanConcurrency,
	}
	return walker.Walk(ctx, visited)
}

// advanceTokens applies SPEC's conditional-advancement rule (line 910 +
// line 934): commit a source's staged token only if every pdir whose
// source matches succeeded. Updates SourceStatus.SyncTokenChanged.
//
// lastFullSync is updated separately by FullSync (per SPEC line 871, the
// timestamp is specifically "last full-sync"; Tick is incremental and
// does not refresh it).
func (r *Reconciler) advanceTokens(
	stagedTokens map[string]string,
	pdirs []PDirResult,
	perSource map[string]SourceStatus,
) {
	failedSources := map[string]bool{}
	for _, pr := range pdirs {
		if pr.Err != nil {
			failedSources[pr.Source] = true
		}
	}
	for source, token := range stagedTokens {
		if failedSources[source] {
			// Don't advance; SPEC's "if any pdir failed, leave the
			// in-memory token unchanged" rule.
			continue
		}
		if token == "" {
			// SPEC line 912: "If the staged token is missing... leave the
			// in-memory token empty so the next cycle re-runs a full
			// source-list." We honor this by NOT writing an empty token;
			// the existing entry stays put (which on FullSync was already
			// untouched by step 1).
			continue
		}
		r.syncTokens[source] = token

		st := perSource[source]
		st.SyncTokenChanged = true
		perSource[source] = st
	}
}

// wrapOutput returns an Output that increments counts before delegating to
// base. base may be nil (the daemon's sink isn't wired in tests that don't
// care about output content) - in that case the wrapper still updates
// counts.
func wrapOutput(base Output, counts *Counts) Output {
	return func(o Outcome) {
		counts.observe(o)
		if base != nil {
			base(o)
		}
	}
}
