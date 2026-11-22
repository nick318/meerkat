// Package resultsgrid is the results grid: a gutter and a column header
// over a virtualized row list. The table view and the query runner both
// render through it, so a result looks the same wherever it came from.
//
// Rows are virtualized with `uniform_list`, so a 500-row page costs the
// same as a 15-row one. Lane widths are measured once from the values
// themselves; dragging a column to resize it comes later.
//
// What is marked lives in [Selection], which the caller owns beside the
// scroll position: the grid paints a selection and reports where the mouse
// landed, and decides nothing itself. Every click comes back as one
// [Hit], so the whole of what a click can mean is one enum the caller
// matches on rather than five callbacks that can disagree.
package resultsgrid

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// RowHeight is the row height from the design comp. The header is 32px,
// data rows 28px.
const RowHeight float32 = 28.
const HeaderHeight float32 = 32.

// dataFontSize is the data text size, from the design comp.
const dataFontSize = theme.SizeBody

// charWidth: the app is monospaced throughout, so one character is one
// advance and a lane can be sized from the character count alone.
// JetBrains Mono advances 0.6em, so 7.2px at 12px; round up so a
// full-width value keeps a hair of slack instead of tripping the ellipsis.
const charWidth float32 = dataFontSize * 0.62

// charAdvance is the advance itself, unrounded. Sizing a lane rounds up so
// a value fits; counting what a lane can show must not, or the cut would
// land short of the lane's edge.
const charAdvance float32 = dataFontSize * 0.6

// cellPadding is the 12px of padding on each side of a cell.
const cellPadding float32 = 24.
const minColumnWidth float32 = 56.

// maxColumnWidth: past this a column steals the pane; the rest of the
// value truncates.
const maxColumnWidth float32 = 320.

// widthSample is the rows sampled to size the lanes. The first screens
// decide the widths; scanning a whole 500-row page for a few pixels is not
// worth it.
const widthSample = 200

// gutterWidth is the lane down the left holding the row number, and the
// target that ticks a row. Wide enough for the comp's own gutter, which is
// 44px in the query editor — the two read as the same lane.
const gutterWidth float32 = 44.

// tick is the mark a ticked row wears in the gutter, in place of its
// number.
const tick = "✓"

// Hit is where a click landed, and what the modifiers said about it. The
// caller turns one of these into a change on its [Selection].
//
// Go port: the Rust enum with data is this interface with one struct per
// variant — [HitCell], [HitDrag], [HitPick], [HitPickAll], [HitColumn] —
// matched with a type switch. Every variant is comparable, so `==` is the
// derived `PartialEq`.
type Hit interface {
	isHit()
}

// HitCell is a cell. `Extend` is ⇧ held, which grows the range instead of
// starting a new one. `Peek` is the second click of a double click,
// which asks to read the whole value: a lane is capped, so a long value
// truncates on screen and the click is how the rest of it is asked for.
type HitCell struct {
	Cell   Cell
	Extend bool
	Peek   bool
}

// HitDrag says the pointer has moved onto this cell with the button still
// down, so the range grows to it. It is not a fresh click: the press that
// started the drag has already taken the focus and set the anchor.
type HitDrag struct {
	Cell Cell
}

// HitPick is the gutter beside a row: tick it, or with ⇧ tick everything
// back to the last row ticked.
type HitPick struct {
	Row     int
	Through bool
}

// HitPickAll is the gutter's own header: every row, or none.
type HitPickAll struct{}

// HitColumn is a column header: the whole column.
type HitColumn struct {
	Column int
}

func (HitCell) isHit()    {}
func (HitDrag) isHit()    {}
func (HitPick) isHit()    {}
func (HitPickAll) isHit() {}
func (HitColumn) isHit()  {}

// OnHit is called with wherever the mouse landed in the grid.
//
// Go port: GPUI handed the callback the window and the app; in mygo the
// view and its state live on the main thread, so the closure captures
// what it needs.
type OnHit func(hit Hit)

// GridState is the scroll position for one grid, held by whoever owns the
// tab so it survives the re-render after every keystroke and every page.
//
// Go port: GPUI's grid had two scroll containers — the `uniform_list`
// for the rows and a plain one around it for the columns. mygo's `List`
// scrolls both ways itself (`RowWidth`), with one `ScrollState` for both
// axes, and the header goes with it through `ScrollWith`. So `rows` is the
// list's own state (what `scroll_to_item` went through) and `scroll` is
// the one handle both overlay bars and both halves of `reveal` read; its
// offsets count **up** as the content travels, where GPUI's ran negative.
// Hold it in place and pass a pointer: a `ui.ListState` shown by a `List`
// must not be copied.
type GridState struct {
	rows    ui.ListState
	scroll  uikit.ScrollHandle
	drag    uikit.DragState
	pointer pointer
}

// pointer is what the pointer is doing to the grid. Neither of these is a
// selection — they are the mouse's own state between one frame and the
// next, so they live beside the scroll position rather than in [Selection].
//
// Go port: plain fields, not `Rc<Cell<…>>`: every closure that reads them
// holds the `*GridState`, and everything runs on the main thread.
type pointer struct {
	// The row whose gutter the pointer is over, if any.
	//
	// A hover style paints the element it is set on, and the wash for a
	// whole row is painted by the *row*, which is the gutter's parent — no
	// style reaches upwards. So the gutter records the row it is over and
	// every row reads it back.
	hoverRow *int
	// The left button went down on a cell and has not come up yet, so a
	// move grows the range instead of doing nothing.
	dragging bool
	// Where the pointer was last seen, relative to the row list's box. A
	// drag held past an edge scrolls on every frame, and a frame is not a
	// mouse event: it has to read the position from somewhere.
	//
	// Go port: GPUI kept the window's coordinates and subtracted the
	// list's bounds; mygo hands the event over relative to the element
	// that took the press, which is the list, so the bounds' origin is
	// already taken off.
	at ui.Point
}

