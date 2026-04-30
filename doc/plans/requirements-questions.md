# Requirements (round 1)

Decisions captured from review round 1, plus research findings on the items that needed investigation. This file is the input to `SPEC.md`.

## Scope and providers

- Google Workspace only. All Google Calendar interactions go through the `gws` CLI; no custom Google app or OAuth flow is needed (auth is handled by `gws`).
- The tool is configurable. Anyone can declare their own calendar pairs - no hardcoded accounts or calendars. Heather's calendar is out of scope.

## Sync pairs

- Sync pairs are user-declared.
- Bidirectional sync is required. Each pair declares direction: `a->b`, `b->a`, or `a<->b`.
- Loop prevention: mirror events carry a marker in `extendedProperties.private` (see "State" below). The sync skips any event that already has the marker, so a bidirectional pair never re-mirrors a mirror.

## Runtime

- Phase 1: macOS launchd on the user's laptop.
- Cloud deployment deferred to a later phase. The polling design supports both.

## State

Stored as `extendedProperties.private` on each mirror event. Confirmed via `gws schema calendar.events.insert` and `events.list`:

- The Event resource has an `extendedProperties` object with `private` and `shared` maps.
- `events.list` accepts repeated `privateExtendedProperty=key=value` filters, so we can locate every mirror event the tool created without scanning the calendar.
- Per-event limits are 16 private + 16 shared properties, ample for sync state.

Per-mirror state we care about: source calendar ID, source event ID, source `updated` timestamp (so we can detect drift). All three fit in private extended properties.

Per-source-calendar state we care about: a `syncToken`. This is the one piece of state that does *not* live on events, and it's worth explaining.

A `syncToken` is an opaque string returned by Google in the `nextSyncToken` field on the last page of an `events.list` response. The next time we call `events.list` with `syncToken=<previous>`, Google returns only the events that changed (created, updated, or cancelled) since the previous call. It's a checkpoint in the calendar's change stream. Without it, every poll has to fetch the full time-window and we figure out what changed by diffing against our own records. With it, an empty delta is a single API call.

Why it can't go on an event:

- It's calendar-scoped, not event-scoped. There's one current token per source calendar, not one per event.
- Bootstrap: on the first sync, no mirror events exist yet, so there's nowhere on an event to put the token returned by the initial full list.
- Updating it on every poll would mean writing to one (or every) event every minute, just to record "we polled". Noise on the calendar, extra API calls, no benefit.
- 410 recovery: when Google expires the token (after roughly 7 days, or after schema changes), we need a clean place to overwrite it after re-deriving via a full list.

So the token lives in a small JSON file at `~/.config/calendar-sync/state.json`, keyed by source calendar ID. Shape:

```json
{
  "<calendar_id>": {
    "sync_token": "CPDC...",
    "last_synced_at": "2026-04-29T23:00:00Z"
  }
}
```

That's the entire local state. No SQLite.

## Mirror events

- Title: copy verbatim from the source.
- Transparency: `opaque` (busy) on every mirror, regardless of source. (We only mirror busy events anyway - see filtering.)
- Visibility: `private` on every mirror.
- Description: copy of the source description, followed by a blank line and a trailing link to the source event. The link uses the source event's `htmlLink` field, which Google auto-populates and which opens the event in Google Calendar for anyone with access. Suggested format:

  ```
  <source description>

  ---
  Source: <htmlLink>
  ```

## Filtering (skip rules)

A source event is skipped if any of these are true:

- The source calendar owner has declined the event.
- The event's `transparency` is `transparent` (i.e. marked Free). This single rule subsumes the all-day case: an all-day event marked Busy syncs, an all-day event marked Free doesn't, same as for time-bounded events.
- The event already has our mirror marker in `extendedProperties.private` (loop prevention).

No keyword or label filtering in this phase.

## Time horizon

- Sync events with start times within `[now, now + 1 year]`.
- Past events are not synced.
- Recurring events are mirrored as a **single recurring target event** by copying the source's `recurrence` field (an array of RRULE/RDATE/EXDATE lines per RFC 5545). `gws schema` confirms this field is writable on `events.insert` and `events.patch`.
- Modified instances of recurring events (the user moved one occurrence to a different time) appear as separate items with `recurringEventId` pointing at the parent. We mirror those as exception instances on the target series.
- This keeps the event count low at the 1-year horizon: a typical weekly meeting is one mirrored series, not 52 mirrored instances.

## Sync frequency

Decision: poll every 60 seconds using `syncToken` for incremental sync. Webhooks are deferred.

Why not webhooks (`events.watch`):

- Google requires the webhook URL to be HTTPS with a domain verified in Search Console (DNS verification on a domain the user controls). Not feasible for a laptop deployment.
- Watch channels expire after 7 days and must be renewed.
- The laptop is intermittent (sleep, no public IP). Even with ngrok, the webhook would miss events when the laptop is offline.

Why polling is cheap enough:

- `events.list` returns `nextSyncToken` on the last page of every successful run.
- Subsequent calls pass `syncToken=<previous>` and get only events that changed since then.
- Per Google's docs: empty delta responses cost a single API call regardless of total event count.
- 60 seconds * 60 minutes * 24 hours = 1440 calls/day per source calendar. Calendar API quota is 1M/day per project. Two-figure orders of magnitude of headroom.

If `syncToken` returns 410 Gone (Google expires tokens after roughly 7 days of inactivity, or after schema changes), full re-list within the time window and re-derive the token.

When the cloud deployment lands, we revisit webhooks.

## Stack

- Go + kong for the CLI. mise for tasks. Same as `slack-cli`.
- Distribution: Homebrew tap (`tammersaleh/homebrew-tap`), release-please + GoReleaser, conventional commits drive releases. Same as `slack-cli`.
- Calendar API access via `gws` CLI shelled out from Go (no direct Google SDK).

## Open follow-ups

None at this point. Ready to draft `SPEC.md`.
