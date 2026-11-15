// The state changes the views call: the sidebar filter, the palette, the
// column find, the result find, the peek, the tab strip and every action
// handler (shell.rs, agent K3a's part — see docs/tasks/shell-split.md).
//
// # How a view reaches these
//
//   - **Actions go through one entry point.** GPUI hung a listener per
//     action on the shell's element (`.on_action(cx.listener(Self::on_x))`)
//     and let a keystroke matched in any deeper context bubble up to it.
//     Here a view dispatches the key table of the element that wore the
//     context — `uikit.DispatchKeys(el, table)` — and hands whatever
//     ShellAction comes back to [Shell.handleAction]; a PaletteAction goes
//     to [Shell.handlePaletteAction]. Each case is one Rust `on_*`
//     listener: a one-line listener is inlined in the switch, and the ones
//     with a body of their own keep it as an `onXxx` method with no
//     parameters (onSelectAll, onCopySelection, …). Every ShellAction has a
//     case, so any table may route into it.
//   - **TextField events** arrive through `Subscribe`, wired where the
//     field is made: onFilterEvent (NewShell), onPaletteEvent,
//     onColumnFindEvent and onResultFindEvent (here).
//   - **Mouse state** — the divider drag, the peek's marking — takes its
//     positions from the view. onDividerDown and dragPaneTo take the
//     pointer in **window** coordinates: mygo reports a move relative to
//     the element that took the press, and the divider's handle moves with
//     the size it drags, so the view adds the handle's origin back in.
//   - The window and `cx` parameters are gone (shell_types.go's shape
//     decisions); `window.focus(&h)` is `h.Focus()`, and
//     `cx.write_to_clipboard` is `mygo.Clipboard.WriteText`.

package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/fuzzy"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/query"
	"github.com/nick318/meerkat/internal/resultsgrid"
	"github.com/nick318/meerkat/internal/sqleditor"
	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/uikit"
)

// resultFindKeys is the strip's own table, built once: the strip
// dispatches it every frame it is on screen.
var resultFindKeys = ResultFindKeyBindings()

// activeTab is `self.tabs.get(self.active)`: nil when there is no such
// tab.
func (s *Shell) activeTab() Tab {
	if s.active < 0 || s.active >= len(s.tabs) {
		return nil
	}
	return s.tabs[s.active]
}

// activeMarked is `self.tabs.get_mut(self.active).and_then(Tab::marked)`.
func (s *Shell) activeMarked() *Marked {
	tab := s.activeTab()
	if tab == nil {
		return nil
	}
	return tab.Marked()
}

// activeCursor is `.and_then(Tab::selection).and_then(Selection::cursor)`
// on the active tab.
func (s *Shell) activeCursor() (resultsgrid.Cell, bool) {
	tab := s.activeTab()
	if tab == nil {
		return resultsgrid.Cell{}, false
	}
	selection := tab.Selection()
	if selection == nil {
		return resultsgrid.Cell{}, false
	}
	return selection.Cursor()
}

// writeClipboard is `cx.write_to_clipboard(ClipboardItem::new_string(..))`.
func writeClipboard(text string) {
	mygo.Clipboard.WriteText(text)
}

// --- the sidebar's filter -----------------------------------------------

func (s *Shell) rebuildCatalogRows() {
	// The matching lowercases as it goes, so the line is passed on as
	// it was typed.
	needle := s.catalogFilter.Trimmed()
	s.catalogRows = catalogRows(
		s.catalogGroups,
		s.openSchemas,
		s.closedSections,
		s.openRelations,
		needle,
	)
	// The cursor is an index into the rows that have just been
	// replaced, so it cannot survive them: a keystroke in the filter
	// puts other names at those indices, and a cursor left where it
	// was would point at one the user never walked to.
	s.catalogSelected = nil
	// What ⇥ would take is read off the rows, so the hint is set here
	// rather than by each caller.
	s.updateFilterGhost()
}

// filterTarget is the relation the keys are aimed at: the one the cursor
// is on, or the first the filter left while the cursor is still on the
// line. ⏎ opens it and ⇥ finishes the line from it, so the hint and the
// key cannot say two different things.
func (s *Shell) filterTarget() (schema, name string, ok bool) {
	var row CatalogRow
	if s.catalogSelected != nil {
		ix := *s.catalogSelected
		if ix < 0 || ix >= len(s.catalogRows) {
			return "", "", false
		}
		row = s.catalogRows[ix]
	} else {
		for _, candidate := range s.catalogRows {
			if _, isRelation := candidate.(CatalogRowRelation); isRelation {
				row = candidate
				break
			}
		}
		if row == nil {
			return "", "", false
		}
	}
	switch row := row.(type) {
	case CatalogRowRelation:
		return row.schema, row.name, true
	default:
		return "", "", false
	}
}

// catalogStops is the rows ↑↓ walk: the relations the filter left, by
// their index into the flattened list. A header is not among them — it
// opens and closes, and ⏎ on this line means "open this relation".
func (s *Shell) catalogStops() []int {
	return catalogStops(s.catalogRows)
}

// stepCatalog moves the cursor one relation on, and scrolls it into view.
func (s *Shell) stepCatalog(forward bool) {
	stops := s.catalogStops()
	if ix, ok := stepStop(stops, s.catalogSelected, forward); ok {
		s.catalogSelected = &ix
		// `Nearest` and not `Center`: the cursor walks one row at a
		// time, and a list that re-centred on every press would move
		// further than the cursor did.
		s.catalogScroll.List.ScrollIntoView(ix)
	} else {
		s.catalogSelected = nil
	}
	// ⇥ finishes the line from the cursor, so moving it changes what
	// the hint offers.
	s.updateFilterGhost()
}

// filterCompletion is what ⇥ would finish the filter line with: the next
// part of the relation the keys are aimed at, as the palette finishes a
// name.
func (s *Shell) filterCompletion() (string, bool) {
	needle := s.catalogFilter.Trimmed()
	schema, name, ok := s.filterTarget()
	if !ok {
		return "", false
	}
	return PaletteCompletePath([]string{schema, name}, needle)
}

// updateFilterGhost shows what ⇥ would take, faint and after the value, in
// the shape `palette::ghost` gives it — the rest of the word when it
// carries on from the line, and `⇥ <name>` when taking it rewrites the
// line.
func (s *Shell) updateFilterGhost() {
	needle := s.catalogFilter.Trimmed()
	ghost := ""
	if completed, ok := s.filterCompletion(); ok {
		ghost = PaletteGhost(needle, completed)
	}
	s.catalogFilter.SetGhost(ghost)
}

// completeFilter takes the first match's name into the filter line, one
// part at a time, so ⇥⇥ walks schema then relation. It **replaces** the
// line rather than appending to it, because a hit sits anywhere inside a
// name: `dev` completes to `sample_dev_sample.`.
func (s *Shell) completeFilter() {
	completed, ok := s.filterCompletion()
	if !ok {
		return
	}
	// Setting the text emits `Changed`, which filters again, so the
	// list already follows the completed line when this returns.
	s.catalogFilter.SetText(completed)
}

