package gws

import (
	"context"
	"encoding/json"
	"fmt"
)

// EventsInsert creates an event on calendarID with the supplied body and
// returns the post-write Event resource. SPEC.md "Mirror identification" /
// "Deterministic mirror event IDs" expects the caller to set body.ID to the
// deterministic mirror ID for the source-tuple; Calendar API rejects
// duplicates with HTTP 409, which currently surfaces as a generic error
// from this wrapper. Layer 2.D will add typed 409 handling so the sync
// layer can drive the cancelled-and-revived path described in SPEC.
//
// The returned *Event is the canonical resource Google now stores. Per
// SPEC.md "Computing the checksum from the post-write event" the caller
// must use this response (not the request body) when computing the
// mirror's calendar-sync:checksum.
func (c *Client) EventsInsert(ctx context.Context, calendarID string, body *Event) (*Event, error) {
	if body == nil {
		return nil, fmt.Errorf("gws events.insert: body is nil")
	}

	paramsJSON, err := json.Marshal(map[string]any{"calendarId": calendarID})
	if err != nil {
		return nil, fmt.Errorf("gws events.insert: marshal params: %w", err)
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gws events.insert: marshal body: %w", err)
	}

	args := []string{
		"calendar", "events", "insert",
		"--params", string(paramsJSON),
		"--json", string(bodyJSON),
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, classifyError(stdout, stderr, exit, "events.insert")
	}

	var out Event
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("gws events.insert: parse response: %w (stdout: %q)", err, string(stdout))
	}
	return &out, nil
}

// EventsPatch sends a partial-update for the event identified by
// (calendarID, eventID). Only fields present in body are written; unset
// fields stay at their existing server-side values. Calendar API treats
// this as a JSON Merge Patch.
//
// The returned *Event is the post-write resource and is again the input to
// the next checksum per SPEC.md.
func (c *Client) EventsPatch(ctx context.Context, calendarID, eventID string, body *Event) (*Event, error) {
	if body == nil {
		return nil, fmt.Errorf("gws events.patch: body is nil")
	}

	paramsJSON, err := json.Marshal(map[string]any{
		"calendarId": calendarID,
		"eventId":    eventID,
	})
	if err != nil {
		return nil, fmt.Errorf("gws events.patch: marshal params: %w", err)
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gws events.patch: marshal body: %w", err)
	}

	args := []string{
		"calendar", "events", "patch",
		"--params", string(paramsJSON),
		"--json", string(bodyJSON),
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, classifyError(stdout, stderr, exit, "events.patch")
	}

	var out Event
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("gws events.patch: parse response: %w (stdout: %q)", err, string(stdout))
	}
	return &out, nil
}

// EventsDelete removes the event identified by (calendarID, eventID).
// Calendar API returns 204 No Content on success, which gws surfaces as
// exit 0 with empty stdout. SPEC.md "Sync Algorithm" uses this for orphan
// cleanup and source_cancelled propagation.
//
// Note: cancellation of a single recurring instance is implemented in the
// recurring-instance handler via events.patch with status="cancelled", not
// via events.delete; this wrapper covers only the "remove an event"
// primitive, not the user-facing action it produces.
func (c *Client) EventsDelete(ctx context.Context, calendarID, eventID string) error {
	paramsJSON, err := json.Marshal(map[string]any{
		"calendarId": calendarID,
		"eventId":    eventID,
	})
	if err != nil {
		return fmt.Errorf("gws events.delete: marshal params: %w", err)
	}

	args := []string{
		"calendar", "events", "delete",
		"--params", string(paramsJSON),
		"--format", "json",
	}

	stdout, stderr, exit, err := c.execute(ctx, args)
	if err != nil {
		return err
	}
	if exit != 0 {
		return classifyError(stdout, stderr, exit, "events.delete")
	}
	return nil
}
