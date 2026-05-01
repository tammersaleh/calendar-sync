package cmd

import (
	"github.com/tammersaleh/calendar-sync/internal/launchd"
)

// UninstallCmd implements `calendar-sync uninstall`. SPEC §"calendar-sync
// uninstall" lines 801-822.
type UninstallCmd struct {
	KeepPlist bool   `name:"keep-plist" help:"Unload but don't delete the plist file."`
	Label     string `name:"label" placeholder:"<id>" default:"org.calendar-sync.agent" help:"launchd Label. Default: org.calendar-sync.agent."`
}

// Run wraps launchd.Uninstall.
func (c *UninstallCmd) Run(rt *Runtime) error {
	cfg := launchd.UninstallConfig{
		Label:     c.Label,
		KeepPlist: c.KeepPlist,
	}
	res, err := launchd.Uninstall(rt.Ctx, cfg, launchd.ExecRunner{})
	if err != nil {
		return err
	}
	p := rt.printer()
	p.Emit(uninstallResult{
		Plist:    res.PlistPath,
		Unloaded: res.Unloaded,
		Removed:  res.Removed,
	})
	p.Meta(metaCount{Count: 1})
	return nil
}

// uninstallResult is SPEC line 811's wire shape.
type uninstallResult struct {
	Plist    string `json:"plist"`
	Unloaded bool   `json:"unloaded"`
	Removed  bool   `json:"removed"`
}
