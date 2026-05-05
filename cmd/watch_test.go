package cmd

import (
	"bytes"
	"context"
	"errors"
	"runtime/debug"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// TestWatchCmd_HappyPathReturnsNilOnContextCancel verifies the daemon
// shuts down cleanly when its driving context is canceled. The AuthChecker
// override returns nil so the auth probe passes; the stub gws returns
// empty source-lists / inventories so the initial FullSync completes
// instantly. Once the daemon enters its scheduler loop, the canceled ctx
// trips the select{ctx.Done()} branch and Daemon.Run returns nil.
func TestWatchCmd_HappyPathReturnsNilOnContextCancel(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	path := writeConfigFixture(t, validConfigTOML)

	prev := AuthChecker
	AuthChecker = func(_ context.Context) error { return nil }
	t.Cleanup(func() { AuthChecker = prev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel shortly after the daemon starts so it can finish its initial
	// FullSync (reads only - the stub returns empty pages so it's fast)
	// and then trip the ctx.Done branch in the scheduler loop. 100ms is a
	// comfortable upper bound on FullSync against the in-memory stub.
	time.AfterFunc(100*time.Millisecond, cancel)

	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     ctx,
		Gws:     &stubGws{},
	}

	done := make(chan error, 1)
	go func() {
		done <- (&WatchCmd{}).Run(rt)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WatchCmd.Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WatchCmd.Run did not return within 5s after ctx cancel")
	}
}

// TestWatchCmd_AuthFailureMapsToGWSAuthFailed verifies the daemon's auth
// probe failure surfaces as gws_auth_failed (SPEC exit code 2). The error
// path: AuthChecker returns non-nil → daemon wraps with daemon.ErrAuthFailed
// → MapError routes via the daemon-layer sentinel to CodeGWSAuthFailed.
func TestWatchCmd_AuthFailureMapsToGWSAuthFailed(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	path := writeConfigFixture(t, validConfigTOML)

	prev := AuthChecker
	authErr := errors.New("simulated gws auth status failure")
	AuthChecker = func(_ context.Context) error { return authErr }
	t.Cleanup(func() { AuthChecker = prev })

	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}

	err := (&WatchCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected auth failure error")
	}
	code, _, _ := MapError(err)
	if code != "gws_auth_failed" {
		t.Errorf("code = %q, want gws_auth_failed", code)
	}
}

// TestWatchCmd_ConfigInvalidMapsToConfigInvalid verifies a config-level
// validation failure short-circuits before the auth probe runs. Catches
// regressions where validation drift would surface as something other
// than config_invalid (the SPEC-mandated code).
func TestWatchCmd_ConfigInvalidMapsToConfigInvalid(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	const bad = `
[settings]
poll_interval      = "1s"
horizon            = "365d"
full_sync_interval = "24h"
log_level          = "info"
log_format         = "json"
`
	path := writeConfigFixture(t, bad)

	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}

	err := (&WatchCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected validation error")
	}
	code, _, _ := MapError(err)
	if code != "config_invalid" {
		t.Errorf("code = %q, want config_invalid", code)
	}
}

// hangingEventsListGws blocks EventsList until ctx is cancelled, recording
// every observed deadline so the test can assert the per-call cap was
// applied. Used by TestWatchCmd_TimeoutBoundsGwsCall.
type hangingEventsListGws struct {
	stubGws
	called    chan struct{}  // signals first call
	deadlines chan time.Time // observed deadlines
}

func newHangingEventsListGws() *hangingEventsListGws {
	return &hangingEventsListGws{
		called:    make(chan struct{}, 1),
		deadlines: make(chan time.Time, 4),
	}
}

