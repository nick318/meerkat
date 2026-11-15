package sqleditor

import (
	"testing"
	"unsafe"
)

// borrowed is Rust's `matches!(text, Cow::Borrowed(_))`: the very string
// that went in came back, no copy made.
func borrowed(got, in string) bool {
	return len(got) == len(in) && unsafe.StringData(got) == unsafe.StringData(in)
}

func TestATabRunsToTheNextStop(t *testing.T) {
	text, m := NewExpanded("a\tb\t\tc")
	if text != "a   b       c" {
		t.Errorf("text = %q", text)
	}
	// `b` is byte 2 of the line and column 4 of the screen.
	if got := m.ToDisplay(2); got != 4 {
		t.Errorf("ToDisplay(2) = %d", got)
	}
	if got := m.ToDisplay(5); got != 12 {
		t.Errorf("ToDisplay(5) = %d", got)
	}
	for ix := 0; ix <= 6; ix++ {
		if got := m.ToBuffer(m.ToDisplay(ix)); got != ix {
			t.Errorf("round trip of %d = %d", ix, got)
		}
	}
}

func TestAPointInsideATabSnapsToItsNearerSide(t *testing.T) {
	_, m := NewExpanded("\tx")
	for _, tc := range []struct{ in, want int }{{1, 0}, {3, 1}, {4, 1}, {5, 2}} {
		if got := m.ToBuffer(tc.in); got != tc.want {
			t.Errorf("ToBuffer(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestALineWithoutATabMapsOntoItself(t *testing.T) {
	line := "select 1"
	text, m := NewExpanded(line)
	if !borrowed(text, line) {
		t.Errorf("a line without a tab was copied")
	}
	if got := m.ToDisplay(5); got != 5 {
		t.Errorf("ToDisplay(5) = %d", got)
	}
	if got := m.ToBuffer(5); got != 5 {
		t.Errorf("ToBuffer(5) = %d", got)
	}
}

func TestAColumnSurvivesALineWithTabs(t *testing.T) {
	if got := columnOf("\tid", 1); got != 4 {
		t.Errorf("columnOf = %d", got)
	}
	for _, tc := range []struct {
		line           string
		column, expect int
	}{
		{"\tid", 4, 1},
		{"\tid", 5, 2},
		{"\tid", 1, 0},
		{"ab", 9, 2},
		{"héllo", 2, 3},
	} {
		if got := indexAtColumn(tc.line, tc.column); got != tc.expect {
			t.Errorf("indexAtColumn(%q, %d) = %d, want %d", tc.line, tc.column, got, tc.expect)
		}
	}
}

func TestOnlyTheRowsOnScreenAreAskedFor(t *testing.T) {
	// 21px rows, 12px of padding, a 210px pane scrolled 2100px down.
	if got := visibleRows(2100., 210., 12., 21., 5000); got != (Range{95, 114}) {
		t.Errorf("got %v", got)
	}
	// The top of the buffer and the end of it both clamp.
	if got := visibleRows(0., 210., 12., 21., 3); got != (Range{0, 3}) {
		t.Errorf("got %v", got)
	}
	if got := visibleRows(99999., 210., 12., 21., 10); got != (Range{10, 10}) {
		t.Errorf("got %v", got)
	}
	// Before the first paint there is no height to read.
	if got := visibleRows(0., 0., 12., 21., 5000); got != (Range{0, 120}) {
		t.Errorf("got %v", got)
	}
}

func TestEveryLineBreakBecomesANewline(t *testing.T) {
	if got := normalize("a\r\nb\rc\n"); got != "a\nb\nc\n" {
		t.Errorf("got %q", got)
	}
	in := "a\nb"
	if !borrowed(normalize(in), in) {
		t.Errorf("text with no carriage return was copied")
	}
}
