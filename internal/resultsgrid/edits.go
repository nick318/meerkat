package resultsgrid

// Staged edits: what the user has changed in a result and not yet applied.
//
// **Nothing here knows a window or a database.** It is the policy — which
// cell holds what, which rows were added or marked, what undo walks back,
// and what statements the lot turns into — as plain functions over
// indices, argued with in a test the way [Selection] is. The view paints
// what it reads here, and the shell is what sends it.
//
// Rows are addressed two ways. The **display** row is what the grid
// paints and the selection holds: the rows the user added, pinned under
// the header newest first, then the result's own rows. A [RowRef] is
// which of those it is, so a staged edit survives a row being added above
// it.

import (
	"fmt"
	"maps"
	"slices"

	"github.com/nick318/meerkat/internal/dbclient"
)

// RowRef names a row of an edited result.
type RowRef struct {
	// Added says the row is one the user added; Index then counts from
	// the top of the added rows. Otherwise Index is the result's own row.
	Added bool
	Index int
}

// Edits is everything staged against one result. The zero value is an
// empty set of edits.
type Edits struct {
	// New values for cells of the result's own rows, by row and column.
	cells map[Cell]dbclient.Value
	// The rows the user added, newest first. A nil cell is one left
	// untouched, which the `INSERT` leaves to the column's default.
	added [][]dbclient.Value
	// The result's own rows marked for deletion.
	deleted map[int]bool
	// What the last apply refused, until the next change.
	fault *Fault
	undo  []editState
	redo  []editState
}

// Fault is a row the server refused on the last apply.
type Fault struct {
	Row RowRef
	// The refused cell's column, or -1 when the refusal is about the row.
	Column int
	// The row is no longer there — deleted, or its key changed: the sand
	// mark, with "reload row".
	Conflict bool
	// The server's own words; empty for a conflict.
	Message string
}

type editState struct {
	cells   map[Cell]dbclient.Value
	added   [][]dbclient.Value
	deleted map[int]bool
}

// Counts is what the change bar says.
type Counts struct {
	// Cells edited, and the rows they are in.
	Cells, Rows int
	Added       int
	Deleted     int
}

// Changes is the bar's headline number: every edited cell, added row and
// deleted row is one change.
func (c Counts) Changes() int {
	return c.Cells + c.Added + c.Deleted
}

// Ref names the display row `row`.
func (e *Edits) Ref(row int) RowRef {
	if row < len(e.added) {
		return RowRef{Added: true, Index: row}
	}
	return RowRef{Index: row - len(e.added)}
}

// DisplayRow is where `ref` is painted.
func (e *Edits) DisplayRow(ref RowRef) int {
	if ref.Added {
		return ref.Index
	}
	return ref.Index + len(e.added)
}

// AddedRows is how many rows the user added.
func (e *Edits) AddedRows() int {
	return len(e.added)
}

// Empty says nothing is staged.
func (e *Edits) Empty() bool {
	return len(e.cells) == 0 && len(e.added) == 0 && len(e.deleted) == 0
}

// Counts adds up what is staged.
func (e *Edits) Counts() Counts {
	rows := map[int]bool{}
	for cell := range e.cells {
		rows[cell.Row] = true
	}
	return Counts{Cells: len(e.cells), Rows: len(rows), Added: len(e.added), Deleted: len(e.deleted)}
}

// Value is what the cell holds now: the staged value, or the result's own.
// `staged` says it was changed; a cell of an added row left untouched is
// [ValueNull] and not staged — see [Edits.IsDefault].
func (e *Edits) Value(ref RowRef, column int, base [][]dbclient.Value) (value dbclient.Value, staged bool) {
	if ref.Added {
		if ref.Index >= len(e.added) || column >= len(e.added[ref.Index]) {
			return dbclient.ValueNull{}, false
		}
		if v := e.added[ref.Index][column]; v != nil {
			return v, true
		}
		return dbclient.ValueNull{}, false
	}
	if v, ok := e.cells[NewCell(ref.Index, column)]; ok {
		return v, true
	}
	if ref.Index < len(base) && column < len(base[ref.Index]) {
		return base[ref.Index][column], false
	}
	return dbclient.ValueNull{}, false
}

