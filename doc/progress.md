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
| 7 | `internal/daemon` (wall-clock scheduler + IPC socket + signal handling + auth probe + outcome JSONL printer) | `b86d888` |

Plus: `2ce11b6` (gitignore tweak), `523fa76` (docs: filter tentative responseStatus alongside declined), `8e5013d` (docs: subagent-per-layer + push policies).

Layer 6.A also extracted three helpers from recurring → mirror (since both layers 5 and 6 needed them): `mirror.DriftedFieldNames`, `mirror.BuildPropagatePatchBody`, `mirror.SourceOwnerResponseStatus`. And added `gws.EventsListParams.PrivateExtendedProperty []string` for the inventory rebuild's filter queries.

## Layers remaining

| Layer | Package | Notes |
|---|---|---|
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
- daemon scheduler is wall-clock-driven (now.Truncate(p).Add(p)) so a sleep crossing tick boundaries fires a single catch-up on wake; SPEC §"Sleep and wake".
- daemon's IPC status response uses a compact duration form (60s, 24h - not Go's 1m0s, 24h0m0s) per SPEC line 725. compactDuration helper in internal/daemon/socket.go.
- daemon owns the auth probe (configurable AuthChecker callback) which short-circuits BEFORE socket bind, so a bad-auth daemon never holds the socket.
- daemon's fast-track FullSync flag handles Tick's NeedsFullResync signal in the next loop iteration (immediate, not on the timer).

## Push status at session end

The user's SSH agent disconnected during session 1, so 19 commits are now stacked locally but not pushed. The user pushes manually when they return - per CLAUDE.md "Push policy", future sessions don't block on push.

## Pointers for the next session

When picking up layer 8 (`internal/launchd`):

1. Read `SPEC.md` end-to-end (still mandatory).
2. Read this file + the relevant `Architecture decisions and gotchas` entries in `CLAUDE.md`.
3. Per `CLAUDE.md` "Implementation strategy", spawn a `general-purpose` subagent.
4. 8's scope: SPEC §"calendar-sync install" (lines 746-799) and §"calendar-sync uninstall" (lines 801-822). Generate the launchd plist (XML) per SPEC's exact template, write it to `~/Library/LaunchAgents/<label>.plist`, run `launchctl load -w` (or `unload`). Resolve calendar-sync's own binary path via `os.Executable`. Detect non-Darwin and surface `not_macos`.
5. The plist template is fixed in SPEC lines 766-787; copy it verbatim and parameterize Label / ProgramArguments / log paths. Don't add a `StartInterval` - the daemon's internal scheduler handles polling.
6. Per `CLAUDE.md` "Workflow" step 6, spawn `feature-dev:code-reviewer` after the layer; address findings; re-review for the clean second pass.
7. Commit per-layer; don't worry about push.
8. Update this file after the layer ships.

When picking up layer 9 (`cmd/`):

1. Same flow.
2. 9's scope: kong CLI struct + each subcommand (`watch`, `run`, `init`, `config show/validate`, `pair list/test`, `mirror list/prune`, `status`, `install`, `uninstall`, `skill`, `version`). Wires gws.Client + config loader + sync.Reconciler + daemon.Daemon. Lift slack-cli's internal/output for the JSONL printer.
3. Layer 7's daemon.Daemon takes an AuthChecker callback - layer 9 wires this to invoke `gws auth status` (either by adding a method to gws.Client or via os/exec).
4. The `daemon.ErrDaemonAlreadyRunning` sentinel (raised when watch detects another daemon on the socket) and `daemon.ErrAuthFailed` (raised on bad auth) need mapping to SPEC's exit codes (1 / 2 respectively, plus the "auth-failed under run/watch routes to exit 2" rule).

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
