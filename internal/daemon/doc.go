// Package daemon implements `calendar-sync watch`: the long-running process
// that owns sync state in memory and drives sync.Reconciler on a wall-clock
// schedule. It binds an IPC socket at $TMPDIR/calendar-sync.sock so
// `calendar-sync status` can query live per-pdir state.
//
// The package layout per SPEC.md:
//
//   - daemon.go - the Daemon struct and Run method (lifecycle orchestration
//     per SPEC §"Daemon lifecycle: startup" lines 887-913).
//   - scheduler.go - the wall-clock-driven scheduler per SPEC §"Sleep and
//     wake" lines 1090-1101. Computes next-tick / next-full-sync and folds
//     the fast-track-FullSync flag set by Tick's NeedsFullResync signal.
//   - socket.go - the Unix-domain socket bind + accept loop + per-connection
//     status response per SPEC §"IPC socket" daemon-side lifecycle.
//   - state.go - the thread-safe snapshot the IPC handler returns. Updated
//     by the main loop after each FullSync/Tick.
//   - auth.go - the AuthChecker startup probe (production wires `gws auth
//     status`; tests pass a closure).
//   - dryrun.go - the JSONL outcome printer that wraps sync.Outcome lines
//     plus a _meta trailer per pass.
//   - clock.go - the Clock abstraction (production wraps time.Now; tests
//     inject a fake clock so the scheduler runs deterministically).
//
// The daemon is single-goroutine for the main loop. The accept loop runs in
// a separate goroutine but only reads the state snapshot (under a mutex);
// per SPEC §"Concurrency" line 1108 a tick won't fire while the previous
// tick is still running, and the main loop is the sole writer of state.
package daemon
