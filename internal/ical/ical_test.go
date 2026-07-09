package ical

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readFixture returns the bytes of a testdata .ics file.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// byUID indexes parsed items by their UID for assertion convenience.
func byUID(items []Item) map[string]Item {
	m := make(map[string]Item, len(items))
	for _, it := range items {
		m[it.UID] = it
	}
	return m
}

// TestParse_TripItFeed exercises the full synthetic TripIt-shaped feed against
// both CRLF and LF line endings. Both must parse identically.
func TestParse_TripItFeed(t *testing.T) {
	nyc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	wantStamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for _, fixture := range []string{"tripit_crlf.ics", "tripit_lf.ics"} {
		t.Run(fixture, func(t *testing.T) {
			items, err := Parse(bytes.NewReader(readFixture(t, fixture)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(items) != 4 {
				t.Fatalf("got %d items, want 4 (VTIMEZONE/VALARM must be ignored)", len(items))
			}
			m := byUID(items)

			// All-day multi-day span: exclusive DTEND preserved verbatim.
			span, ok := m["trip-span-001@example.com"]
			if !ok {
				t.Fatal("missing trip-span-001")
			}
			if !span.Start.AllDay || !span.End.AllDay {
				t.Errorf("span: AllDay start=%v end=%v, want both true", span.Start.AllDay, span.End.AllDay)
			}
			if want := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC); !span.Start.Time.Equal(want) {
				t.Errorf("span start = %v, want %v", span.Start.Time, want)
			}
			if want := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC); !span.End.Time.Equal(want) {
				t.Errorf("span end = %v, want %v (exclusive, not decremented)", span.End.Time, want)
			}
			if span.Summary != "Fake City Trip" {
				t.Errorf("span summary = %q", span.Summary)
			}
			if span.Status != "CONFIRMED" {
				t.Errorf("span status = %q, want CONFIRMED", span.Status)
			}
			// TRANSP:TRANSPARENT captured, upper-cased.
			if span.Transparency != "TRANSPARENT" {
				t.Errorf("span transparency = %q, want TRANSPARENT", span.Transparency)
			}
			if span.Sequence != 0 {
				t.Errorf("span sequence = %d, want 0", span.Sequence)
			}
			if !span.Stamp.Equal(wantStamp) {
				t.Errorf("span stamp = %v, want %v", span.Stamp, wantStamp)
			}

			// UTC timed flight with a folded, escaped DESCRIPTION.
			flight, ok := m["flight-out-002@example.com"]
			if !ok {
				t.Fatal("missing flight-out-002")
			}
			if flight.Start.AllDay {
				t.Error("flight start should not be AllDay")
			}
			if want := time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC); !flight.Start.Time.Equal(want) {
				t.Errorf("flight start = %v, want %v", flight.Start.Time, want)
			}
			if flight.Start.TZID != "" {
				t.Errorf("flight start TZID = %q, want \"\" (trailing Z)", flight.Start.TZID)
			}
			if want := time.Date(2026, 7, 13, 14, 44, 0, 0, time.UTC); !flight.End.Time.Equal(want) {
				t.Errorf("flight end = %v, want %v", flight.End.Time, want)
			}
			if flight.Location != "Fake Intl Airport (FKA)" {
				t.Errorf("flight location = %q", flight.Location)
			}
			if flight.Sequence != 2 {
				t.Errorf("flight sequence = %d, want 2", flight.Sequence)
			}
			// STATUS lower-cased in the feed must be upper-cased.
			if flight.Status != "CONFIRMED" {
				t.Errorf("flight status = %q, want CONFIRMED (upper-cased)", flight.Status)
			}
			// TRANSP absent -> "".
			if flight.Transparency != "" {
				t.Errorf("flight transparency = %q, want \"\" (TRANSP absent)", flight.Transparency)
			}
			// Folding unwrapped (leading space of continuation removed) and
			// TEXT escapes decoded to real runes.
			wantDesc := "Confirmation: ABC123\nSeat 12A, Economy; Gate B4\nBooked via Synthetic Travel"
			if flight.Description != wantDesc {
				t.Errorf("flight description mismatch\n got: %q\nwant: %q", flight.Description, wantDesc)
			}

			// TZID timed event: instant in the named zone, TZID recorded.
			hotel, ok := m["hotel-checkin-003@example.com"]
			if !ok {
				t.Fatal("missing hotel-checkin-003")
			}
			if hotel.Start.AllDay {
				t.Error("hotel start should not be AllDay")
			}
			if hotel.Start.TZID != "America/New_York" {
				t.Errorf("hotel start TZID = %q, want America/New_York", hotel.Start.TZID)
			}
			if want := time.Date(2026, 7, 13, 15, 0, 0, 0, nyc); !hotel.Start.Time.Equal(want) {
				t.Errorf("hotel start = %v, want %v", hotel.Start.Time, want)
			}
			if hotel.End.TZID != "America/New_York" {
				t.Errorf("hotel end TZID = %q, want America/New_York", hotel.End.TZID)
			}
			if want := time.Date(2026, 7, 13, 16, 0, 0, 0, nyc); !hotel.End.Time.Equal(want) {
				t.Errorf("hotel end = %v, want %v", hotel.End.Time, want)
			}
			if hotel.Status != "TENTATIVE" {
				t.Errorf("hotel status = %q, want TENTATIVE", hotel.Status)
			}
			// SEQUENCE absent -> 0.
			if hotel.Sequence != 0 {
				t.Errorf("hotel sequence = %d, want 0 (absent)", hotel.Sequence)
			}

			// Cancelled event.
			cancelled, ok := m["cancelled-flight-004@example.com"]
			if !ok {
				t.Fatal("missing cancelled-flight-004")
			}
			if cancelled.Status != "CANCELLED" {
				t.Errorf("cancelled status = %q, want CANCELLED", cancelled.Status)
			}
		})
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{
			name: "missing UID is a hard error",
			in: "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//T//EN\r\n" +
				"BEGIN:VEVENT\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260713T090000Z\r\nSUMMARY:No UID\r\nEND:VEVENT\r\n" +
				"END:VCALENDAR\r\n",
		},
		{
			name: "empty UID is a hard error",
			in: "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//T//EN\r\n" +
				"BEGIN:VEVENT\r\nUID:\r\nDTSTART:20260713T090000Z\r\nEND:VEVENT\r\n" +
				"END:VCALENDAR\r\n",
		},
		{
			name: "garbage input",
			in:   "this is not an iCalendar stream at all",
		},
		{
			name: "empty input",
			in:   "",
		},
		{
			name: "whitespace-only input",
			in:   "   \r\n  \n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.in))
			if err == nil {
				t.Fatalf("Parse(%q): want error, got nil", tt.name)
			}
		})
	}
}

