package gws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// CalendarListEntry is the subset of the Calendar API CalendarListEntry
// resource that calendar-sync uses. The accessRole field drives config
// validation (SPEC.md "Validation rules") and the per-pdir source_writable
// flag (SPEC.md "Drift detection model").
//
// SummaryOverride is the user-applied display name that takes precedence
// over Summary in Google Calendar's UI; F1 summary lookups match against
// the override-aware effective name. DataOwner is the owner email Google
// reports for secondary calendars; F1 prefers it over the brittle ID-
// substring heuristic when disambiguating by account. Primary marks the
// authenticated user's primary calendar.
type CalendarListEntry struct {
	ID              string `json:"id"`
	Summary         string `json:"summary,omitempty"`
	SummaryOverride string `json:"summaryOverride,omitempty"`
	AccessRole      string `json:"accessRole"`
	DataOwner       string `json:"dataOwner,omitempty"`
	Primary         bool   `json:"primary,omitempty"`
}

// calendarListPage is one page of calendarList.list output. gws --page-all
// emits one of these per line of NDJSON.
type calendarListPage struct {
	Items         []CalendarListEntry `json:"items"`
	NextPageToken string              `json:"nextPageToken,omitempty"`
}

// CalendarListList enumerates every entry on the authenticated user's
// calendar list. SPEC.md does not require this for the sync loop (the
// loop resolves IDs one at a time via CalendarListGet), but it is the
// natural way to look up a calendar by display summary - the entry point
// for F1 (sync non-primary calendars) and a prerequisite for E2E test
// fixture management (find / create / delete by name).
//
// Returns the full merged item list across pages; callers filter by
// summary, accessRole, etc. client-side.
func (c *Client) CalendarListList(ctx context.Context) ([]CalendarListEntry, error) {
	c.debug("gws.CalendarListList")
	args := []string{
		"calendar", "calendarList", "list",
		"--page-all",
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, classifyError(stdout, stderr, exit, "calendarList.list")
	}

	pages, err := parseCalendarListPages(stdout)
	if err != nil {
		return nil, fmt.Errorf("gws calendarList.list: %w", err)
	}

	var out []CalendarListEntry
	for _, p := range pages {
		out = append(out, p.Items...)
	}
	c.debug("gws.CalendarListList result", "items", len(out))
	return out, nil
}

// parseCalendarListPages decodes gws --page-all NDJSON output into one
// page per line. Mirrors parseNDJSONPages in events.go but with the
// calendarList page shape. A non-empty stdout that yields no pages is an
// error - silent truncation would mask gws/format-flag bugs.
func parseCalendarListPages(stdout []byte) ([]calendarListPage, error) {
	var pages []calendarListPage
	dec := json.NewDecoder(bytes.NewReader(stdout))
	for dec.More() {
		var p calendarListPage
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("parse calendarList page: %w", err)
		}
		pages = append(pages, p)
	}
	if len(pages) == 0 && len(bytes.TrimSpace(stdout)) > 0 {
		return nil, fmt.Errorf("no pages parsed but stdout was non-empty: %q", string(stdout))
	}
	return pages, nil
}

// CalendarListGet returns the user's CalendarListEntry for calendarID.
// SPEC.md uses this at config-load time to resolve aliases like "primary"
// to a canonical ID and to capture each referenced calendar's accessRole.
//
// calendarID may be the literal "primary", an email address, or a group
// calendar ID; gws/Calendar API resolves all three.
func (c *Client) CalendarListGet(ctx context.Context, calendarID string) (*CalendarListEntry, error) {
	c.debug("gws.CalendarListGet", "calendar_id", calendarID)
	paramsJSON, err := json.Marshal(map[string]any{"calendarId": calendarID})
	if err != nil {
		return nil, fmt.Errorf("gws calendarList.get: marshal params: %w", err)
	}

	args := []string{
		"calendar", "calendarList", "get",
		"--params", string(paramsJSON),
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, classifyError(stdout, stderr, exit, "calendarList.get")
	}

	var entry CalendarListEntry
	if err := json.Unmarshal(stdout, &entry); err != nil {
		return nil, fmt.Errorf("gws calendarList.get: parse response: %w (stdout: %q)", err, string(stdout))
	}
	return &entry, nil
}
