# progress.md

Session-handoff state. Read this on session start (after `SPEC.md` and `CLAUDE.md`) to know where the autonomous build was last left off. Update after each layer's clean review pass; delete when the implementation reaches the point where this file is no longer useful.

## Layers complete

| Layer | Package | Commits |
|---|---|---|
| 1 | `internal/mirror` (pure-logic primitives: DeterministicID, SourceTuple, StripTrailer, ManagedFields+Checksum) | `92bc349` |
| 2.A | `internal/gws` Client + CalendarListGet + fake-gws harness | `b406f3c` |
| 2.B | `internal/gws` read methods (events.list / get / instances) | `e78b740` |
| 2.C | `internal/gws` write methods (events.insert / patch / delete) | `a83731f` |
| 2.D | `internal/gws` typed error classification (sentinel errors, HTTP-status mapping, ErrAPIGone) | `b4694fe` |
| 3 | `internal/config` TOML loader + parse-time validation | `11b743e` |
| 3.B | `internal/config` canonicalization + pdir expansion | `c0773cc` |
| 4 | `internal/mirror` payload + drift signal + Classify four-way matrix | `214726b` |
| 5 | `internal/recurring` handler (steps 1-3) + v1 migration cells (migration_upgrade, migration_source_won) | `4185541` |
| 6.A | `internal/sync` inventory + classification (parent path with recurring delegation) + drift + v1 parent migration + 409 handling | `a2280e8` |
| 6.B | `internal/sync` orphan walk + concurrent events.get fan-out (semaphore of 5) + per-entry error accumulation | `d3e00bf` |
| 6.C | `internal/sync` Reconciler entrypoint (FullSync + Tick), conditional syncToken advancement, lastFullSync stamping, 410 GONE → NeedsFullResync signaling | `8a0e6ec` |

Plus: `2ce11b6` (gitignore tweak), `523fa76` (docs: filter tentative responseStatus alongside declined), `8e5013d` (docs: subagent-per-layer + push policies).

Layer 6.A also extracted three helpers from recurring → mirror (since both layers 5 and 6 needed them): `mirror.DriftedFieldNames`, `mirror.BuildPropagatePatchBody`, `mirror.SourceOwnerResponseStatus`. And added `gws.EventsListParams.PrivateExtendedProperty []string` for the inventory rebuild's filter queries.

## Layers remaining

| Layer | Package | Notes |
|---|---|---|
| 7 | `internal/daemon` | Lifecycle, scheduler (per-tick + periodic full re-sync), IPC socket, signal handling. |
| 8 | `internal/launchd` | Plist generation, launchctl wrappers. |
| 9 | `cmd/` | kong CLI struct + each subcommand. Lift `slack-cli`'s `internal/output` for the JSONL printer (see `next.md`'s now-deleted "Reuse" section). |

## Conventions established (running list)

All captured in `CLAUDE.md` "Architecture decisions and gotchas". Highlights:

- mirror.EventDateTime / gws.EventDateTime intentionally duplicated; convert.go bridges.
- DriftSignal.NeedsMigration drives v1 schema-migration routing.
- gws.Error sentinel pattern with errors.Is matching by Code.
- ErrAPIGone for 410 syncToken expiry.
- AccessRoleAtLeast fails closed on unknown roles.
- Disabled pairs skipped entirely (no resolution, no validation, no PDir).
- BuildPayload omits checksum; it's a follow-up patch.
- EventsListParams: SingleEvents has omitempty, ShowDeleted does not.
- Conditional syncToken advancement is the sync layer's invariant; wrapper just exposes the last page's token.
- v1 migration cells live in callers, not in mirror.Classify (caller branches on signal.NeedsMigration before Classify).
- recurring.Handler accepts callbacks (MirrorParentLookup, ParentReconciler) to avoid an import cycle with the sync layer.
- Cancellation patches skip the checksum follow-up; no managed field changed.
- sync.Outcome carries SourceUpdated / MirrorUpdated for the layer-7 conflict warn log (empty on migration_source_won per SPEC).
- sync.Classifier.Horizon=0 disables the horizon check entirely; layer 7 wiring MUST set a non-zero value.
- sync layer mutates the per-target Inventory in place after every write; the same map survives across pdirs that share a target.
- Orphan walk's eventType filter is an allowlist ({default, outOfOffice, focusTime}); future Google types prune rather than silently retain.
- Orphan walk fan-out is concurrent (semaphore of 5); inventory + Output mutations happen serially after the fan-out drains. stubAPI gained a sync.Mutex to support this.
- Reconciler.FullSync stamps lastFullSync per source on full-source-list success regardless of whether Google returned a syncToken; gating on token advancement would loop FullSync indefinitely on a no-token source.
- Reconciler.Tick passes nil to runClassifyLoop's visited param; FullSync passes a non-nil map. The orphan walk only runs in FullSync.
- Tick's first call (empty in-memory token) signals NeedsFullResync without marking pdirs as failed - "no work to do" isn't a failure.

