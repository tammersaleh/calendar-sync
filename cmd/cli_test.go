package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_VersionShortPrintsBareString(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	prev := Version
	t.Cleanup(func() { Version = prev })
	Version = "9.9.9"

	code := Run([]string{"version", "--short"}, stdout, stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "9.9.9") {
		t.Errorf("stdout = %q, want to contain 9.9.9", got)
	}
}

func TestRun_UnknownSubcommandIs64(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	code := Run([]string{"definitely-not-a-command"}, stdout, stderr)
	if code != 64 {
		t.Errorf("exit code = %d, want 64", code)
	}
	if stderr.Len() == 0 {
		t.Errorf("expected usage on stderr")
	}
}

func TestRun_NoArgsShowsUsage(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	code := Run([]string{}, stdout, stderr)
	if code != 64 {
		t.Errorf("exit code = %d, want 64", code)
	}
}
