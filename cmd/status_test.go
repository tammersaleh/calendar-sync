package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusCmd_NoSocketEmitsUnreachable(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	stdout := &bytes.Buffer{}
	rt := &Runtime{Stdout: stdout, Stderr: &bytes.Buffer{}, Ctx: context.Background()}

	if err := (&StatusCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), stdout.String())
	}
	var first struct {
		Reachable bool `json:"reachable"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first line: %v", err)
	}
	if first.Reachable {
		t.Errorf("reachable = true, want false")
	}
}

func TestStatusCmd_StaleSocketEmitsUnreachable(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	// Create a regular file at the socket path; dial will fail with
	// !ECONNREFUSED, but the regular file still triggers stale-handling.
	stale := filepath.Join(tmp, "calendar-sync.sock")
	if err := os.WriteFile(stale, []byte{}, 0o644); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	stdout := &bytes.Buffer{}
	rt := &Runtime{Stdout: stdout, Stderr: &bytes.Buffer{}, Ctx: context.Background()}
	err := (&StatusCmd{}).Run(rt)
	// Either succeeds with reachable=false (some Unixes report ECONNREFUSED
	// for non-socket files) or surfaces socket_error. Both are acceptable
	// per SPEC's "Socket file exists with the wrong type" wording.
	if err == nil {
		var first struct {
			Reachable bool `json:"reachable"`
		}
		first.Reachable = true
		if line := strings.SplitN(stdout.String(), "\n", 2)[0]; line != "" {
			_ = json.Unmarshal([]byte(line), &first)
		}
		if first.Reachable {
			t.Errorf("expected unreachable (or error), got reachable=true")
		}
		return
	}
	code, _, _ := MapError(err)
	if code != "socket_error" {
		t.Errorf("code = %q, want socket_error", code)
	}
}

func TestStatusCmd_ConnectForwardsBytes(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	sockPath := filepath.Join(tmp, "calendar-sync.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	const fakeResponse = `{"reachable":true,"pid":42}` + "\n" + `{"_meta":{"count":0}}` + "\n"
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte(fakeResponse))
	}()

	stdout := &bytes.Buffer{}
	rt := &Runtime{Stdout: stdout, Stderr: &bytes.Buffer{}, Ctx: context.Background()}
	if err := (&StatusCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stdout.String(); got != fakeResponse {
		t.Errorf("forwarded bytes = %q, want %q", got, fakeResponse)
	}
}
