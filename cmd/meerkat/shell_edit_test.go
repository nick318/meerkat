package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/resultsgrid"
	"github.com/nick318/meerkat/internal/uikit"
)

const usersOID uint32 = 16400

// withEditableResult is withResult, with the result's origins and the
// table they name read back, on a writable connection.
func withEditableResult(s *Shell) *QueryTab {
	tab := withResult(s)
	s.readOnly = false
	tab.edit = &ResultEdit{
		base: tab.data,
		origins: []dbclient.ColumnOrigin{
			{Table: usersOID, Column: 1}, {Table: usersOID, Column: 2}, {Table: usersOID, Column: 3},
		},
		target: &dbclient.EditTarget{
			OID: usersOID, Schema: "public", Name: "users", Kind: 'r', Key: []int16{1},
			Columns: map[int16]dbclient.EditColumn{
				1: {Name: "id", Type: "bigint"},
				2: {Name: "email", Type: "text"},
				3: {Name: "name", Type: "text"},
			},
		},
		loaded: true,
	}
	return tab
}

func TestTheChangeBarSaysWhatIsStaged(t *testing.T) {
	title, sub := changeBarCopy(resultsgrid.Counts{Cells: 3, Rows: 3, Added: 1, Deleted: 1})
	if title != "5 changes not applied" {
		t.Fatalf("title = %q", title)
	}
	if sub != "3 cells edited in 3 rows · 1 row added · 1 row deleted · nothing has left this window" {
		t.Fatalf("sub = %q", sub)
	}
	title, _ = changeBarCopy(resultsgrid.Counts{Cells: 1, Rows: 1})
	if title != "1 change not applied" {
		t.Fatalf("title = %q", title)
	}
}

func TestTheAnswerSaysWhatTheServerDid(t *testing.T) {
	title, sub := appliedCopy(&ApplyAnswer{
		committed: true, ms: 38, inserted: []string{"1053"},
		counts: resultsgrid.Counts{Cells: 3, Rows: 3, Added: 1, Deleted: 1},
	})
	if !strings.HasPrefix(title, "5 changes applied · committed · ") {
		t.Fatalf("title = %q", title)
	}
	if !strings.Contains(sub, "1 row inserted, key 1053") || !strings.Contains(sub, "3 rows updated") {
		t.Fatalf("sub = %q", sub)
	}
	title, sub = refusedCopy(&ApplyAnswer{refused: true, statement: 4, of: 7, message: `invalid input syntax for type numeric: "12,5"`}, 5)
	if title != "Nothing applied · statement 4 of 7 was refused" {
		t.Fatalf("title = %q", title)
	}
	if !strings.HasSuffix(sub, "all 5 changes are still staged") {
		t.Fatalf("sub = %q", sub)
	}
	title, _ = refusedCopy(&ApplyAnswer{refused: true, conflict: true, statement: 1, of: 1}, 1)
	if title != "Nothing applied · a row is no longer there" {
		t.Fatalf("conflict title = %q", title)
	}
}