// focusCatalogFilter puts the keys on the filter line, wherever they were,
// and marks what is on it. ⌘E is how the sidebar is reached without the
// mouse.
//
// The value is **marked rather than emptied**: a line the user narrowed
// to `dev.` is worth carrying on from, and ⎋ already empties it. Marking
// it means the next character replaces it either way, so nothing is lost
// by keeping it.
//
// The column find is closed for the reason the palette closes it: one
// search line takes the keys at a time. The palette and the close dialog
// are not reached past, because each is already the thing being answered.
func (s *Shell) focusCatalogFilter() {
	if s.palette != nil || s.confirm != nil {
		return
	}
	s.closeColumnFind()
	s.catalogFilter.SelectEverything()
	s.catalogFilter.FocusHandle().Focus()
}

// toggleSchema opens a closed schema, or closes an open one.
func (s *Shell) toggleSchema(key string) {
	if _, open := s.openSchemas[key]; open {
		delete(s.openSchemas, key)
	} else {
		s.openSchemas[key] = struct{}{}
	}
	s.rebuildCatalogRows()
}

// toggleSection closes an open section, or opens a closed one. A section
// is open until it is closed, so the set holds the opposite of the
// schemas'.
func (s *Shell) toggleSection(key string) {
	if _, closed := s.closedSections[key]; closed {
		delete(s.closedSections, key)
	} else {
		s.closedSections[key] = struct{}{}
	}
	s.rebuildCatalogRows()
}

// toggleRelation folds a relation out to show its columns and objects, or
// folds it back. Closed until opened, as a schema is.
func (s *Shell) toggleRelation(key string) {
	if _, open := s.openRelations[key]; open {
		delete(s.openRelations, key)
	} else {
		s.openRelations[key] = struct{}{}
	}
	s.rebuildCatalogRows()
}

// onFilterEvent answers the sidebar filter line's events.
func (s *Shell) onFilterEvent(_ *uikit.TextField, event uikit.TextFieldEvent) {
	switch event {
	case uikit.TextFieldChanged:
		// A filtered list is scrolled to wherever the last one was,
		// which is nowhere in particular once the rows change under
		// it. Send it back to the top.
		s.catalogScroll.List.ScrollTo(0, ui.Start)
		s.rebuildCatalogRows()
	// Enter opens the relation the cursor is on, and the first the
	// filter left while the cursor is still on the line — which is
	// what a one-hit filter is for. Escape empties the line and
	// hands the keys back to the workspace.
	case uikit.TextFieldSubmitted:
		if schema, table, ok := s.filterTarget(); ok {
			s.browseTable(schema, table)
		}
	case uikit.TextFieldCancelled:
		s.catalogFilter.Clear()
		s.focusHandle.Focus()
	// ⇥ finishes the line from the first match, one part at a time.
	case uikit.TextFieldNextFieldEvent:
		s.completeFilter()
	}
}

// --- opening a relation ---------------------------------------------------

// browseTable opens a relation the way a click in the sidebar does: a
// query tab on `SELECT * FROM … LIMIT 500;`, named for the relation and
// run at once.
func (s *Shell) browseTable(schema, table string) {
	s.newQueryWith(BrowseQuery(schema, table))
	// The tab is named for what it opened, not `query 7`: the strip is
	// then a list of what the user is looking at.
	if len(s.tabs) != 0 {
		if tab, ok := s.tabs[len(s.tabs)-1].(*QueryTab); ok {
			tab.title = schema + "." + table
			tab.relation = &[2]string{schema, table}
		}
	}
	// The name and the relation land after the tab was opened, so the
	// strip is written back once more with them on it.
	s.rememberTabs()
	s.runActiveQuery()
}

// browseTableIn browses a relation from the palette, or focuses the tab
// that already browses it. `newTab` is the palette's ⌘⏎: give me another
// tab on this one, so two statements over the same table can sit side by
// side.
//
// A tab is the one that already browses this relation when it was opened
// on it — `QueryTab::relation` is what says so. The statement in it is the
// user's by then and may say anything, which is exactly why the tab is
// focused rather than rewritten.
func (s *Shell) browseTableIn(schema, table string, newTab bool) {
	if !newTab {
		for ix, tab := range s.tabs {
			query, ok := tab.(*QueryTab)
			if !ok || query.relation == nil {
				continue
			}
			if query.relation[0] == schema && query.relation[1] == table {
				s.activate(ix)
				s.focusActiveTab()
				return
			}
		}
	}
	s.browseTable(schema, table)
}

func (s *Shell) tableModel(schema, table string) *introspect.Table {
	if s.catalog == nil {
		return nil
	}
	for si := range s.catalog.Schemas {
		found := &s.catalog.Schemas[si]
		if found.Name != schema {
			continue
		}
		for ti := range found.Tables {
			if found.Tables[ti].Name == table {
				return &found.Tables[ti]
			}
		}
		// `find` stops at the first schema of that name.
		return nil
	}
	return nil
}

// newQuery opens an empty query tab.
func (s *Shell) newQuery() {
	s.newQueryWith("select * from ")
}

// newQueryWith opens a query tab on a statement. The history screen opens
// a run this way, so a query comes back editable rather than re-run behind
// the user's back.
func (s *Shell) newQueryWith(sql string) {
	id := s.takeID()
	vocabulary := s.vocabulary
	editor := sqleditor.New(sql, vocabulary)
	editor.FocusHandle().Focus()
	// The editor knows nothing about a database — it holds the text and
	// paints what it is told — so the shell is what turns an edit into a
	// question for the server.
	editor.Subscribe(func(_ *sqleditor.SqlEditor, event sqleditor.SqlEditorEvent) {
		switch event {
		case sqleditor.Changed:
			s.scheduleCheck(id)
		}
	})
	s.tabs = append(s.tabs, &QueryTab{
		id:            id,
		title:         fmt.Sprintf("query %d", id),
		relation:      nil,
		editor:        editor,
		data:          emptyGrid(),
		hasResult:     false,
		changed:       nil,
		truncated:     false,
		statementsRun: 0,
		timing:        nil,
		error:         nil,
		run:           Run{},
		landed:        0,
		inTransaction: false,
		// A tab opens on auto, whatever connection it is on: the mode
		// is the tab's own, and the toolbar is where it is switched.
		txMode:       dbclient.TxModeAuto,
		txTally:      TxTally{},
		txDone:       nil,
		txEnding:     false,
		lastUsed:     time.Now(),
		sessionEnded: false,
		// Opened on the tab's first run, never here: a strip of a
		// hundred restored tabs must cost the server nothing until the
		// user asks one of them a question.
		session:      nil,
		schemaReport: nil,
		selection:    resultsgrid.Selection{},
		scroll:       resultsgrid.NewGridState(),
		generation:   0,
		checked:      0,
	})
	s.activate(len(s.tabs) - 1)
}

