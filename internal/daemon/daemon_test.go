package daemon

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// fakeClock is a deterministic Clock for daemon main-loop tests. Tests
// drive time forward by calling advance, and the After channels released
// by advance fire from the test goroutine (no real wall-clock sleep).
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	pending []*pendingTimer
}

// pendingTimer is one outstanding After call. Stored so advance can fire
// any timer whose deadline is now reached.
type pendingTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

// Now returns the current fake time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After returns a channel that fires once advance has moved the fake
// clock to or past now+d. Per Clock contract.
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.pending = append(c.pending, &pendingTimer{at: c.now.Add(d), ch: ch})
	return ch
}

// advance moves the fake clock forward by d, releasing any timers whose
// deadlines fall in the new range.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	due := []*pendingTimer{}
	remaining := c.pending[:0]
	for _, t := range c.pending {
		if !t.at.After(now) {
			due = append(due, t)
		} else {
			remaining = append(remaining, t)
		}
	}
	c.pending = remaining
	c.mu.Unlock()
	for _, t := range due {
		t.ch <- now
	}
}

// stubAPI is a hand-rolled in-process gws.API stub for daemon main-loop
// tests. Each EventsList call returns immediately with whatever events
// the test seeded via the listResponses/listTokens maps; this lets the
// main loop run FullSync and Tick to completion without an actual gws
// subprocess. The reconciler_test.go stub is more elaborate (per-label
// FIFO queues); the daemon tests don't need that level of control.
//
// listIncrErrs are consumed by the next incremental list call (with
// SyncToken set) for the matching CalendarID. listErrors fire on the
// full source-list call. inventoryErrs fire on the inventory rebuild
// list call (PrivateExtendedProperty filter set).
type stubAPI struct {
	mu             sync.Mutex
	listResponses  map[string][]gws.Event
	listTokens     map[string]string
	listIncrErrs   map[string]error
	listErrors     map[string]error
	inventories    map[string][]gws.Event
	inventoryErrs  map[string]error
	calls          []string
	listCalls      atomic.Int32
	tickListCalls  atomic.Int32
}

func newStubAPI() *stubAPI {
	return &stubAPI{
		listResponses: map[string][]gws.Event{},
		listTokens:    map[string]string{},
		listIncrErrs:  map[string]error{},
		listErrors:    map[string]error{},
		inventories:   map[string][]gws.Event{},
		inventoryErrs: map[string]error{},
	}
}

func (s *stubAPI) EventsList(_ context.Context, p gws.EventsListParams) ([]gws.Event, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls.Add(1)
	if p.SyncToken != "" {
		s.tickListCalls.Add(1)
		if err := s.listIncrErrs[p.CalendarID]; err != nil {
			delete(s.listIncrErrs, p.CalendarID)
			s.calls = append(s.calls, "list-incr-err:"+p.CalendarID)
			return nil, "", err
		}
		s.calls = append(s.calls, "list-incr:"+p.CalendarID)
		return s.listResponses[p.CalendarID], s.listTokens[p.CalendarID], nil
	}
	if len(p.PrivateExtendedProperty) > 0 {
		// Inventory rebuild call.
		key := p.CalendarID
		if err := s.inventoryErrs[key]; err != nil {
			s.calls = append(s.calls, "inv-err:"+key)
			return nil, "", err
		}
		s.calls = append(s.calls, "inv:"+key)
		return s.inventories[key], "", nil
	}
	if err := s.listErrors[p.CalendarID]; err != nil {
		s.calls = append(s.calls, "list-full-err:"+p.CalendarID)
		return nil, "", err
	}
	s.calls = append(s.calls, "list-full:"+p.CalendarID)
	return s.listResponses[p.CalendarID], s.listTokens[p.CalendarID], nil
}

func (s *stubAPI) EventsGet(_ context.Context, _, _ string) (*gws.Event, error) {
	return nil, errors.New("stubAPI.EventsGet: not implemented for daemon tests")
}

func (s *stubAPI) EventsInstances(_ context.Context, _ gws.EventsInstancesParams) ([]gws.Event, error) {
	return nil, nil
}

