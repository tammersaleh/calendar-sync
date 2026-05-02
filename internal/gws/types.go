package gws

// Event is the subset of the Calendar API Event resource that calendar-sync
// reads from gws responses. Field tags match the wire format exactly so a
// gws response unmarshals straight in.
//
// The shape is deliberately a subset: only the fields the sync algorithm
// actually consults. Fields we don't manage (conferenceData, organizer,
// attendees beyond the source-owner check, etc.) are absent; they round-
// trip through ExtendedProperties or are simply ignored.
type Event struct {
	ID                 string              `json:"id,omitempty"`
	Status             string              `json:"status,omitempty"`
	Summary            string              `json:"summary,omitempty"`
	Description        string              `json:"description,omitempty"`
	Location           string              `json:"location,omitempty"`
	Start              *EventDateTime      `json:"start,omitempty"`
	End                *EventDateTime      `json:"end,omitempty"`
	Transparency       string              `json:"transparency,omitempty"`
	Visibility         string              `json:"visibility,omitempty"`
	Recurrence         []string            `json:"recurrence,omitempty"`
	RecurringEventID   string              `json:"recurringEventId,omitempty"`
	OriginalStartTime  *EventDateTime      `json:"originalStartTime,omitempty"`
	Updated            string              `json:"updated,omitempty"`
	HTMLLink           string              `json:"htmlLink,omitempty"`
	EventType          string              `json:"eventType,omitempty"`
	Attendees          []Attendee          `json:"attendees,omitempty"`
	Reminders          *Reminders          `json:"reminders,omitempty"`
	ExtendedProperties *ExtendedProperties `json:"extendedProperties,omitempty"`
}

// EventDateTime is the Calendar API representation of a start or end time:
// either Date (all-day, "YYYY-MM-DD") or DateTime (RFC 3339), never both.
// TimeZone is an IANA name attached to a DateTime.
//
// This struct intentionally mirrors mirror.EventDateTime field-for-field;
// the canonical-hash form and the wire form are identical. A future
// consolidation may move this to a shared package, but for now both
// definitions stay in lockstep through tests.
type EventDateTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

// Attendee is the subset of an Event's attendee entry that calendar-sync uses.
// The sync algorithm checks Self+ResponseStatus to detect a "declined" event
// for the source-owner attendee. Email is captured for log context.
type Attendee struct {
	Email          string `json:"email,omitempty"`
	Self           bool   `json:"self,omitempty"`
	Organizer      bool   `json:"organizer,omitempty"`
	ResponseStatus string `json:"responseStatus,omitempty"`
}

// Reminders is the Event.reminders subdocument. calendar-sync writes
// {"useDefault": false} on every mirror so destination-calendar reminder
// settings don't fire on busy mirrors.
type Reminders struct {
	UseDefault bool       `json:"useDefault"`
	Overrides  []Reminder `json:"overrides,omitempty"`
}

// Reminder is one entry in Reminders.Overrides; calendar-sync doesn't write
// these but tolerates them in responses.
type Reminder struct {
	Method  string `json:"method"`
	Minutes int    `json:"minutes"`
}

// ExtendedProperties is the Event.extendedProperties subdocument. The
// calendar-sync provenance keys (calendar-sync:source, etc.) live in Private.
type ExtendedProperties struct {
	Private map[string]string `json:"private,omitempty"`
	Shared  map[string]string `json:"shared,omitempty"`
}

// Calendar API attendee response statuses. Used in classification to detect
// declined events from the source calendar's owner.
const (
	ResponseStatusNeedsAction = "needsAction"
	ResponseStatusDeclined    = "declined"
	ResponseStatusTentative   = "tentative"
	ResponseStatusAccepted    = "accepted"
)

// Calendar API event statuses. Cancelled events on a delta represent
// deletions; the sync algorithm consumes them to drive mirror cleanup.
const (
	EventStatusConfirmed = "confirmed"
	EventStatusTentative = "tentative"
	EventStatusCancelled = "cancelled"
)

// Calendar API event types. The list filter is restricted to the first three
// per SPEC.md; birthday/fromGmail/workingLocation are excluded server-side
// via the eventTypes parameter.
const (
	EventTypeDefault         = "default"
	EventTypeOutOfOffice     = "outOfOffice"
	EventTypeFocusTime       = "focusTime"
	EventTypeBirthday        = "birthday"
	EventTypeFromGmail       = "fromGmail"
	EventTypeWorkingLocation = "workingLocation"
)

// Calendar API transparency values. Mirrors are always opaque; transparent
// source events are filtered out at the classification step.
const (
	TransparencyOpaque      = "opaque"
	TransparencyTransparent = "transparent"
)

// Calendar API visibility values. Mirrors are always private regardless of
// source visibility.
const (
	VisibilityDefault      = "default"
	VisibilityPublic       = "public"
	VisibilityPrivate      = "private"
	VisibilityConfidential = "confidential"
)
