package gws

import (
	"context"
	"encoding/json"
	"fmt"
)

// Calendar is the metadata subset of the Calendar API Calendar resource
// that calendar-sync's calendars.* methods read and write. Distinct from
// CalendarListEntry: that's a per-user listing entry; this is the
// underlying calendar resource. The two share `id` and `summary` but
// CalendarListEntry adds user-scoped fields (accessRole, color overrides,
// notification settings) while Calendar carries owner-scoped fields
// (description, timeZone). Only the fields E2E and F1 need are modeled.
type Calendar struct {
	ID          string `json:"id,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	TimeZone    string `json:"timeZone,omitempty"`
}

// CalendarsInsert creates a secondary calendar owned by the authenticated
// user. The returned *Calendar carries the assigned id (a group-calendar
// id of the form `c_<hash>@group.calendar.google.com`).
//
// Only `summary`, `description`, and `timeZone` are sent; the API
// auto-populates `id` and other fields. Used by E2E setup to provision
// fixture calendars on demand.
func (c *Client) CalendarsInsert(ctx context.Context, body *Calendar) (*Calendar, error) {
	if body == nil {
		return nil, fmt.Errorf("gws calendars.insert: body is nil")
	}
	c.debug("gws.CalendarsInsert", "summary", body.Summary)

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gws calendars.insert: marshal body: %w", err)
	}

	args := []string{
		"calendar", "calendars", "insert",
		"--json", string(bodyJSON),
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, classifyError(stdout, stderr, exit, "calendars.insert")
	}

	var out Calendar
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("gws calendars.insert: parse response: %w (stdout: %q)", err, string(stdout))
	}
	return &out, nil
}

// CalendarsDelete deletes a secondary calendar by id. The API returns 204
// No Content on success. Note: Calendar API rejects deletion of primary
// calendars; only secondary (group) calendars can be removed this way.
//
// gws renders the empty 204 body as a "saved file" note on stdout in some
// versions and may write a stray `download.html` in cwd. Callers that
// care about repo hygiene should construct the Client with WithWorkDir
// pointing at a sandbox directory.
func (c *Client) CalendarsDelete(ctx context.Context, calendarID string) error {
	c.debug("gws.CalendarsDelete", "calendar_id", calendarID)
	paramsJSON, err := json.Marshal(map[string]any{"calendarId": calendarID})
	if err != nil {
		return fmt.Errorf("gws calendars.delete: marshal params: %w", err)
	}

	args := []string{
		"calendar", "calendars", "delete",
		"--params", string(paramsJSON),
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return err
	}
	if exit != 0 {
		return classifyError(stdout, stderr, exit, "calendars.delete")
	}
	return nil
}