// Go port: `PointerFrame` carried the view id the hover listener needed
// for `cx.notify(view)`. mygo needs none — `c.Invalidate()` asks for the
// frame — so it is gone.

// NewGridState is `GridState::new`.
func NewGridState() GridState {
	return GridState{}
}

// Reveal brings a cell into view, down **and across**. A cursor that has
// walked off the edge of the pane is a cursor the user has lost, and a
// result wider than the pane is the ordinary case, not the exception.
//
// The rows go through the list's own handle. The columns are a plain
// scroll container, so the offset is worked out here — moving the least
// it can, so walking a row sideways slides the pane along by one lane
// rather than jumping the cell to an edge.
func (g *GridState) Reveal(cell Cell, data *GridData) {
	// ScrollStrategy::Nearest: as little as shows it.
	g.rows.ScrollIntoView(cell.Row)

	left, width, ok := data.LaneSpan(cell.Column)
	if !ok {
		return
	}
	columns := g.columnsHandle()
	viewport := columns.Bounds.W
	// Nothing is painted yet on the frame a result lands: there is no
	// pane to be inside of, and the next frame's scroll would be
	// measured against zero.
	if viewport <= 0. {
		return
	}
	offset := columns.Offset()
	// Go port: GPUI's offset ran negative as the content moved left under
	// the pane, while `max_offset` was a positive distance; mygo's offset
	// is the positive distance travelled, so both read the same way and
	// the Rust's negation is gone.
	visibleLeft := offset.X
	furthest := maxF(columns.MaxOffset().X, 0.)
	wanted := clampF(revealOffset(left, width, visibleLeft, viewport), 0., furthest)
	if wanted != visibleLeft {
		columns.State.X = wanted
	}
}

// rowsHandle is the handle the row list scrolls through; the scrollbar only
// needs the plain one.
func (g *GridState) rowsHandle() *uikit.ScrollHandle {
	return &g.scroll
}

// columnsHandle is the handle the columns scroll through. Go port: the
// same one as the rows', see [GridState].
func (g *GridState) columnsHandle() *uikit.ScrollHandle {
	return &g.scroll
}

// GridData is one rendered result set, with its lanes already sized.
// Widths are computed once here rather than on every frame.
type GridData struct {
	Columns []string
	Rows    [][]dbclient.Value
	widths  []float32
}

// NewGridData is `GridData::new`.
func NewGridData(columns []string, rows [][]dbclient.Value) *GridData {
	widths := ColumnWidths(columns, rows)
	return &GridData{
		Columns: columns,
		Rows:    rows,
		widths:  widths,
	}
}

// WithRows is the same result with other rows in it, keeping this one's
// lane widths: an edit typed into a lane must not make it jump.
func (d *GridData) WithRows(rows [][]dbclient.Value) *GridData {
	return &GridData{Columns: d.Columns, Rows: rows, widths: d.widths}
}

// EmptyGridData is `GridData::empty`.
func EmptyGridData() *GridData {
	return NewGridData(nil, nil)
}

// LaneSpan is where one lane sits in the scrolling content: its left edge
// and its width, measured from the content's own left — so the gutter,
// which scrolls with the content, counts.
//
// The last lane is laid out with its width as a floor and takes the
// slack, so on screen it can be wider than this says. That only ever
// makes it *more* visible than the sum promises, which is the safe way
// round for scrolling something into view.
func (d *GridData) LaneSpan(column int) (left, width float32, ok bool) {
	if column < 0 || column >= len(d.widths) {
		return 0, 0, false
	}
	width = d.widths[column]
	var sum float32
	for _, w := range d.widths[:column] {
		sum += w
	}
	return gutterWidth + sum, width, true
}

// revealOffset is how far the content should be scrolled left so a lane is
// whole on screen, moving the least it can.
//
// A lane already inside the pane is left alone: without that, every step
// along a row would drag the pane about. A lane off to the left comes to
// the pane's left edge; one off to the right comes to its right edge. A
// lane wider than the pane cannot be whole either way, so its left edge
// wins — the value is read from the left.
func revealOffset(left, width, visibleLeft, viewport float32) float32 {
	right := left + width
	if left < visibleLeft || width >= viewport {
		return left
	} else if right > visibleLeft+viewport {
		return right - viewport
	} else {
		return visibleLeft
	}
}

