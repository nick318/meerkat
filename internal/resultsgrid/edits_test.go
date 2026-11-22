package resultsgrid

import (
	"testing"

	"github.com/nick318/meerkat/internal/dbclient"
)

func txt(s string) dbclient.Value { return dbclient.ValueText(s) }

// A result of `SELECT id, email, lower(email) FROM users`: the key, an
// editable column and an expression.
func editRule() dbclient.Editability {
	return dbclient.Editability{
		Columns: []dbclient.ColumnRole{
			{Attnum: 1, Key: true, Editable: true},
			{Attnum: 2, Editable: true},
			{Locked: "expression"},
		},
		Key: []int{0},
	}
}

func editBase() [][]dbclient.Value {
	return [][]dbclient.Value{
		{dbclient.ValueInt(1), txt("a@x.io"), txt("a@x.io")},
		{dbclient.ValueInt(2), txt("b@x.io"), txt("b@x.io")},
		{dbclient.ValueInt(3), txt("c@x.io"), txt("c@x.io")},
	}
}

func TestAnEditBackToTheOriginalIsNoEdit(t *testing.T) {
	var e Edits
	e.Set(RowRef{Index: 1}, 1, txt("new"), txt("b@x.io"))
	if e.Counts().Cells != 1 {
		t.Fatal("not staged")
	}
	e.Set(RowRef{Index: 1}, 1, txt("b@x.io"), txt("b@x.io"))
	if !e.Empty() {
		t.Fatal("still staged")
	}
}

func TestAddedRowsArePinnedAtTheTopNewestFirst(t *testing.T) {
	var e Edits
	e.AddRow(3)
	e.Set(RowRef{Added: true, Index: 0}, 1, txt("first"), nil)
	e.AddRow(3)
	if got := e.Ref(0); got != (RowRef{Added: true, Index: 0}) {
		t.Fatalf("ref(0) = %+v", got)
	}
	if got := e.Ref(2); got != (RowRef{Index: 0}) {
		t.Fatalf("ref(2) = %+v", got)
	}
	// The older row moved down one.
	if v, staged := e.Value(RowRef{Added: true, Index: 1}, 1, nil); !staged || !v.Equal(txt("first")) {
		t.Fatalf("value = %v", v)
	}
	if !e.IsDefault(RowRef{Added: true, Index: 0}, 1) {
		t.Fatal("an untouched cell is not a default")
	}
	display := e.Display(NewGridData([]string{"id", "email", "lower"}, editBase()))
	if len(display.Rows) != 5 {
		t.Fatalf("rows = %d", len(display.Rows))
	}
}

func TestDeletingTogglesAndDropsAddedRows(t *testing.T) {
	var e Edits
	e.Set(RowRef{Index: 0}, 1, txt("gone"), txt("a@x.io"))
	e.AddRow(3)
	e.ToggleDelete([]RowRef{{Index: 0}, {Added: true, Index: 0}})
	if !e.IsDeleted(0) || e.AddedRows() != 0 || e.RowEdited(0) {
		t.Fatalf("after delete: %+v", e.Counts())
	}
	e.ToggleDelete([]RowRef{{Index: 0}})
	if e.IsDeleted(0) {
		t.Fatal("not unmarked")
	}
}

func TestUndoAndRedoWalkOneChangeAtATime(t *testing.T) {
	var e Edits
	e.Set(RowRef{Index: 0}, 1, txt("one"), txt("a@x.io"))
	e.Set(RowRef{Index: 0}, 1, txt("two"), txt("a@x.io"))
	e.AddRow(3)
	e.Undo()
	if e.AddedRows() != 0 {
		t.Fatal("the row is still there")
	}
	e.Undo()
	if v, _ := e.Value(RowRef{Index: 0}, 1, editBase()); !v.Equal(txt("one")) {
		t.Fatalf("value = %v", v)
	}
	e.Redo()
	if v, _ := e.Value(RowRef{Index: 0}, 1, editBase()); !v.Equal(txt("two")) {
		t.Fatalf("value = %v", v)
	}
	e.Set(RowRef{Index: 2}, 1, txt("x"), txt("c@x.io"))
	if e.Redo() {
		t.Fatal("a new change keeps the redo")
	}
}

func TestBuildSendsUpdatesThenInsertsThenDeletes(t *testing.T) {
	var e Edits
	e.Set(RowRef{Index: 2}, 1, txt("c2@x.io"), txt("c@x.io"))
	e.AddRow(3)
	e.Set(RowRef{Added: true, Index: 0}, 1, txt("older@x.io"), nil)
	e.AddRow(3)
	e.ToggleDelete([]RowRef{{Index: 0}})

	changes, refs, err := e.Build(editBase(), nil, editRule())
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 4 {
		t.Fatalf("changes = %d", len(changes))
	}
	update := changes[0].(dbclient.RowChangeUpdate)
	if update.Key[0].Column != 1 || !update.Key[0].Value.Equal(dbclient.ValueInt(3)) {
		t.Fatalf("key = %+v", update.Key)
	}
	if len(update.Set) != 1 || update.Set[0].Column != 2 {
		t.Fatalf("set = %+v", update.Set)
	}
	// The oldest added row goes first, and an untouched row is a bare
	// insert of defaults.
	if got := changes[1].(dbclient.RowChangeInsert); len(got.Values) != 1 || refs[1].Index != 1 {
		t.Fatalf("first insert = %+v, ref %+v", got, refs[1])
	}
	if got := changes[2].(dbclient.RowChangeInsert); len(got.Values) != 0 {
		t.Fatalf("second insert = %+v", got)
	}
	if _, ok := changes[3].(dbclient.RowChangeDelete); !ok || refs[3] != (RowRef{Index: 0}) {
		t.Fatalf("delete = %+v", changes[3])
	}
}

func TestACutKeyRefusesToNameTheRow(t *testing.T) {
	var e Edits
	e.Set(RowRef{Index: 0}, 1, txt("new"), txt("a@x.io"))
	if _, _, err := e.Build(editBase(), map[dbclient.CellAt]bool{{Row: 0, Column: 1}: true}, editRule()); err != nil {
		t.Fatalf("a cut value column refused: %v", err)
	}
	if _, _, err := e.Build(editBase(), map[dbclient.CellAt]bool{{Row: 0, Column: 0}: true}, editRule()); err == nil {
		t.Fatal("a cut key was used")
	}
}

func TestMergePutsTheServersRowsInPlace(t *testing.T) {
	var e Edits
	e.Set(RowRef{Index: 1}, 1, txt("B@X.IO"), txt("b@x.io"))
	e.AddRow(3)
	e.ToggleDelete([]RowRef{{Index: 2}})
	rule := editRule()
	changes, refs, _ := e.Build(editBase(), nil, rule)
	attnums, at := Returning(rule)
	if len(attnums) != 2 || at[2] != -1 {
		t.Fatalf("returning = %v, at = %v", attnums, at)
	}
	outcome := dbclient.ApplyOutcome{Rows: [][]dbclient.Value{
		{dbclient.ValueInt(2), txt("b@x.io")}, // a trigger lowered it
		{dbclient.ValueInt(4), txt("default@x.io")},
		nil,
	}}
	rows, _ := Merge(editBase(), nil, at, changes, refs, outcome)
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	if !rows[0][0].Equal(dbclient.ValueInt(4)) {
		t.Fatalf("the inserted row is not on top: %v", rows[0])
	}
	if !rows[2][1].Equal(txt("b@x.io")) || !rows[2][2].Equal(txt("b@x.io")) {
		t.Fatalf("updated row = %v", rows[2])
	}
}
