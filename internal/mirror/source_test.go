package mirror

import (
	"errors"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
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

func TestIsInheritedRecurringInstance(t *testing.T) {
	withSourceTuple := func(s string) *gws.Event {
		return &gws.Event{
			ExtendedProperties: &gws.ExtendedProperties{
				Private: map[string]string{ExtKeySource: s},
			},
		}
	}

	tests := []struct {
		name           string
		event          *gws.Event
		sourceParentID string
		want           bool
	}{
		{
			"inherited: source-tuple EventID equals parent ID",
			withSourceTuple("src-cal:src-parent"),
			"src-parent",
			true,
		},
		{
			"managed: source-tuple EventID is the instance ID with UTC suffix",
			withSourceTuple("src-cal:src-parent_20260511T160000Z"),
			"src-parent",
			false,
		},
		{
			"managed: source-tuple EventID is unrelated",
			withSourceTuple("src-cal:src-evt"),
			"src-parent",
			false,
		},
		{
			"nil event",
			nil,
			"src-parent",
			false,
		},
		{
			"empty parent ID",
			withSourceTuple("src-cal:src-parent"),
			"",
			false,
		},
		{
			"missing source extended property",
			&gws.Event{ExtendedProperties: &gws.ExtendedProperties{Private: map[string]string{}}},
			"src-parent",
			false,
		},
		{
			"nil ExtendedProperties",
			&gws.Event{},
			"src-parent",
			false,
		},
		{
			"malformed source value (no colon)",
			withSourceTuple("not-a-tuple"),
			"src-parent",
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInheritedRecurringInstance(tc.event, tc.sourceParentID); got != tc.want {
				t.Errorf("IsInheritedRecurringInstance() = %v, want %v", got, tc.want)
			}
		})
	}
}
