# calendar-sync Specification

A Google Calendar event mirroring tool. Replaces the calendar-syncing piece of Reclaim.ai.

For each user-declared pair of calendars, calendar-sync mirrors busy events from one to the other (or both directions) so events on calendar A appear as private busy blocks on calendar B. Updates and deletions propagate. Recurring events mirror as recurring events with their RRULE intact, not as expanded instances.

## Design Principles

- **Google Workspace only.** No iCloud, no Outlook, no CalDAV. All Google Calendar interactions go through the `gws` CLI; calendar-sync never holds OAuth credentials directly.
- **Configuration-driven.** Every calendar pair, account, and tunable lives in a TOML config file. No hardcoded calendars or accounts.
- **Polling, not push.** Phase 1 uses incremental polling via Google's `syncToken`. Webhooks (`events.watch`) are deferred until cloud deployment because they need a verified HTTPS domain and an always-on host.
- **State on events, not on disk.** Mirror provenance lives in `extendedProperties.private` on each mirror event. The on-disk state is one syncToken plus one full-sync timestamp per pair-direction.
- **One-shot by default.** `calendar-sync run` performs a single sync and exits. macOS launchd drives the cadence. A long-running daemon mode is a non-goal for Phase 1.
- **Idempotent and overlap-safe.** Running `calendar-sync run` twice in a row with no calendar changes is a no-op. A second invocation while one is still running exits cleanly without doing anything.
- **Read-only on source calendars.** The tool reads source events but never writes to them. Mirror events live exclusively on destination calendars.

## Architecture

### Components

```
+--------------------+     +----------------+     +---------------------+
|  calendar-sync run | --> | gws (subproc)  | --> | Google Calendar API |
+--------------------+     +----------------+     +---------------------+
         |
         |  reads/writes
         v
+----------------------------+
| ~/.config/calendar-sync/   |
|   config.toml              |
|   state.json (per-pdir)    |
|   <state_file>.lock        |
+----------------------------+
```

calendar-sync is a Go binary that shells out to `gws calendar events ...` for every Calendar API operation. It does not use a Go SDK for Google APIs. Auth and OAuth concerns stay inside `gws` where the user has already invested setup.

### The unit of sync: pair-direction

The atomic unit calendar-sync operates on is a **pair-direction (pdir)**: a `(pair_name, direction)` tuple where direction is `a_to_b` or `b_to_a`. A bidirectional pair expands to two pdirs. A unidirectional pair expands to one. Every piece of sync state is keyed by pdir.

This is intentional and corrects an early design where state was keyed by source calendar. Source-keyed state breaks when a single source fans out to multiple destinations: advancing the syncToken after one destination starves the others, and a partial failure on any destination loses events forever.

Per-pdir state is independent: pdir X failing doesn't prevent pdir Y from advancing its token, even if X and Y share a source calendar.

### Mirror identification

Every mirror event carries these private extended properties (`extendedProperties.private`):

| Key                            | Value example                              | Purpose                                                                                                          |
|--------------------------------|--------------------------------------------|------------------------------------------------------------------------------------------------------------------|
| `calendar-sync:source`         | `alice@example.com:abc123def456`        | `<canonical_source_calendar_id>:<source_event_id>`. Unique on a target calendar. Used to find a specific mirror. |
| `calendar-sync:source_updated` | `2026-04-29T23:00:00Z`                     | The source event's `updated` field at the time of the last reconciliation. Drift heuristic.                      |
| `calendar-sync:scope`          | `work-personal:a_to_b`                     | `<pair>:<direction>`. Composite for bulk listing by pdir. (See "Why composite" below.)                           |
| `calendar-sync:pair`           | `work-personal`                            | Pair name. Stored separately for human-readable output.                                                          |
| `calendar-sync:direction`      | `a_to_b`                                   | Direction. Stored separately for human-readable output.                                                          |
| `calendar-sync:version`        | `1`                                        | Schema version. Bump if the property layout changes.                                                             |

#### Why composite `scope`

Google's events.list documentation is internally inconsistent about whether multiple `privateExtendedProperty` parameters are AND'd or OR'd. To stay on documented-and-stable ground, calendar-sync only ever queries on a **single** extended property. The composite `scope` lets us fetch all mirrors of a pdir in one single-property query without depending on AND-of-multi semantics.

#### Canonical calendar IDs

The `calendar-sync:source` property uses the **canonical** calendar ID, never the alias `primary`. At config-load time, calendar-sync resolves every calendar reference (including `primary`) to its canonical ID via `gws calendar calendarList get`. Canonicalized IDs are used everywhere downstream: state file keys, extended properties, log fields. This survives config reorderings and Phase-2 multi-account migration.

#### Loop prevention

A source event is "already a mirror" if it carries `calendar-sync:source` in its extended properties. Such events are skipped when scanning a calendar as a *source*. This prevents bidirectional pairs from re-mirroring their own output.

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
state_file = "~/.config/calendar-sync/state.json"

# Phase 1 supports a single account named "default". The [accounts]
# section is keyed in anticipation of Phase 2 multi-account.
[accounts.default]
gws_config_dir = "~/.config/gws"

[[pairs]]
name = "work-personal"
direction = "bidirectional"
source = { account = "default", calendar = "alice@example.com" }
target = { account = "default", calendar = "primary" }