// --- the ⌘K palette -----------------------------------------------------

func (s *Shell) togglePalette() {
	if s.palette != nil {
		s.closePalette()
	} else {
		s.openPalette()
	}
}

// openPalette opens the palette on everything the session already holds:
// the catalog it introspected, and the runs it reads back here. Reading
// the runs is a local SQLite call, so it stays on this thread — the
// goroutine bridge is for the database, not for the file beside it.
func (s *Shell) openPalette() {
	// One overlay at a time: the palette takes the focus, so a
	// column-find popover or a value peek left open would sit under it
	// holding keys nobody can see them taking.
	s.columnFind = nil
	s.peek = nil
	query := uikit.NewTextField("Search tables and history…").Bare(PaletteInputFontSize)
	query.Subscribe(s.onPaletteEvent)
	query.FocusHandle().Focus()

	var runs []storage.QueryRun
	if s.store != nil {
		if found, err := s.store.ListHistory(s.scope, storage.HistoryFilter{}); err == nil {
			runs = found
		}
	}

	s.palette = &Palette{
		query:    query,
		chip:     PaletteScopeAll,
		runs:     runs,
		rows:     nil,
		matches:  0,
		selected: 0,
	}
	s.rebuildPalette()
}

func (s *Shell) closePalette() {
	s.palette = nil
	// The workspace must take the focus back, or the key bindings have
	// nowhere to dispatch once the palette is gone.
	s.focusHandle.Focus()
}

// rebuildPalette searches again and keeps the selection on a result. The
// rows change under the selection with every keystroke, so it goes back
// to the first result rather than trying to follow what was there before.
func (s *Shell) rebuildPalette() {
	palette := s.palette
	if palette == nil {
		return
	}
	typed := palette.query.Text()
	scope, needle := PaletteParse(typed, palette.chip)
	found := PaletteBuild(s.catalog, palette.runs, scope, needle, HistoryToday())

	palette.selected = 0
	for ix, row := range found.Rows {
		if _, ok := row.Pick(); ok {
			palette.selected = ix
			break
		}
	}
	palette.rows = found.Rows
	palette.matches = found.Matches
	palette.scroll.List.ScrollTo(palette.selected, ui.Start)
	s.updateGhost()
}

// updateGhost shows what ⇥ would finish the line with, faint and after
// the value.
//
// `PaletteGhost` decides the shape: the rest of the word when the
// completion carries on from what was typed, and `⇥ <name>` when taking it
// would rewrite the line instead.
func (s *Shell) updateGhost() {
	palette := s.palette
	if palette == nil {
		return
	}
	typed := palette.query.Text()
	_, needle := PaletteParse(typed, palette.chip)
	ghost := ""
	if completed, ok := PaletteCompletion(palette.rows, palette.selected, needle); ok {
		ghost = PaletteGhost(needle, completed)
	}
	palette.query.SetGhost(ghost)
}

// completePalette takes the selected row's real name into the search line,
// so the next thing typed narrows inside it. Reports whether there was
// anything to take.
func (s *Shell) completePalette() bool {
	palette := s.palette
	if palette == nil {
		return false
	}
	typed := palette.query.Text()
	_, needle := PaletteParse(typed, palette.chip)
	completed, ok := PaletteCompletion(palette.rows, palette.selected, needle)
	if !ok {
		return false
	}
	// Whatever came before the needle — a `t:` and any space after it
	// — is the user's and stays.
	kept := typed[:len(typed)-len(needle)]
	line := kept + completed
	// Setting the text emits `Changed`, which searches again, so the
	// list is already following the completed line by the time this
	// returns.
	palette.query.SetText(line)
	return true
}

// stepPalette moves the selection to the next result in that direction,
// stepping over the headings. It stops at the ends rather than wrapping: a
// list that jumps back to the top loses the reader's place.
func (s *Shell) stepPalette(forward bool) {
	palette := s.palette
	if palette == nil {
		return
	}
	ix := palette.selected
	for {
		var next int
		if forward {
			next = ix + 1
		} else {
			// `checked_sub(1).unwrap_or(usize::MAX)`: off the front is
			// past the end, where `get` finds nothing.
			next = ix - 1
		}
		if next < 0 || next >= len(palette.rows) {
			return
		}
		row := palette.rows[next]
		ix = next
		if _, ok := row.Pick(); ok {
			break
		}
	}
	palette.selected = ix
	palette.scroll.List.ScrollTo(ix, ui.Center)
	// ⇥ finishes the line from the selection, so moving it changes
	// what the hint offers.
	s.updateGhost()
}

func (s *Shell) setPaletteScope(chip PaletteScope) {
	palette := s.palette
	if palette == nil {
		return
	}
	palette.chip = chip
	// A chip is only meaningful while the line has the focus.
	palette.query.FocusHandle().Focus()
	s.rebuildPalette()
}

// openPaletteSelection opens what the selection points at.
func (s *Shell) openPaletteSelection(newTab bool) {
	palette := s.palette
	if palette == nil {
		return
	}
	if palette.selected < 0 || palette.selected >= len(palette.rows) {
		return
	}
	pick, ok := palette.rows[palette.selected].Pick()
	if !ok {
		return
	}
	s.openPick(pick, newTab)
}

func (s *Shell) openPick(pick PalettePick, newTab bool) {
	// The palette closes first, so the tab it opens is the thing that
	// holds the focus afterwards.
	s.closePalette()
	switch pick.Kind {
	// A relation opens the way a click in the sidebar opens one: a
	// query tab on `SELECT * FROM ... LIMIT 500;`, run at once. The
	// palette answers "show me this table", and the answer is its
	// rows with an editable statement over them.
	case PalettePickTable:
		s.browseTableIn(pick.Schema, pick.Table, newTab)
	// A run comes back editable, never re-run behind the user, for
	// the same reason the history screen opens one that way.
	case PalettePickQuery:
		s.newQueryWith(pick.Statement)
	}
}

func (s *Shell) onPaletteEvent(_ *uikit.TextField, event uikit.TextFieldEvent) {
	switch event {
	case uikit.TextFieldChanged:
		s.rebuildPalette()
	case uikit.TextFieldSubmitted:
		s.openPaletteSelection(false)
	case uikit.TextFieldCancelled:
		s.closePalette()
	// Tab finishes the line from the selected row. With nothing left
	// to finish it walks the scope chips instead, the way it walks the
	// fields of the connection form.
	case uikit.TextFieldNextFieldEvent:
		if s.completePalette() {
			return
		}
		palette := s.palette
		if palette == nil {
			return
		}
		here := 0
		for ix, scope := range PaletteScopes {
			if scope == palette.chip {
				here = ix
				break
			}
		}
		next := PaletteScopes[(here+1)%len(PaletteScopes)]
		s.setPaletteScope(next)
	}
}

// --- finding a column -----------------------------------------------------

