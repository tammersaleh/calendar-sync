package mirror

import (
	"regexp"
	"strings"
	"testing"
)

// idCharset matches the character set Google's Calendar API allows for event
// IDs ([a-v0-9]) per SPEC.md "Deterministic mirror event IDs".
var idCharset = regexp.MustCompile(`^[a-v0-9]+$`)

func TestDeterministicMirrorID(t *testing.T) {
	t.Run("known fixture: alice@example.com:abc123def456", func(t *testing.T) {
		// Derivation (independent of this Go code):
		//   input  = "alice@example.com:abc123def456"
		//   sha256 = 6c7a1cd0221df81ffb0df386ecd33a0c37f1428b1b9dad0f2a968e9ac3efa768
		//   base32hex (RFC 4648 §7, uppercase) =
		//     DHT1PK123NS1VUODUE3EPKPQ1GRV2GKB3EEQQ3PAIQ79LGVFKTK0====
		//   lowercase + drop trailing pad + take first 50 chars + prefix "cs2".
		// Reproduce in Python:
		//   python3 -c 'import hashlib,base64;h=hashlib.sha256(b"alice@example.com:abc123def456").digest();print("cs2"+base64.b32hexencode(h).decode().lower()[:50])'
		got := DeterministicID("alice@example.com", "abc123def456")
		want := "cs2dht1pk123ns1vuodue3epkpq1grv2gkb3eeqq3paiq79lgvfkt"
		if got != want {
			t.Fatalf("DeterministicID(...) = %q, want %q", got, want)
		}
	})

	t.Run("known fixture: bob@example.com:xyz999", func(t *testing.T) {
		// Derivation (see fixture above for method):
		//   sha256 = 716aefc036e47154671ecd2f2e9b3a1af12bac74167fefd9bba4b32cc7966900
		//   base32hex lowercase =
		//     e5levg1mshol8poupknit6pq3boinb3k2pvuvmdrkipiphsmd400====
		got := DeterministicID("bob@example.com", "xyz999")
		want := "cs2e5levg1mshol8poupknit6pq3boinb3k2pvuvmdrkipiphsmd4"
		if got != want {
			t.Fatalf("DeterministicID(...) = %q, want %q", got, want)
		}
	})

	t.Run("length is 53 (cs2 + 50 base32hex chars)", func(t *testing.T) {
		got := DeterministicID("anything@example.com", "evt")
		if len(got) != 53 {
			t.Fatalf("len(DeterministicID(...)) = %d, want 53; got %q", len(got), got)
		}
	})

	t.Run("prefix is cs2", func(t *testing.T) {
		got := DeterministicID("anything@example.com", "evt")
		if !strings.HasPrefix(got, "cs2") {
			t.Fatalf("DeterministicID(...) = %q, want prefix cs2", got)
		}
	})

	t.Run("character set is Google's allowed [a-v0-9]", func(t *testing.T) {
		// Several inputs to cover encoding variety.
		inputs := [][2]string{
			{"alice@example.com", "abc123"},
			{"family@group.calendar.google.com", "ZZZZZZZZZZZ"},
			{"a", "b"},
			{strings.Repeat("x", 256), strings.Repeat("y", 256)},
		}
		for _, in := range inputs {
			got := DeterministicID(in[0], in[1])
			if !idCharset.MatchString(got) {
				t.Fatalf("DeterministicID(%q,%q) = %q; not in [a-v0-9]+", in[0], in[1], got)
			}
		}
	})

	t.Run("deterministic across calls", func(t *testing.T) {
		first := DeterministicID("alice@example.com", "abc123")
		second := DeterministicID("alice@example.com", "abc123")
		if first != second {
			t.Fatalf("DeterministicID is non-deterministic: %q != %q", first, second)
		}
	})

	t.Run("distinct inputs produce distinct IDs", func(t *testing.T) {
		// Both fields are colon-free in practice (calendar IDs are emails or
		// group IDs; event IDs are [a-v0-9]) so the spec's plain
		// concatenation cannot ambiguate. We test the cases that actually
		// occur: different calendars, different event IDs.
		cases := []struct {
			name string
			a1   string
			b1   string
			a2   string
			b2   string
		}{
			{"different calendar id", "alice@example.com", "abc", "bob@example.com", "abc"},
			{"different event id", "alice@example.com", "abc", "alice@example.com", "abd"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				first := DeterministicID(tc.a1, tc.b1)
				second := DeterministicID(tc.a2, tc.b2)
				if first == second {
					t.Fatalf("expected distinct IDs for distinct inputs; both = %q", first)
				}
			})
		}
	})
}
