# calendar-sync Specification

A Google Calendar event mirroring tool. Replaces the calendar-syncing piece of Reclaim.ai.

For each user-declared pair of calendars, calendar-sync mirrors busy events from one to the other (or both directions) so events on calendar A appear as private busy blocks on calendar B. Updates and deletions propagate. Recurring events mirror as recurring events with their RRULE intact, not as expanded instances.

## Design Principles

- **Google Workspace only.** No iCloud, no Outlook, no CalDAV. All Google Calendar interactions go through the `gws` CLI; calendar-sync never holds OAuth credentials directly.
- **Configuration-driven.** Every calendar pair and tunable lives in a TOML config file. No hardcoded calendars.
- **Long-running daemon, polling under the hood.** `calendar-sync watch` runs continuously under launchd `KeepAlive`. An internal scheduler ticks every `poll_interval` and uses Google's `syncToken` for cheap incremental delta lists. Webhooks (`events.watch`) are out of scope: they require a verified HTTPS domain and an always-on host with a public endpoint, neither of which fits a laptop deployment.
- **State on events. State in memory. Nothing on disk except config.** Mirror provenance lives in `extendedProperties.private` on each mirror event. Sync tokens, mirror inventories, and reconciliation state live in process memory and are rebuilt on a cold start. The only file under `~/.config/calendar-sync/` is `config.toml`.
- **Single-process serialization.** Because the daemon is a single long-running process, there's no overlap window and no need for advisory locks. Manual `calendar-sync run` invocations refuse if the daemon is reachable via its IPC socket (regardless of how it was started). Deterministic mirror event IDs provide additional protection against the racey case where a daemon starts mid-`run`.
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

The atomic unit calendar-sync operates on is a **pair-direction (pdir)**: a `(pair_name, direction)` tuple. Every pair expands to exactly one pdir with direction `a_to_b` (source-to-target). Bidirectional sync is achieved by declaring two pairs with swapped source/target. Every piece of sync state is keyed by pdir. The `direction` field in JSONL output and per-pdir state identifiers (`<pair>:a_to_b`) is retained for output stability, but `a_to_b` is the only value emitted.

This is intentional and corrects an early design where state was keyed by source calendar. Source-keyed state breaks when a single source fans out to multiple destinations: advancing the syncToken after one destination starves the others, and a partial failure on any destination loses events forever.

Per-pdir state is independent: pdir X failing doesn't prevent pdir Y from advancing its token, even if X and Y share a source calendar.

### Mirror identification

Every mirror event carries these private extended properties (`extendedProperties.private`):

| Key                            | Value example                              | Purpose                                                                                                          |
|--------------------------------|--------------------------------------------|------------------------------------------------------------------------------------------------------------------|
| `calendar-sync:source`         | `alice@example.com:abc123def456`           | `<canonical_source_calendar_id>:<source_event_id>`. Identifies which source event this is a mirror of. Used by the orphan walk during full re-sync to look up the source. Inventory lookups use the deterministic mirror event ID instead - see below. |
| `calendar-sync:source_updated` | `2026-04-29T23:00:00Z`                     | The source event's `updated` field at the time of the last reconciliation. The "did source change?" signal.      |
| `calendar-sync:checksum`       | `sha256:c3a4...e891`                       | SHA-256 over a canonical serialization of the fields calendar-sync manages on the mirror. The "did mirror drift?" signal. (See "Drift detection model" below.) |
| `calendar-sync:version`        | `3`                                        | Schema version. Current version is `3`. Bump if the property layout or managed-field set changes.                |

The pair name and direction are deliberately *not* stored on the mirror. The deterministic mirror event ID (see below) is derived from the source event alone, so renaming a pair in config is a metadata-only operation that doesn't require touching any mirror events. Bulk operations like `mirror list --pair X` derive the pair-to-mirror mapping client-side from the current config plus the mirror's `calendar-sync:source` value (the source calendar ID identifies the pdir given the target calendar being listed).

#### Canonical calendar IDs

The `calendar-sync:source` property uses the **canonical** calendar ID, never the alias `primary`. At config-load time, calendar-sync resolves every calendar reference (including `primary`) to its canonical ID via `gws calendar calendarList get`. Canonicalized IDs are used everywhere downstream: in-memory state keys, extended properties, log fields. This survives config edits where the user swaps `primary` for the explicit email or vice versa.

#### Deterministic mirror event IDs

Every mirror event is inserted with a **deterministic event ID** computed from `(canonical_source_calendar_id, source_event_id)`. The ID is base32hex-encoded, prefixed `cs2`, and within Google's allowed event-ID character set (`[a-v0-9]`, length 5-1024).

```
mirror_id = "cs2" + lowercase(base32hex(sha256(canonical_source_calendar_id + ":" + source_event_id))[:50])
```

The derivation depends only on source identity. Event IDs are calendar-scoped on Google's side, so `(target_calendar, mirror_id)` is already unique even if the same source event were mirrored to multiple target calendars (different targets produce different `(target_calendar, mirror_id)` tuples even with the same `mirror_id`). Pair names and directions are derived client-side from current config when needed for display; they aren't stored on the mirror, so renaming a pair touches no mirror events.

Deterministic IDs are used **only for inserts of new mirrors**. They are not used as the inventory lookup key, because legacy mirrors (created before this design landed) have random Google-generated event IDs that don't match the deterministic derivation. Inventory is keyed by `(canonical_source_calendar_id, source_event_id)` parsed from each mirror's `calendar-sync:source` extended property, so legacy and deterministic-ID mirrors both look up identically.

What deterministic IDs do guarantee:

1. **Race-free concurrent insert for new mirrors.** When the inventory contains no entry for a given `(canonical_source_calendar_id, source_event_id)` tuple and the daemon (or a `run`) decides to insert, the deterministic ID is used as the explicit `id` in `events.insert`. If two processes race through this point with the same source event, both target the same Google event ID; Google rejects the second with HTTP 409 `duplicate`. The losing process catches the conflict, `events.get`s the just-inserted mirror, and continues. No duplicate mirrors, ever.
2. **Stable across pair renames.** The derivation depends only on source identity; renaming a pair doesn't change anything about the mirror.

Note: the duplicate-insert race is only possible for new mirrors. For source events that already have a mirror in the inventory (the steady-state common case, including all legacy mirrors), classification finds the mirror via the source-tuple key and runs in-place reconciliation against it - no insert, no race.

Cancelled-and-revived: Google retains the event ID after `events.delete` for some retention window. If a mirror was deleted (orphan cleanup, `mirror prune`) and then becomes eligible again, an `events.insert` with the same deterministic ID may return 409 `duplicate` even though `events.get?eventId=<id>` shows `status=cancelled`. The daemon handles this by:

1. Insert with deterministic ID.
2. On 409 duplicate, `events.get` the ID.
3. If the existing event is `status=cancelled`, call `events.patch?eventId=<id>` with `status=confirmed` plus the full mirror payload to revive it.
4. If the existing event is alive, treat as "mirror already exists" and run the standard reconciliation.

Legacy mirrors stay at their random Google IDs forever - calendar-sync does not migrate them to deterministic IDs. There's no functional reason to: lookup by source-tuple finds them, reconciliation works in place, drift detection works on their checksum just like any other mirror. The deterministic-ID guarantee applies only to NEW mirrors created by this version of calendar-sync onward.

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
| no               | no               | `skip(reason=unchanged)` *unless `fields_disagree` (see below) - then* `patch(reason=stale_bookkeeping)`.            |
| yes              | no               | `patch` mirror from source. Reason `source_updated`.                                                                |
| no               | yes              | Drift handling. If source is writable: `propagate` mirror's edits to source. Else `revert` mirror to source values. Reason `target_edited`. |
| yes              | yes              | Conflict. Newer-wins by Google's `updated` timestamps: compare `source.updated` vs `mirror.updated`. If source is newer (or equal), `patch` mirror from source (reason `source_updated`); a `warn` log records that user edits were overwritten. If mirror is newer, drift handling as in the previous row; a `warn` log records that source updates were overwritten. |

Equal timestamps tiebreak to source. Google reports `updated` to milliseconds; concurrent edits within the same millisecond are vanishingly rare in practice.

##### `fields_disagree`: stale-bookkeeping fallback

`source_changed` and `mirror_drifted` answer the question "did either side change since our last write?" not "do source and mirror agree right now?" Those questions diverge if the daemon's last write was a managed-field no-op: stored `source_updated` got bumped to the current source.updated, and stored `checksum` got recomputed over the post-write event whose managed fields hadn't changed. Subsequent edits that don't bump source.updated past stored - or any prior path that left the mirror in an aligned-with-stored-but-out-of-sync-with-source state - then go undetected by the standard signals.

