# calendar-sync Specification

A Google Calendar event mirroring tool. Replaces the calendar-syncing piece of Reclaim.ai.

For each user-declared pair of calendars, calendar-sync mirrors busy events from one to the other (or both directions) so events on calendar A appear as private busy blocks on calendar B. Updates and deletions propagate. Recurring events mirror as recurring events with their RRULE intact, not as expanded instances.

## Design Principles

- **Google Workspace only.** No iCloud, no Outlook, no CalDAV. All Google Calendar interactions go through the `gws` CLI; calendar-sync never holds OAuth credentials directly.
- **Configuration-driven.** Every calendar pair and tunable lives in a TOML config file. No hardcoded calendars.
- **Long-running daemon, polling under the hood.** `calendar-sync watch` runs continuously under launchd `KeepAlive`. An internal scheduler ticks every `poll_interval` and uses Google's `syncToken` for cheap incremental delta lists. Webhooks (`events.watch`) are out of scope: they require a verified HTTPS domain and an always-on host with a public endpoint, neither of which fits a laptop deployment.
- **State on events. State in memory. Nothing on disk except config.** Mirror provenance lives in `extendedProperties.private` on each mirror event. Sync tokens, mirror inventories, and reconciliation state live in process memory and are rebuilt on a cold start. The only file under `~/.config/calendar-sync/` is `config.toml`.
- **Single-process serialization.** Because the daemon is a single long-running process, there's no overlap window and no need for advisory locks. Manual `calendar-sync run` invocations refuse if the daemon is loaded under launchd.
- **Idempotent.** Reconciliation logic is keyed on stable identifiers (source event IDs, mirror checksum/source_updated). A daemon crash mid-sync or a cold restart re-derives the same state from Google.
- **Edits flow both ways.** Source edits flow to the mirror (always). Mirror edits flow back to the source if the source is writable; otherwise the mirror is reverted to the source's values on the next sync cycle. The decision is determined by the source calendar's `accessRole` at config-load time. Read-only sources (subscribed iCal feeds, holiday calendars, `accessRole=reader` shares) are never written to. One carve-out: a user-created override on a recurring mirror with no source counterpart at the same recurrence time (i.e. the user dragged one occurrence to a different time on the mirror, and the source has no override at that occurrence) is not reconciled - see "Limitation: mirror-only instance overrides" below for the rationale.

## Architecture

### Components

```
                   launchd (KeepAlive)
                          |
                          v
+----------------------+     +----------------+     +---------------------+
|  calendar-sync watch | --> | gws (subproc)  | --> | Google Calendar API |
|   (long-running)     |     +----------------+     +---------------------+
+----------------------+
         |
         |  reads (config only)
         v
+----------------------------+
| ~/.config/calendar-sync/   |
|   config.toml              |
+----------------------------+

  in-memory state:
    canonical calendar IDs
    accessRole per calendar
    sync_token per pdir
    mirror inventory per target
```

calendar-sync is a Go binary that shells out to `gws calendar events ...` for every Calendar API operation. It does not use a Go SDK for Google APIs. Auth and OAuth concerns stay inside `gws` where the user has already invested setup.

The primary deployment is `calendar-sync watch`, a long-running daemon launchd starts at user login and restarts on crash via `KeepAlive`. The daemon owns all sync state in memory; it persists nothing except via mirror events on Google Calendar themselves.

`calendar-sync run` exists as a one-shot for manual catch-up, CI, and testing. It refuses to run while `watch` is loaded by launchd.

### The unit of sync: pair-direction

The atomic unit calendar-sync operates on is a **pair-direction (pdir)**: a `(pair_name, direction)` tuple where direction is `a_to_b` or `b_to_a`. A bidirectional pair expands to two pdirs. A unidirectional pair expands to one. Every piece of sync state is keyed by pdir.

This is intentional and corrects an early design where state was keyed by source calendar. Source-keyed state breaks when a single source fans out to multiple destinations: advancing the syncToken after one destination starves the others, and a partial failure on any destination loses events forever.

Per-pdir state is independent: pdir X failing doesn't prevent pdir Y from advancing its token, even if X and Y share a source calendar.

### Mirror identification

Every mirror event carries these private extended properties (`extendedProperties.private`):

| Key                            | Value example                              | Purpose                                                                                                          |
|--------------------------------|--------------------------------------------|------------------------------------------------------------------------------------------------------------------|
| `calendar-sync:source`         | `alice@example.com:abc123def456`           | `<canonical_source_calendar_id>:<source_event_id>`. Unique on a target calendar. Used to find a specific mirror. |
| `calendar-sync:source_updated` | `2026-04-29T23:00:00Z`                     | The source event's `updated` field at the time of the last reconciliation. The "did source change?" signal.      |
| `calendar-sync:checksum`       | `sha256:c3a4...e891`                       | SHA-256 over a canonical serialization of the fields calendar-sync manages on the mirror. The "did mirror drift?" signal. (See "Drift detection model" below.) |
| `calendar-sync:scope`          | `work-personal:a_to_b`                     | `<pair>:<direction>`. Composite for bulk listing by pdir. (See "Why composite" below.)                           |
| `calendar-sync:pair`           | `work-personal`                            | Pair name. Stored separately for human-readable output.                                                          |
| `calendar-sync:direction`      | `a_to_b`                                   | Direction. Stored separately for human-readable output.                                                          |
| `calendar-sync:version`        | `2`                                        | Schema version. Current version is `2`. Bump if the property layout changes.                                     |

#### Why composite `scope`

Google's events.list documentation is internally inconsistent about whether multiple `privateExtendedProperty` parameters are AND'd or OR'd. To stay on documented-and-stable ground, calendar-sync only ever queries on a **single** extended property. The composite `scope` lets us fetch all mirrors of a pdir in one single-property query without depending on AND-of-multi semantics.

#### Canonical calendar IDs

The `calendar-sync:source` property uses the **canonical** calendar ID, never the alias `primary`. At config-load time, calendar-sync resolves every calendar reference (including `primary`) to its canonical ID via `gws calendar calendarList get`. Canonicalized IDs are used everywhere downstream: in-memory state keys, extended properties, log fields. This survives config edits where the user swaps `primary` for the explicit email or vice versa.

#### Deterministic mirror event IDs

Every mirror event is inserted with a **deterministic event ID** computed from `(canonical_source_calendar_id, source_event_id)`. The ID is base32hex-encoded, prefixed `cs2`, and within Google's allowed event-ID character set (`[a-v0-9]`, length 5-1024).

```
mirror_id = "cs2" + lowercase(base32hex(sha256(canonical_source_calendar_id + ":" + source_event_id))[:50])
```

The derivation deliberately omits the pair name and direction. Event IDs are calendar-scoped on Google's side, so `(target_calendar, mirror_id)` is already unique even if the same source event were ever mirrored to multiple target calendars. Omitting `scope` from the ID derivation means renaming a pair in config doesn't change the mirror ID - the same physical mirror just gets re-tagged with the new scope value via the next reconciliation's patch.

This serves three purposes:

1. **Race-free concurrent insert.** If two processes try to mirror the same source event at the same moment (e.g. a daemon and a manual `calendar-sync run` racing through the socket-based exclusion check, or two tick handlers handling overlapping deltas), the deterministic ID makes both insert requests target the same event ID. Google rejects the second one with HTTP 409 (`reason=duplicate`); the losing process catches the conflict, fetches the existing mirror via `events.get?eventId=<computed>`, and continues as if the mirror already existed (which it now does). No duplicate mirrors, ever.
2. **Cheap mirror lookup.** Instead of always querying `events.list?privateExtendedProperty=calendar-sync:source=...` to find a mirror, the daemon computes the expected ID and either reads it from the in-memory inventory (steady state) or does an `events.get` (warm path).
3. **Stable across pair renames.** Renaming a pair from `work-personal` to `work-personal-2` in config keeps the same source-to-mirror mapping. The next reconciliation pass updates the `calendar-sync:scope`/`calendar-sync:pair` fields on each existing mirror via a normal `source_updated` patch path; no duplicate mirrors are created and no orphan cleanup is needed. (The rename does invalidate any in-memory state from the previous daemon process; restart the daemon after config changes per the documented config-reload model.)

Cancelled-and-revived: Google retains the event ID after `events.delete` for some retention window. If a mirror was deleted (orphan cleanup, `mirror prune`) and then becomes eligible again, an `events.insert` with the same deterministic ID may return 409 with `reason=duplicate` even though `events.get?eventId=<id>` shows `status=cancelled`. The daemon handles this by:

1. Insert with deterministic ID.
2. On 409 duplicate, `events.get` the ID.
3. If the existing event is `status=cancelled`, call `events.patch?eventId=<id>` with `status=confirmed` plus the full mirror payload to revive it.
4. If the existing event is alive, treat as "mirror already exists" and run the standard reconciliation.

#### Loop prevention

A source event is "already a mirror" if it carries `calendar-sync:source` in its extended properties. Such events are skipped when scanning a calendar as a *source*. This prevents bidirectional pairs from re-mirroring their own output.

#### Drift detection model

calendar-sync supports user edits on either side: edits to the source flow into the mirror (always), and edits to the mirror flow back into the source (when source is writable) or get reverted (when source is read-only, like a TripIt subscription or a holiday calendar).

Distinguishing "source changed" from "mirror was edited externally" requires recording, on the mirror itself, what the tool last wrote. Two extended properties cooperate:

- `calendar-sync:source_updated` answers **did the source change since our last write?** It records the source event's `updated` field at the moment we wrote the mirror. Comparing it to the source event's current `updated` is the "source changed" signal.
- `calendar-sync:checksum` answers **did the mirror change since our last write?** At write time we compute SHA-256 over a canonical serialization of the fields we manage on the mirror (see "Managed fields and the checksum" below) and store it on the mirror. On the next read, we recompute the hash from the mirror's current content and compare. Any difference is "mirror drifted".

Together they give us an unambiguous classification of every mirror at reconciliation time:

| `source_changed` | `mirror_drifted` | Outcome                                                                                                            |
|------------------|------------------|--------------------------------------------------------------------------------------------------------------------|
| no               | no               | `skip(reason=unchanged)`                                                                                           |
| yes              | no               | `patch` mirror from source. Reason `source_updated`.                                                                |
| no               | yes              | Drift handling. If source is writable: `propagate` mirror's edits to source. Else `revert` mirror to source values. Reason `target_edited`. |
| yes              | yes              | Conflict. Newer-wins by Google's `updated` timestamps: compare `source.updated` vs `mirror.updated`. If source is newer (or equal), `patch` mirror from source (reason `source_updated`); a `warn` log records that user edits were overwritten. If mirror is newer, drift handling as in the previous row; a `warn` log records that source updates were overwritten. |

Equal timestamps tiebreak to source. Google reports `updated` to milliseconds; concurrent edits within the same millisecond are vanishingly rare in practice.

#### Managed fields and the checksum

These are the fields calendar-sync writes when it creates or patches a mirror, and (with one exception) the fields it watches for drift. The checksum is over their canonical serialization:

