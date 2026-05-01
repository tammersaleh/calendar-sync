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

Plus: `2ce11b6` (gitignore tweak), `523fa76` (docs: filter tentative responseStatus alongside declined), `8e5013d` (docs: subagent-per-layer + push policies).

Layer 6.A also extracted three helpers from recurring → mirror (since both layers 5 and 6 needed them): `mirror.DriftedFieldNames`, `mirror.BuildPropagatePatchBody`, `mirror.SourceOwnerResponseStatus`. And added `gws.EventsListParams.PrivateExtendedProperty []string` for the inventory rebuild's filter queries.

## Layers remaining

| Layer | Package | Notes |
|---|---|---|
| 6.C | `internal/sync` (Reconciler entrypoint) | FullSync + Tick methods: orchestrate the source-list / inventory-rebuild / classify-all / orphan-walk / token-management dance. Per-source events.list calls, conditional syncToken advancement, 410 GONE recovery signaling to the daemon. |
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

## Push status at session end

The user's SSH agent disconnected during session 1, so 15 commits are now stacked locally but not pushed. The user pushes manually when they return - per CLAUDE.md "Push policy", future sessions don't block on push.

## Pointers for the next session

When picking up layer 6.C (Reconciler entrypoint):

1. Read `SPEC.md` end-to-end (still mandatory).
2. Read this file + the relevant `Architecture decisions and gotchas` entries in `CLAUDE.md`.
3. Per `CLAUDE.md` "Implementation strategy", spawn a `general-purpose` subagent.
4. 6.C's scope: SPEC §"Daemon lifecycle: startup" + §"per-tick reconciliation" + §"periodic full re-sync". Provides `Reconciler` struct holding per-source syncTokens + per-target inventories, with `FullSync(ctx)` and `Tick(ctx)` methods. FullSync rebuilds inventories, full source-list per source, classify all, orphan walk, conditional token advancement. Tick does incremental events.list with syncToken, classify the delta, conditional advancement. Both produce per-pdir results so layer 7 can decide which tokens to commit.
5. 410 GONE on Tick → set a flag in the result so the daemon schedules an immediate full re-sync for the affected source. Don't try to recover inside Tick (the daemon owns the timer).
6. The Classifier (6.A) mutates Inventory in place during classify; the orphan walk (6.B) needs a `visited map[mirror.SourceTuple]bool` populated during classify. The simplest hookup: FullSync iterates the source list, tracks visited tuples as it calls Classify, then runs OrphanWalker.Walk with the populated set.
7. Per `CLAUDE.md` "Workflow" step 6, spawn `feature-dev:code-reviewer` after the layer; address findings; re-review for the clean second pass.
8. Commit per-layer; don't worry about push.
9. Update this file after the layer ships.

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