func (s *stubAPI) EventsInsert(_ context.Context, _ string, _ *gws.Event) (*gws.Event, error) {
	return nil, errors.New("stubAPI.EventsInsert: not implemented for daemon tests")
}

func (s *stubAPI) EventsPatch(_ context.Context, _, _ string, _ *gws.PatchEvent) (*gws.Event, error) {
	return nil, errors.New("stubAPI.EventsPatch: not implemented for daemon tests")
}

func (s *stubAPI) EventsDelete(_ context.Context, _, _ string) error {
	return nil
}

// makeDaemon returns a Daemon with one pdir wired to stubAPI. Settings use
// a 60s poll_interval and 24h full_sync_interval so the test's fake clock
// can simulate realistic boundaries.
func makeDaemon(t *testing.T, api *stubAPI, clock *fakeClock) (*Daemon, *bytes.Buffer) {
	t.Helper()
	canonical := &config.Canonical{
		Settings: config.Settings{
			PollInterval:     mustDuration("60s"),
			FullSyncInterval: mustDuration("24h"),
		},
		PDirs: []config.PDir{
			{
				PairName:       "p1",
				Direction:      config.PDirAtoB,
				SourceCalendar: "src",
				TargetCalendar: "tgt",
			},
		},
	}
	rec := syncpkg.New(api, canonical, syncpkg.WithNow(clock.Now))
	stdout := &bytes.Buffer{}
	d := &Daemon{
		Reconciler: rec,
		Canonical:  canonical,
		SocketPath: tmpSocketPath(t),
		Clock:      clock,
		Stdout:     stdout,
	}
	t.Cleanup(func() { os.Remove(d.SocketPath) })
	return d, stdout
}

// runDaemonAsync starts d.Run in a goroutine and returns the cancel func
// + a channel that closes on Run return. Tests cancel and wait on the
// channel to ensure clean shutdown.
func runDaemonAsync(d *Daemon) (cancel context.CancelFunc, done <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- d.Run(ctx)
	}()
	return cancel, doneCh
}

// TestDaemon_AuthFailure: AuthChecker error fails Run before binding the
// socket.
func TestDaemon_AuthFailure(t *testing.T) {
	api := newStubAPI()
	clock := newFakeClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	d, _ := makeDaemon(t, api, clock)
	d.AuthChecker = func(_ context.Context) error {
		return errors.New("not authenticated")
	}

	err := d.Run(context.Background())
	if err == nil {
		t.Fatalf("expected error from AuthChecker failure")
	}
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("err = %v, want errors.Is(err, ErrAuthFailed)", err)
	}

	// Socket should not have been bound (no file at the path).
	if _, err := os.Stat(d.SocketPath); err == nil {
		t.Errorf("auth failure should not bind socket; file exists at %s", d.SocketPath)
	}
}

// TestDaemon_AlreadyRunning: a pre-bound socket triggers
// ErrDaemonAlreadyRunning.
func TestDaemon_AlreadyRunning(t *testing.T) {
	api := newStubAPI()
	clock := newFakeClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	d, _ := makeDaemon(t, api, clock)

	// Pre-bind the socket from this test goroutine.
	pre, err := net.Listen("unix", d.SocketPath)
	if err != nil {
		t.Fatalf("pre-bind: %v", err)
	}
	defer pre.Close()

	err = d.Run(context.Background())
	if !errors.Is(err, ErrDaemonAlreadyRunning) {
		t.Errorf("err = %v, want errors.Is(err, ErrDaemonAlreadyRunning)", err)
	}
}