- `summary` (string, possibly empty)
- `description` (string, including the `\n\n---\nSource: <htmlLink>` trailer that calendar-sync appends)
- `start` (object with `dateTime` xor `date`, plus optional `timeZone`)
- `end` (same shape as `start`)
- `recurrence` (array of RRULE/RDATE/EXDATE strings; sorted alphabetically before hashing for stability; omitted entirely for non-recurring events and for instance overrides)
- `transparency` (always `"opaque"` on a clean mirror)
- `visibility` (always `"private"` on a clean mirror)

The canonical serialization is JSON with object keys sorted, no whitespace, RFC 8259 form. The checksum value is `sha256:<hex>`.

**`reminders` is deliberately *not* in the checksum** even though we write `{"useDefault": false}` on every mirror payload. Two reasons: (1) Google does not bump the event's `updated` timestamp when only `reminders` change, so newer-wins conflict resolution would be unreliable for any drift signal that includes `reminders`. (2) Whether reminders fire on the mirror is a personal preference that's reasonable for the user to override. We set the safe default on creation; we don't fight subsequent changes.

Other fields (attendees, location, conferenceData, organizer, eventType, etc.) are also not hashed. calendar-sync doesn't manage them on the mirror, and we don't fight the user if they edit them.

#### Computing the checksum from the post-write event

The checksum is computed from the **Event resource Google returns after the write**, not from the outbound request payload. Google normalizes some fields on write (timezone canonicalization, RRULE re-formatting, whitespace folding in description, etc.). Hashing the outbound payload risks a false-drift signal on the next read because the mirror's actual stored representation differs from the request body.

Concretely, the algorithm for any insert/patch/propagate-followup write is:

1. Compute the desired payload (without `calendar-sync:checksum`).
2. Send `events.insert` or `events.patch` with that payload.
3. Read the response: it's the canonical Event resource as Google now stores it.
4. Compute the checksum over the response's managed fields.
5. Send a follow-up `events.patch` that sets only `extendedProperties.private["calendar-sync:checksum"]` to the computed hash.

This costs one extra round-trip per write, which is acceptable. The follow-up patch only touches `extendedProperties.private["calendar-sync:checksum"]`; it does not modify any managed field.

Whether Google bumps the event's `updated` timestamp on an extended-property-only patch is not explicitly documented. calendar-sync does not depend on the answer:

- The drift-detection signals use `calendar-sync:source_updated` (recorded source `updated`) and `calendar-sync:checksum` directly, not the mirror's live `updated`.
- The mirror's `updated` is only consulted in the conflict-resolution clock (newer-wins). If Google does bump `updated` on the follow-up patch, the inflation is at most a few hundred milliseconds (the latency of the follow-up RPC). A user edit that lands precisely in that window can be misattributed to "newer than our follow-up write" rather than "user-edit time"; in the absolute worst case the user's edit propagates to source even though source's update was technically newer. This is a small race window we accept rather than fight.

#### Description trailer handling

