package feedimport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/feed"
)

// secretURL is a stand-in bearer-secret feed URL. Every test that exercises a
// log path asserts this substring never appears in the recorded output.
const secretURL = "https://feeds.example.com/private/SUPER-SECRET-TOKEN/cal.ics"

// fakeFetcher is an in-process fetcher stub. It records the URL it was handed
// and returns a canned Result / error.
type fakeFetcher struct {
	result feed.Result
	err    error
	calls  int
	gotURL string
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (feed.Result, error) {
	f.calls++
	f.gotURL = url
	return f.result, f.err
}

// recordingLogger captures every log line (level + message + flattened args)
// so tests can assert on content and, critically, on the ABSENCE of the secret.
type recordingLogger struct {
	lines []string
}

func (l *recordingLogger) record(level, msg string, args ...any) {
	parts := []string{level, msg}
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	l.lines = append(l.lines, strings.Join(parts, " "))
}

func (l *recordingLogger) Debug(msg string, args ...any) { l.record("DEBUG", msg, args...) }
func (l *recordingLogger) Info(msg string, args ...any)  { l.record("INFO", msg, args...) }
func (l *recordingLogger) Warn(msg string, args ...any)  { l.record("WARN", msg, args...) }
func (l *recordingLogger) Error(msg string, args ...any) { l.record("ERROR", msg, args...) }

func (l *recordingLogger) all() string { return strings.Join(l.lines, "\n") }

// icsWithEvent returns a minimal valid VCALENDAR carrying one timed VEVENT.
func icsWithEvent(uid, summary string) []byte {
	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//EN",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"SUMMARY:" + summary,
		"DTSTART:20260713T120000Z",
		"DTEND:20260713T130000Z",
		"END:VEVENT",
		"END:VCALENDAR",
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

// entry builds a feedEntry wired to a fake fetcher and a real Importer over the
// given EventsAPI stub. Same-package access lets tests bypass NewRunner (which
// would construct a real, network-bound *feed.Fetcher).
func entry(name, url string, api EventsAPI, ff *fakeFetcher, dryRun bool, log Logger) *feedEntry {
	return &feedEntry{
		name:    name,
		url:     url,
		fetcher: ff,
		importer: &Importer{
			API:    api,
			Target: "tgt@example.com",
			FeedID: name,
			DryRun: dryRun,
			Log:    log,
		},
	}
}

func TestRunner_RunOnce_Skipped(t *testing.T) {
	api := newStub()
	ff := &fakeFetcher{result: feed.Result{Skipped: true}}
	log := &recordingLogger{}
	r := &Runner{entries: []*feedEntry{entry("f1", secretURL, api, ff, false, log)}, log: log}

	got := r.RunOnce(context.Background())
	if len(got) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(got))
	}
	if !got[0].Skipped || got[0].Changed || got[0].Err != nil {
		t.Errorf("result = %+v, want Skipped only", got[0])
	}
	// Skipped => no Reconcile => no list/write calls at all.
	if len(api.listParams) != 0 || len(api.inserts) != 0 || len(api.patches) != 0 || len(api.deletes) != 0 {
		t.Errorf("skipped feed made API calls: lists=%d inserts=%d patches=%d deletes=%d",
			len(api.listParams), len(api.inserts), len(api.patches), len(api.deletes))
	}
	assertNoSecret(t, log)
}

func TestRunner_RunOnce_NotModified(t *testing.T) {
	api := newStub()
	// 304: Changed=false, Skipped=false.
	ff := &fakeFetcher{result: feed.Result{}}
	log := &recordingLogger{}
	r := &Runner{entries: []*feedEntry{entry("f1", secretURL, api, ff, false, log)}, log: log}

	got := r.RunOnce(context.Background())
	if got[0].Changed || got[0].Skipped || got[0].Err != nil {
		t.Errorf("result = %+v, want no-op (304)", got[0])
	}
	if len(api.listParams) != 0 {
		t.Errorf("304 feed triggered a Reconcile (%d list calls)", len(api.listParams))
	}
	assertNoSecret(t, log)
}

func TestRunner_RunOnce_Changed_Reconciles(t *testing.T) {
	api := newStub()
	ff := &fakeFetcher{result: feed.Result{Changed: true, Body: icsWithEvent("evt-1@test", "Flight")}}
	log := &recordingLogger{}
	r := &Runner{entries: []*feedEntry{entry("f1", secretURL, api, ff, false, log)}, log: log}

	got := r.RunOnce(context.Background())
	if !got[0].Changed || got[0].Err != nil {
		t.Fatalf("result = %+v, want Changed with no error", got[0])
	}
	if got[0].Import.Inserted != 1 {
		t.Errorf("Import.Inserted = %d, want 1", got[0].Import.Inserted)
	}
	if len(api.inserts) != 1 {
		t.Errorf("stub recorded %d inserts, want 1 (Reconcile should have inserted)", len(api.inserts))
	}
	if ff.gotURL != secretURL {
		t.Errorf("fetcher got URL %q, want the feed's secret URL", ff.gotURL)
	}
	assertNoSecret(t, log)
}