// toggleColumnFind is ⌘J. A result wider than the pane is the ordinary
// case for a real table, and the name of the column is usually all the
// user knows about the one they are looking for — so the way to it is a
// search over the names, not a scroll along the header.
func (s *Shell) toggleColumnFind() {
	// The palette and the close dialog are each already the thing taking
	// keys while they are up.
	if s.palette != nil || s.confirm != nil {
		return
	}
	if s.columnFind != nil {
		s.closeColumnFind()
	} else {
		s.openColumnFind()
	}
}

func (s *Shell) openColumnFind() {
	active := s.activeTab()
	if active == nil {
		return
	}
	tab := active.ID()
	// A tab with no result has no column to jump to. Nothing is said
	// about it: the button is not there either.
	if len(s.resultColumns()) == 0 {
		return
	}
	query := uikit.NewTextField("find column").Bare(columnFindFontSize)
	query.Subscribe(s.onColumnFindEvent)
	query.FocusHandle().Focus()
	s.columnFind = &ColumnFind{
		tab:      tab,
		query:    query,
		selected: 0,
	}
}

// closeColumnFind closes it and gives the keys back to the tab, so ⎋
// leaves the user where they were rather than nowhere.
func (s *Shell) closeColumnFind() {
	s.columnFind = nil
	s.focusActiveTab()
}

// resultColumns is the column names of the result on screen. Both kinds
// of tab that show one answer here; a history tab shows a list of runs, so
// it has none.
func (s *Shell) resultColumns() []string {
	switch tab := s.activeTab().(type) {
	case *TableTab:
		return tab.data.Columns
	case *QueryTab:
		return tab.data.Columns
	default:
		return nil
	}
}

// columnMatches is which lanes the typed line names, as indices into the
// result. Worked out rather than remembered — see [ColumnFind].
func (s *Shell) columnMatches() []int {
	find := s.columnFind
	if find == nil {
		return nil
	}
	return resultsgrid.FindColumns(s.resultColumns(), find.query.Text())
}

// stepColumnFind: ↑↓ walk what the search found. They stop at the ends
// rather than wrapping, as the palette's list does: a list that jumps back
// to the top loses the reader's place.
func (s *Shell) stepColumnFind(forward bool) {
	count := len(s.columnMatches())
	find := s.columnFind
	if find == nil {
		return
	}
	if count == 0 {
		return
	}
	here := min(find.selected, count-1)
	var next int
	if forward {
		next = min(here+1, count-1)
	} else {
		next = max(here-1, 0)
	}
	find.selected = next
	find.scroll.List.ScrollIntoView(next)
}

// jumpToSelectedColumn is ⏎: jump to the row the list has selected.
func (s *Shell) jumpToSelectedColumn() {
	matches := s.columnMatches()
	find := s.columnFind
	if find == nil {
		return
	}
	at := min(find.selected, max(len(matches)-1, 0))
	if at >= len(matches) {
		return
	}
	s.jumpToColumn(matches[at])
}

// jumpToColumn puts the cursor in one column of the result and brings it
// into view, **keeping the row it is already on**: the user asked for a
// column, not for a cell somewhere else in the result.
func (s *Shell) jumpToColumn(column int) {
	s.columnFind = nil
	marked := s.activeMarked()
	if marked == nil {
		return
	}
	extent := resultsgrid.ExtentOf(marked.data)
	// The result can have been replaced since the list was painted, so
	// the lane is checked against the result in hand rather than trusted.
	if column >= extent.Columns || extent.Rows == 0 {
		return
	}
	row := 0
	if cursor, ok := marked.selection.Cursor(); ok {
		row = cursor.Row
	}
	cell := resultsgrid.NewCell(min(row, extent.Rows-1), column)
	marked.selection.Focus(cell)
	marked.scroll.Reveal(cell, marked.data)
	// A jump hands the keys to the result it jumped in, so ↑↓ walk the
	// grid from the cell it landed on.
	s.gridFocus.Focus()
}

func (s *Shell) onColumnFindEvent(_ *uikit.TextField, event uikit.TextFieldEvent) {
	switch event {
	// The list is read straight off the line, so a keystroke only has
	// to put the selection back on the first match — the rows change
	// under it with every character.
	case uikit.TextFieldChanged:
		if find := s.columnFind; find != nil {
			find.selected = 0
			find.scroll.List.ScrollTo(0, ui.Start)
		}
	case uikit.TextFieldSubmitted:
		s.jumpToSelectedColumn()
	case uikit.TextFieldCancelled:
		s.closeColumnFind()
	// ⇥ has nothing to finish here. The palette completes a path one
	// part at a time because a name is deep; a column name is one part,
	// and the whole list of them is already on screen.
	case uikit.TextFieldNextFieldEvent:
	}
}

// --- finding a value --------------------------------------------------------

// findInTab is ⌘F outside the editor and the grid: find in whatever the
// tab is about. A query tab is about its statement, so the editor's own
// find opens; a table tab is a result and nothing else.
func (s *Shell) findInTab() {
	if s.palette != nil || s.confirm != nil {
		return
	}
	// The strip takes the keys, and one thing takes keys at a time: a
	// popover or a card left open would sit there holding none.
	s.columnFind = nil
	s.peek = nil
	switch tab := s.activeTab().(type) {
	case *QueryTab:
		tab.editor.Find()
	case *TableTab:
		s.openResultFind()
	}
}

// openResultFind is ⌘F in the grid: open the strip over the result, or put
// the keys back in it. It opens on what was last searched for, marked, so
// the next character replaces it.
func (s *Shell) openResultFind() {
	// Each of these is already the thing taking keys while it is up.
	if s.palette != nil || s.confirm != nil {
		return
	}
	active := s.activeTab()
	if active == nil {
		return
	}
	tab := active.ID()
	// A result with no columns has no value to find.
	if len(s.resultColumns()) == 0 {
		return
	}
	// One search line at a time, and the column find is a popover the
	// strip would open under. A value card is dropped for the same
	// reason: it would stay up holding no keys.
	s.columnFind = nil
	s.peek = nil
	var query *uikit.TextField
	if s.resultFind != nil && s.resultFind.tab == tab {
		query = s.resultFind.query
	} else {
		query = uikit.NewTextField("find in result").Bare(uikit.FontSize)
		if s.resultNeedle != "" {
			// Set before the listener is subscribed, as the Rust set it
			// before `subscribe_in`: the text is not a keystroke.
			query.SetText(s.resultNeedle)
		}
		query.Subscribe(s.onResultFindEvent)
		s.resultFind = &ResultFind{
			tab:   tab,
			query: query,
			found: nil,
		}
	}
	query.SelectEverything()
	query.FocusHandle().Focus()
}

// closeResultFind is ⎋ in the line: close the strip and give the keys to
// the grid, whose cursor is on the hit the walk stood on.
func (s *Shell) closeResultFind() {
	if find := s.resultFind; find != nil {
		s.resultFind = nil
		s.resultNeedle = find.query.Text()
	}
	s.gridFocus.Focus()
}

