package mirror

import (
	"strings"
	"testing"
)

func TestChecksum(t *testing.T) {
	// Each known fixture below pins the canonical JSON form alongside the
	// expected hash. The hash values come from independent Python (json.dumps
	// + hashlib.sha256) so a regression in our serializer or struct order
	// fails one of these without depending on Go for both sides.
	t.Run("known fixture: minimal all-empty", func(t *testing.T) {
		// Canonical: {"description":"","end":{},"start":{},"summary":"","transparency":"","visibility":""}
		got := Checksum(ManagedFields{})
		want := "sha256:e4a973bfad828d49419d4e9734d54eb53c040156267a241ef0ea40faf1377ea8"
		if got != want {
			t.Fatalf("Checksum = %q, want %q", got, want)
		}
	})

	t.Run("known fixture: typical mirror payload", func(t *testing.T) {
		// Canonical: {"description":"with bob\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC","end":{"dateTime":"2026-04-30T13:00:00Z","timeZone":"UTC"},"start":{"dateTime":"2026-04-30T12:00:00Z","timeZone":"UTC"},"summary":"Lunch","transparency":"opaque","visibility":"private"}
		got := Checksum(ManagedFields{
			Description:  "with bob\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC",
			Start:        EventDateTime{DateTime: "2026-04-30T12:00:00Z", TimeZone: "UTC"},
			End:          EventDateTime{DateTime: "2026-04-30T13:00:00Z", TimeZone: "UTC"},
			Summary:      "Lunch",
			Transparency: "opaque",
			Visibility:   "private",
		})
		want := "sha256:5c23b7d9caa8108e61760b5a3405b14e7783aaf6ed13f6e7a3d4a1519bc8a245"
		if got != want {
			t.Fatalf("Checksum = %q, want %q", got, want)
		}
	})

	t.Run("known fixture: recurring event", func(t *testing.T) {
		// Canonical (note Recurrence already in alphabetical order): {"description":"weekly","end":{"dateTime":"2026-04-30T13:00:00Z","timeZone":"UTC"},"recurrence":["EXDATE;TZID=UTC:20260507T120000","RRULE:FREQ=WEEKLY"],"start":{"dateTime":"2026-04-30T12:00:00Z","timeZone":"UTC"},"summary":"Standup","transparency":"opaque","visibility":"private"}
		got := Checksum(ManagedFields{
			Description:  "weekly",
			Start:        EventDateTime{DateTime: "2026-04-30T12:00:00Z", TimeZone: "UTC"},
			End:          EventDateTime{DateTime: "2026-04-30T13:00:00Z", TimeZone: "UTC"},
			Recurrence:   []string{"EXDATE;TZID=UTC:20260507T120000", "RRULE:FREQ=WEEKLY"},
			Summary:      "Standup",
			Transparency: "opaque",
			Visibility:   "private",
		})
		want := "sha256:4d156aba53fe505c8c8a961736f2a1c39d94bbcc4dd6345f8e5df3e0d7e1a782"
		if got != want {
			t.Fatalf("Checksum = %q, want %q", got, want)
		}
	})

	t.Run("known fixture: all-day event", func(t *testing.T) {
		// Canonical: {"description":"trip","end":{"date":"2026-05-02"},"start":{"date":"2026-05-01"},"summary":"Vacation","transparency":"opaque","visibility":"private"}
		got := Checksum(ManagedFields{
			Description:  "trip",
			Start:        EventDateTime{Date: "2026-05-01"},
			End:          EventDateTime{Date: "2026-05-02"},
			Summary:      "Vacation",
			Transparency: "opaque",
			Visibility:   "private",
		})
		want := "sha256:3284a9c295696420c3cfd92fcbbc241ccc12c2ca1e7df2e7eb35fd25d7c2c4d4"
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
		// {"description":"1 < 2 && 3 > 2","end":{},"start":{},"summary":"","transparency":"","visibility":""}
		want := "sha256:00fc92f53551c0906f7f5e186136c9d875fa3fe0f234f953a5d90b471cf03eb8"
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
	})
}
