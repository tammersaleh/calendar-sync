package gws

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// EventsListParams holds the events.list parameters calendar-sync uses.
// Empty/zero omitempty fields are dropped from the marshaled JSON; gws
// passes the rest through to Calendar API.
//
// Per SPEC.md "Sync Algorithm", incremental ticks pass SyncToken (and the
// parameters Calendar API permits alongside it: ShowDeleted, EventTypes,
// MaxResults). Full source-list calls pass TimeMin/TimeMax instead and add
// SingleEvents=false. The wrapper does not validate the combination; the
// API rejects mismatches.
//
// SingleEvents has omitempty: false matches Calendar API's default and the
// SPEC's incremental wire shape (which omits the key entirely). The SPEC's
// startup wire shape shows an explicit false; omitting it produces the same
// API behavior and keeps a single struct usable for both call shapes.
//
// ShowDeleted has no omitempty: callers always want true (delta detection
// depends on seeing cancellations), and false is never the right value, so
// the wrapper sends whatever the caller passed verbatim to surface caller
// bugs immediately rather than silently sending the API default.
type EventsListParams struct {
	CalendarID              string   `json:"calendarId"`
	TimeMin                 string   `json:"timeMin,omitempty"`
	TimeMax                 string   `json:"timeMax,omitempty"`
	SyncToken               string   `json:"syncToken,omitempty"`
	SingleEvents            bool     `json:"singleEvents,omitempty"`
	ShowDeleted             bool     `json:"showDeleted"`
	EventTypes              []string `json:"eventTypes,omitempty"`
	MaxResults              int      `json:"maxResults,omitempty"`
	PrivateExtendedProperty []string `json:"privateExtendedProperty,omitempty"`
}

// eventsPage is one page of events.list/instances output. gws --page-all
// emits one of these per line of NDJSON.
type eventsPage struct {
	Items         []Event `json:"items"`
	NextPageToken string  `json:"nextPageToken,omitempty"`
	NextSyncToken string  `json:"nextSyncToken,omitempty"`
}

// EventsList runs `gws calendar events list --params <p> --page-all` and
// returns the merged event list plus the nextSyncToken from the last page.
// On a successful incremental list the caller stores the returned token to
// drive the next tick; per SPEC.md "Sync Algorithm" / "Daemon lifecycle:
// per-tick reconciliation", token advancement is conditional on every
// dependent pdir succeeding.
func (c *Client) EventsList(ctx context.Context, params EventsListParams) (events []Event, nextSyncToken string, err error) {
	c.debug("gws.EventsList",
		"calendar_id", params.CalendarID,
		"sync_token", truncateToken(params.SyncToken),
		"time_min", params.TimeMin,
		"time_max", params.TimeMax,
		"single_events", params.SingleEvents,
		"private_extended_property", params.PrivateExtendedProperty,
	)
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, "", fmt.Errorf("gws events.list: marshal params: %w", err)
	}

	args := []string{
		"calendar", "events", "list",
		"--params", string(paramsJSON),
		"--page-all",
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return nil, "", err
	}
	if exit != 0 {
		return nil, "", classifyError(stdout, stderr, exit, "events.list")
	}

	pages, err := parseNDJSONPages(stdout)
	if err != nil {
		return nil, "", fmt.Errorf("gws events.list: %w", err)
	}

	for _, p := range pages {
		events = append(events, p.Items...)
	}
	if n := len(pages); n > 0 {
		nextSyncToken = pages[n-1].NextSyncToken
	}
	c.debug("gws.EventsList result",
		"calendar_id", params.CalendarID,
		"events", len(events),
		"next_sync_token", truncateToken(nextSyncToken),
	)
	return events, nextSyncToken, nil
}