// resultData is the result the active tab holds, when it holds one.
func (s *Shell) resultData() *resultsgrid.GridData {
	switch tab := s.activeTab().(type) {
	case *TableTab:
		return tab.data
	case *QueryTab:
		return tab.data
	default:
		return nil
	}
}

// currentResultNeedle is what the strip is searching for: the open line's
// text, or what the closed one last held.
//
// Go port: Rust's `result_needle`; in Go that name is the field's.
func (s *Shell) currentResultNeedle() string {
	if find := s.resultFind; find != nil {
		return find.query.Text()
	}
	return s.resultNeedle
}

// resultHits is the cells of the active result that hold the needle, in
// reading order, and whether the search stopped short of the end. Worked
// out again only when the result or the needle changed — see
// [ResultFind].
func (s *Shell) resultHits() ([]resultsgrid.Cell, bool) {
	needle := s.currentResultNeedle()
	data := s.resultData()
	if data == nil {
		return nil, false
	}
	search := func() *FoundCells {
		pattern := fuzzy.NewNeedle(needle)
		cells, capped := resultsgrid.FindCells(data, &pattern, uikit.MaxHits)
		return &FoundCells{
			data:   data,
			needle: needle,
			cells:  cells,
			capped: capped,
		}
	}
	// A closed strip keeps no memo: ⌘G in the grid is one search per
	// press, which is what it costs anyway.
	find := s.resultFind
	if find == nil {
		found := search()
		return found.cells, found.capped
	}
	memo := find.found
	sameData := memo != nil && memo.data == data
	fresh := sameData && memo.needle == needle
	// The memo is what makes this cheap: the view asks for the hits on
	// every frame, so nothing below may run while the memo is current.
	// (The Rust worked the refinement out eagerly and threw it away when
	// the memo was fresh — a full re-match per call, twice a frame.)
	if !fresh {
		// A needle that only grew is looked for among the hits it already
		// has, so typing a word reads the whole result once rather than
		// once per letter. A capped answer left cells out and cannot be.
		if sameData && !memo.capped && memo.needle != "" && len(needle) >= len(memo.needle) &&
			needle[:len(memo.needle)] == memo.needle {
			pattern := fuzzy.NewNeedle(needle)
			find.found = &FoundCells{
				data:   data,
				needle: needle,
				cells:  resultsgrid.RefineCells(data, memo.cells, &pattern),
				capped: false,
			}
		} else {
			find.found = search()
		}
	}
	found := find.found
	return found.cells, found.capped
}

// landResultFind: a keystroke in the line lands on the first hit from the
// cursor, so a hit already under it stays put as the needle grows, and the
// search moves on from where the user was rather than from the first row.
func (s *Shell) landResultFind() {
	hits, _ := s.resultHits()
	at, ok := s.activeCursor()
	if !ok {
		at = resultsgrid.NewCell(0, 0)
	}
	if ix, ok := uikit.FirstFrom(hits, at, resultsgrid.Cell.Compare); ok {
		s.jumpToHit(hits[ix])
	}
}

// stepResultFind is ⏎ and ⇧⏎ in the line, ⌘G and ⇧⌘G in the grid: the
// next hit or the one before, wrapping at both ends.
func (s *Shell) stepResultFind(forward bool) {
	hits, _ := s.resultHits()
	var at *resultsgrid.Cell
	if cursor, ok := s.activeCursor(); ok {
		at = &cursor
	}
	if ix, ok := uikit.Step(hits, at, forward, resultsgrid.Cell.Compare); ok {
		s.jumpToHit(hits[ix])
	}
}

// jumpToHit puts the cursor on a hit and brings it into view. The focus
// stays where it is: in the line, the user is still typing or stepping,
// and in the grid, ⌘G was pressed there.
func (s *Shell) jumpToHit(cell resultsgrid.Cell) {
	marked := s.activeMarked()
	if marked == nil {
		return
	}
	extent := resultsgrid.ExtentOf(marked.data)
	// The hits were worked out from the result in hand, but the check
	// is cheap and a cursor past the end is not.
	if cell.Row >= extent.Rows || cell.Column >= extent.Columns {
		return
	}
	marked.selection.Focus(cell)
	marked.scroll.Reveal(cell, marked.data)
}

func (s *Shell) onResultFindEvent(_ *uikit.TextField, event uikit.TextFieldEvent) {
	switch event {
	case uikit.TextFieldChanged:
		s.landResultFind()
	case uikit.TextFieldSubmitted:
		s.stepResultFind(true)
	case uikit.TextFieldCancelled:
		s.closeResultFind()
	// One field, and nothing to finish.
	case uikit.TextFieldNextFieldEvent:
	}
}

// resultFindStrip is the strip over the result, when it is open on this
// tab. It wears ResultFindKeyContext: the strip dispatches that table
// itself and hands what it caught to handleAction, the way the Rust's
// actions bubbled from the strip to the shell's listeners.
//
// Go port: Rust's `Option<Div>`; the second result is false for `None`,
// and nothing is built then. The element is built into the current
// parent.
func (s *Shell) resultFindStrip(c *ui.Context, tabID uint64) (ui.Element, bool) {
	if s.resultFind == nil || s.resultFind.tab != tabID {
		return ui.Element{}, false
	}
	strip := ui.Box(c.Key("result-find-context")).Shrink(0)
	// No `Invalidate` after the action: a shortcut the strip claims is a
	// consumed input, and mygo builds the view again in the same frame
	// for it, so the count already moves with the step.
	if action, ok := uikit.DispatchKeys(strip, resultFindKeys); ok {
		s.handleAction(action)
	}
	find := s.resultFind
	if find == nil || find.tab != tabID {
		// The action took the strip down; the element is already built,
		// and stays empty for this one frame.
		return strip, true
	}
	hits, capped := s.resultHits()
	var current *int
	if cursor, ok := s.activeCursor(); ok {
		if ix, found := slices.BinarySearchFunc(hits, cursor, resultsgrid.Cell.Compare); found {
			current = &ix
		}
	}
	count := uikit.FindCount{
		Typed:   find.query.Text() != "",
		Total:   len(hits),
		Current: current,
		Capped:  capped,
	}
	query := find.query
	strip.Children(func() {
		uikit.FindBar(c, "result-find", query, count,
			func(forward bool) { s.stepResultFind(forward) },
			func() { s.closeResultFind() })
	})
	return strip, true
}

// --- reading a whole value ------------------------------------------------