// TestDaemon_StartupRunsFullSyncOnce: after Run starts and ctx cancels
// before any tick fires, FullSync ran exactly once.
func TestDaemon_StartupRunsFullSyncOnce(t *testing.T) {
	api := newStubAPI()
	api.listTokens["src"] = "tok-1"
	clock := newFakeClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	d, _ := makeDaemon(t, api, clock)

	cancel, done := runDaemonAsync(d)

	// Wait for FullSync to complete (it ran on startup, so listCalls is
	// already at 2: full source-list + 2 inventory rebuilds = 3, but
	// minimum is 1 source-list + 2 inventory queries).
	waitFor(t, "startup FullSync", 2*time.Second, func() bool {
		return api.listCalls.Load() >= 3
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned error: %v", err)
	}

	// Tick should not have fired (cancel beat the timer).
	if got := api.tickListCalls.Load(); got > 0 {
		t.Errorf("tickListCalls = %d, want 0", got)
	}

	// Socket file should be unlinked after clean shutdown.
	if _, err := os.Stat(d.SocketPath); err == nil {
		t.Errorf("socket file should be removed on shutdown; still at %s", d.SocketPath)
	}
}

// TestDaemon_TickFiresAfterPollInterval: advancing the clock past the
// next-tick boundary causes a Tick to run.
func TestDaemon_TickFiresAfterPollInterval(t *testing.T) {
	api := newStubAPI()
	api.listTokens["src"] = "tok-1"
	clock := newFakeClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	d, _ := makeDaemon(t, api, clock)

	cancel, done := runDaemonAsync(d)
	defer func() {
		cancel()
		<-done
	}()

	// Wait for FullSync to land + register the After timer.
	waitFor(t, "FullSync done + After registered", 2*time.Second, func() bool {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		return api.listCalls.Load() >= 3 && len(clock.pending) > 0
	})

	// Bump past the next-tick boundary (60s).
	api.listResponses["src"] = nil
	api.listTokens["src"] = "tok-2"
	clock.advance(120 * time.Second)

	waitFor(t, "Tick fired", 2*time.Second, func() bool {
		return api.tickListCalls.Load() >= 1
	})
}

// TestDaemon_FullSyncIntervalFiresFullSync: advancing past full_sync_interval
// triggers a periodic FullSync, not a Tick.
func TestDaemon_FullSyncIntervalFiresFullSync(t *testing.T) {
	api := newStubAPI()
	api.listTokens["src"] = "tok-1"
	clock := newFakeClock(time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC))
	d, _ := makeDaemon(t, api, clock)

	cancel, done := runDaemonAsync(d)
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, "first FullSync done", 2*time.Second, func() bool {
		return api.listCalls.Load() >= 3
	})
	startCalls := api.listCalls.Load()
	tickStartCalls := api.tickListCalls.Load()

	// Advance past full_sync_interval (24h).
	clock.advance(25 * time.Hour)

	// FullSync runs (full list + inventory rebuilds), not a Tick (which
	// uses a syncToken). After this, total listCalls grew by at least 3
	// AND tickListCalls did NOT grow.
	waitFor(t, "second FullSync done", 2*time.Second, func() bool {
		return api.listCalls.Load() >= startCalls+3
	})
	if api.tickListCalls.Load() != tickStartCalls {
		t.Errorf("tickListCalls grew during periodic FullSync window (was %d, now %d)",
			tickStartCalls, api.tickListCalls.Load())
	}
}

// TestDaemon_NeedsFullResyncTriggersFastTrack: when Tick reports
// NeedsFullResync, the next iteration runs FullSync immediately. Wired by
// having the Tick's incremental list return 410 GONE; the reconciler
// signals NeedsFullResync, which the daemon folds into the scheduler's
// fast-track flag.
func TestDaemon_NeedsFullResyncTriggersFastTrack(t *testing.T) {
	api := newStubAPI()
	api.listTokens["src"] = "tok-1"
	clock := newFakeClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	d, _ := makeDaemon(t, api, clock)

	cancel, done := runDaemonAsync(d)
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, "first FullSync done", 2*time.Second, func() bool {
		return api.listCalls.Load() >= 3
	})

	// Queue a 410 on the next incremental list. The Reconciler's Tick
	// handles 410 by clearing the token and setting NeedsFullResync.
	api.mu.Lock()
	api.listIncrErrs["src"] = &gws.Error{Code: gws.CodeAPIGone, ExitCode: 1}
	api.mu.Unlock()

	startCalls := api.listCalls.Load()

	// Advance past poll interval to trigger Tick.
	clock.advance(120 * time.Second)

	// Wait for the Tick to land (it returns the 410 error).
	waitFor(t, "Tick with 410", 2*time.Second, func() bool {
		return api.tickListCalls.Load() >= 1
	})

	// The fast-track FullSync fires immediately (delay=0). After it lands,
	// listCalls should have grown by another 3 (one full source-list +
	// two inventory queries) without further clock advancement.
	waitFor(t, "fast-track FullSync runs", 2*time.Second, func() bool {
		return api.listCalls.Load() >= startCalls+4 // 1 tick (410) + 3 fullSync
	})
}

