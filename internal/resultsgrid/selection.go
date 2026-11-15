// What the user has picked out of a result.
//
// Two things get picked, and they are not the same thing. A **range** is a
// rectangle of cells grown from the cell the user pressed on: it is what
// the arrow keys move and what ⌘C copies. **Picks** are whole rows ticked
// in the gutter — discontiguous by nature, which is why they are a set and
// not a second rectangle. A row is ticked to say "this record"; a range is
// drawn to say "these cells", and one cannot stand for the other.
//
// All of it is plain data over indices, with no window and no theme in
// sight, so the policy can be argued with in a test rather than in a
// running window. The view reads it and paints; it decides nothing.

package resultsgrid

import (
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/nick318/meerkat/internal/dbclient"
)

// Cell is one cell, by row and column index into the result on screen. Both
// are indices into what the grid holds, never absolute row numbers in the
// table: paging replaces the rows, and a selection does not survive it.
//
// Cells order **row by row**, left to right within a row — the order the
// fields are declared in, which is what the derive compares. It is the
// order a result is read in, and so the order ⌘F walks its hits.
//
// Go port: the derived `Ord` is [Cell.Compare]; `==` is Go's own.
type Cell struct {
	Row    int
	Column int
}

// NewCell is `Cell::new`.
func NewCell(row, column int) Cell {
	return Cell{Row: row, Column: column}
}

// Compare is the derived `Ord`: by row, then by column.
func (c Cell) Compare(other Cell) int {
	if c.Row != other.Row {
		if c.Row < other.Row {
			return -1
		}
		return 1
	}
	if c.Column != other.Column {
		if c.Column < other.Column {
			return -1
		}
		return 1
	}
	return 0
}

// Step is how far one keystroke moves the cursor.
type Step int

const (
	StepUp Step = iota
	StepDown
	StepLeft
	StepRight
	// StepRowStart and StepRowEnd are ⌘← / ⌘→: the ends of the row the
	// cursor is on.
	StepRowStart
	StepRowEnd
	// StepFirst and StepLast are ⌘↑ / ⌘↓: the first and last row, in the
	// column the cursor is on.
	StepFirst
	StepLast
)

// Extent is how big the result is. The cursor is clamped to it, so a
// selection can never point past the rows in hand.
type Extent struct {
	Rows    int
	Columns int
}

// ExtentOf is `Extent::of`.
func ExtentOf(data *GridData) Extent {
	return Extent{
		Rows:    len(data.Rows),
		Columns: len(data.Columns),
	}
}

func (e Extent) isEmpty() bool {
	return e.Rows == 0 || e.Columns == 0
}

// Rect is the rectangle a range covers, both ends inclusive.
type Rect struct {
	Top    int
	Bottom int
	Left   int
	Right  int
}

func (r Rect) Rows() int {
	return r.Bottom - r.Top + 1
}

func (r Rect) Columns() int {
	return r.Right - r.Left + 1
}

func (r Rect) Cells() int {
	return r.Rows() * r.Columns()
}

func (r Rect) holds(row, column int) bool {
	return r.Top <= row && row <= r.Bottom && r.Left <= column && column <= r.Right
}

// Selection is one grid's selection, held by whoever owns the tab so it
// survives the re-render after every keystroke.
//
// Go port: the zero value is `Selection::default()`. A copy shares the
// picked rows; [Selection.Clone] is the derived `Clone`.
type Selection struct {
	// Where the range began: the cell the user pressed on, or the cell the
	// cursor was at when ⇧ first extended it.
	anchor *Cell
	// Where the range now ends. This is the cell the keys move and the one
	// the row drawer opens on. `nil` is "nothing selected".
	cursor *Cell
	// Rows ticked in the gutter. Kept sorted, as the Rust `BTreeSet` is.
	picked []int
	// The last row ticked, so ⇧-click can fill the gap up to it.
	lead *int
}

func cellPtr(cell Cell) *Cell {
	return &cell
}

func intPtr(value int) *int {
	return &value
}

// Clone is the derived `Clone`.
func (s *Selection) Clone() Selection {
	out := *s
	out.picked = slices.Clone(s.picked)
	return out
}

// Equal is the derived `PartialEq`.
func (s *Selection) Equal(other *Selection) bool {
	eqCell := func(a, b *Cell) bool {
		return (a == nil) == (b == nil) && (a == nil || *a == *b)
	}
	eqInt := func(a, b *int) bool {
		return (a == nil) == (b == nil) && (a == nil || *a == *b)
	}
	return eqCell(s.anchor, other.anchor) &&
		eqCell(s.cursor, other.cursor) &&
		slices.Equal(s.picked, other.picked) &&
		eqInt(s.lead, other.lead)
}

// Cursor is the focused cell, if there is one.
func (s *Selection) Cursor() (Cell, bool) {
	if s.cursor == nil {
		return Cell{}, false
	}
	return *s.cursor, true
}

