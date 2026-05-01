package cmd

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/output"
)

// statusDialTimeout caps the connect attempt so a misbehaving daemon
// doesn't hang `status` for the user. Generous compared to the daemon-
// side bind probe (200ms) because this is a real RPC, not a startup race.
const statusDialTimeout = 2 * time.Second

// StatusCmd implements `calendar-sync status`. SPEC §"calendar-sync status"
// lines 713-744. Connects to the daemon's IPC socket; on success forwards
// the bytes verbatim to stdout. On ECONNREFUSED or a missing file, emits
// `{"reachable":false}` + `{"_meta":{"count":0}}`.
type StatusCmd struct{}

// Run is the SPEC's three-branch decision matrix:
//
//  1. Connect succeeds: read all bytes, write to stdout. The daemon writes
//     the full JSONL response (header + per-pdir + meta) and closes; we
//     trust its framing per SPEC line 725.
//  2. Connect fails with ECONNREFUSED OR file does not exist: emit
//     `{"reachable":false}` + `{"_meta":{"count":0}}`.
//  3. Other I/O errors: surface as socket_error (exit 1).
func (c *StatusCmd) Run(rt *Runtime) error {
	path := defaultSocketPath()

	conn, err := net.DialTimeout("unix", path, statusDialTimeout)
	if err != nil {
		if isStaleSocketErr(err) {
			emitUnreachable(rt)
			return nil
		}
		return newCmdError(output.CodeSocketError,
			"connect to daemon socket: "+err.Error(), "", err)
	}
	defer func() { _ = conn.Close() }()

	body, err := io.ReadAll(conn)
	if err != nil {
		return newCmdError(output.CodeSocketError,
			"read daemon response: "+err.Error(), "", err)
	}
	if rt.Stdout != nil {
		if _, err := rt.Stdout.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// emitUnreachable writes SPEC's daemon-not-reachable JSONL: one
// `{"reachable":false}` line plus the empty `_meta` trailer.
func emitUnreachable(rt *Runtime) {
	p := rt.printer()
	p.Emit(struct {
		Reachable bool `json:"reachable"`
	}{Reachable: false})
	p.Meta(metaCount{Count: 0})
}

// defaultSocketPath returns SPEC's default `$TMPDIR/calendar-sync.sock`.
// Mirrored from the daemon package (we don't import it just for this
// constant; the path is part of SPEC's user-facing contract).
func defaultSocketPath() string {
	dir := os.Getenv("TMPDIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "calendar-sync.sock")
}

// isStaleSocketErr reports whether dial err means "no daemon listening" -
// either ECONNREFUSED or the file doesn't exist. Other I/O failures
// (permissions, wrong file type) surface as socket_error per SPEC line
// 744.
func isStaleSocketErr(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	return false
}
