package fuzzy

import (
	"slices"
	"strings"
	"testing"
)

func found(haystack, needle string) []Range {
	return NewNeedle(needle).FindAll(haystack)
}

func TestEveryPlaceTheTextSitsIsFound(t *testing.T) {
	eqRanges(t,
		found("select id from users where id = 1", "id"),
		[]Range{r(7, 9), r(27, 29)},
	)
}

// A substring, not a word start: the find line is over text, and the
// user looking for `ser` in `users` means it.
func TestAHitMaySitInsideAWord(t *testing.T) {
	eqRanges(t, found("users", "ser"), []Range{r(1, 4)})
}

func TestHitsNeverOverlap(t *testing.T) {
	eqRanges(t, found("aaaa", "aa"), []Range{r(0, 2), r(2, 4)})
}

func TestCaseIsIgnoredUntilTheNeedleMeansIt(t *testing.T) {
	eqRanges(t, found("SELECT 1", "select"), []Range{r(0, 6)})
	eqRanges(t, found("select 1", "SELECT"), []Range{r(0, 6)})
	eqRanges(t, found("userId userid", "userId"), []Range{r(0, 6)})
}

func TestNothingTypedFindsNothing(t *testing.T) {
	if len(found("select", "")) != 0 {
		t.Fatal("an empty needle found something")
	}
	if NewNeedle("").IsIn("select") {
		t.Fatal("an empty needle is in select")
	}
}

// The ranges slice the haystack, so they must fall on character
// boundaries however wide the characters before them are.
func TestARangeIsMeasuredInTheHaystackSOwnBytes(t *testing.T) {
	haystack := "Łódź — łódź"
	hits := found(haystack, "łódź")
	if len(hits) != 2 {
		t.Fatalf("hits = %v, want 2", hits)
	}
	for _, hit := range hits {
		if got := strings.ToLower(haystack[hit.Start:hit.End]); got != "łódź" {
			t.Fatalf("%v = %q", hit, got)
		}
	}
}

func TestACappedSearchSaysThereWasMore(t *testing.T) {
	needle := NewNeedle("a")
	hits, more := needle.FindCapped("aaa", 2)
	if !slices.Equal(hits, []Range{r(0, 1), r(1, 2)}) || !more {
		t.Fatalf("aaa: %v, %v", hits, more)
	}
	hits, more = needle.FindCapped("aa", 2)
	if !slices.Equal(hits, []Range{r(0, 1), r(1, 2)}) || more {
		t.Fatalf("aa: %v, %v", hits, more)
	}
}

func TestANeedleLongerThanWhatIsLeftIsNotFound(t *testing.T) {
	if len(found("sel", "select")) != 0 {
		t.Fatal("select in sel")
	}
	if !NewNeedle("lec").IsIn("select") {
		t.Fatal("lec in select")
	}
}
