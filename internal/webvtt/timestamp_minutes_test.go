package webvtt

import (
	"testing"
	"time"
)

func TestParseTimestampsWithNonzeroMinutes(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"07:30.250", 7*time.Minute + 30*time.Second + 250*time.Millisecond},
		{"59:59.999", 59*time.Minute + 59*time.Second + 999*time.Millisecond},
		{"02:07:30.250", 2*time.Hour + 7*time.Minute + 30*time.Second + 250*time.Millisecond},
	}
	for _, tc := range cases {
		got, err := parseTimestamp(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("parseTimestamp(%q) = %v, %v; want %v", tc.input, got, err, tc.want)
		}
	}
	for _, bad := range []string{"60:00.000", "00:60.000", "01:60:00.000"} {
		if got, err := parseTimestamp(bad); err == nil {
			t.Errorf("parseTimestamp(%q) = %v, want error", bad, got)
		}
	}
}
