package main

// Editing a result in place: the query tab's half.
//
// **The result the server sent is never changed under the user.** The tab
// keeps it as `ResultEdit.base`, and what the grid paints is that plus
// what is staged (`resultsgrid.Edits.Display`), so a ⌘Z or a discard is a
// matter of forgetting, never of reading anything back.
//
// Whether a result takes an edit is the server's to say: the run carries
// each column's origin, and the shell reads the one table they name from
// the catalog on the app pool, after the grid is already on screen — the
// way the server's timing fills in. Until that answers, nothing edits.
//
// **The apply goes down the tab's own session**, because that is where
// the user's transaction is: auto mode commits it as one transaction,
// manual mode puts it inside the open one. A run in flight holds that
// connection, so an apply is refused then, as a commit is.

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/fuzzy"
	"github.com/nick318/meerkat/internal/resultsgrid"
	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// ResultEdit is a query tab's result as something to edit.
type ResultEdit struct {
	// The result as the server sent it, or as the last apply left it.
	base *resultsgrid.GridData
	// Where each column came from, off the run's own description.
	origins []dbclient.ColumnOrigin
	// The cells the memory cap cut, by the base's rows.
	cut map[dbclient.CellAt]bool
	// The table the origins name, once the catalog has answered; nil
	// before that, and nil after it when no such table is visible.
	target *dbclient.EditTarget
	loaded bool
	// What is staged.
	edits resultsgrid.Edits
	// The editor, while one is open on a cell.
	cell *CellEditor
	// An apply is out.
	applying bool
	// What the last apply answered, until the next change.
	answer *ApplyAnswer
}

// CellEditor is the field open over one cell.
type CellEditor struct {
	// The display cell it is open on.
	cell  resultsgrid.Cell
	field *uikit.TextField
	// What the cell held when the editor opened, for the status strip.
	was dbclient.Value
	// The column's name and type, for the status strip.
	column dbclient.EditColumn
	// Which of the enum's matching labels ↑↓ have reached.
	pick int
}

// ApplyAnswer is what the change bar says after an apply.
type ApplyAnswer struct {
	refused  bool
	conflict bool
	// The server's words, or the conflict's.
	message string
	// Which statement was refused, counting from 1, of how many.
	statement, of int
	// The apply opened its own transaction and committed it.
	committed bool
	counts    resultsgrid.Counts
	// The keys of the rows the server inserted, as a person reads them.
	inserted []string
	ms       uint64
	// The statements as they were sent, with literals, for "copy the SQL".
	sql []string
}

// EditGuard is the question a run asks over staged changes.
type EditGuard struct {
	tab   uint64
	focus ui.Handle
}

// ApplyPreview is the dialog that shows the statements before they go.
type ApplyPreview struct {
	tab   uint64
	sql   []string
	focus ui.Handle
}

// landEditable keeps what a landed result needs to be edited, and asks
// the catalog about its table. Every run replaces it, staged edits and
// all — the guard on ⌘⏎ is what made sure nothing staged is lost.
func (s *Shell) landEditable(tab *QueryTab, result dbclient.QueryResult) {
	tab.edit = nil
	if len(result.Columns) == 0 || len(result.Origins) == 0 {
		return
	}
	tables := dbclient.SourceTables(result.Origins)
	if len(tables) == 0 {
		return
	}
	tab.edit = &ResultEdit{base: tab.data, origins: result.Origins, cut: result.Cut}
	if len(tables) == 1 {
		s.readEditTarget(tab.id, tab.generation, tables[0])
	}
}

// readEditTarget reads the table on the app pool, off the run's path.
func (s *Shell) readEditTarget(tabID, generation uint64, oid uint32) {
	connection := s.connection
	if connection == nil {
		return
	}
	go func() {
		if !s.alive() {
			return
		}
		target, err := flatten(joinTask(func() (*dbclient.EditTarget, error) {
			return connection.EditTarget(context.Background(), oid)
		}))
		s.update(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok || tab.generation != generation || tab.edit == nil {
				return
			}
			tab.edit.loaded = true
			if err == nil {
				tab.edit.target = target
			}
		})
	}()
}

// editRule is whether the tab's result takes an edit, and per column what
// each one is.
func (s *Shell) editRule(tab *QueryTab) dbclient.Editability {
	e := tab.edit
	if e == nil {
		return dbclient.Editability{Blocker: dbclient.BlockedNoTable}
	}
	if e.loaded && e.target == nil && len(dbclient.SourceTables(e.origins)) == 1 {
		return dbclient.Editability{Blocker: dbclient.BlockedGone}
	}
	return dbclient.Editable(e.origins, e.target, s.readOnly, false)
}

