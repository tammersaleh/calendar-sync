package mirror

import (
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

func TestChecksum(t *testing.T) {
	// Each known fixture below pins the canonical JSON form alongside the
	// expected hash. The hash values come from independent Python (json.dumps
	// + hashlib.sha256) so a regression in our serializer or struct order
	// fails one of these without depending on Go for both sides.
	t.Run("known fixture: minimal all-empty", func(t *testing.T) {
		// Canonical: {"description":"","end":{},"location":"","start":{},"summary":"","transparency":"","visibility":""}
		got := Checksum(ManagedFields{})
		want := "sha256:0e82458983b2a3f68c54feb005433b5c1825490cd37c14f283288571af2468dc"
		if got != want {
			t.Fatalf("Checksum = %q, want %q", got, want)
		}
	})

	t.Run("known fixture: typical mirror payload", func(t *testing.T) {
		// Canonical: {"description":"with bob\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC","end":{"dateTime":"2026-04-30T13:00:00Z","timeZone":"UTC"},"location":"","start":{"dateTime":"2026-04-30T12:00:00Z","timeZone":"UTC"},"summary":"Lunch","transparency":"opaque","visibility":"private"}
		got := Checksum(ManagedFields{
			Description:  "with bob\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC",
			Start:        EventDateTime{DateTime: "2026-04-30T12:00:00Z", TimeZone: "UTC"},
			End:          EventDateTime{DateTime: "2026-04-30T13:00:00Z", TimeZone: "UTC"},
			Summary:      "Lunch",
			Transparency: "opaque",
			Visibility:   "private",
		})
		want := "sha256:8a21731a38c52f3fd7cdb6a4aadf05c9c93398289e7fc0525b6bcc600e7febf9"
		if got != want {
			t.Fatalf("Checksum = %q, want %q", got, want)
		}
	})

	t.Run("known fixture: recurring event", func(t *testing.T) {
		// Canonical (note Recurrence already in alphabetical order): {"description":"weekly","end":{"dateTime":"2026-04-30T13:00:00Z","timeZone":"UTC"},"location":"","recurrence":["EXDATE;TZID=UTC:20260507T120000","RRULE:FREQ=WEEKLY"],"start":{"dateTime":"2026-04-30T12:00:00Z","timeZone":"UTC"},"summary":"Standup","transparency":"opaque","visibility":"private"}
		got := Checksum(ManagedFields{
			Description:  "weekly",
			Start:        EventDateTime{DateTime: "2026-04-30T12:00:00Z", TimeZone: "UTC"},
			End:          EventDateTime{DateTime: "2026-04-30T13:00:00Z", TimeZone: "UTC"},
			Recurrence:   []string{"EXDATE;TZID=UTC:20260507T120000", "RRULE:FREQ=WEEKLY"},
			Summary:      "Standup",
			Transparency: "opaque",
			Visibility:   "private",
		})
		want := "sha256:09063340a3c3b222e05c2fe28724b8891d89bddd20d7ee894e58e360d33ab8de"
		if got != want {
			t.Fatalf("Checksum = %q, want %q", got, want)
		}
	})

	t.Run("known fixture: all-day event", func(t *testing.T) {
		// Canonical: {"description":"trip","end":{"date":"2026-05-02"},"location":"","start":{"date":"2026-05-01"},"summary":"Vacation","transparency":"opaque","visibility":"private"}
		got := Checksum(ManagedFields{
			Description:  "trip",
			Start:        EventDateTime{Date: "2026-05-01"},
			End:          EventDateTime{Date: "2026-05-02"},
			Summary:      "Vacation",
			Transparency: "opaque",
			Visibility:   "private",
		})
		want := "sha256:ccd5426ae88b48fec556ee6eef21c1dce2f65c6a63714f54c4db66c638102e64"
		if got != want {
			t.Fatalf("Checksum = %q, want %q", got, want)
		}
	})

	t.Run("idempotent across calls", func(t *testing.T) {
		m := ManagedFields{Summary: "x"}
		first := Checksum(m)
		second := Checksum(m)
		if first != second {
			t.Fatalf("non-deterministic: %q != %q", first, second)
		}
	})

	t.Run("recurrence sorted before hashing", func(t *testing.T) {
		base := ManagedFields{
			Recurrence: []string{"RRULE:FREQ=DAILY", "EXDATE;TZID=UTC:20260507T120000"},
		}
		reordered := ManagedFields{
			Recurrence: []string{"EXDATE;TZID=UTC:20260507T120000", "RRULE:FREQ=DAILY"},
		}
		if Checksum(base) != Checksum(reordered) {
			t.Fatalf("checksum changed when only recurrence order differed")
		}
	})

	t.Run("recurrence sort does not mutate caller's slice", func(t *testing.T) {
		original := []string{"RRULE:FREQ=DAILY", "EXDATE;TZID=UTC:20260507T120000"}
		input := make([]string, len(original))
		copy(input, original)

		_ = Checksum(ManagedFields{Recurrence: input})

		for i, v := range input {
			if v != original[i] {
				t.Fatalf("Checksum mutated caller's Recurrence slice at index %d: got %q, want %q",
					i, v, original[i])
			}
		}
	})

	t.Run("nil and empty recurrence hash equal", func(t *testing.T) {
		nilRec := Checksum(ManagedFields{})
		emptyRec := Checksum(ManagedFields{Recurrence: []string{}})
		if nilRec != emptyRec {
			t.Fatalf("nil vs empty recurrence produced different hashes: %q != %q", nilRec, emptyRec)
		}
	})

	t.Run("html-special chars not escaped (RFC 8259 strict)", func(t *testing.T) {
		// SPEC.md "RFC 8259 form" doesn't require escaping <, >, & which
		// Go's encoding/json applies by default. The implementation must
		// disable HTML escaping so user-supplied description text doesn't
		// silently shift the canonical form (and the hash).
		got := Checksum(ManagedFields{Description: "1 < 2 && 3 > 2"})
		// Precomputed against the canonical form
		// {"description":"1 < 2 && 3 > 2","end":{},"location":"","start":{},"summary":"","transparency":"","visibility":""}
		want := "sha256:ef62e5ff376f0b9047cb7a07f417a043402f804972634de1fc5386857c2b98cc"
		if got != want {
			t.Fatalf("Checksum with HTML-special chars = %q, want %q", got, want)
		}
	})

	t.Run("output format: sha256: prefix and hex digest length", func(t *testing.T) {
		got := Checksum(ManagedFields{Summary: "x"})
		if !strings.HasPrefix(got, "sha256:") {
			t.Fatalf("Checksum %q missing sha256: prefix", got)
		}
		hexPart := strings.TrimPrefix(got, "sha256:")
		if len(hexPart) != 64 {
			t.Fatalf("hex digest length = %d, want 64; got %q", len(hexPart), hexPart)
		}
		for _, r := range hexPart {
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
			if !isHex {
				t.Fatalf("non-hex char %q in digest %q", r, hexPart)
			}
		}
	})

	t.Run("differing fields produce differing checksums", func(t *testing.T) {
		base := ManagedFields{
			Summary:      "Lunch",
			Description:  "with bob",
			Start:        EventDateTime{DateTime: "2026-04-30T12:00:00Z"},
			End:          EventDateTime{DateTime: "2026-04-30T13:00:00Z"},
			Transparency: "opaque",
			Visibility:   "private",
		}
		baseSum := Checksum(base)

		mutate := func(name string, mutator func(*ManagedFields)) {
			t.Run(name, func(t *testing.T) {
				m := base
				mutator(&m)
				if Checksum(m) == baseSum {
					t.Fatalf("expected different checksum after mutating %s", name)
				}
			})
		}
		mutate("summary", func(m *ManagedFields) { m.Summary = "Dinner" })
		mutate("description", func(m *ManagedFields) { m.Description = "with alice" })
		mutate("start.dateTime", func(m *ManagedFields) {
			m.Start = EventDateTime{DateTime: "2026-04-30T11:00:00Z"}
		})
		mutate("end.dateTime", func(m *ManagedFields) {
			m.End = EventDateTime{DateTime: "2026-04-30T14:00:00Z"}
		})
		mutate("transparency", func(m *ManagedFields) { m.Transparency = "transparent" })
		mutate("visibility", func(m *ManagedFields) { m.Visibility = "default" })
		mutate("recurrence added", func(m *ManagedFields) {
			m.Recurrence = []string{"RRULE:FREQ=DAILY"}
		})
		mutate("location", func(m *ManagedFields) { m.Location = "Conference room A" })
	})

	t.Run("location: same value -> same hash", func(t *testing.T) {
		// Pin that two ManagedFields with identical Location hash equal. This
		// guards against accidental non-determinism in the location field
		// (e.g. an unintended map-typed encoding).
		a := Checksum(ManagedFields{Location: "Office"})
		b := Checksum(ManagedFields{Location: "Office"})
		if a != b {
			t.Fatalf("same location produced different hashes: %q != %q", a, b)
		}
	})

	t.Run("location: different value -> different hash", func(t *testing.T) {
		a := Checksum(ManagedFields{Location: "Office A"})
		b := Checksum(ManagedFields{Location: "Office B"})
		if a == b {
			t.Fatalf("different locations produced equal hashes: %q", a)
		}
	})

	t.Run("location: empty equals zero-value", func(t *testing.T) {
		// An explicit empty-string Location must hash the same as the zero
		// value (no location set), because an event Google returns with no
		// location and one with location="" are indistinguishable on the
		// wire.
		base := Checksum(ManagedFields{})
		empty := Checksum(ManagedFields{Location: ""})
		if base != empty {
			t.Fatalf("zero vs empty-string location produced different hashes: %q != %q", base, empty)
		}
	})
}

func TestChecksum_StableAcrossEmptyAndDefaultTransparency(t *testing.T) {
	// Whether the Event came from a patch response (explicit "opaque") or a
	// list response (omitted -> ""), ManagedFieldsFromEvent must produce the
	// same ManagedFields, and Checksum must therefore produce the same hash.
	// Without this guarantee the stored post-write checksum disagrees with
	// the next read's live recompute and fires MirrorDrifted forever.
	t.Run("transparency", func(t *testing.T) {
		listResp := &gws.Event{Summary: "Standup", Transparency: ""}
		patchResp := &gws.Event{Summary: "Standup", Transparency: gws.TransparencyOpaque}
		fromList := Checksum(ManagedFieldsFromEvent(listResp))
		fromPatch := Checksum(ManagedFieldsFromEvent(patchResp))
		if fromList != fromPatch {
			t.Errorf("checksum drift between empty and default transparency:\n  fromList  = %s\n  fromPatch = %s", fromList, fromPatch)
		}
	})
	t.Run("visibility", func(t *testing.T) {
		listResp := &gws.Event{Summary: "Standup", Visibility: ""}
		patchResp := &gws.Event{Summary: "Standup", Visibility: gws.VisibilityDefault}
		fromList := Checksum(ManagedFieldsFromEvent(listResp))
		fromPatch := Checksum(ManagedFieldsFromEvent(patchResp))
		if fromList != fromPatch {
			t.Errorf("checksum drift between empty and default visibility:\n  fromList  = %s\n  fromPatch = %s", fromList, fromPatch)
		}
	})
}
