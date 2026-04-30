# calendar-sync

Mirrors busy events between Google Calendars. Replaces the calendar-syncing piece of Reclaim.ai.

For each user-declared pair of calendars, calendar-sync writes a private "Busy" mirror of every busy source event to the destination. Updates and deletions propagate. Recurring events mirror as recurring events.

## Install

macOS (Homebrew tap):

```bash
brew install tammersaleh/tap/calendar-sync
```

Or from source (any platform):

```bash
go install github.com/tammersaleh/calendar-sync/cmd/calendar-sync@latest
```

## Setup

### 1. Authenticate `gws`

calendar-sync delegates all Google Calendar access to the [`gws`](https://github.com/googleworkspace/cli) CLI. Install `gws` and run `gws auth login` once. calendar-sync uses whatever account `gws auth status` reports.

```bash
gws auth login
gws auth status   # confirm success
```

Phase 1 supports a single account. The account must have read+write access to every calendar referenced in your config. If you want to mirror between two Google accounts (e.g. work and personal), share the relevant calendars between them via Google Calendar's calendar-sharing UI before continuing.

### 2. Generate a starter config

```bash
calendar-sync init
```

Writes `~/.config/calendar-sync/config.toml`. Open it and declare your sync pairs.

### 3. Validate

```bash
calendar-sync config validate
```

### 4. Test without writing

```bash
calendar-sync run --dry-run
```

Lists every action calendar-sync would take. No API writes.

### 5. Install the launchd agent

```bash
calendar-sync install
```

Writes `~/Library/LaunchAgents/org.calendar-sync.agent.plist` with `StartInterval=60`. From now on the sync runs every 60 seconds.

To remove:

```bash
calendar-sync uninstall
```

## Configuration

`~/.config/calendar-sync/config.toml`:

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
```

Direction values: `source_to_target`, `target_to_source`, `bidirectional`. Calendar IDs accept `primary`, an email, or a group calendar ID. See `SPEC.md` for the full schema.

## What gets mirrored

A source event is mirrored only if all of these are true:

- `eventType` is `default`, `outOfOffice`, or `focusTime`.
- `transparency` is `opaque` (Busy). Free events are skipped.
- The source calendar owner has not declined the event.
- The event isn't already a calendar-sync mirror (loop guard).
- The event starts within `[now, now + horizon]`.

Mirror events are private, marked busy, and copy the source event's title and description (with a trailing link back to the source). Reminders are off. See `SPEC.md` for the full payload.

## Common commands

```bash
# One-shot sync (this is what launchd runs)
calendar-sync run

# Sync only one pair
calendar-sync run --pair work-personal

# See what's currently mirrored
calendar-sync mirror list primary --pair work-personal

# Wipe every mirror this tool created from a calendar
calendar-sync mirror prune primary --all

# Force a full re-list (after manual cleanup)
calendar-sync run --reset-state

# Show last-sync state for every pair-direction
calendar-sync state show
```

## Privacy

Mirror events copy the source event's title and description verbatim. `visibility=private` only hides those details from *readers* of the destination calendar. Anyone with writer or owner access still sees them. Make sure the destination calendar's writer access is restricted to people you're comfortable sharing the source calendar's contents with.

A future release may add redaction modes (template-based titles, optional description suppression).

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| `gws_auth_failed` (exit 2) | `gws auth status` is unauthenticated. Run `gws auth login`. |
| `calendar_not_found` | Account doesn't have access to the calendar. Check Google Calendar sharing. |
| `rate_limited` (exit 3) | Hit Google's per-user quota. Wait, then re-run. Reduce `poll_interval` if persistent. |
| Mirrors not updating | Check `calendar-sync state show` for the pdir's `last_status`. Run `calendar-sync run --reset-state` to force a full re-list. |
| Duplicate mirrors | Shouldn't happen via the tool. If it does, run `calendar-sync mirror prune <calendar> --orphaned` and report the bug. |

## Development

See `CLAUDE.md` for workflow and project structure. `SPEC.md` is the source of truth for the entire CLI surface, sync algorithm, state model, and error conditions.