// restage paints what is staged: the grid reads `tab.data`.
func (tab *QueryTab) restage() {
	if tab.edit != nil {
		tab.data = tab.edit.edits.Display(tab.edit.base)
	}
}

// staged says the tab holds changes not yet applied.
func (tab *QueryTab) staged() int {
	if tab.edit == nil {
		return 0
	}
	return tab.edit.edits.Counts().Changes()
}

// changeBarUp says ⌘S and ⇧⌘R answer the change bar rather than the
// transaction: something is staged, or the last apply was refused and
// its edits are still waiting.
func (tab *QueryTab) changeBarUp() bool {
	return tab.edit != nil && (!tab.edit.edits.Empty() || (tab.edit.answer != nil && tab.edit.answer.refused))
}

// activeEdit is the active query tab and its rule, when it has a result
// that could be edited.
func (s *Shell) activeEdit() (*QueryTab, dbclient.Editability, bool) {
	tab, ok := s.activeTab().(*QueryTab)
	if !ok || tab.edit == nil {
		return nil, dbclient.Editability{}, false
	}
	return tab, s.editRule(tab), true
}

// cellEditable says whether a display cell takes an edit now.
func cellEditable(tab *QueryTab, rule dbclient.Editability, cell resultsgrid.Cell) bool {
	e := tab.edit
	if e == nil || !rule.OK() || e.applying || tab.run.inFlight() || tab.txEnding {
		return false
	}
	if cell.Column < 0 || cell.Column >= len(rule.Columns) || !rule.Columns[cell.Column].Editable {
		return false
	}
	if cell.Row < 0 || cell.Row >= len(tab.data.Rows) {
		return false
	}
	ref := e.edits.Ref(cell.Row)
	if !ref.Added && (e.edits.IsDeleted(ref.Index) || e.cut[dbclient.CellAt{Row: ref.Index, Column: cell.Column}]) {
		return false
	}
	return true
}

// editText is a value as the editor shows it: the text the server sent,
// or nothing for NULL.
func editText(value dbclient.Value) string {
	if _, null := value.(dbclient.ValueNull); null {
		return ""
	}
	return dbclient.FullText(value)
}

// editCell is ⏎ in the grid: the editor on an editable cell, the value
// card on any other.
func (s *Shell) editCell() {
	cell, ok := s.activeCursor()
	if !ok {
		return
	}
	tab, rule, ok := s.activeEdit()
	if !ok || !cellEditable(tab, rule, cell) {
		s.openPeek(cell)
		return
	}
	s.openCellEditor(tab, rule, cell, "", false)
}

// startTyping is a character typed on the grid: the editor, with that
// character in place of the value.
func (s *Shell) startTyping(text string) {
	cell, ok := s.activeCursor()
	if !ok {
		return
	}
	tab, rule, ok := s.activeEdit()
	if !ok || !cellEditable(tab, rule, cell) || tab.edit.cell != nil {
		return
	}
	s.openCellEditor(tab, rule, cell, text, true)
}

// openCellEditor opens the field over a cell. `replace` starts it on
// `typed` rather than on what the cell holds.
func (s *Shell) openCellEditor(tab *QueryTab, rule dbclient.Editability, cell resultsgrid.Cell, typed string, replace bool) {
	e := tab.edit
	ref := e.edits.Ref(cell.Row)
	current, _ := e.edits.Value(ref, cell.Column, e.base.Rows)
	column := rule.Target.Columns[rule.Columns[cell.Column].Attnum]
	// A boolean edits on three keys, and the key is the whole edit.
	if column.Bool && replace {
		if value, ok := boolKey(typed); ok {
			s.stageCell(tab, cell, value)
			return
		}
	}
	text := typed
	if !replace && !e.edits.IsDefault(ref, cell.Column) {
		text = editText(current)
	}
	field := uikit.NewTextField("").Bare(theme.SizeBody)
	field.SetText(text)
	tabID := tab.id
	field.Subscribe(func(f *uikit.TextField, ev uikit.TextFieldEvent) {
		switch ev {
		case uikit.TextFieldSubmitted:
			s.keepCellEditor(tabID, 0)
		case uikit.TextFieldNextFieldEvent:
			s.keepCellEditor(tabID, 1)
		case uikit.TextFieldCancelled:
			s.cancelCellEditor(tabID)
		case uikit.TextFieldChanged:
			s.cellEditorChanged(tabID)
		}
	})
	e.cell = &CellEditor{cell: cell, field: field, was: current, column: column}
	s.columnFind = nil
	s.peek = nil
	field.FocusHandle().Focus()
	if column.Bool && replace {
		s.cellEditorChanged(tabID)
	}
}

