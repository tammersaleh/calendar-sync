package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// --- feedConfigs unit test -------------------------------------------------

func TestFeedConfigs(t *testing.T) {
	if got := feedConfigs(nil); got != nil {
		t.Errorf("feedConfigs(nil) = %v, want nil", got)
	}
	if got := feedConfigs([]config.CanonicalFeed{}); got != nil {
		t.Errorf("feedConfigs([]) = %v, want nil", got)
	}

	in := []config.CanonicalFeed{
		{Name: "trip", URL: "https://feeds.example.com/private/TOKEN-A/cal.ics", TargetCalendar: "travel@example.com"},
		{Name: "work-cal", URL: "https://other.example.com/x/TOKEN-B/y.ics", TargetCalendar: "workfeed@example.com"},
	}
	got := feedConfigs(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for i, want := range in {
		if got[i].Name != want.Name || got[i].URL != want.URL || got[i].TargetCalendar != want.TargetCalendar {
			t.Errorf("feedConfigs[%d] = %+v, want name/url/target %q/%q/%q",
				i, got[i], want.Name, want.URL, want.TargetCalendar)
		}
	}
}

// --- recording gws stub ----------------------------------------------------

// recordingGws embeds stubGws (all zero-value reads) and records every
// EventsInsert so a test can prove which calendar the feed importer wrote to.
type recordingGws struct {
	stubGws
	mu      sync.Mutex
	inserts []string // calendarIDs, in call order
}

func (r *recordingGws) EventsInsert(_ context.Context, calendarID string, body *gws.Event) (*gws.Event, error) {
	r.mu.Lock()
	r.inserts = append(r.inserts, calendarID)
	r.mu.Unlock()
	out := &gws.Event{}
	if body != nil {
		*out = *body
	}
	if out.ID == "" {
		out.ID = "generated-id"
	}
	return out, nil
}

func (r *recordingGws) insertsTo(calendarID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.inserts {
		if c == calendarID {
			n++
		}
	}
	return n
}

// syntheticICS is a minimal, fully synthetic VCALENDAR (no real data or
// tokens - this is a public repo) carrying one timed VEVENT.
const syntheticICS = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//synthetic//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:synthetic-1@example.test\r\n" +
	"SUMMARY:Synthetic Trip\r\n" +
	"DTSTART:20260713T120000Z\r\n" +
	"DTEND:20260713T130000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// feedFixtureTOML builds a one-pair + one-feed config whose feed URL points at
// the given (httptest) URL.
func feedFixtureTOML(feedURL string, settingsDryRun bool) string {
	dryLine := ""
	if settingsDryRun {
		dryLine = "dry_run = true\n"
	}
	return fmt.Sprintf(`
[settings]
poll_interval      = "60s"
horizon            = "365d"
full_sync_interval = "24h"
log_level          = "info"
log_format         = "json"
%s
[[pairs]]
name   = "work-personal"
source = "work@example.com"
target = "personal@example.com"

[[feeds]]
name   = "trip"
url    = "%s"
target = "travel@example.com"
`, dryLine, feedURL)
}

// startICSServer serves the synthetic .ics and flips hit=true on each request.
func startICSServer(t *testing.T, hit *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hit != nil {
			*hit = true
		}
		w.Header().Set("Content-Type", "text/calendar")
		_, _ = w.Write([]byte(syntheticICS))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- e2e: feed phase inserts into the feed target --------------------------

// TestRunCmd_FeedPhaseInsertsIntoTarget drives a full (unscoped) `run` end to
// end: canonicalize resolves the feed, the feed phase fetches the synthetic
// .ics from an httptest server, parses it, and reconciles it into the feed's
// target calendar. Proves feedConfigs + api wiring + Runner.RunOnce all
// connect, and that the recorded events.insert landed on travel@example.com.
func TestRunCmd_FeedPhaseInsertsIntoTarget(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	srv := startICSServer(t, nil)
	path := writeConfigFixture(t, feedFixtureTOML(srv.URL, false))

	stub := &recordingGws{}
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     stub,
	}
	if err := (&RunCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stub.insertsTo("travel@example.com"); got != 1 {
		t.Errorf("inserts to feed target = %d, want 1 (feed phase should have inserted the synthetic event)", got)
	}
	// The pair's source list is empty, so nothing should have been mirrored
	// into the pair target.
	if got := stub.insertsTo("personal@example.com"); got != 0 {
		t.Errorf("inserts to pair target = %d, want 0", got)
	}
}

// TestRunCmd_FeedPhaseSettingsDryRunSuppressesInsert pins the effective-dryRun
// OR term (`c.DryRun || canonical.Settings.DryRun`). With [settings].dry_run =
// true and NO --dry-run flag, the feed importer must make no real writes. If a
// regression dropped the `|| canonical.Settings.DryRun` term, dryRun would be
// false, the api would be unwrapped, and the insert would reach gws.
func TestRunCmd_FeedPhaseSettingsDryRunSuppressesInsert(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	srv := startICSServer(t, nil)
	path := writeConfigFixture(t, feedFixtureTOML(srv.URL, true))

	stub := &recordingGws{}
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     stub,
	}
	if err := (&RunCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stub.insertsTo("travel@example.com"); got != 0 {
		t.Errorf("inserts to feed target = %d, want 0 ([settings].dry_run must suppress the feed write)", got)
	}
}

// TestRunCmd_PairFilterSkipsFeeds pins the coordinator's gate fix: a
// --pair-filtered run (the shape `pair test` delegates in) must NOT run the
// global feed phase, so no live HTTP hit to the feed provider occurs.
func TestRunCmd_PairFilterSkipsFeeds(t *testing.T) {
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)

	var hit bool
	srv := startICSServer(t, &hit)
	path := writeConfigFixture(t, feedFixtureTOML(srv.URL, false))

	stub := &recordingGws{}
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     stub,
	}
	if err := (&RunCmd{Pair: []string{"work-personal"}}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if hit {
		t.Errorf("feed HTTP endpoint was hit during a --pair-filtered run; the feed phase must be gated on len(c.Pair)==0")
	}
	if got := stub.insertsTo("travel@example.com"); got != 0 {
		t.Errorf("inserts to feed target = %d during --pair run, want 0", got)
	}
}
