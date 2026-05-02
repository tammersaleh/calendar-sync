package cmd

import (
	"context"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// GwsClient is the union of every gws.Client method any subcommand might
// need. The interface is satisfied by *gws.Client; tests inject a stub via
// Runtime.Gws to avoid spawning the real subprocess.
//
// The sync.API and config.CalendarLister sub-interfaces are subsets of this
// shape, so a single GwsClient value can feed the run/watch/mirror paths
// and the canonicalize step alike.
type GwsClient interface {
	syncpkg.API
	config.CalendarLister
}

// gwsClient returns the runtime's injected GwsClient (tests) or a fresh
// production *gws.Client. Lazy construction so tests that stub the runtime
// never accidentally hit the real binary.
//
// Production callers get a Client with the runtime's logger pre-wired, so
// every gws subprocess invocation emits a debug log line that callers can
// see by running with --log-level=debug.
func (rt *Runtime) gwsClient() GwsClient {
	if rt.Gws != nil {
		return rt.Gws
	}
	if rt.Logger != nil {
		return gws.New(gws.WithLogger(rt.Logger))
	}
	return gws.New()
}

// Compile-time assertion that *gws.Client satisfies GwsClient. Catches
// signature drift in either package without waiting for a runtime path.
var _ GwsClient = (*gws.Client)(nil)

// AuthChecker is the production hook for `gws auth status`. Defined as a
// var so tests can replace it with a closure that returns nil (success)
// or a sentinel error.
var AuthChecker = func(ctx context.Context) error {
	return runGwsAuthStatus(ctx)
}
