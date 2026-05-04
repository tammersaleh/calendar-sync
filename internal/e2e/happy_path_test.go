//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestE2E_HappyPath_Insert is the smoke test for the harness and the
// most basic SPEC contract: a source event with no mirror produces an
// `insert/source_updated` outcome and a real mirror event lands on the
// target calendar with the managed-field extended properties at
// SchemaVersion 3 and a checksum that matches the post-write managed
// fields.
func TestE2E_HappyPath_Insert(t *testing.T) {
	h := Setup(t, SetupOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	title := h.Title("happy-insert")
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: title,
		Start:   futureDateTime(0),
		End:     futureDateTime(1 * time.Hour),
	})

	res := h.Run(ctx)
	res.AssertSuccess(t)

	out := res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionInsert),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: source.ID,
	})
	if out.TargetEvent == "" {
		t.Fatal("insert outcome has empty target_event")
	}
	if out.Pair != defaultPairName {
		t.Errorf("outcome.pair = %q, want %q", out.Pair, defaultPairName)
	}

	// Verify the actual mirror event on the target calendar carries
	// the managed-field extended properties and a valid checksum.
	mir, err := h.GWS.EventsGet(ctx, h.TargetCalID, out.TargetEvent)
	if err != nil {
		t.Fatalf("get mirror event %s: %v", out.TargetEvent, err)
	}
	if mir.ExtendedProperties == nil || mir.ExtendedProperties.Private == nil {
		t.Fatalf("mirror missing extendedProperties.private; got %+v", mir.ExtendedProperties)
	}
	priv := mir.ExtendedProperties.Private

	if got := priv[mirror.ExtKeyVersion]; got != mirror.SchemaVersion {
		t.Errorf("mirror %s = %q, want %q", mirror.ExtKeyVersion, got, mirror.SchemaVersion)
	}
	if got := priv[mirror.ExtKeySource]; got == "" {
		t.Errorf("mirror %s is empty", mirror.ExtKeySource)
	}
	if got := priv[mirror.ExtKeySourceUpdated]; got != source.Updated {
		t.Errorf("mirror %s = %q, want source.Updated %q", mirror.ExtKeySourceUpdated, got, source.Updated)
	}

	// Checksum: stored value must equal a fresh hash of the mirror's
	// own current managed fields. SPEC §"Computing the checksum from
	// the post-write event" pins this contract.
	storedChecksum := priv[mirror.ExtKeyChecksum]
	if storedChecksum == "" {
		t.Fatal("mirror has empty calendar-sync:checksum")
	}
	live := mirror.ManagedFieldsFromEvent(mir)
	expected := mirror.Checksum(live)
	if storedChecksum != expected {
		t.Errorf("stored checksum %q != recomputed %q (managed fields: %+v)", storedChecksum, expected, live)
	}

	// Mirror payload sanity. SPEC §"Managed fields and the checksum"
	// pins a clean mirror at transparency=opaque, visibility=private.
	// Use the same normalization the production drift signal uses:
	// Calendar API omits transparency when it equals the default (opaque),
	// so checking the raw response field would false-fail.
	if got := live.Transparency; got != gws.TransparencyOpaque {
		t.Errorf("normalized mirror.transparency = %q, want %q", got, gws.TransparencyOpaque)
	}
	if got := live.Visibility; got != gws.VisibilityPrivate {
		t.Errorf("normalized mirror.visibility = %q, want %q", got, gws.VisibilityPrivate)
	}
	if mir.Summary != title {
		t.Errorf("mirror.summary = %q, want %q", mir.Summary, title)
	}

	// Mirror id should be the deterministic id derived from
	// (source calendar, source event id). SPEC pins this for new
	// inserts.
	wantID := mirror.DeterministicID(h.SourceCalID, source.ID)
	if mir.ID != wantID {
		t.Errorf("mirror id = %q, want deterministic %q", mir.ID, wantID)
	}

	// Idempotency: running again produces a single skip/unchanged.
	res2 := h.Run(ctx)
	res2.AssertSuccess(t)
	res2.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionSkip),
		Reason:      string(mirror.ReasonUnchanged),
		SourceEvent: source.ID,
	})
}

// mustInsertSource creates an event on the harness's source calendar
// and returns the post-write Event. Failure is fatal because the test
// can't proceed without it.
func mustInsertSource(t *testing.T, h *Harness, ctx context.Context, body *gws.Event) *gws.Event {
	t.Helper()
	got, err := h.GWS.EventsInsert(ctx, h.SourceCalID, body)
	if err != nil {
		t.Fatalf("insert source event: %v", err)
	}
	return got
}

