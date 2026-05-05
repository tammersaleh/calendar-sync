package gws

// PatchEvent is the body shape for events.patch (JSON Merge Patch). Pointer
// fields distinguish "leave alone" (nil) from "set to value" (non-nil),
// including "set to the zero value" - which Calendar API treats as "clear
// this field". The full-payload Event type drops zero-valued strings via
// json:",omitempty"; that's correct for events.insert (where omitting a
// field means "no value") but wrong for events.patch when the source has
// genuinely cleared a field. This type is the patch-body counterpart so
// callers can express clear-intent unambiguously.
//
// Construction via the helper functions below: PatchStr / PatchStrClear for
// string fields, PatchRecurrence / PatchRecurrenceClear for recurrence.
// Sub-document fields (Start, End, Reminders, ExtendedProperties) are
// already pointer types on the wire, so the same pointer is fine for
// patches - nil means "leave alone", non-nil sets.
//
// Status is *string rather than enum-typed: the patch surface accepts the
// same string values as the Event type's Status (e.g. EventStatusCancelled,
// EventStatusConfirmed). PatchStr(gws.EventStatusCancelled) is the natural
// way to write a cancellation patch.
//
// ID is intentionally absent: events.patch carries the event ID in the URL,
// not the body. Including it would only invite confusion.
type PatchEvent struct {
	Status             *string             `json:"status,omitempty"`
	Summary            *string             `json:"summary,omitempty"`
	Description        *string             `json:"description,omitempty"`
	Location           *string             `json:"location,omitempty"`
	Start              *EventDateTime      `json:"start,omitempty"`
	End                *EventDateTime      `json:"end,omitempty"`
	Transparency       *string             `json:"transparency,omitempty"`
	Visibility         *string             `json:"visibility,omitempty"`
	Recurrence         *[]string           `json:"recurrence,omitempty"`
	Reminders          *Reminders          `json:"reminders,omitempty"`
	ExtendedProperties *ExtendedProperties `json:"extendedProperties,omitempty"`
}

// PatchStr returns a *string pointing to s for assignment to a PatchEvent
// field. Use for setting a string field to any value, including the zero
// value (which Calendar API treats as "clear this field").
//
// PatchStr("") is equivalent to PatchStrClear; both produce a non-nil
// pointer to "". The two helpers exist so the call site reads naturally:
// PatchStr(live.Summary) for "set to whatever the live value is" (which
// might be empty) and PatchStrClear() for the explicit clear-intent case.
func PatchStr(s string) *string {
	return &s
}

// PatchStrClear returns a non-nil pointer to "" for explicit clear-intent
// on a string field. Equivalent to PatchStr("").
func PatchStrClear() *string {
	v := ""
	return &v
}

// PatchRecurrence returns a *[]string pointing to r for assignment to a
// PatchEvent.Recurrence field. Use for setting recurrence to a specific
// array (RRULE/EXDATE/etc lines). A non-nil pointer to an empty slice is
// the "clear recurrence" form; PatchRecurrenceClear() is the dedicated
// helper for that case.
func PatchRecurrence(r []string) *[]string {
	return &r
}

// PatchRecurrenceClear returns a non-nil pointer to []string{} for explicit
// clear-intent on the Recurrence field. The empty array tells Calendar API
// "remove all recurrence rules", which is how the source converts a
// recurring event back to a single instance.
func PatchRecurrenceClear() *[]string {
	v := []string{}
	return &v
}