// CellText is what a cell paints for a value.
//
// **A row is 28 pixels tall, so only the first line of a value can be
// on screen**, and a value with a newline in it is not merely too long:
// GPUI lays a shaped text out line by line whatever `truncate` says, so
// one pretty-printed `jsonb` would push every row under it out of line.
// So the cut is made here rather than left to the ellipsis, and the
// first line is cut the way a long single-line value already is.
//
// **The line is cut to `chars`, what the lane can show**, which
// [laneChars] works out from the lane's width. `MAX_CELL_BYTES` lets a
// megabyte of text into one value, and shaping a megabyte of it per
// visible cell per frame would freeze the window — but the cut is the
// lane's and not one number for every lane, because the last lane takes
// the slack. A fixed cap sized for a 320-pixel lane painted a one-column
// result, an `EXPLAIN ANALYZE` plan, as a pane-wide lane of lines that
// all stopped at the same character with room to spare after them.
//
// `…` says the cut was made, in the character `Value::display` already
// uses for a shortened `bytea`. Nothing else is dropped: ⏎ over the cell
// or a double click opens the value whole, and ⌘C copies it whole.
func CellText(value dbclient.Value, chars int) string {
	text := value.Display()
	// A value that merely ends in a newline has nothing after it worth
	// marking, and a `text` column full of them would wear an ellipsis on
	// every row for no dropped word.
	body := strings.TrimRight(text, "\n\r")
	first, _, _ := strings.Cut(body, "\n")
	// A CRLF buffer would otherwise leave the carriage return on the end
	// of every line, which shapes as a box or as nothing at all.
	first = strings.TrimSuffix(first, "\r")
	var out strings.Builder
	taken := 0
	for _, ch := range first {
		if taken == chars {
			break
		}
		out.WriteRune(ch)
		taken++
	}
	if out.Len() < len(body) {
		out.WriteRune('…')
	}
	return out.String()
}

// laneChars is how many characters a lane `width` pixels wide can show,
// and one more.
//
// The one more is what leaves the ellipsis to the text system: a value
// cut short of the edge would end in `…` with room left after it, while
// one cut a character past the edge overflows and is truncated at the
// edge, where every other long value in the grid is.
func laneChars(width float32) int {
	return f32ToUsize(float32(math.Ceil(float64(maxF(width-cellPadding, 0.)/charAdvance)))) + 1
}

// cellChars is the characters a cell may shape, from its lane's measured
// width.
//
// The last lane is laid out with its width as a floor and takes whatever
// the pane has left, so its measured width says nothing about how much of
// a value is on screen. The window is the bound instead: the pane is never
// wider than the window it is in, so a cut to the window's width is never
// short, and it is still a bound — a few hundred characters, not the
// megabyte a value may hold.
func cellChars(width float32, last bool, windowWidth float32) int {
	if last {
		return laneChars(maxF(width, windowWidth))
	}
	return laneChars(width)
}

// ColumnWidths sizes each lane to the widest value it actually holds,
// header included.
func ColumnWidths(columns []string, rows [][]dbclient.Value) []float32 {
	widths := make([]float32, 0, len(columns))
	for ix, name := range columns {
		widest := 0
		for _, row := range rows[:min(len(rows), widthSample)] {
			if ix >= len(row) {
				continue
			}
			widest = max(widest, utf8.RuneCountInString(CellText(row[ix], laneChars(maxColumnWidth))))
		}
		widest = max(widest, utf8.RuneCountInString(name))
		// Each step rounds to f32, as the Rust does: no fused multiply-add.
		width := float32(float32(widest)*charWidth) + cellPadding
		widths = append(widths, clampF(width, minColumnWidth, maxColumnWidth))
	}
	return widths
}

// Grid is one result set on screen. `id` must be unique among the grids
// alive in one window, because it keys the list's scroll position.
type Grid struct {
	id        string
	data      *GridData
	state     *GridState
	selection *Selection
	firstRow  int
	onHit     OnHit
	hits      []Cell
	edit      *EditView
}

// NewGrid is `Grid::new`.
func NewGrid(id string, data *GridData, state *GridState, selection *Selection) *Grid {
	return &Grid{
		id:        id,
		data:      data,
		state:     state,
		selection: selection,
		firstRow:  1,
		onHit:     nil,
		hits:      nil,
	}
}

// FirstRow is the number the gutter gives the first row on screen. A table
// page is a window on the table, so its gutter counts from where the page
// starts rather than from 1 again.
func (g *Grid) FirstRow(firstRow int) *Grid {
	g.firstRow = firstRow
	return g
}

func (g *Grid) OnHit(onHit OnHit) *Grid {
	g.onHit = onHit
	return g
}

// Hits are the cells the ⌘F line found, in reading order — what
// [FindCells] answers. Each wears `find_hit`, and the one under the
// cursor `find_current`: the walk moves the cursor, so the cursor is
// where the walk stands.
func (g *Grid) Hits(hits []Cell) *Grid {
	g.hits = hits
	return g
}

