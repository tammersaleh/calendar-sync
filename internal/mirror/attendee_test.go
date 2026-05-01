package mirror

import (
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

func TestSourceOwnerResponseStatus(t *testing.T) {
	tests := []struct {
		name string
		in   *gws.Event
		want string
	}{
		{
			name: "no attendees -> empty",
			in:   &gws.Event{},
			want: "",
		},
		{
			name: "self attendee accepted",
			in:   &gws.Event{Attendees: []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusAccepted}}},
			want: gws.ResponseStatusAccepted,
		},
		{
			name: "self attendee declined",
			in:   &gws.Event{Attendees: []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusDeclined}}},
			want: gws.ResponseStatusDeclined,
		},
		{
			name: "self attendee tentative",
			in:   &gws.Event{Attendees: []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusTentative}}},
			want: gws.ResponseStatusTentative,
		},
		{
			name: "no self attendee",
			in:   &gws.Event{Attendees: []gws.Attendee{{Email: "other@example.com", ResponseStatus: gws.ResponseStatusAccepted}}},
			want: "",
		},
		{
			name: "self attendee with empty status",
			in:   &gws.Event{Attendees: []gws.Attendee{{Self: true}}},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SourceOwnerResponseStatus(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
