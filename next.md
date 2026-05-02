# next.md

Handoff for the next session. Read this first, then `SPEC.md`, then `CLAUDE.md`. The previous session got us through the v1.0.0 install + test, fixed a bunch of bugs surfaced during use, and rolled out one-way then two-way sync to a 365-day horizon. We're now ready to roll out the OTHER direction (personal → coreweave) gradually, which requires a config schema change.

## Where we are

- **Repo on main**: `fa52923 fix: orphan walker swallows ErrAPIGone like ErrAPINotFound (B14)`. release-please cuts patch releases per merged fix; current Homebrew version is `v1.1.2` (B14 hasn't shipped yet — release-please will pick it up next).
- **Local binary**: `/Users/tammersaleh/src/github.com/tammersaleh/calendar-sync/.claude/worktrees/agent-acddc4e012316a613/calendar-sync` is built from a worktree synced to main. The Homebrew binary at `/opt/homebrew/bin/calendar-sync` lags by however many fixes haven't shipped to the tap.
- **Daemon NOT installed**. The whole rollout was run via one-shot `calendar-sync run` invocations against `~/.config/calendar-sync/config.toml`. `calendar-sync install` would wire up launchd; deferred until after the bidirectional rollout decision.
- **Config state** (`~/.config/calendar-sync/config.toml`):
  - `horizon = "365d"`
  - `propagate_target_edits = true`
  - One pair `work-personal`: source=`tsaleh@coreweave.com`, target=`me@tammersaleh.com`, direction=`source_to_target`, enabled=true.
- **Mirrors on personal calendar**: 146 confirmed `calendar-sync:version=2` events, plus a long tail of `cancelled` tombstones from the rollout's earlier prune-and-revive cycles. Tombstones don't render visibly to Tammer.
- **Source calendar safety**: across every phase of the two-way rollout (1d→365d), the safety check scanned 6447 historic source events by etag and observed exactly 1 etag change — confirmed external (a past, declined event with no calendar-sync mirror; organizer was someone else). **Zero two-way leaks attributable to calendar-sync.**

### Bug list

`doc/bugs.md` is the canonical record. Quick summary:

**Fixed during this session** (in chronological order — most are tied to specific commits on main):
- F1: partial_failure envelope dropped underlying error
- B1 (CRITICAL): `--help` triggered live run on subcommands
- B2: dryRunAPI + runClassifyLoop dedup; killed bogus migration_source_won
- B4: horizon config-file path verified
- B6 (CRITICAL): `[settings].dry_run = true` was unwired
- B7: launchd WatchPaths for config.toml auto-reload
- B10: `mirror prune` errors on already-cancelled events
- B11: orphan walk errors on already-cancelled mirrors (skip cancelled at BuildInventory)
- B13 (CRITICAL): gws error JSON parsed from wrong stream — masked 409/410/404 sentinels
- B14: orphan walker swallows ErrAPIGone alongside ErrAPINotFound

**Still open**:
- B3: gws subprocess timeouts cascade to partial_failure at large horizons. Each recurring event triggers an `events.instances` per instance for the locate-and-repair flow; with horizon=365d and many recurring events the per-event sequential calls pile up past the default 5-minute `--timeout`. **Mitigation**: use `--timeout=20m` (90d) or `--timeout=30m` (365d) on `run`. **Real fix** would be parallelizing the per-instance lookups or caching the target instances list.
- B5: gws stderr (`Using keyring backend: keyring`) bleeds into formatted error strings. Cosmetic.
- B8: `config.FindPath` can return relative paths; launchd `WatchPaths` is undefined for relative paths. Edge case.
- B9: plist generator uses `text/template`, not `html/template`; a config path with `&`/`<`/`>` would break the plist. Edge case.

### Test discipline

The user emphasized **strict red-green-refactor** for every fix:
1. Write the failing test first.
2. Run it locally and OBSERVE the red state (`go test -run <name>`). The pre-fix expected behavior must actually fail.
3. Implement the fix.
4. Confirm green.
5. Commit test + fix in a single commit.

Don't split test and fix into separate commits — `mise run check` must stay green on every commit (the pre-push hook runs check on HEAD; intermediate broken commits would also fail any future bisect or rebase).

### Code review discipline

Per project CLAUDE.md, every code-changing commit goes through a `feature-dev:code-reviewer` subagent before push. The reviewer's UUID is reusable across passes — continue conversations via `SendMessage` rather than spawning a fresh reviewer each time. The session has gone through 9 review passes; reviewer history lives in agent transcripts, not here.

## The planned change (next session's main work)

Two config-schema additions and one removal. Both per-pair scoping changes are agreed; `direction` removal is also agreed.

### 1. Add per-pair `horizon`

Optional field on `[[pairs]]`. Falls back to `[settings].horizon` when absent. Each pair's pdirs use the resolved value.

```toml
[settings]
horizon = "365d"   # default for any pair that doesn't override

[[pairs]]
name = "cw-to-personal"
source = "tsaleh@coreweave.com"
target = "me@tammersaleh.com"
enabled = true
# horizon falls back to "365d"

[[pairs]]
name = "personal-to-cw"
source = "me@tammersaleh.com"
target = "tsaleh@coreweave.com"
horizon = "1d"      # explicit override, gradual rollout
enabled = true
```

### 2. Add per-pair `propagate_target_edits`

Optional field on `[[pairs]]`. Falls back to `[settings].propagate_target_edits` when absent. The current global gate stays as a default; per-pair overrides allow ramping two-way one direction at a time.

### 3. Remove `direction` field entirely (BREAKING change to config schema)

Reasons:
- `bidirectional` is just shorthand for two pairs. With per-pair scoping for horizon and propagate, two pairs becomes strictly more flexible.
- `target_to_source` is just `source_to_target` with source/target swapped. Pure cognitive overhead.
- Eliminating `direction` removes a source of confusion (which way does data flow?) and forces explicit pair declarations.

Post-change, every pair is implicitly `source_to_target`. Bidirectional setups declare two pairs.

### Existing config migration

The current config has `direction = "source_to_target"` on the one pair. Removing the field makes that pair implicitly source_to_target — same semantics. **No on-disk migration required for this case.** Anyone with `direction = "bidirectional"` would need to split their pair into two; that's a documented breaking change.

### Implementation scope

Touches:
- `internal/config/types.go`: `Pair` struct gets `Horizon *Duration` and `PropagateTargetEdits *bool` (nil = use settings default). Drop `Direction` field.
- `internal/config/validate.go` / `canonicalize.go`: resolve effective per-pdir values; drop direction-based pdir expansion (now always 1 pdir per pair, a_to_b only).
- `internal/sync/reconciler.go` + `classifier`: thread per-pdir horizon and per-pdir propagate. Currently both are at Reconciler level (single value across all pdirs); they need to move to `PDir` struct or the Reconciler needs to look them up per pdir.
- All consumers of `pd.SourceWritable` (which is currently `propagate_target_edits AND accessRole>=writer`) need to read the per-pair propagate flag.
- Config validation: error if a TOML still has `direction` field (parse-time error with a one-line migration hint pointing at this doc).
- Tests at every layer.
- SPEC.md update.
- doc/bugs.md update if anything new surfaces.
- CLAUDE.md update (remove the `propagate_target_edits` global note; replace with per-pair note).

This is `feat!:` (major bump if we were >= 1.0; we're at 1.1.x — pre-1.0 it'd be minor, post-1.0 it's major). The current version is 1.1.x, so this would be 2.0.0. release-please handles the bump from the commit subject line.

### Acceptance test (manual, post-implementation)

After the schema change lands and the binary rebuilds, the user adds the second pair to config with `horizon = "1d"` and starts the gradual two-way rollout in the personal → coreweave direction:

1. Edit config: add the second pair at horizon=1d. Leave the first pair untouched at horizon=365d.
2. Run `calendar-sync run` against the new pair — predict + verify with the same harness used in this session (snapshot source events, run, check no foreign-side modifications).
3. Ramp 1d → 2d → 7d → 14d → 30d → 90d → 365d on the second pair, leaving the first pair at 365d.
4. After both pairs reach 365d and are validated, install the daemon (`calendar-sync install`).

## Things to know about this codebase (durable context)

The user's project CLAUDE.md is the source of truth for these conventions; this section is a TLDR.

- Releases are automated. Conventional-commit subject drives release-please. `feat:` minor-bumps, `fix:` patch-bumps, `feat!:` or `BREAKING CHANGE:` major-bumps. `chore:` / `docs:` / `test:` / `refactor:` ship nothing.
- Tests live next to code. Use the in-process stub pattern (`stubAPI`, `stubGws`, `inventoryGws`, etc.) for unit-level coverage. The fake-gws harness in `internal/testhelpers/` is reserved for end-to-end command tests where the gws argv shape itself is what's under test.
- gws CLI behavior: emits API error JSON envelope on **stdout**, not stderr (B13 lesson). Error mapping in `internal/gws/errors.go` parses stdout first, falls back to stderr.
- The dryRunAPI wrapper in `cmd/run.go` keeps a per-(calendarID, eventID) cache populated by `EventsInsert` and merged by `EventsPatch` (B2 lesson). Don't go back to body-echo only — it loses prior insert state.
- `BuildInventory` skips `status=cancelled` events (B11 lesson). The cancelled-and-revived flow doesn't need them in the inventory; revival is triggered by 409-on-insert and uses a per-event events.get.
- `mirror prune` skips `status=cancelled` candidates (B10 lesson) and the orphan walker swallows both ErrAPINotFound and ErrAPIGone (B14 lesson). Don't add new "delete-and-error" code paths without considering both cases.
- kong's `--help` flag short-circuits via the kong-Exit callback in `cmd/cli.go`. A `kongExitCode` sentinel prevents subcommand dispatch when `--help` (or any future kong-builtin terminator) fires (B1 lesson). Don't pass `kong.Exit(func(int){})` — that swallows the signal.
- Tests for new flags must verify `--help` on every subcommand does NOT dispatch, even with mixed flags (`TestRun_HelpFlagDoesNotDispatchSubcommand` is the matrix).
- For dry-run write-bypass tests, use the `panicWriteGws` stress stub. It panics on any unexpected write and surfaces the leaking method + calendar + event ID in the panic message.
- `[settings].dry_run` and `--dry-run` OR together (`c.DryRun || canonical.Settings.DryRun`). Config wins over CLI by design (a deliberate config flag shouldn't be overridden by a typo'd CLI invocation). `mirror prune` is intentionally NOT gated by `[settings].dry_run`; it has its own `--dry-run` flag (B6 lesson).
- The `--prune-horizon` flag on `mirror prune` is distinct from the sync `horizon`. Phased mirror cleanup uses it to scope deletes to mirrors with start in `[now, now+dur]`. Inclusive on both edges. Events without parseable start are excluded when the flag is set.
- launchd `WatchPaths` on the plist (B7) auto-reloads the daemon when config.toml changes. Resolved config path comes from `config.FindPath`. Caveats live in B8 and B9 (relative paths, XML escaping).

## Things NOT to touch in this session

- The 146 confirmed mirrors on personal calendar are correct, in horizon, validated. Don't prune them.
- The cancelled tombstones on personal calendar are harmless — Google GCs them eventually. No need to "clean up" further.
- B3 (gws timeout cascade) is real and worth fixing eventually, but the workaround (`--timeout=30m` for the `run` command) suffices for the planned bidirectional rollout. Don't spend a session on the underlying parallelization work without the user's explicit go-ahead.
- B5 / B8 / B9 are tracked but not blocking. Skip unless the user asks.
- The Reclaim mirrors on the work calendar — Tammer cleaned them up manually.

## Concrete next-action checklist

In order:

1. Read `SPEC.md`, `CLAUDE.md`, `doc/bugs.md`, this file.
2. Confirm with Tammer that he wants to drop `direction` (he's said yes, but worth re-checking on resume in case he changed his mind).
3. Create a feature branch: `feat/per-pair-config-scoping`.
4. Plan the schema change concretely (which structs, which call sites). Probably worth a code-explorer agent for the planning if the call graph isn't fresh.
5. Implement in subagent (red-green per fix; one commit per logical change). The change is big enough that splitting into commits like:
   - `feat!: drop direction field from pair config (BREAKING)`
   - `feat: add per-pair horizon override`
   - `feat: add per-pair propagate_target_edits override`
   - might be cleaner than a single mega-commit. Each ships independently.
6. After each substantive commit: `feature-dev:code-reviewer` subagent review (mandatory, see project CLAUDE.md).
7. Update `SPEC.md` with the new schema (remove direction, add per-pair fields).
8. Update `~/.config/calendar-sync/config.toml`: drop the `direction` field. Validate with `calendar-sync config validate`.
9. Add the second pair (`personal-to-cw` at horizon=1d) and start the ramp. Use the same predict/verify harness as the first rollout (`/tmp/historic_check.py` exists from this session and is reusable; baseline is `/tmp/historic-baseline.json` but rebuild it before each phase).
10. After full ramp, install the daemon.

## When this file becomes useless

When the bidirectional rollout completes and both directions are at horizon=365d running under the daemon, delete this file. Per-pair config will be the new normal in SPEC.md and CLAUDE.md by then.