// Render builds the grid into the current parent and returns it.
func (g *Grid) Render(c *ui.Context) ui.Element {
	id, data, state, selection, firstRow, onHit, hits, edit :=
		g.id, g.data, g.state, g.selection, g.firstRow, g.onHit, g.hits, g.edit
	colors := &theme.Current().Colors
	// The content can be wider than the pane; the whole grid scrolls
	// sideways as one, gutter and header included. The gutter goes with
	// it rather than pinning itself to the left edge: pinning would
	// need a second vertical scroller kept in step with the row list,
	// and `uniform_list` gives nothing to keep it in step with.
	contentWidth := gutterWidth
	for _, w := range data.widths {
		contentWidth += w
	}
	// The list closure outlives this frame's borrow, so it takes a
	// selection of its own. One clone of a small set per frame.
	marks := selection.Clone()
	extent := ExtentOf(data)

	// The bars sit outside the scrolling content, or they would scroll
	// away with it.
	grid := ui.Box(c.Key(id)).
		Grow(1).Basis(0).
		MinHeight(0).
		// A grid is laid out beside the row drawer, so it is a flex item
		// in a row — and a flex item may not shrink below its content by
		// default. Without this the wide content makes the *grid* wide
		// and nothing overflows the scroll container inside it, which is
		// to say the columns stop scrolling sideways at all.
		MinWidth(0)
	grid.Children(func() {
		ui.Column(c.Key(id + "-grid")).
			Fill().
			// The lanes are measured against this size; leaving
			// the default here would render the data wider than
			// its lane.
			FontSize(dataFontSize).
			// Lanes are sized by a fixed advance per
			// character, which only a monospace honours.
			Font(theme.MonoFontFamily).
			Children(func() {
				head := headerRow(c, data, &marks, extent, onHit, edit, colors)
				// The drag lives on the window, not on a hitbox, so it
				// goes on working past the edges of the pane. Go port:
				// it is the list's own pointer capture; see
				// [dragSurface].
				var surface *dragSurface
				if onHit != nil {
					surface = &dragSurface{data: data, state: state, onHit: onHit}
					surface.frame(c)
				}
				rows := rowList(c, id, data, state, &marks, hits, firstRow, onHit, edit, contentWidth)
				if surface != nil {
					surface.listen(rows)
				}
				head.ScrollWith(rows)
			})

		if bar, ok := uikit.NewScrollbar(
			true,
			state.rowsHandle(),
			&state.drag,
			colors.TextFaint,
			colors.TextMuted,
		); ok {
			ui.Box(c.Key(id + "-vbar")).
				Absolute().
				// Start below the header, so the bar never covers it.
				Top(HeaderHeight).
				Right(0).
				Bottom(0).
				Width(uikit.Thickness).
				Children(func() { bar.View(c) })
		}
		if bar, ok := uikit.NewScrollbar(
			false,
			state.columnsHandle(),
			&state.drag,
			colors.TextFaint,
			colors.TextMuted,
		); ok {
			ui.Box(c.Key(id + "-hbar")).
				Absolute().
				Left(0).
				Right(0).
				Bottom(0).
				Height(uikit.Thickness).
				Children(func() { bar.View(c) })
		}
	})
	return grid
}

// autoscrollMin and autoscrollMax bound a drag past the pane's edge, in
// pixels of scroll per frame. The floor keeps a pointer one pixel over the
// line moving at a readable speed; the ceiling keeps a pointer flung to the
// far side of the screen from crossing a 500-row page in three frames.
const autoscrollMin float32 = 6.
const autoscrollMax float32 = 48.

// dragSurface is the drag's own mouse handlers, and the frames that carry
// an autoscroll.
//
// Both live on the **window** rather than on a hitbox, which is the
// scrollbar's pattern and is here for a sharper reason: the whole point of
// a drag past the edge is that the pointer has left the cells, so an
// element that only hears about its own bounds would go deaf exactly when
// it is needed. It also means the cells themselves carry no move handler —
// one listener a grid, instead of one for every cell on screen.
//
// The cell under the pointer is worked out from the geometry rather than
// asked of the elements, for the same reason: past the edge there is no
// element to ask, and the nearest cell is what the drag is reaching for.
//
// It paints nothing and takes no room. It is a place in the frame to hang
// listeners from.
//
// Go port: mygo's window-level listener is its implicit pointer capture —
// the element that takes a press gets every move and the release, wherever
// the pointer goes. The element that takes it here is the **row list**,
// which outlives the rows it scrolls away; a cell that took it would be
// dropped by the virtualized list mid-drag. So the press that GPUI heard
// on each cell (`on_mouse_down` in `data_row`) is heard here too, and the
// cell it landed on is worked out from the geometry, as the drag's are.
// GPUI's `paint` (the per-frame autoscroll) is [dragSurface.frame], run
// from the build; `on_next_frame` is the next build, asked for with
// `c.AnimationFrame()`.
type dragSurface struct {
	data  *GridData
	state *GridState
	onHit OnHit
}

// listen hangs the press, the moves and the release on the row list.
func (d *dragSurface) listen(rows ui.Element) {
	state, data, onHit := d.state, d.data, d.onHit
	p := &state.pointer
	rows.HandleInput(func(ev ui.InputEvent) bool {
		switch ev.Kind {
		case ui.InputPointerDown:
			if ev.Button != 0 {
				return false
			}
			// On the press, not the release: a drag has to start from the
			// cell the button went down on, and the release may be three
			// cells away — or off the pane entirely. Where it goes from
			// here is the moves'.
			at, ok := pressedCell(ev.X, ev.Y, data, state)
			if !ok {
				// The gutter's own click, or nothing under the press.
				return false
			}
			p.dragging = true
			p.at = ui.Point{X: ev.X, Y: ev.Y}
			onHit(HitCell{
				Cell:   at,
				Extend: ev.Mods&ui.Shift != 0,
				Peek:   ev.Clicks >= 2,
			})
			return true
		case ui.InputPointerMove:
			if !p.dragging {
				return false
			}
			// A button that came up somewhere nothing here heard about
			// ends the drag, rather than leaving it out for ever.
			if ev.Button != 0 {
				p.dragging = false
				return false
			}
			p.at = ui.Point{X: ev.X, Y: ev.Y}
			dragTo(p.at, data, state.rowsHandle(), state.columnsHandle(), onHit)
			return true
		case ui.InputPointerUp:
			if ev.Button == 0 && p.dragging {
				p.dragging = false
				return true
			}
		}
		return false
	})
}

