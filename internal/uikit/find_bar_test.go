package uikit

import (
	"cmp"
	"testing"
)

// some is Rust's `Some(ix)` for a step's answer.
func some(ix int) (int, bool) { return ix, true }

func ref(v int) *int { return &v }

func checkStep(t *testing.T, what string, gotIx int, gotOK bool, wantIx int, wantOK bool) {
	t.Helper()
	if gotIx != wantIx || gotOK != wantOK {
		t.Errorf("%s = (%d, %v), want (%d, %v)", what, gotIx, gotOK, wantIx, wantOK)
	}
}

func TestAStepForwardLandsPastWhereTheWalkStands(t *testing.T) {
	hits := []int{2, 5, 9}
	ix, ok := Step(hits, ref(2), true, cmp.Compare[int])
	wantIx, wantOK := some(1)
	checkStep(t, "step from 2", ix, ok, wantIx, wantOK)
	ix, ok = Step(hits, ref(3), true, cmp.Compare[int])
	wantIx, wantOK = some(1)
	checkStep(t, "step from 3", ix, ok, wantIx, wantOK)
	ix, ok = Step(hits, ref(0), true, cmp.Compare[int])
	wantIx, wantOK = some(0)
	checkStep(t, "step from 0", ix, ok, wantIx, wantOK)
}

func TestAStepBackLandsBeforeWhereTheWalkStands(t *testing.T) {
	hits := []int{2, 5, 9}
	ix, ok := Step(hits, ref(5), false, cmp.Compare[int])
	wantIx, wantOK := some(0)
	checkStep(t, "back from 5", ix, ok, wantIx, wantOK)
	ix, ok = Step(hits, ref(7), false, cmp.Compare[int])
	wantIx, wantOK = some(1)
	checkStep(t, "back from 7", ix, ok, wantIx, wantOK)
}

// One press past the last hit comes round to the first, and the other way
// round.
func TestTheWalkWrapsAtBothEnds(t *testing.T) {
	hits := []int{2, 5, 9}
	ix, ok := Step(hits, ref(9), true, cmp.Compare[int])
	wantIx, wantOK := some(0)
	checkStep(t, "forward from 9", ix, ok, wantIx, wantOK)
	ix, ok = Step(hits, ref(12), true, cmp.Compare[int])
	wantIx, wantOK = some(0)
	checkStep(t, "forward from 12", ix, ok, wantIx, wantOK)
	ix, ok = Step(hits, ref(2), false, cmp.Compare[int])
	wantIx, wantOK = some(2)
	checkStep(t, "back from 2", ix, ok, wantIx, wantOK)
	ix, ok = Step(hits, ref(0), false, cmp.Compare[int])
	wantIx, wantOK = some(2)
	checkStep(t, "back from 0", ix, ok, wantIx, wantOK)
}

func TestWithNothingToStandOnTheWalkStartsAtAnEnd(t *testing.T) {
	hits := []int{2, 5, 9}
	ix, ok := Step(hits, nil, true, cmp.Compare[int])
	wantIx, wantOK := some(0)
	checkStep(t, "forward from nothing", ix, ok, wantIx, wantOK)
	ix, ok = Step(hits, nil, false, cmp.Compare[int])
	wantIx, wantOK = some(2)
	checkStep(t, "back from nothing", ix, ok, wantIx, wantOK)
	ix, ok = Step([]int{}, nil, true, cmp.Compare[int])
	checkStep(t, "no hits", ix, ok, 0, false)
}

// Typing keeps a hit the user is already on, and otherwise moves on from
// where they were rather than from the top.
func TestAKeystrokeLandsOnTheFirstHitFromWhereTheUserWas(t *testing.T) {
	hits := []int{2, 5, 9}
	ix, ok := FirstFrom(hits, 5, cmp.Compare[int])
	wantIx, wantOK := some(1)
	checkStep(t, "from 5", ix, ok, wantIx, wantOK)
	ix, ok = FirstFrom(hits, 6, cmp.Compare[int])
	wantIx, wantOK = some(2)
	checkStep(t, "from 6", ix, ok, wantIx, wantOK)
	ix, ok = FirstFrom(hits, 10, cmp.Compare[int])
	wantIx, wantOK = some(0)
	checkStep(t, "from 10", ix, ok, wantIx, wantOK)
	ix, ok = FirstFrom([]int{}, 1, cmp.Compare[int])
	checkStep(t, "no hits", ix, ok, 0, false)
}

func TestTheCountSaysWhereTheWalkIs(t *testing.T) {
	count := func(typed bool, total int, current *int, capped bool) FindCount {
		return FindCount{Typed: typed, Total: total, Current: current, Capped: capped}
	}
	labels := []struct {
		got, want string
	}{
		{count(false, 0, nil, false).Label(), ""},
		{count(true, 0, nil, false).Label(), "no matches"},
		{count(true, 12, ref(2), false).Label(), "3 of 12"},
		{count(true, 12, nil, false).Label(), "12 found"},
		{count(true, 10_000, ref(0), true).Label(), "1 of 10,000+"},
	}
	for _, l := range labels {
		if l.got != l.want {
			t.Errorf("label = %q, want %q", l.got, l.want)
		}
	}
	if !count(true, 0, nil, false).Missed() {
		t.Error("a typed needle with no hits is not missed")
	}
	if count(false, 0, nil, false).Missed() {
		t.Error("an empty line is missed")
	}
}
