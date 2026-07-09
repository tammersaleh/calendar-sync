package config_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// feedSecret is the fake bearer token embedded in test URLs. Every error /
// redaction assertion checks this substring is ABSENT: a feed URL is a
// secret and must never reach a log line, error message, or config-show
// redaction.
const feedSecret = "SUPERSECRETTOKEN"

func feedURL() string {
	return "https://www.tripit.com/feed/ical/private/" + feedSecret + "/tripit.ics"
}

// --- Parse -----------------------------------------------------------------

func TestLoad_FeedWithURLAndStringTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[[feeds]]
name = "tripit"
url = "` + feedURL() + `"
target = "personal@example.com"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Feeds) != 1 {
		t.Fatalf("len(Feeds) = %d, want 1", len(cfg.Feeds))
	}
	f := cfg.Feeds[0]
	if f.Name != "tripit" {
		t.Errorf("Name = %q, want tripit", f.Name)
	}
	if f.URL != feedURL() {
		t.Errorf("URL = %q, want %q", f.URL, feedURL())
	}
	if f.Target.ID != "personal@example.com" {
		t.Errorf("Target.ID = %q, want personal@example.com", f.Target.ID)
	}
	if !f.IsEnabled() {
		t.Errorf("feed without explicit enabled should default to true")
	}
}

func TestLoad_FeedWithTableTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[[feeds]]
name = "tripit"
url_env = "TRIPIT_URL"
target = {summary = "Travel"}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Feeds) != 1 {
		t.Fatalf("len(Feeds) = %d, want 1", len(cfg.Feeds))
	}
	f := cfg.Feeds[0]
	if f.URLEnv != "TRIPIT_URL" {
		t.Errorf("URLEnv = %q, want TRIPIT_URL", f.URLEnv)
	}
	if !f.Target.IsSummaryRef() || f.Target.Summary != "Travel" {
		t.Errorf("Target = %+v, want summary=Travel", f.Target)
	}
}

// --- Validate --------------------------------------------------------------

// feedConfig returns a valid base config with a single enabled feed applied
// by mutate. Settings + Pairs come from baseConfig (canonicalize_test.go).
func feedConfig(mutate func(*config.Feed)) config.Config {
	c := baseConfig()
	f := config.Feed{
		Name:   "tripit",
		URL:    feedURL(),
		Target: config.CalendarRef{ID: "personal@example.com"},
	}
	mutate(&f)
	c.Feeds = []config.Feed{f}
	return c
}

func TestValidate_FeedMissingName(t *testing.T) {
	c := feedConfig(func(f *config.Feed) { f.Name = "" })
	if !errors.Is(c.Validate(), config.ErrInvalid) {
		t.Errorf("expected ErrInvalid for feed with empty name")
	}
}

func TestValidate_FeedDuplicateNames(t *testing.T) {
	c := baseConfig()
	c.Feeds = []config.Feed{
		{Name: "dup", URL: feedURL(), Target: config.CalendarRef{ID: "a@example.com"}},
		{Name: "dup", URL: feedURL(), Target: config.CalendarRef{ID: "b@example.com"}},
	}
	if !errors.Is(c.Validate(), config.ErrInvalid) {
		t.Errorf("expected ErrInvalid for duplicate feed names")
	}
}

func TestValidate_FeedBothURLAndEnv(t *testing.T) {
	c := feedConfig(func(f *config.Feed) { f.URLEnv = "TRIPIT_URL" })
	err := c.Validate()
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v; want ErrInvalid when both url and url_env set", err)
	}
	if strings.Contains(err.Error(), feedSecret) {
		t.Errorf("error message leaked the secret URL: %v", err)
	}
}

func TestValidate_FeedNeitherURLNorEnv(t *testing.T) {
	c := feedConfig(func(f *config.Feed) { f.URL = "" })
	if !errors.Is(c.Validate(), config.ErrInvalid) {
		t.Errorf("expected ErrInvalid when neither url nor url_env set")
	}
}