func TestRunner_Isolation_FetchError(t *testing.T) {
	api1 := newStub()
	api2 := newStub()
	// A redacted fetch error, as the real feed layer would produce.
	ff1 := &fakeFetcher{err: errors.New("feed: request to host feeds.example.com failed")}
	ff2 := &fakeFetcher{result: feed.Result{Changed: true, Body: icsWithEvent("evt-2@test", "Hotel")}}
	log := &recordingLogger{}
	r := &Runner{
		entries: []*feedEntry{
			entry("f1", secretURL, api1, ff1, false, log),
			entry("f2", "https://other.example.com/x/ANOTHER-SECRET/y.ics", api2, ff2, false, log),
		},
		log: log,
	}

	got := r.RunOnce(context.Background())
	if got[0].Err == nil {
		t.Errorf("f1 fetch error not surfaced in result")
	}
	// Isolation: f2 still reconciled despite f1's failure.
	if got[1].Err != nil || !got[1].Changed || got[1].Import.Inserted != 1 {
		t.Errorf("f2 result = %+v, want a clean reconcile despite f1's failure", got[1])
	}
	if len(api2.inserts) != 1 {
		t.Errorf("f2 recorded %d inserts, want 1", len(api2.inserts))
	}
	assertNoSecret(t, log)
}

func TestRunner_Isolation_ParseError(t *testing.T) {
	api1 := newStub()
	api2 := newStub()
	ff1 := &fakeFetcher{result: feed.Result{Changed: true, Body: []byte("this is not a calendar")}}
	ff2 := &fakeFetcher{result: feed.Result{Changed: true, Body: icsWithEvent("evt-3@test", "Trip")}}
	log := &recordingLogger{}
	r := &Runner{
		entries: []*feedEntry{
			entry("f1", secretURL, api1, ff1, false, log),
			entry("f2", "https://other.example.com/x/ANOTHER-SECRET/y.ics", api2, ff2, false, log),
		},
		log: log,
	}

	got := r.RunOnce(context.Background())
	if got[0].Err == nil {
		t.Errorf("f1 parse error not surfaced in result")
	}
	if len(api1.inserts) != 0 {
		t.Errorf("f1 parse error should have prevented any write, got %d inserts", len(api1.inserts))
	}
	// Isolation: f2 still reconciled.
	if got[1].Err != nil || got[1].Import.Inserted != 1 {
		t.Errorf("f2 result = %+v, want a clean reconcile despite f1's parse failure", got[1])
	}
	assertNoSecret(t, log)
}

func TestRunner_DryRun_Propagates(t *testing.T) {
	api := newStub()
	ff := &fakeFetcher{result: feed.Result{Changed: true, Body: icsWithEvent("evt-1@test", "Flight")}}
	log := &recordingLogger{}
	r := &Runner{entries: []*feedEntry{entry("f1", secretURL, api, ff, true, log)}, log: log}

	got := r.RunOnce(context.Background())
	// Importer tallies the would-be insert under dry-run...
	if got[0].Import.Inserted != 1 {
		t.Errorf("Import.Inserted = %d, want 1 (dry-run still tallies)", got[0].Import.Inserted)
	}
	// ...but performs no actual write.
	if len(api.inserts) != 0 {
		t.Errorf("dry-run recorded %d real inserts, want 0", len(api.inserts))
	}
	assertNoSecret(t, log)
}

func TestRunner_NoFeeds(t *testing.T) {
	r := &Runner{}
	got := r.RunOnce(context.Background())
	if len(got) != 0 {
		t.Errorf("empty runner returned %d results, want 0", len(got))
	}
}

func TestRunner_NewRunner_WiresEntries(t *testing.T) {
	api := newStub()
	now := func() time.Time { return time.Unix(0, 0) }
	log := &recordingLogger{}
	feeds := []FeedConfig{
		{Name: "trip", URL: secretURL, TargetCalendar: "tgt@example.com"},
	}
	r := NewRunner(api, feeds, true, now, log)

	if len(r.entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(r.entries))
	}
	e := r.entries[0]
	if e.name != "trip" || e.url != secretURL {
		t.Errorf("entry name/url = %q/%q", e.name, e.url)
	}
	if e.importer.FeedID != "trip" || e.importer.Target != "tgt@example.com" {
		t.Errorf("importer FeedID/Target = %q/%q", e.importer.FeedID, e.importer.Target)
	}
	if !e.importer.DryRun {
		t.Errorf("importer DryRun = false, want true (dryRun propagated)")
	}
	if e.importer.API != EventsAPI(api) {
		t.Errorf("importer API not the shared api")
	}
	// The fetcher is a real *feed.Fetcher with the injected clock.
	ff, ok := e.fetcher.(*feed.Fetcher)
	if !ok {
		t.Fatalf("fetcher type = %T, want *feed.Fetcher", e.fetcher)
	}
	if ff.Now == nil {
		t.Errorf("fetcher.Now is nil; now func was not threaded through")
	}
}

// assertNoSecret fails if any recorded log line contains the secret URL or its
// token. This is the calendar-sync bearer-secret invariant for the Runner.
func assertNoSecret(t *testing.T, log *recordingLogger) {
	t.Helper()
	out := log.all()
	for _, secret := range []string{secretURL, "SUPER-SECRET-TOKEN", "ANOTHER-SECRET"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked into logs:\n%s", secret, out)
		}
	}
}
