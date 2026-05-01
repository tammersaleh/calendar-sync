package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

// ErrDaemonAlreadyRunning is returned by Daemon.Run when the IPC socket
// path is already bound by another process. SPEC §"IPC socket" daemon-side
// lifecycle line 1334 calls this a "paranoia guard"; launchd's KeepAlive
// shouldn't normally produce two daemons, but if it does we surface a
// distinct error so the caller can log it without re-trying the
// stale-cleanup path.
var ErrDaemonAlreadyRunning = errors.New("daemon: socket is bound by another process")

// bindSocket implements SPEC §"IPC socket" daemon-side lifecycle (lines
// 1330-1336):
//
//  1. stat() the path. If it doesn't exist, proceed to bind.
//  2. If it exists, attempt to connect.
//  3. Connect succeeds → another daemon is running → ErrDaemonAlreadyRunning.
//  4. ECONNREFUSED OR non-socket file type → unlink, then bind.
//  5. bind + listen.
//
// On any other I/O error during stat or connect, return wrapped error.
// The brief connect dial uses a small timeout so a misbehaving stale
// socket can't hang startup indefinitely.
func bindSocket(path string) (net.Listener, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Fresh path; bind directly.
		return net.Listen("unix", path)
	case err != nil:
		return nil, fmt.Errorf("stat socket %q: %w", path, err)
	}

	// Path exists. Probe whether anything is listening.
	conn, err := net.DialTimeout("unix", path, socketProbeTimeout)
	if err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %s", ErrDaemonAlreadyRunning, path)
	}

	isSocketFile := info != nil && (info.Mode()&os.ModeSocket) != 0
	switch {
	case isStaleSocketErr(err):
		// ECONNREFUSED or vanished file: stale, safe to unlink.
	case !isSocketFile:
		// Whatever it is (regular file, named pipe, dir), it's not a
		// socket and dial fails for unrelated reasons; treat as stale.
	default:
		// Real socket file, dial failed for a non-stale reason: bail
		// rather than risk unlinking a still-running daemon whose
		// accept goroutine is just slow.
		return nil, fmt.Errorf("dial socket %q: %w", path, err)
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("unlink stale socket %q: %w", path, err)
	}
	return net.Listen("unix", path)
}

// socketProbeTimeout is the dial timeout for the "is anyone listening?"
// check during bind. Short enough not to noticeably delay startup; long
// enough to tolerate a slow daemon.
const socketProbeTimeout = 200 * time.Millisecond

// isStaleSocketErr reports whether the given dial error indicates "no one
// is listening at the path" (vs. some other transport failure). Either
// ECONNREFUSED on a real socket, or fs.ErrNotExist if the file vanished
// between stat and dial, qualifies.
func isStaleSocketErr(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return false
}

// statusServer runs the accept loop for the IPC socket. Each accepted
// connection gets a one-shot handler that writes the status snapshot
// JSONL and closes. Per SPEC §"calendar-sync status" stdout shape (lines
// 722-728), the response is a global header line followed by per-pdir
// lines plus a `_meta` trailer.
type statusServer struct {
	listener net.Listener
	store    *stateStore
}

// newStatusServer constructs a server bound to ln, reading state via
// store on each connection.
func newStatusServer(ln net.Listener, store *stateStore) *statusServer {
	return &statusServer{listener: ln, store: store}
}

// serve runs the accept loop until the listener is closed. Errors from
// individual Accept calls are silently ignored unless the listener has
// been closed (the typical shutdown path), in which case the loop exits.
//
// One goroutine per connection. Connections are short-lived and
// independent: a slow client doesn't block other clients or the main
// loop.
func (s *statusServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient accept errors are rare on Unix sockets; fall
			// through and try again so a single failure doesn't kill
			// the daemon's status surface.
			continue
		}
		go s.handle(conn)
	}
}

// handle writes one status response to conn and closes it. The on-the-wire
// shape matches SPEC §"calendar-sync status" stdout: one JSON object per
// line, ending with a `_meta` trailer.
func (s *statusServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	snap := s.store.snapshot()
	writeStatusJSONL(conn, snap)
}

