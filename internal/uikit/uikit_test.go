package uikit

import "testing"

func TestCountsReadCompactly(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{640, "640"},
		{18_412, "18.4k"},
		{311_000, "311k"},
		// A round thousand reads better without the trailing zero.
		{5_000, "5k"},
		{20_000, "20k"},
		{1_200_000, "1.2m"},
		{2_400_000_000, "2.4b"},
	}
	for _, tc := range cases {
		if got := FormatCount(tc.in); got != tc.want {
			t.Errorf("FormatCount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTimingsSwitchToSeconds(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{34, "34 ms"},
		{999, "999 ms"},
		{1_900, "1.9 s"},
	}
	for _, tc := range cases {
		if got := FormatMillis(tc.in); got != tc.want {
			t.Errorf("FormatMillis(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestARunningClockNeverChangesUnits(t *testing.T) {
	// What `FormatMillis` would call "34 ms", the timer calls "0.0 s":
	// the unit has to hold still while the number climbs.
	cases := []struct {
		in   uint64
		want string
	}{
		{34, "0.0 s"},
		{900, "0.9 s"},
		{1_900, "1.9 s"},
		{64_000, "64.0 s"},
	}
	for _, tc := range cases {
		if got := FormatSeconds(tc.in); got != tc.want {
			t.Errorf("FormatSeconds(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