// openPeek is ⏎ over the cursor's cell, and a double click on any cell:
// show the value whole.
func (s *Shell) openPeek(cell resultsgrid.Cell) {
	// Each of those is already the thing taking keys while it is up.
	if s.palette != nil || s.confirm != nil {
		return
	}
	active := s.activeTab()
	if active == nil {
		return
	}
	tab := active.ID()
	s.columnFind = nil
	// Go port: GPUI needed `window.prevent_default()` here, or the grid
	// pane — focused by the press of the double click — took the focus
	// straight back. mygo focuses on the press, before the double click
	// is reported, so the card's Focus below is the last word.
	s.peek = &Peek{
		tab:       tab,
		cell:      cell,
		selection: query.Range{},
		anchor:    nil,
	}
	s.peek.focus.Focus()
	// Nothing worth showing means nothing shown: an out-of-range cell is
	// a cell the result no longer has.
	if _, _, _, ok := s.peekValue(); !ok {
		s.peek = nil
		s.gridFocus.Focus()
		return
	}
}

// peekCursor is ⏎ from the grid: the cursor's own cell.
func (s *Shell) peekCursor() {
	cell, ok := s.activeCursor()
	if !ok {
		return
	}
	s.openPeek(cell)
}

// peekValue is what the card is showing: the column's name, the row's
// number as the gutter counts it, and the value.
//
// Read from the result in hand every time, never remembered — see [Peek].
// `ok == false` says there is nothing there to show, which is what a page
// turn under an open card leaves behind.
func (s *Shell) peekValue() (name string, row int, value string, ok bool) {
	peek := s.peek
	if peek == nil {
		return "", 0, "", false
	}
	tab := s.activeTab()
	if tab == nil || tab.ID() != peek.tab {
		return "", 0, "", false
	}
	var data *resultsgrid.GridData
	var firstRow int
	switch tab := tab.(type) {
	// A table tab is a window on the table, so the row wears the
	// number its gutter gives it rather than its index in the page.
	case *TableTab:
		data, firstRow = tab.data, tab.page*PageSize+1
	case *QueryTab:
		data, firstRow = tab.data, 1
	default:
		return "", 0, "", false
	}
	if peek.cell.Row < 0 || peek.cell.Row >= len(data.Rows) {
		return "", 0, "", false
	}
	cells := data.Rows[peek.cell.Row]
	if peek.cell.Column < 0 || peek.cell.Column >= len(cells) {
		return "", 0, "", false
	}
	if peek.cell.Column >= len(data.Columns) {
		return "", 0, "", false
	}
	return data.Columns[peek.cell.Column], firstRow + peek.cell.Row, cells[peek.cell.Column].Display(), true
}

// peekShown is the text the card paints: the value, cut to `PEEK_CHARS`.
//
// The marked range indexes into **this** rather than into the value, so
// both are worked out from one string and a selection can never name
// characters that were never on screen.
func (s *Shell) peekShown() (string, bool) {
	_, _, value, ok := s.peekValue()
	if !ok {
		return "", false
	}
	return takeChars(value, peekChars), true
}

// takeChars is `s.chars().take(n).collect()`: the first n characters.
func takeChars(text string, n int) string {
	count := 0
	for at := range text {
		if count == n {
			return text[:at]
		}
		count++
	}
	return text
}

// peekPress: the pointer went down in the text. One click starts a drag
// from there, two mark the word under it, three the whole line — the three
// gestures every text view answers to.
func (s *Shell) peekPress(offset, clicks int) {
	shown, ok := s.peekShown()
	if !ok {
		return
	}
	peek := s.peek
	if peek == nil {
		return
	}
	switch clicks {
	case 0, 1:
		offset := clampToBoundary(shown, offset)
		peek.anchor = &offset
		peek.selection = query.Range{Start: offset, End: offset}
	case 2:
		peek.anchor = nil
		peek.selection = wordAt(shown, offset)
	default:
		peek.anchor = nil
		peek.selection = lineAt(shown, offset)
	}
}

// peekDrag: the pointer moved with the button still down. The mark runs
// from the press to here, so a drag backwards marks the same text as a
// drag forwards over it.
func (s *Shell) peekDrag(offset int) {
	shown, ok := s.peekShown()
	if !ok {
		return
	}
	peek := s.peek
	if peek == nil {
		return
	}
	if peek.anchor == nil {
		return
	}
	anchor := *peek.anchor
	offset = clampToBoundary(shown, offset)
	peek.selection = query.Range{Start: min(anchor, offset), End: max(anchor, offset)}
}

// endPeekDrag: the button came up, so the drag is over. What is marked
// stays marked: it is what ⌘C is about to copy.
func (s *Shell) endPeekDrag() {
	peek := s.peek
	if peek == nil {
		return
	}
	// The surface is painted only while the anchor is set, so this is
	// what takes the drag's listeners and the I-beam cursor away.
	peek.anchor = nil
}

// selectAllPeek: ⌘A marks everything the card painted, which on a cut
// value is everything there is to mark — the rest was never on screen.
func (s *Shell) selectAllPeek() {
	shown, ok := s.peekShown()
	if !ok {
		return
	}
	peek := s.peek
	if peek == nil {
		return
	}
	peek.selection = query.Range{Start: 0, End: len(shown)}
}

// peekCopyText is what ⌘C takes: the marked text, or the whole value when
// nothing is marked.
//
// **Marked wins, and it is the painted string that is sliced.** A value
// longer than `PEEK_CHARS` is on screen in part, and a mark inside that
// part must copy exactly what it covers rather than the megabyte behind
// it.
func (s *Shell) peekCopyText() (string, bool) {
	peek := s.peek
	if peek == nil {
		return "", false
	}
	if !peek.selection.IsEmpty() {
		shown, ok := s.peekShown()
		if !ok {
			return "", false
		}
		if marked, ok := strGet(shown, peek.selection); ok {
			return marked, true
		}
	}
	_, _, value, ok := s.peekValue()
	if !ok {
		return "", false
	}
	return value, true
}

// strGet is `str::get(range)`: the slice, or false when the range is out
// of bounds, backwards, or splits a character.
func strGet(text string, r query.Range) (string, bool) {
	if r.Start < 0 || r.Start > r.End || r.End > len(text) {
		return "", false
	}
	boundary := func(at int) bool {
		return at == 0 || at == len(text) || utf8.RuneStart(text[at])
	}
	if !boundary(r.Start) || !boundary(r.End) {
		return "", false
	}
	return text[r.Start:r.End], true
}

func (s *Shell) closePeek() {
	s.peek = nil
	// Back to the result the card was opened over, not to the tab's
	// usual focus: the user was walking the grid a keystroke ago.
	s.gridFocus.Focus()
}

// copyPeek is ⌘C in the card: it copies **what is marked, and the whole
// value when nothing is** — never the selection the grid's own ⌘C would
// take.
//
// It is written bare, with none of CSV's quoting: a value read on its own
// is not a row, so a URL with a comma in it must not come back wrapped in
// quotes it never had.
func (s *Shell) copyPeek() {
	text, ok := s.peekCopyText()
	if !ok {
		return
	}
	writeClipboard(text)
}