// IsDefault says a cell of an added row is left to the column's default.
func (e *Edits) IsDefault(ref RowRef, column int) bool {
	return ref.Added && ref.Index < len(e.added) && column < len(e.added[ref.Index]) &&
		e.added[ref.Index][column] == nil
}

// IsDeleted says one of the result's own rows is marked for deletion.
func (e *Edits) IsDeleted(row int) bool {
	return e.deleted[row]
}

// RowEdited says one of the result's own rows has a staged cell.
func (e *Edits) RowEdited(row int) bool {
	for cell := range e.cells {
		if cell.Row == row {
			return true
		}
	}
	return false
}

// Fault is what the last apply refused, if anything.
func (e *Edits) Fault() *Fault {
	return e.fault
}

// SetFault records what the last apply refused.
func (e *Edits) SetFault(fault *Fault) {
	e.fault = fault
}

// Set stages a cell. `original` is what the result holds there; an edit
// back to it is no edit at all.
func (e *Edits) Set(ref RowRef, column int, value, original dbclient.Value) {
	e.save()
	if ref.Added {
		if ref.Index < len(e.added) && column < len(e.added[ref.Index]) {
			e.added[ref.Index][column] = value
		}
		return
	}
	cell := NewCell(ref.Index, column)
	if value.Equal(original) {
		delete(e.cells, cell)
		return
	}
	if e.cells == nil {
		e.cells = map[Cell]dbclient.Value{}
	}
	e.cells[cell] = value
}

// Revert puts one of the result's own cells back to what the result holds,
// or an added row's cell back to its default.
func (e *Edits) Revert(ref RowRef, column int) {
	e.save()
	if ref.Added {
		if ref.Index < len(e.added) && column < len(e.added[ref.Index]) {
			e.added[ref.Index][column] = nil
		}
		return
	}
	delete(e.cells, NewCell(ref.Index, column))
}

// AddRow adds an empty row at the top of the added rows: display row 0.
func (e *Edits) AddRow(columns int) {
	e.save()
	e.added = slices.Insert(e.added, 0, make([]dbclient.Value, columns))
}

// ToggleDelete marks the rows for deletion, or unmarks them when every one
// of the result's own rows among them is marked already. An added row is
// simply dropped: there is nothing on the server to delete.
func (e *Edits) ToggleDelete(refs []RowRef) {
	if len(refs) == 0 {
		return
	}
	e.save()
	all := true
	var dropped []int
	for _, ref := range refs {
		if ref.Added {
			dropped = append(dropped, ref.Index)
		} else if !e.deleted[ref.Index] {
			all = false
		}
	}
	for _, ref := range refs {
		if ref.Added {
			continue
		}
		if all {
			delete(e.deleted, ref.Index)
			continue
		}
		if e.deleted == nil {
			e.deleted = map[int]bool{}
		}
		e.deleted[ref.Index] = true
		// A row on its way out has nothing left to update.
		for cell := range e.cells {
			if cell.Row == ref.Index {
				delete(e.cells, cell)
			}
		}
	}
	slices.Sort(dropped)
	for i := len(dropped) - 1; i >= 0; i-- {
		if dropped[i] < len(e.added) {
			e.added = slices.Delete(e.added, dropped[i], dropped[i]+1)
		}
	}
}

// Undo walks back one staged change. False when there is none.
func (e *Edits) Undo() bool {
	if len(e.undo) == 0 {
		return false
	}
	e.redo = append(e.redo, e.snapshot())
	e.restore(e.undo[len(e.undo)-1])
	e.undo = e.undo[:len(e.undo)-1]
	return true
}

