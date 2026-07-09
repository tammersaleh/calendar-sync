package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/feedimport"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// feedRunner is the feed-import phase the daemon runs before each Tick's mirror
// reconcile (NOT before FullSync - see runFeeds and B26). *feedimport.Runner
// satisfies it. A nil Feeds field means no feeds are configured and the phase
// is skipped entirely.
type feedRunner interface {
	RunOnce(ctx context.Context) []feedimport.FeedResult
}

// Daemon is the long-running process that drives sync.Reconciler. Construct
// one per process; call Run to start the main loop.
//
// Required fields:
//
//   - Reconciler: the sync.Reconciler the daemon drives. The daemon installs
//     its own Output sink wrapping the existing one (so outcome JSONL
//     writes flow through both the daemon's printer and any test sink).
//   - Canonical: the resolved config the daemon consults for pdir list,
//     intervals, and inventory targeting.
//
// Optional fields:
//
//   - SocketPath: defaults to $TMPDIR/calendar-sync.sock per SPEC §"IPC
//     socket". An explicit value is convenient for tests.
//   - AuthChecker: nil skips the auth probe (tests). Production wires
//     `gws auth status`.
//   - Clock: nil uses realClock (time.Now / time.After). Tests inject a
//     fakeClock.
//   - Stdout: nil disables outcome JSONL output (used by --quiet). The
//     Reconciler's Counts wrapper still tracks numbers regardless.
type Daemon struct {
	Reconciler  *syncpkg.Reconciler
	Canonical   *config.Canonical
	SocketPath  string
	AuthChecker AuthChecker
	Clock       Clock
	Stdout      io.Writer

	// Feeds is the optional feed-import phase. When non-nil it runs BEFORE
	// each Tick's mirror reconcile (not FullSync - see runFeeds/B26) so a feed
	// change reaches its target calendar and then propagates through the mirror
	// mesh within the same tick. nil skips the phase.
	Feeds feedRunner
}