`fields_disagree` is a third signal that compares the source's current managed fields to the mirror's current managed fields directly. The check uses `mirror.DriftedFieldNames(mirror, desired)` which already implements the canonical-form rules used by the checksum (trailer stripping on description, order-insensitive recurrence). Computing it costs nothing on the wire - the daemon already builds `desired` from source for every reconciliation; reusing it for one extra equality check is free.

`fields_disagree` only changes the `!source_changed && !mirror_drifted` cell. The other three cells already produce a write whose post-write resource and checksum follow-up bring stored bookkeeping back in sync, so adding a check there would be redundant. The new cell:

| `source_changed` | `mirror_drifted` | `fields_disagree` | Outcome |
|------------------|------------------|-------------------|---------|
| no               | no               | yes               | `patch` mirror from source. Reason `stale_bookkeeping`. No `Conflict` label - the daemon doesn't have evidence of a user-edit conflict, just evidence of bookkeeping divergence. |

The patch path is identical to the existing `source_changed && !mirror_drifted` cell: write the full managed-field payload, then run the standard `calendar-sync:checksum` follow-up. The new reason gives operators a machine-readable signal that something in the mirror's history left bookkeeping inconsistent (a B20-shaped cancellation flow, a manual edit cycle, a Google cascade, etc.) so they can investigate without having to reconstruct the audit trail from a `source_updated` log line.

"Source is writable" in the table above means BOTH the source calendar's `accessRole >= writer` AND the pdir's effective `propagate_target_edits = true`. The effective value is `[[pairs]].propagate_target_edits` when set, otherwise `[settings].propagate_target_edits` (which itself defaults to false). A fresh install runs one-way (mirror edits revert) until the operator opts in. Per-pair scoping lets operators ramp two-way sync one direction at a time after validating the read path. A read-only source can never propagate regardless of the setting.

#### Managed fields and the checksum

These are the fields calendar-sync writes when it creates or patches a mirror, and (with one exception) the fields it watches for drift. The checksum is over their canonical serialization:

- `summary` (string, possibly empty)
- `description` (string, including the `\n\n---\nSource: <htmlLink>` trailer that calendar-sync appends)
- `location` (string, possibly empty; copied verbatim from the source)
- `start` (object with `dateTime` xor `date`, plus optional `timeZone`)
- `end` (same shape as `start`)
- `recurrence` (array of RRULE/RDATE/EXDATE strings; sorted alphabetically before hashing for stability; omitted entirely for non-recurring events and for instance overrides)
- `transparency` (always `"opaque"` on a clean mirror)
- `visibility` (always `"private"` on a clean mirror)

The canonical serialization is JSON with object keys sorted, no whitespace, RFC 8259 form. The checksum value is `sha256:<hex>`.

**`reminders` is deliberately *not* in the checksum** even though we write `{"useDefault": false}` on every mirror payload. Two reasons: (1) Google does not bump the event's `updated` timestamp when only `reminders` change, so newer-wins conflict resolution would be unreliable for any drift signal that includes `reminders`. (2) Whether reminders fire on the mirror is a personal preference that's reasonable for the user to override. We set the safe default on creation; we don't fight subsequent changes.

Other fields (attendees, conferenceData, organizer, eventType, etc.) are also not hashed. calendar-sync doesn't manage them on the mirror, and we don't fight the user if they edit them.

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

The current schema version is `3`. A mirror whose stored `calendar-sync:version` differs from the current value is a legacy mirror; calendar-sync supports migrating any prior version (v1, v2) to the current schema in a single write on first encounter.

Two characteristics of legacy mirrors block a naive drift check:

- v1 mirrors predate the `calendar-sync:checksum` property. There is no stored hash to compare against, so the standard `mirror_drifted` signal is meaningless for them.
- v2 mirrors lack the `location` managed field. Their stored checksum was computed over a smaller field set; the current Checksum function would always disagree, so `mirror_drifted` would always fire even on a clean mirror.

On first encounter of any legacy mirror, calendar-sync derives `mirror_drifted` by comparing the live mirror's managed fields to the desired payload computed from the source:

- `mirror_drifted = (any managed field on the mirror differs from the desired-from-source value)`.

Then the four-way matrix runs as usual:

- `!source_changed && !mirror_drifted`: no drift, just upgrade. Re-write the mirror at the current `version` with a fresh checksum (and, for v2 mirrors, picking up the `location` field from source). Action `patch`, reason `migration_upgrade`.
- `!source_changed && mirror_drifted`: source wins by default during migration (same conservative rationale as `source_changed && mirror_drifted`). The mirror is rewritten from source with `migration_source_won` conflict logged. We don't propagate during migration because the drift may be schema-induced (e.g. a v3 field that didn't exist in the v2 mirror) rather than a real user edit, and we can't safely distinguish.
- `source_changed && !mirror_drifted`: `patch` from source as normal.
- `source_changed && mirror_drifted`: source wins by default during migration (more conservative than newer-wins). A `warn` log records `migration_source_won` so the user knows mirror edits may have been overwritten. v1 mirrors fall under this rule because they have no reliable user-edit timestamp; v2 mirrors keep the same rule for simplicity and consistency, even though their `updated` timestamp is technically reliable.

`migration_source_won` is used for both the `source_changed && mirror_drifted` cell and the `!source_changed && mirror_drifted` cell during migration; any drift on a legacy mirror routes through the same source-wins path.

After this single migration write, the mirror is at the current `version` and subsequent reconciliations use the standard drift detection model.

#### Inherited recurring-instance handling

When calendar-sync writes a recurring parent mirror, Google Calendar materializes the parent's instances using the parent's RRULE and copies the parent's `extendedProperties.private` to each materialized instance verbatim. So an auto-materialized instance inherits the parent's `calendar-sync:source`, `calendar-sync:checksum`, `calendar-sync:source_updated`, and `calendar-sync:version` until calendar-sync explicitly writes that instance.

Two consequences:

- The inherited `calendar-sync:source` value is the parent's tuple (`<S>:<parent_id>`), not the per-instance form (`<S>:<parent_id>_<UTC>`). String compare against the source's `RecurringEventID` is sufficient detection.
- The stored `calendar-sync:checksum` was computed over the parent's managed fields, not the instance's. Recomputing the live instance's checksum will almost always disagree, so the standard `mirror_drifted` signal fires even on a clean instance.

The recurring-instance handler therefore routes inherited instances through the same bootstrap source-wins path used for schema migration. After running the same `DriftedFieldNames`-based recompute as the migration path:

- `!source_changed && !mirror_drifted`: rewrite explicitly so the instance carries per-instance metadata. Action `patch`, reason `inherited_upgrade`.
- `mirror_drifted` (with or without source change): the instance's drift may be a real source-side override (a rescheduled occurrence) that the parent's RRULE projection didn't carry. The mirror's value is bootstrap state, not a user edit, and propagating it back would clobber the source's override. Source-wins unconditionally with `inherited_source_won` conflict logged. The source is never patched on this path.

The standard newer-wins tiebreak does not apply: the mirror's `updated` timestamp is fresh by construction (just materialized at parent-insert time) and would always defeat a pre-existing source override. Source-wins regardless of timestamps.

After this write the instance is explicitly managed (its `calendar-sync:source` matches the source instance ID) and subsequent reconciliations use the standard drift matrix, including target-edited propagation when the source is writable.

When `calendar-sync:version` is missing or behind the current value, the schema-migration path takes precedence over the inherited-instance path - the schema bump is the more specific story.

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
propagate_target_edits = true

[[pairs]]
name = "work-to-personal"
source = "alice@example.com"
target = "primary"

# Optional per-pair overrides of [settings] fields. Useful for ramping
# two-way sync one direction at a time:
[[pairs]]
name = "personal-to-work"
source = "primary"
target = "alice@example.com"
horizon = "1d"                      # narrower than the 365d default
propagate_target_edits = false      # keep this direction read-only for now
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
| `propagate_target_edits` | bool | `false` | Default two-way sync gate for any pair without an override. When false, drift on a writable-source pdir routes to `revert` instead of `propagate` - the source is never modified. When true, SPEC's two-way behavior in §"Drift detection model" is in effect. Per-pair `[[pairs]].propagate_target_edits` overrides this default. Pdirs whose source is read-only (`accessRole < writer`) always revert regardless. The default-off posture lets operators verify the one-way path before opting in. |