// Redo walks forward again. False when there is nothing to redo.
func (e *Edits) Redo() bool {
	if len(e.redo) == 0 {
		return false
	}
	e.undo = append(e.undo, e.snapshot())
	e.restore(e.redo[len(e.redo)-1])
	e.redo = e.redo[:len(e.redo)-1]
	return true
}

// Clear drops everything staged, undo included.
func (e *Edits) Clear() {
	*e = Edits{}
}

// save is the undo point before a change. Every change also forgets what
// the last apply refused: the edit may be the fix.
func (e *Edits) save() {
	e.undo = append(e.undo, e.snapshot())
	e.redo = nil
	e.fault = nil
}

func (e *Edits) snapshot() editState {
	added := make([][]dbclient.Value, len(e.added))
	for i, row := range e.added {
		added[i] = slices.Clone(row)
	}
	return editState{cells: maps.Clone(e.cells), added: added, deleted: maps.Clone(e.deleted)}
}

func (e *Edits) restore(state editState) {
	e.cells, e.added, e.deleted = state.cells, state.added, state.deleted
	e.fault = nil
}

// Display is the result as the grid paints it: the added rows first, then
// the result's own rows with their staged values. The lanes keep the
// result's widths, so a lane does not jump as a value is typed into it.
func (e *Edits) Display(base *GridData) *GridData {
	if e.Empty() {
		return base
	}
	rows := make([][]dbclient.Value, 0, len(e.added)+len(base.Rows))
	for _, added := range e.added {
		row := make([]dbclient.Value, len(added))
		for i, v := range added {
			if v == nil {
				v = dbclient.ValueNull{}
			}
			row[i] = v
		}
		rows = append(rows, row)
	}
	for r, values := range base.Rows {
		if !e.RowEdited(r) {
			rows = append(rows, values)
			continue
		}
		row := slices.Clone(values)
		for c := range row {
			if v, ok := e.cells[NewCell(r, c)]; ok {
				row[c] = v
			}
		}
		rows = append(rows, row)
	}
	return base.WithRows(rows)
}

// Returning is the table columns the apply asks back, and for each result
// column where its value is in that list (-1 when it is not a table
// column, and keeps what it showed).
func Returning(rule dbclient.Editability) (attnums []int16, at []int) {
	at = make([]int, len(rule.Columns))
	index := map[int16]int{}
	for i, role := range rule.Columns {
		at[i] = -1
		if role.Attnum <= 0 {
			continue
		}
		if j, ok := index[role.Attnum]; ok {
			at[i] = j
			continue
		}
		index[role.Attnum] = len(attnums)
		at[i] = len(attnums)
		attnums = append(attnums, role.Attnum)
	}
	return attnums, at
}