// boolKey reads the three keys a boolean takes.
func boolKey(text string) (dbclient.Value, bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "t", "true":
		return dbclient.ValueText("true"), true
	case "f", "false":
		return dbclient.ValueText("false"), true
	case "n", "null":
		return dbclient.ValueNull{}, true
	}
	return nil, false
}

// enumMatches is the labels the editor's text matches, by the `fuzzy`
// rule every other name search in the app uses, best first.
func enumMatches(labels []string, text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return labels
	}
	pattern := fuzzy.NewPattern(text)
	type scored struct {
		label string
		score int32
	}
	var hits []scored
	for _, label := range labels {
		if match := pattern.Score(label); match != nil {
			hits = append(hits, scored{label, match.Score})
		}
	}
	// Stable, so labels the query cannot tell apart keep the type's order.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].score > hits[j-1].score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	out := make([]string, len(hits))
	for i, hit := range hits {
		out[i] = hit.label
	}
	return out
}

// editorTab is the tab whose editor is open, by id.
func (s *Shell) editorTab(tabID uint64) (*QueryTab, *CellEditor) {
	tab, ok := s.tabMut(tabID).(*QueryTab)
	if !ok || tab.edit == nil || tab.edit.cell == nil {
		return nil, nil
	}
	return tab, tab.edit.cell
}

// cellEditorChanged is a keystroke in the editor: a boolean takes its key
// at once, an enum starts its pick over.
func (s *Shell) cellEditorChanged(tabID uint64) {
	tab, ed := s.editorTab(tabID)
	if ed == nil {
		return
	}
	if ed.column.Bool {
		if value, ok := boolKey(ed.field.Text()); ok && len(strings.TrimSpace(ed.field.Text())) == 1 {
			tab.edit.cell = nil
			s.stageCell(tab, ed.cell, value)
			s.gridFocus.Focus()
			return
		}
	}
	ed.pick = 0
}

// stepPick is ↑↓ in an enum's editor.
func (s *Shell) stepPick(forward bool) {
	tab, ok := s.activeTab().(*QueryTab)
	if !ok || tab.edit == nil || tab.edit.cell == nil {
		return
	}
	ed := tab.edit.cell
	matches := enumMatches(ed.column.Labels, ed.field.Text())
	if len(matches) == 0 {
		return
	}
	if forward {
		ed.pick = (ed.pick + 1) % len(matches)
	} else {
		ed.pick = (ed.pick - 1 + len(matches)) % len(matches)
	}
}

