package mirror

import "github.com/tammersaleh/calendar-sync/internal/gws"

// SchemaVersion is the value calendar-sync writes to
// extendedProperties.private["calendar-sync:version"] on every mirror it
// creates. SPEC.md "Schema version migration" describes the upgrade path
// for legacy mirrors. v3 added `location` to the managed-field set.
// Existing v1 and v2 mirrors are migrated on first reconciliation via the
// direct managed-field comparison path; both legacy versions converge
// onto v3 in a single write.
const SchemaVersion = "3"

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
//   - summary, location, start, end, recurrence copied verbatim.
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
//
// Equivalent to BuildPayloadWithTimeZone(sourceCalendarID, source, "").
// Use BuildPayloadWithTimeZone to apply the per-pair `time_zone` config
// to all-day mirrored events.
func BuildPayload(sourceCalendarID string, source *gws.Event) *gws.Event {
	return BuildPayloadWithTimeZone(sourceCalendarID, source, "")
}

// BuildPayloadWithTimeZone is BuildPayload with the per-pair time_zone
// override applied to all-day source starts/ends. SPEC.md
// "[[pairs]].time_zone" defines the field as the IANA name to stamp onto
// mirrored events when the source is all-day.
//
// All-day events have Start.Date set and Start.DateTime empty. For those
// (and only those), the returned mirror's Start.TimeZone and End.TimeZone
// carry the supplied timeZone. timed events keep their original tz; the
// override is intentionally narrow.
//
// timeZone="" disables the override (matching the legacy BuildPayload
// behavior); Calendar API then falls back to the destination calendar's
// default.
func BuildPayloadWithTimeZone(sourceCalendarID string, source *gws.Event, timeZone string) *gws.Event {
	if source == nil {
		panic("mirror.BuildPayload: source is nil")
	}

	tuple := SourceTuple{CalendarID: sourceCalendarID, EventID: source.ID}

	mirror := &gws.Event{
		ID:           DeterministicID(sourceCalendarID, source.ID),
		Summary:      source.Summary,
		Description:  source.Description + trailerPrefix + source.HTMLLink,
		Location:     source.Location,
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

	if timeZone != "" {
		mirror.Start = applyAllDayTimeZone(source.Start, timeZone)
		mirror.End = applyAllDayTimeZone(source.End, timeZone)
	}

	return mirror
}

// applyAllDayTimeZone returns a copy of dt with TimeZone replaced when
// dt represents an all-day value (Date set, DateTime empty). For timed
// events the original pointer is returned unchanged - the source's
// per-event tz on a dateTime carries its own offset and overriding it
// would shift the wall-clock time.
//
// nil dt passes through unchanged so callers don't need to special-case
// missing Start/End.
func applyAllDayTimeZone(dt *gws.EventDateTime, timeZone string) *gws.EventDateTime {
	if dt == nil {
		return nil
	}
	if dt.Date == "" || dt.DateTime != "" {
		return dt
	}
	out := *dt
	out.TimeZone = timeZone
	return &out
}

// BuildInstancePayload constructs the mirror payload for one occurrence
// of a recurring event. Differs from BuildPayload in two ways per
// SPEC.md "Recurring Events" / "Step 3": recurrence is omitted (instances
// don't have their own recurrence; they belong to the parent), and
// calendar-sync:source carries the EXCEPTION's own ID, not the parent's.
//
// sourceCalendarID is the source calendar; source is the exception event
// (with recurringEventId pointing at the parent).
//
// Equivalent to BuildInstancePayloadWithTimeZone with empty timeZone.
func BuildInstancePayload(sourceCalendarID string, source *gws.Event) *gws.Event {
	return BuildInstancePayloadWithTimeZone(sourceCalendarID, source, "")
}

// BuildInstancePayloadWithTimeZone is BuildInstancePayload with the per-
// pair time_zone override applied to all-day source starts/ends; see
// BuildPayloadWithTimeZone for the override semantics.
func BuildInstancePayloadWithTimeZone(sourceCalendarID string, source *gws.Event, timeZone string) *gws.Event {
	mirror := BuildPayloadWithTimeZone(sourceCalendarID, source, timeZone)
	mirror.Recurrence = nil
	return mirror
}
