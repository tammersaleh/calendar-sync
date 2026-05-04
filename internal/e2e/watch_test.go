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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	w := startWatch(t, ctx, h)

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
	if _, err := h.GWS.EventsPatch(ctx, h.SourceCalID, source.ID, &gws.Event{
		Summary: editedTitle,
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
}

// startWatch launches `calendar-sync watch --config <h.ConfigPath>` from
// h.Sandbox so any stray writes (e.g. a `download.html` from a gws
// subprocess) land inside the per-test sandbox. Registers a t.Cleanup
// that SIGKILLs the process if the test forgot to call stop().
func startWatch(t *testing.T, ctx context.Context, h *Harness) *watchProc {
	t.Helper()
	cmd := exec.CommandContext(ctx, h.Binary, "watch", "--config", h.ConfigPath)
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

	// Belt-and-suspenders cleanup: if the test panics or returns without
	// calling stop, kill the process so it doesn't outlive the test.
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

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
// SPEC §"IPC socket" daemon-side: SIGTERM unbinds the socket and returns.
// A wedged daemon fails the test rather than leaking the process.
func (w *watchProc) stop(t *testing.T) {
	t.Helper()
	if err := w.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to watch: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case err := <-done:
		// Exit codes from a SIGTERM-killed Go process: 0 (signal handled
		// cleanly via signal.NotifyContext) is what the daemon does.
		// Anything else is a clean shutdown failure.
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				w.stderrMu.Lock()
				stderr := w.stderrBuf.String()
				w.stderrMu.Unlock()
				t.Fatalf("watch exited %d; stderr:\n%s", ee.ExitCode(), stderr)
			}
			t.Fatalf("watch wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = w.cmd.Process.Kill()
		<-done
		t.Fatal("watch did not exit within 10s of SIGTERM")
	}
}
