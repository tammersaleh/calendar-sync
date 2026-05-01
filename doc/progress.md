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

Plus: `2ce11b6` (gitignore tweak), `523fa76` (docs: filter tentative responseStatus alongside declined), `8e5013d` (docs: subagent-per-layer + push policies).

## Layers remaining

| Layer | Package | Notes |
|---|---|---|
| 6 | `internal/sync` | Classification logic. The 8-step switch (now including step 5 Tentative per `523fa76`), drift handling actions, schema migration path for v1 parent mirrors (recurring already covers v1 instance migration), orphan walk. Will inject `MirrorParentLookup` and `ParentReconciler` callbacks into `recurring.Handler` per the inversion in CLAUDE.md "recurring.Handler accepts callbacks". |
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

## Push status at session end

The user's SSH agent disconnected during session 1, so 11 commits are now stacked locally but not pushed. The user pushes manually when they return - per CLAUDE.md "Push policy", future sessions don't block on push.

## Pointers for the next session

When picking up layer 6, the session should:

1. Read `SPEC.md` end-to-end (still mandatory).
2. Read this file + the relevant `Architecture decisions and gotchas` entries in `CLAUDE.md`.
3. Per `CLAUDE.md` "Implementation strategy", spawn a `general-purpose` subagent.
4. Layer 6 owns the classification path (steps 1-8 from SPEC.md "Classification logic"), the orphan walk, and the parent-side v1 migration cells. It also implements the `MirrorParentLookup` / `ParentReconciler` closures that get injected into `recurring.Handler` from layer 5.
5. The `patchMirrorWithChecksum` helper is currently inlined in `internal/recurring/handler.go`. Layer 6 will need the same primitive for parent reconciliation; consider extracting to a shared helper if the duplication becomes annoying.
6. Per `CLAUDE.md` "Workflow" step 6, spawn `feature-dev:code-reviewer` after each layer; address findings; re-review for the clean second pass.
7. Commit per-layer; don't worry about push.
8. Update this file after each layer ships.

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
