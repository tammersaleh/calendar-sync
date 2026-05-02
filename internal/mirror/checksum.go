package mirror

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// EventDateTime is the canonical Calendar-API representation of a start or end
// time. Either Date (an all-day "YYYY-MM-DD") or DateTime (an RFC 3339
// timestamp) is set, never both. TimeZone is an IANA name attached to a
// DateTime.
//
// JSON field ordering is alphabetical (date < dateTime < timeZone) so that
// encoding/json's struct-order serialization produces the SPEC.md canonical
// "object keys sorted" form without any post-processing.
type EventDateTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

// ManagedFields is the subset of an Event resource that calendar-sync writes
// on every mirror and watches for drift. The checksum hashes a canonical
// serialization of these fields.
//
// Fields are declared in alphabetical order so encoding/json emits keys in
// the order SPEC.md's canonical form requires. `recurrence` is omitted entirely
// for non-recurring events and for instance overrides; an empty slice and a
// nil slice both serialize the same way thanks to omitempty.
type ManagedFields struct {
	Description  string        `json:"description"`
	End          EventDateTime `json:"end"`
	Location     string        `json:"location"`
	Recurrence   []string      `json:"recurrence,omitempty"`
	Start        EventDateTime `json:"start"`
	Summary      string        `json:"summary"`
	Transparency string        `json:"transparency"`
	Visibility   string        `json:"visibility"`
}

// Checksum returns "sha256:<hex>" over a canonical JSON serialization of m
// per SPEC.md "Drift detection model" / "Managed fields and the checksum".
//
// Canonical form: object keys sorted (handled by alphabetical struct field
// declaration), no whitespace, no HTML escaping (RFC 8259 strict). Recurrence
// is sorted before hashing so two equivalent rule sets in different orders
// produce the same hash. The caller's slice is not mutated.
//
// Caller contract: per SPEC.md "Computing the checksum from the post-write
// event", m must be populated from the Event resource Google returns AFTER
// the insert/patch, never from the outbound request payload. Google
// normalizes timezone strings, RRULE formatting, and description whitespace
// on write; hashing the pre-write payload would produce a checksum that
// disagrees with the next read of the same mirror and trigger spurious drift.
// This function does not enforce that contract; the sync layer does.
func Checksum(m ManagedFields) string {
	if len(m.Recurrence) > 1 {
		sorted := make([]string, len(m.Recurrence))
		copy(sorted, m.Recurrence)
		sort.Strings(sorted)
		m.Recurrence = sorted
	}

	raw, err := canonicalJSON(m)
	if err != nil {
		// ManagedFields contains only strings, structs of strings, and a
		// []string. encoding/json cannot fail on any of those, so reaching
		// here means a programmer error in the type definition.
		panic(fmt.Errorf("mirror: marshal ManagedFields: %w", err))
	}

	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canonicalJSON encodes v with HTML escaping disabled. encoding/json's default
// behavior of replacing <, >, and & with \u escapes is aimed at HTML embedding
// and is not part of RFC 8259; allowing it would let a user-typed "<" in a
// description quietly shift the canonical form between versions.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a newline; canonical JSON has no whitespace.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