## Push status at session end

The user's SSH agent disconnected during session 1, so 17 commits are now stacked locally but not pushed. The user pushes manually when they return - per CLAUDE.md "Push policy", future sessions don't block on push.

## Pointers for the next session

When picking up layer 7 (`internal/daemon`):

1. Read `SPEC.md` end-to-end (still mandatory).
2. Read this file + the relevant `Architecture decisions and gotchas` entries in `CLAUDE.md`.
3. Per `CLAUDE.md` "Implementation strategy", spawn a `general-purpose` subagent.
4. 7's scope: SPEC §"Daemon lifecycle" timer-driven loop, IPC socket (`$TMPDIR/calendar-sync.sock`) for `calendar-sync status`, SIGTERM/SIGINT signal handling, `gws auth status` startup probe.
5. The daemon constructs ONE `sync.Reconciler` at startup, calls `FullSync` once at launch, then schedules `Tick` every `poll_interval` and `FullSync` every `full_sync_interval`. On a `Tick` result with `NeedsFullResync=true` for any source, schedule an immediate FullSync (don't wait for the timer).
6. Per SPEC §"Sleep and wake" (lines 1090-1101), the wall-clock-driven scheduler computes next-tick as `now.Truncate(poll_interval).Add(poll_interval)`, so a sleep that crosses tick boundaries fires a single catch-up tick on wake.
7. The IPC socket emits a JSON snapshot of per-pdir state on connect (per SPEC §"calendar-sync status" stdout shape). The daemon owns the bind/cleanup lifecycle (SPEC §"IPC socket" - daemon-side lifecycle).
8. Output: the daemon wires `r.Output = func(o sync.Outcome) { jsonl.Emit(o) }` to a JSONL printer (probably the `internal/output` package per CLAUDE.md project structure).
9. Per `CLAUDE.md` "Workflow" step 6, spawn `feature-dev:code-reviewer` after the layer; address findings; re-review for the clean second pass.
10. Commit per-layer; don't worry about push.
11. Update this file after the layer ships.

### Conventions established this session

All captured in `CLAUDE.md` "Architecture decisions and gotchas". Highlights:

- mirror.EventDateTime / gws.EventDateTime intentionally duplicated; convert.go bridges.
- DriftSignal.NeedsMigration drives v1 schema-migration routing.
- gws.Error sentinel pattern with errors.Is matching by Code.
- ErrAPIGone for 410 syncToken expiry (the SPEC's full-resync recovery trigger).
- AccessRoleAtLeast fails closed on unknown roles.
- Disabled pairs skipped entirely (no resolution, no validation, no PDir).
- BuildPayload omits checksum; it's a follow-up patch.
- EventsListParams: SingleEvents has omitempty, ShowDeleted does not.
- Conditional syncToken advancement is the sync layer's invariant; wrapper just exposes the last page's token.

### Pointers for the next session

When picking up layer 5+, the session should:

1. Read `SPEC.md` end-to-end (still mandatory).
2. Read this file + the relevant `Architecture decisions and gotchas` entries in `CLAUDE.md`.
3. Per `CLAUDE.md` "Implementation strategy", spawn a `general-purpose` subagent with the SPEC excerpt + the prior-layer interfaces it'll consume.
4. Per `CLAUDE.md` "Workflow" step 6, spawn `feature-dev:code-reviewer` after each layer; address findings; re-review for the clean second pass.
5. Commit per-layer; don't worry about push.
6. Update this file after each layer ships.