// EventsGet fetches a single event by (calendarID, eventID). Used for
// orphan-walk lookups and recurring-instance parent fetches per SPEC.md.
func (c *Client) EventsGet(ctx context.Context, calendarID, eventID string) (*Event, error) {
	c.debug("gws.EventsGet", "calendar_id", calendarID, "event_id", eventID)
	paramsJSON, err := json.Marshal(map[string]any{
		"calendarId": calendarID,
		"eventId":    eventID,
	})
	if err != nil {
		return nil, fmt.Errorf("gws events.get: marshal params: %w", err)
	}

	args := []string{
		"calendar", "events", "get",
		"--params", string(paramsJSON),
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, classifyError(stdout, stderr, exit, "events.get")
	}

	var event Event
	if err := json.Unmarshal(stdout, &event); err != nil {
		return nil, fmt.Errorf("gws events.get: parse response: %w (stdout: %q)", err, string(stdout))
	}
	return &event, nil
}

// EventsInstancesParams holds the events.instances parameters. SPEC.md
// requires bounded TimeMin/TimeMax for horizon-eligibility checks; passing
// an unbounded query is the original Codex-caught bug and the caller is
// responsible for setting them.
type EventsInstancesParams struct {
	CalendarID    string `json:"calendarId"`
	EventID       string `json:"eventId"`
	TimeMin       string `json:"timeMin,omitempty"`
	TimeMax       string `json:"timeMax,omitempty"`
	OriginalStart string `json:"originalStart,omitempty"`
	MaxResults    int    `json:"maxResults,omitempty"`
	ShowDeleted   bool   `json:"showDeleted"`
}

// EventsInstances returns the materialized instances of a recurring event
// over the supplied time window. SPEC.md uses this for two purposes:
// horizon-eligibility checks on recurring parents (small bounded window,
// MaxResults=1) and locating a specific mirror instance by OriginalStart
// for the recurring-instance handler.
func (c *Client) EventsInstances(ctx context.Context, params EventsInstancesParams) ([]Event, error) {
	c.debug("gws.EventsInstances",
		"calendar_id", params.CalendarID,
		"event_id", params.EventID,
		"original_start", params.OriginalStart,
		"time_min", params.TimeMin,
		"time_max", params.TimeMax,
		"max_results", params.MaxResults,
	)
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("gws events.instances: marshal params: %w", err)
	}

	args := []string{
		"calendar", "events", "instances",
		"--params", string(paramsJSON),
		"--page-all",
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, classifyError(stdout, stderr, exit, "events.instances")
	}

	pages, err := parseNDJSONPages(stdout)
	if err != nil {
		return nil, fmt.Errorf("gws events.instances: %w", err)
	}

	var out []Event
	for _, p := range pages {
		out = append(out, p.Items...)
	}
	c.debug("gws.EventsInstances result",
		"calendar_id", params.CalendarID,
		"event_id", params.EventID,
		"items", len(out),
	)
	return out, nil
}

// truncateToken returns the first 12 chars of a sync token plus an ellipsis,
// or the full string if shorter. Sync tokens are opaque ~80-char blobs;
// logging the prefix is enough to correlate calls across a debug stream.
func truncateToken(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "..."
}

// parseNDJSONPages decodes gws --page-all output: one JSON object per line,
// each representing a single API page. Empty or whitespace-only lines are
// tolerated. A non-zero stdout that decodes to no pages is an error - it
// signals that gws emitted unexpected output we shouldn't silently ignore.
//
// Buffer capacity is 16MB. Real Calendar API pages are at most ~750KB
// (250 events × ~3KB each); the cap leaves an order of magnitude of
// headroom. A page exceeding the cap surfaces as a wrapped bufio.ErrTooLong
// from scanner.Err(), not as silent truncation.
func parseNDJSONPages(stdout []byte) ([]eventsPage, error) {
	var pages []eventsPage
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var p eventsPage
		if err := json.Unmarshal(line, &p); err != nil {
			return nil, fmt.Errorf("parse page line: %w (line: %q)", err, string(line))
		}
		pages = append(pages, p)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan stdout: %w", err)
	}
	if len(pages) == 0 && len(bytes.TrimSpace(stdout)) > 0 {
		return nil, fmt.Errorf("no pages parsed but stdout was non-empty: %q", string(stdout))
	}
	return pages, nil
}