// IsEmpty says nothing is marked at all — no cell, no ticked row. The
// status strip and ⌘C both need to know.
func (s *Selection) IsEmpty() bool {
	return s.cursor == nil && len(s.picked) == 0
}

// Focus puts the cursor on one cell and starts a fresh range there.
func (s *Selection) Focus(cell Cell) {
	s.anchor = cellPtr(cell)
	s.cursor = cellPtr(cell)
}

// ExtendTo grows the range to `cell`, keeping the anchor where it was. With
// nothing selected yet this is the same as a plain click: ⇧ on a fresh
// result has no anchor to grow from.
func (s *Selection) ExtendTo(cell Cell) {
	if s.anchor != nil {
		s.cursor = cellPtr(cell)
	} else {
		s.Focus(cell)
	}
}

// SelectColumn selects the whole column, from its first row to its last.
func (s *Selection) SelectColumn(column int, extent Extent) {
	if extent.isEmpty() || column >= extent.Columns {
		return
	}
	s.anchor = cellPtr(NewCell(0, column))
	s.cursor = cellPtr(NewCell(extent.Rows-1, column))
}

// SelectAll selects every cell in the result.
func (s *Selection) SelectAll(extent Extent) {
	if extent.isEmpty() {
		return
	}
	s.anchor = cellPtr(NewCell(0, 0))
	s.cursor = cellPtr(NewCell(extent.Rows-1, extent.Columns-1))
}

// saturatingSub1 is `usize::saturating_sub(1)`.
func saturatingSub1(value int) int {
	if value == 0 {
		return 0
	}
	return value - 1
}

// Step moves the cursor. `extend` keeps the anchor, so ⇧↓ grows the range
// where ↓ moves it whole. Returns the cell the cursor landed on, so
// the caller can scroll it into view; `false` when nothing moved.
//
// With no cursor yet the first keystroke lands on the first cell rather
// than doing nothing: a result that has just come back is one the user
// can start walking without clicking it first.
func (s *Selection) Step(step Step, extend bool, extent Extent) (Cell, bool) {
	if extent.isEmpty() {
		return Cell{}, false
	}
	lastRow := extent.Rows - 1
	lastColumn := extent.Columns - 1
	if s.cursor == nil {
		cell := NewCell(0, 0)
		s.Focus(cell)
		return cell, true
	}
	from := *s.cursor

	var to Cell
	switch step {
	case StepUp:
		to = NewCell(saturatingSub1(from.Row), from.Column)
	case StepDown:
		to = NewCell(min(from.Row+1, lastRow), from.Column)
	case StepLeft:
		to = NewCell(from.Row, saturatingSub1(from.Column))
	case StepRight:
		to = NewCell(from.Row, min(from.Column+1, lastColumn))
	case StepRowStart:
		to = NewCell(from.Row, 0)
	case StepRowEnd:
		to = NewCell(from.Row, lastColumn)
	case StepFirst:
		to = NewCell(0, from.Column)
	case StepLast:
		to = NewCell(lastRow, from.Column)
	}
	if to == from {
		return Cell{}, false
	}
	if extend {
		s.ExtendTo(to)
	} else {
		s.Focus(to)
	}
	return to, true
}

// Rect is the rectangle the range covers, if a range is drawn.
func (s *Selection) Rect() (Rect, bool) {
	if s.anchor == nil || s.cursor == nil {
		return Rect{}, false
	}
	anchor, cursor := *s.anchor, *s.cursor
	return Rect{
		Top:    min(anchor.Row, cursor.Row),
		Bottom: max(anchor.Row, cursor.Row),
		Left:   min(anchor.Column, cursor.Column),
		Right:  max(anchor.Column, cursor.Column),
	}, true
}

// Contains says whether this cell is inside the range. The cursor's own
// cell is, and the view paints it a shade stronger.
func (s *Selection) Contains(row, column int) bool {
	rect, ok := s.Rect()
	return ok && rect.holds(row, column)
}

func (s *Selection) IsCursor(row, column int) bool {
	return s.cursor != nil && *s.cursor == NewCell(row, column)
}

// insertPick is `BTreeSet::insert`, keeping the rows sorted.
func (s *Selection) insertPick(row int) {
	if at, found := slices.BinarySearch(s.picked, row); !found {
		s.picked = slices.Insert(s.picked, at, row)
	}
}

// removePick is `BTreeSet::remove`: whether the row was there.
func (s *Selection) removePick(row int) bool {
	at, found := slices.BinarySearch(s.picked, row)
	if found {
		s.picked = slices.Delete(s.picked, at, at+1)
	}
	return found
}

// TogglePick ticks a row, or unticks it. The row becomes the lead, so a
// ⇧-click after it fills the gap from here.
func (s *Selection) TogglePick(row int) {
	if !s.removePick(row) {
		s.insertPick(row)
	}
	s.lead = intPtr(row)
}

