package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// tmpSocketPath returns a short Unix socket path suitable for a test.
// macOS imposes a 104-byte limit on socket paths; t.TempDir() typically
// uses /var/folders/.../T/<test-name>... which is already 60+ bytes. We
// build a short path under os.TempDir() with a per-test random suffix and
// register cleanup so the file is removed after the test.
func tmpSocketPath(t *testing.T) string {
	t.Helper()
	// 8 hex chars from the test process suffices for uniqueness across
	// the small number of socket tests in this package.
	name := fmt.Sprintf("cs-%d-%d.sock", os.Getpid(), nextSocketSeq.Add(1))
	path := filepath.Join(os.TempDir(), name)
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// nextSocketSeq is a monotonic counter used to produce unique socket
// filenames across tests in a single test binary run.
var nextSocketSeq atomic.Int64

// TestBindSocket_FreshPath: no existing file → bind succeeds.
func TestBindSocket_FreshPath(t *testing.T) {
	path := tmpSocketPath(t)
	defer os.Remove(path)

	ln, err := bindSocket(path)
	if err != nil {
		t.Fatalf("bindSocket = %v", err)
	}
	defer ln.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("socket file not created: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("created file is not a socket: mode=%v", info.Mode())
	}
}

// TestBindSocket_StaleNonSocketUnlinked: a regular file at the path is
// treated as stale and unlinked, then the bind proceeds.
func TestBindSocket_StaleNonSocketUnlinked(t *testing.T) {
	path := tmpSocketPath(t)
	defer os.Remove(path)

	// Create a regular file at the socket path.
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	ln, err := bindSocket(path)
	if err != nil {
		t.Fatalf("bindSocket = %v", err)
	}
	defer ln.Close()

	// After bind, the path should be a socket, not a regular file.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after bind: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("after bind, file is not a socket: mode=%v", info.Mode())
	}
}

// TestBindSocket_StaleECONNREFUSEDUnlinked: a leftover socket file with
// no listener (typical after a crash) returns ECONNREFUSED on dial; the
// bind should unlink and recreate.
func TestBindSocket_StaleSocketECONNREFUSEDUnlinked(t *testing.T) {
	path := tmpSocketPath(t)
	defer os.Remove(path)

	// Bind once and immediately close the listener WITHOUT removing the
	// file - this leaves a stale socket inode (the natural Unix behavior
	// is for the file to remain after Close on some platforms, especially
	// when the listener isn't a UnixListener with SetUnlinkOnClose).
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ln1, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	// Disable Go's auto-unlink so the file lingers.
	ln1.SetUnlinkOnClose(false)
	if err := ln1.Close(); err != nil {
		t.Fatalf("close ln1: %v", err)
	}

	// Confirm the file still exists.
	if _, err := os.Stat(path); err != nil {
		t.Skipf("socket file does not survive Close on this platform: %v", err)
	}

	// bindSocket should unlink + relisten.
	ln2, err := bindSocket(path)
	if err != nil {
		t.Fatalf("bindSocket on stale = %v", err)
	}
	defer ln2.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected new socket file at %s after rebind: %v", path, err)
	}
}

// TestBindSocket_ActiveListenerReturnsErrAlreadyRunning: an already-listening
// socket means another daemon is alive; bindSocket returns
// ErrDaemonAlreadyRunning.
func TestBindSocket_ActiveListenerReturnsErrAlreadyRunning(t *testing.T) {
	path := tmpSocketPath(t)
	defer os.Remove(path)

	first, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer first.Close()

	second, err := bindSocket(path)
	if err == nil {
		second.Close()
		t.Fatalf("expected ErrDaemonAlreadyRunning; got nil")
	}
	if !errors.Is(err, ErrDaemonAlreadyRunning) {
		t.Errorf("err = %v, want ErrDaemonAlreadyRunning", err)
	}
}

