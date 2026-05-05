package gws

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// patchBodyFields returns the names of the top-level PatchEvent fields that
// are present in body (non-nil), sorted alphabetically. Used by the debug
// log to surface what a patch is actually changing without dumping the full
// payload (which would be noisy and might leak event content). Presence is
// the right signal for the patch type: a non-nil pointer to "" represents
// an explicit clear, which a value-based "non-empty" check would miss.
func patchBodyFields(body *PatchEvent) []string {
	if body == nil {
		return nil
	}
	var fields []string
	if body.Status != nil {
		fields = append(fields, "status")
	}
	if body.Summary != nil {
		fields = append(fields, "summary")
	}
	if body.Description != nil {
		fields = append(fields, "description")
	}
	if body.Location != nil {
		fields = append(fields, "location")
	}
	if body.Start != nil {
		fields = append(fields, "start")
	}
	if body.End != nil {
		fields = append(fields, "end")
	}
	if body.Transparency != nil {
		fields = append(fields, "transparency")
	}
	if body.Visibility != nil {
		fields = append(fields, "visibility")
	}
	if body.Recurrence != nil {
		fields = append(fields, "recurrence")
	}
	if body.Reminders != nil {
		fields = append(fields, "reminders")
	}
	if body.ExtendedProperties != nil {
		fields = append(fields, "extendedProperties")
	}
	sort.Strings(fields)
	return fields
}

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
	c.debug("gws.EventsInsert", "calendar_id", calendarID, "event_id", body.ID, "summary", body.Summary)

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

	stdout, _, err := c.executeTyped(ctx, args, "events.insert")
	if err != nil {
		return nil, err
	}

	var out Event
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("gws events.insert: parse response: %w (stdout: %q)", err, string(stdout))
	}
	return &out, nil
}

// EventsPatch sends a partial-update for the event identified by
// (calendarID, eventID). Only fields PRESENT (non-nil) in body are written;
// unset fields stay at their existing server-side values. Calendar API
// treats this as a JSON Merge Patch.
//
// Body is *PatchEvent (not *Event) so callers can express clear-intent on
// individual fields. A nil PatchEvent pointer is rejected; a non-nil
// PatchEvent with all fields nil is a no-op patch (Google accepts it,
// nothing changes server-side, the response is the unchanged resource).
//
// The returned *Event is the post-write resource and is again the input to
// the next checksum per SPEC.md.
func (c *Client) EventsPatch(ctx context.Context, calendarID, eventID string, body *PatchEvent) (*Event, error) {
	if body == nil {
		return nil, fmt.Errorf("gws events.patch: body is nil")
	}
	c.debug("gws.EventsPatch", "calendar_id", calendarID, "event_id", eventID, "field_keys", patchBodyFields(body))

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

	stdout, _, err := c.executeTyped(ctx, args, "events.patch")
	if err != nil {
		return nil, err
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
	c.debug("gws.EventsDelete", "calendar_id", calendarID, "event_id", eventID)
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

	if _, _, err := c.executeTyped(ctx, args, "events.delete"); err != nil {
		return err
	}
	return nil
}