// futureDateTime returns an EventDateTime offset from "now" by delta.
// 24h ahead so even tests run near midnight don't accidentally produce
// past events that filtering would reject.
func futureDateTime(delta time.Duration) *gws.EventDateTime {
	t := time.Now().UTC().Add(24*time.Hour + delta).Format(time.RFC3339)
	return &gws.EventDateTime{DateTime: t, TimeZone: "UTC"}
}

// TestE2E_SourceModified_PatchMirror pins the source-changed path of the
// drift matrix: a managed-field edit on the source produces an
// `action=patch, reason=source_updated` outcome and the mirror's stored
// bookkeeping (source_updated + checksum) advances to match the post-
// patch source. Schema version stays at the current SchemaVersion - a
// patch of an already-current mirror is not a migration.
func TestE2E_SourceModified_PatchMirror(t *testing.T) {
	h := Setup(t, SetupOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	originalTitle := h.Title("patch-original")
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: originalTitle,
		Start:   futureDateTime(0),
		End:     futureDateTime(1 * time.Hour),
	})

	res := h.Run(ctx)
	res.AssertSuccess(t)
	insertOut := res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionInsert),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: source.ID,
	})
	mirrorID := insertOut.TargetEvent
	if mirrorID == "" {
		t.Fatal("insert outcome has empty target_event")
	}

	preMirror, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorID)
	if err != nil {
		t.Fatalf("get mirror %s: %v", mirrorID, err)
	}
	prePriv := preMirror.ExtendedProperties.Private
	preSourceUpdated := prePriv[mirror.ExtKeySourceUpdated]
	preChecksum := prePriv[mirror.ExtKeyChecksum]
	if preSourceUpdated == "" || preChecksum == "" {
		t.Fatalf("pre-patch mirror missing bookkeeping: source_updated=%q checksum=%q", preSourceUpdated, preChecksum)
	}

	// Patch source.summary. EventsPatch returns the post-write source
	// resource; its Updated stamp is what the mirror should adopt.
	newTitle := h.Title("patch-edited")
	patched, err := h.GWS.EventsPatch(ctx, h.SourceCalID, source.ID, &gws.Event{Summary: newTitle})
	if err != nil {
		t.Fatalf("patch source %s: %v", source.ID, err)
	}
	if patched.Summary != newTitle {
		t.Fatalf("post-patch source.summary = %q, want %q", patched.Summary, newTitle)
	}
	if patched.Updated == preSourceUpdated {
		// Calendar API bumps `updated` on every server-accepted change;
		// equality here would mean either the patch silently no-op'd or
		// the API returned a stale resource. Either way the rest of the
		// test loses meaning.
		t.Fatalf("source.updated did not advance after patch: still %q", patched.Updated)
	}

	res2 := h.Run(ctx)
	res2.AssertSuccess(t)
	patchOut := res2.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionPatch),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: source.ID,
		TargetEvent: mirrorID,
	})
	if patchOut.TargetEvent != mirrorID {
		t.Errorf("patch outcome target_event = %q, want %q", patchOut.TargetEvent, mirrorID)
	}

	postMirror, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorID)
	if err != nil {
		t.Fatalf("get mirror %s post-patch: %v", mirrorID, err)
	}
	if postMirror.Summary != newTitle {
		t.Errorf("post-patch mirror.summary = %q, want %q", postMirror.Summary, newTitle)
	}
	postPriv := postMirror.ExtendedProperties.Private
	if got := postPriv[mirror.ExtKeySourceUpdated]; got != patched.Updated {
		t.Errorf("mirror %s = %q, want post-patch source.Updated %q",
			mirror.ExtKeySourceUpdated, got, patched.Updated)
	}
	postChecksum := postPriv[mirror.ExtKeyChecksum]
	if postChecksum == "" {
		t.Fatal("post-patch mirror has empty calendar-sync:checksum")
	}
	if postChecksum == preChecksum {
		t.Errorf("checksum did not change after summary patch: still %q", postChecksum)
	}
	// Stored checksum must equal a fresh hash of the mirror's own current
	// managed fields. Same contract as the happy-path assertion.
	live := mirror.ManagedFieldsFromEvent(postMirror)
	if expected := mirror.Checksum(live); postChecksum != expected {
		t.Errorf("stored checksum %q != recomputed %q (managed fields: %+v)", postChecksum, expected, live)
	}
	if got := postPriv[mirror.ExtKeyVersion]; got != mirror.SchemaVersion {
		t.Errorf("mirror %s = %q, want %q", mirror.ExtKeyVersion, got, mirror.SchemaVersion)
	}
}