// Run executes SPEC §"Daemon lifecycle: startup" (lines 887-913) plus the
// main scheduler loop. Blocks until ctx is canceled, SIGTERM/SIGINT
// arrives, or a fatal error occurs.
//
// Order of operations:
//
//  1. AuthChecker probe; non-nil error returns ErrAuthFailed-wrapped error.
//  2. Bind the IPC socket (with stale-socket cleanup); failure returns
//     ErrDaemonAlreadyRunning or a wrapped I/O error.
//  3. Run Reconciler.FullSync once.
//  4. Enter the main loop: schedule next-tick / next-full-sync per SPEC's
//     wall-clock formula; on each fire, run the corresponding op,
//     update state, fold any NeedsFullResync into a fast-track flag.
//  5. On ctx cancel / SIGTERM / SIGINT: stop the listener (which unlinks
//     the socket file) and return.
//
// Returns nil on a clean shutdown; non-nil on auth failure, socket bind
// failure, or any other unrecoverable error during startup.
func (d *Daemon) Run(ctx context.Context) error {
	if d.Reconciler == nil {
		return errors.New("daemon: Reconciler is nil")
	}
	if d.Canonical == nil {
		return errors.New("daemon: Canonical is nil")
	}

	clock := d.Clock
	if clock == nil {
		clock = realClock{}
	}

	socketPath := d.SocketPath
	if socketPath == "" {
		socketPath = defaultSocketPath()
	}

	// Wrap context with signal handling. SIGTERM and SIGINT trigger ctx
	// cancellation per SPEC §"IPC socket" daemon-side: "On clean shutdown
	// (SIGTERM or SIGINT received), the daemon unlink()s the socket
	// before exiting."
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Step 1: auth probe.
	if err := runAuthCheck(ctx, d.AuthChecker); err != nil {
		return err
	}

	// Step 2: bind socket. Returns ErrDaemonAlreadyRunning if another
	// daemon is alive; otherwise a fresh listener.
	listener, err := bindSocket(socketPath)
	if err != nil {
		return err
	}
	// Closing the listener removes the socket file on most Unixes
	// (net.UnixListener honors SetUnlinkOnClose=true by default in Go).
	// Belt-and-suspenders: explicitly unlink on shutdown too, in case a
	// future Go version changes the default.
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()

	startedAt := clock.Now()

	// Set up state store seeded from Canonical.PDirs so the IPC handler
	// can answer status queries even before the first FullSync completes.
	store := newStateStore(os.Getpid(), startedAt, d.Canonical.Settings, d.Canonical.PDirs)
	server := newStatusServer(listener, store)
	go server.serve()

	// Wire the outcome printer + per-pass meta emission into the
	// Reconciler. The wrapper preserves any existing Output sink (set by
	// tests) while the daemon adds its own JSONL printer.
	printer := newOutcomePrinter(d.Stdout)
	prevOutput := d.Reconciler.Output
	d.Reconciler.Output = func(o syncpkg.Outcome) {
		printer.emitOutcome(o)
		if prevOutput != nil {
			prevOutput(o)
		}
	}

	// Step 4 setup: the scheduler is constructed before the initial
	// FullSync so we can hand it to runFullSync (which folds NeedsFullResync
	// signals into its fast-track flag).
	settings := d.Canonical.Settings
	sch := newScheduler(startedAt, settings.PollInterval.Duration(), settings.FullSyncInterval.Duration())

	// Step 3: initial FullSync. Per SPEC's "best effort" startup, per-pdir
	// errors don't bubble up here - they're surfaced via PDirResult.Err
	// and the conditional-token-advancement gate handles their effect on
	// the next tick.
	if err := d.runFullSync(ctx, clock, sch, store, printer); err != nil {
		return err
	}

	// Step 4: scheduler loop.
	for {
		now := clock.Now()
		_, delay := sch.nextEvent(now)

		select {
		case <-ctx.Done():
			return nil
		case <-clock.After(delay):
		}

		// Re-evaluate which event to fire AFTER the sleep - the wall-clock
		// may have jumped (sleep/wake) such that a different event is now
		// due. SPEC §"Sleep and wake" lines 1096-1097 describe this:
		// after a long sleep, both timers may be past, in which case
		// FullSync (which subsumes Tick's work) wins.
		event, _ := sch.nextEvent(clock.Now())

		var err error
		switch event {
		case eventFullSync:
			err = d.runFullSync(ctx, clock, sch, store, printer)
		case eventTick:
			err = d.runTick(ctx, clock, sch, store, printer)
		}
		if err != nil {
			return err
		}
	}
}

// runFullSync executes one Reconciler.FullSync, records its result into
// state + printer, advances the scheduler, and folds any
// NeedsFullResync signal into the fast-track flag. Used by both the
// startup path (called once before the loop) and the periodic / fast-track
// FullSync branches inside the loop.
func (d *Daemon) runFullSync(
	ctx context.Context,
	clock Clock,
	sch *scheduler,
	store *stateStore,
	printer *outcomePrinter,
) error {
	// NB: feeds are deliberately NOT imported on the FullSync pass - see runTick
	// and runFeeds. FullSync resets each source's syncToken from a full list,
	// and that reset can leap past a feed event written moments earlier (B26).
	res, err := d.Reconciler.FullSync(ctx)
	if err != nil {
		return fmt.Errorf("daemon: FullSync: %w", err)
	}
	doneAt := clock.Now()
	store.recordFullSync(doneAt, res, d.inventorySizes())
	printer.emitMeta(len(res.PDirs), res.Aggregated, time.Duration(res.DurationMS)*time.Millisecond)
	sch.recordFullSyncRan(doneAt)
	if anyNeedsFullResync(res.PerSource) {
		sch.requestFastTrackFullSync()
	}
	return nil
}