func TestTheChipNamesWhyAResultTakesNoEdit(t *testing.T) {
	target := &dbclient.EditTarget{Schema: "public", Name: "events"}
	cases := map[string]dbclient.Editability{
		"not editable · the result reads 2 tables":                  {Blocker: dbclient.BlockedTables, Tables: 2},
		"not editable · public.events has no primary key":           {Blocker: dbclient.BlockedNoKey, Target: target},
		"not editable · the key id is not in the result":            {Blocker: dbclient.BlockedKeyMissing, Missing: []string{"id"}},
		"not editable · public.events is a view":                    {Blocker: dbclient.BlockedView, Target: target},
		"read-only connection · edits off":                          {Blocker: dbclient.BlockedReadOnly},
		"":                                                          {Blocker: dbclient.BlockedNoTable},
		"not editable · the key columns a, b are not in the result": {Blocker: dbclient.BlockedKeyMissing, Missing: []string{"a", "b"}},
	}
	for want, rule := range cases {
		if got := editBlockerCopy(rule); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestABooleanTakesThreeKeys(t *testing.T) {
	for text, want := range map[string]dbclient.Value{
		"t": dbclient.ValueText("true"), "F": dbclient.ValueText("false"), "n": dbclient.ValueNull{},
	} {
		got, ok := boolKey(text)
		if !ok || !got.Equal(want) {
			t.Errorf("%q: %v", text, got)
		}
	}
	if _, ok := boolKey("yes"); ok {
		t.Error("yes is not a key")
	}
}

func TestAnEnumOffersItsLabelsBestFirst(t *testing.T) {
	labels := []string{"free", "pro", "team", "pro_annual"}
	got := enumMatches(labels, "pr")
	if len(got) != 2 || got[0] != "pro" {
		t.Fatalf("got %v", got)
	}
	if got := enumMatches(labels, ""); len(got) != 4 {
		t.Fatalf("an empty field offers %v", got)
	}
}

func TestAnEditThatReadsBackAsTheValueIsNoEdit(t *testing.T) {
	s := newViewTestShell(nil)
	s.newQuery()
	tab := withEditableResult(s)
	s.stageCell(tab, resultsgrid.NewCell(0, 0), dbclient.ValueText("1"))
	if tab.staged() != 0 {
		t.Fatal("`1` typed over the integer 1 was staged")
	}
	s.stageCell(tab, resultsgrid.NewCell(0, 2), dbclient.ValueText("Ada King"))
	if tab.staged() != 1 || !tab.data.Rows[0][2].Equal(dbclient.ValueText("Ada King")) {
		t.Fatalf("staged = %d, painted %v", tab.staged(), tab.data.Rows[0][2])
	}
	// Typing the original back takes the edit away.
	s.stageCell(tab, resultsgrid.NewCell(0, 2), dbclient.ValueText("Ada Lovelace"))
	if tab.staged() != 0 {
		t.Fatal("an edit back to the original is still staged")
	}
}

func TestTheEditorStagesWhatItHolds(t *testing.T) {
	s := newViewTestShell(nil)
	s.newQuery()
	tab := withEditableResult(s)
	tab.selection.Focus(resultsgrid.NewCell(1, 1))
	s.editCell()
	if tab.edit.cell == nil {
		t.Fatal("⏎ on an editable cell opened no editor")
	}
	if got := tab.edit.cell.field.Text(); got != "grace@example.com" {
		t.Fatalf("the editor opened on %q", got)
	}
	tab.edit.cell.field.SetText("grace@navy.mil")
	s.keepCellEditor(tab.id, 1)
	if tab.edit.cell != nil {
		t.Fatal("the editor is still open")
	}
	if cursor, _ := tab.selection.Cursor(); cursor.Column != 2 {
		t.Fatalf("⇥ left the cursor on column %d", cursor.Column)
	}
	if !tab.data.Rows[1][1].Equal(dbclient.ValueText("grace@navy.mil")) {
		t.Fatalf("painted %v", tab.data.Rows[1][1])
	}

	// A read-only connection opens the card, as ⏎ always did.
	s.readOnly = true
	s.editCell()
	if tab.edit.cell != nil || s.peek == nil {
		t.Fatal("⏎ on a read-only connection did not open the card")
	}
}

func TestRowsAreAddedDeletedAndWalkedBack(t *testing.T) {
	s := newViewTestShell(nil)
	s.newQuery()
	tab := withEditableResult(s)
	s.addRow()
	if len(tab.data.Rows) != 4 || tab.edit.cell == nil || tab.edit.cell.cell.Row != 0 {
		t.Fatalf("rows = %d, editor = %+v", len(tab.data.Rows), tab.edit.cell)
	}
	s.cancelCellEditor(tab.id)
	tab.selection.Focus(resultsgrid.NewCell(2, 0))
	s.deleteRows()
	counts := tab.edit.edits.Counts()
	if counts.Added != 1 || counts.Deleted != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	s.undoEdit(false)
	s.undoEdit(false)
	if tab.staged() != 0 || len(tab.data.Rows) != 3 {
		t.Fatalf("after two undos: staged %d, rows %d", tab.staged(), len(tab.data.Rows))
	}
	s.undoEdit(true)
	if tab.edit.edits.AddedRows() != 1 {
		t.Fatal("redo did not bring the row back")
	}
}

func TestARunOverStagedChangesAsksFirst(t *testing.T) {
	s := newViewTestShell(nil)
	s.newQuery()
	tab := withEditableResult(s)
	s.stageCell(tab, resultsgrid.NewCell(0, 2), dbclient.ValueText("Ada King"))
	s.runActiveQuery()
	if s.editGuard == nil {
		t.Fatal("the run went over staged changes")
	}
	s.answerEditGuard(false, false)
	if s.editGuard != nil || tab.staged() != 1 {
		t.Fatal("stay lost the changes")
	}
}

func TestStagedChangesAreAtStakeOnClose(t *testing.T) {
	s := newViewTestShell(nil)
	s.newQuery()
	s.newQuery()
	tab := withEditableResult(s)
	s.stageCell(tab, resultsgrid.NewCell(0, 2), dbclient.ValueText("Ada King"))
	if s.guardClose(Close{Kind: CloseKindTab, Tab: tab.id}) {
		t.Fatal("the close did not ask")
	}
	copy := confirmCopy(s.confirm.what, s.confirm.running, s.confirm.open, s.confirm.staged...)
	if len(copy.Body) != 1 || !strings.Contains(copy.Body[0], "1 change not applied") {
		t.Fatalf("body = %q", copy.Body)
	}
}

func TestTheChangeBarTakesTheCommitKey(t *testing.T) {
	s := newViewTestShell(nil)
	s.newQuery()
	tab := withEditableResult(s)
	s.stageCell(tab, resultsgrid.NewCell(0, 2), dbclient.ValueText("Ada King"))
	if !tab.changeBarUp() {
		t.Fatal("no change bar over a staged edit")
	}
	s.handleAction(ShellRollbackTransaction)
	if tab.staged() != 0 {
		t.Fatal("⇧⌘R did not discard the staged edit")
	}
}

func TestTheShellRendersAnEditableResult(t *testing.T) {
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	s := newViewTestShell(nil)
	s.newQuery()
	tab := withEditableResult(s)
	tt := ui.NewTester(func(c *ui.Context) { s.View(c) }, 1360, 880)
	tt.Frame()
	mustFind(t, tt, "editable ·", "public.users", "+ row")
	writeShellPNG(t, tt, "20-editable")

	s.stageCell(tab, resultsgrid.NewCell(0, 2), dbclient.ValueText("Ada King"))
	s.stageCell(tab, resultsgrid.NewCell(1, 2), dbclient.ValueNull{})
	s.addRow()
	tab.edit.cell.field.SetText("new@example.com")
	tt.Frame()
	mustFind(t, tt, "3 changes not applied", "apply 3 changes", "discard", "preview SQL", "editing", "DEFAULT")
	writeShellPNG(t, tt, "21-staged-and-editing")
	s.cancelCellEditor(tab.id)

	tab.edit.answer = &ApplyAnswer{refused: true, statement: 2, of: 3, message: `invalid input syntax for type numeric: "12,5"`}
	tt.Frame()
	mustFind(t, tt, "Nothing applied · statement 2 of 3 was refused", "apply again")
	writeShellPNG(t, tt, "22-refused")
	tab.edit.answer = nil

	s.preview = &ApplyPreview{tab: tab.id, sql: []string{
		"UPDATE \"public\".\"users\" SET \"name\" = 'Ada King'\n WHERE \"id\" = 1\n   AND format('%s', \"email\") = 'ada@example.com';",
		"INSERT INTO \"public\".\"users\" DEFAULT VALUES\nRETURNING \"id\", \"email\", \"name\";",
	}}
	tt.Frame()
	mustFind(t, tt, "copy into a new query tab", "values go as parameters; shown here as literals")
	writeShellPNG(t, tt, "23-preview")
	s.preview = nil

	s.runActiveQuery()
	tt.Frame()
	mustFind(t, tt, "Run again over 3 staged changes?", "apply, then run", "discard and run")
	writeShellPNG(t, tt, "24-guard")
	s.editGuard = nil

	tab.edit.edits.Clear()
	tab.restage()
	tab.edit.answer = &ApplyAnswer{committed: true, ms: 38, counts: resultsgrid.Counts{Cells: 1, Rows: 1}}
	tt.Frame()
	mustFind(t, tt, "copy the SQL")
	writeShellPNG(t, tt, "25-applied")
}

func TestARefusalKeepsItsStatementAndRow(t *testing.T) {
	refused := &dbclient.ApplyError{Change: 0, Of: 1, Err: errors.New("function record_search_lexemes(uuid, tsvector) does not exist")}
	answer := refusedAnswer(refused, 1, nil)
	if answer.statement != 1 || answer.message != "function record_search_lexemes(uuid, tsvector) does not exist" {
		t.Fatalf("answer = %+v", answer)
	}
	title, _ := refusedCopy(answer, 1)
	if title != "Nothing applied · statement 1 of 1 was refused" {
		t.Fatalf("title = %q", title)
	}
}
