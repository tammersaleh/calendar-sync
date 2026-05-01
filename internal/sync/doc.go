// Package sync implements SPEC.md's Classification logic and surrounding
// orchestration. It runs once per source event per pdir and decides what to
// do with the corresponding mirror: insert, patch, delete, propagate, revert,
// or skip.
//
// The package is organized by phase:
//
//   - inventory.go: per-target mirror inventory plus the SPEC §"Mirror
//     inventory rebuild" two-pass loader.
//   - classify.go: the 8-step Classifier switch (SPEC §"Classification logic").
//   - insert.go: the no-mirror branch (events.insert + 409 handling per SPEC
//     §step 8 first bullet).
//   - drift.go: drift-handling helpers shared between the inventory-hit code
//     path and the v1 migration cells.
//   - migration.go: the two v1-specific cells (migration_upgrade,
//     migration_source_won) that live outside mirror.Classify per CLAUDE.md
//     "v1 migration cells live in callers".
//   - helpers.go: patchMirrorWithChecksum (the SPEC §"Computing the checksum
//     from the post-write event" two-step write).
//   - orphan.go: SPEC §"Daemon lifecycle: periodic full re-sync" step 5,
//     the post-classify cleanup walk that prunes mirrors whose source is
//     gone, outside horizon, or now-ineligible.
//
// Layer 6.A covers the parent / non-recurring path; recurring instances
// delegate to internal/recurring's Handler via the Classifier's Recurring
// field. Layer 6.B adds the orphan walk; the Reconciler entrypoint
// (FullSync, Tick) is added in 6.C.
package sync