// TestParse_WellFormedEmptyFeed: a valid VCALENDAR with zero VEVENTs is not an
// error; it yields an empty slice.
func TestParse_WellFormedEmptyFeed(t *testing.T) {
	in := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//T//EN\r\nEND:VCALENDAR\r\n"
	items, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("got %d items, want 0", len(items))
	}
}

// TestParse_FloatingLocalTime documents the v1 behavior: a timed value with no
// trailing Z and no TZID param is parsed as UTC with an empty TZID.
func TestParse_FloatingLocalTime(t *testing.T) {
	in := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//T//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:floating@example.com\r\nDTSTART:20260713T090000\r\nDTEND:20260713T100000\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	items, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	got := items[0]
	if got.Start.AllDay {
		t.Error("floating start should not be AllDay")
	}
	if got.Start.TZID != "" {
		t.Errorf("floating start TZID = %q, want \"\"", got.Start.TZID)
	}
	if want := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC); !got.Start.Time.Equal(want) {
		t.Errorf("floating start = %v, want %v (parsed as UTC)", got.Start.Time, want)
	}
}

// TestParse_UnknownTZIDErrors: a TZID that time.LoadLocation cannot resolve is a
// hard error rather than a silent UTC fallback.
func TestParse_UnknownTZIDErrors(t *testing.T) {
	in := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//T//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:badtz@example.com\r\nDTSTART;TZID=Mars/Olympus:20260713T090000\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	if _, err := Parse(strings.NewReader(in)); err == nil {
		t.Fatal("want error for unresolvable TZID, got nil")
	}
}

// TestParse_MinimalTimedEvent: DTSTAMP/STATUS/DTEND all absent -> zero-values,
// and a bare UID+DTSTART event parses without error.
func TestParse_MinimalTimedEvent(t *testing.T) {
	in := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//T//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:min@example.com\r\nDTSTART:20260713T090000Z\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	items, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	got := items[0]
	if got.Status != "" {
		t.Errorf("status = %q, want empty", got.Status)
	}
	if !got.Stamp.IsZero() {
		t.Errorf("stamp = %v, want zero", got.Stamp)
	}
	if got.Sequence != 0 {
		t.Errorf("sequence = %d, want 0", got.Sequence)
	}
	if !got.End.Time.IsZero() || got.End.AllDay {
		t.Errorf("end = %+v, want zero DateTime", got.End)
	}
}

// TestParse_CaseInsensitiveParams pins RFC 5545 §3.1 case-insensitivity across
// three axes that real-world producers vary and the library gets wrong:
//   - property NAMES (`dtstart` vs `DTSTART`) - the library's GetProperty uses a
//     case-sensitive ==, so getProp's EqualFold is what makes this work; a
//     regression would silently drop DTSTART/DTEND to a zero date.
//   - parameter KEYS (`tzid=`, `value=`) - matched via firstParam/paramHasValue.
//   - enumerated parameter VALUES (`date` vs `DATE`) - matched via paramHasValue.
//
// TZID *values* (IANA zone names) are intentionally kept canonical: the tz
// database is case-sensitive and LoadLocation's behavior on a lowercased name is
// platform-dependent, so that axis is out of scope.
func TestParse_CaseInsensitiveParams(t *testing.T) {
	in := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//T//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:lower-allday@example.com\r\ndtstart;value=date:20260713\r\ndtend;value=date:20260718\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:lower-tzid@example.com\r\ndtstart;tzid=America/New_York:20260713T090000\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	items, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	// Lowercase property name + lowercase param key + lowercase enum value: still
	// detected as all-day with the exclusive DTEND preserved verbatim.
	allDay := items[0]
	if !allDay.Start.AllDay {
		t.Error("lowercase value=date start should be AllDay")
	}
	if want := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC); !allDay.End.Time.Equal(want) {
		t.Errorf("lowercase value=date end = %v, want %v (exclusive, verbatim)", allDay.End.Time, want)
	}

	// Lowercase property name + lowercase tzid= key: resolves the named zone.
	tz := items[1]
	if tz.Start.AllDay {
		t.Error("lowercase tzid start should not be AllDay")
	}
	if tz.Start.TZID != "America/New_York" {
		t.Errorf("lowercase tzid TZID = %q, want %q", tz.Start.TZID, "America/New_York")
	}
	loc, _ := time.LoadLocation("America/New_York")
	if want := time.Date(2026, 7, 13, 9, 0, 0, 0, loc); !tz.Start.Time.Equal(want) {
		t.Errorf("lowercase tzid start = %v, want %v", tz.Start.Time, want)
	}
}