// keepCellEditor stages what the editor holds and closes it. `move` is
// how many editable columns to step right (negative: left).
func (s *Shell) keepCellEditor(tabID uint64, move int) {
	tab, ed := s.editorTab(tabID)
	if ed == nil {
		return
	}
	text := ed.field.Text()
	var value dbclient.Value = dbclient.ValueText(text)
	if labels := ed.column.Labels; labels != nil && !containsString(labels, text) {
		if matches := enumMatches(labels, text); len(matches) > 0 {
			value = dbclient.ValueText(matches[ed.pick%len(matches)])
		}
	}
	if ed.column.Bool {
		if v, ok := boolKey(text); ok {
			value = v
		}
	}
	tab.edit.cell = nil
	s.stageCell(tab, ed.cell, value)
	if move != 0 {
		s.stepEditable(tab, move)
	}
	s.gridFocus.Focus()
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// stepEditable moves the cursor to the next editable column of its row.
func (s *Shell) stepEditable(tab *QueryTab, move int) {
	cursor, ok := tab.selection.Cursor()
	if !ok {
		return
	}
	rule := s.editRule(tab)
	column := cursor.Column
	for range len(rule.Columns) {
		column += move
		if column < 0 || column >= len(rule.Columns) {
			return
		}
		if rule.Columns[column].Editable {
			tab.selection.Focus(resultsgrid.NewCell(cursor.Row, column))
			tab.scroll.Reveal(resultsgrid.NewCell(cursor.Row, column), tab.data)
			return
		}
	}
}

// cancelCellEditor is ⎋ in the editor: the old value stays.
func (s *Shell) cancelCellEditor(tabID uint64) {
	tab, ed := s.editorTab(tabID)
	if ed == nil {
		return
	}
	tab.edit.cell = nil
	s.gridFocus.Focus()
}

// stageCell stages one value. An edit that reads back as what the result
// holds is no edit: `5` typed over the integer 5 changes nothing.
func (s *Shell) stageCell(tab *QueryTab, cell resultsgrid.Cell, value dbclient.Value) {
	e := tab.edit
	ref := e.edits.Ref(cell.Row)
	if ref.Added {
		e.edits.Set(ref, cell.Column, value, nil)
	} else {
		original := e.base.Rows[ref.Index][cell.Column]
		_, wasNull := original.(dbclient.ValueNull)
		_, isNull := value.(dbclient.ValueNull)
		same := wasNull == isNull && (isNull || editText(original) == editText(value))
		_, staged := e.edits.Value(ref, cell.Column, e.base.Rows)
		switch {
		case same && staged:
			e.edits.Revert(ref, cell.Column)
		case same:
			return
		default:
			e.edits.Set(ref, cell.Column, value, original)
		}
	}
	e.answer = nil
	tab.restage()
}

// setNull is ⌥⌘N: NULL into the editor's cell, or the cursor's.
func (s *Shell) setNull() {
	tab, rule, ok := s.activeEdit()
	if !ok {
		return
	}
	if ed := tab.edit.cell; ed != nil {
		tab.edit.cell = nil
		s.stageCell(tab, ed.cell, dbclient.ValueNull{})
		s.gridFocus.Focus()
		return
	}
	cell, ok := tab.selection.Cursor()
	if !ok || !cellEditable(tab, rule, cell) {
		return
	}
	s.stageCell(tab, cell, dbclient.ValueNull{})
}

// addRow is ⌘I: a row pinned under the header, the editor on its first
// editable cell.
func (s *Shell) addRow() {
	tab, rule, ok := s.activeEdit()
	if !ok || !rule.OK() || tab.edit.applying || tab.run.inFlight() {
		return
	}
	tab.edit.cell = nil
	tab.edit.edits.AddRow(len(tab.data.Columns))
	tab.edit.answer = nil
	tab.restage()
	tab.selection.Clear()
	first := 0
	for i, role := range rule.Columns {
		if role.Editable {
			first = i
			break
		}
	}
	cell := resultsgrid.NewCell(0, first)
	tab.selection.Focus(cell)
	tab.scroll.Reveal(cell, tab.data)
	s.gridFocus.Focus()
	if cellEditable(tab, rule, cell) {
		s.openCellEditor(tab, rule, cell, "", false)
	}
}

// deleteRows is ⌘⌫: the ticked rows, or the cursor's, marked for
// deletion — or unmarked, when they all are already.
func (s *Shell) deleteRows() {
	tab, rule, ok := s.activeEdit()
	if !ok || !rule.OK() || tab.edit.applying || tab.run.inFlight() {
		return
	}
	var rows []int
	if tab.selection.Picks() > 0 {
		for row := range tab.selection.PickedRows() {
			rows = append(rows, row)
		}
	} else if cursor, ok := tab.selection.Cursor(); ok {
		rows = append(rows, cursor.Row)
	}
	refs := make([]resultsgrid.RowRef, 0, len(rows))
	dropsAdded := false
	for _, row := range rows {
		if row < len(tab.data.Rows) {
			ref := tab.edit.edits.Ref(row)
			dropsAdded = dropsAdded || ref.Added
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return
	}
	tab.edit.cell = nil
	tab.edit.edits.ToggleDelete(refs)
	tab.edit.answer = nil
	tab.restage()
	// Dropping an added row moves every row under it up, and a mark that
	// points at rows by index would point at the wrong ones.
	if dropsAdded {
		tab.selection.Clear()
	}
}

// undoEdit and redoEdit are ⌘Z and ⇧⌘Z in the grid.
func (s *Shell) undoEdit(forward bool) {
	tab, _, ok := s.activeEdit()
	if !ok || tab.edit.applying {
		return
	}
	before := tab.edit.edits.AddedRows()
	moved := false
	if forward {
		moved = tab.edit.edits.Redo()
	} else {
		moved = tab.edit.edits.Undo()
	}
	if !moved {
		return
	}
	tab.edit.cell = nil
	tab.edit.answer = nil
	tab.restage()
	if tab.edit.edits.AddedRows() != before {
		tab.selection.Clear()
	}
}

// pasteCells is ⌘V in the grid: CSV — the format ⌘C writes — staged into
// the range from the cursor, as far as the result's rows go. A cell that
// takes no edit is skipped; nothing is added.
func (s *Shell) pasteCells() {
	tab, rule, ok := s.activeEdit()
	if !ok || !rule.OK() {
		return
	}
	cursor, ok := tab.selection.Cursor()
	if !ok {
		return
	}
	reader := csv.NewReader(strings.NewReader(mygo.Clipboard.ReadText()))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	records, err := reader.ReadAll()
	if err != nil || len(records) == 0 {
		return
	}
	tab.edit.cell = nil
	for r, record := range records {
		for c, field := range record {
			cell := resultsgrid.NewCell(cursor.Row+r, cursor.Column+c)
			if !cellEditable(tab, rule, cell) {
				continue
			}
			// An empty field is what ⌘C writes for NULL.
			var value dbclient.Value = dbclient.ValueText(field)
			if field == "" {
				value = dbclient.ValueNull{}
			}
			s.stageCell(tab, cell, value)
		}
	}
}

// discardEdits is ⇧⌘R on the change bar: everything staged goes.
func (s *Shell) discardEdits() {
	tab, ok := s.activeTab().(*QueryTab)
	if !ok || tab.edit == nil || tab.edit.applying {
		return
	}
	tab.edit.edits.Clear()
	tab.edit.cell = nil
	tab.edit.answer = nil
	tab.restage()
	tab.selection.Clear()
}

// buildApply is what an apply of the tab would send.
func (s *Shell) buildApply(tab *QueryTab) (dbclient.Editability, []dbclient.RowChange, []resultsgrid.RowRef, []int16, []int, error) {
	rule := s.editRule(tab)
	if !rule.OK() {
		return rule, nil, nil, nil, nil, errors.New(editBlockerCopy(rule))
	}
	changes, refs, err := tab.edit.edits.Build(tab.edit.base.Rows, tab.edit.cut, rule)
	returning, at := resultsgrid.Returning(rule)
	return rule, changes, refs, returning, at, err
}

// openPreview is "preview SQL" on the change bar.
func (s *Shell) openPreview() {
	tab, ok := s.activeTab().(*QueryTab)
	if !ok || tab.edit == nil || tab.edit.edits.Empty() || s.connection == nil {
		return
	}
	rule, changes, _, returning, _, err := s.buildApply(tab)
	if err != nil {
		message := err.Error()
		tab.error = &message
		return
	}
	sql := s.connection.PreviewEdit(rule.Target, changes, returning)
	s.preview = &ApplyPreview{tab: tab.id, sql: sql}
	s.preview.focus.Focus()
}

func (s *Shell) closePreview() {
	s.preview = nil
	s.gridFocus.Focus()
}

// applyEdits is ⌘S on the change bar. `then` runs once the apply has
// landed and worked — the re-run guard's "apply, then run".
func (s *Shell) applyEdits(then func()) {
	tab, ok := s.activeTab().(*QueryTab)
	if !ok || tab.edit == nil || tab.edit.edits.Empty() || tab.edit.applying {
		return
	}
	if tab.run.inFlight() || tab.txEnding {
		message := runHoldsTheSession
		tab.error = &message
		return
	}
	connection := s.connection
	if connection == nil {
		message := notConnectedCopy(s.status)
		tab.error = &message
		return
	}
	if tab.edit.cell != nil {
		s.keepCellEditor(tab.id, 0)
	}
	rule, changes, refs, returning, at, err := s.buildApply(tab)
	if err != nil {
		tab.edit.answer = &ApplyAnswer{refused: true, message: err.Error()}
		return
	}
	session := tab.session
	if session == nil && !s.makeRoomForSession() {
		message := noSessionLeft
		tab.error = &message
		return
	}
	e := tab.edit
	e.applying = true
	e.answer = nil
	tab.error = nil
	s.preview = nil
	tabID, generation := tab.id, tab.generation
	counts := e.edits.Counts()
	target := rule.Target
	sql := connection.PreviewEdit(target, changes, returning)
	started := time.Now()
	go func() {
		if !s.alive() {
			return
		}
		opened := false
		if session == nil {
			fresh, err := flatten(joinTask(func() (dbclient.Session, error) {
				return connection.OpenSession(context.Background())
			}))
			if err != nil {
				s.update(func() {
					if tab, ok := s.tabMut(tabID).(*QueryTab); ok && tab.edit != nil {
						tab.edit.applying = false
						tab.edit.answer = &ApplyAnswer{refused: true, message: err.Error(), of: len(changes)}
					}
				})
				return
			}
			session, opened = fresh, true
		}
		// Not `flatten`: it turns the error into its text, and the bar
		// needs the `ApplyError` itself — which statement, and whether it
		// was a conflict — to number the refusal and mark the row.
		outcome, err, join := joinTask(func() (dbclient.ApplyOutcome, error) {
			return session.Apply(context.Background(), target, changes, returning)
		})
		if join != nil {
			err = fmt.Errorf("the apply was interrupted: %v", join)
		}
		s.updateOr(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok {
				if opened {
					dropSession(session)
				}
				return
			}
			if opened {
				tab.session = session
				tab.sessionEnded = false
			}
			tab.lastUsed = time.Now()
			elapsed := uint64(time.Since(started).Milliseconds())
			statement := strings.Join(sql, "\n")
			if tab.generation != generation || tab.edit == nil {
				return
			}
			e := tab.edit
			e.applying = false
			if err != nil {
				e.answer = refusedAnswer(err, len(changes), sql)
				var refused *dbclient.ApplyError
				if errors.As(err, &refused) && refused.Change < len(refs) {
					e.edits.SetFault(faultOf(refused, changes[refused.Change], refs[refused.Change], rule))
				}
				message := err.Error()
				s.recordRun(statement, storage.RunSourceUser, &Outcome{elapsed: &elapsed, error: &message})
				s.reloadOpenHistory()
				s.refreshTransaction(tabID)
				return
			}
			rows, cut := resultsgrid.Merge(e.base.Rows, e.cut, at, changes, refs, outcome)
			e.base = e.base.WithRows(rows)
			e.cut = cut
			e.edits.Clear()
			e.cell = nil
			tab.restage()
			tab.selection.Clear()
			affected := uint64(len(changes))
			if outcome.Committed {
				e.answer = &ApplyAnswer{
					committed: true,
					counts:    counts,
					inserted:  insertedKeys(rule, at, changes, outcome),
					ms:        elapsed,
					sql:       sql,
				}
			} else {
				// Inside the user's transaction nothing committed: the
				// transaction bar is the answer, and it counts these.
				tab.inTransaction = true
				tab.txDone = nil
				tab.txTally.add(len(changes), affected)
			}
			zero := uint64(0)
			s.recordRun(statement, storage.RunSourceUser, &Outcome{elapsed: &elapsed, rows: &zero, affected: &affected})
			s.reloadOpenHistory()
			s.refreshTransaction(tabID)
			s.startSessionTimer()
			if then != nil {
				then()
			}
		}, func() {
			if opened {
				dropSession(session)
			}
		})
	}()
}

// refusedAnswer is the change bar's words for a refused apply.
func refusedAnswer(err error, of int, sql []string) *ApplyAnswer {
	answer := &ApplyAnswer{refused: true, message: err.Error(), of: of, sql: sql}
	var refused *dbclient.ApplyError
	if errors.As(err, &refused) {
		answer.statement = refused.Change + 1
		answer.conflict = refused.Conflict
		if refused.Err != nil {
			answer.message = refused.Err.Error()
		} else {
			answer.message = "the row is no longer there: deleted, or its key changed"
		}
	}
	return answer
}

// faultOf marks the row the server refused — the cell, when the refusal
// can only be about one.
func faultOf(refused *dbclient.ApplyError, change dbclient.RowChange, ref resultsgrid.RowRef, rule dbclient.Editability) *resultsgrid.Fault {
	fault := &resultsgrid.Fault{Row: ref, Column: -1, Conflict: refused.Conflict}
	if refused.Err != nil {
		fault.Message = refused.Err.Error()
	}
	if update, ok := change.(dbclient.RowChangeUpdate); ok && !refused.Conflict && len(update.Set) == 1 {
		for column, role := range rule.Columns {
			if role.Attnum == update.Set[0].Column {
				fault.Column = column
				break
			}
		}
	}
	return fault
}

// insertedKeys is the key of every row the server inserted, as a person
// reads it: `1053`, or `a = 1, b = 2` for a key of two columns.
func insertedKeys(rule dbclient.Editability, at []int, changes []dbclient.RowChange, outcome dbclient.ApplyOutcome) []string {
	var keys []string
	for i, change := range changes {
		if _, ok := change.(dbclient.RowChangeInsert); !ok || outcome.Rows[i] == nil {
			continue
		}
		parts := make([]string, 0, len(rule.Key))
		for _, column := range rule.Key {
			if at[column] >= 0 {
				parts = append(parts, outcome.Rows[i][at[column]].Display())
			}
		}
		keys = append(keys, strings.Join(parts, ", "))
	}
	return keys
}

// reloadRow is the conflict's "reload row": the row as the server holds
// it now, and the staged edits of that row dropped.
func (s *Shell) reloadRow() {
	tab, rule, ok := s.activeEdit()
	if !ok || !rule.OK() || tab.edit.applying {
		return
	}
	e := tab.edit
	fault := e.edits.Fault()
	if fault == nil || fault.Row.Added || tab.session == nil {
		return
	}
	row := fault.Row.Index
	key := make([]dbclient.AttrValue, 0, len(rule.Key))
	for _, column := range rule.Key {
		key = append(key, dbclient.AttrValue{Column: rule.Columns[column].Attnum, Value: e.base.Rows[row][column]})
	}
	returning, at := resultsgrid.Returning(rule)
	session, target := tab.session, rule.Target
	tabID, generation := tab.id, tab.generation
	e.applying = true
	go func() {
		values, err := flatten(joinTask(func() ([]dbclient.Value, error) {
			return session.Reload(context.Background(), target, key, returning)
		}))
		s.update(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok || tab.generation != generation || tab.edit == nil {
				return
			}
			e := tab.edit
			e.applying = false
			if err != nil {
				message := err.Error()
				tab.error = &message
				return
			}
			e.edits.SetFault(nil)
			e.answer = nil
			// The row's own edits were made against what it held; the
			// user reads the new value and decides again.
			for column := range tab.data.Columns {
				if _, staged := e.edits.Value(resultsgrid.RowRef{Index: row}, column, e.base.Rows); staged {
					e.edits.Revert(resultsgrid.RowRef{Index: row}, column)
				}
			}
			rows := make([][]dbclient.Value, len(e.base.Rows))
			copy(rows, e.base.Rows)
			if values == nil {
				// Gone on the server: a delete of it would fail too.
				e.edits.ToggleDelete([]resultsgrid.RowRef{{Index: row}})
			} else {
				fresh := make([]dbclient.Value, len(rows[row]))
				copy(fresh, rows[row])
				for column := range fresh {
					if at[column] >= 0 {
						fresh[column] = values[at[column]]
					}
				}
				rows[row] = fresh
			}
			e.base = e.base.WithRows(rows)
			tab.restage()
		})
	}()
}