[[pairs]]
name = "work-family"
direction = "source_to_target"
source = { account = "default", calendar = "alice@example.com" }
target = { account = "default", calendar = "family@group.calendar.google.com" }
```

### Schema

#### `[settings]`

| Field                | Type     | Default                              | Description                                                                                                       |
|----------------------|----------|--------------------------------------|-------------------------------------------------------------------------------------------------------------------|
| `poll_interval`      | duration | `60s`                                | Cadence for the launchd plist. Used by `calendar-sync install`. Min `30s`.                                        |
| `horizon`            | duration | `365d`                               | How far ahead to mirror. Source events with `start > now + horizon` are skipped at apply time.                    |
| `full_sync_interval` | duration | `24h`                                | Force a full re-list (ignoring saved syncToken) at least this often per pdir. Bounds the sliding-horizon problem. |
| `log_level`          | string   | `info`                               | One of `debug`, `info`, `warn`, `error`.                                                                          |
| `log_format`         | string   | `json`                               | One of `json` (JSONL to stderr), `text` (human-readable to stderr).                                               |
| `state_file`         | path     | `~/.config/calendar-sync/state.json` | Where to persist per-pdir state. Tilde-expanded; parent directory created on demand.                              |
| `dry_run`            | bool     | `false`                              | If true, log what would change but make no API writes. Equivalent to passing `--dry-run` to every `run`.          |

Duration strings follow Go's `time.ParseDuration` syntax (`30s`, `5m`, `24h`) plus `d` (days) which calendar-sync adds.

#### `[accounts.<name>]`

Each entry under `[accounts]` declares a gws identity.

| Field            | Type | Default          | Description                                                                                              |
|------------------|------|------------------|----------------------------------------------------------------------------------------------------------|
| `gws_config_dir` | path | `~/.config/gws`  | Directory passed to `gws` via the `XDG_CONFIG_HOME` environment override when invoked for this account.  |

Phase 1 honors only the account named `default`. Pairs that reference any other account name fail validation. Phase 2 lifts this.

#### `[[pairs]]`

| Field         | Type     | Required  | Description                                                                                                                                                       |
|---------------|----------|-----------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `name`        | string   | yes       | Unique. Used as the `calendar-sync:pair` extended property and in logs. Must match `^[a-z0-9][a-z0-9-]{0,62}$`.                                                  |
| `direction`   | string   | yes       | One of `source_to_target`, `target_to_source`, `bidirectional`.                                                                                                   |
| `source`      | table    | yes       | `{ account = "<name>", calendar = "<id>" }`. The "left" calendar.                                                                                                 |
| `target`      | table    | yes       | `{ account = "<name>", calendar = "<id>" }`. The "right" calendar.                                                                                                |
| `enabled`     | bool     | no (true) | If false, the pair is skipped entirely.                                                                                                                           |
| `time_zone`   | string   | no        | IANA name (e.g. `America/New_York`). Used as the `timeZone` on mirrored events when the source event is all-day. Defaults to the destination calendar's default. |

Calendar IDs accepted: a calendar email (`alice@example.com`), the literal `primary` (which calendar-sync resolves to its canonical ID), or a group calendar ID (`<hash>@group.calendar.google.com`).

#### Validation rules

Run on every command that touches config. Failures exit with code 1 and a JSON error to stderr (see Output).

- `name` is unique across all pairs.
- `direction` is one of the three allowed values (case-sensitive).
- `source.account` and `target.account` are both declared in `[accounts]`.
- After canonicalization, `source.calendar != target.calendar`. Mirroring a calendar to itself is rejected.
- After canonicalization and pdir expansion, no two pdirs share the same `(canonical_source, canonical_target, direction)` triple. Two pdirs writing identical mirrors to the same calendar is a configuration bug.
- `poll_interval >= 30s`.
- `horizon` is between `1d` and `730d` inclusive.
- `full_sync_interval` is between `1h` and `30d` inclusive.
- `log_level` is one of the four allowed values.
- `log_format` is `json` or `text`.

### Examples

#### Minimal: one bidirectional pair

```toml
[accounts.default]

[[pairs]]
name = "primary-pair"
direction = "bidirectional"
source = { account = "default", calendar = "alice@example.com" }
target = { account = "default", calendar = "primary" }
```

#### Three pairs, mixed directions

```toml
[settings]
poll_interval = "60s"
horizon = "365d"

[accounts.default]

[[pairs]]
name = "work-personal"
direction = "bidirectional"
source = { account = "default", calendar = "alice@example.com" }
target = { account = "default", calendar = "primary" }

[[pairs]]
name = "work-family"
direction = "source_to_target"
source = { account = "default", calendar = "alice@example.com" }
target = { account = "default", calendar = "family@group.calendar.google.com" }

