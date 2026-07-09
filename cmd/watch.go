package cmd

import (
	"time"

	"github.com/tammersaleh/calendar-sync/internal/daemon"
	"github.com/tammersaleh/calendar-sync/internal/feedimport"
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

	// SPEC line 488: --timeout is the "wall-clock cap for any single
	// source-list or mirror-list call." Wrap the gws client so every
	// gws subprocess invocation gets bounded at the call boundary,
	// INCLUDING the calendarList resolution Canonicalize calls. A
	// wedged gws subprocess (network blip, hung helper) returns
	// context.DeadlineExceeded after timeout instead of hanging the
	// daemon's startup or any subsequent pass.
	api := newCallTimeoutAPI(rt.gwsClient(), c.Timeout)
	canonical, err := cfg.Canonicalize(rt.Ctx, api)
	if err != nil {
		return err
	}

	// SPEC line 253: `[settings].dry_run = true` must suppress writes. The
	// daemon has no `--dry-run` flag (long-running by design), so settings
	// is the only way to flip the wrapper for `watch`.
	if canonical.Settings.DryRun {
		api = newDryRunAPI(api)
	}
	opts := []syncpkg.Option{}
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

	// Feed-import phase. One long-lived Runner over the same timeout-wrapped
	// api (dry-run-wrapped above when settings.dry_run is set); each Fetcher
	// keeps its ETag/cache gate across ticks. No feeds => leave d.Feeds nil so
	// the daemon skips the phase. The Importer.DryRun flag mirrors the
	// effective settings dry-run (watch has no --dry-run flag).
	if feeds := feedConfigs(canonical.Feeds); len(feeds) > 0 {
		d.Feeds = feedimport.NewRunner(api, feeds, canonical.Settings.DryRun, time.Now, rt.Logger)
	}

	return d.Run(rt.Ctx)
}