Duration strings follow Go's `time.ParseDuration` syntax (`30s`, `5m`, `24h`) plus `d` (days) which calendar-sync adds.

#### `[[pairs]]`

| Field                    | Type                  | Required  | Description                                                                                                                                                  |
|--------------------------|-----------------------|-----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `name`                   | string                | yes       | Unique. Used in logs and as the human-facing identifier for `--pair` flags on `mirror list`/`mirror prune`. Must match `^[a-z0-9][a-z0-9-]{0,62}$`.          |
| `source`                 | string or inline table | yes      | Source calendar reference. See "Calendar references" below.                                                                                                  |
| `target`                 | string or inline table | yes      | Target calendar reference. See "Calendar references" below.                                                                                                  |
| `enabled`                | bool                  | no (true) | If false, the pair is skipped entirely.                                                                                                                      |
| `time_zone`              | string                | no        | IANA name (e.g. `America/New_York`). Used as the `timeZone` on mirrored events when the source event is all-day. Defaults to the destination calendar's default. |
| `horizon`                | duration              | no        | Optional override of `[settings].horizon` for this pair. Same `1d`..`730d` bounds. Falls back to the settings value when unset.                              |
| `propagate_target_edits` | bool                  | no        | Optional override of `[settings].propagate_target_edits` for this pair. Falls back to the settings value when unset. Useful for ramping two-way sync one direction at a time. |

##### Calendar references

`source` and `target` accept two forms:

