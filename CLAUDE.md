# calendar-sync

Google Calendar event mirroring tool. Replaces the calendar-syncing piece of Reclaim.ai. Go, kong, `gws` subprocess for Calendar API access. JSONL output, launchd-driven polling.

## Workflow

Work is driven by `SPEC.md`. Every change - feature, bug fix, perf fix, refactor - follows the same workflow. No shortcuts for "small" fixes:

1. Read `SPEC.md` for the relevant command/feature.
2. Create a feature branch off main (or work directly on main for hot fixes - still follow every other step).
3. Red-green-refactor: write failing tests first, then implement, then clean up.
4. Run both `mise run test` and `mise run lint` after every change. Both must pass before committing.
5. Keep commits small and conventional. Commit types drive releases - see "Release versioning" below.
6. **MANDATORY code review before push** (code changes only - skip for docs-only changes where every modified file is Markdown or plain-text documentation): spawn a `feature-dev:code-reviewer` sub-agent on the pending changes (`git diff main...HEAD` for a branch, or on the commits about to push for direct-to-main work). Tell the reviewer to scrutinize tests: look for tests that don't actually test what they claim, useless tests, and missing test coverage. Address every important or critical finding. **Re-run the reviewer after addressing feedback** to confirm the fixes are clean - "before and after" reviews are both required. Never push code without a clean review pass. Skipping is not acceptable regardless of how small a code change looks; the docs carve-out is the only exception.
7. Merge to main and push. The pre-push hook runs `mise run check` (test + lint); never bypass with `--no-verify`.
8. Retrospective: review your approach and these instructions. Update CLAUDE.md with anything you learned that would help future sessions.
9. Move on to the next feature.

## Release versioning

Releases are fully automated via release-please + GoReleaser. Release-please watches main; when a commit with a version-bumping type lands, it opens a release PR which auto-merges once CI passes. The merged PR cuts a tag + GitHub Release; GoReleaser builds binaries and pushes an updated Formula to `tammersaleh/homebrew-tap`. Nobody runs `git tag` by hand - and nobody clicks Merge on the release PR either.

This means commit type is not a style choice. It is the entire release trigger. A `feat:` or `fix:` commit pushed to main ships as a new Homebrew release within minutes. A `chore:` or `docs:` commit pushed to main ships nothing. Pick the type based on user-facing impact, not diff size:

- `feat:` - minor bump, listed under "Features". New commands, flags, outputs, or API surface.
- `fix:` - patch bump, listed under "Bug Fixes". Behavior that was already promised but broken.
- `feat!:` (or `BREAKING CHANGE:` footer) - minor bump pre-1.0, major post-1.0, listed under "BREAKING CHANGES". Anything that can break an existing caller: removed/renamed flags, changed output shape, changed exit codes, changed command behavior, changed config schema, changed extended-property layout, changed state.json shape.
- `chore:`, `docs:`, `test:`, `refactor:`, `perf:`, `style:` - no release, not in changelog. Internal only.

Rules:

- If a commit contains both a feat and a fix, split it into two commits.
- Dependency bumps: `fix:` if the bump reaches users, otherwise `chore:`.
- Don't downgrade a type to avoid a release. If it's user-facing, it's `feat:` or `fix:`.
- Don't upgrade a type to force a release. Internal refactors are `refactor:` even if the change is large.
- Write the subject in imperative mood and keep it under ~70 chars.

## Autonomy

Work through features independently. Never stop to ask "should I continue?" or "want me to keep going?" - the answer is always yes. After giving a status summary, keep working. Only escalate when:

- A design decision isn't covered by `SPEC.md`.
- Something feels wrong (scope creep, gws/Calendar API limitation, etc.).

## Project structure

The intended layout once code lands:

```
cmd/
  calendar-sync/
    main.go        # entrypoint; wires kong, calls into cmd
  root.go          # CLI struct, global flags, NewRunner
  run.go           # `run` subcommand
  init.go          # `init` subcommand
  config.go        # `config show`, `config validate`
  pair.go          # `pair list`, `pair test`
  mirror.go        # `mirror list`, `mirror prune`
  state.go         # `state show`, `state reset`
  install.go       # `install`, `uninstall`
  skill.go         # `skill`
  version.go       # `version`
internal/
  config/          # TOML loader, validation, canonicalization
  state/           # state.json read/write, lock acquisition
  gws/             # gws subprocess wrapper (events.list, events.insert, etc.)
  sync/            # core algorithm: list, classify, reconcile, prune
  mirror/          # mirror payload construction, extended-property layout
  recurring/       # recurring-instance handler, originalStart helpers
  output/          # JSONL printer, _meta trailer, error stderr writer
  errors/          # error codes, exit-code mapping
  launchd/         # plist generation, launchctl wrappers
```

This is the target. The autonomous implementation session is allowed to deviate where it makes sense, but the boundaries between `gws` (subprocess), `sync` (orchestration), and `mirror`/`recurring` (per-event reconciliation) are load-bearing for testability.

## Testing

Tests live next to the code they test (`foo_test.go`). Use table-driven tests.

The `gws` subprocess is the test boundary. Mocking happens by building a small fake `gws` Go program at `go test` time and pointing the production code at it via `PATH` manipulation. The fake reads its argv, matches against expected `gws calendar events ...` invocations, and emits canned JSON to stdout. No HTTP-level mocking. This mirrors the way `slack-cli` tests its API surface but at the subprocess boundary instead of `httptest.Server`.

End-to-end command tests use a `runWithFakeGWS(t, scenario, args...)` helper that:

1. Creates a temp dir for `XDG_CONFIG_HOME` (so config.toml, state.json, lock are isolated).
2. Builds the fake `gws` binary into that temp dir (cached across tests).
3. Prepends the temp dir to `PATH`.
4. Invokes the real CLI with the given args.
5. Returns stdout, stderr, exit code.

For functions only accessible within an internal package, put unit tests in a file that uses `package <pkg>` rather than `package <pkg>_test`.

## Git

This is a personal project. The workflow is entirely local: commit on a branch (or directly on main for small fixes), merge to main, push. Don't open pull requests. `gh pr create` is not part of any flow here. The only PR that exists is the release-please automation PR, and that's created and merged without human involvement.

## Output

JSONL to stdout. Every command emits one JSON object per line, ending with a `_meta` trailer. Errors as JSON to stderr. See `SPEC.md` for the full output model.

## Architecture decisions and gotchas

(Empty until implementation begins. Add notes here as issues are discovered. Examples of the kind of thing that belongs here: subtle Calendar API behaviors that aren't in the docs, gws quirks, gotchas with extended properties, edge cases in recurring events, race conditions and how we sidestep them.)

## Sandbox

`mise run` commands work fine in sandboxed processes. Network access required during `go mod tidy` (first run) and during `go test` for any test that pulls a module.

The `gws` subprocess uses the user's keyring; running tests against a real `gws` outside the fake-binary harness needs the sandbox disabled (Netskope proxy + keyring access).