// frame is GPUI's `paint`: one frame of autoscroll.
func (d *dragSurface) frame(c *ui.Context) {
	p := &d.state.pointer
	rows := d.state.rowsHandle()
	columns := d.state.columnsHandle()

	// Past an edge with the button still down, the grid scrolls itself:
	// the cells the user is reaching for are the ones not on screen, and
	// a pointer held still out there sends no more events to act on. So
	// the scroll is per *frame*, and each frame asks for the next by
	// moving the selection, which repaints.
	if !p.dragging {
		return
	}
	at := p.at
	// Go port: `at` is relative to the list's box, so its top and left
	// are zero and its bottom and right are its size.
	down := autoscrollStep(at.Y, 0, rows.Bounds.H)
	across := autoscrollStep(at.X, 0, columns.Bounds.W)
	nextRows, rowsMove := steppedOffset(rows, true, down)
	nextColumns, columnsMove := steppedOffset(columns, false, across)
	// Nothing left to travel on either axis ends the loop, or a drag
	// held past the last row would repaint for ever.
	if !rowsMove && !columnsMove {
		return
	}
	// Go port: the two handles are one, so each step sets only its own
	// axis, or the second would put the first one's back.
	if rowsMove {
		rows.State.Y = nextRows.Y
	}
	if columnsMove {
		columns.State.X = nextColumns.X
	}
	dragTo(at, d.data, rows, columns, d.onHit)
	c.AnimationFrame()
}

// pressedCell is the cell a press on the row list landed on, or false for
// a press the cells do not take: on the gutter, which ticks the row with a
// click of its own, under the last row, or past the end of a ragged row —
// the places GPUI had no cell to hear it.
func pressedCell(x, y float32, data *GridData, state *GridState) (Cell, bool) {
	rows, columns := state.rowsHandle(), state.columnsHandle()
	if x+columns.Offset().X < gutterWidth {
		return Cell{}, false
	}
	row, ok := rowAt(y, 0, -rows.Offset().Y)
	if !ok || row >= len(data.Rows) {
		return Cell{}, false
	}
	column, ok := columnAt(x, 0, -columns.Offset().X, data.widths)
	if !ok || column >= len(data.Rows[row]) {
		return Cell{}, false
	}
	return NewCell(row, column), true
}

// dragTo grows the range to whatever cell the pointer is over. Outside the
// pane that is the nearest cell, which is the one the drag is reaching for.
//
// Go port: `at` is relative to the list's box, so the bounds' origin is
// zero; the offsets are mygo's positive ones, negated into the negative
// offsets [rowAt] and [columnAt] were written for.
func dragTo(at ui.Point, data *GridData, rows, columns *uikit.ScrollHandle, onHit OnHit) {
	row, rowOK := rowAt(at.Y, 0, -rows.Offset().Y)
	column, columnOK := columnAt(at.X, 0, -columns.Offset().X, data.widths)
	if !rowOK || len(data.Rows) == 0 || !columnOK {
		return
	}
	cell := NewCell(min(row, len(data.Rows)-1), column)
	onHit(HitDrag{Cell: cell})
}

// rowAt is which row a pointer at `y` is over, counting from the pane's
// top. The scroll offset runs negative as the content moves up, so it is
// taken away rather than added. Above the first row answers the first row:
// a drag has to reach the top of the page, and there is nothing else up
// there.
func rowAt(y, top, offset float32) (int, bool) {
	row := float32(math.Floor(float64((y - top - offset) / RowHeight)))
	if math.IsInf(float64(row), 0) || math.IsNaN(float64(row)) {
		return 0, false
	}
	return f32ToUsize(maxF(row, 0.)), true
}

// columnAt is which lane a pointer at `x` is over. The gutter counts,
// because it scrolls with the content; a pointer over it is over the first
// lane, for the same reason a pointer above the first row is over the first
// row.
func columnAt(x, left, offset float32, widths []float32) (int, bool) {
	if len(widths) == 0 {
		return 0, false
	}
	position := x - left - offset - gutterWidth
	for ix, width := range widths {
		if position < width {
			return ix, true
		}
		position -= width
	}
	// Past the last lane's own width: the last lane takes the slack, so
	// that is still the last lane.
	return len(widths) - 1, true
}

// autoscrollStep is how far one frame of a drag scrolls, from how far the
// pointer is past the pane's edge. Nothing at all while the pointer is
// inside it — a drag that scrolled from the middle of the pane could never
// be made to stop.
func autoscrollStep(position, low, high float32) float32 {
	var past float32
	if position < low {
		past = position - low
	} else if position > high {
		past = position - high
	} else {
		return 0.
	}
	return signumF(past) * clampF(absF(past), autoscrollMin, autoscrollMax)
}

