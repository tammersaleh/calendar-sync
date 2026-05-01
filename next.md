# next.md

Session-handoff for the install + test pass. Read this on session start (after `SPEC.md` and `CLAUDE.md`). The implementation is shipped (v1.0.0); this session installs it on Tammer's laptop, verifies basic functionality in one-way mode, then optionally enables two-way sync.

## State at handoff

- v1.0.0 shipped on 2026-05-01 via the homebrew tap. Release page: https://github.com/tammersaleh/calendar-sync/releases/tag/v1.0.0
- The `propagate_target_edits` safety gate is in v1.0.0 (commit `828fe72`). Default false: edits on a mirror revert on the next tick rather than flowing back to the source. That's the deliberate posture for this test session.
- All nine implementation layers are committed and reviewed. SPEC.md and CLAUDE.md are current.
- `gws` CLI is the only system dependency. `calendar-sync` shells out to it for every Calendar API call.

## What success looks like

By the end of this session:

1. `calendar-sync` is installed via brew and running under launchd.
2. At least one pair is mirroring source events to target events.
3. Mirror events land with the right private extended properties (`calendar-sync:source`, `:source_updated`, `:checksum`, `:version`).
4. A test edit on the source flows to the mirror within a tick.
5. A test edit on the mirror reverts on the next tick (proves the one-way gate works).
6. The daemon survives a sleep/wake cycle.

Optional stretch: once 1-6 pass, flip `propagate_target_edits = true`, restart the daemon, and verify mirror edits propagate back.

## Install

```sh
brew install tammersaleh/tap/calendar-sync
calendar-sync version
```

Expect `{"version":"1.0.0",...}` not `"dev"`. If it says `dev`, the user is running `go run ./cmd/calendar-sync` from the repo, not the released binary.

## Authenticate gws

```sh
gws auth status
```

Non-zero exit → `gws auth login`. Tammer typically already has gws authenticated for personal use; the same auth covers calendar-sync.

The user is responsible for sharing source/target calendars with the gws-authenticated account in Google Calendar's UI before configuring pairs. SPEC §"Authentication" explains; the practical version is "share both calendars with one account, point gws at that account."

## Configure

```sh
calendar-sync init
```

Writes `~/.config/calendar-sync/config.toml` with one disabled placeholder pair. Edit it:

- Set the actual source / target calendar IDs (email addresses or `primary`).
- Pick a direction: `source_to_target`, `target_to_source`, or `bidirectional`.
- Flip `enabled = true`.
- Leave `propagate_target_edits` commented out (defaults false). DO NOT enable it for this session.

Then validate:

```sh
calendar-sync config validate
calendar-sync config show --canonicalize
```

`config show --canonicalize` resolves `primary` to the actual email and surfaces each pair's accessRole. Confirm the source/target accessRoles before going further. Reader+writer is the typical mix.

## Dry-run reconcile

```sh
calendar-sync run --dry-run
```

Reads source events, builds the inventory, prints what it would do without making API writes. Inspect the JSONL output:

- Most lines should be `action:"insert"` with `reason:"source_updated"` (first-run, every eligible source event has no mirror yet).
- `action:"skip"` with `reason:"is_mirror"` is common if the user has bidirectional pairs - it's the loop guard.
- Any `error` lines on stderr are blockers.

If the dry-run looks sane, do the real thing:

```sh
calendar-sync run
```

Same output but writes happen. Expect this to take 10-20s on a normal calendar.

## Verify mirrors landed correctly

```sh
calendar-sync mirror list <target-email>
```

One JSON line per mirror. Spot-check a couple:

1. Open Google Calendar in a browser.
2. Find one of the listed events on the target calendar.
3. Verify it shows as Busy (opaque), Private visibility, has a description ending with `\n\n---\nSource: <htmlLink>`.
4. The reminders should NOT fire (Default reminders are off on mirrors per SPEC).

## Install the daemon

```sh
calendar-sync install
calendar-sync status
```

`status` should report `reachable:true` with a recent `started_at`. If it reports `reachable:false`, the daemon failed to start - check `~/Library/Logs/calendar-sync/calendar-sync.err.log`.

The daemon ticks every `poll_interval` (60s default) and does a periodic full re-sync every `full_sync_interval` (24h default).

## Verify one-way safety gate

This is the load-bearing test for the user's stated concern.

1. Pick a mirror event on the target calendar.
2. Edit its title in the Google Calendar UI ("test edit - should revert").
3. Wait ~90s (one tick + buffer).
4. Refresh. The title should be back to the source's value.
5. Tail the daemon log: there should be one `action:"revert"` line with the test event's mirror ID.

If the edit persists, something's wrong with the gate. Check `calendar-sync config show` - `propagate_target_edits` MUST be false.

Then verify the source-to-mirror direction works:

1. Edit the title of an event on the SOURCE calendar.
2. Wait ~90s.
3. The mirror's title should match the new source title. Action in the log: `patch` with reason `source_updated`.

## Sleep/wake test

1. With the daemon running, close the laptop.
2. Wait at least `poll_interval` (60s+).
3. Open the laptop. Wait a few seconds.
4. `calendar-sync status` - the daemon should still be reachable.
5. Make an edit on the source. The mirror should update within a tick.

SPEC §"Sleep and wake" guarantees a single catch-up tick fires immediately on wake.

## Enable two-way sync (only after the above passes)

Edit `~/.config/calendar-sync/config.toml`:

```toml
[settings]
propagate_target_edits = true
```

Restart the daemon (config is read at daemon startup, not per-tick):

```sh
calendar-sync uninstall
calendar-sync install
```

Re-run the mirror-edit test. Now the edit should flow BACK to source instead of reverting.

## If things go wrong

- Daemon won't start: `tail -100 ~/Library/Logs/calendar-sync/calendar-sync.err.log`.
- Mirrors look weird: `calendar-sync mirror list <target> --orphaned` flags ones whose source no longer exists.
- Wholesale reset of mirrors for a pair: `calendar-sync mirror prune <target> --pair <name> --yes`.
- Stuck daemon: `calendar-sync uninstall`, kill any leftover process (`launchctl list | grep calendar-sync`), `calendar-sync install`.
- Sync token expiry on long laptop sleeps (>1 week): the daemon detects 410 GONE and runs an immediate FullSync to mint a new token. Visible in the err log as a `warn` line.

## Cleanup if abandoning the test

```sh
calendar-sync uninstall
calendar-sync mirror prune <target> --all --yes  # nukes every mirror calendar-sync created
```

## Open question to address mid-session

The user mentioned wanting to "test out the basic functionality first and then later enable the two-way sync." Once the one-way verification passes, ask whether they want to enable two-way in the same session or defer it. Defer is fine; the gate is durable, no rush.

## When this file becomes useless

After the install + verification is done and the daemon has been running for a few days successfully, delete this file. The work is one-shot, not durable handoff. (Unlike `progress.md`, which was about iterative build state.)
