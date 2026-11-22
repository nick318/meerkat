package resultsgrid

// How an editable result looks: the marks the edit model asks the grid to
// paint. The grid decides nothing about edits — the shell reads [Edits]
// and hands the answers over here — so a result that takes no edit paints
// exactly as it always has.
//
// **No mark takes a pixel from the content box.** A wash is a background,
// a flag is an absolute child in the corner, and the editor's ring is an
// inset line drawn over the cell, so the value never moves as a cell
// changes state — the grid's rule for the cursor and the range.

import (
	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/theme"
)

// RowMark is what an edit has made of a row.
type RowMark int

const (
	RowPlain RowMark = iota
	// RowEdited: a cell of it is staged.
	RowEdited
	// RowAdded: the user added it; it is pinned under the header.
	RowAdded
	// RowDeleted: marked for deletion.
	RowDeleted
	// RowRefused: the server refused it on the last apply.
	RowRefused
	// RowConflict: somebody changed it since it was read.
	RowConflict
)

// CellMark is what an edit has made of a cell.
type CellMark int

const (
	CellPlain CellMark = iota
	// CellStaged: a new value waits to be applied.
	CellStaged
	// CellDefault: a cell of an added row, left to the column's default.
	CellDefault
	// CellRefused: the value the server refused.
	CellRefused
	// CellLocked: the column does not take an edit.
	CellLocked
)

// ColumnMark is what a header says about its column.
type ColumnMark struct {
	Key    bool
	Locked bool
}

// EditView is everything the grid paints for an editable result.
type EditView struct {
	// Row is a display row's mark, and the number its gutter shows — 0
	// for an added row, which has none yet.
	Row func(row int) (RowMark, int)
	// Cell is a display cell's mark.
	Cell func(row, column int) CellMark
	// Columns is one mark per column; nil paints plain headers.
	Columns []ColumnMark
	// Editing is the cell the editor is open on, if any.
	Editing *Cell
	// Editor builds the editor inside that cell.
	Editor func(c *ui.Context)
}

// Edit hands the grid an editable result's marks.
func (g *Grid) Edit(view *EditView) *Grid {
	g.edit = view
	return g
}

// rowMark is a row's mark and gutter number, plain when nothing is edited.
func (v *EditView) rowMark(row int, number int) (RowMark, int) {
	if v == nil || v.Row == nil {
		return RowPlain, number
	}
	return v.Row(row)
}

func (v *EditView) cellMark(row, column int) CellMark {
	if v == nil || v.Cell == nil {
		return CellPlain
	}
	return v.Cell(row, column)
}

func (v *EditView) column(column int) ColumnMark {
	if v == nil || column >= len(v.Columns) {
		return ColumnMark{}
	}
	return v.Columns[column]
}

func (v *EditView) editing(row, column int) bool {
	return v != nil && v.Editing != nil && v.Editing.Row == row && v.Editing.Column == column
}

// rowColors is the wash and the gutter's ink for a row mark; ok is false
// for a plain row, which keeps the grid's own marks.
func rowColors(mark RowMark, colors *theme.ThemeColors) (wash, gutter, ink theme.Hsla, ok bool) {
	switch mark {
	case RowEdited:
		return theme.Hsla{}, colors.Edit, theme.Hsla{}, true
	case RowAdded:
		return colors.EditAddedSurface, colors.EditAdded, theme.Hsla{}, true
	case RowDeleted:
		return colors.EditDeletedSurface, colors.Error, colors.EditDeletedText, true
	case RowRefused:
		return theme.Hsla{}, colors.Error, theme.Hsla{}, true
	case RowConflict:
		return colors.EditConflictSurface, colors.EditConflictText, theme.Hsla{}, true
	}
	return theme.Hsla{}, theme.Hsla{}, theme.Hsla{}, false
}

// gutterGlyph is what the gutter says of a marked row.
func gutterGlyph(mark RowMark) string {
	switch mark {
	case RowEdited, RowRefused:
		return "~"
	case RowAdded:
		return "+"
	case RowDeleted:
		return "−"
	case RowConflict:
		return "!"
	}
	return ""
}

// cornerFlag is the 7px triangle in a staged cell's top-left corner: the
// mark that says "changed" in shape, for an eye that does not read the
// wash's tone.
func cornerFlag(c *ui.Context, color theme.Hsla) {
	ui.Box(c).
		Absolute().
		Left(0).
		Top(0).
		Size(7, 7).
		Draw(func(p *ui.Painter, r ui.Rect) {
			path := new(ui.Path).
				MoveTo(r.X, r.Y).
				LineTo(r.X+r.W, r.Y).
				LineTo(r.X, r.Y+r.H).
				Close()
			p.FillPath(path, color.Color())
		})
}

// chip is a small word in a well: NULL, DEFAULT.
func chip(c *ui.Context, word string, surface, ink theme.Hsla) {
	ui.Box(c).
		Shrink(0).
		PaddingX(5).
		Radius(4).
		Background(surface.Color()).
		TextColor(ink.Color()).
		FontSize(theme.SizeCaption).
		FixedLineHeight(17).
		Children(func() { ui.Text(c, word) })
}