// copyAppliedSQL is "copy the SQL" on an applied bar.
func (s *Shell) copyAppliedSQL() {
	tab, ok := s.activeTab().(*QueryTab)
	if !ok || tab.edit == nil || tab.edit.answer == nil {
		return
	}
	writeClipboard(strings.Join(tab.edit.answer.sql, "\n\n"))
}

// previewIntoTab is the preview's "copy into a new query tab".
func (s *Shell) previewIntoTab() {
	preview := s.preview
	if preview == nil {
		return
	}
	s.preview = nil
	s.newQueryWith("BEGIN;\n" + strings.Join(preview.sql, "\n") + "\nCOMMIT;\n")
}

// guardRun is ⌘⏎ over staged changes: true when the run may go, false when
// the question is up.
func (s *Shell) guardRun(tab *QueryTab) bool {
	if tab.staged() == 0 {
		return true
	}
	if s.editGuard != nil {
		return false
	}
	s.columnFind = nil
	s.peek = nil
	s.editGuard = &EditGuard{tab: tab.id}
	s.editGuard.focus.Focus()
	return false
}

// answerEditGuard is the re-run question's three answers.
func (s *Shell) answerEditGuard(apply, run bool) {
	guard := s.editGuard
	s.editGuard = nil
	if guard == nil {
		return
	}
	tab, ok := s.tabMut(guard.tab).(*QueryTab)
	if !ok {
		return
	}
	switch {
	case apply:
		s.applyEdits(func() { s.runActiveQuery() })
	case run:
		tab.edit.edits.Clear()
		tab.edit.cell = nil
		tab.restage()
		s.runActiveQuery()
	default:
		s.focusActiveTab()
	}
}

