// Package mirror provides the pure-logic primitives that calendar-sync uses
// to identify, hash, and serialize mirror events. None of these functions
// touch the network or the filesystem; they implement the rules in SPEC.md
// at the level of strings and structs.
package mirror

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// idPrefix tags every deterministic mirror event ID. The trailing digit
// matches the current calendar-sync:version property; bumping the schema
// version implies bumping this prefix so a parallel-deployment bug couldn't
// produce two mirror events with the same Google ID for different schemas.
const idPrefix = "cs2"

// idHashChars is how many base32hex characters of the SHA-256 digest go into
// the deterministic ID. SPEC.md fixes this at 50; combined with the 3-char
// prefix that puts every ID at length 53, well within Google's [5,1024] range.
const idHashChars = 50

// DeterministicID returns the mirror event ID for a source event, derived per
// SPEC.md "Deterministic mirror event IDs" as
//
//	"cs2" + lowercase(base32hex(sha256(canonicalCalendarID + ":" + sourceEventID))[:50])
//
// Output: 53 characters, charset [a-v0-9] (Google's allowed event-ID set).
//
// The function is the de-duplication key for new-mirror inserts: two processes
// racing to mirror the same source event compute the same ID, so Google
// rejects the second insert with HTTP 409 instead of producing duplicates.
func DeterministicID(canonicalCalendarID, sourceEventID string) string {
	sum := sha256.Sum256([]byte(canonicalCalendarID + ":" + sourceEventID))
	encoded := strings.ToLower(base32.HexEncoding.EncodeToString(sum[:]))
	return idPrefix + encoded[:idHashChars]
}
