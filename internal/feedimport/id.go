package feedimport

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// idPrefix tags every deterministic feed-import event ID. It is deliberately
// distinct from mirror's "cs2" so a feed-import event and a mirror event can
// never collide on a shared calendar, and so the two schemas evolve
// independently.
const idPrefix = "csf"

// idHashChars is how many base32hex characters of the SHA-256 digest go into
// the deterministic ID. Matches mirror's 50; with the 3-char prefix every ID
// is 53 characters, inside Google's [5,1024] event-ID range.
const idHashChars = 50

// DeterministicID returns the feed-local event ID for a feed item, derived as
//
//	"csf" + lowercase(base32hex(sha256(feedID + ":" + uid))[:50])
//
// Output: 53 characters, charset [a-v0-9] (Google's allowed event-ID set).
//
// feedID namespaces the ID so two feeds writing the same target calendar can
// never produce the same Google ID for different source UIDs. It is also the
// de-duplication key for inserts: a re-run computes the same ID, so Google
// rejects the duplicate with HTTP 409 and the importer takes the revive path.
func DeterministicID(feedID, uid string) string {
	sum := sha256.Sum256([]byte(feedID + ":" + uid))
	encoded := strings.ToLower(base32.HexEncoding.EncodeToString(sum[:]))
	return idPrefix + encoded[:idHashChars]
}
