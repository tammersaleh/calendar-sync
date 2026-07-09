// Package ical parses iCalendar (RFC 5545) streams into a normalized internal
// model. It is pure: no network and no I/O beyond the io.Reader handed to
// Parse. It wraps github.com/arran4/golang-ical, adding the VALUE=DATE
// all-day distinction that the library's GetStartAt/GetEndAt helpers discard.
package ical

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
)

// Item is one normalized VEVENT.
type Item struct {
	UID         string    // ComponentPropertyUniqueId; REQUIRED, error if missing/empty
	Summary     string    // SUMMARY; "" if absent
	Description string    // DESCRIPTION, unfolded and unescaped; "" if absent
	Location    string    // LOCATION; "" if absent
	Start       DateTime  // DTSTART
	End         DateTime  // DTEND; zero DateTime if absent
	Status      string    // upper-cased VEVENT STATUS; "" if absent
	Sequence    int       // SEQUENCE; 0 if absent or unparseable
	Stamp       time.Time // DTSTAMP as UTC; zero time.Time if absent
}

// DateTime is a normalized DTSTART/DTEND.
type DateTime struct {
	AllDay bool      // true when the property carried VALUE=DATE
	Time   time.Time // AllDay: UTC midnight of the date; timed: the instant
	TZID   string    // timed only: the TZID param; "" means UTC. Unused when AllDay.
}

// layouts for the raw RFC 5545 value forms this package handles.
const (
	layoutDate     = "20060102"       // VALUE=DATE
	layoutDateTime = "20060102T150405" // floating / TZID-qualified
	layoutUTC      = "20060102T150405Z"
)

// Parse reads an iCalendar stream and returns its VEVENTs as normalized Items.
// It errors if the stream is not a parseable VCALENDAR, if the stream is
// empty/whitespace-only, or if any VEVENT is missing a UID or carries an
// unresolvable TZID. Non-VEVENT components (VTIMEZONE, VALARM, ...) are ignored.
func Parse(r io.Reader) ([]Item, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("ical: read stream: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, errors.New("ical: empty stream")
	}

	cal, err := ics.ParseCalendar(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("ical: parse calendar: %w", err)
	}

	events := cal.Events()
	items := make([]Item, 0, len(events))
	for _, ev := range events {
		item, err := normalizeEvent(ev)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func normalizeEvent(ev *ics.VEvent) (Item, error) {
	uid := propValue(ev, ics.ComponentPropertyUniqueId)
	if uid == "" {
		return Item{}, errors.New("ical: VEVENT missing UID")
	}

	item := Item{
		UID:         uid,
		Summary:     propValue(ev, ics.ComponentPropertySummary),
		Description: propValue(ev, ics.ComponentPropertyDescription),
		Location:    propValue(ev, ics.ComponentPropertyLocation),
		Status:      strings.ToUpper(propValue(ev, ics.ComponentPropertyStatus)),
	}

	if seq := propValue(ev, ics.ComponentPropertySequence); seq != "" {
		// Per spec, an unparseable SEQUENCE is treated as 0.
		if n, err := strconv.Atoi(strings.TrimSpace(seq)); err == nil {
			item.Sequence = n
		}
	}

	if stamp := getProp(ev, ics.ComponentPropertyDtstamp); stamp != nil {
		if t, err := time.Parse(layoutUTC, strings.TrimSpace(stamp.Value)); err == nil {
			item.Stamp = t.UTC()
		}
	}

	start, err := parseDateTime(getProp(ev, ics.ComponentPropertyDtStart))
	if err != nil {
		return Item{}, fmt.Errorf("ical: UID %s DTSTART: %w", uid, err)
	}
	if start != nil {
		item.Start = *start
	}

	end, err := parseDateTime(getProp(ev, ics.ComponentPropertyDtEnd))
	if err != nil {
		return Item{}, fmt.Errorf("ical: UID %s DTEND: %w", uid, err)
	}
	if end != nil {
		item.End = *end
	}

	return item, nil
}

// parseDateTime normalizes a DTSTART/DTEND property. It returns nil (no error)
// when the property is absent so the caller leaves a zero DateTime.
func parseDateTime(prop *ics.IANAProperty) (*DateTime, error) {
	if prop == nil {
		return nil, nil
	}
	value := strings.TrimSpace(prop.Value)

	if paramHasValue(prop, "VALUE", "DATE") {
		t, err := time.ParseInLocation(layoutDate, value, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("parse all-day date %q: %w", value, err)
		}
		return &DateTime{AllDay: true, Time: t}, nil
	}

	// Trailing Z: an explicit UTC instant.
	if strings.HasSuffix(value, "Z") {
		t, err := time.Parse(layoutUTC, value)
		if err != nil {
			return nil, fmt.Errorf("parse UTC datetime %q: %w", value, err)
		}
		return &DateTime{Time: t.UTC()}, nil
	}

	// TZID param: local wall time in the named zone.
	if tzid := firstParam(prop, "TZID"); tzid != "" {
		loc, err := time.LoadLocation(tzid)
		if err != nil {
			return nil, fmt.Errorf("load TZID %q: %w", tzid, err)
		}
		t, err := time.ParseInLocation(layoutDateTime, value, loc)
		if err != nil {
			return nil, fmt.Errorf("parse datetime %q in %q: %w", value, tzid, err)
		}
		return &DateTime{Time: t, TZID: tzid}, nil
	}

	// Floating local time (no Z, no TZID): parse as UTC, record empty TZID. v1.
	t, err := time.ParseInLocation(layoutDateTime, value, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("parse floating datetime %q: %w", value, err)
	}
	return &DateTime{Time: t}, nil
}

// propValue returns the (already unfolded/unescaped) value of a property, or ""
// when it is absent.
func propValue(ev *ics.VEvent, name ics.ComponentProperty) string {
	if p := getProp(ev, name); p != nil {
		return p.Value
	}
	return ""
}

// getProp is a case-insensitive replacement for the library's GetProperty.
// RFC 5545 §3.1 makes property names case-insensitive, but the library matches
// IANAToken with a case-sensitive ==, so a feed emitting `dtstart:` instead of
// `DTSTART:` would slip past GetProperty and leave a zero DateTime - a silent
// corruption rather than an error. Matching on EqualFold closes that path.
func getProp(ev *ics.VEvent, name ics.ComponentProperty) *ics.IANAProperty {
	for i := range ev.Properties {
		if strings.EqualFold(ev.Properties[i].IANAToken, string(name)) {
			return &ev.Properties[i]
		}
	}
	return nil
}

// firstParam returns the first value of an iCalendar parameter, matching the
// key case-insensitively; "" when absent.
func firstParam(prop *ics.IANAProperty, key string) string {
	for k, vals := range prop.ICalParameters {
		if strings.EqualFold(k, key) && len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

// paramHasValue reports whether the named parameter carries the given value
// (both key and value compared case-insensitively).
func paramHasValue(prop *ics.IANAProperty, key, want string) bool {
	for k, vals := range prop.ICalParameters {
		if !strings.EqualFold(k, key) {
			continue
		}
		for _, v := range vals {
			if strings.EqualFold(strings.TrimSpace(v), want) {
				return true
			}
		}
	}
	return false
}
