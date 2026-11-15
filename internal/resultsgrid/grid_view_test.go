package resultsgrid

// Tests of the painted grid, run headless through mygo's Tester. They are
// extra to the Rust's 45 — GPUI's view had no tests — and check that the
// view hears the clicks and the drag the way the Rust view did.

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/uikit"
)

func TestMain(m *testing.M) {
	if err := uikit.RegisterFonts(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// harness is the shell's side of the grid, cut down: it owns the data, the
// scroll state and the selection, and turns a Hit into a change on the
// selection the way `Shell::hit_handler` does.
type harness struct {
	data      *GridData
	state     GridState
	selection Selection
	hits      []Hit
}

func newHarness(rows, columns int) *harness {
	names := make([]string, columns)
	for i := range names {
		names[i] = fmt.Sprintf("column_%d", i)
	}
	values := make([][]dbclient.Value, rows)
	for r := range values {
		row := make([]dbclient.Value, columns)
		for i := range row {
			switch {
			case i == 0:
				row[i] = dbclient.ValueInt(r)
			case i == 2 && r%3 == 0:
				row[i] = dbclient.ValueNull{}
			default:
				row[i] = dbclient.ValueText(fmt.Sprintf("r%d c%d some text", r, i))
			}
		}
		values[r] = row
	}
	return &harness{data: NewGridData(names, values), state: NewGridState()}
}

func (h *harness) onHit(hit Hit) {
	h.hits = append(h.hits, hit)
	extent := ExtentOf(h.data)
	switch hit := hit.(type) {
	case HitCell:
		if hit.Extend {
			h.selection.ExtendTo(hit.Cell)
		} else {
			h.selection.Focus(hit.Cell)
		}
	case HitDrag:
		h.selection.ExtendTo(hit.Cell)
	case HitPick:
		if hit.Through {
			h.selection.PickThrough(hit.Row)
		} else {
			h.selection.TogglePick(hit.Row)
		}
	case HitPickAll:
		h.selection.ToggleAllPicks(extent)
	case HitColumn:
		h.selection.SelectColumn(hit.Column, extent)
	}
}

func (h *harness) view(c *ui.Context) {
	ui.Column(c).Fill().Children(func() {
		NewGrid("tab-1", h.data, &h.state, &h.selection).OnHit(h.onHit).Render(c)
	})
}

// laneCentre is the window x of the middle of a lane, unscrolled.
func (h *harness) laneCentre(column int) float32 {
	left, width, _ := h.data.LaneSpan(column)
	return left + width/2
}

// rowCentre is the window y of the middle of a data row, unscrolled.
func rowCentre(row int) float32 {
	return HeaderHeight + float32(row)*RowHeight + RowHeight/2
}

func savePNG(t *testing.T, tt *ui.Tester, name string) {
	t.Helper()
	dir := os.Getenv("RESULTSGRID_PNG_DIR")
	if dir == "" {
		return
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, tt.Image()); err != nil {
		t.Fatal(err)
	}
}

func TestTheGridPaintsAHeaderAGutterAndOnlyTheRowsOnScreen(t *testing.T) {
	h := newHarness(500, 8)
	tt := ui.NewTester(h.view, 700, 300)
	tt.Frame()
	for _, text := range []string{"column_0", "#", "1", "r0 c1 some text", "NULL"} {
		if !tt.HasText(text) {
			t.Errorf("%q is not on screen", text)
		}
	}
	if tt.HasText("r400 c1 some text") {
		t.Error("a row far below the pane was built")
	}
	if h.state.scroll.MaxOffset().Y <= 0 || h.state.scroll.MaxOffset().X <= 0 {
		t.Errorf("max offsets %+v: the grid should scroll both ways", h.state.scroll.MaxOffset())
	}
	savePNG(t, tt, "grid.png")
}

func TestAPressOnACellFocusesItAndShiftExtends(t *testing.T) {
	h := newHarness(50, 4)
	tt := ui.NewTester(h.view, 900, 400)
	tt.Frame()
	tt.ClickAt(h.laneCentre(1), rowCentre(2))
	tt.Frame()
	if !h.selection.IsCursor(2, 1) {
		t.Fatalf("cursor %v after a click on (2, 1); hits %v", h.selection.cursor, h.hits)
	}
	tt.ClickAtWith(ui.Shift, h.laneCentre(3), rowCentre(4))
	tt.Frame()
	if got, ok := h.selection.Rect(); !ok || got != (Rect{Top: 2, Bottom: 4, Left: 1, Right: 3}) {
		t.Fatalf("rect %+v, %v after a shift-click", got, ok)
	}
	savePNG(t, tt, "range.png")
}

func TestTheGutterAndTheHeaderSpeakForRowsAndColumns(t *testing.T) {
	h := newHarness(5, 3)
	tt := ui.NewTester(h.view, 900, 400)
	tt.Frame()
	tt.ClickAt(gutterWidth/2, rowCentre(1))
	tt.Frame()
	tt.ClickAtWith(ui.Shift, gutterWidth/2, rowCentre(3))
	tt.Frame()
	if h.selection.Picks() != 3 {
		t.Fatalf("picks %d after a click and a shift-click in the gutter; hits %v", h.selection.Picks(), h.hits)
	}
	tt.ClickAt(gutterWidth/2, HeaderHeight/2)
	tt.Frame()
	if !h.selection.AllPicked(ExtentOf(h.data)) {
		t.Fatalf("the gutter's head did not tick every row; hits %v", h.hits)
	}
	tt.ClickAt(h.laneCentre(2), HeaderHeight/2)
	tt.Frame()
	if got, ok := h.selection.Rect(); !ok || got != (Rect{Top: 0, Bottom: 4, Left: 2, Right: 2}) {
		t.Fatalf("rect %+v, %v after a header click; hits %v", got, ok, h.hits)
	}
	for _, hit := range h.hits {
		if _, ok := hit.(HitCell); ok {
			t.Fatalf("a gutter or header click reached the cells: %v", h.hits)
		}
	}
	savePNG(t, tt, "picks.png")
}

func TestADragGrowsTheRangeAndScrollsPastTheEdge(t *testing.T) {
	h := newHarness(500, 4)
	tt := ui.NewTester(h.view, 900, 300)
	tt.Frame()
	tt.Press(h.laneCentre(0), rowCentre(1))
	tt.Frame()
	tt.Move(h.laneCentre(1), rowCentre(3))
	tt.Frame()
	if got, ok := h.selection.Rect(); !ok || got != (Rect{Top: 1, Bottom: 3, Left: 0, Right: 1}) {
		t.Fatalf("rect %+v, %v after dragging to (3, 1)", got, ok)
	}
	// Held past the bottom edge, the grid scrolls itself one step a
	// frame, and the range follows.
	tt.Move(h.laneCentre(1), 300+40)
	for range 20 {
		tt.Advance(16 * time.Millisecond)
	}
	if h.state.scroll.State.Y <= 0 {
		t.Fatal("a drag held past the bottom did not scroll")
	}
	cursor, _ := h.selection.Cursor()
	if cursor.Row <= 9 {
		t.Fatalf("cursor at row %d after scrolling", cursor.Row)
	}
	tt.Release(h.laneCentre(1), 300+40)
	tt.Frame()
	if h.state.pointer.dragging {
		t.Fatal("the release did not end the drag")
	}
	y := h.state.scroll.State.Y
	for range 5 {
		tt.Advance(16 * time.Millisecond)
	}
	if h.state.scroll.State.Y != y {
		t.Fatal("the grid went on scrolling after the release")
	}
	savePNG(t, tt, "drag.png")
}

func TestRevealSlidesThePaneAlongByOneLane(t *testing.T) {
	h := newHarness(20, 12)
	tt := ui.NewTester(h.view, 600, 300)
	tt.Frame()
	h.state.Reveal(NewCell(0, 11), h.data)
	tt.Frame()
	left, width, _ := h.data.LaneSpan(11)
	viewport := h.state.scroll.Bounds.W
	want := min(left+width-viewport, h.state.scroll.MaxOffset().X)
	if got := h.state.scroll.State.X; got != want {
		t.Fatalf("x %v after revealing the last lane, want %v", got, want)
	}
	savePNG(t, tt, "reveal.png")
}
