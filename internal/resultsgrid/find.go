// Finding a value in a result.
//
// ⌘J finds a column by its name; ⌘F finds a **cell by what it holds**. A
// result of five hundred rows is a lot of values to read for one email
// address, and the value is often all the user knows about the row they
// want.
//
// The rule is [fuzzy.Needle]'s, the SQL editor's find as well: a plain
// substring, case ignored until the needle is written in both cases.
//
// **What is searched is the value, not what the cell paints.** A cell cuts
// its value to one line and to what its lane can show, so a hit past the
// cut would be a hit the user cannot see in the grid — but it is still the
// cell they are looking for, and ⏎ on it opens the value whole. A NULL
// reads `NULL` on screen and is found by that word.
//
// The answer is cells in reading order — row by row, left to right — which
// is the order [Cell] sorts in, so the walk between hits can binary
// search it.
//
// **The work is bounded two ways, and neither is a promise.** The count
// stops at a limit, so a one-letter needle does not collect a quarter of a
// gigabyte of cells. And a needle that only grew is [RefineCells] over
// the hits it already has, because every hit of `abc` is a hit of `ab` —
// so typing a word costs one full read, on its first letter, not one per
// letter. A needle that finds nothing still reads every value once, on
// the GPUI thread; a result that large is the rare case, and the cap on a
// result's bytes is what bounds it.
//
// The limit counts from the first row, so past it the tail of the result
// is not walked to: a search that stopped at `10,000+` is one to narrow,
// not one to step through.

package resultsgrid

import (
	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/fuzzy"
)

// FindCells answers with the cells of `data` whose value holds `needle`, in
// reading order, and whether the search stopped at `limit` rather than at
// the end.
//
// The limit is what keeps a keystroke cheap. `MAX_BYTES` lets a quarter of
// a gigabyte into one result, and a one-letter needle over that finds
// something in nearly every cell: past a few thousand hits the count is
// all the user reads, and counting further buys nothing.
func FindCells(data *GridData, needle *fuzzy.Needle, limit int) ([]Cell, bool) {
	var found []Cell
	if needle.IsEmpty() {
		return found, false
	}
	columns := len(data.Columns)
	for row, values := range data.Rows {
		// A ragged row is painted only as far as the header reaches, so it
		// is searched only as far as well: a hit in a lane that is not
		// there could not be walked to.
		for column, value := range values[:min(len(values), columns)] {
			if !holds(value, needle) {
				continue
			}
			if len(found) == limit {
				return found, true
			}
			found = append(found, NewCell(row, column))
		}
	}
	return found, false
}

// RefineCells answers with the cells of `hits` that still hold `needle`,
// for a needle that grew from the one `hits` was found with. Only an
// **uncapped** answer may be refined: a capped one left cells out, and the
// longer needle may be in exactly those.
//
// Case cannot break the rule that the new hits are a subset of the old:
// a needle only turns strict by growing both cases, and a strict hit of
// `abC` is still a hit of `ab` read either way.
func RefineCells(data *GridData, hits []Cell, needle *fuzzy.Needle) []Cell {
	var out []Cell
	for _, cell := range hits {
		if cell.Row < 0 || cell.Row >= len(data.Rows) {
			continue
		}
		row := data.Rows[cell.Row]
		if cell.Column < 0 || cell.Column >= len(row) {
			continue
		}
		if holds(row[cell.Column], needle) {
			out = append(out, cell)
		}
	}
	return out
}

// holds says whether one value holds the needle. Text is read where it lies
// rather than copied: it is nearly every value in a result, and a copy of
// each per keystroke is the cost the limit is there to bound.
func holds(value dbclient.Value, needle *fuzzy.Needle) bool {
	if text, ok := value.(dbclient.ValueText); ok {
		return needle.IsIn(string(text))
	}
	return needle.IsIn(value.Display())
}