// steppedOffset is where a scroll handle lands after one frame of
// autoscroll, or false when the content cannot travel that way any
// further.
//
// Go port: mygo's offsets are the positive distance travelled, so the
// Rust's two negations cancel and the point holds the distance itself.
func steppedOffset(handle *uikit.ScrollHandle, vertical bool, step float32) (ui.Point, bool) {
	if step == 0. {
		return ui.Point{}, false
	}
	offset := handle.Offset()
	max := handle.MaxOffset()
	var travelled, furthest float32
	if vertical {
		travelled, furthest = offset.Y, maxF(max.Y, 0.)
	} else {
		travelled, furthest = offset.X, maxF(max.X, 0.)
	}
	wanted := clampF(travelled+step, 0., furthest)
	if wanted == travelled {
		return ui.Point{}, false
	}
	if vertical {
		return ui.Point{X: offset.X, Y: wanted}, true
	}
	return ui.Point{X: wanted, Y: offset.Y}, true
}

// rowList is the virtualized row list. Built here rather than inline so
// the axis lock can be set: `UniformList` carries an `Interactivity` but
// not `StatefulInteractiveElement`, so the style flag that
// `restrict_scroll_to_axis()` would set is set by hand.
//
// Go port: mygo has no axis lock to set; the list scrolls both ways
// itself (`RowWidth`), so there is no second container for a diagonal
// swipe to move at the same time.
func rowList(
	c *ui.Context,
	id string,
	data *GridData,
	state *GridState,
	marks *Selection,
	hits []Cell,
	firstRow int,
	onHit OnHit,
	edit *EditView,
	contentWidth float32,
) ui.Element {
	colors := &theme.Current().Colors
	windowWidth, _ := c.Size()
	rows := ui.List(c.Key(id+"-rows"), &state.rows, len(data.Rows), func(ix int) {
		dataRow(
			c,
			ix,
			data.Rows[ix],
			data.widths,
			windowWidth,
			marks,
			hits,
			firstRow,
			state,
			onHit,
			edit,
			colors,
		)
	}).
		Grow(1).Basis(0).
		RowWidth(contentWidth).
		TrackScroll(&state.scroll.State).
		Scrollbars(false)
	// The viewport the bars and `reveal` measure against: the list's box
	// as the last frame laid it out, which is what GPUI's `bounds()` was.
	state.scroll.Bounds = rows.Bounds()
	return rows
}

func headerRow(
	c *ui.Context,
	data *GridData,
	marks *Selection,
	extent Extent,
	onHit OnHit,
	edit *EditView,
	colors *theme.ThemeColors,
) ui.Element {
	row := ui.Row(c).
		Height(HeaderHeight).
		Shrink(0).
		// Rows are laid out inside the list, which does not stretch them;
		// without this the hairlines stop where the values do.
		FillWidth().
		AlignItems(ui.Center).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Border.Color()).
		Background(colors.Panel.Color()).
		FontWeight(600).
		TextColor(colors.TextSecondary.Color()).
		// Go port: `ScrollWith` moves what is inside; what goes out of
		// the row must not show.
		Clip()

	row.Children(func() {
		// The gutter's own head is the "all of them, or none" tick. It says
		// which state it is in rather than what a click would do, as every
		// other tick in the app does.
		all := marks.AllPicked(extent)
		hoverBg := colors.Hairline
		headColor := colors.TextMuted
		headText := "#"
		if all {
			headColor = colors.AccentDeep
			headText = tick
		}
		head := ui.Row(c.Key("gutter-head")).
			Width(gutterWidth).
			Shrink(0).
			FillHeight().
			AlignItems(ui.Center).
			Justify(ui.Center).
			BorderWidth(0, 1, 0, 0).
			BorderColor(colors.Hairline.Color()).
			FontWeight(400).
			TextColor(headColor.Color())
		head.Children(func() { ui.Text(c, headText) })
		if onHit != nil {
			head.Cursor(ui.CursorPointer)
			if head.Hovered() {
				head.Background(hoverBg.Color())
			}
			if head.Clicked() {
				onHit(HitPickAll{})
			}
		}

		last := saturatingSub1(len(data.Columns))
		selectedColumns, haveColumns := marks.Rect()
		if haveColumns && selectedColumns.Rows() != max(extent.Rows, 1) {
			haveColumns = false
		}
		cursor, haveCursor := marks.Cursor()
		for ix, name := range data.Columns {
			// A column reads as selected only when the range covers all of it,
			// which is what clicking its header does.
			whole := haveColumns && selectedColumns.Left <= ix && ix <= selectedColumns.Right
			// The header also says which column the cursor is in. It is the mark
			// that survives scrolling: the cursor's own cell can be a hundred
			// rows down the page, and after a jump to a column it is the only
			// thing that says the jump landed.
			current := haveCursor && cursor.Column == ix
			cell := lane(ui.Row(c.Key(ix)), data.widths[ix], ix == last).
				PaddingX(12).
				FillHeight().
				AlignItems(ui.Center).
				Clip()
			if ix > 0 {
				cell.BorderWidth(0, 0, 0, 1).BorderColor(colors.Hairline.Color())
			}
			if whole {
				cell.Background(colors.Selection.Color()).TextColor(colors.AccentDeep.Color())
			}
			if current {
				cell.TextColor(colors.AccentDeep.Color())
			}
			mark := edit.column(ix)
			if mark.Key || mark.Locked {
				cell.Gap(6)
			}
			cell.Children(func() {
				// A key column wears the key, and a column that takes no
				// edit inside a result that does wears a lock: the rest
				// of the header is the part that edits.
				if mark.Key {
					uikit.DrawIcon(c, uikit.IconKey, 11, colors.AccentDeep)
				}
				if mark.Locked {
					uikit.DrawIcon(c, uikit.IconLock, 11, colors.TextMuted)
				}
				// The name as the database spells it. The first edition set
				// headers in capitals, which made `userId` and `userid` — two
				// different columns to Postgres — read as the same one.
				ui.Text(c, name).SingleLine()
				if current {
					// Drawn as a child rather than as a bottom border, because a
					// border carries one colour for all four sides and the left
					// hairline has already claimed it — and because an absolutely
					// positioned rule takes no room, so the name above it does not
					// move as the cursor changes lane.
					ui.Box(c).
						Absolute().
						Left(0).
						Right(0).
						Bottom(0).
						Height(2).
						Background(colors.Accent.Color())
				}
			})
			if onHit != nil {
				cell.Cursor(ui.CursorPointer)
				if cell.Clicked() {
					onHit(HitColumn{Column: ix})
				}
			}
		}
	})
	return row
}