// editBlockerCopy is the toolbar chip's words for why a result takes no
// edit. Empty when nothing is worth saying: a result with no table column
// was never a candidate, and a table still being read is a moment.
func editBlockerCopy(rule dbclient.Editability) string {
	switch rule.Blocker {
	case dbclient.BlockedTables:
		return fmt.Sprintf("not editable · the result reads %d tables", rule.Tables)
	case dbclient.BlockedGone:
		return "not editable · the table is not visible to this connection"
	case dbclient.BlockedView:
		return fmt.Sprintf("not editable · %s is a view", rule.Target.Qualified())
	case dbclient.BlockedNoKey:
		return fmt.Sprintf("not editable · %s has no primary key", rule.Target.Qualified())
	case dbclient.BlockedKeyMissing:
		if len(rule.Missing) == 1 {
			return fmt.Sprintf("not editable · the key %s is not in the result", rule.Missing[0])
		}
		return fmt.Sprintf("not editable · the key columns %s are not in the result", strings.Join(rule.Missing, ", "))
	case dbclient.BlockedReadOnly:
		return "read-only connection · edits off"
	case dbclient.BlockedTxFailed:
		return "transaction failed · roll back to edit"
	}
	return ""
}

// changeBarCopy is the change bar's two lines while something is staged.
func changeBarCopy(counts resultsgrid.Counts) (title, sub string) {
	changes := counts.Changes()
	title = fmt.Sprintf("%d %s not applied", changes, plural(changes, "change", "changes"))
	var parts []string
	if counts.Cells > 0 {
		parts = append(parts, fmt.Sprintf("%d %s edited in %d %s",
			counts.Cells, plural(counts.Cells, "cell", "cells"), counts.Rows, plural(counts.Rows, "row", "rows")))
	}
	if counts.Added > 0 {
		parts = append(parts, fmt.Sprintf("%d %s added", counts.Added, plural(counts.Added, "row", "rows")))
	}
	if counts.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d %s deleted", counts.Deleted, plural(counts.Deleted, "row", "rows")))
	}
	parts = append(parts, "nothing has left this window")
	return title, strings.Join(parts, " · ")
}