func TestValidate_FeedURLEnvUnset(t *testing.T) {
	const varName = "CALENDAR_SYNC_TEST_FEED_URL_UNSET"
	t.Setenv(varName, "") // set to empty => treated as unset/empty
	c := feedConfig(func(f *config.Feed) {
		f.URL = ""
		f.URLEnv = varName
	})
	err := c.Validate()
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v; want ErrInvalid for empty url_env var", err)
	}
	if !strings.Contains(err.Error(), varName) {
		t.Errorf("error should name the env var %q; got %v", varName, err)
	}
	if strings.Contains(err.Error(), feedSecret) {
		t.Errorf("error message leaked a secret: %v", err)
	}
}

func TestValidate_DisabledFeedSkipsValidation(t *testing.T) {
	// A disabled feed with an otherwise-invalid config (both url and url_env,
	// empty name) must NOT fail validation - mirrors disabled pairs.
	c := baseConfig()
	c.Feeds = []config.Feed{{
		Name:    "",
		URL:     feedURL(),
		URLEnv:  "TRIPIT_URL",
		Target:  config.CalendarRef{},
		Enabled: enabled(false),
	}}
	if err := c.Validate(); err != nil {
		t.Errorf("disabled feed with invalid config should be skipped; got %v", err)
	}
}

// --- Canonicalize ----------------------------------------------------------

func TestCanonicalize_FeedWritableStringTarget(t *testing.T) {
	c := feedConfig(func(*config.Feed) {})
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"personal@example.com": {ID: "personal.canonical@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if len(can.Feeds) != 1 {
		t.Fatalf("len(Feeds) = %d, want 1", len(can.Feeds))
	}
	cf := can.Feeds[0]
	if cf.Name != "tripit" {
		t.Errorf("Name = %q, want tripit", cf.Name)
	}
	if cf.URL != feedURL() {
		t.Errorf("URL = %q, want resolved secret", cf.URL)
	}
	if cf.TargetCalendar != "personal.canonical@example.com" {
		t.Errorf("TargetCalendar = %q, want canonical id", cf.TargetCalendar)
	}
}

func TestCanonicalize_FeedURLEnvResolves(t *testing.T) {
	const varName = "CALENDAR_SYNC_TEST_FEED_URL"
	t.Setenv(varName, feedURL())
	c := feedConfig(func(f *config.Feed) {
		f.URL = ""
		f.URLEnv = varName
	})
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"personal@example.com": {ID: "personal@example.com", AccessRole: "owner"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if got := can.Feeds[0].URL; got != feedURL() {
		t.Errorf("URL = %q, want resolved from env", got)
	}
}

func TestCanonicalize_FeedSummaryTargetResolves(t *testing.T) {
	c := feedConfig(func(f *config.Feed) {
		f.Target = config.CalendarRef{Summary: "Travel"}
	})
	lister := &stubLister{
		listResponses: []gws.CalendarListEntry{
			{ID: "travel-abc@group.calendar.google.com", Summary: "Travel", AccessRole: "writer"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if got, want := can.Feeds[0].TargetCalendar, "travel-abc@group.calendar.google.com"; got != want {
		t.Errorf("TargetCalendar = %q, want %q", got, want)
	}
	if lister.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1 (summary feed target uses one CalendarListList)", lister.listCalls)
	}
}

func TestCanonicalize_FeedNonWritableTargetRejected(t *testing.T) {
	c := feedConfig(func(*config.Feed) {})
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"personal@example.com": {ID: "personal@example.com", AccessRole: "reader"},
		},
	}
	_, err := c.Canonicalize(context.Background(), lister)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v; want ErrInvalid for reader-only feed target", err)
	}
	if !strings.Contains(err.Error(), "writer") {
		t.Errorf("err %q should mention the writer requirement", err.Error())
	}
}