[[pairs]]
name = "personal-family"
direction = "source_to_target"
source = { account = "default", calendar = "primary" }
target = { account = "default", calendar = "family@group.calendar.google.com" }
enabled = false
```

## Authentication

calendar-sync delegates all authentication to `gws`.

### Phase 1 (single account)

- The user runs `gws auth login` once.
- `~/.config/gws/credentials.enc` holds the encrypted token; `gws` refreshes it transparently.
- calendar-sync invokes `gws` as a subprocess. Whatever account `gws auth status` reports is what calendar-sync uses.

The user is responsible for ensuring this single account has **read+write access to every calendar referenced in the config**. For the typical mixed work/personal use case, this means sharing the personal Google calendar with the work account (or vice versa) via Google Calendar's calendar-sharing UI before configuring calendar-sync.

`calendar-sync run` calls `gws auth status` once at startup and exits with code 2 if it returns non-zero.

### Phase 2 (multi-account)

Each `[accounts.<name>]` entry declares its own `gws_config_dir`. calendar-sync sets `XDG_CONFIG_HOME=<dir>` when shelling out for that account. The user runs `XDG_CONFIG_HOME=<dir> gws auth login` once per account, or uses the wrapper `calendar-sync auth login --account <name>`.

## Privacy and the mirror payload

By design, mirror events copy the source event's title and description verbatim. This is *not* a redaction tool. The destination calendar's writers and owners can read those details. The `visibility=private` setting only hides details from readers.

This is intentional: the user creating the pairs controls the destination calendars and the people who have writer access to them. In the typical case (mirroring between calendars the user owns), the only readers are the user and people they've explicitly shared with. If the user mirrors to a calendar with broader writer access, the leak is the user's responsibility.

Phase 2 will offer redaction modes (`title_template`, `redact_description`) for users who need them.

## Output and Logging

### stdout

Most commands produce JSONL (newline-delimited JSON) ending with a `_meta` trailer:

```
$ calendar-sync pair list
{"name":"work-personal","direction":"bidirectional","source":{"account":"default","calendar":"alice@example.com"},"target":{"account":"default","calendar":"primary"},"enabled":true}
{"name":"work-family","direction":"source_to_target","source":{"account":"default","calendar":"alice@example.com"},"target":{"account":"default","calendar":"family@group.calendar.google.com"},"enabled":true}
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
{"error":"config_invalid","detail":"pair 'work-personal' references undeclared account 'work'","hint":"Add an [accounts.work] section to your config","cause":"<wrapped, optional>"}
```

### Exit codes

| Code | Meaning              | When                                                                                          |
|------|----------------------|-----------------------------------------------------------------------------------------------|
| 0    | Success              | Command ran to completion. For `run`, all pdirs synced or the lock-already-held shortcut hit. |
| 1    | General error        | Config invalid, gws subprocess failed for non-auth reasons, partial sync failure.             |
| 2    | Auth error           | `gws auth status` reports unauthenticated, or 401 returned from a Calendar API call.          |
| 3    | Rate limited         | Hit retry ceiling (5 retries, exponential backoff with jitter, respects `Retry-After`).       |
| 4    | Network error        | DNS failure, connection refused, TLS error.                                                   |
| 5    | State corrupt        | `state.json` is unreadable or malformed and `--reset-state` was not passed.                   |
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

### calendar-sync run

Perform one sync pass across all enabled pdirs and exit. This is the command launchd invokes.

```
calendar-sync run [flags]
  --pair <name>        Sync only the named pair. May be repeated. Default: all enabled pairs.
  --direction <dir>    Limit to one direction within each pair. One of a_to_b, b_to_a. Default: both where applicable.
  --dry-run            Plan and print actions but make no API writes. Reads still happen.
  --reset-state        Discard saved syncTokens before running. Forces a full re-list of every relevant pdir.
  --no-prune           Skip the orphaned-mirror cleanup pass.
  --timeout <dur>      Wall-clock cap for the entire command. Default: 5m.
```

Stdout: one JSON object per action plus `_meta`.

```
$ calendar-sync run
{"action":"insert","pair":"work-personal","direction":"a_to_b","source_event":"abc123","target_event":"def456","summary":"Standup"}
{"action":"patch","pair":"work-personal","direction":"a_to_b","source_event":"abc124","target_event":"def457","reason":"source_updated"}
{"action":"delete","pair":"work-personal","direction":"a_to_b","target_event":"def458","reason":"source_cancelled"}
{"action":"skip","pair":"work-personal","direction":"a_to_b","source_event":"abc125","reason":"transparency_transparent"}
{"_meta":{"pdirs":2,"events_processed":18,"inserts":1,"patches":1,"deletes":1,"skips":15,"duration_ms":1842}}
```

A given `reason` is paired with one of four `action` values: `insert`, `patch`, `delete`, `skip`. Some reasons can produce different actions depending on whether a mirror exists. The following table is exhaustive.

| `reason`                  | Possible `action` values | Trigger                                                                                                                                  |
|---------------------------|--------------------------|------------------------------------------------------------------------------------------------------------------------------------------|
| `is_mirror`               | `skip`                   | Source already carries `calendar-sync:source` (bidirectional loop guard).                                                                |
| `cancelled`               | `skip`                   | Source `status=cancelled` and no mirror exists to delete.                                                                                |
| `source_cancelled`        | `delete`                 | Source `status=cancelled` and a mirror exists.                                                                                           |
| `declined`                | `skip` or `delete`       | Source calendar owner's `responseStatus=declined`. `delete` if a mirror exists, `skip` if not.                                           |
| `transparency_transparent`| `skip` or `delete`       | Source `transparency=transparent`. `delete` if a mirror exists, `skip` if not.                                                           |
| `outside_horizon`         | `skip` or `delete`       | Non-recurring source `start > now + horizon`, or recurring source has no instance in `[now, now + horizon]`. `delete` if mirror exists.  |
| `parent_not_eligible`     | `skip`                   | A recurring instance arrived but its source parent is itself filtered out (cancelled, transparent, declined, outside_horizon).           |
| `unchanged`               | `skip`                   | Mirror is up-to-date relative to source.                                                                                                  |
| `pair_disabled`           | `skip`                   | The pdir is `enabled=false`. Emitted only when the user explicitly named the pair via `--pair`.                                          |
| `instance_unmaterializable`| `skip`                  | Recurring-instance lookup returned zero results even after re-patching the mirror parent (rare; see "Zero-result instance lookup" below). |
| `source_updated`          | `insert` or `patch`      | Source `updated` moved past the recorded value, or no mirror exists yet (then `insert`). Covers source-side recurrence-rule changes too - the parent patch carries the new `recurrence`.  |
| `orphaned`                | `delete`                 | Prune pass found a mirror whose source no longer exists.                                                                                 |

Server-side `eventTypes` filtering means events of excluded types (`birthday`, `fromGmail`, `workingLocation`) never appear on the wire and so don't produce a `skip` event.

The recurring-instance handler uses `events.patch` (with `status=cancelled`) under the hood for cancellation cases, but at the CLI layer those still report as `action=delete`. The user-facing action describes effect on the mirror, not the API primitive.

#### Overlap protection

`run` acquires an advisory lock via `flock(LOCK_EX | LOCK_NB)` immediately after argument parsing. The lock file lives at `<state_file>.lock` (so it sits next to the state file - default `~/.config/calendar-sync/state.json.lock`). Scoping the lock to the state file means two unrelated calendar-sync configs (different `--config`, different `state_file`) on the same machine don't serialize each other.

If the lock is held by another `calendar-sync run` process for the same state file, the new invocation logs `already_running` to stderr at level `info` and exits **0** (this is normal, not a failure). The launchd cycle just lost a tick; the next tick picks up.

The lock is released automatically when the process exits, including on crash.

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
| `state_corrupt`       | 5    | `state.json` is unreadable. Use `--reset-state` to discard.                           |
| `partial_failure`     | 1    | Some pdirs succeeded, others failed. `_meta.failures` lists them.                     |
| `timeout`             | 1    | Exceeded `--timeout`.                                                                 |

`run` does not abort on the first error. Each pdir is tried independently. A single pdir failure causes exit 1 at the end with `partial_failure`, but every other pdir gets its chance and saves its own state on success.

#### Examples

```
# Normal launchd-driven invocation
calendar-sync run

