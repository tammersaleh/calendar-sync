package cmd

import (
	_ "embed"
	"fmt"
)

// skillContent is the SKILL.md text shipped alongside the binary. SPEC line
// 831 says `calendar-sync skill` writes the contents of SKILL.md to stdout
// verbatim; the file is embedded so a `go install` / `homebrew install`
// produces a self-contained binary.
//
//go:embed SKILL.md
var skillContent string

// SkillCmd implements `calendar-sync skill`. SPEC §"calendar-sync skill"
// (lines 824-831): no flags; emit SKILL.md verbatim to stdout. Notably this
// is NOT JSONL - SPEC line 831 says "Stdout: contents of SKILL.md."; the
// command exists so an LLM agent can pipe the content into ~/.claude/skills.
type SkillCmd struct{}

// Run writes the embedded SKILL.md to rt.Stdout. --quiet suppresses the
// write (rt.Stdout is nil in that case).
func (c *SkillCmd) Run(rt *Runtime) error {
	if rt.Stdout == nil {
		return nil
	}
	if _, err := fmt.Fprint(rt.Stdout, skillContent); err != nil {
		return err
	}
	return nil
}