// statusHeader is the first JSONL line of the status response. SPEC line
// 725 pins the field names: reachable / pid / started_at / poll_interval /
// full_sync_interval / last_full_sync_at.
type statusHeader struct {
	Reachable        bool   `json:"reachable"`
	PID              int    `json:"pid"`
	StartedAt        string `json:"started_at"`
	PollInterval     string `json:"poll_interval"`
	FullSyncInterval string `json:"full_sync_interval"`
	LastFullSyncAt   string `json:"last_full_sync_at,omitempty"`
}

// statusPDir is one per-pdir line of the status response. SPEC lines 726-
// 727 pin the field names and the `<pair>:<direction>` formatting of the
// pdir field.
type statusPDir struct {
	PDir              string `json:"pdir"`
	SourceCalendar    string `json:"source_calendar"`
	TargetCalendar    string `json:"target_calendar"`
	Mirrors           int    `json:"mirrors"`
	LastTickAt        string `json:"last_tick_at,omitempty"`
	LastTickStatus    string `json:"last_tick_status,omitempty"`
	LastTickInserts   int    `json:"last_tick_inserts"`
	LastTickPatches   int    `json:"last_tick_patches"`
	LastTickDeletes   int    `json:"last_tick_deletes"`
	LastTickPropagates int   `json:"last_tick_propagates"`
	LastTickReverts   int    `json:"last_tick_reverts"`
	LastTickSkips     int    `json:"last_tick_skips"`
}

// statusMeta is the `_meta` trailer. SPEC line 728 shows `count` =
// number of pdirs in the response.
type statusMeta struct {
	Count int `json:"count"`
}

// writeStatusJSONL serializes a stateSnapshot in SPEC's wire format. Errors
// from json.Marshal are unreachable for this struct shape (no maps, no
// channels); writer errors are silently dropped because there's nothing
// useful the daemon can do about a client that disconnected.
func writeStatusJSONL(w net.Conn, snap stateSnapshot) {
	header := statusHeader{
		Reachable:        true,
		PID:              snap.PID,
		StartedAt:        snap.StartedAt.UTC().Format(time.RFC3339),
		PollInterval:     compactDuration(snap.PollInterval),
		FullSyncInterval: compactDuration(snap.FullSyncInterval),
	}
	if !snap.LastFullSyncAt.IsZero() {
		header.LastFullSyncAt = snap.LastFullSyncAt.UTC().Format(time.RFC3339)
	}
	writeJSONLine(w, header)

	for _, pd := range snap.PDirs {
		row := statusPDir{
			PDir:              pd.Pair + ":" + pd.Direction,
			SourceCalendar:    pd.SourceCalendar,
			TargetCalendar:    pd.TargetCalendar,
			Mirrors:           pd.Mirrors,
			LastTickStatus:    pd.LastTickStatus,
			LastTickInserts:   pd.Counts.Inserts,
			LastTickPatches:   pd.Counts.Patches,
			LastTickDeletes:   pd.Counts.Deletes,
			LastTickPropagates: pd.Counts.Propagates,
			LastTickReverts:   pd.Counts.Reverts,
			LastTickSkips:     pd.Counts.Skips,
		}
		if !pd.LastTickAt.IsZero() {
			row.LastTickAt = pd.LastTickAt.UTC().Format(time.RFC3339)
		}
		writeJSONLine(w, row)
	}

	writeJSONLine(w, struct {
		Meta statusMeta `json:"_meta"`
	}{Meta: statusMeta{Count: len(snap.PDirs)}})
}

// writeJSONLine marshals v and writes one JSON line plus newline to w.
func writeJSONLine(w net.Conn, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
}

// compactDuration formats d in the compact form SPEC.md uses for the IPC
// status response (line 725: "60s", "24h" - not Go's verbose "1m0s" or
// "24h0m0s"). The rule is:
//
//   - whole hours (value % 1h == 0 && value > 0): emit as "<N>h"
//   - everything else: emit as "<N>s" (seconds)
//
// Deliberately doesn't auto-promote 60 seconds to "1m" or 5 minutes to
// "5m" - the SPEC example shows "60s" for the default poll_interval, and
// promoting whole minutes would round-trip the user's "60s" config to
// "1m" on the wire. Promoting whole hours IS done because SPEC shows
// "24h" (not "86400s" or "1440m") for the default full_sync_interval.
//
// Sub-second precision falls back to time.Duration.String. The daemon's
// settings validate to >=15s (poll_interval) and >=1h (full_sync_interval)
// so the fallback is never reached in production.
func compactDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}
