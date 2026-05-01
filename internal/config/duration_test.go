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
