package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/output"
)

// starterConfigTemplate is the content `calendar-sync init` writes when no
// destination file exists. It serves as a worked example: a single
// disabled pair plus comments pointing at SPEC for the fields that aren't
// shown. Post-v2.0.0 every pair is implicitly source-to-target; a
// bidirectional setup declares two pairs with swapped source/target.
//
// Literal `\n` newlines (not raw strings with embedded tabs) keep the file
// portable across editors and avoid TOML's whitespace quirks.
const starterConfigTemplate = `# calendar-sync configuration.
#
# Edit this file, then run:
#   calendar-sync config validate
#   calendar-sync install
#
# See https://github.com/tammersaleh/calendar-sync for the full schema.

[settings]
poll_interval      = "60s"
horizon            = "365d"
full_sync_interval = "24h"
log_level          = "info"
log_format         = "json"
dry_run            = false

# Two-way sync gate. Defaults to false: source-side edits flow into
# mirrors, but mirror-side edits get reverted on the next tick (the
# source is never modified). Flip to true once you've confirmed the
# one-way path behaves as expected and you actually want bidirectional
# sync. Pdirs whose source is read-only (accessRole < writer) always
# revert regardless of this setting.
# propagate_target_edits = true

[[pairs]]
name      = "work-personal"
source    = "you@work.example"
target    = "primary"
enabled   = false  # set to true after editing the source/target above.
`

// InitCmd implements `calendar-sync init`. SPEC §"calendar-sync init" lines
// 549-572: write a starter config to $XDG_CONFIG_HOME (default) or
// --output. --force overwrites; without it, an existing destination
// returns config_exists.
type InitCmd struct {
	Output string `name:"output" placeholder:"<path>" help:"Where to write. Default: $XDG_CONFIG_HOME/calendar-sync/config.toml."`
	Force  bool   `name:"force" help:"Overwrite an existing file."`
}

// Run resolves the destination, refuses to overwrite without --force, then
// writes the starter template + emits the SPEC's JSONL line.
func (c *InitCmd) Run(rt *Runtime) error {
	dest := c.Output
	if dest == "" {
		dest = config.FindPath(rt.Globals.Config)
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return newCmdError(output.CodeWriteFailed,
			fmt.Sprintf("resolve destination %q: %s", dest, err), "", err)
	}

	if !c.Force {
		if _, err := os.Stat(abs); err == nil {
			return newCmdError(output.CodeConfigExists,
				fmt.Sprintf("config file already exists at %s", abs),
				"Re-run with --force to overwrite.", nil)
		} else if !errors.Is(err, os.ErrNotExist) {
			return newCmdError(output.CodeWriteFailed,
				fmt.Sprintf("stat %s: %s", abs, err), "", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return newCmdError(output.CodeWriteFailed,
			fmt.Sprintf("create config dir %s: %s", filepath.Dir(abs), err), "", err)
	}
	if err := os.WriteFile(abs, []byte(starterConfigTemplate), 0o644); err != nil {
		return newCmdError(output.CodeWriteFailed,
			fmt.Sprintf("write %s: %s", abs, err), "", err)
	}

	p := rt.printer()
	p.Emit(initResult{Path: abs, Status: "created"})
	p.Meta(metaCount{Count: 1})
	return nil
}

// initResult is SPEC line 563's wire shape: `{"path":"...","status":"created"}`.
type initResult struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}
