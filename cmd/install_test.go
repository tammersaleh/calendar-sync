package cmd

import (
	"bytes"
	"context"
	"runtime"
	"testing"
)

func TestInstallCmd_NonDarwinReturnsNotMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("test exercises the non-darwin error path")
	}
	rt := &Runtime{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Ctx:    context.Background(),
	}
	err := (&InstallCmd{NoLoad: true}).Run(rt)
	if err == nil {
		t.Fatalf("expected not_macos on %s", runtime.GOOS)
	}
	code, _, _ := MapError(err)
	if code != "not_macos" {
		t.Errorf("code = %q, want not_macos", code)
	}
}

func TestUninstallCmd_NonDarwinReturnsNotMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("test exercises the non-darwin error path")
	}
	rt := &Runtime{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Ctx:    context.Background(),
	}
	err := (&UninstallCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected not_macos on %s", runtime.GOOS)
	}
	code, _, _ := MapError(err)
	if code != "not_macos" {
		t.Errorf("code = %q, want not_macos", code)
	}
}
