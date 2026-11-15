package resultsgrid

import (
	"slices"
	"testing"

	"github.com/nick318/meerkat/internal/dbclient"
)

// selectionData is the Rust test module's `data()`. Go port: renamed, as
// find_test.go has a `data()` of its own in the same package.
func selectionData() *GridData {
	return NewGridData(
		[]string{"id", "email", "plan"},
		[][]dbclient.Value{
			{
				dbclient.ValueInt(1041),
				dbclient.ValueText("ida@northwind.io"),
				dbclient.ValueText("scale"),
			},
			{
				dbclient.ValueInt(1040),
				dbclient.ValueText("m.okafor@lumen.dev"),
				dbclient.ValueNull{},
			},
			{
				dbclient.ValueInt(1039),
				dbclient.ValueText("Reviewed report, prepared comments"),
				dbclient.ValueText("pro"),
			},
		},
	)
}

func extent() Extent {
	return Extent{
		Rows:    3,
		Columns: 3,
	}
}

func assertRect(t *testing.T, selection *Selection, want Rect) {
	t.Helper()
	got, ok := selection.Rect()
	if !ok || got != want {
		t.Fatalf("rect = %+v, %v; want %+v", got, ok, want)
	}
}

func assertStep(t *testing.T, got Cell, ok bool, want Cell, wantOK bool) {
	t.Helper()
	if ok != wantOK || (ok && got != want) {
		t.Fatalf("step = %+v, %v; want %+v, %v", got, ok, want, wantOK)
	}
}

func clip(t *testing.T, data *GridData, selection *Selection) string {
	t.Helper()
	text, ok := ClipboardText(data, selection)
	if !ok {
		t.Fatalf("clipboard_text answered None")
	}
	return text
}

func TestARangeGrowsFromTheAnchorBothWays(t *testing.T) {
	var selection Selection
	selection.Focus(NewCell(1, 1))
	selection.ExtendTo(NewCell(0, 2))
	assertRect(t, &selection, Rect{
		Top:    0,
		Bottom: 1,
		Left:   1,
		Right:  2,
	})
	// Dragging back past the anchor turns the rectangle over rather
	// than emptying it.
	selection.ExtendTo(NewCell(2, 0))
	assertRect(t, &selection, Rect{
		Top:    1,
		Bottom: 2,
		Left:   0,
		Right:  1,
	})
	if !selection.Contains(2, 0) {
		t.Fatal("contains(2, 0)")
	}
	if selection.Contains(0, 0) {
		t.Fatal("!contains(0, 0)")
	}
	if !selection.IsCursor(2, 0) {
		t.Fatal("is_cursor(2, 0)")
	}
}

// ⇧ with nothing selected has no anchor to grow from, so it lands as
// a plain click instead of doing nothing.
func TestExtendingFromNothingSelectsTheOneCell(t *testing.T) {
	var selection Selection
	selection.ExtendTo(NewCell(1, 2))
	assertRect(t, &selection, Rect{
		Top:    1,
		Bottom: 1,
		Left:   2,
		Right:  2,
	})
}

func TestTheCursorStopsAtTheEdges(t *testing.T) {
	var selection Selection
	selection.Focus(NewCell(0, 0))
	got, ok := selection.Step(StepUp, false, extent())
	assertStep(t, got, ok, Cell{}, false)
	got, ok = selection.Step(StepLeft, false, extent())
	assertStep(t, got, ok, Cell{}, false)
	got, ok = selection.Step(StepLast, false, extent())
	assertStep(t, got, ok, NewCell(2, 0), true)
	got, ok = selection.Step(StepDown, false, extent())
	assertStep(t, got, ok, Cell{}, false)
	got, ok = selection.Step(StepRowEnd, false, extent())
	assertStep(t, got, ok, NewCell(2, 2), true)
	got, ok = selection.Step(StepRight, false, extent())
	assertStep(t, got, ok, Cell{}, false)
}

// Walking without ⇧ moves the range whole; walking with it leaves the
// anchor where it was.
func TestShiftKeepsTheAnchorAndAPlainStepDoesNot(t *testing.T) {
	var selection Selection
	selection.Focus(NewCell(0, 0))
	selection.Step(StepDown, true, extent())
	selection.Step(StepRight, true, extent())
	assertRect(t, &selection, Rect{
		Top:    0,
		Bottom: 1,
		Left:   0,
		Right:  1,
	})

	selection.Step(StepDown, false, extent())
	assertRect(t, &selection, Rect{
		Top:    2,
		Bottom: 2,
		Left:   1,
		Right:  1,
	})
}

// The first keystroke on a result nobody has clicked lands on the
// first cell.
func TestTheFirstKeystrokeNeedsNoClickBeforeIt(t *testing.T) {
	var selection Selection
	got, ok := selection.Step(StepDown, false, extent())
	assertStep(t, got, ok, NewCell(0, 0), true)
	if !selection.IsCursor(0, 0) {
		t.Fatal("is_cursor(0, 0)")
	}
}

