package mirror

import "testing"

func TestStripTrailer(t *testing.T) {
	const wwwTrailer = "\n\n---\nSource: https://www.google.com/calendar/event?eid=AbCdEf123_-="
	const calendarTrailer = "\n\n---\nSource: https://calendar.google.com/calendar/event?eid=XyZ987"

	tests := []struct {
		name        string
		input       string
		wantOut     string
		wantStripped bool
	}{
		{
			name:        "standard www.google.com trailer with description body",
			input:       "Lunch with Bob" + wwwTrailer,
			wantOut:     "Lunch with Bob",
			wantStripped: true,
		},
		{
			name:        "alternate calendar.google.com trailer",
			input:       "Body" + calendarTrailer,
			wantOut:     "Body",
			wantStripped: true,
		},
		{
			name:        "trailing whitespace tolerated",
			input:       "Body" + wwwTrailer + "   \n",
			wantOut:     "Body",
			wantStripped: true,
		},
		{
			name:        "empty body, trailer only",
			input:       wwwTrailer,
			wantOut:     "",
			wantStripped: true,
		},
		{
			name:        "no trailer at all",
			input:       "Just a plain description.",
			wantOut:     "Just a plain description.",
			wantStripped: false,
		},
		{
			name:        "empty description",
			input:       "",
			wantOut:     "",
			wantStripped: false,
		},
		{
			name:        "trailer with characters after eid",
			input:       "Body" + wwwTrailer + " trailing words",
			wantOut:     "Body" + wwwTrailer + " trailing words",
			wantStripped: false,
		},
		{
			name:        "wrong domain not stripped",
			input:       "Body\n\n---\nSource: https://docs.google.com/calendar/event?eid=AbCd",
			wantOut:     "Body\n\n---\nSource: https://docs.google.com/calendar/event?eid=AbCd",
			wantStripped: false,
		},
		{
			name:        "missing horizontal rule before Source line",
			input:       "Body\n\nSource: https://www.google.com/calendar/event?eid=AbCd",
			wantOut:     "Body\n\nSource: https://www.google.com/calendar/event?eid=AbCd",
			wantStripped: false,
		},
		{
			name:        "trailer-shaped fragment in middle is left alone",
			input:       "intro" + wwwTrailer + "\n\nmore body",
			wantOut:     "intro" + wwwTrailer + "\n\nmore body",
			wantStripped: false,
		},
		{
			name:        "missing eid value",
			input:       "Body\n\n---\nSource: https://www.google.com/calendar/event?eid=",
			wantOut:     "Body\n\n---\nSource: https://www.google.com/calendar/event?eid=",
			wantStripped: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOut, gotStripped := StripTrailer(tc.input)
			if gotStripped != tc.wantStripped {
				t.Errorf("stripped flag = %v, want %v", gotStripped, tc.wantStripped)
			}
			if gotOut != tc.wantOut {
				t.Errorf("output = %q, want %q", gotOut, tc.wantOut)
			}
		})
	}
}
