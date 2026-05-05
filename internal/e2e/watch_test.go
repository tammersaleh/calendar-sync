//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestE2E_Watch_TickPropagatesSourceEdit pins SPEC §"calendar-sync watch"
// (line 473) and §"per-tick reconciliation": the daemon does a startup
// full sync and then ticks every poll_interval. Insert source pre-startup
// → startup sync mirrors it; patch source while running → next tick
// reconciles the delta.
//
// This is a longer-running scenario than the run-mode tests. The harness
// writes poll_interval = "15s" (SPEC minimum) so a full insert-then-patch
// cycle costs ~30s of wall-clock plus startup overhead.
func TestE2E_Watch_TickPropagatesSourceEdit(t *testing.T) {
	h := Setup(t, SetupOptions{})
	// 240s outer budget covers: ~5s daemon startup + up to 60s startup
	// outcome wait + a few EventsGet/Patch round trips + up to 60s tick
	// outcome wait + 10s graceful SIGTERM. Per-call timeouts inside
	// waitForOutcome cap individual blocking points; this outer ctx is
	// only a safety net for the test's API operations and does NOT
	// govern the watch subprocess (see startWatch comment).
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// Insert source BEFORE starting the daemon so the startup FullSync
	// picks it up. SPEC line 482: "The daemon does a full startup sync,
	// then ticks every poll_interval" - the startup sync surfaces the
	// insert as the first outcome line on stdout.
	originalTitle := h.Title("watch-original")
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: originalTitle,
		Start:   futureDateTime(0),
		End:     futureDateTime(1 * time.Hour),
	})

	w := startWatch(t, h)

	// Startup FullSync: expect the insert outcome filtered to our source.
	insertOut := w.waitForOutcome(t, 60*time.Second, func(o Outcome) bool {
		return o.SourceEvent == source.ID && o.Action == string(mirror.ActionInsert)
	})
	mirrorID := insertOut.TargetEvent
	if mirrorID == "" {
		t.Fatal("watch insert outcome has empty target_event")
	}
	if insertOut.Reason != string(mirror.ReasonSourceUpdated) {
		t.Errorf("watch insert reason = %q, want %q", insertOut.Reason, mirror.ReasonSourceUpdated)
	}

	mir, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorID)
	if err != nil {
		t.Fatalf("get mirror %s after startup sync: %v", mirrorID, err)
	}
	if mir.Summary != originalTitle {
		t.Errorf("mirror.summary after startup = %q, want %q", mir.Summary, originalTitle)
	}

	// Patch the source. The next tick (≤ 15s away on the wall-clock-aligned
	// boundary) should reconcile this as patch/source_updated.
	editedTitle := h.Title("watch-edited")
	if _, err := h.GWS.EventsPatch(ctx, h.SourceCalID, source.ID, &gws.PatchEvent{
		Summary: gws.PatchStr(editedTitle),
	}); err != nil {
		t.Fatalf("patch source %s: %v", source.ID, err)
	}

	// 15s poll_interval + startup overhead + classifier work = ~30s budget.
	patchOut := w.waitForOutcome(t, 60*time.Second, func(o Outcome) bool {
		return o.SourceEvent == source.ID &&
			o.Action == string(mirror.ActionPatch) &&
			o.Reason == string(mirror.ReasonSourceUpdated)
	})
	if patchOut.TargetEvent != mirrorID {
		t.Errorf("watch patch outcome target_event = %q, want %q", patchOut.TargetEvent, mirrorID)
	}

	postMirror, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorID)
	if err != nil {
		t.Fatalf("get mirror %s after tick patch: %v", mirrorID, err)
	}
	if postMirror.Summary != editedTitle {
		t.Errorf("post-tick mirror.summary = %q, want %q", postMirror.Summary, editedTitle)
	}

	// Clean shutdown via SIGTERM. The daemon's signal handler unbinds the
	// IPC socket and returns; the cleanup waits up to 10s for that.
	w.stop(t)
}

// watchProc owns a running calendar-sync watch subprocess plus the
// goroutine that streams its stdout into a channel for outcome assertions.
type watchProc struct {
	cmd      *exec.Cmd
	outcomes <-chan Outcome
	stdout   io.Closer

	// stderrBuf accumulates the daemon's stderr for failure diagnostics.
	// Mutex-guarded because the scanner goroutine writes while the test
	// goroutine reads.
	stderrMu  sync.Mutex
	stderrBuf bytes.Buffer

	// shutdown ensures the SIGTERM-then-SIGKILL-then-Wait sequence runs
	// exactly once. Both stop() (the test's explicit shutdown) and the
	// t.Cleanup safety net dispatch through it; the Once collapses
	// duplicate calls so we don't double-Wait or signal a dead process.
	shutdownOnce sync.Once
	shutdownErr  error
}

