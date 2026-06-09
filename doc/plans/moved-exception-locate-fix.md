# Fix: recurring mirror instances become unsyncable once moved

## Status

Done (B24). Branch: `fix/locate-moved-exception`. Code + tests + SPEC landed;
`mise run test` + `mise run lint` green; Codex design review + independent
`feature-dev:code-reviewer` (3 passes) clean. Not yet merged/pushed - awaiting
user (a `fix:` push to main auto-ships a Homebrew release).

Follow-up filed in `doc/bugs.md` B24: `patchMirrorWithChecksum` can still drop
the post-main parent on a checksum-follow-up failure (pre-existing B19 gap on
that narrow sub-path).

## Symptom (verified)

User's recurring "Lunch & Reading" on 2026-06-10. Source (Personal,
`me@tammersaleh.com`) instance at 11:30-12:30; mirror (CoreWeave,
`tsaleh@coreweave.com`) stuck at 12:00-13:00. Source `updated`
2026-06-09T14:19Z; mirror `calendar-sync:source_updated` still
2026-06-08T19:31Z. Daemon healthy, ticking, pdir `ok` - but the
`personal-to-work` tick logged `skips:1`. Log line:

```
{"action":"skip","pair":"personal-to-work","source_event":"ahi8vpr13tii9lo75vrmte314f_20260610T183000Z","reason":"instance_unmaterializable"}
```

## Root cause

`internal/recurring/handler.go` `locateMirrorInstance` finds the mirror
instance via Google `events.instances?eventId=<mirrorParent>&originalStart=<src.originalStartTime.dateTime>&maxResults=1&showDeleted=true`.

Live-API finding: **Google's `originalStart` filter does not return an
instance once it has been moved off its native recurrence slot** (start !=
originalStartTime). Verified on the real mirror parent:

- 6/9 instance (un-moved, start==ost==11:30): filter returns 1, every tz form.
- 6/10 instance (moved, start=12:00, ost=11:30): filter returns 0 in
  `-07:00`, `-08:00`, and UTC `Z` form. A plain `timeMin/timeMax` window with
  no `originalStart` DOES return it (`id=<parent>_20260610T183000Z`).

The repair path re-fetches the source parent, force-rewrites the mirror
parent recurrence, and retries the **same** `originalStart` lookup - still 0,
because the instance is still a moved exception. Result:
`skip(instance_unmaterializable)` every tick, forever.

Self-perpetuating: a mirror instance becomes a moved exception precisely
because a prior sync moved it successfully. So **any recurring instance that
has ever been moved is permanently unsyncable for future source edits.** Not
lunch-specific.

SPEC.md (lines 1317-1325) encodes the wrong mental model: "empty = the mirror
parent's recurrence rule doesn't generate an instance at originalStart ...
stale recurrence." The recurrence is fine; the filter just doesn't match
moved exceptions. `doc/dry-run-anomaly-analysis.md:92` noticed the symptom.

## Fix: locate by constructed instance ID + GET

Stop using the `originalStart` filter. The instance ID is deterministic:
`<parentID>_<occurrenceKey>` where the occurrence key is the original start in
compact UTC (timed) or `YYYYMMDD` (all-day). Both series share the DTSTART
grid (`mirror.BuildPayload` copies source start/timezone verbatim; all-day
only overrides TimeZone, not Date), so the mirror instance has the SAME
occurrence-key suffix as the source instance.

Algorithm for `locateMirrorInstance`:

1. `key = occurrenceKey(source.ID)` - tail after the last `_` (handles `_R...`
   anchored parents: `foo_R20260323T163000_20260504T163000Z` -> last segment).
2. `mirrorInstanceID = mirrorParent.ID + "_" + key`.
3. `EventsGet(target, mirrorInstanceID)`.
4. Found -> sanity-check `RecurringEventID == mirrorParent.ID` and
   `OriginalStartTime` matches source's; return it.
5. `errors.Is(err, gws.ErrAPINotFound)` (404) -> repair: fetch source parent,
   force-rewrite mirror parent, rebuild ID with the repaired parent ID, retry
   GET.
6. Second GET 404 -> `skip(instance_unmaterializable)` with recurrence arrays
   (as today). Preserve `PostWriteMirrorParent` (B19).
7. Any non-404 GET error -> propagate as a real read failure.

GET returns cancelled instances natively, so the old `showDeleted=true`
rationale is moot (still need the cancelled mirror to resolve so the revive
cell can fire).

### Live-API verification of the fix

- GET `<parent>_20260610T183000Z` (moved exception) -> returns it (start
  12:00, ost 11:30, status confirmed). The bug case.
- GET `<parent>_20260611T183000Z` (plain non-exception occurrence) -> returns
  generated instance (start==ost==11:30). GET-by-synthetic-id works for
  non-exceptions.
- GET `<parent>_20260613T183000Z` (Saturday, outside MO-FR rule) ->
  `error[api]: Not Found` (404). Genuine unmaterializable cleanly 404s.

## Codex review notes (thread 019eace3)

Endorsed construct-ID-and-GET over windowed listing. Key asks:

- Centralize occurrence-key extraction in a tested helper (last-underscore
  tail). `internal/sync/target_delta.go` has a narrower regex
  `_\d{8}T\d{6}Z$` (timed-only) - adjacent fragility, note it; unifying is a
  follow-up, not required for this fix.
- Post-GET sanity check turns the inferred ID-shape assumption into a checked
  one.
- The only real risk: instance-ID string shape is server convention, not a
  documented contract. Accepted because the current filter is empirically
  wrong and the codebase already depends on the convention in target_delta.
- Fixtures: `makeSourceException` uses `ID:"src-evt"` - unrealistic, doesn't
  carry an occurrence key. Must use realistic suffixed IDs.

## Test plan (red first)

Unit - occurrence-key helper table: timed `parent_20260610T183000Z`, all-day
`parent_20260610`, anchored `parent_R20260323T163000_20260504T163000Z`,
no-underscore (malformed) -> error/empty.

Unit - recurring handler (realistic IDs):

1. first-GET hit.
2. 404 -> repair -> GET hit.
3. 404 -> repair -> 404 -> unmaterializable (preserves PostWriteMirrorParent +
   recurrence arrays).
4. cancelled instance resolves via GET (revive path).
5. all-day suffix.
6. `_R...` anchored parent.

Fix `src-evt` fixtures in `internal/recurring/handler_test.go` and any
`internal/sync` tests that drive the real handler
(`classify_test.go`, `transient_test.go`) to realistic instance IDs. Move
transient locate-path failures from the `events.instances` bucket to the
`events.get` bucket.

E2E - strengthen `internal/e2e/recurring_test.go`
(`TestE2E_InstanceOverridePropagates`) or add
`TestE2E_MovedRecurringInstance_RemainsSyncable`: sync moves the mirror
instance, then edit the source override again and assert the moved mirror is
relocated and patched. (E2E needs real gws + sandbox off; may defer to manual.)

## SPEC.md changes

Rewrite step 2 "locate the mirror instance" + "Zero-result instance lookup":
locate by constructed instance ID via GET; correct the "empty = stale
recurrence" claim; the 404 (not empty-list) is the repair trigger.

## Decisions

- User chose NOT to manually correct the stuck 6/10 mirror; the shipped fix
  will reconcile it on the next tick/full sync once the source changes or the
  full-sync inventory pass re-runs the (now-fixed) locate.
- Commit type `fix:` (user-facing: broken sync behavior that was promised).