// appliedCopy is the bar's two lines after an apply that committed.
func appliedCopy(answer *ApplyAnswer) (title, sub string) {
	changes := answer.counts.Changes()
	title = fmt.Sprintf("%d %s applied · committed · %s", changes, plural(changes, "change", "changes"), uikit.FormatMillis(answer.ms))
	var parts []string
	if answer.counts.Rows > 0 {
		parts = append(parts, fmt.Sprintf("%d %s updated", answer.counts.Rows, plural(answer.counts.Rows, "row", "rows")))
	}
	if answer.counts.Added > 0 {
		part := fmt.Sprintf("%d %s inserted", answer.counts.Added, plural(answer.counts.Added, "row", "rows"))
		if len(answer.inserted) == 1 {
			part += ", key " + answer.inserted[0]
		}
		parts = append(parts, part)
	}
	if answer.counts.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d %s deleted", answer.counts.Deleted, plural(answer.counts.Deleted, "row", "rows")))
	}
	parts = append(parts, "the rows on screen are what the server returned")
	return title, strings.Join(parts, " · ")
}

// refusedCopy is the bar's two lines after a refused apply.
func refusedCopy(answer *ApplyAnswer, staged int) (title, sub string) {
	switch {
	case answer.conflict:
		title = "Nothing applied · a row is no longer there"
	case answer.statement > 0:
		title = fmt.Sprintf("Nothing applied · statement %d of %d was refused", answer.statement, answer.of)
	default:
		title = "Nothing applied"
	}
	sub = fmt.Sprintf("%s · the transaction rolled back · all %d %s still staged",
		answer.message, staged, plural(staged, "change is", "changes are"))
	return title, sub
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// editStarter hands a character typed on the grid to a new cell editor.
// It is the grid's text-input client: the platform delivers text to the
// focused element, and the grid has no buffer of its own.
type editStarter struct {
	s *Shell
}

func (e *editStarter) TextForRange(ui.TextInputRange) (string, ui.TextInputRange) {
	return "", ui.TextInputRange{}
}
func (e *editStarter) Selection() ui.TextInputSelection { return ui.TextInputSelection{} }
func (e *editStarter) MarkedRange() (ui.TextInputRange, bool) {
	return ui.TextInputRange{}, false
}
func (e *editStarter) ReplaceText(_ *ui.TextInputRange, text string) { e.s.startTyping(text) }
func (e *editStarter) SetMarkedText(_ *ui.TextInputRange, text string, _ ui.TextInputRange) {
	// A composition starts the editor, which takes the rest of it.
	e.s.startTyping(text)
}
func (e *editStarter) UnmarkText() {}
func (e *editStarter) BoundsForRange(ui.TextInputRange) (ui.Rect, ui.TextInputRange, bool) {
	return ui.Rect{}, ui.TextInputRange{}, false
}
func (e *editStarter) IndexForPoint(ui.Point) (int, bool) { return 0, false }