// TestStatusServer_ReportsHeaderAndPDirs: connecting to the socket
// returns SPEC's JSONL shape (header + per-pdir lines + _meta trailer).
func TestStatusServer_ReportsHeaderAndPDirs(t *testing.T) {
	path := tmpSocketPath(t)
	ln, err := bindSocket(path)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()
	defer os.Remove(path)

	settings := config.Settings{
		PollInterval:     mustDuration("60s"),
		FullSyncInterval: mustDuration("24h"),
	}
	pdirs := []config.PDir{
		{
			PairName:       "work-personal",
			Direction:      config.PDirAtoB,
			SourceCalendar: "alice@example.com",
			TargetCalendar: "alice.personal@example.org",
		},
	}
	startedAt := time.Date(2026, 4, 30, 8, 0, 0, 0, time.UTC)
	store := newStateStore(54321, startedAt, settings, pdirs)
	store.recordFullSync(startedAt, syncpkg.FullSyncResult{
		PDirs: []syncpkg.PDirResult{
			{
				Pair:      "work-personal",
				Direction: config.PDirAtoB,
				Counts:    syncpkg.Counts{Patches: 1, EventsProcessed: 1},
			},
		},
	}, map[string]int{"alice.personal@example.org": 42})

	server := newStatusServer(ln, store)
	go server.serve()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	br := bufio.NewReader(conn)
	headerLine, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	var header statusHeader
	if err := json.Unmarshal(headerLine, &header); err != nil {
		t.Fatalf("decode header: %v (line=%q)", err, headerLine)
	}
	if !header.Reachable {
		t.Errorf("header.Reachable = false, want true")
	}
	if header.PID != 54321 {
		t.Errorf("header.PID = %d, want 54321", header.PID)
	}
	// SPEC line 725 shows compact form ("60s", "24h"), not Go's verbose
	// "1m0s" / "24h0m0s". compactDuration drops zero sub-units.
	if header.PollInterval != "60s" {
		t.Errorf("header.PollInterval = %q, want 60s", header.PollInterval)
	}
	if header.FullSyncInterval != "24h" {
		t.Errorf("header.FullSyncInterval = %q, want 24h", header.FullSyncInterval)
	}
	if header.LastFullSyncAt != "2026-04-30T08:00:00Z" {
		t.Errorf("header.LastFullSyncAt = %q, want 2026-04-30T08:00:00Z", header.LastFullSyncAt)
	}

	pdirLine, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read pdir line: %v", err)
	}
	var pdir statusPDir
	if err := json.Unmarshal(pdirLine, &pdir); err != nil {
		t.Fatalf("decode pdir: %v (line=%q)", err, pdirLine)
	}
	if pdir.PDir != "work-personal:a_to_b" {
		t.Errorf("pdir.PDir = %q, want work-personal:a_to_b", pdir.PDir)
	}
	if pdir.Mirrors != 42 {
		t.Errorf("pdir.Mirrors = %d, want 42", pdir.Mirrors)
	}
	if pdir.LastTickPatches != 1 {
		t.Errorf("pdir.LastTickPatches = %d, want 1", pdir.LastTickPatches)
	}
	if pdir.LastTickStatus != "ok" {
		t.Errorf("pdir.LastTickStatus = %q, want ok", pdir.LastTickStatus)
	}

	metaLine, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read meta line: %v", err)
	}
	var meta struct {
		Meta statusMeta `json:"_meta"`
	}
	if err := json.Unmarshal(metaLine, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.Meta.Count != 1 {
		t.Errorf("meta.Count = %d, want 1", meta.Meta.Count)
	}

	// Server closes after sending; we don't assert on the post-trailer
	// read because both EOF and closed-connection errors are acceptable.
}

// TestStatusServer_FailedPDirShowsFailedStatus: a PDirResult with Err
// non-nil renders LastTickStatus="failed".
func TestStatusServer_FailedPDirShowsFailedStatus(t *testing.T) {
	path := tmpSocketPath(t)
	ln, err := bindSocket(path)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()
	defer os.Remove(path)

	settings := config.Settings{
		PollInterval:     mustDuration("60s"),
		FullSyncInterval: mustDuration("24h"),
	}
	pdirs := []config.PDir{
		{
			PairName:       "p1",
			Direction:      config.PDirAtoB,
			SourceCalendar: "src",
			TargetCalendar: "tgt",
		},
	}
	store := newStateStore(1, time.Now(), settings, pdirs)
	store.recordTick(time.Now(), syncpkg.TickResult{
		PDirs: []syncpkg.PDirResult{
			{Pair: "p1", Direction: config.PDirAtoB, Err: errors.New("boom")},
		},
	}, nil)

	server := newStatusServer(ln, store)
	go server.serve()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	br := bufio.NewReader(conn)
	_, _ = br.ReadBytes('\n') // header
	pdirLine, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read pdir: %v", err)
	}
	var pdir statusPDir
	if err := json.Unmarshal(pdirLine, &pdir); err != nil {
		t.Fatalf("decode pdir: %v", err)
	}
	if pdir.LastTickStatus != "failed" {
		t.Errorf("LastTickStatus = %q, want failed", pdir.LastTickStatus)
	}
}

// mustDuration parses a duration string for tests; panics on error so the
// test source stays uncluttered.
func mustDuration(s string) config.Duration {
	var d config.Duration
	if err := d.UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return d
}

func TestCompactDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		// SPEC example values.
		{60 * time.Second, "60s"},
		{24 * time.Hour, "24h"},
		// Validates poll_interval minimum and other typical values.
		{15 * time.Second, "15s"},
		{90 * time.Second, "90s"},
		// 5 minutes stays in seconds (SPEC doesn't auto-promote sub-hour values).
		{5 * time.Minute, "300s"},
		// Whole hours promote.
		{time.Hour, "1h"},
		{30 * 24 * time.Hour, "720h"}, // 30d → 720h after Duration.Duration()
		// Edges.
		{0, "0s"},
		{-5 * time.Second, "0s"},
		// Sub-second / mixed-precision falls back to time.Duration.String.
		{time.Second + 500*time.Millisecond, "1.5s"},
	}
	for _, tc := range tests {
		got := compactDuration(tc.in)
		if got != tc.want {
			t.Errorf("compactDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
