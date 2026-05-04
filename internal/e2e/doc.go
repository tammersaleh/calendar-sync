//go:build e2e

// Package e2e holds end-to-end tests that run calendar-sync against real
// Google Calendar via the user's authenticated gws subprocess. They are
// gated behind the `e2e` build tag so the default `go test ./...` and the
// pre-push `check` task never trigger them.
//
// Run them with:
//
//	mise run test:e2e
//
// which sets CALENDAR_SYNC_E2E=1. The harness refuses to start without
// the guard env var so an accidental `go test -tags=e2e ./...` cannot
// touch any of the user's calendars.
//
// Test fixture calendars are created and destroyed by the harness itself
// (no per-machine env vars, no checked-in IDs). See doc/e2e-design.md
// for the full rationale, scenario list, and what's deferred to unit
// tests.
package e2e
