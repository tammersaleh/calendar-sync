package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestCalendarRef_UnmarshalTOML covers every input shape UnmarshalTOML
// promises to handle: string form (backwards-compat), table with summary
// only, table with summary+account, plus the rejection cases (empty
// string, unknown key, wrong type).
func TestCalendarRef_UnmarshalTOML(t *testing.T) {
	tests := []struct {
		name        string
		toml        string
		want        CalendarRef
		wantErr     bool
		wantErrText string
	}{
		{
			name: "string form preserves ID",
			toml: `source = "alice@example.com"`,
			want: CalendarRef{ID: "alice@example.com"},
		},
		{
			name: "string form preserves primary",
			toml: `source = "primary"`,
			want: CalendarRef{ID: "primary"},
		},
		{
			name:        "empty string rejected",
			toml:        `source = ""`,
			wantErr:     true,
			wantErrText: "must not be empty",
		},
		{
			name: "table with summary only",
			toml: `source = {summary = "TripIt"}`,
			want: CalendarRef{Summary: "TripIt"},
		},
		{
			name: "table with summary and account",
			toml: `source = {summary = "CoreWeave", account = "alice@example.com"}`,
			want: CalendarRef{Summary: "CoreWeave", Account: "alice@example.com"},
		},
		{
			name:        "table with unknown key rejected",
			toml:        `source = {summary = "X", flavor = "vanilla"}`,
			wantErr:     true,
			wantErrText: `unknown field "flavor"`,
		},
		{
			name:        "non-string non-table rejected",
			toml:        `source = 42`,
			wantErr:     true,
			wantErrText: "must be a string or inline table",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dest struct {
				Source CalendarRef `toml:"source"`
			}
			_, err := toml.Decode(tc.toml, &dest)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UnmarshalTOML(%q) returned nil; want error", tc.toml)
				}
				if tc.wantErrText != "" && !strings.Contains(err.Error(), tc.wantErrText) {
					t.Errorf("err = %q; want substring %q", err.Error(), tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalTOML(%q) returned error: %v", tc.toml, err)
			}
			if dest.Source != tc.want {
				t.Errorf("got %+v, want %+v", dest.Source, tc.want)
			}
		})
	}
}

// TestCalendarRef_TableEmptySummaryAllowedAtUnmarshal covers the "allow at
// unmarshal time, validatePair rejects" carve-out from the design plan: an
// inline table with a missing summary key (or summary = "") must NOT fail
// in UnmarshalTOML, so the JSON error envelope from validate covers it
// uniformly with the bare-string empty case.
func TestCalendarRef_TableEmptySummaryAllowedAtUnmarshal(t *testing.T) {
	var dest struct {
		Source CalendarRef `toml:"source"`
	}
	if _, err := toml.Decode(`source = {summary = ""}`, &dest); err != nil {
		t.Fatalf("UnmarshalTOML rejected empty-summary table: %v", err)
	}
	if dest.Source.IsSummaryRef() {
		t.Errorf("IsSummaryRef() = true on empty-summary table; want false")
	}
}

// TestCalendarRef_AccountWithoutSummaryAllowedAtUnmarshal: the rule
// "account requires summary" lives in validatePair; UnmarshalTOML is
// permissive so the validator owns the full required-fields error
// envelope. Pinned to prevent a future tightening at unmarshal time
// from silently bypassing the validate.go error path.
func TestCalendarRef_AccountWithoutSummaryAllowedAtUnmarshal(t *testing.T) {
	var dest struct {
		Source CalendarRef `toml:"source"`
	}
	if _, err := toml.Decode(`source = {account = "alice@example.com"}`, &dest); err != nil {
		t.Fatalf("UnmarshalTOML rejected account-only table: %v", err)
	}
	if dest.Source.Account != "alice@example.com" {
		t.Errorf("Account = %q, want alice@example.com", dest.Source.Account)
	}
}