func TestAnEmptyResultTakesNoSelection(t *testing.T) {
	empty := Extent{
		Rows:    0,
		Columns: 0,
	}
	var selection Selection
	got, ok := selection.Step(StepDown, false, empty)
	assertStep(t, got, ok, Cell{}, false)
	selection.SelectAll(empty)
	selection.SelectColumn(0, empty)
	if !selection.IsEmpty() {
		t.Fatal("is_empty")
	}
}

func TestAColumnAndTheWholeResultAreRangesToo(t *testing.T) {
	var selection Selection
	selection.SelectColumn(1, extent())
	assertRect(t, &selection, Rect{
		Top:    0,
		Bottom: 2,
		Left:   1,
		Right:  1,
	})
	selection.SelectAll(extent())
	assertRect(t, &selection, Rect{
		Top:    0,
		Bottom: 2,
		Left:   0,
		Right:  2,
	})
}

func TestTickingARowTwiceUnticksIt(t *testing.T) {
	var selection Selection
	selection.TogglePick(1)
	if !selection.IsPicked(1) {
		t.Fatal("is_picked(1)")
	}
	selection.TogglePick(1)
	if selection.IsPicked(1) {
		t.Fatal("!is_picked(1)")
	}
	if selection.Picks() != 0 {
		t.Fatalf("picks = %d", selection.Picks())
	}
}

func TestShiftClickingTheGutterFillsTheGap(t *testing.T) {
	var selection Selection
	selection.TogglePick(0)
	selection.PickThrough(2)
	if got := slices.Collect(selection.PickedRows()); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("picked = %v", got)
	}
	if !selection.AllPicked(extent()) {
		t.Fatal("all_picked")
	}

	// With no lead, ⇧ ticks the one row it landed on.
	var fresh Selection
	fresh.PickThrough(2)
	if got := slices.Collect(fresh.PickedRows()); !slices.Equal(got, []int{2}) {
		t.Fatalf("picked = %v", got)
	}
}

// One target does both, so a fully ticked result clears on the same
// click that ticked it.
func TestTheHeaderTickTakesAllOfThemAndGivesThemBack(t *testing.T) {
	var selection Selection
	selection.ToggleAllPicks(extent())
	if !selection.AllPicked(extent()) {
		t.Fatal("all_picked")
	}
	selection.ToggleAllPicks(extent())
	if selection.Picks() != 0 {
		t.Fatalf("picks = %d", selection.Picks())
	}
}

func TestCopyingARangeTakesTheCellsItCovers(t *testing.T) {
	data := selectionData()
	var selection Selection
	selection.Focus(NewCell(0, 0))
	selection.ExtendTo(NewCell(1, 1))
	if got, want := clip(t, data, &selection), "1041,ida@northwind.io\n1040,m.okafor@lumen.dev"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Ticked rows are copied whole, whatever rectangle is also drawn: the
// tick says "this record".
func TestCopyingTickedRowsTakesThemWhole(t *testing.T) {
	data := selectionData()
	var selection Selection
	selection.Focus(NewCell(0, 0))
	selection.TogglePick(1)
	if got, want := clip(t, data, &selection),
		// The NULL is an absence, so the field is empty.
		"1040,m.okafor@lumen.dev,"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A value holding the separator would read as two fields, so it is
// quoted — and a quote inside it is doubled, as RFC 4180 has it.
func TestAValueHoldingACommaIsQuoted(t *testing.T) {
	data := selectionData()
	var selection Selection
	selection.Focus(NewCell(2, 1))
	if got, want := clip(t, data, &selection), "\"Reviewed report, prepared comments\""; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	quoted := NewGridData(
		[]string{"note"},
		[][]dbclient.Value{{dbclient.ValueText("she said \"yes\"")}},
	)
	var one Selection
	one.Focus(NewCell(0, 0))
	if got, want := clip(t, quoted, &one), "\"she said \"\"yes\"\"\""; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// A line break inside a value would paste as two rows.
	wrapped := NewGridData(
		[]string{"note"},
		[][]dbclient.Value{{dbclient.ValueText("first\nsecond")}},
	)
	if got, want := clip(t, wrapped, &one), "\"first\nsecond\""; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNothingSelectedCopiesNothing(t *testing.T) {
	var selection Selection
	if got, ok := ClipboardText(selectionData(), &selection); ok {
		t.Fatalf("got %q, want None", got)
	}
}

func TestTheStatusStripSaysNothingAboutOneCell(t *testing.T) {
	var selection Selection
	selection.Focus(NewCell(0, 0))
	if got, ok := selection.Summary(); ok {
		t.Fatalf("got %q, want None", got)
	}

	selection.ExtendTo(NewCell(1, 2))
	if got, ok := selection.Summary(); !ok || got != "2 × 3 cells" {
		t.Fatalf("got %q, %v", got, ok)
	}

	selection.TogglePick(0)
	if got, ok := selection.Summary(); !ok || got != "1 row picked · 2 × 3 cells" {
		t.Fatalf("got %q, %v", got, ok)
	}
}