// Build turns what is staged into the changes the apply sends, in the
// order it sends them — updates, then inserts oldest first, then deletes —
// and says which row each change is about.
func (e *Edits) Build(base [][]dbclient.Value, cut map[dbclient.CellAt]bool, rule dbclient.Editability) ([]dbclient.RowChange, []RowRef, error) {
	var changes []dbclient.RowChange
	var refs []RowRef

	key := func(row int) ([]dbclient.AttrValue, error) {
		parts := make([]dbclient.AttrValue, 0, len(rule.Key))
		for _, column := range rule.Key {
			if cut[dbclient.CellAt{Row: row, Column: column}] {
				return nil, fmt.Errorf("row %d: its key is too long to name the row", row+1)
			}
			parts = append(parts, dbclient.AttrValue{Column: rule.Columns[column].Attnum, Value: base[row][column]})
		}
		return parts, nil
	}
	edited := map[int][]int{}
	for cell := range e.cells {
		edited[cell.Row] = append(edited[cell.Row], cell.Column)
	}
	rows := slices.Sorted(maps.Keys(edited))
	for _, row := range rows {
		columns := edited[row]
		slices.Sort(columns)
		var set []dbclient.AttrValue
		for _, column := range columns {
			if column < len(rule.Columns) && rule.Columns[column].Editable {
				set = append(set, dbclient.AttrValue{Column: rule.Columns[column].Attnum, Value: e.cells[NewCell(row, column)]})
			}
		}
		if len(set) == 0 {
			continue
		}
		k, err := key(row)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, dbclient.RowChangeUpdate{Key: k, Set: set})
		refs = append(refs, RowRef{Index: row})
	}
	for i := len(e.added) - 1; i >= 0; i-- {
		var values []dbclient.AttrValue
		for column, v := range e.added[i] {
			if v != nil && column < len(rule.Columns) && rule.Columns[column].Editable {
				values = append(values, dbclient.AttrValue{Column: rule.Columns[column].Attnum, Value: v})
			}
		}
		changes = append(changes, dbclient.RowChangeInsert{Values: values})
		refs = append(refs, RowRef{Added: true, Index: i})
	}
	for _, row := range slices.Sorted(maps.Keys(e.deleted)) {
		k, err := key(row)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, dbclient.RowChangeDelete{Key: k})
		refs = append(refs, RowRef{Index: row})
	}
	return changes, refs, nil
}

// Merge is the result after an apply landed: the rows the server returned
// in place of the ones the user edited, the added rows as real rows at the
// top where they were painted, the deleted rows gone. `cut` is carried
// over to the rows that stayed, and the server's own cuts are added.
func Merge(
	base [][]dbclient.Value,
	cut map[dbclient.CellAt]bool,
	at []int,
	changes []dbclient.RowChange,
	refs []RowRef,
	outcome dbclient.ApplyOutcome,
) ([][]dbclient.Value, map[dbclient.CellAt]bool) {
	width := len(at)
	returned := func(change, column int) (dbclient.Value, bool) {
		if at[column] < 0 || change >= len(outcome.Rows) || outcome.Rows[change] == nil {
			return nil, false
		}
		return outcome.Rows[change][at[column]], true
	}

	type inserted struct {
		index  int
		change int
	}
	var inserts []inserted
	updated := map[int]int{}
	deleted := map[int]bool{}
	for change, ref := range refs {
		switch changes[change].(type) {
		case dbclient.RowChangeInsert:
			inserts = append(inserts, inserted{ref.Index, change})
		case dbclient.RowChangeDelete:
			deleted[ref.Index] = true
		default:
			updated[ref.Index] = change
		}
	}
	slices.SortFunc(inserts, func(a, b inserted) int { return a.index - b.index })

	var rows [][]dbclient.Value
	var newCut map[dbclient.CellAt]bool
	mark := func(row, column int) {
		if newCut == nil {
			newCut = map[dbclient.CellAt]bool{}
		}
		newCut[dbclient.CellAt{Row: row, Column: column}] = true
	}
	for _, insert := range inserts {
		row := make([]dbclient.Value, width)
		for column := range row {
			if v, ok := returned(insert.change, column); ok {
				row[column] = v
				if outcome.Cut[dbclient.CellAt{Row: insert.change, Column: at[column]}] {
					mark(len(rows), column)
				}
			} else {
				row[column] = dbclient.ValueNull{}
			}
		}
		rows = append(rows, row)
	}
	for r, values := range base {
		if deleted[r] {
			continue
		}
		change, isUpdate := updated[r]
		row := values
		if isUpdate {
			row = slices.Clone(values)
		}
		for column := range row {
			if isUpdate {
				if v, ok := returned(change, column); ok {
					row[column] = v
					if outcome.Cut[dbclient.CellAt{Row: change, Column: at[column]}] {
						mark(len(rows), column)
					}
					continue
				}
			}
			if cut[dbclient.CellAt{Row: r, Column: column}] {
				mark(len(rows), column)
			}
		}
		rows = append(rows, row)
	}
	return rows, newCut
}
