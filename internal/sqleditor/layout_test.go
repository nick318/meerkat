package sqleditor

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
)

// walkToUTF16 and walkFromUTF16 are the conversions as they were first
// written, a walk from the start of the buffer: what the index must agree
// with.
func walkToUTF16(content string, target int) int {
	utf8n, utf16n := 0, 0
	for _, ch := range content {
		if utf8n >= target {
			break
		}
		utf8n += utf8.RuneLen(ch)
		utf16n += utf16Len(ch)
	}
	return utf16n
}

func walkFromUTF16(content string, target int) int {
	utf8n, utf16n := 0, 0
	for _, ch := range content {
		if utf16n >= target {
			break
		}
		utf16n += utf16Len(ch)
		utf8n += utf8.RuneLen(ch)
	}
	return utf8n
}

func TestUTF16OffsetsAgreeWithAWalkFromTheStart(t *testing.T) {
	for _, content := range []string{
		"",
		"select 1;\nselect 2;\n",
		"select 'é';\n-- 日本語\nselect '😀' as x;\n\nselect 1",
		"😀😀\n😀\n\n",
		"bad \xff utf8\nhere é",
	} {
		e := New(content, nil)
		total := walkToUTF16(content, len(content)+1)
		for b := -1; b <= len(content)+2; b++ {
			if got, want := e.offsetToUTF16(b), walkToUTF16(content, b); got != want {
				t.Errorf("%q: offsetToUTF16(%d) = %d, want %d", content, b, got, want)
			}
		}
		for u := -1; u <= total+2; u++ {
			if got, want := e.offsetFromUTF16(u), walkFromUTF16(content, u); got != want {
				t.Errorf("%q: offsetFromUTF16(%d) = %d, want %d", content, u, got, want)
			}
		}
	}
}

func longLine(word string, size int) string {
	var b strings.Builder
	for i := 0; b.Len() < size; i++ {
		b.WriteString(word)
		b.WriteString(strings.Repeat("x", i%5))
		b.WriteString(", ")
	}
	return b.String()
}

// A long line on the grid is placed where shaping it whole would have put
// every character, and a point on it lands on the character shaping would
// have answered.
func TestALongLineOnTheGridIsPlacedWhereShapingPutsIt(t *testing.T) {
	font := editorFont()
	for _, text := range []string{
		longLine("col_", 3000),
		longLine("café_", 3000),
	} {
		lc := newLineCache(text, font)
		if lc.grid == nil {
			t.Fatalf("%.20q… is not on the grid", text)
		}
		shaped := shapeLine(text, font)
		for b := 0; b <= len(text); b += 7 {
			if !isCharBoundary(text, b) {
				continue
			}
			got, want := lc.xForIndex(b), shaped.xForIndex(b)
			if math.Abs(float64(got-want)) > 0.05 {
				t.Fatalf("%.20q…: x of byte %d = %v, shaped %v", text, b, got, want)
			}
		}
		for x := float32(-3); x < shaped.width+20; x += 2.9 {
			got, want := lc.grid.closestIndexForX(text, x), shaped.closestIndexForX(x)
			if got != want {
				t.Fatalf("%.20q…: index at x %v = %d, shaped %d", text, x, got, want)
			}
		}
	}
}

// A line that is short, or holds a character off the grid, is shaped
// whole, as every line was.
func TestALineOffTheGridIsShapedWhole(t *testing.T) {
	font := editorFont()
	for _, text := range []string{
		"select 1",
		longLine("名前_", 3000),
		longLine("é_", 3000),
	} {
		if lc := newLineCache(text, font); lc.grid != nil {
			t.Errorf("%.20q… is on the grid", text)
		}
	}
}

// The binary searches over a shaped line answer what the walks did.
func TestTheShapedLineSearchesAgreeWithItsWalks(t *testing.T) {
	for _, text := range []string{"select 'é' from 日本 where a -> b", "x", "abc שלום def"} {
		l := shapeLine(text, editorFont())
		linear := *l
		linear.ascending, linear.xAscending = false, false
		for b := 0; b <= len(text)+1; b++ {
			if got, want := l.xForIndex(b), linear.xForIndex(b); got != want {
				t.Errorf("%q: xForIndex(%d) = %v, want %v", text, b, got, want)
			}
		}
		for x := float32(-2); x < l.width+10; x += 1.3 {
			if got, want := l.closestIndexForX(x), linear.closestIndexForX(x); got != want {
				t.Errorf("%q: closestIndexForX(%v) = %d, want %d", text, x, got, want)
			}
		}
	}
}

// A two-hundred-kilobyte line is painted by its window: what is shaped is
// what the pane shows, and the caret at its far end still lands where the
// columns put it.
func TestALongLineIsShapedOnlyWhereThePaneShowsIt(t *testing.T) {
	text := "select " + longLine("col_", 200_000) + "1;"
	e := New(text, nil)
	tt := ui.NewTester(editorView(e), 800, 300)
	e.FocusHandle().Focus()
	e.pendingAutoscroll = true
	tt.Frame()
	tt.Frame()
	line := e.lastLayout.line(0)
	if line == nil || line.grid == nil {
		t.Fatalf("the line is not on the grid")
	}
	if n := len(line.shaped.glyphs); n == 0 || n > 400 {
		t.Errorf("%d glyphs shaped for a pane 800 wide", n)
	}
	want := float32(len(text)) * monoAdvance()
	if got := line.xFor(len(text)); math.Abs(float64(got-want)) > 1e-6*float64(want) {
		t.Errorf("the end of the line is at %v, want %v", got, want)
	}
	if e.hScrollHandle.State.X <= 0 {
		t.Errorf("the view did not follow the caret across")
	}
	if line.paintX <= 0 {
		t.Errorf("the window starts at %v, not at the caret", line.paintX)
	}
	// A click at the left of the pane lands on the column under it.
	at := e.offsetForPosition(ui.Point{X: e.lastBounds.X + line.paintX + 10*monoAdvance() + 1, Y: e.lastBounds.Y + 1})
	if want := int((line.paintX)/monoAdvance()+0.5) + 10; at != want {
		t.Errorf("a click landed on %d, want %d", at, want)
	}
}

// The index measures the widest line by the same rule as widestLine, and
// knows whether any statement changes the shape.
func TestTheIndexMeasuresTheWidestLine(t *testing.T) {
	for _, content := range []string{
		"select 1",
		"a\n\tbb\n日本語のテキスト\n",
		"x\t\ty\nlonger line here\n",
		"\n\n",
		"alter table t add c int;\nselect 1;",
	} {
		e := New(content, nil)
		if want := widestLine(content); e.widest != want {
			t.Errorf("%q: widest = %d, want %d", content, e.widest, want)
		}
		if want := !allNone(lineShape(content, nil)) || strings.Contains(content, "alter"); e.anyShape != want {
			t.Errorf("%q: anyShape = %v", content, e.anyShape)
		}
	}
}
