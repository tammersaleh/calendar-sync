package mirror

import (
	"errors"
	"testing"
)

func TestSourceTupleString(t *testing.T) {
	tests := []struct {
		name string
		in   SourceTuple
		want string
	}{
		{
			"email calendar id",
			SourceTuple{CalendarID: "alice@example.com", EventID: "abc123"},
			"alice@example.com:abc123",
		},
		{
			"group calendar id",
			SourceTuple{CalendarID: "deadbeef@group.calendar.google.com", EventID: "evt9"},
			"deadbeef@group.calendar.google.com:evt9",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseSourceTuple(t *testing.T) {
	t.Run("round-trip", func(t *testing.T) {
		want := SourceTuple{CalendarID: "alice@example.com", EventID: "abc123"}
		got, err := ParseSourceTuple(want.String())
		if err != nil {
			t.Fatalf("ParseSourceTuple(%q) returned error: %v", want.String(), err)
		}
		if got != want {
			t.Fatalf("ParseSourceTuple(%q) = %#v, want %#v", want.String(), got, want)
		}
	})

	t.Run("group-calendar round-trip", func(t *testing.T) {
		want := SourceTuple{CalendarID: "x@group.calendar.google.com", EventID: "ev"}
		got, err := ParseSourceTuple(want.String())
		if err != nil {
			t.Fatalf("ParseSourceTuple returned error: %v", err)
		}
		if got != want {
			t.Fatalf("ParseSourceTuple = %#v, want %#v", got, want)
		}
	})

	t.Run("error cases", func(t *testing.T) {
		bad := []string{
			"",
			"no-colon",
			":missing-calendar-id",
			"missing-event-id:",
			":",
		}
		for _, in := range bad {
			t.Run(in, func(t *testing.T) {
				_, err := ParseSourceTuple(in)
				if err == nil {
					t.Fatalf("ParseSourceTuple(%q) = nil error; want non-nil", in)
				}
				if !errors.Is(err, ErrInvalidSourceTuple) {
					t.Fatalf("ParseSourceTuple(%q) error = %v; want errors.Is ErrInvalidSourceTuple", in, err)
				}
			})
		}
	})
}
