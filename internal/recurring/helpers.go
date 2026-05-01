package recurring

import (
	"errors"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// computeOriginalStart picks the originalStart query string for SPEC.md
// "Step 2": prefer DateTime (timezone-aware RFC 3339) over Date (all-day
// "YYYY-MM-DD"). Returns an error when both are missing - the caller
// routed a non-exception event here, which is a programmer error in the
// classification logic.
func computeOriginalStart(t *gws.EventDateTime) (string, error) {
	if t == nil {
		return "", errors.New("recurring: source.OriginalStartTime is nil")
	}
	if t.DateTime != "" {
		return t.DateTime, nil
	}
	if t.Date != "" {
		return t.Date, nil
	}
	return "", errors.New("recurring: source.OriginalStartTime has neither DateTime nor Date")
}