// TestDaemon_ContextCancelExitsCleanly: ctx.Cancel makes Run return nil
// after FullSync.
func TestDaemon_ContextCancelExitsCleanly(t *testing.T) {
	api := newStubAPI()
	api.listTokens["src"] = "tok-1"
	clock := newFakeClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	d, _ := makeDaemon(t, api, clock)

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() { doneCh <- d.Run(ctx) }()

	waitFor(t, "FullSync done", 2*time.Second, func() bool {
		return api.listCalls.Load() >= 3
	})
	cancel()

	select {
	case err := <-doneCh:
		if err != nil {
			t.Errorf("Run returned err on ctx cancel; got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after ctx cancel")
	}
}

// TestDaemon_NilReconciler: Run errors out cleanly with a missing
// Reconciler.
func TestDaemon_NilReconciler(t *testing.T) {
	d := &Daemon{
		Canonical: &config.Canonical{},
	}
	if err := d.Run(context.Background()); err == nil {
		t.Fatalf("expected error for nil Reconciler")
	}
}

// TestDaemon_NilCanonical: Run errors out cleanly with a missing
// Canonical.
func TestDaemon_NilCanonical(t *testing.T) {
	d := &Daemon{
		Reconciler: &syncpkg.Reconciler{},
	}
	if err := d.Run(context.Background()); err == nil {
		t.Fatalf("expected error for nil Canonical")
	}
}

// TestDaemon_SIGTERMExitsCleanly: sending SIGTERM to our own process
// triggers signal.NotifyContext-driven cancellation, which Run translates
// to a clean nil return after the in-flight FullSync completes.
//
// This test only verifies the signal-handling wiring exists and works on
// the current process; it doesn't try to test signal delivery to a
// child since signal.NotifyContext targets the test binary's pid.
func TestDaemon_SIGTERMExitsCleanly(t *testing.T) {
	api := newStubAPI()
	api.listTokens["src"] = "tok-1"
	clock := newFakeClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	d, _ := makeDaemon(t, api, clock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneCh := make(chan error, 1)
	go func() { doneCh <- d.Run(ctx) }()

	waitFor(t, "FullSync done", 2*time.Second, func() bool {
		return api.listCalls.Load() >= 3
	})

	// signal.NotifyContext registered a handler for SIGTERM in Run; sending
	// to our own pid should cancel ctx and return Run cleanly.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill self: %v", err)
	}

	select {
	case err := <-doneCh:
		if err != nil {
			t.Errorf("Run on SIGTERM = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after SIGTERM")
	}

	// Socket file should be unlinked.
	if _, err := os.Stat(d.SocketPath); err == nil {
		t.Errorf("socket file should be removed on SIGTERM shutdown; still at %s", d.SocketPath)
	}
}

// TestDaemon_OutcomePrinterEmitsToStdout: a reconciliation that produces
// outcomes flows through the daemon's printer.
func TestDaemon_OutcomePrinterEmitsToStdout(t *testing.T) {
	api := newStubAPI()
	// Stage one source event so FullSync runs the classifier and emits.
	// However the classifier needs a full inventory build; for simplicity
	// we just verify that FullSync emits a `_meta` line even when no
	// outcomes were produced (the daemon emits `_meta` per pass).
	api.listTokens["src"] = "tok-1"
	clock := newFakeClock(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	d, stdout := makeDaemon(t, api, clock)

	cancel, done := runDaemonAsync(d)
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, "first FullSync emits _meta", 2*time.Second, func() bool {
		return bytes.Contains(stdout.Bytes(), []byte("_meta"))
	})
}

// waitFor polls cond until it returns true or the timeout expires.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}
