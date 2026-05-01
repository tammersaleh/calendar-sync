package cmd

import (
	"fmt"
	"runtime"
)

// Build-time-injected version metadata. ldflags wires these on a tagged
// release; an unset Version means "dev" in the JSON output, which is
// useful when developers run `go run ./cmd/calendar-sync version` against
// a dirty checkout.
var (
	Version   = "dev"
	Commit    = ""
	BuildDate = ""
)

// VersionCmd implements `calendar-sync version`. SPEC §"calendar-sync
// version" lines 833-844: a single JSON line with version/commit/date/go,
// followed by `_meta`. --short emits just the version.
type VersionCmd struct {
	Short bool `name:"short" help:"Just the version, no build metadata."`
}

// Run prints the version per SPEC. --short bypasses the JSONL printer
// because the SPEC explicitly says "just the version, no metadata" - i.e.
// raw text, not a JSON object.
func (c *VersionCmd) Run(rt *Runtime) error {
	if c.Short {
		if rt.Stdout != nil {
			fmt.Fprintln(rt.Stdout, Version)
		}
		return nil
	}

	p := rt.printer()
	p.Emit(versionPayload{
		Version: Version,
		Commit:  Commit,
		Date:    BuildDate,
		Go:      runtime.Version(),
	})
	p.Meta(metaCount{Count: 1})
	return nil
}

// versionPayload is SPEC line 842's wire shape for the version JSON.
type versionPayload struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`
	Go      string `json:"go"`
}

// metaCount is the trivial `{"count":N}` shape several commands emit as
// their `_meta` body.
type metaCount struct {
	Count int `json:"count"`
}