// runTick executes one Reconciler.Tick, records its result into state +
// printer, advances the scheduler, and folds any NeedsFullResync signal
// (typically from a 410 GONE) into the fast-track flag.
func (d *Daemon) runTick(
	ctx context.Context,
	clock Clock,
	sch *scheduler,
	store *stateStore,
	printer *outcomePrinter,
) error {
	d.runFeeds(ctx)
	res, err := d.Reconciler.Tick(ctx)
	if err != nil {
		return fmt.Errorf("daemon: Tick: %w", err)
	}
	doneAt := clock.Now()
	store.recordTick(doneAt, res, d.inventorySizes())
	printer.emitMeta(len(res.PDirs), res.Aggregated, time.Duration(res.DurationMS)*time.Millisecond)
	sch.recordTickRan(doneAt)
	if anyNeedsFullResync(res.PerSource) || anyTargetNeedsFullResync(res.PerTarget) {
		sch.requestFastTrackFullSync()
	}
	return nil
}

// runFeeds runs the feed-import phase when configured. Only runTick calls it,
// and it runs BEFORE the tick's mirror reconcile: a feed change reaches its
// target calendar and then propagates through the mirror mesh within the same
// tick. It is intentionally NOT called from runFullSync. FullSync resets each
// source's syncToken from a full events.list, and that list can omit an event
// the importer wrote seconds earlier while the returned token already sits past
// it, stranding the event until the next FullSync/restart (B26). Tick uses an
// incremental delta whose token is consistent with its own results, so a
// briefly-lagged feed write is simply caught by the next delta - never
// stranded. Feeds still poll every tick (subject to the fetcher's cache gate);
// at startup the import lands on the first tick rather than the startup
// FullSync (~one poll_interval later, race-free). The Runner isolates per-feed
// failures internally and logs each one, so a feed error can never abort the
// mirror sync; the daemon deliberately ignores the returned results.
func (d *Daemon) runFeeds(ctx context.Context) {
	if d.Feeds == nil {
		return
	}
	d.Feeds.RunOnce(ctx)
}

// inventorySizes returns a map[targetCalendarID]int with the current
// inventory size for each unique target in Canonical.PDirs. Unknown
// targets (no inventory yet) report 0 - the IPC handler surfaces "0
// mirrors" until the first FullSync rebuilds them. SPEC §"calendar-sync
// status" line 726 pins the `mirrors` field; the Reconciler's
// InventorySize accessor provides the live count.
func (d *Daemon) inventorySizes() map[string]int {
	sizes := map[string]int{}
	for _, pd := range d.Canonical.PDirs {
		if _, ok := sizes[pd.TargetCalendar]; ok {
			continue
		}
		sizes[pd.TargetCalendar] = d.Reconciler.InventorySize(pd.TargetCalendar)
	}
	return sizes
}

// anyNeedsFullResync reports whether any source in the per-source status
// map has NeedsFullResync=true. Used to decide whether to set the
// scheduler's fast-track flag after a Tick or FullSync.
func anyNeedsFullResync(perSource map[string]syncpkg.SourceStatus) bool {
	for _, st := range perSource {
		if st.NeedsFullResync {
			return true
		}
	}
	return false
}

// anyTargetNeedsFullResync mirrors anyNeedsFullResync for the per-target
// status map. A 410 GONE on a target-syncToken stream sets
// PerTarget[t].NeedsFullResync=true; the daemon must re-seed that token
// via a fast-track FullSync rather than coasting until the next periodic
// re-sync.
func anyTargetNeedsFullResync(perTarget map[string]syncpkg.TargetStatus) bool {
	for _, st := range perTarget {
		if st.NeedsFullResync {
			return true
		}
	}
	return false
}

// defaultSocketPath returns SPEC's default `$TMPDIR/calendar-sync.sock`.
// macOS sets TMPDIR per-user, so the socket is naturally scoped to one
// user's daemon. On other platforms, fall back to whatever os.TempDir
// returns (typically /tmp).
func defaultSocketPath() string {
	dir := os.Getenv("TMPDIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "calendar-sync.sock")
}
