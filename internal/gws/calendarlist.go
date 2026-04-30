package gws

import (
	"context"
	"encoding/json"
	"fmt"
)

// CalendarListEntry is the subset of the Calendar API CalendarListEntry
// resource that calendar-sync uses. The accessRole field drives config
// validation (SPEC.md "Validation rules") and the per-pdir source_writable
// flag (SPEC.md "Drift detection model").
type CalendarListEntry struct {
	ID         string `json:"id"`
	Summary    string `json:"summary,omitempty"`
	AccessRole string `json:"accessRole"`
}

// CalendarListGet returns the user's CalendarListEntry for calendarID.
// SPEC.md uses this at config-load time to resolve aliases like "primary"
// to a canonical ID and to capture each referenced calendar's accessRole.
//
// calendarID may be the literal "primary", an email address, or a group
// calendar ID; gws/Calendar API resolves all three.
func (c *Client) CalendarListGet(ctx context.Context, calendarID string) (*CalendarListEntry, error) {
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
		return nil, fmt.Errorf("gws calendarList.get: exit %d: %s", exit, string(stderr))
	}

	var entry CalendarListEntry
	if err := json.Unmarshal(stdout, &entry); err != nil {
		return nil, fmt.Errorf("gws calendarList.get: parse response: %w (stdout: %q)", err, string(stdout))
	}
	return &entry, nil
}
