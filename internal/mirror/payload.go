package mirror

import "github.com/tammersaleh/calendar-sync/internal/gws"

// SchemaVersion is the value calendar-sync writes to
// extendedProperties.private["calendar-sync:version"] on every mirror it
// creates. SPEC.md "Schema version migration" describes the v1→v2 upgrade
// path; bumping this constant requires adding a new migration branch.
const SchemaVersion = "2"

// Extended-property keys SPEC.md "Mirror identification" mandates on every
// mirror. The "calendar-sync:" prefix is the package's namespace; nothing
// else should write keys with that prefix.
const (
	ExtKeySource         = "calendar-sync:source"
	ExtKeySourceUpdated  = "calendar-sync:source_updated"
	ExtKeyChecksum       = "calendar-sync:checksum"
	ExtKeyVersion        = "calendar-sync:version"
)

// trailerPrefix is the literal prefix calendar-sync prepends to the source
// description before the htmlLink. The full trailer matches trailerPattern
// (in trailer.go) so StripTrailer can remove it on the propagate path.
const trailerPrefix = "\n\n---\nSource: "

// BuildPayload constructs the full mirror Event payload from a source
// event per SPEC.md "Mirror event payload (insert and patch)". The
// returned *gws.Event has:
//
//   - id set to the deterministic mirror ID derived from
//     (sourceCalendarID, source.ID).
//   - summary, start, end, recurrence copied verbatim.
//   - description = source.description + the calendar-sync trailer.
//   - transparency=opaque, visibility=private (forced regardless of
//     source values).
//   - reminders.useDefault=false so destination calendar reminders don't
//     fire on busy mirrors.
//   - extendedProperties.private populated with calendar-sync:source,
//     :source_updated, :version. The :checksum key is intentionally
//     omitted; it's set by a follow-up patch using the post-write
//     resource per SPEC.md "Computing the checksum from the post-write
//     event".
//
// Source ID and HTMLLink come from the source.ID and source.HTMLLink
// fields; nil source is a programmer bug and panics.
func BuildPayload(sourceCalendarID string, source *gws.Event) *gws.Event {
	if source == nil {
		panic("mirror.BuildPayload: source is nil")
	}

	tuple := SourceTuple{CalendarID: sourceCalendarID, EventID: source.ID}

	mirror := &gws.Event{
		ID:           DeterministicID(sourceCalendarID, source.ID),
		Summary:      source.Summary,
		Description:  source.Description + trailerPrefix + source.HTMLLink,
		Start:        source.Start,
		End:          source.End,
		Recurrence:   source.Recurrence,
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		Reminders:    &gws.Reminders{UseDefault: false},
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				ExtKeySource:        tuple.String(),
				ExtKeySourceUpdated: source.Updated,
				ExtKeyVersion:       SchemaVersion,
			},
		},
	}
	return mirror
}

// BuildInstancePayload constructs the mirror payload for one occurrence
// of a recurring event. Differs from BuildPayload in two ways per
// SPEC.md "Recurring Events" / "Step 3": recurrence is omitted (instances
// don't have their own recurrence; they belong to the parent), and
// calendar-sync:source carries the EXCEPTION's own ID, not the parent's.
//
// sourceCalendarID is the source calendar; source is the exception event
// (with recurringEventId pointing at the parent).
func BuildInstancePayload(sourceCalendarID string, source *gws.Event) *gws.Event {
	mirror := BuildPayload(sourceCalendarID, source)
	mirror.Recurrence = nil
	return mirror
}
