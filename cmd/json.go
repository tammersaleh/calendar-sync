package cmd

import "encoding/json"

// jsonMarshal is a thin alias used by MarshalJSON implementations that
// don't want to import encoding/json directly into their file. Keeps the
// per-file imports tidy.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
