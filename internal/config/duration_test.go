package config

import (
	"strings"
	"testing"
	"time"
)

func TestDuration_UnmarshalText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"60s", "60s", 60 * time.Second},
		{"5m", "5m", 5 * time.Minute},
		{"24h", "24h", 24 * time.Hour},
		{"1h30m mixed", "1h30m", 90 * time.Minute},
		{"1d", "1d", 24 * time.Hour},
		{"365d", "365d", 365 * 24 * time.Hour},
		{"30d", "30d", 30 * 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d Duration
			if err := d.UnmarshalText([]byte(tc.in)); err != nil {
				t.Fatalf("UnmarshalText(%q) returned error: %v", tc.in, err)
			}
			if d.Duration() != tc.want {
				t.Errorf("got %v, want %v", d.Duration(), tc.want)
			}
		})
	}
}

func TestDuration_Compact(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"60 seconds", 60 * time.Second, "60s"},
		{"15 seconds (min poll)", 15 * time.Second, "15s"},
		{"five minutes", 5 * time.Minute, "300s"},
		{"24 hours", 24 * time.Hour, "24h"},
		{"one hour", time.Hour, "1h"},
		{"two hours", 2 * time.Hour, "2h"},
		{"365 days", 365 * 24 * time.Hour, "8760h"},
		{"zero", 0, "0s"},
		{"negative falls to zero", -time.Second, "0s"},
		{"sub-second falls back to Go form", 500 * time.Millisecond, "500ms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Duration(tc.in)
			if got := d.Compact(); got != tc.want {
				t.Errorf("Duration(%v).Compact() = %q, want %q", tc.in, got, tc.want)
			}
			if got := CompactDuration(tc.in); got != tc.want {
				t.Errorf("CompactDuration(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDuration_UnmarshalTextErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"unknown unit", "365y"},
		{"bare d", "d"},
		{"non-numeric d prefix", "manyd"},
		{"d with mixed", "1d12h"}, // not supported per SPEC's stated syntax
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d Duration
			err := d.UnmarshalText([]byte(tc.in))
			if err == nil {
				t.Fatalf("UnmarshalText(%q) = nil error; want non-nil", tc.in)
			}
			if !strings.Contains(err.Error(), tc.in) && tc.in != "" {
				t.Errorf("error %q does not include the offending input %q", err, tc.in)
			}
		})
	}
}
