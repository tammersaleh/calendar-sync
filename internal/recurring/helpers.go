package recurring

import "strings"

// occurrenceKey extracts the occurrence-key suffix from a recurring-instance
// event ID: the substring after the LAST underscore. Google instance IDs are
// "<parentID>_<occurrenceKey>" where the key is the original start in compact
// UTC ("20260610T183000Z" for timed events) or "YYYYMMDD" (all-day).
//
// The last-underscore rule handles anchored parents whose own ID carries an
// underscore, e.g. "foo_R20260323T163000_20260504T163000Z" yields
// "20260504T163000Z". Returns ok=false when the ID has no underscore - the
// caller routed a non-instance event here, a programmer error in the
// classification logic.
func occurrenceKey(id string) (string, bool) {
	i := strings.LastIndex(id, "_")
	if i < 0 {
		return "", false
	}
	return id[i+1:], true
}
