package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestSkillCmd_WritesEmbeddedFileVerbatim(t *testing.T) {
	stdout := &bytes.Buffer{}
	rt := &Runtime{Stdout: stdout, Stderr: &bytes.Buffer{}}

	if err := (&SkillCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := stdout.String()
	if got != skillContent {
		t.Errorf("output mismatch\ngot:\n%s\nwant:\n%s", got, skillContent)
	}
	if !strings.Contains(got, "calendar-sync") {
		t.Errorf("expected SKILL.md to mention calendar-sync, got %q", got)
	}
}

func TestSkillCmd_QuietSuppresses(t *testing.T) {
	stdout := &bytes.Buffer{}
	rt := &Runtime{Stdout: nil, Stderr: stdout}

	if err := (&SkillCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("nil stdout should suppress output, got %q", stdout.String())
	}
}