// PickThrough is ⇧-click in the gutter: tick every row from the last one
// ticked to this one. With no lead yet it ticks the one row, which is what a
// plain click would have done.
func (s *Selection) PickThrough(row int) {
	from := row
	if s.lead != nil {
		from = *s.lead
	}
	low, high := min(from, row), max(from, row)
	for r := low; r <= high; r++ {
		s.insertPick(r)
	}
	s.lead = intPtr(row)
}

// ToggleAllPicks is the gutter's header tick: all of them, or none.
// Ticking all of a ticked result clears it, so one target does both.
func (s *Selection) ToggleAllPicks(extent Extent) {
	if s.AllPicked(extent) {
		s.picked = nil
		s.lead = nil
	} else {
		s.picked = make([]int, 0, extent.Rows)
		for r := range extent.Rows {
			s.picked = append(s.picked, r)
		}
		if extent.Rows > 0 {
			s.lead = intPtr(extent.Rows - 1)
		} else {
			s.lead = nil
		}
	}
}

func (s *Selection) IsPicked(row int) bool {
	_, found := slices.BinarySearch(s.picked, row)
	return found
}

func (s *Selection) AllPicked(extent Extent) bool {
	return extent.Rows > 0 && len(s.picked) == extent.Rows
}

func (s *Selection) Picks() int {
	return len(s.picked)
}

// PickedRows walks the ticked rows in ascending order.
func (s *Selection) PickedRows() iter.Seq[int] {
	return func(yield func(int) bool) {
		for _, row := range s.picked {
			if !yield(row) {
				return
			}
		}
	}
}

// Clear is ⎋ and every new result: nothing marked.
func (s *Selection) Clear() {
	*s = Selection{}
}

// Summary is what the status strip says about the selection, or nothing
// when there is nothing worth saying — a single focused cell is not news.
func (s *Selection) Summary() (string, bool) {
	var parts []string
	switch n := len(s.picked); n {
	case 0:
	case 1:
		parts = append(parts, "1 row picked")
	default:
		parts = append(parts, fmt.Sprintf("%d rows picked", n))
	}
	if rect, ok := s.Rect(); ok && rect.Cells() > 1 {
		parts = append(parts, fmt.Sprintf("%d × %d cells", rect.Rows(), rect.Columns()))
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, " · "), true
}

// ClipboardText is what ⌘C and "copy row" put on the clipboard: **CSV**,
// one line per row.
//
// **Ticked rows win over the range, and they are copied whole.** Ticking
// rows says "these records", so cutting them down to whatever rectangle
// happened to be drawn as well would answer a question nobody asked.
//
// No header line either way. A copy pastes back exactly what was marked,
// and a heading nobody marked is a surprise wherever it lands.
//
// A NULL copies as an empty field. It is an absence, and pasting the word
// `NULL` into a sheet would make it data.
func ClipboardText(data *GridData, selection *Selection) (string, bool) {
	if selection.Picks() > 0 {
		var lines []string
		for row := range selection.PickedRows() {
			if row < 0 || row >= len(data.Rows) {
				continue
			}
			values := data.Rows[row]
			fields := make([]string, 0, len(values))
			for _, value := range values {
				fields = append(fields, field(value))
			}
			lines = append(lines, strings.Join(fields, separator))
		}
		if len(lines) == 0 {
			return "", false
		}
		return strings.Join(lines, "\n"), true
	}

	rect, ok := selection.Rect()
	if !ok {
		return "", false
	}
	var lines []string
	for row := rect.Top; row <= rect.Bottom; row++ {
		if row >= len(data.Rows) {
			continue
		}
		values := data.Rows[row]
		fields := make([]string, 0, rect.Columns())
		for column := rect.Left; column <= rect.Right; column++ {
			if column < len(values) {
				fields = append(fields, field(values[column]))
			} else {
				fields = append(fields, "")
			}
		}
		lines = append(lines, strings.Join(fields, separator))
	}
	if len(lines) == 0 {
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

// separator is what separates one field from the next. A comma, so what
// lands on the clipboard is a CSV row: the thing that can be pasted into a
// file, a `COPY … FROM stdin`, or a ticket, and still read as the row it
// came from.
const separator = ","

// field is one value as one CSV field, by RFC 4180's rules.
//
// A value holding the separator, a quote or a line break is wrapped in
// quotes with its own quotes doubled. Without that, one cell with a comma
// in it would paste as two fields, and one with a newline as two rows,
// shearing everything below it out of line.
func field(value dbclient.Value) string {
	if _, null := value.(dbclient.ValueNull); null {
		return ""
	}
	text := value.Display()
	if strings.ContainsAny(text, ",\n\r\"") {
		return "\"" + strings.ReplaceAll(text, "\"", "\"\"") + "\""
	}
	return text
}
