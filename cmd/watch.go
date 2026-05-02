package cmd

import (
	"time"

	"github.com/tammersaleh/calendar-sync/internal/daemon"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// WatchCmd implements `calendar-sync watch`. SPEC §"calendar-sync watch"
// lines 419-434: the long-running daemon. Wires gws.Client + AuthChecker
// + sync.Reconciler + daemon.Daemon and blocks until SIGTERM / SIGINT /
// fatal startup error.
type WatchCmd struct {
	Timeout time.Duration `name:"timeout" placeholder:"<dur>" default:"5m" help:"Wall-clock cap for any single source-list or mirror-list call. Default: 5m."`
}

// Run loads + canonicalizes config, constructs the Reconciler + Daemon,
// then blocks on Daemon.Run.
func (c *WatchCmd) Run(rt *Runtime) error {
	cfg, err := loadConfig(rt)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	canonical, err := cfg.Canonicalize(rt.Ctx, rt.gwsClient())
	if err != nil {
		return err
	}

	api := rt.gwsClient()
	opts := []syncpkg.Option{
		syncpkg.WithHorizon(canonical.Settings.Horizon.Duration()),
		syncpkg.WithPropagateTargetEdits(canonical.Settings.PropagateTargetEdits),
	}
	if rt.Logger != nil {
		opts = append(opts, syncpkg.WithLogger(rt.Logger))
	}
	reconciler := syncpkg.New(api, canonical, opts...)

	d := &daemon.Daemon{
		Reconciler:  reconciler,
		Canonical:   canonical,
		AuthChecker: AuthChecker,
		Stdout:      rt.Stdout,
	}
	return d.Run(rt.Ctx)
}
