# progress.md

Session-handoff state. Read this on session start (after `SPEC.md` and `CLAUDE.md`) to know where the autonomous build was last left off. Update after each layer's clean review pass; delete when the implementation reaches the point where this file is no longer useful.

## Session 1 (2026-04-30)

### Layers complete

Per the bottom-up build order documented in the deleted `next.md`:

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

Plus: `2ce11b6` (gitignore tweak), `523fa76` (docs: filter tentative responseStatus alongside declined - SPEC.md change only; implementation lands in layer 6).

### Layers remaining

| Layer | Package | Notes |
|---|---|---|
| 5 | `internal/recurring` | Recurring-instance handler. SPEC §"Recurring Events" → "The recurring-instance handler" (steps 1-3). Tightly coupled with layer 6: recurring calls back into classification for parent reconciliation. May make sense to do 5+6 in close succession. |
| 6 | `internal/sync` | Classification logic. The 8-step switch (now including step 5 Tentative per `523fa76`), drift handling actions, schema migration path for v1 mirrors via `DriftSignal.NeedsMigration`, orphan walk. |
| 7 | `internal/daemon` | Lifecycle, scheduler (per-tick + periodic full re-sync), IPC socket, signal handling. |
| 8 | `internal/launchd` | Plist generation, launchctl wrappers. |
| 9 | `cmd/` | kong CLI struct + each subcommand. Lift `slack-cli`'s `internal/output` for the JSONL printer (see `next.md`'s now-deleted "Reuse" section). |

### Push status at session end

The user's SSH agent disconnected during the session, so 9 commits are stacked locally but not pushed (the layer-1 commit `92bc349` plus all subsequent ones except the originally-pushed docs ones). The user pushes manually when they return - per the "Push policy" added to CLAUDE.md, future sessions don't block on push.

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
