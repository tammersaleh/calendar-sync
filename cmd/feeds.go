package cmd

import (
	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/feedimport"
)

// feedConfigs flattens the resolved canonical feeds into the feedimport
// primitives the Runner takes. feedimport does not import internal/config, so
// this mapping lives in the command layer. The URL secret is carried through
// to the Runner (held on its entry, never logged); everything else redacts it.
// Returns nil for an empty feed list so callers can leave Daemon.Feeds nil.
func feedConfigs(feeds []config.CanonicalFeed) []feedimport.FeedConfig {
	if len(feeds) == 0 {
		return nil
	}
	out := make([]feedimport.FeedConfig, 0, len(feeds))
	for _, f := range feeds {
		out = append(out, feedimport.FeedConfig{
			Name:            f.Name,
			URL:             f.URL,
			TargetCalendar:  f.TargetCalendar,
			ForceAllDayBusy: f.ForceAllDayBusy,
		})
	}
	return out
}