// copyError is the error strip's `copy`: the server's message, whole and
// bare.
//
// Whole, because the line the user needs to paste is often the `LINE 1:`
// under the first; bare, because it is going into a ticket or a search
// box, not a grid.
func (s *Shell) copyError(tabID uint64, message string) {
	writeClipboard(message)
	s.copiedError = &copiedError{tab: tabID, message: message}
}

// --- walking the tabs -------------------------------------------------

// stepTab: ⌃⇥ moves to the next tab, ⌃⇧⇥ to the one before it, in the
// order the strip paints them. There is no popup: the switch happens on
// the keystroke, and the strip is the list.
//
// It stays out of the way while the palette is up: that dialog is already
// the one taking keys.
func (s *Shell) stepTab(forward bool) {
	if s.palette != nil || len(s.tabs) < 2 {
		return
	}
	s.activate(stepWrapping(len(s.tabs), s.active, forward))
	s.focusActiveTab()
}

// activate makes one tab the active one. Every path that changes the
// active tab goes through here, which is also what makes this the place
// the strip is written back from.
func (s *Shell) activate(ix int) {
	if ix < 0 || ix >= len(s.tabs) {
		return
	}
	// The column-find popover and the value peek are both controls on
	// one result. Every caller here hands the focus on to the tab it
	// activated, so they go rather than being carried to a result they
	// were never opened on.
	s.columnFind = nil
	// Its needle outlives it, so ⌘G on the next tab steps over what was
	// typed here, as it would after ⎋.
	if find := s.resultFind; find != nil {
		s.resultFind = nil
		s.resultNeedle = find.query.Text()
	}
	s.peek = nil
	s.active = ix
	s.rememberTabs()
}

// focusActiveTab: a query tab types into its editor, so it wants the focus
// itself; a table tab is a result and nothing else, so it opens with the
// grid focused and its keys live at once. Everything else leaves the focus
// on the shell, where the shell's own keys are bound.
func (s *Shell) focusActiveTab() {
	switch tab := s.activeTab().(type) {
	case *QueryTab:
		tab.editor.FocusHandle().Focus()
	case *TableTab:
		s.gridFocus.Focus()
	default:
		s.focusHandle.Focus()
	}
}

func (s *Shell) stepPage(forward bool) {
	tab, ok := s.activeTab().(*TableTab)
	if !ok {
		return
	}
	if tab.loading {
		return
	}
	id, page := tab.id, tab.page
	var next int
	if forward {
		// Only offer the next page when this one came back full.
		if len(tab.data.Rows) < PageSize {
			return
		}
		next = page + 1
	} else {
		if page == 0 {
			return
		}
		next = page - 1
	}
	s.loadPage(id, next)
}

func (s *Shell) refreshActive() {
	switch tab := s.activeTab().(type) {
	case *TableTab:
		s.loadPage(tab.id, tab.page)
	case *QueryTab:
		s.runActiveQuery()
	// Refreshing the history re-reads the local file, which is what ⌘R
	// means on every other tab: show me this again.
	case *HistoryTab:
		s.loadHistory(tab.id)
	}
}

// tabMut is the tab with that id, or nil.
func (s *Shell) tabMut(tabID uint64) Tab {
	for _, tab := range s.tabs {
		if tab.ID() == tabID {
			return tab
		}
	}
	return nil
}

func (s *Shell) takeID() uint64 {
	id := s.nextID
	s.nextID++
	return id
}

// --- the selection ---------------------------------------------------

// stepCursor moves the cursor, and takes the scroll position with it.
//
// One function for every way the cursor moves — the arrow keys and ⇧ with
// them — so a cursor that has walked off the edge of the pane is always
// brought back into view, whichever key moved it.
func (s *Shell) stepCursor(step resultsgrid.Step, extend bool) {
	marked := s.activeMarked()
	if marked == nil {
		return
	}
	extent := resultsgrid.ExtentOf(marked.data)
	cell, ok := marked.selection.Step(step, extend, extent)
	if !ok {
		return
	}
	marked.scroll.Reveal(cell, marked.data)
}

// --- actions ---------------------------------------------------------

// handleAction is every `.on_action(cx.listener(Self::on_*))` the shell
// hung on its elements, in one switch: whichever table caught the key —
// the shell's, the grid's, the confirmation's, the peek's, the column
// find's, the result find's, the filter's — hands its action here.
func (s *Shell) handleAction(action ShellAction) {
	switch action {
	case ShellRunQuery:
		s.runActiveQuery()
	case ShellStopQuery:
		s.stopActiveQuery()
	case ShellNewQuery:
		s.newQuery()
	case ShellCloseTab:
		s.onCloseTab()
	case ShellRefresh:
		s.refreshActive()
	case ShellPrevPage:
		s.stepPage(false)
	case ShellNextPage:
		s.stepPage(true)
	case ShellShowHistory:
		s.onShowHistory()
	case ShellNextTab:
		s.stepTab(true)
	case ShellPrevTab:
		s.stepTab(false)
	case ShellConfirmClose:
		s.onConfirmClose()
	case ShellCancelClose:
		s.closeConfirm()
	case ShellSelectUp:
		s.stepCursor(resultsgrid.StepUp, false)
	case ShellSelectDown:
		s.stepCursor(resultsgrid.StepDown, false)
	case ShellSelectLeft:
		s.stepCursor(resultsgrid.StepLeft, false)
	case ShellSelectRight:
		s.stepCursor(resultsgrid.StepRight, false)
	case ShellExtendUp:
		s.stepCursor(resultsgrid.StepUp, true)
	case ShellExtendDown:
		s.stepCursor(resultsgrid.StepDown, true)
	case ShellExtendLeft:
		s.stepCursor(resultsgrid.StepLeft, true)
	case ShellExtendRight:
		s.stepCursor(resultsgrid.StepRight, true)
	case ShellSelectRowStart:
		s.stepCursor(resultsgrid.StepRowStart, false)
	case ShellSelectRowEnd:
		s.stepCursor(resultsgrid.StepRowEnd, false)
	case ShellSelectFirstRow:
		s.stepCursor(resultsgrid.StepFirst, false)
	case ShellSelectLastRow:
		s.stepCursor(resultsgrid.StepLast, false)
	case ShellSelectAll:
		s.onSelectAll()
	case ShellCopySelection:
		s.onCopySelection()
	case ShellTogglePick:
		s.onTogglePick()
	case ShellClearSelection:
		s.onClearSelection()
	case ShellFindColumn:
		s.toggleColumnFind()
	case ShellColumnPrev:
		s.stepColumnFind(false)
	case ShellColumnNext:
		s.stepColumnFind(true)
	case ShellFindInTab:
		s.findInTab()
	case ShellFindInResult:
		s.openResultFind()
	case ShellResultFindNext:
		s.stepResultFind(true)
	case ShellResultFindPrev:
		s.stepResultFind(false)
	case ShellFilterCatalog:
		s.focusCatalogFilter()
	case ShellCatalogPrev:
		s.stepCatalog(false)
	case ShellCatalogNext:
		s.stepCatalog(true)
	case ShellPeekValue:
		s.peekCursor()
	case ShellClosePeek:
		s.closePeek()
	case ShellCopyPeek:
		s.copyPeek()
	case ShellSelectAllPeek:
		s.selectAllPeek()
	case ShellCommitTransaction:
		s.endTransaction(dbclient.TxEndCommit)
	case ShellRollbackTransaction:
		s.endTransaction(dbclient.TxEndRollback)
	}
}

