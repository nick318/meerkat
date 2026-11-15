package uikit

import "testing"

func TestTheThumbShrinksAsTheContentGrows(t *testing.T) {
	// Twice the viewport: half the track, at the top.
	start, length := thumbSpan(0., 100., 100., 100.)
	if length != 50. {
		t.Errorf("length = %v, want 50", length)
	}
	if start != 0. {
		t.Errorf("start = %v, want 0", start)
	}

	// Scrolled to the end: the thumb sits against the far edge.
	start, _ = thumbSpan(100., 100., 100., 100.)
	if start != 50. {
		t.Errorf("start at the end = %v, want 50", start)
	}

	// Halfway.
	start, _ = thumbSpan(50., 100., 100., 100.)
	if start != 25. {
		t.Errorf("start halfway = %v, want 25", start)
	}
}

func TestALongResultStillHasAGrabbableThumb(t *testing.T) {
	// 500 rows of 28px in a 300px viewport.
	_, length := thumbSpan(0., 13_700., 300., 300.)
	if length != minThumb {
		t.Errorf("length = %v, want %v", length, minThumb)
	}
}

func TestDraggingTheThumbMapsBackToTheOffset(t *testing.T) {
	// Track 100, thumb 50: 50px of travel covers 100px of content.
	cases := []struct{ start, want float32 }{
		{0., 0.},
		{25., 50.},
		{50., 100.},
		// Past either end clamps rather than running off.
		{-40., 0.},
		{999., 100.},
	}
	for _, tc := range cases {
		if got := offsetForThumb(tc.start, 100., 100., 50.); got != tc.want {
			t.Errorf("offsetForThumb(%v) = %v, want %v", tc.start, got, tc.want)
		}
	}
}

func TestAThumbThatFillsTheTrackCannotMove(t *testing.T) {
	if got := offsetForThumb(10., 100., 50., 50.); got != 0. {
		t.Errorf("offsetForThumb = %v, want 0", got)
	}
}
