# Plan: iCal feed importer (TripIt fast-sync)

Status: IMPLEMENTED on branch `feat/ical-importer` (not merged/pushed). All six
layers built red-green, each with a before+after `feature-dev:code-reviewer`
pass. `mise run check` green. Commits: `912f780` (plan) → `5667f51` ical →
`0354c6d` feed → `7819adf` feedimport → `a8f9309` config → `6407228` feat (the
user-facing wiring) → docs (this pass).

Remaining before it does anything (all await user go-ahead - outward-facing /
secret-writing, so NOT done autonomously):
1. Final full-diff `feature-dev:code-reviewer` pass, then merge `feat/ical-importer`
   to main + push. The `feat:` commit auto-cuts a Homebrew release.
2. Install the release + restart the daemon (per CLAUDE.md "Installing a release").
3. User adds a `[[feeds]]` entry to config.toml (recommend `url_env = "TRIPIT_ICAL_URL"`
   with the TripIt feed URL in the env) targeting `me@tammersaleh.com`; the existing
   `personal-to-work` pair mirrors onward to CoreWeave.
4. Verify the July-13 trip imports with the rebooked flights, then do the DEFERRED
   one-time cleanup of the 16 stale `navan*` events on CoreWeave.

## Resolved decisions (user, this session)

- **Root cause was NOT what we first thought.** The travel events on the user's
  calendars come from Navan's OAuth Google-Calendar integration (native
  `navan*`-ID events on CoreWeave, mirrored to Personal), NOT the TripIt feed.
  That OAuth integration is now DISCONNECTED (16 orphaned `navan*` events on
  CoreWeave, newest `updated` 2026-06-12, frozen at the pre-rebook itinerary).
  The user's Navan settings show only the read-only feed link, not the "Active"
  OAuth widget — likely CoreWeave disabled Navan OAuth calendar write org-wide.
  Both fresh feeds (TripIt PT15M, Navan PT12H) already have the rebooked
  itinerary (STS->LAX->EWR); the two stale Google-side copies (native Navan
  events + Google's TripIt subscription) do not.

- **Source feed: TripIt** (`https://www.tripit.com/feed/ical/private/<TOKEN>/tripit.ics`).
  Chosen over Navan's feed for freshness (15 min vs 12 h; they agree in content).

- **Target: Personal** (`me@tammersaleh.com`). The existing enabled
  `personal-to-work` pair (source=Personal -> target=CoreWeave) then mirrors the
  imported flights onto CoreWeave automatically. Both pairs confirmed
  `enabled=true`; global `propagate_target_edits=true`.

- **Single-tick propagation.** Importer phase runs BEFORE the reconcile phase in
  the same daemon tick, so a feed change reaches Personal and then CoreWeave in
  one ~60s tick (worst case next tick if Google's change feed lags). This is why
  the importer is a phase in the daemon loop, not a separate schedule.

- **PREREQUISITE: one-time cleanup of the 16 stale `navan*` events on CoreWeave**
  before enabling the importer. Otherwise CoreWeave mirrors stale navan flights
  to Personal WHILE the importer adds fresh TripIt flights to Personal =>
  duplicate/conflicting flights on both calendars. calendar-sync auto-prunes the
  personal-side mirrors once the CoreWeave originals are gone (same as the
  Reclaim cleanup in next.md). List for user OK before deleting.

- **CALENDAR-SAFETY INVARIANT (target is a real calendar the user edits).** The
  importer's insert/patch/DELETE must be scoped EXCLUSIVELY to events carrying
  its own `calendar-sync-feed:*` marker. "Absent from feed => delete" means
  "feed-owned event whose UID vanished from a SUCCESSFUL fetch", never "any
  Personal event not in the feed". Pin with tests; this is the calendar-wipe
  guard.

- **Parser scope: GENERIC, third-party dependency.** User confirmed: keep it
  generalized, a third-party package is fine. Selected
  **`github.com/arran4/golang-ical`** (v0.3.5, Apache-2.0, go1.20): 406 stars, 38
  contributors, commits within days (2026-06/07), tagged releases. README notice
  "Looking for a co-maintainer" = seeking help, NOT abandoned (maintainer
  active). Rejected: emersion/go-ical (72 stars, minimal, 2025-06),
  apognu/gocal (parse-only, 2024-10 going stale), PuloV/ics-golang (abandoned
  2020), luxifer/ical (low activity 2023).
  - API: `ics.ParseCalendar(io.Reader)` -> `cal.Events()` ->
    `event.GetProperty(ics.ComponentPropertyX).Value`, `GetStartAt()/GetEndAt()`.
    We fetch bytes ourselves (control caching + secret URL) and pass a reader;
    do NOT use `ParseCalendarFromUrl` (no header/secret control).
  - **RRULE caveat:** golang-ical parses but does NOT expand recurrence into
    occurrences. Fine for TripIt/Navan/most booking feeds (pre-expanded concrete
    VEVENTs). A truly generic feed carrying a recurring MASTER would need
    `teambition/rrule-go` (374 stars, MIT, low-activity 2024-08 - re-vet if
    needed) paired in for expansion. Out of scope for v1; document the boundary.
  - **All-day handling:** `GetStartAt()` returns time.Time; read the `VALUE=DATE`
    parameter explicitly to map to Google `start.date` (all-day, exclusive end)
    vs `start.dateTime`. Pin with tests (multi-day hotel/trip spans).

## Superseded framing

## Problem

User subscribes Google Calendar to a TripIt iCal feed. TripIt regenerates the
feed within ~15 min (`X-PUBLISHED-TTL:PT15M`, `Cache-Control: max-age=900`), but
Google re-polls external iCal subscriptions on its own slow schedule (hours to a
day). Verified live 2026-06-29: the July-13 NYC trip was rebooked end to end
(four new flight numbers, routing STS↔LAX↔EWR/PDX instead of STS↔PDX↔JFK/SAN,
parking drop-off moved 2h earlier); the feed showed the new flights, Google's
subscribed copy still showed the old ones. The four flight VEVENTs even have
different UIDs on each side (TripIt deletes old objects + creates new on
rebook), so it's a wholesale itinerary swap Google hadn't picked up.

Fix: calendar-sync polls the `.ics` itself on a fast cadence and materializes it
into a Google calendar it owns, replacing Google's lazy subscription.

## Chosen architecture: Option B (dedicated importer), not a new source "kind"

Rejected Option A (make a pair's source an iCal URL inside the existing
Reconciler). The reuse seam is narrower than it looks: everything *above*
`sync.API.EventsList -> []gws.Event` is Google-shaped — `config.CalendarRef`/
`Canonicalize` assume a Google calendar with an accessRole; `Tick`/`FullSync`
assume a per-source syncToken lifecycle (no token => `NeedsFullResync`, so an
iCal source would look permanently broken); the orphan/delete walk explains
unvisited mirrors via source-side `events.get`/`events.instances`, impossible
for a feed. Option A would force a second source-lifecycle model and a second
delete model into the reconciler — i.e. a second reconciler anyway.

Option B: a separate importer subsystem that fetches the feed snapshot,
normalizes to a small internal item model, and reconciles it into ONE owned
Google ("shadow") calendar via a full-snapshot diff. The Google-to-Google
reconciler stays pure. Output is a normal Google calendar; if the user wants
those events elsewhere, the EXISTING mesh mirrors that calendar onward like any
other.

Keep a small internal seam (snapshot-source interface) so a future second feed
(another `.ics`, CalDAV, etc.) is cheap — but do NOT expose a generic source
kind on `Pair` yet.

## Frozen design decisions (from Codex review, thread 019f14ec)

1. **Dedicated owned shadow calendar**, distinct summary (e.g. `TripIt
   (calendar-sync)`), NOT the existing slow subscription and NOT a bare `TripIt`
   (would make summary-based config lookup ambiguous). Importer owns its
   lifecycle cleanly.

2. **Importer events do NOT use mirror metadata.** No `calendar-sync:source`
   (loop prevention would skip them as "already a mirror" if the shadow calendar
   is later a source) and no reuse of `calendar-sync:checksum/source_updated/
   version` (mirror tooling — `BuildInventory` etc. — treats the
   `calendar-sync:` mirror namespace as "this is a mirror"). Use a SEPARATE
   namespace, e.g. `calendar-sync-feed:key`, `calendar-sync-feed:checksum`,
   `calendar-sync-feed:version`.

3. **Stable deterministic Google event IDs for importer events**, derived from
   stable feed-entry identity (UID), with a DIFFERENT prefix than mirror IDs.
   Never let Google auto-generate. Reason: downstream mirror identity is keyed
   by `(source_cal_id, source_event_id)`; if the importer churns Google event
   IDs, downstream sees every event as new => delete+insert churn + unstable
   mirror IDs. Do NOT derive the ID from the secret URL or the mutable summary.

4. **Identity by `UID`** (+ `RECURRENCE-ID` only if recurrence is ever
   supported). Time is exactly what changes on a rebook, so never key by time.

5. **Change detection via checksum over the normalized managed payload**, not
   `DTSTAMP`. DTSTAMP/SEQUENCE are hints, not source of truth.

6. **Import the FULL feed snapshot — no importer-side horizon.** This removes
   the "missing from feed = deleted OR aged-out?" ambiguity entirely. Deletion
   becomes exact: managed event absent from a *successful* snapshot => delete.
   Downstream pairs keep applying their normal per-pair horizon when mirroring
   onward. (Optional separate retention/prune policy later if the shadow
   calendar gets cluttered with past trips — but retention != delete-detection.)

7. **Delete ONLY on a successful 200 + full parse.** Never treat fetch failure,
   timeout, 304, or parse error as an empty feed. This is the calendar-wipe
   footgun.

8. **Feed-backed shadow calendars are semantically read-only sources.** A
   user-owned calendar has `accessRole=writer` => the existing engine computes
   `SourceWritable=true`. If a downstream pdir sources the shadow calendar with
   `propagate_target_edits=true`, a mirror edit propagates back into the shadow
   calendar, then the next feed poll overwrites it, then it bounces out again.
   Global default is already `false`, so v1 is safe by default, but the rule is
   explicit: pdirs sourcing a feed calendar MUST keep `propagate_target_edits=
   false`. (A first-class "authoritative-but-not-back-propagatable" source
   concept, independent of accessRole, is a possible future hardening.)

9. **Phase ordering: importer runs BEFORE normal sync each tick**, so a feed
   change can propagate downstream in the same tick. Another reason it's a
   separate phase, not inside `sync.Reconciler`.

10. **HTTP caching, but correctness never depends on it.** In-memory
    `next_fetch_at` gate honoring `Cache-Control: max-age` (fallback
    `X-PUBLISHED-TTL`). Send `If-None-Match` when an ETag is cached; 304 => no-op.

11. **Feed URL is a bearer secret.** Config accepts `url` XOR `url_env`
    (env-var name). Never log the full URL, never store it in event properties,
    never use it in deterministic IDs, redact in `config show`. Be careful not
    to leak it across hosts on redirect.

12. **Failure isolation.** A feed fetch/parse failure must not abort unrelated
    Google pair syncs; leave the shadow calendar stale and surface the feed as
    failed separately.

## Parser stance (decision 1, see Open decisions)

Codex: do NOT write a permissive "minimal" parser — worst of both worlds. Either
a strict narrow parser or a real dependency. Recommendation: **TripIt-profile
strict parser**, stdlib-only, supporting exactly what the feed emits — line
unfolding, CRLF/LF, TEXT unescaping (`\n \, \; \\`), `VEVENT` with `UID /
SUMMARY / DESCRIPTION / LOCATION / DTSTART / DTEND / DTSTAMP`, `VALUE=DATE`
(all-day, DTEND exclusive — preserve as `start.date`/`end.date`, never midnight
UTC), and UTC `...Z` datetimes — and HARD-REJECT (loud error, no silent
mishandling) `RRULE`, `TZID`/floating local times, `DURATION`-instead-of-DTEND,
`RECURRENCE-ID`. Verified the live feed has none of the rejected constructs.

## Sketch of components

- `internal/ical/` — strict TripIt-profile parser. `Parse([]byte) ([]VEvent,
  error)`. Pure, table-tested against captured feed fixtures + adversarial
  inputs (folding, escaping, rejected constructs).
- `internal/feed/` (name TBD) — the importer: HTTP fetch w/ conditional GET +
  cache gate, normalize VEvent -> `*gws.Event` (managed payload + feed
  namespace props + deterministic ID), full-snapshot reconcile against the
  shadow calendar's current contents (insert/patch-on-checksum-change/delete),
  failure isolation. Reuses `gws.Client` and mirror checksum *machinery* (not
  the mirror property namespace).
- Config: new top-level `[[feeds]]` (or `[[ical]]`) array — `url`/`url_env`,
  target shadow calendar ref, optional poll cadence. Separate from `[[pairs]]`.
- Daemon wiring: importer phase before reconcile in the tick; shadow-calendar
  provisioning (create-if-missing by summary) in `install`/startup.
- Commands: surface in `config show` (redacted URL), maybe `feed list`/`feed
  test` mirroring `pair list`/`pair test`. `status` IPC could report
  last-fetch/last-success/stale per feed.

## Open decisions (need user input before writeup is final)

### Decision 1 — Scope: TripIt-profile vs generic iCal
A `url = "...ics"` config reads as "generic iCal importer" to users, which is a
support trap if the parser is TripIt-shaped. Recommend: build v1 as
TripIt-profile on purpose (strict parser, reject unsupported constructs loudly),
document it as such. Alternative: take an iCal-parser dependency and support
arbitrary feeds (breaks the stdlib-first ethos; adds RRULE/TZID/VTIMEZONE
handling and recurrence identity).

### Decision 2 — Topology: standalone fast calendar vs mirror onward
Does the imported shadow calendar just stand alone (user overlays it in Google,
replacing the slow subscription), or should it also flow into Personal/CoreWeave
via the existing mesh? Standalone makes most of the collision surface moot
(loop prevention, writable-source bounce) though we'd still build defensively.
Meshing onward is where decisions 2/3/8 above earn their keep. Current TripIt
subscription appears standalone (not meshed), so standalone is the likely answer
— confirm.

## Workflow reminders (project CLAUDE.md)

- Red-green-refactor; `mise run test` + `mise run lint` after every change.
- Mandatory `feature-dev:code-reviewer` pass before push (before AND after
  addressing findings).
- New config schema / new command surface => `feat:`. Commit-type drives the
  release. Update SPEC.md + this plan + next.md + doc/bugs.md as work lands.
- e2e: feed importer needs an e2e scenario (build tag `e2e`) against a real
  fixture calendar, served from a local test HTTP server hosting a canned .ics.
