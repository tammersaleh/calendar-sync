package cmd

import (
	"github.com/tammersaleh/calendar-sync/internal/launchd"
)

// InstallCmd implements `calendar-sync install`. SPEC §"calendar-sync
// install" lines 746-799. Generates the launchd plist + (unless --no-load)
// runs `launchctl load -w`.
type InstallCmd struct {
	LogDir string `name:"log-dir" placeholder:"<path>" help:"Where launchd writes stdout/stderr. Default: ~/Library/Logs/calendar-sync/."`
	Label  string `name:"label" placeholder:"<id>" default:"org.calendar-sync.agent" help:"launchd Label. Default: org.calendar-sync.agent."`
	Force  bool   `name:"force" help:"Overwrite an existing plist."`
	NoLoad bool   `name:"no-load" help:"Write the plist but don't launchctl load it."`
}

// Run wraps launchd.Install.
func (c *InstallCmd) Run(rt *Runtime) error {
	cfg := launchd.Config{
		Label:  c.Label,
		LogDir: c.LogDir,
		Force:  c.Force,
		NoLoad: c.NoLoad,
	}
	res, err := launchd.Install(rt.Ctx, cfg, launchd.ExecRunner{})
	if err != nil {
		return err
	}
	p := rt.printer()
	p.Emit(installResult{Plist: res.PlistPath, Loaded: res.Loaded})
	p.Meta(metaCount{Count: 1})
	return nil
}

// installResult is SPEC line 760's wire shape.
type installResult struct {
	Plist  string `json:"plist"`
	Loaded bool   `json:"loaded"`
}