- **String** - the calendar ID directly. Accepts an email address (`"alice@example.com"`), the literal `"primary"` (which calendar-sync resolves to its canonical ID), or a group calendar ID (`"<hash>@group.calendar.google.com"`).
- **Inline table** - lookup by display summary. The `summary` key (required) is matched case-insensitively against the gws-authenticated account's calendar list. Matching is against the user-visible name: each candidate's `summaryOverride` if set (the per-calendar display label users edit in Google's UI), otherwise its underlying `summary`. The optional `account` key disambiguates when multiple visible calendars share the same display name; matching prefers the candidate's `dataOwner` (case-insensitive equality), falling back to a case-insensitive substring of the calendar's ID when `dataOwner` is empty. Useful for subscriptions whose IDs aren't memorable (`source = {summary = "TripIt"}` resolves to whatever `<random>@import.calendar.google.com` ID Google assigned).

| Inline-table field | Type   | Required | Description                                                                                                  |
|--------------------|--------|----------|--------------------------------------------------------------------------------------------------------------|
| `summary`          | string | yes      | Display name as it appears in Google Calendar (the override the user applied if set, otherwise the calendar's underlying summary). Matched case-insensitively. |
| `account`          | string | no       | Disambiguator when multiple calendars share `summary`. Matched first against each candidate's `dataOwner` (case-insensitive equality), then against the calendar ID as a case-insensitive substring when `dataOwner` is empty. |

The inline-table form makes one `gws calendar calendarList list` call at canonicalize time and resolves every summary-form ref against the result, regardless of how many pairs use it. ID-form refs continue to use one `calendar calendarList get` per distinct ref.

Limitation: when `dataOwner` is empty, `account` falls back to ID-substring matching, which breaks for import/subscription calendars whose IDs look like `<random>@import.calendar.google.com` - the random hash carries no account information. If two such calendars share a display name and neither can be disambiguated by `dataOwner` or ID substring, fall back to the bare ID-form ref. New calendars added in Google Calendar's UI after the daemon starts aren't visible until config reload (same behavior as the pre-F1 ID-form path).

#### Validation rules

Run on every command that touches config. Failures exit with code 1 and a JSON error to stderr (see Output).

- `name` is unique across all pairs.
- `direction` field on `[[pairs]]` is rejected if present. The field was removed in v2.0.0; validation fails with a migration hint pointing users at the two-pair pattern (declare two `[[pairs]]` entries with swapped source/target for bidirectional sync; remove the field for the new default of source-to-target).
- `source` and `target` are required (an inline-table form with neither `summary` nor a string value is rejected).
- An inline-table calendar ref with `account` but no `summary` is rejected; `account` is only meaningful as a summary-lookup disambiguator.
- A summary-form ref that resolves to zero or to multiple calendars (after the optional `account` filter) is rejected with a detail listing the matches so the user can correct the ref.
- After canonicalization, `source != target`. Mirroring a calendar to itself is rejected.
- After canonicalization and pdir expansion, no two pdirs share the same `(canonical_source, canonical_target)` pair. Two pdirs writing identical mirrors to the same calendar is a configuration bug. (Direction is always `a_to_b` post-v2.0.0, so the prior triple collapses to a pair.)
- `poll_interval >= 15s`.
- `horizon` is between `1d` and `730d` inclusive. The same range applies to per-pair `[[pairs]].horizon` when set.
- `full_sync_interval` is between `1h` and `30d` inclusive.
- `log_level` is one of the four allowed values.
- `log_format` is `json` or `text`.
- **Access role** (resolved during canonicalization via `gws calendar calendarList get` for each unique calendar reference; the response's `accessRole` field is one of `freeBusyReader`, `reader`, `writer`, `owner`):
  - The source calendar's `accessRole` is `>= reader` (i.e. `reader`, `writer`, or `owner`). `freeBusyReader` is rejected because we cannot read event details.
  - The target calendar's `accessRole` is `>= writer` (`writer` or `owner`). A read-only target means we can never write mirrors there.
  - The pdir's `source_writable` flag (used by drift handling) is `true` iff the source's `accessRole` is `>= writer`. A `source_writable=false` pdir can still mirror events from source to target; it just `revert`s any mirror drift instead of `propagate`ing.

### Examples

#### Minimal: one source-to-target pair

```toml
[[pairs]]
name = "primary-pair"
source = "alice@example.com"
target = "primary"
```

#### Four pairs, including a bidirectional declared as two

```toml
[settings]
poll_interval = "60s"
horizon = "365d"
propagate_target_edits = true

# Bidirectional sync between work and personal: declared as two pairs with
# swapped source/target. The second pair narrows its horizon as part of a
# gradual two-way rollout.
[[pairs]]
name = "work-to-personal"
source = "alice@example.com"
target = "primary"

[[pairs]]
name = "personal-to-work"
source = "primary"
target = "alice@example.com"
horizon = "30d"
propagate_target_edits = false

[[pairs]]
name = "work-to-family"
source = "alice@example.com"
target = "family@group.calendar.google.com"

[[pairs]]
name = "personal-to-family"
source = "primary"
target = "family@group.calendar.google.com"
enabled = false
```

#### Mirror a TripIt subscription by summary

The TripIt-generated calendar's ID is a random hash (`<hash>@import.calendar.google.com`) that's hard to type and changes if the user re-subscribes. The summary the user sees in Google Calendar's UI - "TripIt" - is stable, so the inline-table form is more useful here than the bare ID:

```toml
[[pairs]]
name = "tripit-to-personal"
source = {summary = "TripIt"}
target = "primary"
```

If the user has shared multiple "TripIt"-summary calendars across accounts, `account` disambiguates by case-insensitive substring of the calendar's ID:

```toml
[[pairs]]
name = "coreweave-to-personal"
source = {summary = "CoreWeave", account = "alice@example.com"}
target = "primary"
```

## Authentication

calendar-sync delegates all authentication to `gws`.

- The user runs `gws auth login` once.
- `~/.config/gws/credentials.enc` holds the encrypted token; `gws` refreshes it transparently.
- calendar-sync invokes `gws` as a subprocess. Whatever account `gws auth status` reports is what calendar-sync uses.

The user is responsible for ensuring this single `gws`-authenticated account has **appropriate access to every calendar referenced in the config** (`accessRole >= reader` for sources, `>= writer` for targets). For the typical mixed work/personal use case, this means sharing the personal Google calendar with the work account (or vice versa) via Google Calendar's calendar-sharing UI before configuring calendar-sync.

`calendar-sync watch` and `calendar-sync run` both call `gws auth status` at startup and exit with code 2 if it returns non-zero.

## Privacy and the mirror payload

By design, mirror events copy the source event's title, description, and location verbatim. This is *not* a redaction tool. The destination calendar's writers and owners can read those details. The `visibility=private` setting only hides details from readers.

This is intentional: the user creating the pairs controls the destination calendars and the people who have writer access to them. In the typical case (mirroring between calendars the user owns), the only readers are the user and people they've explicitly shared with. If the user mirrors to a calendar with broader writer access, the leak is the user's responsibility.

There's a related caveat for **`reader`-access source calendars**: per Google's sharing model, events with `visibility=private` on a source where the user only has `reader` access have their `summary`, `description`, and other details hidden. Such events come back from `events.list` with empty or stripped fields. calendar-sync mirrors what Google returns, so a private source event on a reader-access calendar produces a mirror with an empty title and description (still marked busy and at the right time). For TripIt and other public-by-default subscriptions this isn't an issue; for shared calendars where the sharer marks events private, the mirror won't carry details. Workaround: ask the source calendar owner for `writer` access.

Redaction modes (`title_template`, `redact_description`) are out of scope for this version.

### Edits flow back too

Because mirror edits propagate to writable sources (see "Drift detection model"), edits to a mirror's title, description, or location are **also visible to the source's other readers** after the next sync. If the user edits a work-mirrored event on their personal calendar to add private notes, those notes flow back to the work calendar where colleagues will see them. The user should treat their own mirror edits as if they were editing the source directly when the source is writable.

## Output and Logging

### stdout

Most commands produce JSONL (newline-delimited JSON) ending with a `_meta` trailer:

```
$ calendar-sync pair list
{"name":"work-to-personal","source":"alice@example.com","target":"primary","enabled":true}
{"name":"work-to-family","source":"alice@example.com","target":"family@group.calendar.google.com","enabled":true}
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
{"error":"config_invalid","detail":"pair 'work-personal' has direction = 'bidirectional'; the direction field was removed in v2.0.0","hint":"remove the field for source-to-target (the new default); for bidirectional sync, declare two pairs with swapped source/target","cause":"<wrapped, optional>"}
```

### Exit codes

| Code | Meaning              | When                                                                                          |
|------|----------------------|-----------------------------------------------------------------------------------------------|
| 0    | Success              | Command ran to completion.                                                                    |
| 1    | General error        | Config invalid, gws subprocess failed for non-auth reasons, partial sync failure.             |
| 2    | Auth error           | `gws auth status` reports unauthenticated, or 401 returned from a Calendar API call.          |
| 3    | Rate limited         | Hit retry ceiling (5 retries, exponential backoff with jitter; see Retry policy).             |
| 4    | Network error        | DNS failure, connection refused, TLS error.                                                   |
| 5    | Daemon already running | `calendar-sync run` was invoked while `calendar-sync watch` is reachable on its IPC socket. |
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
  --direction <dir>    Limit to one direction within each pair. Only `a_to_b` is currently meaningful (every pair is implicitly source-to-target post-v2.0.0).
  --dry-run            Plan and print actions but make no API writes. Reads still happen.
  --timeout <dur>      Wall-clock cap for the entire command. Default: 5m.
```

`run` refuses to start if `calendar-sync watch` is reachable via its IPC socket at `$TMPDIR/calendar-sync.sock`. The check works regardless of how the daemon was started (launchd, manual `watch` in a terminal, anything else). The intent is intentional friction: stop the daemon (`calendar-sync uninstall` if launchd-managed, or signal the foreground daemon to exit) before running manual reconciles. Deterministic mirror IDs prevent duplicate creation in the race window where a daemon could start mid-`run`.

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
| `tentative`                | `skip` or `delete`          | Source calendar owner's `responseStatus=tentative` (the "maybe" state). Treated like `declined` because a tentative event isn't a confirmed busy block. `delete` if a mirror exists, `skip` if not. |
| `transparency_transparent` | `skip` or `delete`          | Source `transparency=transparent`. `delete` if a mirror exists, `skip` if not.                                                           |
| `outside_horizon`          | `skip` or `delete`          | Non-recurring source `start > now + horizon`, or recurring source has no instance in `[now, now + horizon]`. `delete` if mirror exists.  |
| `parent_not_eligible`      | `skip`                      | A recurring instance arrived but its source parent is itself filtered out (cancelled, transparent, declined, tentative, outside_horizon). |
| `unchanged`                | `skip`                      | Both drift signals false: mirror up-to-date and unmodified relative to source.                                                            |
| `pair_disabled`            | `skip`                      | The pdir is `enabled=false`. Emitted only when the user explicitly named the pair via `--pair`.                                          |
| `instance_unmaterializable`| `skip`                      | Recurring-instance lookup returned zero results even after re-patching the mirror parent (rare; see "Zero-result instance lookup").       |
| `source_updated`           | `insert` or `patch`         | `source_changed=true && mirror_drifted=false`, or no mirror exists yet (then `insert`). Also covers `source_changed && mirror_drifted` resolved to source-wins (see `conflict_source_won` below). |
| `target_edited`            | `propagate` or `revert`     | `mirror_drifted=true && source_changed=false`, or `source_changed && mirror_drifted` resolved to mirror-wins. `propagate` if `pdir.source_writable`, else `revert`. |
| `stale_bookkeeping`        | `patch`                     | `source_changed=false && mirror_drifted=false && fields_disagree=true` - stored bookkeeping reports both signals clean but the source's current managed fields differ from the mirror's. Repairs the divergence by rewriting the mirror from source. No conflict label; the daemon doesn't have evidence of a user-edit conflict, just bookkeeping divergence (see §"`fields_disagree`: stale-bookkeeping fallback"). |
| `migration_upgrade`        | `patch`                     | A legacy mirror (v1 or v2) with no source change and no drift, re-written at the current `version` with a fresh `calendar-sync:checksum`. One-time per pre-existing mirror. |
| `inherited_upgrade`        | `patch`                     | A recurring-instance mirror auto-materialized by Google from a parent we wrote, with no actual drift, re-written so the instance carries per-instance `calendar-sync:source` and `:checksum`. One-time per inherited instance. See "Inherited recurring-instance handling". |
| `orphaned`                 | `delete`                    | Prune pass found a mirror whose source no longer exists.                                                                                 |
| `mirror_only_override`     | `skip`                      | Target-delta saw a recurring instance whose source has no override at that occurrence. Phase 1 limitation; SPEC §"Limitation: mirror-only recurring instance overrides" + B17 Phase 2 follow-up. |
| `source_orphan`            | `skip`                      | Target-delta event references a source that no longer exists. The orphan-walk's existing prune pass cleans the mirror; target-delta's job is just to surface the observation. |

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
- `migration_source_won` - same as `conflict_source_won` but during a legacy schema migration (v1 or v2 → current). v1 mirrors have no reliable user-edit timestamp; v2 mirrors keep the same simpler rule for consistency. Source wins by default. The action carries `reason=source_updated`.
- `inherited_source_won` - drift on a recurring-instance mirror that has not been explicitly written by calendar-sync (auto-materialized from the parent we wrote, identified by `calendar-sync:source` matching the parent tuple). The mirror's value is bootstrap state, not a user edit, so source-wins regardless of the timestamp tiebreak. The action carries `reason=source_updated`. See "Inherited recurring-instance handling" above.

The `source_updated` and `mirror_updated` fields show the timestamps that drove the newer-wins decision (omitted on `migration_source_won` and `inherited_source_won` since both bootstrap paths use source-wins-by-default rather than a timestamp comparison), so the user can verify it was the call they wanted.

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
| `daemon_already_running` | 5 | The `calendar-sync watch` daemon is reachable on its IPC socket. Stop it before running a manual reconcile. |
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
calendar-sync run --pair work-to-personal
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
{"settings":{"poll_interval":"60s","horizon":"365d","full_sync_interval":"24h","log_level":"info","log_format":"json","dry_run":false,"propagate_target_edits":true},"pairs":[{"name":"work-to-personal","source":"alice@example.com","target":"primary","enabled":true}]}
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
{"name":"work-to-personal","source":"alice@example.com","target":"primary","enabled":true}
{"_meta":{"count":1}}
```

Errors: same as `config show`.

### calendar-sync pair test

Equivalent to `calendar-sync run --pair <name> --dry-run` for a single pair, with one extra check: it canonicalizes both calendar IDs and prints them in the output for sanity-checking.

```
calendar-sync pair test <name> [flags]
  --direction <dir>    Limit to one direction. Only `a_to_b` is currently meaningful (every pair is implicitly source-to-target post-v2.0.0).
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
  --pair <name>        Only mirrors created by this pair.
  --direction <dir>    With --pair. Only `a_to_b` is currently meaningful (every pdir is a_to_b post-v2.0.0).
  --orphaned           Only mirrors whose source no longer exists. Triggers per-mirror source lookup.
  --limit <n>          Max items to return. Default: 250.
  --all                Fetch all pages.
```

`--pair`/`--direction` filtering is applied client-side: the command lists all mirrors via `privateExtendedProperty=calendar-sync:version=3` (plus one list per legacy version still in the wild), parses each mirror's `calendar-sync:source` to recover the source calendar ID, and matches against the current config to determine which pdir produced it. Matching is uniquely defined because validation guarantees no two pdirs share the same `(canonical_source, canonical_target)` pair.

```
$ calendar-sync mirror list primary --pair work-to-personal
{"id":"cs2abc...","summary":"Standup","start":"2026-04-30T15:00:00Z","end":"2026-04-30T15:30:00Z","source":"alice@example.com:abc123","source_updated":"2026-04-29T23:00:00Z","pair":"work-to-personal","direction":"a_to_b"}
{"_meta":{"count":1,"has_more":false}}
```

The `pair` and `direction` fields in the output are derived client-side from current config; they're not stored on the mirror itself.

#### Errors

| Error code           | Exit | When                                               |
|----------------------|------|----------------------------------------------------|
| `calendar_not_found` | 1    | Calendar ID not accessible.                        |
| `pair_not_found`     | 1    | `--pair <name>` doesn't match.                     |

### calendar-sync mirror prune

Delete mirror events from a calendar.

```
calendar-sync mirror prune <calendar> [flags]
  --pair <name>            Only delete mirrors created by this pair.
  --direction <dir>        With --pair. Only `a_to_b` is currently meaningful (every pdir is a_to_b post-v2.0.0).
  --orphaned               Only delete mirrors whose source no longer exists.
  --all                    Delete every mirror calendar-sync has ever created on this calendar.
  --prune-horizon <dur>    Narrow the selection to mirrors whose start falls in [now, now+dur].
                           Inclusive on both edges. Distinct from sync horizon - prune is a one-shot
                           operation, not the rolling sync window.
  --dry-run                List what would be deleted, do nothing.
  --yes, -y                Skip the interactive confirmation.
```

Exactly one of `--pair`, `--orphaned`, `--all` must be provided. Without `--yes`, the command prompts (TTY only). `--prune-horizon` is a narrowing modifier on top of one of those broad selectors; events whose start can't be parsed (or is missing) are excluded when `--prune-horizon` is set, so the user must drop the flag for a final cleanup sweep that catches them.

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
{"pdir":"work-to-personal:a_to_b","source_calendar":"alice@example.com","target_calendar":"alice.personal@example.org","mirrors":1245,"last_tick_at":"2026-04-30T20:55:30Z","last_tick_status":"ok","last_tick_inserts":0,"last_tick_patches":1,"last_tick_deletes":0,"last_tick_propagates":0,"last_tick_reverts":0,"last_tick_skips":2}
{"pdir":"personal-to-work:a_to_b","source_calendar":"alice.personal@example.org","target_calendar":"alice@example.com","mirrors":782,"last_tick_at":"2026-04-30T20:55:30Z","last_tick_status":"ok","last_tick_inserts":0,"last_tick_patches":0,"last_tick_deletes":0,"last_tick_propagates":0,"last_tick_reverts":0,"last_tick_skips":1}
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

#### Upgrades via Homebrew

`brew upgrade calendar-sync` replaces the Homebrew-managed `calendar-sync` symlink, but launchd does NOT bounce the daemon when the binary file is replaced - the running process keeps executing the prior binary's mmap'd code. The Homebrew cask's `postflight` hook bridges the gap: it runs `launchctl kickstart -k gui/<uid>/org.calendar-sync.agent` whenever the agent is currently loaded, restarting the daemon against the new binary.

Cold installs (no agent loaded yet) skip the kickstart so a first-time `brew install` doesn't fail; the user runs `calendar-sync install` once to load the agent for the first time. `kickstart` failures are non-fatal (`must_succeed: false`) so a transient launchctl issue won't block the upgrade itself. To verify the daemon picked up the new binary, run `calendar-sync version` (which prints the binary version on disk) and compare with `launchctl print gui/$(id -u)/org.calendar-sync.agent | grep pid` and the daemon's startup time in its log file - a stale daemon would have a `started_at` predating the upgrade.

The hook only knows the default launchd label (`org.calendar-sync.agent`). Users who installed with `calendar-sync install --label <custom>` need to keep using the manual `calendar-sync uninstall && calendar-sync install` dance after `brew upgrade`.

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
- The source calendar owner's attendee `responseStatus` is one of `accepted` or `needsAction` (or there's no attendees array, in which case the owner is implicitly accepted). `declined` and `tentative` are both excluded - the owner has indicated this isn't a confirmed busy block, and mirroring would create a busy block on the destination calendar that doesn't reflect the owner's actual commitment.
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
- Mirror inventory: a `map[<canonical_source_calendar_id>:<source_event_id>] -> live mirror Event resource` containing every mirror calendar-sync currently has on this calendar. Keyed by the source-tuple parsed from each mirror's `calendar-sync:source` extended property, *not* by Google event ID. This handles legacy mirrors (created before deterministic IDs were specified) which have random Google event IDs that don't match the post-spec deterministic-ID derivation - lookups by source-tuple find them regardless.
- `targetSyncToken` (from the most recent `events.list` against the target). Maintained only when at least one pdir on this target has the effective two-way-sync gate open (`source_writable && propagate_target_edits`). Drives the per-tick target-delta phase that propagates target-side edits back to source within one tick instead of waiting for the next periodic full re-sync.

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

   The `timeMax` for the source-list is `now + horizon`, where `horizon` is the maximum effective horizon across all pdirs that share this source. When pdirs sharing a source have different horizons (e.g. 365d on one pdir, 1d on another during a gradual two-way rollout), the longer wins for the wire call so the longer-horizon pdir's classifier sees its events; the shorter-horizon pdir filters per its own horizon at classification time.
5a. **Seed per-target syncToken (writable targets only).** For each distinct target calendar `T` whose unique writable-source set is non-empty (at least one pdir with `source_writable && propagate_target_edits`), issue a fresh `events.list` against `T` with empty `syncToken` and capture `nextSyncToken` into the in-memory per-target token. This pre-inventory-rebuild step establishes the baseline for the per-tick target-delta phase. Idle targets (no writable-source pdir) are skipped. Failures here log a warning and leave the slot empty; the next FullSync re-attempts. Seeding MUST run before the inventory rebuild in step 6 - if seeded after, an edit landing in the gap is invisible to both inventory (already snapshotted) and the seeded token (which starts after the edit).
6. **Mirror inventory per unique target.** For each distinct target calendar `T`, run the rebuild described in "Mirror inventory rebuild" - one `events.list` call per known schema version (current `version=3` plus each legacy version still in the wild), merged into a single inventory. Legacy entries are flagged for migration during reconciliation.
7. **Reconcile.** For each enabled pdir `(P, D)` with source `S` and target `T`, walk the in-memory list of source events for `S`. For each event, run the classification logic (see below) using the `T` mirror inventory to look up existing mirrors. Track success/failure per pdir.
8. **Commit syncTokens conditionally.** For each unique source `S`, install the staged `nextSyncToken` from step 5 into the in-memory per-source token *only if every pdir whose source matches `S` succeeded in step 7*. If any pdir for `S` failed, leave the in-memory token empty so the next cycle re-runs a full source-list for `S`. This is the same conditional-advancement rule that protects the per-tick path; both paths apply it.

   If the staged token is missing (Google can omit `nextSyncToken` on very long full lists) the same rule applies in spirit: leave the in-memory token empty so the next cycle re-runs a full source-list.
9. **Schedule.** Set the per-tick timer (`poll_interval`) and the periodic-full-resync timer (`full_sync_interval`).

Startup wall-clock cost on real-world calendars (1-year horizon, ~1000 events per source): on the order of 10-20s for a typical multi-pdir setup. Mostly Google API latency.

### Daemon lifecycle: per-tick reconciliation

Every `poll_interval`, the internal scheduler fires the per-tick path.

**Phase ordering.** The target-delta phase (step 0 below) MUST run before the source-delta phase (steps 1-4). If reversed, source-driven mirror rewrites in the source-delta phase can clobber target-side edits before the target-delta phase has a chance to detect and propagate them. The two phases are sequential, not interleaved, within a single tick.

0. **Target-delta phase (B17).** For each writable-source target `T` whose `targetSyncToken` is non-empty:
   ```
   gws calendar events list --params '{
     "calendarId": "<T>",
     "syncToken": "<in-memory targetSyncToken for T>",
     "showDeleted": true,
     "eventTypes": ["default", "outOfOffice", "focusTime"],
     "maxResults": 250
   }' --page-all
   ```
   For each event `E` in the response:
   - Skip silently if `E` lacks `calendar-sync:source` (not a calendar-sync mirror; defensive).
   - Parse the source-tuple `(source_cal, source_event_id)` from `calendar-sync:source`. Look up the SINGLE owning pdir `P` where `P.target == T && P.source == source_cal && P.source_writable && P.propagate_target_edits`. Skip silently if no match (stray mirror from a since-disabled pdir).
   - For non-recurring or recurring-parent events (`E.recurringEventId == ""`), `events.get(source_cal, source_event_id)` and dispatch through the standard Classifier.
   - For recurring instances (`E.recurringEventId != ""`):
     - **Managed form** (`source_event_id` already has the `_<UTC>` suffix): use it directly.
     - **Inherited form** (`source_event_id` is the source PARENT id without suffix): construct the source-instance id as `source_event_id + suffix`, where `suffix` is the `_<UTC>` portion of `E.id`.
     - `events.get(source_cal, source_instance_id)` and dispatch through the Classifier (which routes recurring instances to the recurring handler at step 2 of classification).
   - 404 on the source `events.get`: for non-recurring or parents, treat as a future orphan-walk concern and skip; for recurring instances, this is **mirror-only override** territory and Phase 1 emits `skip(reason=mirror_only_override)` (see "Limitation: mirror-only recurring instance overrides").
   - 410 GONE on the target-syncToken stream: clear `targetSyncToken[T]`, surface `NeedsFullResync=true` for `T`, skip the rest of this phase for `T`. The next FullSync re-seeds.
   - Token advancement: replace `targetSyncToken[T]` with the response's `nextSyncToken` only if every event in the delta was processed without error. Errors leave the token unchanged so the next tick re-delivers.

   The dispatch through the Classifier means the four-way drift matrix's `!source_changed && mirror_drifted && source_writable -> propagate` cell fires naturally, producing the same propagate write the periodic full re-sync would catch - just at tick rate instead of full-sync rate.

For each unique source `S`:

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

#### Per-event transient read tolerance

The token-gating rule above treats "any per-event classify error" as pdir failure. That conflates two error classes: write failures (mirror state may be partially updated) versus read flakes (a 5xx on `events.get` for a horizon check, a 400 on `events.instances` from Google's recurring-exception indexing quirk). One persistently-flaky source event under the strict rule keeps the source's token pinned forever, and the daemon falls into back-to-back FullSyncs.

Per-event errors during the classify loop are split into two classes:

- **Transient read** errors are logged at `warn` (the `transient` field on the log line is `true`) and skipped. The pdir keeps running. The token is allowed to advance once every other event in the delta succeeds. The skipped event is re-evaluated on the next tick (if it appears in a future delta) or the next FullSync (which re-walks every source event regardless). The transient class is intentionally narrow - only well-understood Calendar API hiccups that don't represent state mutation:

  | Op                  | Code(s)                                                       | Why transient                                              |
  |---------------------|---------------------------------------------------------------|------------------------------------------------------------|
  | `events.instances`  | `backend_error`, `api_invalid_request`, `api_not_found`       | Read-side; horizon eligibility / mirror-instance lookup    |
  | `events.get`        | `backend_error`, `api_not_found`                              | Read-side; recurring-handler parent fetch                  |

  `events.get` + `api_invalid_request` is intentionally NOT transient: 400 there is much more likely a request-shape bug than a Google quirk, and silencing it would hide real issues.

- **Fatal** errors (everything else) keep the pdir's `Err` non-nil and the token pinned. This includes:
  - any write op (`events.insert` / `events.patch` / `events.delete`) regardless of code, because a partially-applied write must be re-attempted on a stable token;
  - rate-limit / auth / forbidden / 410-gone / network errors, because skipping them defeats backoff or hides config drift;
  - context cancellation / deadline exceeded, because those signal whole-pass shutdown (SIGTERM) or run-budget exhaustion (`calendar-sync run --timeout`) rather than per-event flake;
  - the post-409 `events.get` inside the insert-recovery path, because that read drives a write decision (revive cancelled mirror vs reconcile alive mirror) and a flake leaves the daemon unable to know the colliding mirror's state.

A pdir whose loop contains any fatal error fails the pdir even if other events in the same loop only flaked transiently - the conditional-advancement rule still gates the token. Only loops with zero fatal errors clear the gate.

Empty deltas (the common case) cost a single API call per source - measured at ~270ms for an empty incremental response.

### Daemon lifecycle: periodic full re-sync

Every `full_sync_interval` (default 24h), the daemon repeats the source-list / inventory-rebuild / reconcile work of startup, plus a follow-up orphan-cleanup walk that doesn't run during the per-tick path. The full re-sync does NOT re-read `config.toml` from disk: a parsed config snapshot is captured at daemon startup and reused for the daemon's lifetime. Config edits require a daemon restart.

What full re-sync does refresh:

1. Re-canonicalize calendar IDs (in case Google reassigned a primary, though rare) and re-fetch each calendar's `accessRole` via `gws calendar calendarList get`. If a target's `accessRole` has dropped below `writer`, log an error and skip pdirs that target it for the rest of this re-sync. If a source's `accessRole` has changed, recompute the corresponding pdirs' `source_writable` flag (drift handling switches between propagate and revert accordingly).
2. Full source-list per unique source. Replace the in-memory source listings.
3. Mirror inventory rebuild per unique target (see "Mirror inventory rebuild" below).
4. Reconcile every source event through the classification logic against the rebuilt inventory.
5. **Orphan walk.** For each mirror inventory entry whose corresponding source event ID was *not* visited in step 4 (i.e. its source wasn't returned by the full source-list), parse `calendar-sync:source` from the mirror to recover `<source_calendar_id>:<source_event_id>` and look up the source via `events.get?calendarId=<source_calendar_id>&eventId=<source_event_id>`:
   - **Source returns 404 or has `status=cancelled`**: delete the mirror. Action `delete`, reason `orphaned`.
   - **Source is non-recurring and `start > now + horizon`**: delete the mirror. Action `delete`, reason `outside_horizon`.
   - **Source is a recurring parent (has `recurrence`)**: don't trust `start` (which is the series start). Call `events.instances?calendarId=<S>&eventId=<source_id>&timeMin=<now>&timeMax=<now + horizon>&maxResults=1&showDeleted=false`. Zero instances: delete the mirror, reason `outside_horizon`.
   - **Source is alive and in horizon but was filtered** (eventType excluded by Google's server-side filter, transparency=transparent, declined, tentative, etc.): delete the mirror, reason `source_filtered`. The fact that the source exists but doesn't match our query means it's no longer eligible for mirroring.

Step 5 closes the gap that the per-tick path can't: incremental deltas via `syncToken` carry `status=cancelled` for source deletions only when the daemon was up to receive them. If the daemon was down (laptop closed, system rebooted) when a source was deleted, the cancellation event is consumed and lost - the next incremental delta never sees it. Periodic full re-sync's orphan walk catches what the per-tick path missed.

What this catches:

- **Horizon ingress.** Events that crossed into `[now, now + horizon]` simply by passage of time, without changing. Incremental sync wouldn't return them; full sync does.
- **Mirror drift on currently-eligible source events.** A user edited a mirror but its source hasn't changed since the last delta. Most cases are also caught at tick granularity by the per-tick target-delta phase (B17), but full sync visits every source event regardless and remains the safety net for cases the target-delta path can't reach (e.g. parent-induced inherited instance reshapes, mirror-only overrides per "Limitation: mirror-only recurring instance overrides").
- **Orphans.** Mirrors whose source was deleted, moved beyond horizon, or made ineligible (transparency, declined, tentative) while the daemon was down. The orphan walk in step 5 handles these.
- **`accessRole` changes on calendars.** Step 1 re-fetches access roles. A target that lost writer access stops accepting writes; a source that gained writer access starts having its mirror drift propagated.

What this does NOT catch (still documented limitations):

- **Mirror-only recurring instance overrides** (a user-created override at a recurrence time the source has no override for). See the dedicated limitation section.
- **Config changes.** Editing `config.toml` while the daemon is running has no effect; restart the daemon (`calendar-sync uninstall && calendar-sync install`) for changes to take effect.

After each full re-sync the in-memory inventories are replaced atomically.

#### Mirror inventory rebuild

For each unique target, the rebuild runs one `events.list` call per known schema version, in order:

1. `privateExtendedProperty=calendar-sync:version=3` to find current-schema mirrors.
2. `privateExtendedProperty=calendar-sync:version=2` to find v2 legacy mirrors (lack the `location` managed field).
3. `privateExtendedProperty=calendar-sync:version=1` to find v1 legacy mirrors (predate `calendar-sync:checksum`).

All responses are merged into the single in-memory inventory. Legacy entries are flagged for migration; on first reconciliation each gets re-written at the current `version` with a fresh `calendar-sync:checksum` per the schema-migration rules.

Without the legacy queries, mirrors that were inserted before the latest schema bump would never appear in inventory and would never be reconciled or cleaned up - they'd become permanent zombies.

The rebuild runs in two passes to keep auto-materialized recurring-instance mirrors from shadowing their parent at the same source-tuple key. When calendar-sync writes a recurring parent, Google materializes the parent's instances and copies the parent's `extendedProperties.private` to each materialized instance verbatim; an instance the user has overridden on the target therefore comes back from `events.list` with `calendar-sync:source` pointing at the source PARENT's tuple, the same value the actual parent mirror carries. Pass 1 builds a (mirror parent ID -> source-tuple) map across all version queries. Pass 2 indexes each event, dropping any instance (`recurringEventId != ""`) whose parsed source-tuple matches its parent's recorded source-tuple - those are inherited and would clobber the parent. Explicitly-managed instances (`calendar-sync:source = "<S>:<source_instance_id>"` with the `_<UTC>` suffix) carry a per-instance source-tuple that does not collide with the parent and are kept.

### Classification logic

This runs once per source event `E` per pdir `(P, D)`. Called from both startup (over the full source list) and per-tick (over the delta).

1. **Already a mirror.** If `E.extendedProperties.private["calendar-sync:source"]` is set, `skip(reason=is_mirror)`. This is the bidirectional loop guard.

2. **Recurring instance.** If `E.recurringEventId` is set, route to the recurring-instance handler (see "Recurring Events"). The handler internally deals with cancelled, transparent, declined, tentative, and updated instances. Generic skip rules in steps 3-7 do NOT apply to recurring instances - the handler subsumes them.

3. **Cancelled (non-recurring).** If `E.status == "cancelled"` (and step 2 didn't fire), look up the mirror in the inventory by `(canonical_source_calendar_id, E.id)`. If found, delete it (action `delete`, reason `source_cancelled`). If not, `skip(reason=cancelled)`.

4. **Declined.** If the source calendar owner's attendee entry has `responseStatus=declined`, `skip(reason=declined)`. (Plus delete the mirror if one exists.)

5. **Tentative.** If the source calendar owner's attendee entry has `responseStatus=tentative`, `skip(reason=tentative)`. (Plus delete the mirror if one exists.) The owner hasn't committed; mirroring it would advertise busy time the owner isn't sure about.

6. **Transparent.** If `E.transparency == "transparent"`, `skip(reason=transparency_transparent)`. (Plus delete the mirror if one exists.)

7. **Outside horizon.** Compute the horizon-eligibility for this event:
   - For non-recurring: the event is in horizon if `start <= now + horizon` (where `start = E.start.dateTime || E.start.date`).
   - For recurring parents (`E.recurrence` is set): the event is in horizon if **any instance falls in `[now, now + horizon]`**. To check, call `events.instances?eventId=<E.id>&calendarId=<S>&timeMin=<now>&timeMax=<now + horizon>&maxResults=1&showDeleted=false`. If the response has at least one instance, the parent is in horizon. If empty, treat as outside_horizon.

   If outside horizon: `skip(reason=outside_horizon)` and delete the mirror if one exists.

8. **Normal reconciliation.** Look up the mirror in the inventory by `(canonical_source_calendar_id, E.id)`:
   - **No mirror in inventory**: compute the deterministic mirror ID from `(canonical_source_calendar_id, E.id)` and `events.insert` on `T` with the full mirror payload (including the deterministic `id`). Action `insert`, reason `source_updated`. Add the post-write resource to the inventory.
     - **On HTTP 409 `duplicate`**: another process inserted the same mirror between our inventory build and our insert (e.g. concurrent `calendar-sync run` racing through the socket exclusion, or a deleted mirror that Google still has the ID reserved for). `events.get?eventId=<deterministic_id>` to fetch the existing mirror. If `status=cancelled`, `events.patch?eventId=<deterministic_id>` with `status=confirmed` plus the full mirror payload to revive it (action `insert`, reason `source_updated`). If alive, treat as "mirror exists" and re-run normal reconciliation against the fetched resource (drift detection etc.). Either way add to inventory.
   - **Mirror exists, `status=cancelled`**: by the time step 8 runs the source has passed steps 3-7 (it's not cancelled, declined, tentative, transparent, or outside-horizon - it's syncable). A cancelled mirror at this point is the leftover of an earlier cancellation cell that has since flipped back. `Status` is not in the managed-field set the checksum hashes, so the standard drift signal would emit `skip(reason=unchanged)` and leave the mirror cancelled forever. `events.patch?eventId=<mirror.id>` with `status=confirmed` plus the full mirror payload to revive it (action `insert`, reason `source_updated`), then run the standard `calendar-sync:checksum` follow-up. Replace the inventory entry with the post-write resource. This shape is symmetric with the post-409 revive subroutine above. Recurring instances follow the same revive shape inside the recurring-instance handler before the four-way matrix runs.

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

If a user moves or cancels an *individual occurrence* of a recurring mirror (creating an override on the mirror side that has no source counterpart), the daemon does not currently propagate the change. The B17 target-delta phase detects the edit (it shows up in the per-target events.list delta) but the dispatch's `events.get` against the source instance returns 404 - there's no existing source override to drive the propagate path against. Phase 1 of B17 emits `skip(reason=mirror_only_override)` for the JSONL stream and stops there.

Why Phase 1 stops:

- The classification logic iterates over source events; with no corresponding source override at the same `originalStartTime`, there's nothing to compare against from the source-delta side.
- Listing all materialized instances on the mirror side via `events.instances` doesn't cleanly distinguish user-modified overrides from auto-generated occurrences without round-trip comparison against the parent's RRULE projection - and Google does not document a "delete this override to restore the auto-generated occurrence" semantic, so a `revert` primitive isn't safely available.
- Drift on the recurring **parent** itself (summary, description, start/end, recurrence rule, etc.) IS detected by the standard reconciliation path. Drift on instances that the source also overrides IS detected by the recurring-instance handler.

Phase 2 (deferred): when the source-instance `events.get` returns 404, `events.patch` the source PARENT'S occurrence (creating the source override) with the mirror's managed fields minus `recurrence`, then re-fetch and dispatch normally. This introduces a new source-side write path and is gated on user demand.

Consequence: until Phase 2 ships, a mirror-only instance override persists until either (a) the source's parent recurrence changes (which triggers a parent re-reconciliation, but doesn't directly clean up the override), (b) the user manually deletes it, or (c) the user runs `calendar-sync mirror prune <calendar> --pair <name>` to remove all of that pdir's mirrors and let them regenerate.

The standard drift detection covers parents and source-corresponding instances, which together cover the vast majority of practical edits.

### Mirror event payload (insert and patch)

```json
{
  "id": "cs2<base32hex_hash>",
  "summary": "<source.summary>",
  "description": "<source.description>\n\n---\nSource: <source.htmlLink>",
  "location": "<source.location>",
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
      "calendar-sync:version": "3"
    }
  }
}
```

Notes:
- `summary`, `description`, and `location` are copied verbatim.
- `description` always ends with a blank line, `---`, and `Source: <htmlLink>`. If source description is empty, just the trailer.
- `start`, `end` preserve the source's `dateTime`/`date` distinction and `timeZone`.
- `transparency` forced to `opaque`. `visibility` forced to `private`.
- `recurrence` is the source's array of RRULE/RDATE/EXDATE strings, omitted for non-recurring events. Omitted on mirror *instances* even when the parent has it.
- `reminders.useDefault = false` is mandatory. Omitting it lets the destination calendar's default reminders fire on every mirror, which is wrong: mirrors should be silent.
- `attendees`, `conferenceData` are deliberately omitted.
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

Every API call is keyed by stable identifiers (source event ID for find, deterministic mirror event ID for insert/patch/delete). The deterministic ID is the de-duplication key: a previous reconciliation that crashed mid-insert leaves either no mirror (next pass inserts cleanly) or the mirror already at its computed ID (next pass `events.get`s it via the 409-duplicate path, then patches or skips). Two processes attempting concurrent insert collide on the same Google event ID and one gets HTTP 409 `duplicate`. No duplicate mirrors are ever created.

A daemon crash mid-tick is recoverable: launchd's `KeepAlive` restarts the process, the cold-start path rebuilds inventories from Google, and reconciliation converges on the same end state.

A manual `calendar-sync run` and the daemon don't interleave: `run` refuses if the daemon is reachable via the IPC socket, and deterministic mirror IDs eliminate duplicate-insert risk if they ever did overlap (the race window between `run`'s socket check and the daemon starting is closed by Google's HTTP 409 on conflicting event IDs).

## Recurring Events

Recurring events mirror as recurring events. The source's `recurrence` array (RRULE/RDATE/EXDATE per RFC 5545) is copied verbatim onto the mirror. Modified and cancelled instances mirror as instance overrides on the mirror series.

### Parent-only recurring events

A recurring source event with no overrides comes back from `events.list?singleEvents=false` exactly once - the parent. It's reconciled exactly like a non-recurring event.

### The recurring-instance handler

When the classification logic's recurring-instance branch routes here, this is the full reconciliation algorithm. It internally handles cancelled, transparent, declined, tentative, and modified cases - all the skip rules that the early-branch bypassed for the generic path.

#### Step 1: find or repair the mirror parent

Look up the mirror parent by `calendar-sync:source = "<S>:<E.recurringEventId>"` on `T`.

If absent (incremental sync can return only the exception when only the exception changed), fetch the source parent via `events.get?calendarId=<S>&eventId=<E.recurringEventId>` and reconcile it through the classification logic's normal-reconciliation step. After that succeeds, the mirror parent exists and we proceed.

If the source parent is now ineligible (status=cancelled, transparency=transparent, declined, tentative, or outside_horizon when checked via `events.instances`), record `skip(reason=parent_not_eligible)` and stop. The mirror parent isn't created, and any prior mirror instance for this exception will be cleaned up by the orphan-prune pass.

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

If step 2 succeeds but step 3 returns an error (the retry `events.instances` fails for any reason - transient 5xx, gws subprocess timeout, etc.), the recurring handler must still surface the post-rewrite mirror parent through its return Result so the sync layer's inventory reflects the completed force-rewrite. Without that propagation, the next tick's classify loop sees a stale inventory entry and re-fires the force-rewrite - bounded only by the next full sync's inventory rebuild. The sync layer applies the post-write inventory updates from the Result (keying the mirror parent by the source PARENT's tuple `(canonical_source_calendar_id, E.recurringEventId)`) before returning the underlying error so the per-event error tolerance (transient skip vs fatal) decision in `runClassifyLoop` operates on a consistent inventory view.

#### Step 3: decide insert/patch/delete/propagate/revert

Apply these rules in order to the source exception `E`. The "user-facing action" reported on stdout is shown alongside.

1. **Cancelled** - If `E.status == "cancelled"`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise call `events.patch` on the mirror instance with `{"status": "cancelled"}`. Action `delete`, reason `source_cancelled`. The API primitive is `patch`, but the effect on the mirror is removal of the busy block, so we report `delete`.
2. **Declined** - If the source calendar owner's attendee entry on `E` has `responseStatus=declined`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise patch the mirror instance to `status=cancelled`. Action `delete`, reason `declined`.
3. **Tentative** - If the source calendar owner's attendee entry on `E` has `responseStatus=tentative`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise patch the mirror instance to `status=cancelled`. Action `delete`, reason `tentative`.
4. **Transparent** - If `E.transparency == "transparent"`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise patch the mirror instance to `status=cancelled`. Action `delete`, reason `transparency_transparent`.
5. **Drift detection on the instance.** Compute the same two signals as for non-recurring events, but on the instance:
   - `source_changed = E.updated > mirror_instance.calendar-sync:source_updated`
   - `mirror_drifted = sha256(canonical(mirror_instance.<managed fields>)) != mirror_instance.calendar-sync:checksum`. If `version < 2` on the mirror instance, derive `mirror_drifted` per the "Schema version migration" rules instead (compare live managed fields to desired-from-source).

   Before the four-way matrix, branch on whether the mirror instance's `calendar-sync:source` matches the source PARENT's tuple (i.e. its EventID equals `E.recurringEventId`). If so, the instance was auto-materialized when calendar-sync wrote the parent and has not been explicitly written; route through the inherited-instance bootstrap path described in "Inherited recurring-instance handling" rather than the standard matrix below.

   Apply the four-way matrix (only for explicitly-managed instances; inherited instances took the branch above):
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
| `access_role_insufficient` | 1 | Config         | A calendar's `accessRole` is too low for its declared role (`reader` for a target, or `freeBusyReader` for either side).                                              |
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
| `daemon_already_running` | 5    | run              | The `watch` daemon is reachable on its IPC socket. Stop it before running a manual reconcile.        |
| `socket_error`           | 1    | status           | Socket file exists but I/O failed for non-`ECONNREFUSED` reasons.                                    |

### Error format on stderr

```json
{"error":"<code>","detail":"<human message>","hint":"<remediation>","cause":"<wrapped lower-level message, optional>"}
```

`cause` is included when the error wraps a subprocess or API failure that has its own message worth preserving.

### Retry policy

The API layer retries on these statuses with exponential backoff (1s, 2s, 4s, 8s, 16s), 5 attempts max, with jitter. `Retry-After` honoring is on the roadmap but not yet implemented: gws does not currently expose the header in its error envelope, so calendar-sync's retry layer cannot consult it. The fixed exponential schedule applies regardless.

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
refuse if `calendar-sync watch` is reachable via the IPC socket (exit 5, daemon_already_running)
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

A pdir is considered successful when every event in its delta either classified cleanly or hit a transient read error (per §"Per-event transient read tolerance"). Any fatal per-event error fails the pdir, gates token advancement, and contributes to `_meta.failures`.

## State

calendar-sync persists nothing to disk except `config.toml`. All sync state lives in process memory inside `calendar-sync watch`. There's no `state.json`, no lock file, no per-pdir checkpoint file.

What lives in process memory:

- Canonical calendar IDs and `accessRole`s for every calendar referenced in config (resolved at startup).
- Per-source `syncToken` (one token per unique source calendar; pdirs sharing a source share the token).
- Per-source last-full-sync timestamp.
- Per-target mirror inventory (a map from `(canonical_source_calendar_id, source_event_id)` tuple to live Event resource).

What survives across daemon restarts:

- `extendedProperties.private` on every mirror event. The provenance (`calendar-sync:source`, `calendar-sync:source_updated`, `calendar-sync:checksum`, `calendar-sync:version`) is colocated with the mirror itself on Google Calendar. A cold start re-derives the in-memory inventory by running the "Mirror inventory rebuild" subroutine (one `events.list` call per known schema version per target).

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

Look up a specific mirror by deterministic ID (the common path):
```
events.get?calendarId=<T>&eventId=<deterministic_mirror_id>
```

Find every mirror calendar-sync has ever created on a calendar (used by the inventory rebuild and bulk operations):
```
events.list?calendarId=<T>&privateExtendedProperty=calendar-sync:version=3
events.list?calendarId=<T>&privateExtendedProperty=calendar-sync:version=2
events.list?calendarId=<T>&privateExtendedProperty=calendar-sync:version=1
```

All are single-property queries (or single-event lookups).

## Out of scope

The following are intentionally not part of this version. None require structural changes to the spec to add later, but each is a deliberate non-goal here:

- **Multi-account support.** calendar-sync uses whatever account `gws auth status` reports. No `[accounts.<name>]` config table, no `--account` flag. Users with calendars across multiple Google accounts share calendars between accounts (Google Calendar's sharing UI) so a single `gws`-authenticated account has access to everything.
- **Webhook push notifications via `events.watch`.** Requires a verified HTTPS domain in Google Search Console and a publicly-reachable always-on endpoint. Doesn't fit a laptop deployment. Polling with `syncToken` gets near-real-time latency at low API cost on a long-running daemon, which is sufficient.
- **Per-pair redaction modes.** Mirror events copy source title and description verbatim. There are no `title_template`, `redact_description`, or similar config knobs. Users who need redaction control writer access to the destination calendar instead.
- **Mirror-only recurring instance overrides (Phase 2).** A user dragging one occurrence of a recurring mirror to a different time creates an override the source doesn't have. B17 Phase 1 detects the edit at tick rate and emits `skip(reason=mirror_only_override)` but doesn't propagate; Phase 2 (deferred) would create the source override and propagate. Workaround until Phase 2: `calendar-sync mirror prune <calendar> --pair <name>` and let the next sync regenerate. See "Limitation: mirror-only recurring instance overrides" in Sync Algorithm.
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