# Test config without writing
calendar-sync run --dry-run

# Force a full re-list everywhere
calendar-sync run --reset-state

# Sync only one pair
calendar-sync run --pair work-personal

# Sync only one direction of one pair
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
{"settings":{"poll_interval":"60s","horizon":"365d","full_sync_interval":"24h","log_level":"info","log_format":"json","state_file":"~/.config/calendar-sync/state.json","dry_run":false},"accounts":{"default":{"gws_config_dir":"~/.config/gws"}},"pairs":[{"name":"work-personal","direction":"bidirectional","source":{"account":"default","calendar":"alice@example.com"},"target":{"account":"default","calendar":"primary"},"enabled":true}]}
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
{"status":"ok","pairs":2,"pdirs":3,"accounts":1}
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
{"name":"work-personal","direction":"bidirectional","source":{"account":"default","calendar":"alice@example.com"},"target":{"account":"default","calendar":"primary"},"enabled":true}
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
  --account <name>     Account that owns the calendar. Default: default.
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
  --account <name>     Account that owns the calendar. Default: default.
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

### calendar-sync state show

```
calendar-sync state show
```

Stdout:

```
$ calendar-sync state show
{"pdir":"work-personal:a_to_b","source_calendar":"alice@example.com","target_calendar":"alice.personal@example.org","checkpoint":{"sync_token":"CPDC...","full_sync_at":"2026-04-29T03:00:00Z","query_fingerprint":"sha256:abc..."},"last_attempt_at":"2026-04-29T23:00:00Z","last_synced_at":"2026-04-29T23:00:00Z","last_status":"ok","last_error":null}
{"pdir":"work-personal:b_to_a","source_calendar":"alice.personal@example.org","target_calendar":"alice@example.com","checkpoint":{"sync_token":"CMD3...","full_sync_at":"2026-04-29T03:00:00Z","query_fingerprint":"sha256:abc..."},"last_attempt_at":"2026-04-29T23:00:00Z","last_synced_at":"2026-04-29T23:00:00Z","last_status":"ok","last_error":null}
{"_meta":{"count":2}}
```

If `state.json` does not exist: just `{"_meta":{"count":0}}`. Exit 0.

#### Errors

| Error code      | Exit | When                                  |
|-----------------|------|---------------------------------------|
| `state_corrupt` | 5    | `state.json` exists but won't parse.  |

### calendar-sync state reset

Clear saved sync state.

```
calendar-sync state reset [flags]
  --pair <name>     Only clear pdirs of this pair.
  --direction <dir> With --pair, limit to a_to_b or b_to_a.
  --pdir <id>       Clear a specific pdir, e.g. "work-personal:a_to_b". May be repeated.
  --yes, -y         Skip the interactive confirmation (required on non-TTY).
```

Without selectors, every entry in `state.json` is cleared. Confirmation is required.

```
$ calendar-sync state reset --pair work-personal --yes
{"pdir":"work-personal:a_to_b","cleared":true}
{"pdir":"work-personal:b_to_a","cleared":true}
{"_meta":{"count":2}}
```

#### Errors

| Error code              | Exit | When                                                      |
|-------------------------|------|-----------------------------------------------------------|
| `pair_not_found`        | 1    | `--pair <name>` doesn't match.                            |
| `pdir_not_found`        | 1    | `--pdir <id>` doesn't match a configured pdir.            |
| `confirmation_required` | 1    | Non-TTY without `--yes`.                                  |
| `state_corrupt`         | 5    | Existing `state.json` won't parse and reset isn't full.   |