// handlePaletteAction is the palette's listeners: `on_toggle_palette` on
// the shell's element (⌘K, PaletteShellKeyBindings) and `on_palette_prev`,
// `on_palette_next`, `on_palette_new_tab` on the palette's own
// (PaletteKeyBindings).
func (s *Shell) handlePaletteAction(action PaletteAction) {
	switch action {
	case PaletteToggle:
		s.togglePalette()
	case PaletteSelectPrev:
		s.stepPalette(false)
	case PaletteSelectNext:
		s.stepPalette(true)
	case PaletteOpenInNewTab:
		s.openPaletteSelection(true)
	}
}

func (s *Shell) onSelectAll() {
	marked := s.activeMarked()
	if marked == nil {
		return
	}
	extent := resultsgrid.ExtentOf(marked.data)
	marked.selection.SelectAll(extent)
}

// onCopySelection is ⌘C. Nothing marked copies nothing rather than the
// whole result: a copy that took 500 rows because the user pressed the
// key with an empty selection is a copy nobody asked for.
func (s *Shell) onCopySelection() {
	marked := s.activeMarked()
	if marked == nil {
		return
	}
	text, ok := resultsgrid.ClipboardText(marked.data, marked.selection)
	if !ok {
		return
	}
	writeClipboard(text)
}

// onTogglePick: space ticks the row the cursor is on, so a run of rows can
// be picked without the mouse: ↓ then space, down the result.
func (s *Shell) onTogglePick() {
	marked := s.activeMarked()
	if marked == nil {
		return
	}
	cursor, ok := marked.selection.Cursor()
	if !ok {
		return
	}
	marked.selection.TogglePick(cursor.Row)
}

// onClearSelection: ⎋ drops what is marked.
func (s *Shell) onClearSelection() {
	marked := s.activeMarked()
	if marked == nil {
		return
	}
	marked.selection.Clear()
}

func (s *Shell) onCloseTab() {
	// A strip with nothing in it still answers ⌘W, and it answers with
	// the window: there is no tab to close and no way back to the
	// connections screen. The id is never read on that path.
	var id uint64
	if tab := s.activeTab(); tab != nil {
		id = tab.ID()
	}
	what := closeIntent(len(s.tabs), id)
	if s.guardClose(what) {
		s.proceedClose(what)
	}
}

func (s *Shell) onConfirmClose() {
	confirm := s.confirm
	if confirm == nil {
		return
	}
	s.confirm = nil
	s.proceedClose(confirm.what)
}

func (s *Shell) onShowHistory() {
	s.openHistory()
	// The history tab leaves the focus on the shell, where the shell's
	// own keys are bound. Saying so here matters because the tab this
	// one replaces may have been holding the focus in a popover.
	s.focusActiveTab()
}

// --- the run button ---------------------------------------------------

// releaseRunButton: the press ends, and nothing else happens: a release
// away from the button is not a click on it.
func (s *Shell) releaseRunButton() {
	if s.runPressed {
		s.runPressed = false
	}
}

// toggleRun: the button does whichever of the three things its label says
// — and nothing at all while it says "run" over a run that is already out.
// The word under the pointer is what the click has to mean: a click during
// the arming window would either start a second run over the first, which
// is refused anyway, or stop a run the button never offered to stop. ⌘. is
// unaffected: a key press is aimed at the run, not at a word on a button.
func (s *Shell) toggleRun() {
	if tab, ok := s.activeTab().(*QueryTab); ok {
		switch {
		case tab.run.arming():
			return
		case tab.run.inFlight():
			s.stopActiveQuery()
			return
		}
	}
	s.runActiveQuery()
}

// --- the pane sizes -----------------------------------------------------

func (s *Shell) paneSize(divider Divider) float32 {
	switch divider {
	case DividerSidebar:
		return s.sidebarWidth
	default:
		return s.editorHeight
	}
}

func (s *Shell) setPaneSize(divider Divider, size float32) {
	slot := &s.editorHeight
	if divider == DividerSidebar {
		slot = &s.sidebarWidth
	}
	if *slot != size {
		*slot = size
	}
}

// onDividerDown is the press that starts a drag — or, on a double click,
// puts the divider back where the comp drew it. The reset also ends any
// drag the first click of the pair began, or the pane would chase the
// pointer away from the default it was just given.
//
// Go port: Rust's `&MouseDownEvent` is the pointer's position, in window
// coordinates, and the click count.
func (s *Shell) onDividerDown(divider Divider, x, y float32, clicks int) {
	if clicks > 1 {
		s.paneDrag = nil
		s.setPaneSize(divider, divider.defaultSize())
		s.rememberPaneSize(divider)
		return
	}
	anchor := y
	if divider == DividerSidebar {
		anchor = x
	}
	s.paneDrag = &PaneDrag{
		divider: divider,
		anchor:  anchor,
		start:   s.paneSize(divider),
	}
}

// dragPaneTo follows the pointer during a divider drag.
//
// Go port: Rust's `Point<Pixels>` and `Size<Pixels>` are the pointer's
// window coordinates and the viewport's width and height.
func (s *Shell) dragPaneTo(x, y, viewportWidth, viewportHeight float32) {
	drag := s.paneDrag
	if drag == nil {
		return
	}
	at := y
	if drag.divider == DividerSidebar {
		at = x
	}
	min, max := drag.divider.rangeIn(viewportWidth, viewportHeight)
	s.setPaneSize(drag.divider, draggedSize(drag.start, drag.anchor, at, min, max))
}

// endPaneDrag: the button coming up anywhere ends the drag, and only then
// is the size written back: the store is local SQLite, but a write per
// mouse-move frame would still be a write per frame for nothing — the size
// on screen is already the truth for the whole drag.
func (s *Shell) endPaneDrag() {
	drag := s.paneDrag
	if drag == nil {
		return
	}
	s.paneDrag = nil
	s.rememberPaneSize(drag.divider)
}

func (s *Shell) rememberPaneSize(divider Divider) {
	if s.store != nil {
		size := roundF32(s.paneSize(divider))
		_ = s.store.SetUiValue(divider.key(), formatF32(size))
	}
}

// roundF32 is Rust's `f32::round`: half away from zero.
func roundF32(x float32) float32 {
	return float32(math.Round(float64(x)))
}

// formatF32 is Rust's `format!("{size}")` for an f32: the shortest digits
// that read back as the same value, never in exponent form and with no
// `.0` — a rounded size prints as `246`.
func formatF32(x float32) string {
	return strconv.FormatFloat(float64(x), 'f', -1, 32)
}
