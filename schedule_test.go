package main

import (
	"strings"
	"testing"
)

func TestCronToSchtasks(t *testing.T) {
	cases := map[string]string{
		"0 9 * * 1-5":    "/SC WEEKLY /D MON,TUE,WED,THU,FRI /ST 09:00",
		"30 7 * * 1":     "/SC WEEKLY /D MON /ST 07:30",
		"30 8 * * 7":     "/SC WEEKLY /D SUN /ST 08:30",
		"15 22 * * *":    "/SC DAILY /ST 22:15",
		"0 8 1 * *":      "/SC MONTHLY /D 1 /ST 08:00",
		"*/30 * * * *":   "/SC MINUTE /MO 30",
		"0 9,18 * * *":   "", // lists of hours: unsupported
		"0 9 1 * 1":      "", // dom+dow combined: unsupported
		"0 9 * 6 *":      "", // month restriction: unsupported
		"bad":            "",
		"*/0 * * * *":    "",
	}
	for expr, want := range cases {
		got, err := cronToSchtasks(expr)
		if want == "" {
			if err == nil {
				t.Errorf("%q: expected error, got %v", expr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", expr, err)
			continue
		}
		if joined := strings.Join(got, " "); joined != want {
			t.Errorf("%q: got %q, want %q", expr, joined, want)
		}
	}
}