### calendar-sync install

Install the launchd agent.

```
calendar-sync install [flags]
  --interval <dur>   Override settings.poll_interval. Min 30s.
  --log-dir <path>   Where launchd writes stdout/stderr. Default: ~/Library/Logs/calendar-sync/.
  --label <id>       launchd Label. Default: org.calendar-sync.agent.
  --force            Overwrite an existing plist.
  --no-load          Write the plist but don't `launchctl load` it.
```

```
$ calendar-sync install
{"plist":"/Users/alice/Library/LaunchAgents/org.calendar-sync.agent.plist","interval":"60s","loaded":true}
{"_meta":{"count":1}}
```

The plist sets `StartInterval` to the resolved interval (in seconds), `ProgramArguments` to `[<absolute path to calendar-sync>, "run"]`, and `EnvironmentVariables` includes `PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin` so `gws` is found.

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

### Inputs

For pdir `(P, D)`:
- Canonical source calendar ID `S`.
- Canonical target calendar ID `T`.
- `state.json["<P>:<D>"].checkpoint`: `sync_token`, `full_sync_at`, `query_fingerprint`. The whole `checkpoint` object may be absent (first run) or present with empty `sync_token` (Google didn't return one on the previous full sync).
- `settings.horizon`, `settings.full_sync_interval`.
- The current query fingerprint = SHA-256 of the canonical concatenation of `S`, `sorted(eventTypes)`, and `horizon` as exact nanoseconds. Any change to any input invalidates a saved syncToken.

### Sliding-horizon protection

A naïve syncToken loop misses events that enter the horizon by *passage of time*, not by being modified. calendar-sync handles this with a periodic forced full re-sync:

A pdir runs in **full-sync mode** if any of these are true:
- `state["<P>:<D>"]` is absent or has no `checkpoint`.
- `--reset-state` was passed.
- `state["<P>:<D>"].checkpoint.sync_token` is empty.
- `state["<P>:<D>"].checkpoint.query_fingerprint != current fingerprint` (config changed in a way that affects the source query).
- `now - state["<P>:<D>"].checkpoint.full_sync_at > settings.full_sync_interval` (default 24h).

Otherwise, **incremental mode** uses the saved syncToken.

### Phase 1: list source events

#### Full-sync mode

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

After all pages succeed, capture `nextSyncToken` from the last page. If it's missing (which Google can do for very long full lists), record `sync_token = ""` so the next run is also a full sync.

#### Incremental mode

```
gws calendar events list --params '{
  "calendarId": "<S>",
  "syncToken": "<saved>",
  "showDeleted": true,
  "eventTypes": ["default", "outOfOffice", "focusTime"],
  "maxResults": 250
}' --page-all
```

`timeMin`, `timeMax`, `updatedMin`, `q`, `iCalUID`, `orderBy`, and the `privateExtendedProperty` / `sharedExtendedProperty` filters are all rejected by Google when sent alongside `syncToken`. Other parameters (including `singleEvents`, `eventTypes`, `showDeleted`) must match the values used in the initial full sync that produced the token. If they differ, behavior is undefined per Google; calendar-sync's `query_fingerprint` invalidation prevents this by forcing a full re-sync on any change.

If any page returns 410 GONE, abort the syncToken path, switch to full-sync mode, and retry within the same `run`. Don't surface 410 to the user as an error.

`singleEvents=false` is critical: recurring events return as a single parent (with `recurrence` set), not as N expanded instances. Modified instances come back as separate items with `recurringEventId` and `originalStartTime` set.

### Phase 2: classify and reconcile

For each event `E` returned, in `events.list` order:

1. **Already a mirror.** If `E.extendedProperties.private["calendar-sync:source"]` is set, `skip(reason=is_mirror)`. This is the bidirectional loop guard.

2. **Recurring instance.** If `E.recurringEventId` is set, route to the recurring-instance handler (see "Recurring Events"). The handler internally deals with cancelled, transparent, declined, and updated instances. Generic skip rules in steps 3-6 do NOT apply to recurring instances - the handler subsumes them.

3. **Cancelled (non-recurring).** If `E.status == "cancelled"` (and step 2 didn't fire), look up the mirror by `calendar-sync:source = "<S>:<E.id>"` on `T`. If found, delete it (`reason=source_cancelled`). If not, `skip(reason=cancelled)`.

4. **Declined.** If the source calendar owner's attendee entry has `responseStatus=declined`, `skip(reason=declined)`. (Plus delete the mirror if one exists.)

5. **Transparent.** If `E.transparency == "transparent"`, `skip(reason=transparency_transparent)`. (Plus delete the mirror if one exists.)

6. **Outside horizon.** Compute the horizon-eligibility for this event:
   - For non-recurring: the event is in horizon if `start <= now + horizon` (where `start = E.start.dateTime || E.start.date`).
   - For recurring parents (`E.recurrence` is set): the event is in horizon if **any instance falls in `[now, now + horizon]`**. To check, call `events.instances?eventId=<E.id>&calendarId=<S>&timeMin=<now>&timeMax=<now + horizon>&maxResults=1&showDeleted=false`. If the response has at least one instance, the parent is in horizon. If empty, treat as outside_horizon.
   
   If outside horizon: `skip(reason=outside_horizon)` and delete the mirror if one exists.

7. **Normal reconciliation.** Look up the existing mirror by `calendar-sync:source = "<S>:<E.id>"`:
   - **No mirror**: `events.insert` on `T` with the mirror payload. Action: `insert`.
   - **Mirror exists, source updated**: if `E.updated > mirror.extendedProperties.private["calendar-sync:source_updated"]`, `events.patch` on `T` with the full mirror payload (`reason=source_updated`). Action: `patch`. If the source's `recurrence` array differs from the mirror's, the patch covers it; no separate fanout is needed (see "Recurring Events" / "Parent recurrence rule changes").
   - **Mirror exists, unchanged**: `skip(reason=unchanged)`.

The order of 3-6 doesn't change correctness for non-recurring events (the rules are non-overlapping for the cases that matter), but step 2's early branch is critical: a transparent or declined recurring instance must reach the recurring-instance handler so the mirror instance gets deleted, not skipped.

### Phase 3: prune orphans

Skipped if `--no-prune` was passed or the run was incremental-only (incremental responses include cancellations explicitly, so orphans can't accumulate).

In full-sync mode after Phase 2:

1. List every mirror on `T` for this pdir: `events.list?calendarId=<T>&privateExtendedProperty=calendar-sync:scope=<P>:<D>&maxResults=250` (paginated, all pages). Single-property query, safe.
2. Build a set of source IDs the run actually saw in Phase 2.
3. For each mirror not in that set, parse `calendar-sync:source` to recover `<source_id>` and look up the source via `events.get?calendarId=<S>&eventId=<source_id>`:
   - **Source returns 404 or has `status=cancelled`**: delete the mirror (`reason=orphaned`).
   - **Source is non-recurring and `start > now + horizon`**: delete the mirror (`reason=outside_horizon`).
   - **Source is a recurring parent (has `recurrence`)**: don't trust `start` (which is the series start). Call `events.instances?calendarId=<S>&eventId=<source_id>&timeMin=<now>&timeMax=<now + horizon>&maxResults=1&showDeleted=false`. If the response has zero instances, the series no longer generates anything in our window: delete the mirror (`reason=outside_horizon`). If it has at least one instance, leave the mirror alone.
   - **Source exists and is in horizon**: leave the mirror alone. (Defensive: a non-overlapping race could explain why Phase 2 didn't see it; the next full sync re-checks.)

Source lookups during prune (both `events.get` and any follow-up `events.instances`) fan out with a semaphore of 5.

### Phase 4: persist state

Always written for every pdir attempted, success or failure:
- `state["<P>:<D>"].last_attempt_at = now`.
- `state["<P>:<D>"].last_status = "ok"` on success, otherwise the error code.
- `state["<P>:<D>"].last_error = null` on success, otherwise the error JSON.

Written only on success of Phase 1, 2, and 3:
- `state["<P>:<D>"].checkpoint.sync_token = nextSyncToken` (may be empty if Google didn't return one; next run will re-do a full sync).
- If full-sync mode: `state["<P>:<D>"].checkpoint.full_sync_at = now`.
- `state["<P>:<D>"].checkpoint.query_fingerprint = current fingerprint`.
- `state["<P>:<D>"].last_synced_at = now`.

The split is deliberate. The attempt log lets `state show` answer "did the last sync work?" without needing a separate status file. The checkpoint only advances on success, so a failed pdir retries from the same starting point on the next run.

State is written via tempfile + fsync + rename. There's exactly one `state.json` write at the end of `run`, after every pdir has been processed.

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
      "calendar-sync:scope": "<P>:<D>",
      "calendar-sync:pair": "<P>",
      "calendar-sync:direction": "<D>",
      "calendar-sync:version": "1"
    }
  }
}
```

Notes:
- `summary` and `description` are copied verbatim.
- `description` always ends with a blank line, `---`, and `Source: <htmlLink>`. If source description is empty, just the trailer.
- `start`, `end` preserve the source's `dateTime`/`date` distinction and `timeZone`.
- `transparency` forced to `opaque`. `visibility` forced to `private`.
- `recurrence` is the source's array of RRULE/RDATE/EXDATE strings, omitted for non-recurring events.
- `reminders.useDefault = false` is mandatory. Omitting it lets the destination calendar's default reminders fire on every mirror, which is wrong: mirrors should be silent.
- `attendees`, `location`, `conferenceData` are deliberately omitted.

### Concurrency within a run

- Pdirs in this run are processed serially in Phase 1. (Parallelism is a Phase 2 optimization once we have wall-clock numbers.)
- Within a pdir, events are processed serially.
- Orphan-prune source lookups fan out with a semaphore of 5.

### Idempotency

Every API call is keyed by stable identifiers (source event ID for find, mirror event ID for patch/delete). The mirror's `calendar-sync:source` extended property is the de-duplication key: a previous run that crashed mid-insert leaves either no mirror (next run inserts) or a mirror with the marker (next run patches/skips). No duplicates.

The advisory lock guarantees no two `run` invocations interleave on the same machine. If a user invokes `run` while launchd's instance is in flight, the second invocation exits 0 with `already_running` and does nothing.

## Recurring Events

Recurring events mirror as recurring events. The source's `recurrence` array (RRULE/RDATE/EXDATE per RFC 5545) is copied verbatim onto the mirror. Modified and cancelled instances mirror as instance overrides on the mirror series.

### Parent-only recurring events

A recurring source event with no overrides comes back from `events.list?singleEvents=false` exactly once - the parent. It's reconciled exactly like a non-recurring event.

### The recurring-instance handler

When Phase 2 step 2 routes a recurring instance here, this is the full reconciliation algorithm. It internally handles cancelled, transparent, declined, and modified cases - all the skip rules that step 2 bypassed for the generic path.

#### Step 1: find or repair the mirror parent

Look up the mirror parent by `calendar-sync:source = "<S>:<E.recurringEventId>"` on `T`.

If absent (incremental sync can return only the exception when only the exception changed), fetch the source parent via `events.get?calendarId=<S>&eventId=<E.recurringEventId>` and reconcile it as a normal event first using Phase 2's step 7 logic. After that succeeds, the mirror parent exists and we proceed.

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

#### Step 3: decide insert/patch/delete

Apply these rules in order to the source exception `E`. The "user-facing action" reported on stdout is shown alongside.

1. **Cancelled** - If `E.status == "cancelled"`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise call `events.patch` on the mirror instance with `{"status": "cancelled"}` (`action=delete`, `reason=source_cancelled`). The API primitive is `patch`, but the effect on the mirror is removal of the busy block, so we report `delete`.
2. **Declined** - If the source calendar owner's attendee entry on `E` has `responseStatus=declined`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise patch the mirror instance to `status=cancelled` (`action=delete`, `reason=declined`).
3. **Transparent** - If `E.transparency == "transparent"`: if the mirror instance is already cancelled, `skip(reason=unchanged)`. Otherwise patch the mirror instance to `status=cancelled` (`action=delete`, `reason=transparency_transparent`).
4. **Modified, content changed** - Otherwise compare `E.updated` to the mirror instance's `calendar-sync:source_updated` (via the mirror instance's extended properties). If `E.updated >` the recorded value, `events.patch` the mirror instance with the full mirror payload (`action=patch`, `reason=source_updated`).
5. **Otherwise** `skip(reason=unchanged)`.

The mirror payload for an instance patch is the same shape as for non-recurring events except:
- `recurrence` is omitted (instances don't have their own recurrence; they belong to the parent).
- `extendedProperties.private["calendar-sync:source"]` is updated to `"<S>:<E.id>"` (the exception's own ID, not the parent's) so a direct lookup later finds the right instance.

#### What this handler does *not* cover

- The source parent itself when it changes. That's Phase 2 step 7's normal reconciliation. The handler only gets called for instances (events with `recurringEventId` set).
- Source-level recurrence rule changes. Those reach Phase 2 step 7 as a `source_updated` patch on the parent. See "Parent recurrence rule changes" below.

### Parent recurrence rule changes

When a source parent's `recurrence` array changes, Phase 2 patches the mirror parent with the new recurrence as part of a regular `source_updated` patch. That's the entire reconciliation step for the parent.

There is no separate instance-fanout pass on a recurrence change. Justification:

- Auto-generated instances (those with no override) are not standalone resources. They're materialized by the parent's recurrence rule. When the parent's `recurrence` changes, the set of auto-generated instances changes for free, on both the source and the mirror.
- Standalone instance resources (modified or cancelled overrides) are independent events that persist through a parent rule change. Source-side they continue to live as overrides; mirror-side our existing overrides also continue to live. If a source override is at a recurrence time the new rule no longer generates, Google keeps it as an attached one-off; the mirror's override behaves the same. This is consistent with how Google itself behaves.
- Future changes to source overrides arrive via the regular incremental stream and are reconciled by the modified-instance path in Phase 2. No special pass needed at recurrence-change time.

The `events.instances` endpoint returns all materialized instances (override and synthetic) within an optional time window; it's used in this spec only for the bounded targeted lookup in modified-instance reconciliation, not for unbounded scans during recurrence-change handling.

### Parent timezone changes

A source parent whose `start.timeZone` (or whose recurrence implicit timezone via `DTSTART;TZID=`) changes is reconciled by the same parent-patch path. The mirror parent's `start.timeZone` is updated alongside `recurrence` whenever Phase 2 detects a source-updated patch.

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
| `state_corrupt`          | 5    | State            | `state.json` exists but won't parse.                                                                 |
| `state_write_failed`     | 1    | State            | Filesystem error writing `state.json`.                                                               |
| `partial_failure`        | 1    | `run`            | At least one pdir failed but others succeeded. Final exit; details in `_meta.failures`.              |
| `not_macos`              | 1    | install/uninstall| Running on Linux/Windows.                                                                            |
| `plist_exists`           | 1    | install          | Plist exists and `--force` not set.                                                                  |
| `plist_not_found`        | 1    | uninstall        | Plist not present.                                                                                   |
| `launchctl_failed`       | 1    | install/uninstall| `launchctl load` or `unload` failed.                                                                 |
| `binary_not_resolvable`  | 1    | install          | calendar-sync's own binary path can't be determined.                                                 |
| `write_failed`           | 1    | init             | Filesystem error writing the starter config.                                                         |
| `timeout`                | 1    | run              | Exceeded `--timeout`.                                                                                |
| `already_running`        | 0    | run              | Lock held by another `run` invocation. Logged at info, exit 0.                                       |

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
acquire <state_file>.lock or exit(0, already_running)
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

Each pdir's state is saved independently on its own success.

## State Management

### `state.json`

Path: `settings.state_file` (default `~/.config/calendar-sync/state.json`).

Shape:

```json
{
  "version": 1,
  "pdirs": {
    "work-personal:a_to_b": {
      "source_calendar": "alice@example.com",
      "target_calendar": "alice.personal@example.org",
      "checkpoint": {
        "sync_token": "CPDC...",
        "full_sync_at": "2026-04-29T03:00:00Z",
        "query_fingerprint": "sha256:abc123..."
      },
      "last_attempt_at": "2026-04-29T23:00:00Z",
      "last_synced_at": "2026-04-29T23:00:00Z",
      "last_status": "ok",
      "last_error": null
    },
    "work-personal:b_to_a": {
      "source_calendar": "alice.personal@example.org",
      "target_calendar": "alice@example.com",
      "checkpoint": {
        "sync_token": "CMD3...",
        "full_sync_at": "2026-04-29T03:00:00Z",
        "query_fingerprint": "sha256:abc123..."
      },
      "last_attempt_at": "2026-04-29T23:00:00Z",
      "last_synced_at": "2026-04-29T23:00:00Z",
      "last_status": "ok",
      "last_error": null
    },
    "work-family:a_to_b": {
      "source_calendar": "alice@example.com",
      "target_calendar": "family@group.calendar.google.com",
      "checkpoint": {
        "sync_token": "",
        "full_sync_at": "2026-04-28T03:00:00Z",
        "query_fingerprint": "sha256:abc123..."
      },
      "last_attempt_at": "2026-04-29T23:00:00Z",
      "last_synced_at": "2026-04-28T03:00:00Z",
      "last_status": "rate_limited",
      "last_error": {"error": "rate_limited", "detail": "Rate limited after 5 retries", "endpoint": "events.list"}
    }
  }
}
```

The state for each pdir is split into two parts that are written under different rules:

- **`checkpoint`** (sync_token, full_sync_at, query_fingerprint). Updated only when Phase 1, 2, and 3 all succeeded for that pdir. This is the durable "where to resume" state.
- **`last_attempt_at`, `last_synced_at`, `last_status`, `last_error`**. The attempt log. Always written at the end of every `run` for every pdir we tried, success or failure. `last_synced_at` only advances on success; the attempt log advances regardless. This is what `state show` and the future Phase 2 `status` command read to answer "did the last sync work?".

Notes:
- Keys are `<pair>:<direction>`.
- `last_status` is `"ok"` or an error code from the table.
- `last_error` is null on success, otherwise a JSON error object identical to what would have gone to stderr.
- Written atomically: tempfile + fsync + rename.
- The advisory `<state_file>.lock` prevents concurrent writers on the same config (see "Lock file" below).

### Lock file

Path: `<settings.state_file>.lock` (default `~/.config/calendar-sync/state.json.lock`).

A zero-byte file used purely as a `flock(2)` target. Acquired non-blocking at the start of `run`. Released on process exit (even on crash, by the kernel).

Scoping the lock to the state file rather than to a fixed path means a user can run two calendar-sync configs against disjoint calendars in parallel by passing `--config` and a different `state_file` to each.

### Mirror identification (recap)

Find a specific mirror:
```
events.list?calendarId=<T>&privateExtendedProperty=calendar-sync:source=<S>:<E.id>
```

Find every mirror created by a pdir:
```
events.list?calendarId=<T>&privateExtendedProperty=calendar-sync:scope=<P>:<D>
```

Both are single-property queries.

## Phasing

### Phase 1 (initial release)

- TOML config with single account, multiple pairs, three directions.
- Calendar ID canonicalization at config load.
- `calendar-sync run` with full and incremental sync, recurring events, modified and cancelled instances, recurrence-rule changes.
- Sliding-horizon protection via `full_sync_interval`.
- Overlap protection via `flock` on `<state_file>.lock`.
- Per-pdir state with `query_fingerprint` invalidation.
- `init`, `config show`, `config validate`.
- `pair list`, `pair test`.
- `mirror list`, `mirror prune`.
- `state show`, `state reset`.
- `install`, `uninstall`.
- `skill`, `version`.
- JSONL stdout, JSONL or text stderr.
- launchd plist generation.
- Homebrew tap distribution via release-please + GoReleaser.

### Phase 2 (multi-account, drift, ergonomics)

- Multiple `[accounts.<name>]` entries; per-account `gws_config_dir`.
- `calendar-sync auth login --account <name>`, `auth status [--account <name>]`.
- Long-running `calendar-sync watch` (alternative to launchd).
- Parallel pdir execution.
- Mirror drift detection: compute desired payload for every existing mirror and patch on diff, not just on `source.updated` change. New action reason: `mirror_drift`.
- Per-pair redaction modes (`title_template`, `redact_description`).
- `calendar-sync status` summary command.
- Per-source-calendar list deduplication (one `events.list` per source calendar shared across pdirs).

### Phase 3 (push)

- `events.watch` integration for push notifications.
- Hosted webhook receiver (cloud deployment).
- Dynamic interval (poll fallback when watch channel is down).

## Dependencies

- **CLI framework**: [kong](https://github.com/alecthomas/kong).
- **Config**: [BurntSushi/toml](https://github.com/BurntSushi/toml).
- **Subprocess**: stdlib `os/exec`. `gws` is invoked with `--format json` and parsed.
- **Locking**: stdlib `golang.org/x/sys/unix` for `flock`.
- **Logging**: stdlib `log/slog` with custom JSON and text handlers.
- **Tests**: stdlib `testing` with table-driven tests. `gws` is faked via a stub binary built at `go test` time that matches the expected `gws calendar events ...` argument shapes and emits canned JSON. The test boundary is the gws subprocess, not HTTP.

No direct Google SDK. No SQLite. No third-party logging library.
