package resultsgrid

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nick318/meerkat/internal/dbclient"
)

func text(value string) dbclient.Value {
	return dbclient.ValueText(value)
}

// wide is room enough for anything these tests paint, so the line rule is
// argued with apart from the cut.
const wide = 1_000

func assertEq[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func assertSome[T comparable](t *testing.T, got T, ok bool, want T) {
	t.Helper()
	if !ok || got != want {
		t.Fatalf("got %v, %v; want Some(%v)", got, ok, want)
	}
}

func TestACellPaintsTheFirstLineOfAValue(t *testing.T) {
	assertEq(t, CellText(text("one\ntwo\nthree"), wide), "one…")
	assertEq(t, CellText(text("one\r\ntwo"), wide), "one…")
}

func TestAValueShortEnoughToFitIsPaintedWhole(t *testing.T) {
	assertEq(t, CellText(text("ada@example.com"), wide), "ada@example.com")
	assertEq(t, CellText(dbclient.ValueNull{}, wide), "NULL")
	// A trailing newline drops nothing anybody can read.
	assertEq(t, CellText(text("one\n"), wide), "one")
}

func TestALongLineIsCutToWhatALaneCanHold(t *testing.T) {
	chars := laneChars(maxColumnWidth)
	cut := CellText(text(strings.Repeat("x", chars*4)), chars)
	assertEq(t, utf8.RuneCountInString(cut), chars+1)
	if !strings.HasSuffix(cut, "…") {
		t.Fatalf("%q does not end in …", cut)
	}
}

// The cut must never land inside the lane: a value that reaches the
// edge has to go on past it, so the text system truncates it there.
func TestALaneIsCutPastItsOwnEdge(t *testing.T) {
	for _, width := range []float32{minColumnWidth, 100., maxColumnWidth, 1_400.} {
		shown := float32(float32(laneChars(width)) * charAdvance)
		if !(shown > width-cellPadding) {
			t.Fatalf("%vpx lane", width)
		}
	}
}

// The case this was written for. `EXPLAIN ANALYZE` answers with one
// column, a plan line per row, and the one lane of a one-column result
// is the last lane — it spans the pane, whatever it measured.
func TestTheLastLaneIsCutToTheWindowNotToItsMeasure(t *testing.T) {
	line := fmt.Sprintf(
		"  ->  Seq Scan on orders  (cost=0.00..1834.00 rows=100000 width=8)%s",
		" (actual time=0.011..9.874 rows=100000 loops=1)",
	)
	data := NewGridData([]string{"QUERY PLAN"}, [][]dbclient.Value{{text(line)}})
	assertEq(t, data.widths[0], maxColumnWidth)
	chars := cellChars(data.widths[0], true, 1_400.)
	assertEq(t, CellText(text(line), chars), line)
	// Any other lane is laid out at its measure, so it is cut there.
	chars = cellChars(data.widths[0], false, 1_400.)
	assertEq(t, chars, laneChars(maxColumnWidth))
	// A window narrower than the lane cannot shrink the cut below it.
	assertEq(t, cellChars(maxColumnWidth, true, 0.), chars)
}

func TestLanesFitTheWidestValue(t *testing.T) {
	data := NewGridData(
		[]string{"id", "email"},
		[][]dbclient.Value{
			{dbclient.ValueInt(1), text("ada@example.com")},
			{dbclient.ValueInt(1041), text("a@b.co")},
		},
	)
	// "1041" is 4 characters, but the floor keeps the lane usable.
	assertEq(t, data.widths[0], minColumnWidth)
	// 15 characters plus the padding. Go port: the arithmetic is held to
	// f32 a step at a time, as Rust's const evaluation is.
	fifteen := float32(15.)
	assertEq(t, data.widths[1], float32(fifteen*charWidth)+cellPadding)
	// A lane must hold its own text: 15 characters at the data size.
	if !(data.widths[1] >= float32(float32(fifteen*dataFontSize)*0.6)+cellPadding) {
		t.Fatalf("width %v", data.widths[1])
	}
}

func TestAHeaderWiderThanItsValuesStillFits(t *testing.T) {
	data := NewGridData(
		[]string{"reactivated_at_utc"},
		[][]dbclient.Value{{text("x")}},
	)
	eighteen := float32(18.)
	assertEq(t, data.widths[0], float32(eighteen*charWidth)+cellPadding)
}

func TestOneHugeValueCannotStealThePane(t *testing.T) {
	data := NewGridData(
		[]string{"blob"},
		[][]dbclient.Value{{text(strings.Repeat("x", 4_000))}},
	)
	assertEq(t, data.widths[0], maxColumnWidth)
}

func TestAnEmptyResultHasNoLanes(t *testing.T) {
	if len(EmptyGridData().widths) != 0 {
		t.Fatal("widths not empty")
	}
}

// A lane already on screen must not move the pane: the cursor walks a
// row a lane at a time, and a pane that re-centred on every step would
// slide about under the reader.
func TestALaneAlreadyInViewScrollsNothing(t *testing.T) {
	assertEq(t, revealOffset(200., 100., 150., 400.), 150.)
	// Flush against each edge still counts as whole on screen.
	assertEq(t, revealOffset(150., 100., 150., 400.), 150.)
	assertEq(t, revealOffset(450., 100., 150., 400.), 150.)
}

func TestALaneOffTheEdgeComesTheShortestWayBack(t *testing.T) {
	// Off to the left: its left edge to the pane's left.
	assertEq(t, revealOffset(100., 80., 150., 400.), 100.)
	// Off to the right: its right edge to the pane's right, which is
	// one lane's worth of movement, not a jump to the left edge.
	assertEq(t, revealOffset(500., 100., 150., 400.), 200.)
}

// A lane wider than the pane cannot be whole on screen. Its left edge
// wins, because that is the end the value is read from.
func TestALaneWiderThanThePaneShowsItsStart(t *testing.T) {
	assertEq(t, revealOffset(600., 500., 0., 400.), 600.)
}

// A drag reads the cell out of the geometry rather than out of the
// elements, because past the pane's edge there is no element to ask.
func TestADragFindsTheRowUnderThePointer(t *testing.T) {
	// A pane whose top edge is 100px down the window, unscrolled.
	row, ok := rowAt(100., 100., 0.)
	assertSome(t, row, ok, 0)
	row, ok = rowAt(100.+RowHeight-0.1, 100., 0.)
	assertSome(t, row, ok, 0)
	row, ok = rowAt(100.+RowHeight, 100., 0.)
	assertSome(t, row, ok, 1)
	// Ten rows scrolled away: the offset runs negative.
	row, ok = rowAt(100., 100., -10.*RowHeight)
	assertSome(t, row, ok, 10)
	// Above the pane there is nothing but the first row to reach for.
	row, ok = rowAt(20., 100., 0.)
	assertSome(t, row, ok, 0)
}

func TestADragFindsTheLaneUnderThePointer(t *testing.T) {
	widths := []float32{56., 100.}
	// The gutter scrolls with the content, so it is part of the sums —
	// and a pointer over it is over the first lane.
	column, ok := columnAt(10., 0., 0., widths)
	assertSome(t, column, ok, 0)
	column, ok = columnAt(gutterWidth, 0., 0., widths)
	assertSome(t, column, ok, 0)
	column, ok = columnAt(gutterWidth+55.9, 0., 0., widths)
	assertSome(t, column, ok, 0)
	column, ok = columnAt(gutterWidth+56., 0., 0., widths)
	assertSome(t, column, ok, 1)
	// The last lane takes the slack, so past its measured width is
	// still the last lane.
	column, ok = columnAt(5_000., 0., 0., widths)
	assertSome(t, column, ok, 1)
	// Scrolled one lane across.
	column, ok = columnAt(gutterWidth, 0., -56., widths)
	assertSome(t, column, ok, 1)
	if _, ok := columnAt(10., 0., 0., []float32{}); ok {
		t.Fatal("want None")
	}
}

// Inside the pane a drag scrolls nothing: a drag that crept while the
// pointer sat in the middle of the result could never be made to stop.
func TestADragScrollsOnlyOnceItIsPastTheEdge(t *testing.T) {
	assertEq(t, autoscrollStep(300., 100., 500.), 0.)
	assertEq(t, autoscrollStep(100., 100., 500.), 0.)
	assertEq(t, autoscrollStep(500., 100., 500.), 0.)
}

func TestADragPastTheEdgeScrollsByHowFarPastItIs(t *testing.T) {
	// A hair over the line still moves, and moves readably.
	assertEq(t,
		autoscrollStep(501., 100., 500.),
		autoscrollMin,
	)
	assertEq(t,
		autoscrollStep(99., 100., 500.),
		-autoscrollMin,
	)
	// In between, the overshoot is the speed.
	assertEq(t, autoscrollStep(520., 100., 500.), 20.)
	assertEq(t, autoscrollStep(80., 100., 500.), -20.)
	// Flung to the far side of the screen, it is still bounded.
	assertEq(t,
		autoscrollStep(2_000., 100., 500.),
		autoscrollMax,
	)
	assertEq(t,
		autoscrollStep(-900., 100., 500.),
		-autoscrollMax,
	)
}

// The gutter scrolls with the content, so it counts in every lane's
// position — the first lane does not start at zero.
func TestLanePositionsCountTheGutter(t *testing.T) {
	data := NewGridData(
		[]string{"id", "email"},
		[][]dbclient.Value{{dbclient.ValueInt(1), text("ada@example.com")}},
	)
	left, width, ok := data.LaneSpan(0)
	if !ok || left != gutterWidth || width != data.widths[0] {
		t.Fatalf("lane 0 = %v, %v, %v", left, width, ok)
	}
	left, width, ok = data.LaneSpan(1)
	if !ok || left != gutterWidth+data.widths[0] || width != data.widths[1] {
		t.Fatalf("lane 1 = %v, %v, %v", left, width, ok)
	}
	if _, _, ok := data.LaneSpan(2); ok {
		t.Fatal("lane 2 want None")
	}
}