func TestCanonicalize_DisabledFeedAbsentFromCanonical(t *testing.T) {
	c := baseConfig()
	c.Feeds = []config.Feed{{
		Name:    "off",
		URL:     feedURL(),
		Target:  config.CalendarRef{ID: "personal@example.com"},
		Enabled: enabled(false),
	}}
	lister := &stubLister{
		// Sentinel: disabled feed must not trigger any calendar lookup.
		err: errors.New("CalendarListGet must not be called for disabled feed"),
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if len(can.Feeds) != 0 {
		t.Errorf("len(Feeds) = %d, want 0 (disabled feed skipped)", len(can.Feeds))
	}
	if len(lister.calls) != 0 {
		t.Errorf("expected 0 lister calls for disabled feed; got %v", lister.calls)
	}
}

// TestCanonicalize_FeedAndPairShareOneListCall: an enabled feed and an
// enabled pair each carry a summary-form ref sharing the same
// CalendarListList result; canonicalize must make exactly one list call.
func TestCanonicalize_FeedAndPairShareOneListCall(t *testing.T) {
	c := baseConfig()
	c.Pairs = []config.Pair{{
		Name:   "p",
		Source: config.CalendarRef{Summary: "Holidays"},
		Target: config.CalendarRef{ID: "primary"},
	}}
	c.Feeds = []config.Feed{{
		Name:   "tripit",
		URL:    feedURL(),
		Target: config.CalendarRef{Summary: "Travel"},
	}}
	lister := &stubLister{
		responses: map[string]*gws.CalendarListEntry{
			"primary": {ID: "alice@example.com", AccessRole: "owner"},
		},
		listResponses: []gws.CalendarListEntry{
			{ID: "holidays-1@group.calendar.google.com", Summary: "Holidays", AccessRole: "reader"},
			{ID: "travel-1@group.calendar.google.com", Summary: "Travel", AccessRole: "writer"},
		},
	}
	can, err := c.Canonicalize(context.Background(), lister)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if lister.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1 (feed + pair share one CalendarListList call)", lister.listCalls)
	}
	if len(can.Feeds) != 1 || len(can.PDirs) != 1 {
		t.Errorf("Feeds=%d PDirs=%d, want 1 and 1", len(can.Feeds), len(can.PDirs))
	}
}

// --- RedactedURL -----------------------------------------------------------

func TestRedactedURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"full secret url", feedURL(), "https://www.tripit.com/<redacted>"},
		{"parse failure", "http://ho\x7fst/path", "<redacted>"},
		{"empty host", "mailto:foo@example.com", "<redacted>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cf := config.CanonicalFeed{URL: tc.url}
			got := cf.RedactedURL()
			if got != tc.want {
				t.Errorf("RedactedURL() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, feedSecret) {
				t.Errorf("RedactedURL leaked the secret: %q", got)
			}
		})
	}
}

// TestCanonicalFeed_MarshalJSONRedacts pins the fail-safe: json.Marshal of a
// CanonicalFeed (however it's reached - a future config-show wiring, an
// embedding struct, a log line) must emit the redacted URL, never the secret.
func TestCanonicalFeed_MarshalJSONRedacts(t *testing.T) {
	cf := config.CanonicalFeed{Name: "tripit", URL: feedURL(), TargetCalendar: "cal@x"}

	b, err := json.Marshal(cf)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := string(b)
	if strings.Contains(out, feedSecret) {
		t.Fatalf("MarshalJSON leaked the secret: %s", out)
	}
	// The redacted host is present and the token path is gone. (encoding/json
	// HTML-escapes the "<redacted>" marker to <redacted>, so match on
	// the host + "redacted" rather than the literal angle brackets.)
	if !strings.Contains(out, "www.tripit.com") || !strings.Contains(out, "redacted") {
		t.Errorf("MarshalJSON should emit the redacted URL, got: %s", out)
	}

	// Also cover marshaling by value inside a parent struct (the naive
	// "just reuse CanonicalFeed" mistake the fix guards against).
	parent := struct {
		Feeds []config.CanonicalFeed `json:"feeds"`
	}{Feeds: []config.CanonicalFeed{cf}}
	pb, err := json.Marshal(parent)
	if err != nil {
		t.Fatalf("Marshal parent: %v", err)
	}
	if strings.Contains(string(pb), feedSecret) {
		t.Fatalf("MarshalJSON leaked the secret when embedded: %s", pb)
	}
}
