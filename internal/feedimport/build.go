package feedimport

import (
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/ical"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// Feed-import extended-property namespace. Deliberately disjoint from the
// mirror namespace (calendar-sync:*): these events must look like ordinary
// user events to the sync engine so the work<->personal mirror mesh picks
// them up normally. Setting any calendar-sync:* key here would make the sync
// engine treat them as its own mirrors and skip them for loop-prevention.
const (
	propUID       = "calendar-sync-feed:uid"      // the ical UID; stable identity key
	propFeedID    = "calendar-sync-feed:feed_id"  // owning feed; scopes the delete candidate set
	propChecksum  = "calendar-sync-feed:checksum" // mirror.Checksum over the desired managed fields
	propVersion   = "calendar-sync-feed:version"  // schema version
	schemaVersion = "1"
)

const layoutDate = "2006-01-02"

// buildEvent projects one ical.Item into the desired Calendar-API event,
// including the feed extended properties and the pre-write checksum. The
// checksum is computed over the same managed fields the importer will compare
// on the next run, so change detection is self-consistent (we never compare
// against Google's post-write normalization).
func (im *Importer) buildEvent(it ical.Item) *gws.Event {
	ev := &gws.Event{
		Status:       gws.EventStatusConfirmed,
		Summary:      it.Summary,
		Description:  it.Description,
		Location:     it.Location,
		Start:        toEventDateTime(it.Start),
		End:          toEventDateTime(it.End),
		Transparency: transparency(it.Transparency),
	}
	checksum := mirror.Checksum(managedFields(ev))
	ev.ExtendedProperties = &gws.ExtendedProperties{
		Private: map[string]string{
			propUID:      it.UID,
			propFeedID:   im.FeedID,
			propVersion:  schemaVersion,
			propChecksum: checksum,
		},
	}
	return ev
}

// buildPatch produces the merge-patch body for updating an existing feed event
// to match want. It carries every managed field (including Transparency, which
// is part of the checksum) plus the feed extended properties; recurrence,
// visibility, and reminders are intentionally left untouched (they belong to
// the user/calendar default). Status is not set here - the revive path sets it
// explicitly. Transparency is written unconditionally (via PatchStr, clearing
// to "" when the feed drops TRANSP): guarding on non-empty would let a checksum
// that includes Transparency diverge from the event Google actually stores,
// stranding the field permanently as "Unchanged".
func (im *Importer) buildPatch(want *gws.Event) *gws.PatchEvent {
	return &gws.PatchEvent{
		Summary:            gws.PatchStr(want.Summary),
		Description:        gws.PatchStr(want.Description),
		Location:           gws.PatchStr(want.Location),
		Start:              want.Start,
		End:                want.End,
		Transparency:       gws.PatchStr(want.Transparency),
		ExtendedProperties: want.ExtendedProperties,
	}
}

// toEventDateTime maps an ical.DateTime to the Calendar-API shape. A zero
// DateTime (absent DTEND) yields nil so the field is omitted. All-day uses
// Date; timed uses an RFC 3339 DateTime plus TimeZone (the item's TZID, or
// "UTC" when TZID is empty per the ical package's UTC-means-empty contract).
func toEventDateTime(dt ical.DateTime) *gws.EventDateTime {
	if dt.Time.IsZero() {
		return nil
	}
	if dt.AllDay {
		return &gws.EventDateTime{Date: dt.Time.Format(layoutDate)}
	}
	tz := dt.TZID
	if tz == "" {
		tz = "UTC"
	}
	return &gws.EventDateTime{DateTime: dt.Time.Format(time.RFC3339), TimeZone: tz}
}

// transparency maps the ical (upper-cased) TRANSP value to the Calendar-API
// lower-case form, leaving it unset ("") when the feed didn't specify it.
func transparency(t string) string {
	switch t {
	case "TRANSPARENT":
		return gws.TransparencyTransparent
	case "OPAQUE":
		return gws.TransparencyOpaque
	default:
		return ""
	}
}

// managedFields builds the checksum input from a desired event. Visibility is
// always "" and Recurrence always nil: the importer never manages either, so
// they must not enter the hash.
func managedFields(ev *gws.Event) mirror.ManagedFields {
	return mirror.ManagedFields{
		Description:  ev.Description,
		End:          toMirrorDateTime(ev.End),
		Location:     ev.Location,
		Recurrence:   nil,
		Start:        toMirrorDateTime(ev.Start),
		Summary:      ev.Summary,
		Transparency: ev.Transparency,
		Visibility:   "",
	}
}

func toMirrorDateTime(dt *gws.EventDateTime) mirror.EventDateTime {
	if dt == nil {
		return mirror.EventDateTime{}
	}
	return mirror.EventDateTime{Date: dt.Date, DateTime: dt.DateTime, TimeZone: dt.TimeZone}
}