The description `propagate` writes to the source has the trailer stripped. The strip uses a strict pattern that matches the exact format calendar-sync writes: a description ending with `\n\n---\nSource: <htmlLink>` where `<htmlLink>` is `https://www.google.com/calendar/event?eid=<base64url>` (Google's auto-populated `htmlLink` format).

The recognizer regex (Go syntax): `\n\n---\nSource: https://(?:www\.google\.com|calendar\.google\.com)/calendar/event\?eid=[A-Za-z0-9_\-=]+\s*$`.

Three cases:

- **Trailer matches the regex**: strip everything from the leading `\n\n` of the trailer to end of string; propagate the remainder to source.
- **Trailer is absent** (user removed it cleanly): propagate the entire description to source.
- **Trailer is partially edited** (user mangled the trailer text or pasted text after it, breaking the regex match): the regex doesn't match. We don't attempt to recover; we propagate the entire description (with whatever fragment the user left). A `warn` log records `trailer_unrecognized` so the user can see we couldn't safely strip. The next reconciliation re-adds a fresh trailer to the mirror, so subsequent syncs return to the normal pattern.

This is a deliberate trade-off: trying to repair a partial trailer risks corrupting the source description in unpredictable ways. Surfacing the issue to the user via a warning is the safest behavior.

#### Field-level propagate

`propagate` only writes the fields that actually drifted, not the whole payload. The `events.patch` request to the source carries just those fields. This keeps fields the source has that calendar-sync doesn't manage (attendees, location, conferenceData) untouched.

#### Schema version migration

A mirror with `calendar-sync:version=1` predates the checksum property. We can't simply assume `mirror_drifted=false` on first encounter - the user may have edited the v1 mirror at some point, and we'd silently adopt that edit as the new baseline.

On first encounter of a `version=1` mirror, calendar-sync derives `mirror_drifted` by comparing the live mirror's managed fields to the desired payload computed from the source:

- `mirror_drifted = (any managed field on the mirror differs from the desired-from-source value)`.

Then the four-way matrix runs as usual:

- `!source_changed && !mirror_drifted`: no drift, just upgrade. Re-write the mirror with `version=2` and a fresh checksum. Action `patch`, reason `migration_upgrade`. The patch only touches the extended-property layout - no managed field changes.
- `!source_changed && mirror_drifted`: drift handling as normal (`propagate` or `revert`).
- `source_changed && !mirror_drifted`: `patch` from source as normal.
- `source_changed && mirror_drifted`: source wins by default during migration (more conservative than newer-wins, since v1 mirrors have no reliable user-edit timestamp). A `warn` log records `migration_source_won` so the user knows v1 mirror edits may have been overwritten.

After this single migration write, the mirror is `version=2` and subsequent reconciliations use the standard drift detection model.

## Configuration

### Location

calendar-sync looks for its configuration file at, in order:

1. `--config <path>` flag.
2. `$CALENDAR_SYNC_CONFIG` environment variable.
3. `$XDG_CONFIG_HOME/calendar-sync/config.toml` (or `~/.config/calendar-sync/config.toml` if unset).

If none exists, commands that require config exit with code 1 and a hint to run `calendar-sync init`.

### Format

TOML. Example:

```toml
# ~/.config/calendar-sync/config.toml

[settings]
poll_interval = "60s"
horizon = "365d"
full_sync_interval = "24h"
log_level = "info"
log_format = "json"

[[pairs]]
name = "work-personal"
direction = "bidirectional"
source = "alice@example.com"
target = "primary"

[[pairs]]
name = "work-family"
direction = "source_to_target"
source = "alice@example.com"
target = "family@group.calendar.google.com"
```

calendar-sync uses whatever account `gws auth status` reports. Multi-account support is out of scope; the user is responsible for ensuring the `gws`-authenticated account has appropriate access to every calendar referenced in config (typically achieved by sharing each calendar with the gws-authenticated account in Google Calendar's UI).

### Schema

#### `[settings]`

| Field                | Type     | Default | Description                                                                                                                  |
|----------------------|----------|---------|------------------------------------------------------------------------------------------------------------------------------|
| `poll_interval`      | duration | `60s`   | Internal scheduler cadence inside the daemon. Min `15s`.                                                                     |
| `horizon`            | duration | `365d`  | How far ahead to mirror. Source events with `start > now + horizon` are skipped at apply time.                               |
| `full_sync_interval` | duration | `24h`   | How often the daemon does an internal full re-sync per pdir (rebuilds source inventory, refreshes `syncToken`, catches horizon ingress). Min `1h`, max `30d`. |
| `log_level`          | string   | `info`  | One of `debug`, `info`, `warn`, `error`.                                                                                     |
| `log_format`         | string   | `json`  | One of `json` (JSONL to stderr), `text` (human-readable to stderr).                                                          |
| `dry_run`            | bool     | `false` | If true, log what would change but make no API writes. Reads still happen.                                                   |

Duration strings follow Go's `time.ParseDuration` syntax (`30s`, `5m`, `24h`) plus `d` (days) which calendar-sync adds.

#### `[[pairs]]`

| Field       | Type   | Required  | Description                                                                                                                                                  |
|-------------|--------|-----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `name`      | string | yes       | Unique. Used as the `calendar-sync:pair` extended property and in logs. Must match `^[a-z0-9][a-z0-9-]{0,62}$`.                                            |
| `direction` | string | yes       | One of `source_to_target`, `target_to_source`, `bidirectional`.                                                                                              |
| `source`    | string | yes       | Calendar ID for the "left" calendar.                                                                                                                         |
| `target`    | string | yes       | Calendar ID for the "right" calendar.                                                                                                                        |
| `enabled`   | bool   | no (true) | If false, the pair is skipped entirely.                                                                                                                      |
| `time_zone` | string | no        | IANA name (e.g. `America/New_York`). Used as the `timeZone` on mirrored events when the source event is all-day. Defaults to the destination calendar's default. |

Calendar IDs accepted: an email address (`alice@example.com`), the literal `primary` (which calendar-sync resolves to its canonical ID), or a group calendar ID (`<hash>@group.calendar.google.com`).

#### Validation rules

Run on every command that touches config. Failures exit with code 1 and a JSON error to stderr (see Output).

- `name` is unique across all pairs.
- `direction` is one of the three allowed values (case-sensitive).
- After canonicalization, `source != target`. Mirroring a calendar to itself is rejected.
- After canonicalization and pdir expansion, no two pdirs share the same `(canonical_source, canonical_target, direction)` triple. Two pdirs writing identical mirrors to the same calendar is a configuration bug.
- `poll_interval >= 15s`.
- `horizon` is between `1d` and `730d` inclusive.
- `full_sync_interval` is between `1h` and `30d` inclusive.
- `log_level` is one of the four allowed values.
- `log_format` is `json` or `text`.
- **Access role** (resolved during canonicalization via `gws calendar calendarList get` for each unique calendar reference; the response's `accessRole` field is one of `freeBusyReader`, `reader`, `writer`, `owner`):
  - The source calendar's `accessRole` is `>= reader` (i.e. `reader`, `writer`, or `owner`). `freeBusyReader` is rejected because we cannot read event details.
  - The target calendar's `accessRole` is `>= writer` (`writer` or `owner`). A read-only target means we can never write mirrors there.
  - For `direction = bidirectional`, both calendars are `>= writer` (each is a target in one of the two pdirs).
  - The pdir's `source_writable` flag (used by drift handling) is `true` iff the source's `accessRole` is `>= writer`. A `source_writable=false` pdir can still mirror events from source to target; it just `revert`s any mirror drift instead of `propagate`ing.

### Examples

#### Minimal: one bidirectional pair

```toml
[[pairs]]
name = "primary-pair"
direction = "bidirectional"
source = "alice@example.com"
target = "primary"
```

#### Three pairs, mixed directions

```toml
[settings]
poll_interval = "60s"
horizon = "365d"

[[pairs]]
name = "work-personal"
direction = "bidirectional"
source = "alice@example.com"
target = "primary"

[[pairs]]
name = "work-family"
direction = "source_to_target"
source = "alice@example.com"
target = "family@group.calendar.google.com"

[[pairs]]
name = "personal-family"
direction = "source_to_target"
source = "primary"
target = "family@group.calendar.google.com"
enabled = false
```

## Authentication

calendar-sync delegates all authentication to `gws`.

- The user runs `gws auth login` once.
- `~/.config/gws/credentials.enc` holds the encrypted token; `gws` refreshes it transparently.
- calendar-sync invokes `gws` as a subprocess. Whatever account `gws auth status` reports is what calendar-sync uses.

The user is responsible for ensuring this single `gws`-authenticated account has **appropriate access to every calendar referenced in the config** (`accessRole >= reader` for sources, `>= writer` for targets). For the typical mixed work/personal use case, this means sharing the personal Google calendar with the work account (or vice versa) via Google Calendar's calendar-sharing UI before configuring calendar-sync.

`calendar-sync watch` and `calendar-sync run` both call `gws auth status` at startup and exit with code 2 if it returns non-zero.

## Privacy and the mirror payload

By design, mirror events copy the source event's title and description verbatim. This is *not* a redaction tool. The destination calendar's writers and owners can read those details. The `visibility=private` setting only hides details from readers.

This is intentional: the user creating the pairs controls the destination calendars and the people who have writer access to them. In the typical case (mirroring between calendars the user owns), the only readers are the user and people they've explicitly shared with. If the user mirrors to a calendar with broader writer access, the leak is the user's responsibility.

There's a related caveat for **`reader`-access source calendars**: per Google's sharing model, events with `visibility=private` on a source where the user only has `reader` access have their `summary`, `description`, and other details hidden. Such events come back from `events.list` with empty or stripped fields. calendar-sync mirrors what Google returns, so a private source event on a reader-access calendar produces a mirror with an empty title and description (still marked busy and at the right time). For TripIt and other public-by-default subscriptions this isn't an issue; for shared calendars where the sharer marks events private, the mirror won't carry details. Workaround: ask the source calendar owner for `writer` access.

Redaction modes (`title_template`, `redact_description`) are out of scope for this version.

### Edits flow back too

Because mirror edits propagate to writable sources (see "Drift detection model"), edits to a mirror's title or description are **also visible to the source's other readers** after the next sync. If the user edits a work-mirrored event on their personal calendar to add private notes, those notes flow back to the work calendar where colleagues will see them. The user should treat their own mirror edits as if they were editing the source directly when the source is writable.

## Output and Logging

### stdout

Most commands produce JSONL (newline-delimited JSON) ending with a `_meta` trailer:

```
$ calendar-sync pair list
{"name":"work-personal","direction":"bidirectional","source":"alice@example.com","target":"primary","enabled":true}
{"name":"work-family","direction":"source_to_target","source":"alice@example.com","target":"family@group.calendar.google.com","enabled":true}
{"_meta":{"count":2}}
```

`_meta` is always present, even on empty results.

`run` is special: stdout is one JSON object per reconciliation action plus `_meta`. See `calendar-sync run` below.

### stderr

Diagnostic logs. Format controlled by `settings.log_format`:

- `json` (default): one JSON object per line.
  ```json
  {"ts":"2026-04-29T23:00:00Z","level":"info","msg":"sync complete","pair":"work-personal","direction":"a_to_b","duration_ms":847,"events_processed":12,"inserts":1,"patches":2,"deletes":0,"skips":9}
  ```
- `text`: human-readable.
  ```
  2026-04-29T23:00:00Z INFO sync complete pair=work-personal direction=a_to_b duration_ms=847 events_processed=12 inserts=1 patches=2 deletes=0 skips=9
  ```

Errors that prevent a command from running go to stderr as a single JSON object:

```json
{"error":"config_invalid","detail":"pair 'work-personal' has invalid direction 'left_to_right'","hint":"direction must be one of source_to_target, target_to_source, bidirectional","cause":"<wrapped, optional>"}
```

### Exit codes

| Code | Meaning              | When                                                                                          |
|------|----------------------|-----------------------------------------------------------------------------------------------|
| 0    | Success              | Command ran to completion.                                                                    |
| 1    | General error        | Config invalid, gws subprocess failed for non-auth reasons, partial sync failure.             |
| 2    | Auth error           | `gws auth status` reports unauthenticated, or 401 returned from a Calendar API call.          |
| 3    | Rate limited         | Hit retry ceiling (5 retries, exponential backoff with jitter, respects `Retry-After`).       |
| 4    | Network error        | DNS failure, connection refused, TLS error.                                                   |
| 5    | Daemon already running | `calendar-sync run` was invoked while `calendar-sync watch` is loaded under launchd.        |
| 64   | Usage error          | Unknown command, missing required flag, invalid flag value.                                   |

## Global Flags

```
--config <path>      Path to config.toml. Overrides $CALENDAR_SYNC_CONFIG and the default location.
--log-level <level>  One of debug, info, warn, error. Overrides settings.log_level.
--log-format <fmt>   One of json, text. Overrides settings.log_format.
--quiet, -q          Suppress stdout (logs still go to stderr).
--no-color           Disable ANSI colors in text-format logs.
--help, -h           Show help for the command and exit.
--version            Print version and exit.
```

Environment variables: `CALENDAR_SYNC_CONFIG`, `CALENDAR_SYNC_LOG_LEVEL`, `CALENDAR_SYNC_LOG_FORMAT`.

Precedence: CLI flag > env var > config file > built-in default.

## Commands

### calendar-sync watch

The primary deployment. Long-running daemon that owns sync state in process memory. launchd starts it at user login (`RunAtLoad=true`) and restarts it on crash (`KeepAlive=true`).

```
calendar-sync watch [flags]
  --timeout <dur>      Wall-clock cap for any single source-list or mirror-list call. Default: 5m. (Process itself runs forever.)
```

The daemon does a full startup sync, then ticks every `poll_interval` for incremental deltas, with a periodic full re-sync every `full_sync_interval` (see "Sync Algorithm"). Logs each tick's actions to stdout as JSONL and operational events to stderr.

`watch` is normally started indirectly via `calendar-sync install`, which writes the launchd plist. Running `calendar-sync watch` directly in a terminal is supported for debugging - it foregrounds the same code path.

#### Errors

Same set as `run` below, plus the daemon exits non-zero on unrecoverable startup errors (config invalid, gws_auth_failed, etc.). launchd's `KeepAlive` will restart it; if the same error persists, launchd applies its standard exponential-backoff between restarts.

### calendar-sync run

A one-shot full reconcile. Useful for: testing config changes before installing the daemon, manual catch-up, CI/automation. Equivalent to running the startup path of `watch` once and exiting.

```
calendar-sync run [flags]
  --pair <name>        Reconcile only the named pair. May be repeated. Default: all enabled pairs.
  --direction <dir>    Limit to one direction within each pair. One of a_to_b, b_to_a. Default: both where applicable.
  --dry-run            Plan and print actions but make no API writes. Reads still happen.
  --timeout <dur>      Wall-clock cap for the entire command. Default: 5m.
```

`run` refuses to start if `calendar-sync watch` is loaded under launchd for the current user (detected via `launchctl print gui/<uid>/<label>`). Override is intentional friction: stop the daemon (`calendar-sync uninstall` or `launchctl unload`) before running manual reconciles. This avoids two processes racing on the same calendar pairs.

Stdout: one JSON object per action plus `_meta`.

```
$ calendar-sync run
{"action":"insert","pair":"work-personal","direction":"a_to_b","source_event":"abc123","target_event":"def456","summary":"Standup","reason":"source_updated"}
{"action":"patch","pair":"work-personal","direction":"a_to_b","source_event":"abc124","target_event":"def457","reason":"source_updated"}
{"action":"propagate","pair":"work-personal","direction":"a_to_b","source_event":"abc126","target_event":"def460","reason":"target_edited","fields":["summary","start","end"]}
{"action":"revert","pair":"tripit-personal","direction":"a_to_b","source_event":"flight-AA-123","target_event":"def461","reason":"target_edited","fields":["summary"]}
{"action":"delete","pair":"work-personal","direction":"a_to_b","target_event":"def458","reason":"source_cancelled"}
{"action":"skip","pair":"work-personal","direction":"a_to_b","source_event":"abc125","reason":"transparency_transparent"}
{"_meta":{"pdirs":2,"events_processed":18,"inserts":1,"patches":1,"propagates":1,"reverts":1,"deletes":1,"skips":13,"duration_ms":1842}}
```

A given `reason` is paired with one of six `action` values: `insert`, `patch`, `delete`, `propagate`, `revert`, `skip`. Some reasons can produce different actions depending on the state of the mirror. The following table is exhaustive.

| `reason`                   | Possible `action` values    | Trigger                                                                                                                                  |
|----------------------------|-----------------------------|------------------------------------------------------------------------------------------------------------------------------------------|
| `is_mirror`                | `skip`                      | Source already carries `calendar-sync:source` (bidirectional loop guard).                                                                |
| `cancelled`                | `skip`                      | Source `status=cancelled` and no mirror exists to delete.                                                                                |
| `source_cancelled`         | `delete`                    | Source `status=cancelled` and a mirror exists.                                                                                           |
| `declined`                 | `skip` or `delete`          | Source calendar owner's `responseStatus=declined`. `delete` if a mirror exists, `skip` if not.                                           |
| `transparency_transparent` | `skip` or `delete`          | Source `transparency=transparent`. `delete` if a mirror exists, `skip` if not.                                                           |
| `outside_horizon`          | `skip` or `delete`          | Non-recurring source `start > now + horizon`, or recurring source has no instance in `[now, now + horizon]`. `delete` if mirror exists.  |
| `parent_not_eligible`      | `skip`                      | A recurring instance arrived but its source parent is itself filtered out (cancelled, transparent, declined, outside_horizon).           |
| `unchanged`                | `skip`                      | Both drift signals false: mirror up-to-date and unmodified relative to source.                                                            |
| `pair_disabled`            | `skip`                      | The pdir is `enabled=false`. Emitted only when the user explicitly named the pair via `--pair`.                                          |
| `instance_unmaterializable`| `skip`                      | Recurring-instance lookup returned zero results even after re-patching the mirror parent (rare; see "Zero-result instance lookup").       |
| `source_updated`           | `insert` or `patch`         | `source_changed=true && mirror_drifted=false`, or no mirror exists yet (then `insert`). Also covers `source_changed && mirror_drifted` resolved to source-wins (see `conflict_source_won` below). |
| `target_edited`            | `propagate` or `revert`     | `mirror_drifted=true && source_changed=false`, or `source_changed && mirror_drifted` resolved to mirror-wins. `propagate` if `pdir.source_writable`, else `revert`. |
| `migration_upgrade`        | `patch`                     | A v1 mirror with no source change and no drift, re-written to add the `calendar-sync:checksum` and bump `version` to 2. One-time per pre-existing mirror. |
| `orphaned`                 | `delete`                    | Prune pass found a mirror whose source no longer exists.                                                                                 |

Server-side `eventTypes` filtering means events of excluded types (`birthday`, `fromGmail`, `workingLocation`) never appear on the wire and so don't produce a `skip` event.

The recurring-instance handler uses `events.patch` (with `status=cancelled`) under the hood for cancellation cases, but at the CLI layer those still report as `action=delete`. The user-facing action describes effect, not the API primitive.

#### Conflict logging

When both `source_changed` and `mirror_drifted` are true at the same reconciliation, calendar-sync logs one extra `warn` line on stderr in addition to the normal action line on stdout:

```json
{"ts":"...","level":"warn","msg":"conflict_source_won","pair":"work-personal","direction":"a_to_b","source_event":"abc","target_event":"def","source_updated":"...","mirror_updated":"..."}
```

Possible `msg` values:

- `conflict_source_won` - both signals true, source's `updated` won (or tied) and the mirror was patched from source. The accompanying stdout action carries `reason=source_updated`.
- `conflict_target_won` - both signals true, mirror's `updated` won and drift handling fired. The accompanying stdout action carries `reason=target_edited`.
- `migration_source_won` - same as `conflict_source_won` but during the v1→v2 migration, where there was no reliable user-edit timestamp to compare against; source wins by default. The action carries `reason=source_updated`.

The `source_updated` and `mirror_updated` fields show the timestamps that drove the newer-wins decision (omitted on `migration_source_won` since v1 mirrors have no comparable timestamp), so the user can verify it was the call they wanted.

#### Daemon-running detection

`run` checks for a daemon by attempting to connect to `$TMPDIR/calendar-sync.sock` (the same socket `calendar-sync status` uses). The check works regardless of how the daemon was started (launchd-loaded, manual `calendar-sync watch` in a terminal, anything else). Three outcomes:

- **Connect succeeds**: a daemon is running. Exit 5 with `daemon_already_running` before any API calls.
- **Connect returns `ECONNREFUSED`**: the socket file exists but no process is listening. Treat as "not running" - the file is stale from a crashed daemon. `run` proceeds and the next `watch` startup will unlink the stale file before binding.
- **Socket file does not exist**: no daemon. `run` proceeds.

There's a TOCTOU window between the socket check and the start of `run`'s API calls during which a daemon could appear. The exclusion is intentionally advisory, not a hard distributed lock. The deterministic mirror event ID design (see "Mirror identification" / "Deterministic mirror event IDs") is what makes this safe: even if two processes race through the check and both attempt to mirror the same source event, both insert requests target the same Google event ID, and one gets HTTP 409 `duplicate` which is handled by fetching the existing mirror and continuing. No duplicate mirrors are ever created. The socket-based exclusion is friction to prevent obviously-redundant work, not the safety mechanism.

#### Errors

| Error code            | Exit | When                                                                                  |
|-----------------------|------|---------------------------------------------------------------------------------------|
| `config_invalid`      | 1    | Config validation failed. `detail` names the offending field.                         |
| `pair_not_found`      | 1    | `--pair <name>` doesn't match any configured pair.                                    |
| `gws_not_found`       | 1    | The `gws` binary is not on `$PATH`.                                                   |
| `gws_auth_failed`     | 2    | `gws auth status` returned non-zero.                                                  |
| `api_auth_failed`     | 2    | A Calendar API call returned 401 (also 403 with auth reasons).                        |
| `api_forbidden`       | 1    | A Calendar API call returned 403 with non-auth, non-rate-limit reasons.               |
| `rate_limited`        | 3    | 429 / 403 rateLimitExceeded / 403 userRateLimitExceeded retries exhausted.            |
| `backend_error`       | 1    | 500 / 503 retries exhausted.                                                          |
| `network_error`       | 4    | DNS, connection, or TLS failure beneath the gws subprocess.                           |
| `daemon_already_running` | 5 | The `calendar-sync watch` daemon is loaded under launchd. Stop it before running a manual reconcile. |
| `partial_failure`     | 1    | Some pdirs succeeded, others failed. `_meta.failures` lists them.                     |
| `timeout`             | 1    | Exceeded `--timeout`.                                                                 |

`run` does not abort on the first error. Each pdir is tried independently. A single pdir failure causes exit 1 at the end with `partial_failure`, but every other pdir gets its chance.

#### Examples

```
# Test config without writing
calendar-sync run --dry-run

# Manual catch-up (with daemon stopped first)
calendar-sync uninstall
calendar-sync run
calendar-sync install

# Reconcile only one pair
calendar-sync run --pair work-personal

# Reconcile only one direction of one pair
calendar-sync run --pair work-personal --direction a_to_b
```

### calendar-sync init

Generate a starter `config.toml`.

```
calendar-sync init [flags]
  --output <path>   Where to write. Default: $XDG_CONFIG_HOME/calendar-sync/config.toml.
  --force           Overwrite an existing file.
```

Stdout:

```
$ calendar-sync init
{"path":"/Users/alice/.config/calendar-sync/config.toml","status":"created"}
{"_meta":{"count":1}}
```

#### Errors

| Error code      | Exit | When                                                  |
|-----------------|------|-------------------------------------------------------|
| `config_exists` | 1    | Destination exists and `--force` was not passed.      |
| `write_failed`  | 1    | Filesystem error.                                     |

### calendar-sync config show

Print the resolved configuration as a single JSON object.

```
calendar-sync config show [flags]
  --include-defaults   Include fields that fall through to built-in defaults.
  --canonicalize       Resolve aliased calendar IDs (e.g. "primary") to their canonical IDs in the output. Requires gws.
```

Stdout:

```
$ calendar-sync config show
{"settings":{"poll_interval":"60s","horizon":"365d","full_sync_interval":"24h","log_level":"info","log_format":"json","dry_run":false},"pairs":[{"name":"work-personal","direction":"bidirectional","source":"alice@example.com","target":"primary","enabled":true}]}
{"_meta":{"count":1}}
```

#### Errors

| Error code         | Exit | When                                |
|--------------------|------|-------------------------------------|
| `config_not_found` | 1    | No config file at any search path.  |
| `config_invalid`   | 1    | Parse or validation failure.        |

### calendar-sync config validate

```
calendar-sync config validate
```

Stdout on success:

```
$ calendar-sync config validate
{"status":"ok","pairs":2,"pdirs":3}
{"_meta":{"count":1}}
```

On failure: nothing on stdout. Error to stderr.

#### Errors

| Error code         | Exit | When                                    |
|--------------------|------|-----------------------------------------|
| `config_not_found` | 1    | No config file.                         |
| `config_invalid`   | 1    | Validation failure.                     |

### calendar-sync pair list

```
calendar-sync pair list [flags]
  --enabled-only       Skip pairs with enabled=false.
```

```
$ calendar-sync pair list
{"name":"work-personal","direction":"bidirectional","source":"alice@example.com","target":"primary","enabled":true}
{"_meta":{"count":1}}
```

Errors: same as `config show`.

### calendar-sync pair test

Equivalent to `calendar-sync run --pair <name> --dry-run` for a single pair, with one extra check: it canonicalizes both calendar IDs and prints them in the output for sanity-checking.

```
calendar-sync pair test <name> [flags]
  --direction <dir>    Limit to one direction. One of a_to_b, b_to_a.
```

#### Errors

| Error code       | Exit | When                                             |
|------------------|------|--------------------------------------------------|
| `pair_not_found` | 1    | No pair with that name.                          |
| (run errors)     | -    | All errors `run` can produce.                    |

### calendar-sync mirror list

List mirror events on a calendar.

```
calendar-sync mirror list <calendar> [flags]
  --pair <name>        Only mirrors created by this pair (any direction).
  --direction <dir>    With --pair, limit to a_to_b or b_to_a.
  --orphaned           Only mirrors whose source no longer exists. Triggers per-mirror source lookup.
  --limit <n>          Max items to return. Default: 250.
  --all                Fetch all pages.
```

`--pair` + `--direction` builds the composite `scope` query (`pair:direction`) which is a single-property events.list filter.

```
$ calendar-sync mirror list primary --pair work-personal --direction a_to_b
{"id":"def456","summary":"Standup","start":"2026-04-30T15:00:00Z","end":"2026-04-30T15:30:00Z","source":"alice@example.com:abc123","source_updated":"2026-04-29T23:00:00Z","scope":"work-personal:a_to_b"}
{"_meta":{"count":1,"has_more":false}}
```

#### Errors

| Error code           | Exit | When                                               |
|----------------------|------|----------------------------------------------------|
| `calendar_not_found` | 1    | Calendar ID not accessible.                        |
| `pair_not_found`     | 1    | `--pair <name>` doesn't match.                     |

### calendar-sync mirror prune

Delete mirror events from a calendar.

```
calendar-sync mirror prune <calendar> [flags]
  --pair <name>        Only delete mirrors created by this pair.
  --direction <dir>    With --pair, limit to a_to_b or b_to_a.
  --orphaned           Only delete mirrors whose source no longer exists.
  --all                Delete every mirror calendar-sync has ever created on this calendar.
  --dry-run            List what would be deleted, do nothing.
  --yes, -y            Skip the interactive confirmation.
```

Exactly one of `--pair`, `--orphaned`, `--all` must be provided. Without `--yes`, the command prompts (TTY only).

```
$ calendar-sync mirror prune primary --orphaned --yes
{"action":"deleted","id":"def456","summary":"Standup","source":"alice@example.com:abc123"}
{"_meta":{"count":1}}
```

#### Errors

| Error code               | Exit | When                                                    |
|--------------------------|------|---------------------------------------------------------|
| `calendar_not_found`     | 1    | Calendar ID not accessible.                             |
| `pair_not_found`         | 1    | `--pair <name>` doesn't match.                          |
| `selector_required`      | 1    | None of `--pair`, `--orphaned`, `--all` provided.       |
| `confirmation_required`  | 1    | Non-TTY without `--yes`.                                |

### calendar-sync status

Report whether the daemon is reachable via its IPC socket and, if so, its current per-pdir state. "Reachable" is intentionally distinct from "loaded by launchd": if a user runs `calendar-sync watch` directly in a terminal (supported for debugging), the socket exists and `status` reports it reachable, even though launchd didn't start it.

```
calendar-sync status
```

Stdout when daemon reachable:

```
{"reachable":true,"pid":54321,"started_at":"2026-04-30T08:00:00Z","poll_interval":"60s","full_sync_interval":"24h","last_full_sync_at":"2026-04-30T08:00:00Z"}
{"pdir":"work-personal:a_to_b","source_calendar":"alice@example.com","target_calendar":"alice.personal@example.org","mirrors":1245,"last_tick_at":"2026-04-30T20:55:30Z","last_tick_status":"ok","last_tick_inserts":0,"last_tick_patches":1,"last_tick_deletes":0,"last_tick_propagates":0,"last_tick_reverts":0,"last_tick_skips":2}
{"pdir":"work-personal:b_to_a","source_calendar":"alice.personal@example.org","target_calendar":"alice@example.com","mirrors":782,"last_tick_at":"2026-04-30T20:55:30Z","last_tick_status":"ok","last_tick_inserts":0,"last_tick_patches":0,"last_tick_deletes":0,"last_tick_propagates":0,"last_tick_reverts":0,"last_tick_skips":1}
{"_meta":{"count":2}}
```

Stdout when daemon not reachable:

```
{"reachable":false}
{"_meta":{"count":0}}
```

See "IPC socket" in the State section for the full client/daemon lifecycle.

#### Errors

| Error code      | Exit | When                                                              |
|-----------------|------|-------------------------------------------------------------------|
| `socket_error`  | 1    | Socket file exists with the wrong type, wrong permissions, or other non-`ECONNREFUSED` I/O failure. |

### calendar-sync install

Install the launchd agent that runs `calendar-sync watch`.

```
calendar-sync install [flags]
  --log-dir <path>   Where launchd writes stdout/stderr. Default: ~/Library/Logs/calendar-sync/.
  --label <id>       launchd Label. Default: org.calendar-sync.agent.
  --force            Overwrite an existing plist.
  --no-load          Write the plist but don't `launchctl load` it.
```

```
$ calendar-sync install
{"plist":"/Users/alice/Library/LaunchAgents/org.calendar-sync.agent.plist","loaded":true}
{"_meta":{"count":1}}
```

The plist generated:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>org.calendar-sync.agent</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/calendar-sync</string>
        <string>watch</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>ProcessType</key><string>Interactive</string>
    <key>StandardOutPath</key><string>/Users/alice/Library/Logs/calendar-sync/calendar-sync.out.log</string>
    <key>StandardErrorPath</key><string>/Users/alice/Library/Logs/calendar-sync/calendar-sync.err.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
</dict>
</plist>
```

`KeepAlive=true` makes launchd restart the daemon if it crashes (with launchd's standard exponential-backoff between restarts). `RunAtLoad=true` starts it at user login. `ProcessType=Interactive` signals to launchd that the process behaves like an interactive (foreground) program rather than a long-running background service - this affects launchd's resource scheduling but does not prevent the OS from suspending the process during system sleep. (Sleep behavior is documented in "Sleep and wake" within the Sync Algorithm section.) There is no `StartInterval`; the daemon's own internal scheduler drives the polling cadence (see `settings.poll_interval`).

#### Errors

| Error code               | Exit | When                                                              |
|--------------------------|------|-------------------------------------------------------------------|
| `not_macos`              | 1    | Non-Darwin platform.                                              |
| `plist_exists`           | 1    | Plist exists and `--force` not set.                               |
| `launchctl_failed`       | 1    | `launchctl load` failed.                                          |
| `binary_not_resolvable`  | 1    | calendar-sync's own binary path can't be determined.              |

### calendar-sync uninstall

```
calendar-sync uninstall [flags]
  --keep-plist   Unload but don't delete the plist file.
  --label <id>   launchd Label. Default: org.calendar-sync.agent.
```

```
$ calendar-sync uninstall
{"plist":"/Users/alice/Library/LaunchAgents/org.calendar-sync.agent.plist","unloaded":true,"removed":true}
{"_meta":{"count":1}}
```

#### Errors

| Error code           | Exit | When                                                          |
|----------------------|------|---------------------------------------------------------------|
| `not_macos`          | 1    | Non-Darwin.                                                   |
| `plist_not_found`    | 1    | Nothing to remove.                                            |
| `launchctl_failed`   | 1    | `launchctl unload` failed.                                    |

### calendar-sync skill

Generate a Claude Code skill file describing the CLI for agent use. Same pattern as `slack skill`.

```
calendar-sync skill
```

Stdout: contents of `SKILL.md`.

### calendar-sync version

```
calendar-sync version [flags]
  --short   Just the version, no build metadata.
```

```
$ calendar-sync version
{"version":"0.1.0","commit":"abc1234","date":"2026-04-29T23:00:00Z","go":"go1.25"}
{"_meta":{"count":1}}
```

## Filtering

A source event is mirrored only if **all** of these are true:

- `eventType` is in `{default, outOfOffice, focusTime}`. Excludes `birthday`, `fromGmail`, `workingLocation`. Set as the `eventTypes` parameter on `events.list` so Google does the filtering server-side.
- `transparency != "transparent"`. Free events are skipped regardless of all-day-ness.
- `responseStatus != "declined"` for the source calendar's owner attendee entry. (No attendees array means the source owner is implicitly accepted - mirror.)
- Does not already carry `extendedProperties.private["calendar-sync:source"]` (loop prevention).
- `start.dateTime <= now + horizon` (or `start.date <= today + horizon` for all-day).

Any false condition is a `skip` action with the corresponding `reason`.

The `eventTypes` filter is part of the **query fingerprint** stored alongside the syncToken. If the filter changes between runs (e.g. a future config option exposes it), the saved syncToken is invalidated and a full re-list runs.

## Sync Algorithm

The atomic unit is the pdir. The algorithm runs once per pdir, independently. Failure of one pdir does not affect others.

### In-memory state

The daemon holds these in process memory only. Nothing is persisted to disk between runs. A cold start (daemon launch, restart after crash, system reboot) re-derives all of it from Google.

Per source calendar (deduplicated across pdirs that share a source):
- Canonical calendar ID
- `accessRole` (`reader` / `writer` / `owner`)
- Current `syncToken` (from the most recent `events.list` for that source)
- Last full-sync timestamp

Per target calendar (deduplicated across pdirs that share a target):
- Canonical calendar ID
- `accessRole`
- Mirror inventory: a `map[(scope, source_event_id)] -> live mirror Event resource` containing every mirror calendar-sync currently has on this calendar

Per pdir:
- Source canonical ID, target canonical ID
- `source_writable: bool` derived from the source's `accessRole`
- Direction
- Pair name

The mirror inventory and source listings are grown and pruned in place as the daemon makes inserts/patches/deletes. On a `propagate` followed by mirror re-write, the inventory entry is replaced with the fresh post-write resource.

### Daemon lifecycle: startup

`calendar-sync watch` does this exactly once at process start, and again whenever the periodic full re-sync timer fires:

1. Load and validate config from `config.toml`.
2. Run `gws auth status`. Exit code 2 on failure.
3. Resolve every distinct calendar ID referenced in config to its canonical form via `gws calendar calendarList get`. Cache `accessRole` for each.
4. Run config validation (collision rules, accessRole minimums).
5. **Full source-list per unique source.** For each distinct source calendar `S` in any enabled pdir:
   ```
   gws calendar events list --params '{
     "calendarId": "<S>",
     "timeMin": "<now (RFC3339)>",
     "timeMax": "<now + horizon (RFC3339)>",
     "singleEvents": false,
     "showDeleted": true,
     "eventTypes": ["default", "outOfOffice", "focusTime"],
     "maxResults": 250
   }' --page-all
   ```
   Capture `nextSyncToken` from the final page into a *staging* variable - not into the in-memory per-source token yet. See step 8.
6. **Mirror inventory per unique target.** For each distinct target calendar `T`, run the rebuild described in "Mirror inventory rebuild" - two `events.list` calls, one for `version=2` and one for `version=1`, merged into a single inventory. v1 entries are flagged for migration during reconciliation.
7. **Reconcile.** For each enabled pdir `(P, D)` with source `S` and target `T`, walk the in-memory list of source events for `S`. For each event, run the classification logic (see below) using the `T` mirror inventory to look up existing mirrors. Track success/failure per pdir.
8. **Commit syncTokens conditionally.** For each unique source `S`, install the staged `nextSyncToken` from step 5 into the in-memory per-source token *only if every pdir whose source matches `S` succeeded in step 7*. If any pdir for `S` failed, leave the in-memory token empty so the next cycle re-runs a full source-list for `S`. This is the same conditional-advancement rule that protects the per-tick path; both paths apply it.

   If the staged token is missing (Google can omit `nextSyncToken` on very long full lists) the same rule applies in spirit: leave the in-memory token empty so the next cycle re-runs a full source-list.
9. **Schedule.** Set the per-tick timer (`poll_interval`) and the periodic-full-resync timer (`full_sync_interval`).

Startup wall-clock cost on real-world calendars (1-year horizon, ~1000 events per source): on the order of 10-20s for a typical multi-pdir setup. Mostly Google API latency.

### Daemon lifecycle: per-tick reconciliation

Every `poll_interval`, the internal scheduler fires the per-tick path. For each unique source `S`:

1. **Incremental delta.**
   ```
   gws calendar events list --params '{
     "calendarId": "<S>",
     "syncToken": "<in-memory token for S>",
     "showDeleted": true,
     "eventTypes": ["default", "outOfOffice", "focusTime"],
     "maxResults": 250
   }' --page-all
   ```
   `timeMin`, `timeMax`, `updatedMin`, `q`, `iCalUID`, `orderBy`, and the `privateExtendedProperty`/`sharedExtendedProperty` filters are all rejected when sent alongside `syncToken`. Other parameters (`singleEvents`, `eventTypes`, `showDeleted`) must match the values used in the prior full sync. The daemon never changes these between full and incremental calls, so they always match.
2. **410 GONE recovery.** If any page returns 410, the in-memory token is invalid. Schedule an immediate full re-sync for `S` (which gets a fresh token), and skip the rest of this tick for that source.
3. **Reconcile delta to every dependent pdir.** For each event `E` in the response, for each enabled pdir whose source matches `S`, run the classification logic against the in-memory mirror inventory for that pdir's target. Track success/failure per pdir.
4. **Conditionally update token.** Replace the in-memory `syncToken` for `S` with the response's `nextSyncToken` *only if every pdir whose source matches `S` successfully processed every event in the delta*. If any pdir failed (a Calendar API error, rate-limit retries exhausted, etc.), leave the in-memory token unchanged. The next tick re-fetches the same delta and re-reconciles. Idempotency in the classification logic (the `unchanged` skip when `source_changed=false && mirror_drifted=false`) means successful pdirs from the previous tick re-do their work as no-ops.

The conditional advancement is what protects against the original "source-keyed state loses events on partial failure" bug: a failed pdir prevents the source's token from moving past events it didn't process, so the next tick re-delivers them.

Empty deltas (the common case) cost a single API call per source - measured at ~270ms for an empty incremental response.

### Daemon lifecycle: periodic full re-sync

Every `full_sync_interval` (default 24h), the daemon repeats the source-list / inventory-rebuild / reconcile work of startup, plus a follow-up orphan-cleanup walk that doesn't run during the per-tick path. The full re-sync does NOT re-read `config.toml` from disk: a parsed config snapshot is captured at daemon startup and reused for the daemon's lifetime. Config edits require a daemon restart.

What full re-sync does refresh:

1. Re-canonicalize calendar IDs (in case Google reassigned a primary, though rare) and re-fetch each calendar's `accessRole` via `gws calendar calendarList get`. If a target's `accessRole` has dropped below `writer`, log an error and skip pdirs that target it for the rest of this re-sync. If a source's `accessRole` has changed, recompute the corresponding pdirs' `source_writable` flag (drift handling switches between propagate and revert accordingly).
2. Full source-list per unique source. Replace the in-memory source listings.
3. Mirror inventory rebuild per unique target (see "Mirror inventory rebuild" below).
4. Reconcile every source event through the classification logic against the rebuilt inventory.
5. **Orphan walk.** For each mirror inventory entry whose `(scope, source_event_id)` was *not* visited in step 4 (i.e. its source wasn't returned by the full source-list), look up the source via `events.get?calendarId=<S>&eventId=<source_id>`:
   - **Source returns 404 or has `status=cancelled`**: delete the mirror. Action `delete`, reason `orphaned`.
   - **Source is non-recurring and `start > now + horizon`**: delete the mirror. Action `delete`, reason `outside_horizon`.
   - **Source is a recurring parent (has `recurrence`)**: don't trust `start` (which is the series start). Call `events.instances?calendarId=<S>&eventId=<source_id>&timeMin=<now>&timeMax=<now + horizon>&maxResults=1&showDeleted=false`. Zero instances: delete the mirror, reason `outside_horizon`.
   - **Source is alive and in horizon but was filtered** (eventType excluded by Google's server-side filter, transparency=transparent, declined, etc.): delete the mirror, reason `source_filtered`. The fact that the source exists but doesn't match our query means it's no longer eligible for mirroring.

Step 5 closes the gap that the per-tick path can't: incremental deltas via `syncToken` carry `status=cancelled` for source deletions only when the daemon was up to receive them. If the daemon was down (laptop closed, system rebooted) when a source was deleted, the cancellation event is consumed and lost - the next incremental delta never sees it. Periodic full re-sync's orphan walk catches what the per-tick path missed.

What this catches:

- **Horizon ingress.** Events that crossed into `[now, now + horizon]` simply by passage of time, without changing. Incremental sync wouldn't return them; full sync does.
- **Mirror drift on currently-eligible source events.** A user edited a mirror but its source hasn't changed since the last delta. The incremental delta wouldn't include the source event, so the classification logic never had a chance to detect drift. Full sync visits every source event, checks every mirror, catches the drift.
- **Orphans.** Mirrors whose source was deleted, moved beyond horizon, or made ineligible (transparency, declined) while the daemon was down. The orphan walk in step 5 handles these.
- **`accessRole` changes on calendars.** Step 1 re-fetches access roles. A target that lost writer access stops accepting writes; a source that gained writer access starts having its mirror drift propagated.

What this does NOT catch (still documented limitations):

- **Mirror-only recurring instance overrides** (a user-created override at a recurrence time the source has no override for). See the dedicated limitation section.
- **Config changes.** Editing `config.toml` while the daemon is running has no effect; restart the daemon (`calendar-sync uninstall && calendar-sync install`) for changes to take effect.

After each full re-sync the in-memory inventories are replaced atomically.

#### Mirror inventory rebuild

For each unique target, the rebuild runs two `events.list` calls in sequence:

1. `privateExtendedProperty=calendar-sync:version=2` to find current-schema mirrors.
2. `privateExtendedProperty=calendar-sync:version=1` to find legacy mirrors that haven't been migrated yet.

Both responses are merged into the single in-memory inventory. v1 entries are flagged for migration; on first reconciliation each gets re-written with `version=2` and a fresh `calendar-sync:checksum` per the schema-migration rules.

Without the v1 query, mirrors that were inserted before the schema bump would never appear in inventory and would never be reconciled or cleaned up - they'd become permanent zombies.

### Classification logic

This runs once per source event `E` per pdir `(P, D)`. Called from both startup (over the full source list) and per-tick (over the delta).

1. **Already a mirror.** If `E.extendedProperties.private["calendar-sync:source"]` is set, `skip(reason=is_mirror)`. This is the bidirectional loop guard.

2. **Recurring instance.** If `E.recurringEventId` is set, route to the recurring-instance handler (see "Recurring Events"). The handler internally deals with cancelled, transparent, declined, and updated instances. Generic skip rules in steps 3-6 do NOT apply to recurring instances - the handler subsumes them.

3. **Cancelled (non-recurring).** If `E.status == "cancelled"` (and step 2 didn't fire), look up the mirror in the inventory by `(scope = "<P>:<D>", source_event_id = E.id)`. If found, delete it (action `delete`, reason `source_cancelled`). If not, `skip(reason=cancelled)`.

4. **Declined.** If the source calendar owner's attendee entry has `responseStatus=declined`, `skip(reason=declined)`. (Plus delete the mirror if one exists.)

5. **Transparent.** If `E.transparency == "transparent"`, `skip(reason=transparency_transparent)`. (Plus delete the mirror if one exists.)

6. **Outside horizon.** Compute the horizon-eligibility for this event:
   - For non-recurring: the event is in horizon if `start <= now + horizon` (where `start = E.start.dateTime || E.start.date`).
   - For recurring parents (`E.recurrence` is set): the event is in horizon if **any instance falls in `[now, now + horizon]`**. To check, call `events.instances?eventId=<E.id>&calendarId=<S>&timeMin=<now>&timeMax=<now + horizon>&maxResults=1&showDeleted=false`. If the response has at least one instance, the parent is in horizon. If empty, treat as outside_horizon.

   If outside horizon: `skip(reason=outside_horizon)` and delete the mirror if one exists.

7. **Normal reconciliation.** Look up the mirror in the inventory by `(scope, source_event_id)`:
   - **No mirror**: `events.insert` on `T` with the full mirror payload. Action `insert`, reason `source_updated`. Add the post-write resource to the inventory.
   - **Mirror exists**: compute the two drift-detection signals from "Drift detection model":
     - `source_changed = E.updated > mirror.calendar-sync:source_updated`
     - `mirror_drifted = sha256(canonical(mirror.<managed fields>)) != mirror.calendar-sync:checksum`. If `version < 2` on the mirror, derive `mirror_drifted` per the "Schema version migration" rules instead.

     Apply the four-way outcome table:
     - `!source_changed && !mirror_drifted`: `skip(reason=unchanged)`.
     - `source_changed && !mirror_drifted`: `events.patch` on `T` with the full mirror payload. Action `patch`, reason `source_updated`. Replace the inventory entry with the post-write resource.
     - `!source_changed && mirror_drifted`: drift handling (see below). If `pdir.source_writable`: action `propagate`, reason `target_edited`. Else: action `revert`, reason `target_edited`.
     - `source_changed && mirror_drifted`: newer-wins. Compare `E.updated` to the mirror inventory entry's `updated` field. Source wins on ties.
       - Source newer (or equal): action `patch`, reason `source_updated`. A `warn` log records `conflict_source_won`.
       - Mirror newer: drift handling as above. A `warn` log records `conflict_target_won`.

#### Drift handling

When `propagate` is selected:

1. Compute the desired payload from the source.
2. For each managed field, determine if the live mirror's value differs from the desired value. The set of fields that differ is the "drifted set."
3. For `description`, strip the `\n\n---\nSource: ` trailer from the mirror's value (per the strict regex in "Description trailer handling") before adding it to the patch.
4. `events.patch` on the **source calendar** with `eventId=<source.id>`, body containing only the drifted fields' values.
5. After Google returns the patched source, take its new `updated` timestamp and re-write the mirror with the full mirror payload, the new `calendar-sync:source_updated`, and a fresh `calendar-sync:checksum`. Replace the inventory entry with the post-write resource.

When `revert` is selected:

1. `events.patch` on the **target calendar** with the full mirror payload, fresh `calendar-sync:checksum`, and the existing `calendar-sync:source_updated` (the source hasn't changed).
2. Replace the inventory entry with the post-write resource.

In both cases the action JSON includes a `fields` array listing the drifted field names:

```json
{"action":"propagate","pair":"work-personal","direction":"a_to_b","source_event":"abc","target_event":"def","reason":"target_edited","fields":["summary","start","end"]}
{"action":"revert","pair":"tripit-personal","direction":"a_to_b","source_event":"xyz","target_event":"def","reason":"target_edited","fields":["summary"]}
```

### Limitation: mirror-only recurring instance overrides

If a user moves or cancels an *individual occurrence* of a recurring mirror (creating an override on the mirror side that has no source counterpart), the daemon does not detect or reconcile it. The reasons:

- The classification logic iterates over source events; with no corresponding source override at the same `originalStartTime`, there's nothing to compare against.
- Listing all materialized instances on the mirror side via `events.instances` doesn't cleanly distinguish user-modified overrides from auto-generated occurrences without round-trip comparison against the parent's RRULE projection - and Google does not document a "delete this override to restore the auto-generated occurrence" semantic, so a `revert` primitive isn't safely available.
- Drift on the recurring **parent** itself (summary, description, start/end, recurrence rule, etc.) IS detected by the standard reconciliation path. Drift on instances that the source also overrides IS detected by the recurring-instance handler.

Consequence: a mirror-only instance override persists until either (a) the source's parent recurrence changes (which triggers a parent re-reconciliation, but doesn't directly clean up the override), (b) the user manually deletes it, or (c) the user runs `calendar-sync mirror prune <calendar> --pair <name>` to remove all of that pdir's mirrors and let them regenerate.

The standard drift detection covers parents and source-corresponding instances, which together cover the vast majority of practical edits.

### Mirror event payload (insert and patch)

```json
{
  "summary": "<source.summary>",
  "description": "<source.description>\n\n---\nSource: <source.htmlLink>",
  "start": <source.start>,
  "end": <source.end>,
  "transparency": "opaque",
  "visibility": "private",
  "recurrence": <source.recurrence>,
  "reminders": { "useDefault": false },
  "extendedProperties": {
    "private": {
      "calendar-sync:source": "<S>:<E.id>",
      "calendar-sync:source_updated": "<E.updated>",
      "calendar-sync:checksum": "sha256:<hex>",
      "calendar-sync:scope": "<P>:<D>",
      "calendar-sync:pair": "<P>",
      "calendar-sync:direction": "<D>",
      "calendar-sync:version": "2"
    }
  }
}
```

Notes:
- `summary` and `description` are copied verbatim.
- `description` always ends with a blank line, `---`, and `Source: <htmlLink>`. If source description is empty, just the trailer.
- `start`, `end` preserve the source's `dateTime`/`date` distinction and `timeZone`.
- `transparency` forced to `opaque`. `visibility` forced to `private`.
- `recurrence` is the source's array of RRULE/RDATE/EXDATE strings, omitted for non-recurring events. Omitted on mirror *instances* even when the parent has it.
- `reminders.useDefault = false` is mandatory. Omitting it lets the destination calendar's default reminders fire on every mirror, which is wrong: mirrors should be silent.
- `attendees`, `location`, `conferenceData` are deliberately omitted.
- `calendar-sync:checksum` is set by a follow-up `events.patch` after the main write, using the post-write Event resource as the input to the hash. See "Drift detection model" / "Computing the checksum from the post-write event" for the algorithm and rationale.

The `propagate` action uses a **different payload shape**: only the drifted managed fields, with the description trailer stripped, written to the **source** event. The mirror is then re-written separately with the full payload above and a fresh checksum derived from the new source state.

### Sleep and wake

macOS pauses long-running launchd processes (including those with `ProcessType=Interactive`) when the system sleeps and resumes them on wake. The in-memory state - syncTokens, mirror inventories, accessRoles, scheduler timers - survives sleep/wake intact. No special handling is needed for short sleeps.

The daemon's internal scheduler is wall-clock-driven, not monotonic-clock-driven: each tick is computed as `next_tick = now.Truncate(poll_interval).Add(poll_interval)`, and similarly for the periodic-full-resync timer. This means after a sleep that crosses one or more tick boundaries:

- The next tick fires immediately on wake (the wall-clock-derived next-tick time is already in the past).
- The periodic full re-sync fires immediately on wake if the gap since the last completed full re-sync exceeds `full_sync_interval`.

Result: a laptop that slept overnight wakes up to a single catch-up tick, and (if the sleep crossed `full_sync_interval`) a full re-sync follows. The catch-up tick uses the in-memory syncToken, which is normally still valid (Google's syncTokens have a tolerance of roughly a week before they're revoked). If the syncToken is stale, the standard 410 GONE recovery path triggers an immediate full re-sync for that source.

A tick that was mid-flight when sleep hit (e.g., laptop closed during an `events.list` call) resumes from where it left off - the HTTPS connection may have been broken, in which case gws's transport layer surfaces a network error, and the daemon logs the failure and proceeds. The classification logic for events processed before the failure has already updated the in-memory mirror inventory; the affected pdir's syncToken stays at the pre-tick value (per the conditional-advancement rule), so the next tick re-fetches the delta and re-reconciles.

### Concurrency

- Source-list and mirror-inventory builds during startup or periodic full re-sync run sequentially per source/target. Parallelization is not done; wall-clock is dominated by Google API latency, not local CPU.
- During a single tick or startup pass, events are processed serially. Each event's reconciliation is a single critical section over the in-memory inventory.
- Orphan-detection lookups (when a mirror's source ID isn't found in the source list and we need to confirm via `events.get`) fan out with a semaphore of 5.
- The daemon is single-process by design; there's no in-process scheduler concurrency between ticks (a tick won't fire while the previous tick is still running - the scheduler holds the next tick until the current one completes).

### Idempotency

Every API call is keyed by stable identifiers (source event ID for find, mirror event ID for patch/delete). The mirror's `calendar-sync:source` extended property is the de-duplication key: a previous reconciliation that crashed mid-insert leaves either no mirror (next pass inserts) or a mirror with the marker (next pass patches/skips). No duplicates.

A daemon crash mid-tick is recoverable: launchd's `KeepAlive` restarts the process, the cold-start path rebuilds inventories from Google, and reconciliation converges on the same end state.

A manual `calendar-sync run` and the daemon don't interleave because `run` refuses if the daemon is loaded under launchd.

## Recurring Events

Recurring events mirror as recurring events. The source's `recurrence` array (RRULE/RDATE/EXDATE per RFC 5545) is copied verbatim onto the mirror. Modified and cancelled instances mirror as instance overrides on the mirror series.

### Parent-only recurring events

A recurring source event with no overrides comes back from `events.list?singleEvents=false` exactly once - the parent. It's reconciled exactly like a non-recurring event.

### The recurring-instance handler

When the classification logic's recurring-instance branch routes here, this is the full reconciliation algorithm. It internally handles cancelled, transparent, declined, and modified cases - all the skip rules that the early-branch bypassed for the generic path.

#### Step 1: find or repair the mirror parent

Look up the mirror parent by `calendar-sync:source = "<S>:<E.recurringEventId>"` on `T`.

If absent (incremental sync can return only the exception when only the exception changed), fetch the source parent via `events.get?calendarId=<S>&eventId=<E.recurringEventId>` and reconcile it through the classification logic's normal-reconciliation step. After that succeeds, the mirror parent exists and we proceed.

If the source parent is now ineligible (status=cancelled, transparency=transparent, declined, or outside_horizon when checked via `events.instances`), record `skip(reason=parent_not_eligible)` and stop. The mirror parent isn't created, and any prior mirror instance for this exception will be cleaned up by the orphan-prune pass.

#### Step 2: locate the mirror instance

Compute the `originalStart` query value from the source exception:
- If `E.originalStartTime.dateTime` is set, use that (a timezone-aware RFC 3339 string).
- Else `E.originalStartTime.date` is set (an all-day exception, `YYYY-MM-DD`).

Call `events.instances?calendarId=<T>&eventId=<mirror_parent.id>&originalStart=<value>&maxResults=1&showDeleted=true`. (showDeleted=true is needed so a previously-cancelled instance still resolves; we may need to "uncancel" it.)

##### Zero-result instance lookup

If the response is empty, the mirror parent's recurrence rule doesn't generate an instance at `originalStart`. This is rare but real - it can happen when the mirror parent's `recurrence` is stale relative to the source parent's, or when an exception's `originalStartTime` doesn't match the new RRULE after a series-rule change.

Repair path:
1. Fetch the source parent via `events.get?calendarId=<S>&eventId=<E.recurringEventId>`.
2. Force-patch the mirror parent with the source parent's current state (full mirror payload, `reason=source_updated`), refreshing its `recurrence`.
3. Retry the `events.instances` lookup.
4. If still empty, the exception falls outside the mirror's recurrence even after refresh. `skip(reason=instance_unmaterializable)` and log at level `warn` with both the source parent's and mirror parent's recurrence arrays. The next full sync (within `full_sync_interval`) re-checks the parent and usually self-heals.

#### Step 3: decide insert/patch/delete/propagate/revert

Apply these rules in order to the source exception `E`. The "user-facing action" reported on stdout is shown alongside.

1. **Cancelled** - If `E.status == "cancelled"`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise call `events.patch` on the mirror instance with `{"status": "cancelled"}`. Action `delete`, reason `source_cancelled`. The API primitive is `patch`, but the effect on the mirror is removal of the busy block, so we report `delete`.
2. **Declined** - If the source calendar owner's attendee entry on `E` has `responseStatus=declined`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise patch the mirror instance to `status=cancelled`. Action `delete`, reason `declined`.
3. **Transparent** - If `E.transparency == "transparent"`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise patch the mirror instance to `status=cancelled`. Action `delete`, reason `transparency_transparent`.
4. **Drift detection on the instance.** Compute the same two signals as for non-recurring events, but on the instance:
   - `source_changed = E.updated > mirror_instance.calendar-sync:source_updated`
   - `mirror_drifted = sha256(canonical(mirror_instance.<managed fields>)) != mirror_instance.calendar-sync:checksum`. If `version < 2` on the mirror instance, derive `mirror_drifted` per the "Schema version migration" rules instead (compare live managed fields to desired-from-source).

   Apply the four-way matrix:
   - `!source_changed && !mirror_drifted`: `skip(reason=unchanged)`.
   - `source_changed && !mirror_drifted`: `events.patch` the mirror instance with the full mirror-instance payload. Action `patch`, reason `source_updated`.
   - `!source_changed && mirror_drifted`: drift handling. The source instance always exists in this frame (we got here because the source-list call with `singleEvents=false` returned `E` as a source override). If `pdir.source_writable`, `events.patch` the **source instance** with the drifted fields. Action `propagate`, reason `target_edited`. Then re-write the mirror instance with the full payload, fresh checksum, and the source instance's new `updated`. Else `events.patch` the mirror instance to overwrite the user's edits. Action `revert`, reason `target_edited`.
   - `source_changed && mirror_drifted`: newer-wins by `E.updated` vs `mirror_instance.updated`. Source wins on equal. Source-newer: action `patch`, reason `source_updated`, with a `warn` log `conflict_source_won`. Mirror-newer: drift handling as above, with a `warn` log `conflict_target_won`.

The mirror payload for an instance patch (used by `patch`, `revert`, and the post-`propagate` re-write) is the same shape as for non-recurring events with two differences:

- `recurrence` is omitted (instances don't have their own recurrence; they belong to the parent).
- `extendedProperties.private["calendar-sync:source"]` is `"<S>:<E.id>"` (the exception's own ID, not the parent's) so a direct lookup later finds the right instance.

The checksum on a mirror instance is computed over the same managed fields as on a non-recurring event, with `recurrence` always omitted.

#### Mirror-only instance overrides (limitation)

If the user creates an override on the mirror side with no source counterpart (e.g. dragging one occurrence of a recurring mirror to a new time), the daemon does not reconcile it. See "Limitation: mirror-only recurring instance overrides" in the Sync Algorithm section for the full justification. Drift on the recurring parent and on instances that the source also overrides is fully detected.

#### What this handler does *not* cover

- The source parent itself when it changes. That's the classification logic's normal-reconciliation step. The handler only gets called for instances (events with `recurringEventId` set).
- Source-level recurrence rule changes. Those reach normal reconciliation as a `source_updated` patch on the parent. See "Parent recurrence rule changes" below.

### Parent recurrence rule changes

When a source parent's `recurrence` array changes, the classification logic patches the mirror parent with the new recurrence as part of a regular `source_updated` patch. That's the entire reconciliation step for the parent.

There is no separate instance-fanout pass on a recurrence change. Justification:

- Auto-generated instances (those with no override) are not standalone resources. They're materialized by the parent's recurrence rule. When the parent's `recurrence` changes, the set of auto-generated instances changes for free, on both the source and the mirror.
- Standalone instance resources (modified or cancelled overrides) are independent events that persist through a parent rule change. Source-side they continue to live as overrides; mirror-side our existing overrides also continue to live. If a source override is at a recurrence time the new rule no longer generates, Google keeps it as an attached one-off; the mirror's override behaves the same. This is consistent with how Google itself behaves.
- Future changes to source overrides arrive via the regular incremental stream and are reconciled by the recurring-instance handler. No special pass needed at recurrence-change time.

The `events.instances` endpoint returns all materialized instances (override and synthetic) within an optional time window; it's used in this spec only for the bounded targeted lookup in modified-instance reconciliation, not for unbounded scans during recurrence-change handling.

### Parent timezone changes

A source parent whose `start.timeZone` (or whose recurrence implicit timezone via `DTSTART;TZID=`) changes is reconciled by the same parent-patch path. The mirror parent's `start.timeZone` is updated alongside `recurrence` whenever the classification logic detects a source-updated patch.

### `UNTIL` clauses past the horizon

Source recurrences with an `UNTIL=<date>` clause copy through unchanged. If `UNTIL` is past `now + horizon`, the mirror has the same UNTIL. We don't truncate - the horizon is a query-time filter, not an ongoing constraint on mirror events.

## Error Conditions

Complete list of error codes:

| Error code               | Exit | Layer            | Trigger                                                                                              |
|--------------------------|------|------------------|------------------------------------------------------------------------------------------------------|
| `config_not_found`       | 1    | Config           | No config file at any search path.                                                                   |
| `config_invalid`         | 1    | Config           | Parse error or validation failure.                                                                   |
| `config_exists`          | 1    | `init`           | `init` target exists and `--force` not set.                                                          |
| `pair_not_found`         | 1    | CLI              | `--pair <name>` does not match any configured pair.                                                  |
| `pdir_not_found`         | 1    | CLI              | `--pdir <id>` does not match any configured pdir.                                                    |
| `calendar_not_found`     | 1    | API              | Calendar ID returned 404.                                                                            |
| `calendar_canonicalize_failed` | 1 | API           | Could not resolve a configured calendar ID (including `primary`) to its canonical ID.                |
| `access_role_insufficient` | 1 | Config         | A calendar's `accessRole` is too low for its declared role (`reader` for a target, or `freeBusyReader` for either side, or `bidirectional` with one side `<= reader`). |
| `selector_required`      | 1    | `mirror prune`   | None of `--pair`, `--orphaned`, `--all` provided.                                                    |
| `confirmation_required`  | 1    | Interactive      | Non-TTY and `--yes` not provided for a destructive command.                                          |
| `gws_not_found`          | 1    | Subprocess       | `gws` not on `$PATH`.                                                                                |
| `gws_auth_failed`        | 2    | Subprocess       | `gws auth status` returned non-zero.                                                                 |
| `api_auth_failed`        | 2    | API              | Calendar API returned 401, or 403 with auth-related reason.                                          |
| `api_invalid_request`    | 1    | API              | Calendar API returned 400 (most often a malformed payload - bug).                                    |
| `api_not_found`          | 1    | API              | Calendar API returned 404 for an event or calendar that should exist.                                |
| `api_conflict`           | 1    | API              | Calendar API returned 409 (concurrent edit).                                                         |
| `api_forbidden`          | 1    | API              | Calendar API returned 403 with a non-auth, non-rate-limit reason (e.g. read-only calendar).          |
| `rate_limited`           | 3    | API              | Retries exhausted on 429, 403 `rateLimitExceeded`, or 403 `userRateLimitExceeded`.                   |
| `backend_error`          | 1    | API              | Retries exhausted on 500 or 503.                                                                     |
| `network_error`          | 4    | Subprocess       | gws reports a network failure.                                                                       |
| `partial_failure`        | 1    | `run`/`watch`    | At least one pdir failed but others succeeded. Final exit; details in `_meta.failures`.              |
| `not_macos`              | 1    | install/uninstall| Running on Linux/Windows.                                                                            |
| `plist_exists`           | 1    | install          | Plist exists and `--force` not set.                                                                  |
| `plist_not_found`        | 1    | uninstall        | Plist not present.                                                                                   |
| `launchctl_failed`       | 1    | install/uninstall| `launchctl load` or `unload` failed.                                                                 |
| `binary_not_resolvable`  | 1    | install          | calendar-sync's own binary path can't be determined.                                                 |
| `write_failed`           | 1    | init             | Filesystem error writing the starter config.                                                         |
| `timeout`                | 1    | run              | Exceeded `--timeout`.                                                                                |
| `daemon_already_running` | 5    | run              | The `watch` daemon is loaded under launchd. Stop it before running a manual reconcile.               |
| `socket_error`           | 1    | status           | Socket file exists but I/O failed for non-`ECONNREFUSED` reasons.                                    |

### Error format on stderr

```json
{"error":"<code>","detail":"<human message>","hint":"<remediation>","cause":"<wrapped lower-level message, optional>"}
```

`cause` is included when the error wraps a subprocess or API failure that has its own message worth preserving.

### Retry policy

The API layer retries on these statuses with exponential backoff (1s, 2s, 4s, 8s, 16s, capped by `Retry-After` if present), 5 attempts max, with jitter:

- **429** (any reason).
- **403** with reason `rateLimitExceeded` or `userRateLimitExceeded` (Google sometimes returns 403 instead of 429 for per-user quotas).
- **500** Internal Server Error.
- **503** Service Unavailable.

After exhaustion: `rate_limited` (exit 3) for the 429/403 cases, `backend_error` (exit 1) for 500/503.

Other 4xx are not retried. Network errors (DNS, TCP, TLS) bubble up as `network_error` (exit 4); the gws subprocess handles its own retries on those internally.

Each retry is logged at level `warn`:

```json
{"ts":"...","level":"warn","msg":"retrying","endpoint":"events.list","status":429,"reason":"rateLimitExceeded","attempt":2,"wait_ms":2000}
```

### gws subprocess error mapping

| gws exit | calendar-sync error  | Exit |
|----------|----------------------|------|
| 0        | (success)            | 0    |
| 2        | `gws_auth_failed`    | 2    |
| 3        | `network_error`      | 4    |
| 4        | `rate_limited`       | 3    |
| other    | `api_invalid_request` | 1   |

If `gws` writes a JSON error to stderr (it does, on most failures), that message is parsed and surfaced as `cause`.

### Partial failure semantics

`run` does not abort on the first error. Execution model:

```
refuse if `calendar-sync watch` is loaded (exit 5, daemon_already_running)
load and validate config
canonicalize calendar IDs
expand pairs to pdirs (skip enabled=false)
for each pdir P (deterministic order):
    try sync(P)
    on error: failures.append((P, error)); continue
if failures:
    emit partial_failure to stderr
    exit 1
else:
    exit 0
```

Each pdir's syncToken is advanced independently on its own success per the conditional-advancement rule.

## State

calendar-sync persists nothing to disk except `config.toml`. All sync state lives in process memory inside `calendar-sync watch`. There's no `state.json`, no lock file, no per-pdir checkpoint file.

What lives in process memory:

- Canonical calendar IDs and `accessRole`s for every calendar referenced in config (resolved at startup).
- Per-source `syncToken` (one token per unique source calendar; pdirs sharing a source share the token).
- Per-source last-full-sync timestamp.
- Per-target mirror inventory (a map from `(scope, source_event_id)` to live Event resource).

What survives across daemon restarts:

- `extendedProperties.private` on every mirror event. The full provenance (`calendar-sync:source`, `calendar-sync:source_updated`, `calendar-sync:checksum`, `calendar-sync:scope`, etc.) is colocated with the mirror itself on Google Calendar. A cold start re-derives the in-memory inventory by running the "Mirror inventory rebuild" subroutine (two `events.list` calls per target: one for `version=2`, one for `version=1`).

A cold start (process launch, restart after crash, system reboot) walks the same path described in "Daemon lifecycle: startup": canonicalize, list, build inventory, reconcile. Wall-clock cost on a real-world calendar setup is on the order of 10-20 seconds.

### IPC socket

The daemon binds a Unix domain socket at `$TMPDIR/calendar-sync.sock` for the `calendar-sync status` command to query live state. macOS sets `$TMPDIR` per-user, so the path is naturally scoped to one user's daemon.

#### Daemon-side lifecycle

On `watch` startup, the bind sequence is:

1. `stat()` the socket path. If it doesn't exist, proceed to step 4.
2. If it exists, attempt to connect to it.
3. **Connect succeeds** - another daemon is already running. Exit with `daemon_already_running` (a paranoia guard; launchd's `KeepAlive` should not normally produce this since it manages a single instance).
4. **Connect returns `ECONNREFUSED` or the stat returned a non-socket file type** - the file is stale from a crashed daemon (or, weirdly, a non-socket file at the path). `unlink()` it.
5. `bind()` and `listen()` on the socket path.

On clean shutdown (SIGTERM or SIGINT received), the daemon `unlink()`s the socket before exiting. On crash (SIGKILL, panic, OS kill), the socket file remains until the next startup's stale-cleanup at step 4.

If the path exists but is a non-socket file with the wrong owner or permissions, `unlink` may fail with `EACCES`/`EPERM`. The daemon logs this and exits 1; the user must remove the offending file manually. This shouldn't happen in practice since `$TMPDIR` is per-user.

#### Client-side lifecycle (`status`)

`calendar-sync status` connects to the socket and treats `ECONNREFUSED` (or the absence of the file) as "daemon not reachable":

```json
{"reachable":false}
{"_meta":{"count":0}}
```

On other I/O errors (permission denied, stale stale socket file with wrong type), exits 1 with `socket_error`.

The socket carries no persistent state. Its existence indicates the daemon is currently running; nothing more.

### Mirror identification queries (recap)

Find a specific mirror:
```
events.list?calendarId=<T>&privateExtendedProperty=calendar-sync:source=<S>:<E.id>
```

Find every mirror created by a pdir:
```
events.list?calendarId=<T>&privateExtendedProperty=calendar-sync:scope=<P>:<D>
```

Find every mirror calendar-sync has ever created on a calendar:
```
events.list?calendarId=<T>&privateExtendedProperty=calendar-sync:version=2
```

All are single-property queries.

## Out of scope

The following are intentionally not part of this version. None require structural changes to the spec to add later, but each is a deliberate non-goal here:

- **Multi-account support.** calendar-sync uses whatever account `gws auth status` reports. No `[accounts.<name>]` config table, no `--account` flag. Users with calendars across multiple Google accounts share calendars between accounts (Google Calendar's sharing UI) so a single `gws`-authenticated account has access to everything.
- **Webhook push notifications via `events.watch`.** Requires a verified HTTPS domain in Google Search Console and a publicly-reachable always-on endpoint. Doesn't fit a laptop deployment. Polling with `syncToken` gets near-real-time latency at low API cost on a long-running daemon, which is sufficient.
- **Per-pair redaction modes.** Mirror events copy source title and description verbatim. There are no `title_template`, `redact_description`, or similar config knobs. Users who need redaction control writer access to the destination calendar instead.
- **Mirror-only recurring instance overrides.** A user dragging one occurrence of a recurring mirror to a different time creates an override the source doesn't have. The daemon does not detect or reconcile these. Workaround: `calendar-sync mirror prune <calendar> --pair <name>` and let the next sync regenerate. See "Limitation: mirror-only recurring instance overrides" in Sync Algorithm.
- **Parallel pdir execution.** Each tick processes pdirs sequentially. Wall-clock cost is dominated by Google API latency, not local CPU; parallelism would reduce latency at the cost of complexity.
- **Multi-machine sync coordination.** A single user running `watch` on multiple machines simultaneously would have both machines making redundant API calls. Don't do this; pick one host. There's no state arbitration to make multi-host correct.
- **Linux/Windows.** `install`/`uninstall` write a launchd plist; running on non-Darwin platforms exits with `not_macos`. The sync engine itself is portable Go and could be built on Linux for ad-hoc `run` invocations, but no service-installation story is provided for non-macOS hosts.

## Dependencies

- **CLI framework**: [kong](https://github.com/alecthomas/kong).
- **Config**: [BurntSushi/toml](https://github.com/BurntSushi/toml).
- **Subprocess**: stdlib `os/exec`. `gws` is invoked with `--format json` and parsed.
- **Locking**: stdlib `golang.org/x/sys/unix` for `flock`.
- **Logging**: stdlib `log/slog` with custom JSON and text handlers.
- **Tests**: stdlib `testing` with table-driven tests. `gws` is faked via a stub binary built at `go test` time that matches the expected `gws calendar events ...` argument shapes and emits canned JSON. The test boundary is the gws subprocess, not HTTP.

No direct Google SDK. No SQLite. No third-party logging library.