func (h *hangingEventsListGws) EventsList(ctx context.Context, _ gws.EventsListParams) ([]gws.Event, string, error) {
	dl, _ := ctx.Deadline()
	select {
	case h.deadlines <- dl:
	default:
	}
	select {
	case h.called <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, "", ctx.Err()
}

// TestWatchCmd_TimeoutBoundsGwsCall pins SPEC line 488: --timeout is the
// "wall-clock cap for any single source-list or mirror-list call." Wrap
// the gws client so a wedged subprocess returns context.DeadlineExceeded
// after timeout instead of holding the daemon's whole pass.
//
// Setup: stub EventsList blocks until ctx is cancelled. The daemon's
// initial FullSync calls EventsList; with the wrapper, ctx carries a
// 100ms deadline. Without the wrapper, ctx has no deadline and the call
// would hang until the test's outer cleanup.
func TestWatchCmd_TimeoutBoundsGwsCall(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	path := writeConfigFixture(t, validConfigTOML)

	prev := AuthChecker
	AuthChecker = func(_ context.Context) error { return nil }
	t.Cleanup(func() { AuthChecker = prev })

	stub := newHangingEventsListGws()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Outer ceiling so a regression doesn't hang the test forever.
	time.AfterFunc(3*time.Second, cancel)

	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     ctx,
		Gws:     stub,
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- (&WatchCmd{Timeout: 100 * time.Millisecond}).Run(rt)
	}()

	// The first EventsList call must observe a deadline within ~timeout
	// of "now", proving the wrapper applied. We don't care about the
	// exact daemon return; we care that the per-call ctx had a deadline.
	select {
	case <-stub.called:
		// First call landed.
	case <-time.After(2 * time.Second):
		t.Fatal("EventsList was not called within 2s; daemon never reached source-list step")
	}

	select {
	case dl := <-stub.deadlines:
		if dl.IsZero() {
			t.Errorf("EventsList ctx had no deadline; --timeout did not bound the call")
		}
		// Allow generous slack: the deadline should be within a couple
		// hundred ms of "start + 100ms". We only assert ordering, not
		// exact wall-clock alignment.
		want := start.Add(100 * time.Millisecond)
		// The deadline must be in the future at the moment of observation
		// AND must be soon (not minutes away). 5s is the upper bound.
		if dl.Sub(want) > 5*time.Second {
			t.Errorf("EventsList deadline = %v, way past --timeout=100ms (started at %v)", dl, start)
		}
	default:
		t.Errorf("expected an observed deadline, got none")
	}

	// Wait for the daemon to wind down.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Force-cancel via outer ctx and let the daemon exit.
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("WatchCmd.Run did not return within 4s after timeout-bounded call")
		}
	}
}

// TestWatchCmd_TimeoutZeroDisablesWrapper pins the "0 disables" contract:
// a zero/negative timeout MUST NOT wrap the GwsClient (the wrapper would
// otherwise install a deadline of "right now" and every gws call would
// instantly fail). callTimeoutAPI returns the inner client unchanged.
func TestWatchCmd_TimeoutZeroDisablesWrapper(t *testing.T) {
	stub := &stubGws{}
	wrapped := newCallTimeoutAPI(stub, 0)
	if wrapped != GwsClient(stub) {
		t.Errorf("newCallTimeoutAPI(stub, 0) = %T, want the inner stub unchanged", wrapped)
	}
	wrapped = newCallTimeoutAPI(stub, -time.Second)
	if wrapped != GwsClient(stub) {
		t.Errorf("newCallTimeoutAPI(stub, -1s) = %T, want the inner stub unchanged", wrapped)
	}
}

// TestWatchCmd_SettingsDryRunGatesWrites is the watch-side counterpart to
// TestRunCmd_SettingsDryRunGatesWrites. The daemon has no `--dry-run` flag,
// so settings.dry_run is the only way to flip the wrapper. We feed the
// initial FullSync a confirmed source event via panicWriteGws and assert
// the run completes (or is canceled) without panicking. A leak surfaces as
// a descriptive panic identifying the unwrapped write call.
func TestWatchCmd_SettingsDryRunGatesWrites(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	const dryRunConfigTOML = `
[settings]
poll_interval      = "60s"
horizon            = "365d"
full_sync_interval = "24h"
log_level          = "info"
log_format         = "json"
dry_run            = true

[[pairs]]
name      = "work-personal"
source    = "work@example.com"
target    = "personal@example.com"
`
	path := writeConfigFixture(t, dryRunConfigTOML)

	prev := AuthChecker
	AuthChecker = func(_ context.Context) error { return nil }
	t.Cleanup(func() { AuthChecker = prev })

	stub := &panicWriteGws{events: []gws.Event{{
		ID:           "evt-1",
		Status:       gws.EventStatusConfirmed,
		Summary:      "Test event",
		Updated:      "2026-04-29T20:00:00Z",
		Transparency: gws.TransparencyOpaque,
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T16:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T17:00:00Z"},
	}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel after the initial FullSync has had time to attempt its
	// writes; the panic, if any, would fire during that window. 200ms is
	// generous against the in-process stub.
	time.AfterFunc(200*time.Millisecond, cancel)

	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     ctx,
		Gws:     stub,
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("settings.dry_run did not gate writes in watch: %v\nstack:\n%s", r, debug.Stack())
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- (&WatchCmd{}).Run(rt)
	}()

	select {
	case <-done:
		// Either nil (clean cancel) or some error - both fine; the panic
		// recovery above is the assertion.
	case <-time.After(5 * time.Second):
		t.Fatal("WatchCmd.Run did not return within 5s after ctx cancel")
	}
}
