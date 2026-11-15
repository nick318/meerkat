package uikit

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const testURL = "postgres://ada@db.internal:5432/app"

func TestWordMotionWalksAUrlPartByPart(t *testing.T) {
	if got := testURL[:motion.nextWordEnd(testURL, 0)]; got != "postgres" {
		t.Errorf("first word = %q", got)
	}
	afterScheme := motion.nextWordEnd(testURL, 0)
	if got := testURL[:motion.nextWordEnd(testURL, afterScheme)]; got != "postgres://ada" {
		t.Errorf("second word = %q", got)
	}
	// Backwards from the end: the database name, then the port.
	back := motion.previousWordStart(testURL, len(testURL))
	if got := testURL[back:]; got != "app" {
		t.Errorf("last word = %q", got)
	}
	if got := testURL[motion.previousWordStart(testURL, back):]; got != "5432/app" {
		t.Errorf("word before the last = %q", got)
	}
}

func TestADoubleClickTakesTheWholeWord(t *testing.T) {
	ix := strings.Index(testURL, "internal") + 2
	word := motion.wordAt(testURL, ix)
	if got := testURL[word.start:word.end]; got != "internal" {
		t.Errorf("word = %q", got)
	}
}

func TestMotionNeverSplitsACharacter(t *testing.T) {
	text := "héllo wörld"
	isCharBoundary := func(ix int) bool { return ix == len(text) || utf8.RuneStart(text[ix]) }
	ix := 0
	for ix < len(text) {
		ix = motion.nextWordEnd(text, ix)
		if !isCharBoundary(ix) {
			t.Fatalf("split at %d", ix)
		}
	}
	for ix > 0 {
		ix = motion.previousWordStart(text, ix)
		if !isCharBoundary(ix) {
			t.Fatalf("split at %d", ix)
		}
	}
}

func TestAStaleRangeIsPulledBackIntoTheValue(t *testing.T) {
	text := "postgres://localhost"
	cases := []struct{ in, want textRange }{
		{textRange{40, 50}, textRange{20, 20}},
		{textRange{2, 50}, textRange{2, 20}},
		{textRange{6, 2}, textRange{6, 6}},
	}
	for _, tc := range cases {
		if got := clampRange(text, tc.in); got != tc.want {
			t.Errorf("clampRange(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestClampingNeverSplitsACharacter(t *testing.T) {
	text := "héllo"
	if got := clampOffset(text, 2); got != 1 {
		t.Errorf("clampOffset(2) = %d, want 1", got)
	}
	for offset := 0; offset <= len(text)+4; offset++ {
		got := clampOffset(text, offset)
		if !(got == len(text) || utf8.RuneStart(text[got])) {
			t.Errorf("clampOffset(%d) = %d splits a character", offset, got)
		}
	}
}

func TestCompositionSelectionIsMeasuredInsideTheInsertedText(t *testing.T) {
	if got := utf16ToByte("héllo", 2); got != 3 {
		t.Errorf("utf16ToByte(héllo, 2) = %d, want 3", got)
	}
	if got := utf16ToByte("abc", 99); got != 3 {
		t.Errorf("utf16ToByte(abc, 99) = %d, want 3", got)
	}
}