// lane sizes one cell. The last lane absorbs the slack, so short results
// still rule the full pane width instead of stopping mid-way; it keeps its
// fixed width as a floor, so a wide result still scrolls sideways.
func lane(cell ui.Element, width float32, last bool) ui.Element {
	if last {
		return cell.MinWidth(width).Grow(1).Basis(0)
	}
	return cell.Width(width).Shrink(0)
}

func dataRow(
	c *ui.Context,
	ix int,
	values []dbclient.Value,
	widths []float32,
	windowWidth float32,
	marks *Selection,
	hits []Cell,
	firstRow int,
	state *GridState,
	onHit OnHit,
	edit *EditView,
	colors *theme.ThemeColors,
) ui.Element {
	p := &state.pointer
	rowMark, number := edit.rowMark(ix, firstRow+ix)
	wash, gutterInk, rowInk, marked := rowColors(rowMark, colors)
	picked := marks.IsPicked(ix)
	// The gutter is the one target that speaks for the whole record, so it
	// is the one target that washes the whole row. A cell washes itself:
	// a click there marks that cell, and a hover must say what a click
	// would do rather than promise the row.
	rowHovered := p.hoverRow != nil && *p.hoverRow == ix
	row := ui.Row(c.Key(ix)).
		Height(RowHeight).
		Shrink(0).
		FillWidth().
		AlignItems(ui.Center).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Hairline.Color()).
		Cursor(ui.CursorPointer)
	if picked {
		// A ticked row is washed whole: the tick is a statement about the
		// record, not about a cell in it.
		row.Background(colors.Selection.Color())
	} else if marked && wash != (theme.Hsla{}) {
		// An added, deleted or conflicting row says so whole, under any
		// mark a cell of it carries.
		row.Background(wash.Color())
	} else if rowHovered {
		row.Background(colors.Panel.Color())
	}
	if rowMark == RowDeleted {
		row.Strikethrough().DecorationColor(rowInk.Color())
	}

	row.Children(func() {
		// The gutter: the row's number, or the tick when it is picked.
		gutterColor := colors.LineNumber
		cursor, haveCursor := marks.Cursor()
		switch {
		case picked:
			gutterColor = colors.AccentDeep
		// The number marks where the cursor is, so the eye can find the
		// focused row after scrolling sideways off its cell.
		case haveCursor && cursor.Row == ix:
			gutterColor = colors.TextSecondary
		}
		gutterText := strconv.Itoa(number)
		if marked {
			gutterText = gutterGlyph(rowMark)
			gutterColor = gutterInk
		}
		if picked {
			gutterText = tick
			gutterColor = colors.AccentDeep
		}
		gutter := ui.Row(c.Key("gutter")).
			Width(gutterWidth).
			Shrink(0).
			FillHeight().
			AlignItems(ui.Center).
			Justify(ui.Center).
			BorderWidth(0, 1, 0, 0).
			BorderColor(colors.Hairline.Color()).
			FontSize(theme.SizeMeta).
			TextColor(gutterColor.Color())
		if marked {
			gutter.FontWeight(600)
		}
		gutter.Children(func() { ui.Text(c, gutterText) })
		if onHit != nil {
			gutterHover := colors.Hairline
			hovered := gutter.Hovered()
			if hovered {
				gutter.Background(gutterHover.Color())
			}
			// The row wash is painted by the row, and no hover style
			// reaches up to a parent — so the row is told which gutter the
			// pointer is over, and repaints on the change.
			//
			// Go port: GPUI's `on_hover` fired on the change; mygo's hover
			// is read as the view builds, so the same rule runs every
			// frame and asks for a frame when it changed something.
			func() {
				var now *int
				if hovered {
					now = intPtr(ix)
				}
				same := (p.hoverRow == nil && now == nil) ||
					(p.hoverRow != nil && now != nil && *p.hoverRow == *now)
				// A leave for a row that is not the marked one is the tail
				// of a move onto another row, which has already said so.
				if same || (!hovered && (p.hoverRow == nil || *p.hoverRow != ix)) {
					return
				}
				p.hoverRow = now
				c.Invalidate()
			}()
			if gutter.Clicked() {
				onHit(HitPick{
					Row:     ix,
					Through: gutter.ClickModifiers()&ui.Shift != 0,
				})
			}
		}

		last := saturatingSub1(len(widths))
		for column, value := range values {
			// A row can be shorter than the header when a driver returns
			// ragged rows; lay out only what the header has room for.
			if column >= len(widths) {
				break
			}
			width := widths[column]
			cursor := marks.IsCursor(ix, column)
			_, hit := slices.BinarySearchFunc(hits, NewCell(ix, column), Cell.Compare)
			cellMark := edit.cellMark(ix, column)
			editing := edit.editing(ix, column)
			cell := lane(ui.Row(c.Key(column)), width, column == last).
				PaddingX(12).
				FillHeight().
				AlignItems(ui.Center).
				Clip().
				TextColor(valueColor(value, colors).Color())
			if rowMark == RowDeleted {
				cell.TextColor(rowInk.Color())
			}
			if editing {
				// The editor is the cell: white, with the accent's ring
				// drawn inside it, so the value does not move.
				cell.Background(colors.Raised.Color()).
					InsetShadow(0, 0, 0, 2, colors.Accent.Color()).
					PaddingX(10).
					Children(func() { edit.Editor(c) })
				continue
			}
			cell.Children(func() {
				switch cellMark {
				case CellStaged:
					cornerFlag(c, colors.Edit)
				case CellRefused:
					cornerFlag(c, colors.Error)
				}
				_, null := value.(dbclient.ValueNull)
				switch {
				case cellMark == CellDefault:
					chip(c, "DEFAULT", colors.EditDefault, colors.EditAdded)
				case null && cellMark == CellStaged:
					chip(c, "NULL", colors.EditBorder, colors.EditText)
				case cellMark == CellRefused:
					ui.Text(c, CellText(value, cellChars(width, column == last, windowWidth))).
						SingleLine().
						WavyUnderline().
						DecorationColor(colors.Error.Color())
				case null:
					// Absence, set in a well so it cannot be read as the word
					// somebody stored. Faint grey alone was 2.2:1 — it said
					// "nothing here" by being nearly invisible.
					ui.Box(c).
						Shrink(0).
						PaddingX(5).
						Radius(4).
						Background(colors.Sunk.Color()).
						FontSize(theme.SizeCaption).
						FixedLineHeight(17).
						Children(func() { ui.Text(c, "NULL") })
				default:
					ui.Text(c, CellText(value, cellChars(width, column == last, windowWidth))).SingleLine()
				}
			})
			switch cellMark {
			case CellStaged:
				cell.Background(colors.EditSurface.Color()).TextColor(colors.EditText.Color())
			case CellRefused:
				cell.Background(colors.MatchError.Color()).TextColor(colors.Error.Color())
			case CellLocked:
				if rowMark != RowAdded && rowMark != RowDeleted && rowMark != RowConflict {
					cell.Background(colors.EditLocked.Color())
				}
			}
			// The cursor's cell is the strongest mark on screen, the rest of
			// the range a wash under it. Neither carries a border: a border
			// would take a pixel out of the cell's content box and shift the
			// value inside it every time the cursor moved.
			//
			// A find hit sits between the two. The walk moves the cursor from
			// hit to hit, so the cursor on a hit is the one the count names and
			// takes the find family's deep tone; every other hit is washed
			// over the range, because while the line is open the hits are what
			// the user is reading.
			if cursor && hit {
				cell.Background(colors.FindCurrent.Color()).TextColor(colors.Text.Color())
			} else if cursor {
				cell.Background(colors.MatchStrong.Color()).TextColor(colors.Text.Color())
			} else if hit {
				cell.Background(colors.FindHit.Color())
			} else if marks.Contains(ix, column) {
				cell.Background(colors.RangeSurface.Color())
			} else if !picked && !rowHovered {
				// Hovering says "clickable", and here it says it of one cell,
				// because one cell is what a click marks. A cell already
				// carrying a mark says something truer, so it keeps it.
				hoverBg := colors.Panel
				if cell.Hovered() {
					cell.Background(hoverBg.Color())
				}
			}
			// Go port: GPUI's `on_mouse_down` on the cell is heard by the
			// row list, which takes the press for the drag that follows
			// it; see [dragSurface].
		}
	})
	return row
}