// startWatch launches `calendar-sync watch --config <h.ConfigPath>` from
// h.Sandbox so any stray writes (e.g. a `download.html` from a gws
// subprocess) land inside the per-test sandbox. Registers a t.Cleanup
// that runs the unified shutdown so the child is always reaped, even
// on a test panic or early-return path.
//
// Note: the subprocess is NOT bound to ctx via exec.CommandContext.
// ctx governs the TEST's API calls (which have their own per-call
// timeouts); the daemon subprocess's lifetime is owned by watchProc.
// Binding to ctx would let an outer-test-context expiry kill the
// subprocess via SIGKILL, defeating the graceful-shutdown assertion
// in stop(). The cleanup safety net guarantees the child cannot
// outlive the test regardless.
func startWatch(t *testing.T, h *Harness) *watchProc {
	t.Helper()
	cmd := exec.Command(h.Binary, "watch", "--config", h.ConfigPath)
	cmd.Dir = h.Sandbox

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("watch stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("watch stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start watch: %v", err)
	}

	w := &watchProc{cmd: cmd, stdout: stdout}
	outcomes := make(chan Outcome, 32)
	w.outcomes = outcomes

	// stdout goroutine: parse JSONL into Outcome (skipping _meta lines).
	go func() {
		defer close(outcomes)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			// Distinguish meta from outcomes: meta lines wrap a single
			// `_meta` envelope key that the Outcome shape doesn't carry.
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(line, &probe); err != nil {
				continue
			}
			if _, ok := probe["_meta"]; ok {
				continue
			}
			var o Outcome
			if err := json.Unmarshal(line, &o); err != nil {
				continue
			}
			outcomes <- o
		}
	}()

	// stderr goroutine: drain into a buffer for diagnostics on failure.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				w.stderrMu.Lock()
				w.stderrBuf.Write(buf[:n])
				w.stderrMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Belt-and-suspenders cleanup. If the test panics or returns
	// without calling stop(), the unified shutdown still runs and
	// reaps the child. The Once collapses double-shutdown when stop()
	// also fires.
	t.Cleanup(func() { w.shutdown(false, nil) })

	return w
}

// waitForOutcome reads outcomes until one matches predicate or timeout
// elapses. Failure dumps captured stderr for diagnostics.
func (w *watchProc) waitForOutcome(t *testing.T, timeout time.Duration, predicate func(Outcome) bool) Outcome {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case o, ok := <-w.outcomes:
			if !ok {
				w.stderrMu.Lock()
				stderr := w.stderrBuf.String()
				w.stderrMu.Unlock()
				t.Fatalf("watch stdout closed before matching outcome arrived; stderr:\n%s", stderr)
			}
			if predicate(o) {
				return o
			}
		case <-deadline.C:
			w.stderrMu.Lock()
			stderr := w.stderrBuf.String()
			w.stderrMu.Unlock()
			t.Fatalf("timeout %s waiting for outcome; stderr:\n%s", timeout, stderr)
		}
	}
}

// stop sends SIGTERM and waits up to 10s for the daemon to exit cleanly.
// SPEC §"IPC socket" daemon-side: SIGTERM unbinds the socket and
// returns. A wedged daemon fails the test rather than leaking the
// process. Routes through shutdown so the cleanup safety net is a
// no-op once stop() succeeds.
func (w *watchProc) stop(t *testing.T) {
	t.Helper()
	w.shutdown(true, t)
}

// shutdown is the unified subprocess teardown: SIGTERM, wait up to 10s,
// SIGKILL if needed, always Wait() so the child is reaped. Idempotent
// via sync.Once.
//
// expectClean=true means the caller (stop) requires a zero-exit clean
// shutdown; failure fatals on t. expectClean=false (cleanup safety
// net) means the caller doesn't care about the exit status, just
// wants the child reaped.
func (w *watchProc) shutdown(expectClean bool, t *testing.T) {
	w.shutdownOnce.Do(func() {
		if w.cmd.Process == nil {
			return
		}
		// SIGTERM. signal.NotifyContext in the daemon translates this
		// into a context cancellation that returns nil from Run.
		_ = w.cmd.Process.Signal(syscall.SIGTERM)

		done := make(chan error, 1)
		go func() { done <- w.cmd.Wait() }()

		select {
		case w.shutdownErr = <-done:
		case <-time.After(10 * time.Second):
			_ = w.cmd.Process.Kill()
			w.shutdownErr = <-done
			if expectClean && t != nil {
				t.Helper()
				t.Fatal("watch did not exit within 10s of SIGTERM")
			}
			return
		}

		if expectClean && t != nil && w.shutdownErr != nil {
			t.Helper()
			var ee *exec.ExitError
			if errors.As(w.shutdownErr, &ee) {
				w.stderrMu.Lock()
				stderr := w.stderrBuf.String()
				w.stderrMu.Unlock()
				t.Fatalf("watch exited %d; stderr:\n%s", ee.ExitCode(), stderr)
			}
			t.Fatalf("watch wait: %v", w.shutdownErr)
		}
	})
}
