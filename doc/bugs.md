# bugs.md

Running list of bugs surfaced during the v1.0.0 install + test session. Add a new section as each new bug is identified. Move a section to "Fixed" when the fix lands and is verified. Don't delete entries - the trail is part of the artifact.

## Open

### B1 - `--help` triggers a live run on the `run` subcommand (CRITICAL)

`./calendar-sync run --help` printed kong's usage text AND then executed a real, non-dry-run sync. 37 mirror events landed on `me@tammersaleh.com` before the process was killed; they have full `calendar-sync:source/source_updated/checksum/version` properties and `status: confirmed`.

The same may be true of other subcommands. Audit before any further binary invocation.

Suspected mechanism: kong's `--help` handler is ordered after the kong dispatch picks the subcommand, OR the `RunCmd.Run` method does work before kong's help-exit fires, OR something else. Root cause not yet pinned.

Reproduction: `./calendar-sync run --help` against a configured but disabled-or-empty target. Don't reproduce on a live calendar without expecting writes.

### B2 - Bogus `migration_source_won` outcomes in dry-run

15 patches reported with `conflict: migration_source_won` despite zero v1/v2 mirrors on the target. Two cooperating causes per `doc/dry-run-anomaly-analysis.md`:

- Cause A: `dryRunAPI.EventsPatch` echoes only the request body, dropping any extended properties the prior insert wrote.
- Cause B: source-list duplication - `_R<timestamp>` recurring parents appear both as a top-level event and as a `recurring_event_id` on their instances, so `runClassifyLoop` processes the same source tuple twice.

Dry-run cosmetic only - production semantics are unaffected (real Calendar API merges patches correctly).

Fix sketch: dedupe `runClassifyLoop` by source-tuple and tighten `dryRunAPI.EventsPatch` to merge into a cached resource.

### B3 - gws subprocess timeouts cascade to `partial_failure`

A 365-day-horizon dry-run produced ~200 `gws subprocess: context deadline exceeded` errors. The outer `--timeout` default is 5m; per-event `events.instances` lookups serialize and queue past it.

Possible mitigations:
- Increase the per-event gws subprocess timeout independently of the outer wall-clock cap.
- Reduce per-event API calls (e.g. cache the target instances list rather than one call per instance).
- Parallelize per-event processing.

### B4 - No `--horizon` CLI flag

`config.toml` has `horizon`, but there's no `--horizon` flag override. User wants day-by-day rollout (`--horizon=1d`, `2d`, ..., `365d`) without editing config each time.

### B5 - gws stderr bleeds into the error message

A captured failure cause read:

```
horizon check for recurring parent 2on8pejh4aunu1nqco5hi7ut88: api_invalid_request during events.instances: Using keyring backend: keyring
error: HTTP request failed
```

`Using keyring backend: keyring` is gws's own informational stderr leaking into the formatted error string. Not safety-critical but obscures the real error.

## Fixed

### F1 - `partial_failure` envelope dropped underlying error

Initially fixed in `5a412e6 fix: surface underlying error in partial_failure stderr envelope` but that pass missed the joinError case. Followup `803317c fix: surface joinError cause in partial_failure stderr envelope` reads `cmdError.cause` directly via type assertion. Verified empirically against the live calendar - the envelope's `cause` field now carries the joined classify errors (~32KB).