func valueColor(value dbclient.Value, colors *theme.ThemeColors) theme.Hsla {
	switch value.(type) {
	// NULL must read as absence, not as data.
	case dbclient.ValueNull:
		return colors.TextMuted
	case dbclient.ValueBytes:
		return colors.TextMuted
	default:
		return colors.TextBody
	}
}

// maxF is Rust's `f32::max`: a NaN operand is ignored.
func maxF(a, b float32) float32 {
	if a != a {
		return b
	}
	if b != b {
		return a
	}
	return max(a, b)
}

// clampF is Rust's `f32::clamp`: a NaN passes through.
func clampF(v, lo, hi float32) float32 {
	if v != v {
		return v
	}
	return min(max(v, lo), hi)
}

// absF is Rust's `f32::abs`.
func absF(v float32) float32 {
	return float32(math.Abs(float64(v)))
}

// signumF is Rust's `f32::signum`: 1 for +0 and up, -1 for -0 and down,
// NaN for NaN.
func signumF(v float32) float32 {
	if v != v {
		return v
	}
	if math.Signbit(float64(v)) {
		return -1
	}
	return 1
}

// f32ToUsize is Rust's saturating `as usize` from `f32`: NaN and anything
// below zero answer 0, anything past the top answers the top.
func f32ToUsize(v float32) int {
	if v != v || v <= 0 {
		return 0
	}
	if float64(v) >= float64(math.MaxInt) {
		return math.MaxInt
	}
	return int(v)
}
