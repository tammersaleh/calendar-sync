# calendar-sync

Command-line tool that mirrors Google Calendar events between calendars.
Replaces the calendar-syncing portion of Reclaim.ai. Polls source calendars
on a schedule and writes shadow events to target calendars.

## Common operations

Generate a starter config:

```bash
calendar-sync init
```

Validate a config without running:

```bash
calendar-sync config validate
```

Show resolved config (useful for debugging "primary" → canonical resolution):

```bash
calendar-sync config show --canonicalize
```

Install the launchd agent (macOS):

```bash
calendar-sync install
```

Run a one-shot reconcile (with the daemon stopped):

```bash
calendar-sync uninstall
calendar-sync run --dry-run
calendar-sync install
```

Inspect the daemon's live state:

```bash
calendar-sync status
```

List mirrors on a target calendar:

```bash
calendar-sync mirror list primary --pair work-personal
```

## Configuration

Lives at `$XDG_CONFIG_HOME/calendar-sync/config.toml` (or
`~/.config/calendar-sync/config.toml`). Each `[[pairs]]` entry names two
calendars and a direction (`source_to_target`, `target_to_source`,
`bidirectional`).

## Output

JSONL on stdout, ending with a `_meta` trailer. Errors as a single JSON
object on stderr.

## Authentication

Delegated to `gws`. Run `gws auth login` once before configuring
calendar-sync; both `watch` and `run` call `gws auth status` at startup
and exit 2 if it fails.