// TestCalendarRef_MarshalJSON pins the wire shape:
//   - ID-form refs marshal as plain JSON strings (backwards-compat with
//     pre-F1 `pair list` / `config show` output).
//   - Summary-form refs marshal as objects so the consumer can see what
//     the user wrote.
func TestCalendarRef_MarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   CalendarRef
		want string
	}{
		{
			name: "ID-form emits as bare string",
			in:   CalendarRef{ID: "alice@example.com"},
			want: `"alice@example.com"`,
		},
		{
			name: "primary alias emits as bare string",
			in:   CalendarRef{ID: "primary"},
			want: `"primary"`,
		},
		{
			name: "summary-form emits as object",
			in:   CalendarRef{Summary: "TripIt"},
			want: `{"summary":"TripIt"}`,
		},
		{
			name: "summary+account emits both fields",
			in:   CalendarRef{Summary: "CoreWeave", Account: "alice@example.com"},
			want: `{"summary":"CoreWeave","account":"alice@example.com"}`,
		},
		{
			name: "empty ref emits as empty string (zero value round-trip)",
			in:   CalendarRef{},
			want: `""`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if got := string(b); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestCalendarRef_UnmarshalJSON_RoundTrips pins inverse symmetry of
// MarshalJSON. Necessary because cmd/pair_test.go decodes pairPayload via
// json.Unmarshal; without an UnmarshalJSON the source/target field would
// fail with a type-mismatch error.
func TestCalendarRef_UnmarshalJSON_RoundTrips(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want CalendarRef
	}{
		{
			name: "bare string decodes to ID",
			in:   `"alice@example.com"`,
			want: CalendarRef{ID: "alice@example.com"},
		},
		{
			name: "object with summary decodes to summary form",
			in:   `{"summary":"TripIt"}`,
			want: CalendarRef{Summary: "TripIt"},
		},
		{
			name: "object with summary and account",
			in:   `{"summary":"CoreWeave","account":"alice@example.com"}`,
			want: CalendarRef{Summary: "CoreWeave", Account: "alice@example.com"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got CalendarRef
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("Unmarshal(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCalendarRef_IsSummaryRef pins the predicate: any non-empty Summary
// flips the bit; ID-only refs are NOT summary refs even if Account is set
// (validatePair rejects the latter shape, but the predicate itself must
// stay strict on Summary so canonicalize routing is unambiguous).
func TestCalendarRef_IsSummaryRef(t *testing.T) {
	tests := []struct {
		name string
		in   CalendarRef
		want bool
	}{
		{"empty", CalendarRef{}, false},
		{"id only", CalendarRef{ID: "x@example.com"}, false},
		{"summary only", CalendarRef{Summary: "X"}, true},
		{"summary and account", CalendarRef{Summary: "X", Account: "y"}, true},
		// Sanity: an Account without Summary is malformed; the predicate
		// still returns false because "summary lookup" is gated on Summary.
		{"account without summary stays non-summary", CalendarRef{Account: "y"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.IsSummaryRef(); got != tc.want {
				t.Errorf("IsSummaryRef() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPair_TomlIntegration_StringSourceTarget pins the backwards-compat
// path end-to-end: a pre-F1 string source/target TOML decodes into a
// CalendarRef{ID: ...} on the Pair. This is the single most important
// behavior - every existing user config must continue to work unchanged.
func TestPair_TomlIntegration_StringSourceTarget(t *testing.T) {
	const cfg = `
[[pairs]]
name = "p"
source = "alice@example.com"
target = "primary"
`
	var c Config
	if _, err := toml.Decode(cfg, &c); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Pairs) != 1 {
		t.Fatalf("len(Pairs) = %d, want 1", len(c.Pairs))
	}
	if got, want := c.Pairs[0].Source, (CalendarRef{ID: "alice@example.com"}); got != want {
		t.Errorf("Source = %+v, want %+v", got, want)
	}
	if got, want := c.Pairs[0].Target, (CalendarRef{ID: "primary"}); got != want {
		t.Errorf("Target = %+v, want %+v", got, want)
	}
}

// TestPair_TomlIntegration_TableSourceTarget pins the new F1 path: an
// inline-table source decodes into a summary-form CalendarRef.
func TestPair_TomlIntegration_TableSourceTarget(t *testing.T) {
	const cfg = `
[[pairs]]
name = "p"
source = {summary = "TripIt"}
target = {summary = "CoreWeave", account = "alice@example.com"}
`
	var c Config
	if _, err := toml.Decode(cfg, &c); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got, want := c.Pairs[0].Source, (CalendarRef{Summary: "TripIt"}); got != want {
		t.Errorf("Source = %+v, want %+v", got, want)
	}
	if got, want := c.Pairs[0].Target, (CalendarRef{Summary: "CoreWeave", Account: "alice@example.com"}); got != want {
		t.Errorf("Target = %+v, want %+v", got, want)
	}
}
