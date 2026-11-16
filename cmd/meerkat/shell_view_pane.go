// The pane: the active tab's toolbars, strips, result and status line
// (shell.rs, agent K3b's part — see docs/tasks/shell-split.md).

package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/resultsgrid"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

func (s *Shell) pane(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	pane := ui.Column(c.Key("pane")).Grow(1).Basis(0).MinHeight(0).MinWidth(0)
	var tab Tab
	if s.active < len(s.tabs) {
		tab = s.tabs[s.active]
	}
	return pane.Children(func() {
		switch tab := tab.(type) {
		case *TableTab:
			s.tableToolbar(c, tab, colors)
			if tab.error != nil {
				s.errorStrip(c, tab.id, *tab.error, colors)
			}
			s.resultBody(c, tab.id, colors)
		case *QueryTab:
			s.queryPane(c, tab, colors)
		case *HistoryTab:
			s.historyPane(c, tab, colors)
		default:
			s.placeholder(c, colors)
		}
	})
}

// resultBody is the grid under the result line — or, where there is no
// result set, what to say instead of one. The grid sits in a focus and a
// key context of its own; both kinds of tab that show a result render
// through here, so the keys reach the grid the same way in each.
//
// **No columns, no grid.** A statement that changes rows describes no
// columns, and a `Grid` over none of them painted a bare gutter and an
// empty header rule: a table where there had never been a table, which
// is what made a working `UPDATE` look like a failure. The same shape
// showed on a tab that had run nothing at all.
func (s *Shell) resultBody(c *ui.Context, tabID uint64, colors *theme.ThemeColors) ui.Element {
	var tab Tab
	for _, candidate := range s.tabs {
		if candidate.ID() == tabID {
			tab = candidate
			break
		}
	}
	if tab == nil {
		return ui.Box(c)
	}
	var data *resultsgrid.GridData
	var selection *resultsgrid.Selection
	var scroll *resultsgrid.GridState
	firstRow := 1
	switch tab := tab.(type) {
	case *TableTab:
		data, selection, scroll = tab.data, &tab.selection, &tab.scroll
		// A page is a window on the table, so the gutter counts
		// from where the page starts rather than from 1 again.
		firstRow = tab.page*PageSize + 1
	case *QueryTab:
		data, selection, scroll = tab.data, &tab.selection, &tab.scroll
	default:
		return ui.Box(c)
	}
	if len(data.Columns) == 0 {
		// An open strip stays on screen over a result with nothing in
		// it — an `UPDATE`, a failed run — because it holds the focus,
		// and a line taken off the screen would take ⎋ with it.
		return ui.Column(c.Key("result-body")).
			Grow(1).Basis(0).
			MinHeight(0).
			Children(func() {
				s.resultFindStrip(c, tabID)
				noResultNote(c, tab, colors)
			})
	}

	return ui.Column(c.Key("result-body")).
		Grow(1).Basis(0).
		MinHeight(0).
		MinWidth(0).
		Children(func() {
			// The strip is a sibling of the grid's key context, never a
			// child: see `RESULT_FIND_KEY_CONTEXT`.
			_, strip := s.resultFindStrip(c, tabID)
			// Only an open strip paints its hits. `result_hits` answers
			// for a closed one too, because ⌘G steps without it, but a
			// wash nobody asked for would be one the user cannot put
			// away.
			var hits []resultsgrid.Cell
			if strip {
				hits, _ = s.resultHits()
			}
			grid := ui.Row(c.Key("result-grid")).
				Focusable().
				TabStop(false).
				Bind(&s.gridFocus).
				Grow(1).Basis(0).
				MinHeight(0).
				MinWidth(0).
				AlignItems(ui.Stretch)
			if action, ok := uikit.DispatchKeys(grid, gridViewKeys); ok {
				s.handleAction(action)
			}
			grid.Children(func() {
				resultsgrid.NewGrid(fmt.Sprintf("tab-%d", tabID), data, scroll, selection).
					FirstRow(firstRow).
					OnHit(s.hitHandler(tabID)).
					Hits(hits).
					Render(c)
			})
		})
}

// hitHandler is everything a click in the grid can mean, in one place.
//
// The grid reports where the mouse landed and what the modifiers said;
// what that does to the selection is decided here, and carried out by
// [`Selection`]'s own methods — so the mouse and the keys move the same
// selection through the same code.
func (s *Shell) hitHandler(tabID uint64) resultsgrid.OnHit {
	return func(hit resultsgrid.Hit) {
		// A click in the result is a click outside the column-find
		// popover, and a popover a click has landed behind is one
		// the user is done with. A drag is not a fresh click: the
		// press that started it closed the popover already, and
		// took the focus with it.
		_, dragging := hit.(resultsgrid.HitDrag)
		if !dragging {
			s.columnFind = nil
		}
		tab := s.tabMut(tabID)
		if tab == nil {
			return
		}
		marked := tab.Marked()
		if marked == nil {
			return
		}
		extent := resultsgrid.ExtentOf(marked.data)
		var askedToPeek *resultsgrid.Cell
		switch hit := hit.(type) {
		case resultsgrid.HitCell:
			if hit.Extend {
				marked.selection.ExtendTo(hit.Cell)
			} else {
				marked.selection.Focus(hit.Cell)
			}
			// The second click of a double click asks for the
			// whole value. The first one has already moved the
			// cursor there, so the card opens on the cell the
			// user is looking at either way.
			if hit.Peek {
				cell := hit.Cell
				askedToPeek = &cell
			}
		// The pointer dragging over the cells is the range
		// growing, which is what ⇧ with an arrow key does — so
		// it is the same call.
		case resultsgrid.HitDrag:
			marked.selection.ExtendTo(hit.Cell)
		case resultsgrid.HitPick:
			if hit.Through {
				marked.selection.PickThrough(hit.Row)
			} else {
				marked.selection.TogglePick(hit.Row)
			}
		case resultsgrid.HitPickAll:
			marked.selection.ToggleAllPicks(extent)
		case resultsgrid.HitColumn:
			marked.selection.SelectColumn(hit.Column, extent)
		}
		// A query tab types into its editor, which holds the focus
		// while the user is typing. A click in the result says the
		// grid is what the keys are for now, so the grid takes the
		// focus — and with it its own key context, which is what
		// makes a bare `space` mean "tick this row" here and a
		// space in the SQL there.
		if !dragging {
			s.gridFocus.Focus()
		}
		if askedToPeek != nil {
			// After the focus, not before: the card takes the focus
			// for itself, and ⎋ hands it back to the grid.
			s.openPeek(*askedToPeek)
		}
	}
}

func (s *Shell) tableToolbar(c *ui.Context, tab *TableTab, colors *theme.ThemeColors) ui.Element {
	columns := len(tab.data.Columns)
	if model := s.tableModel(tab.schema, tab.table); model != nil {
		columns = len(model.Columns)
	}
	rows := "row count unknown"
	if tab.approxRows != nil {
		rows = fmt.Sprintf("~%s rows", uikit.FormatCount(*tab.approxRows))
	}

	toolbar := ui.Row(c.Key("table-toolbar")).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(10).
		Padding(9, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Hairline.Color())
	// Go port: the popover hangs out of this row over the grid below,
	// which GPUI's `deferred` painted after it; a z-index over its
	// siblings does that here (see columnFindControl).
	if s.columnFindOpen() != nil {
		toolbar.ZIndex(1)
	}
	return toolbar.Children(func() {
		ui.Text(c, tab.table).
			FontSize(12).
			FontWeight(500).
			TextColor(colors.Text.Color())
		ui.Text(c, fmt.Sprintf("%s · %d columns", rows, columns)).
			FontSize(11).
			TextColor(colors.TextMuted.Color())
		if tab.loading {
			ui.Text(c, "loading…").
				FontSize(11).
				TextColor(colors.Accent.Color())
		}
		ui.Box(c).Grow(1).Basis(0)
		s.columnFindControl(c, colors)
		refresh := ui.Box(c.Key("refresh")).
			Padding(5, 9).
			Border(1, colors.BorderStrong.Color()).
			Radius(6).
			Background(colors.Elevated.Color()).
			FontSize(11).
			TextColor(colors.TextSecondary.Color()).
			Cursor(ui.CursorPointer).
			KeepFocus()
		if refresh.Hovered() {
			refresh.BorderColor(colors.TextFaint.Color())
		}
		refresh.OnClick(func() { s.refreshActive() })
		refresh.Children(func() { ui.Text(c, "refresh") })
	})
}

// columnFindOpen is the popover, when it is open over the active tab's
// result and neither dialog is up: `column_find_control`'s `open`.
func (s *Shell) columnFindOpen() *ColumnFind {
	if s.active >= len(s.tabs) {
		return nil
	}
	find := s.columnFind
	if find == nil || find.tab != s.tabs[s.active].ID() || s.palette != nil || s.confirm != nil {
		return nil
	}
	return find
}

// columnFindControl is the comp's "column ⌘J" button, and the popover it
// opens under itself. Both kinds of tab that show a result carry it: a
// result is as wide wherever it came from.
//
// Nothing is drawn while the result has no columns. A button that opened
// an empty list would be a control that says it can do something it
// cannot.
//
// The popover is `deferred`, which paints it after everything around it
// rather than under: GPUI paints siblings in order, and the toolbar is
// painted before the grid it hangs over.
//
// Go port: `deferred` is a z-index on the popover and on the row that
// holds the control (the caller's), so the popover paints and hit-tests
// over the grid while staying inside the shell's focus chain — a
// `ui.Overlay` would take it out of the chain, and ⌘J would no longer
// reach the shell from the popover's search line.
func (s *Shell) columnFindControl(c *ui.Context, colors *theme.ThemeColors) {
	if s.active >= len(s.tabs) {
		return
	}
	if len(s.resultColumns()) == 0 {
		return
	}
	// Only ever over the result it was opened on, and never under one of
	// the two dialogs — both of them are already the thing taking keys.
	open := s.columnFindOpen()
	// Open, the button wears the run timer's warm pill: the same "this is
	// live" reading, in the accent's family rather than a warning's.
	border, fill := colors.Border, colors.Window
	if open != nil {
		border, fill = colors.RunningBorder, colors.RunningSurface
	}

	wrapper := ui.Box(c.Key("find-column-control")).Shrink(0)
	if open != nil {
		wrapper.ZIndex(1)
	}
	wrapper.Children(func() {
		button := ui.Row(c.Key("find-column")).
			Height(28).
			AlignItems(ui.Center).
			Gap(7).
			Padding(0, 5, 0, 10).
			Border(1, border.Color()).
			Radius(7).
			Background(fill.Color()).
			FontSize(12).
			TextColor(colors.TextSecondary.Color()).
			Cursor(ui.CursorPointer)
		if button.Hovered() {
			button.BorderColor(colors.BorderStrong.Color())
		}
		button.OnClick(func() { s.toggleColumnFind() })
		button.Children(func() {
			uikit.DrawIcon(c, uikit.IconSearch, 12., colors.TextMuted)
			ui.Text(c, "Column")
			keyBadge(c, "⌘J", colors)
		})
		if open != nil {
			s.columnFindPopover(c, open, colors)
		}
	})
}

// columnFindPopover is the popover: the search line, what it found, and
// what the keys do.
func (s *Shell) columnFindPopover(c *ui.Context, find *ColumnFind, colors *theme.ThemeColors) ui.Element {
	matches := s.columnMatches()
	columns := s.resultColumns()
	count := fmt.Sprintf("%d/%d", len(matches), len(columns))

	popover := ui.Column(c.Key("column-find")).
		Absolute().
		Top(columnFindTop).
		// Hung from the button's right edge, not its left as the comp
		// draws it: the button sits at the right end of the toolbar
		// here, and 290px to the right of it is off the window.
		Right(0).
		Width(columnFindWidth).
		ZIndex(1).
		Clip().
		Border(1, colors.BorderStrong.Color()).
		Radius(9).
		Background(colors.Elevated.Color()).
		// A click in the popover is not a click in the grid under it,
		// and it leaves the focus on the search line.
		KeepFocus()
	popover.OnClick(func() {})
	uikit.BoxShadow{OffsetX: 0, OffsetY: 18, BlurRadius: 40, SpreadRadius: -14, Color: colors.Shadow}.Apply(popover)
	if action, ok := uikit.DispatchKeys(popover, columnFindViewKeys); ok {
		s.handleAction(action)
	}
	return popover.Children(func() {
		ui.Row(c).
			Shrink(0).
			AlignItems(ui.Center).
			Gap(8).
			Padding(9, 11).
			BorderWidth(0, 0, 1, 0).
			BorderColor(colors.Border.Color()).
			Children(func() {
				uikit.SearchGlyph(c, colors.TextFaint)
				ui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
					find.query.View(c)
				})
				// How much of the result is left, so a search that has
				// narrowed to one column says so without counting rows.
				ui.Text(c, count).
					FontSize(10).
					TextColor(colors.TextFaint.Color())
			})
		s.columnFindList(c, find, matches, colors)
		ui.Row(c).
			Shrink(0).
			AlignItems(ui.Center).
			Gap(10).
			Padding(7, 11).
			BorderWidth(1, 0, 0, 0).
			BorderColor(colors.Border.Color()).
			Background(colors.Panel.Color()).
			FontSize(9).
			TextColor(colors.TextMuted.Color()).
			Children(func() {
				ui.Text(c, "↑↓ browse")
				ui.Text(c, "⏎ jump")
				ui.Text(c, "esc close")
			})
	})
}

// columnFindList is what the search found, one column per row. The list
// grows to the matches and stops at nine rows: a popover is a way to one
// column, not a second view of the header.
func (s *Shell) columnFindList(c *ui.Context, find *ColumnFind, matches []int, colors *theme.ThemeColors) ui.Element {
	if len(matches) == 0 {
		return ui.Box(c).
			Shrink(0).
			Padding(10, 13).
			FontSize(11).
			TextColor(colors.TextFaint.Color()).
			Children(func() { ui.Text(c, "no column matches") })
	}

	columns := s.resultColumns()
	// Which lane the cursor is in, so the list can say so: the user is
	// being shown where they are as well as where they could go.
	cursor := -1
	if cell, ok := s.activeCursor(); ok {
		cursor = cell.Column
	}
	selected := min(find.selected, len(matches)-1)
	jump := func(column int) { s.jumpToColumn(column) }

	// The 5px of padding the list sits in counts towards the height the
	// comp gives the box, so it comes off the rows rather than being
	// added to them: a scrolling list must not be a half-row tall.
	const padding float32 = 5.
	height := minF32(float32(len(matches))*columnRowHeight, columnListMaxHeight-2.*padding)
	return ui.Column(c.Key("column-matches-box")).
		Height(height + 2.*padding).
		Shrink(0).
		Padding(padding).
		Children(func() {
			ui.List(c.Key("column-matches"), &find.scroll.List, len(matches), func(ix int) {
				column := matches[ix]
				columnRow(c, ix, column, columns[column], ix == selected, cursor, jump, colors)
			}).
				Grow(1).Basis(0).
				MinHeight(0).
				TrackScroll(&find.scroll.Handle.State)
		})
}

// columnRow is one row of the column-find list: where the column sits in
// the result, its name, and the one thing worth saying about it.
//
// The trailing mark says **`cursor`** for the column the cursor is already
// in and **⏎** for the one the key would jump to. They are never both:
// "you are here" outranks "you could go here", and a row that claimed both
// would be saying the jump goes nowhere.
//
// Go port: Rust's `(usize, SharedString)` is `column, name`; `cursor` is
// -1 for `None`.
func columnRow(
	c *ui.Context,
	ix int,
	column int,
	name string,
	selected bool,
	cursor int,
	jump func(column int),
	colors *theme.ThemeColors,
) ui.Element {
	mark, markInk := "", colors.Accent
	if cursor == column {
		mark, markInk = "cursor", colors.AccentDeep
	} else if selected {
		mark, markInk = "⏎", colors.Accent
	}

	row := ui.Row(c.Key(fmt.Sprintf("column-row-%d", ix))).
		Height(columnRowHeight).
		AlignItems(ui.Center).
		Gap(9).
		Padding(0, 8).
		Radius(5).
		Cursor(ui.CursorPointer)
	row.OnClick(func() { jump(column) })
	if selected {
		row.Background(colors.Selection.Color())
	} else if row.Hovered() {
		row.Background(colors.Panel.Color())
	}
	return row.Children(func() {
		// The lane's own number, counted from one as the header reads,
		// so the popover and the result agree about where a column is.
		ui.Box(c).
			Width(15).
			Shrink(0).
			FontSize(9).
			TextColor(colors.LineNumber.Color()).
			Children(func() { ui.Text(c, fmt.Sprintf("%d", column+1)) })
		nameInk := colors.TextSecondary
		if selected {
			nameInk = colors.Text
		}
		ui.Box(c).
			Grow(1).Basis(0).
			MinWidth(0).
			FontSize(11).
			TextColor(nameInk.Color()).
			Children(func() { ui.Text(c, name).SingleLine() })
		ui.Text(c, mark).
			FontSize(10).
			TextColor(markInk.Color())
	})
}

// runTimer is the timer beside the run button. It is the only thing on
// screen that says a slow query is still alive, so it is there for every
// state where the server has the statement — cancelling included — but
// not for the first second of one: it appears at `1.0 s` and counts from
// there, so a query that answers at once never raises it at all.
func (s *Shell) runTimer(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) {
	live := tab.run.live()
	if live == nil {
		return
	}
	waited := time.Since(live.started)
	if waited < timerDelay {
		return
	}
	ui.Row(c.Key("run-timer")).
		Shrink(0).
		Height(28).
		AlignItems(ui.Center).
		Gap(7).
		Padding(0, 10).
		Border(1, colors.RunningBorder.Color()).
		Radius(7).
		Background(colors.RunningSurface.Color()).
		Font(theme.MonoFontFamily).
		FontSize(12).
		TextColor(colors.AccentDeep.Color()).
		Children(func() {
			uikit.StatusDot(c, colors.RunningMark)
			ui.Text(c, uikit.FormatSeconds(uint64(waited.Milliseconds())))
		})
}

// txSwitch is the comp's `tx auto | manual` switch, in the query toolbar.
//
// Two chips and one of them is always lit: the mode is a choice
// between two states rather than a switch with an off position. The
// dot on `manual` is the quiet mark that a transaction is open — the
// bar under the editor is the loud one.
//
// **Auto does nothing while a transaction is open**, and paints faint
// to say so. Switching would leave the transaction standing with
// nothing offering to end it; the bar's two buttons are the way out.
func (s *Shell) txSwitch(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) ui.Element {
	manual := tab.txMode == dbclient.TxModeManual
	locked := tab.inTransaction
	chip := func(mode dbclient.TxMode, lit, faint bool, extra func()) {
		weight, ink := 400, colors.TextSecondary
		if lit {
			weight, ink = 500, colors.Text
		} else if faint {
			ink = colors.TextFaint
		}
		label := "Auto"
		if mode == dbclient.TxModeManual {
			label = "Manual"
		}
		el := ui.Row(c.Key("tx-"+mode.AsStr())).
			Height(24).
			AlignItems(ui.Center).
			Gap(6).
			Padding(0, 10).
			Radius(5).
			FontSize(12.5).
			FontWeight(weight).
			TextColor(ink.Color()).
			KeepFocus()
		if lit {
			// The lit chip is lifted out of the track, the way a
			// segmented control says which segment is chosen.
			el.Background(colors.Raised.Color())
			uikit.BoxShadow{OffsetX: 0, OffsetY: 1, BlurRadius: 2, Color: colors.ShadowSoft}.Apply(el)
		} else if !faint && el.Hovered() {
			el.TextColor(colors.Text.Color())
		}
		if !faint {
			el.Cursor(ui.CursorPointer)
			el.OnClick(func() { s.setTxMode(mode) })
		}
		el.Children(func() {
			ui.Text(c, label)
			if extra != nil {
				extra()
			}
		})
	}
	border, fill, labelInk := colors.Sunk, colors.Sunk, colors.TextMuted
	if manual {
		border, fill, labelInk = colors.RunningBorder, colors.RunningSurface, colors.AccentMuted
	}
	return ui.Row(c.Key("tx-switch")).
		Shrink(0).
		Height(30).
		AlignItems(ui.Center).
		Gap(2).
		Padding(2).
		Border(1, border.Color()).
		Radius(7).
		Background(fill.Color()).
		Children(func() {
			ui.Box(c).
				Padding(0, 4, 0, 6).
				FontSize(12).
				TextColor(labelInk.Color()).
				Children(func() { ui.Text(c, "Transaction") })
			chip(dbclient.TxModeAuto, !manual, locked, nil)
			var dot func()
			if manual {
				dot = func() {
					ink := colors.RunningBorder
					if tab.inTransaction {
						ink = colors.RunningMark
					}
					uikit.StatusDot(c, ink)
				}
			}
			chip(dbclient.TxModeManual, manual, false, dot)
		})
}

// schemaStrip is the comp's SCHEMA CHANGED panel: what the run did to the
// shape of the database, read back off the catalog. See [`SchemaReport`]
// for why it exists and where its numbers come from.
//
// It wears the DDL family — the teal the statement's own lines wear
// in the editor — so the statement and its report read as one event.
// A column that appeared is a `+` row on the green wash, one that
// went is a `−` row on the clay wash with its name struck through,
// and one that changed is a `~` row saying `text → integer`. The line
// under them says whether any of it can still be taken back, which is
// the one sentence a shape change is owed.
func (s *Shell) schemaStrip(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) {
	report := tab.schemaReport
	if report == nil {
		return
	}
	verbs := reportVerbs(report.statements)
	subject, reveal := reportSubject(&report.delta)
	meta := fmt.Sprintf("%s · %s", verbs, uikit.FormatMillis(report.ms))
	if report.pending {
		meta = fmt.Sprintf("%s · %s · reading the catalog…", verbs, uikit.FormatMillis(report.ms))
	}
	dropped := report.delta.DroppedNames()
	var note string
	var noteInk theme.Hsla
	switch {
	case report.error != nil:
		note, noteInk = fmt.Sprintf("could not read the catalog back · %s", *report.error), colors.Error
	case report.pending:
		note, noteInk = "", colors.DdlMuted
	case report.delta.IsEmpty():
		note = fmt.Sprintf(
			"%s · nothing the sidebar lists changed — grants, comments and function bodies are not compared",
			schemaNote(report.held, nil),
		)
		noteInk = colors.DdlMuted
	default:
		note = schemaNote(report.held, dropped)
		noteInk = colors.Error
		if report.held || len(dropped) == 0 {
			noteInk = colors.DdlMuted
		}
	}

	ui.Row(c.Key("schema-strip")).
		Shrink(0).
		AlignItems(ui.Stretch).
		Background(colors.DdlSurface.Color()).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.DdlInner.Color()).
		Children(func() {
			ui.Box(c).Width(2).Shrink(0).Background(colors.Ddl.Color())
			ui.Column(c).
				Grow(1).Basis(0).
				MinWidth(0).
				Padding(11, 14, 12, 13).
				Gap(8).
				Children(func() {
					ui.Row(c).
						AlignItems(ui.Center).
						Gap(9).
						Children(func() {
							ui.Row(c).
								Size(14, 14).
								Shrink(0).
								Radius(3).
								Background(colors.Ddl.Color()).
								AlignItems(ui.Center).
								Justify(ui.Center).
								FontSize(9).
								FontWeight(600).
								TextColor(colors.DdlSurface.Color()).
								Children(func() { ui.Text(c, "Δ") })
							ui.Text(c, "SCHEMA CHANGED").
								FontSize(9).
								FontWeight(600).
								TextColor(colors.DdlText.Color())
							if subject != nil {
								ui.Box(c).
									MinWidth(0).
									FontSize(11).
									FontWeight(500).
									TextColor(colors.Text.Color()).
									Children(func() { ui.Text(c, *subject).SingleLine() })
							}
							ui.Text(c, meta).
								FontSize(10).
								TextColor(colors.DdlMuted.Color())
							ui.Box(c).Grow(1).Basis(0)
							if reveal != nil {
								schema, name := reveal[0], reveal[1]
								link := ui.Box(c.Key("reveal-relation")).
									Shrink(0).
									Cursor(ui.CursorPointer).
									FontSize(10).
									TextColor(colors.DdlText.Color())
								if link.Hovered() {
									link.TextColor(colors.Ddl.Color())
								}
								link.OnClick(func() { s.revealRelation(schema, name) })
								link.Children(func() { ui.Text(c, "reveal in schema tree") })
							}
						})
					deltaRowsView(c, &report.delta, colors)
					if note != "" {
						ui.Text(c, note).FontSize(10).TextColor(noteInk.Color())
					}
				})
		})
}

// connectStrip is the connect's own strip, over the pane: where the
// connect is going while it is out, and what went wrong when it fails.
//
// A session opens on its tabs before the connection lands, so the
// pane cannot be what reports the connect — it is busy being an
// editor. The foot of the sidebar said "connecting…" and "not
// connected" in the faint ink, which is where a host name belongs and
// not where a failure does: a window that will never run a query has
// to say so where the user is looking. So the strip sits over
// whichever tab is open and pushes it down, as the ⌘F strip does,
// rather than covering it: the SQL under it is still the user's.
//
// A failure is three lines and two ways out. The **headline** is the
// class, the **detail** is the server's or the system's own words in
// the mono — it is what gets pasted into a ticket, so it carries the
// `copy` link the error strip has — and the **hint** is where to
// look first. **Retry** asks again in place; **Back to connections**
// is the title bar's own way out, through the same guard.
//
// Connected, there is no strip: a strip that said "connected" would
// be one that is always there saying nothing.
func (s *Shell) connectStrip(c *ui.Context, colors *theme.ThemeColors) {
	var connectError dbclient.ConnectError
	switch status := s.status.(type) {
	case StatusConnected:
		return
	case StatusConnecting:
		if time.Since(s.connectStarted) < connectArm {
			return
		}
		ui.Row(c.Key("connect-strip")).
			Shrink(0).
			AlignItems(ui.Center).
			Gap(10).
			Padding(9, 14).
			BorderWidth(0, 0, 1, 0).
			BorderColor(colors.RunningBorder.Color()).
			Background(colors.RunningSurface.Color()).
			Children(func() {
				uikit.StatusDot(c, colors.RunningMark)
				ui.Text(c, "Connecting to").
					FontSize(12).
					FontWeight(500).
					TextColor(colors.AccentDeep.Color())
				ui.Box(c).
					MinWidth(0).
					Font(theme.MonoFontFamily).
					FontSize(11.5).
					TextColor(colors.AccentDeep.Color()).
					Children(func() { ui.Text(c, status.To+"…").SingleLine() })
			})
		return
	case StatusFailed:
		connectError = status.Error
	default:
		return
	}
	detail := connectError.Detail
	copied := s.copiedError != nil && s.copiedError.tab == connectCopyID && s.copiedError.message == detail
	ui.Row(c.Key("connect-strip")).
		Shrink(0).
		AlignItems(ui.Start).
		Gap(16).
		Padding(12, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.ErrorBorder.Color()).
		Background(colors.ErrorSurface.Color()).
		Children(func() {
			ui.Box(c).Shrink(0).Padding(5, 0, 0, 0).Children(func() {
				uikit.StatusDot(c, colors.ErrorMark)
			})
			ui.Column(c).
				Grow(1).Basis(0).
				MinWidth(0).
				Gap(5).
				Children(func() {
					ui.Text(c, connectError.Kind.Headline()).
						FontSize(13).
						FontWeight(600).
						TextColor(colors.Error.Color())
					ui.Row(c).
						AlignItems(ui.Start).
						Gap(10).
						Children(func() {
							ui.Box(c).
								MinWidth(0).
								Shrink(1).
								Font(theme.MonoFontFamily).
								FontSize(11.5).
								TextColor(colors.Error.Color()).
								Children(func() { ui.Text(c, detail) })
							copyLink(c, "copy-connect-error", copied, colors, func() {
								s.copyError(connectCopyID, detail)
							})
						})
					ui.Text(c, connectError.Kind.Hint()).
						FontSize(12).
						TextColor(colors.TextSecondary.Color())
				})
			ui.Row(c).
				Shrink(0).
				AlignItems(ui.Center).
				Gap(8).
				Children(func() {
					retry := uikit.AccentButton(c.Key("retry-connect"), "Retry")
					retry.OnClick(func() { s.retryConnect() })
					back := uikit.ToolbarButton(c.Key("back-to-connections"), "Back to connections")
					back.OnClick(func() {
						// The title bar's own way out, and its
						// guard: nothing is running here, so it
						// does not ask, but one path means the
						// strip is written back either way.
						if s.guardClose(Close{Kind: CloseKindShell}) {
							s.proceedClose(Close{Kind: CloseKindShell})
						}
					})
				})
		})
}

// copyLink is the `copy` link both error strips carry, which says
// `copied` while the message on the clipboard is still this one.
//
// Go port: the Rust wrote it out in each strip; it is one element here so
// the two cannot drift.
func copyLink(c *ui.Context, id string, copied bool, colors *theme.ThemeColors, onCopy func()) ui.Element {
	label := "copy"
	if copied {
		label = "copied"
	}
	link := ui.Box(c.Key(id)).
		Shrink(0).
		Cursor(ui.CursorPointer).
		FontSize(10).
		TextColor(colors.ErrorSecondary.Color()).
		KeepFocus()
	if link.Hovered() {
		link.TextColor(colors.Error.Color())
	}
	link.OnClick(onCopy)
	return link.Children(func() { ui.Text(c, label) })
}

// errorStrip is what the server said when it refused a run, with a `copy`
// link on the right. The text of a GPUI element cannot be marked with the
// pointer, and an error is the one message on screen most often wanted
// somewhere else — a ticket, a search, a colleague.
func (s *Shell) errorStrip(c *ui.Context, tabID uint64, message string, colors *theme.ThemeColors) ui.Element {
	copied := s.copiedError != nil && s.copiedError.tab == tabID && s.copiedError.message == message
	return ui.Row(c.Key(fmt.Sprintf("error-strip-%d", tabID))).
		Shrink(0).
		AlignItems(ui.Start).
		Gap(12).
		Padding(9, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.ErrorBorder.Color()).
		Background(colors.ErrorSurface.Color()).
		FontSize(11).
		TextColor(colors.Error.Color()).
		Children(func() {
			ui.Box(c).Grow(1).Basis(0).MinWidth(0).Children(func() { ui.Text(c, message) })
			copyLink(c, fmt.Sprintf("copy-error-%d", tabID), copied, colors, func() {
				s.copyError(tabID, message)
			})
		})
}

// txBar is the strip over the result: what the open transaction holds,
// and the two ways to end it.
//
// It is painted **whichever mode the tab is in**. A `BEGIN` the user
// typed into an auto-mode buffer opens a transaction exactly as manual
// mode does, and leaving that unmarked was the old behaviour's real
// gap: the transaction was on screen only as a badge, and the only way
// out of it was to close the tab, which rolled it back.
//
// It also stays up for a moment after the transaction ends, saying
// which way it went. "committed" is the answer to the question the
// user just asked, and a strip that vanished would leave it
// unanswered.
func (s *Shell) txBar(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) {
	state := TxState{
		mode:       tab.txMode,
		open:       tab.inTransaction,
		done:       tab.txDone,
		statements: tab.txTally.statements,
		affected:   tab.txTally.affected,
		running:    tab.run.inFlight(),
		ending:     tab.txEnding,
	}
	title, sub, ok := txCopy(state)
	if !ok {
		return
	}
	// Open warms to the accent's family, a commit rests in the dev
	// green the read-only mark already uses, and a rollback goes back
	// to paper: nothing was written, so nothing is worth a colour.
	var surface, border, ink, subInk, dot theme.Hsla
	switch {
	case state.open:
		surface, border, ink, subInk, dot = colors.RunningSurface, colors.RunningBorder,
			colors.AccentDeep, colors.AccentMuted, colors.RunningMark
	case state.done != nil && *state.done == dbclient.TxEndCommit:
		surface, border, ink, subInk, dot = colors.EnvDevSurface, colors.EnvDevInner,
			colors.EnvDevText, colors.TextMuted, colors.EnvDev
	default:
		surface, border, ink, subInk, dot = colors.Panel, colors.BorderStrong,
			colors.TextSecondary, colors.TextMuted, colors.TextFaint
	}
	// A boundary needs the session, and a run is holding it. The
	// buttons go faint rather than away: the transaction is still
	// there, and so is the answer to it once ⌘. has landed.
	ready := state.open && !state.running && !state.ending

	ui.Row(c.Key("tx-bar")).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(10).
		Padding(9, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(border.Color()).
		Background(surface.Color()).
		Children(func() {
			uikit.StatusDot(c, dot)
			ui.Text(c, title).
				FontSize(11).
				FontWeight(500).
				TextColor(ink.Color())
			ui.Text(c, sub).FontSize(10).TextColor(subInk.Color())
			ui.Box(c).Grow(1).Basis(0)
			if state.open {
				s.txButton(c, dbclient.TxEndRollback, ready, colors)
				s.txButton(c, dbclient.TxEndCommit, ready, colors)
			}
		})
}

// txButton is one of the bar's two buttons. Commit is filled in the
// accent, as the deliberate answer; rollback is outlined over paper,
// because discarding work the user has not seen the point of yet must not
// be the button the eye lands on first.
//
// The key sits in a **keycap**, the same one the run button carries.
// These three are the app's only buttons with a key written on them,
// and a key that reads as a key in one of them and as trailing text in
// the others would say the three were different kinds of thing. The
// tones follow the fill, as they do there: paper over the outlined
// button, and the white-on-fill pair over the filled one.
func (s *Shell) txButton(c *ui.Context, how dbclient.TxEnd, ready bool, colors *theme.ThemeColors) ui.Element {
	label, keys := "rollback", "⇧⌘R"
	border, fill, ink, capSurface, capBorder := colors.BorderStrong, colors.Elevated,
		colors.TextSecondary, colors.Window, colors.ModeOffBorder
	if how == dbclient.TxEndCommit {
		label, keys = "commit", "⌘S"
		border, fill, ink, capSurface, capBorder = colors.Accent, colors.Accent,
			colors.Window, colors.KeyOnFillSurface, colors.KeyOnFillBorder
	}
	button := ui.Row(c.Key("tx-"+label)).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(6).
		// Less room on the right than on the left: the keycap carries a
		// box of its own, and even padding either side of it reads as
		// too much. The run button is spaced the same way.
		//
		// **Everything here is a size under the run button's**, and the
		// difference is the point: that one is the verb of the whole
		// pane and these two are controls on a strip inside it. A
		// rollback drawn as large as a run would read as the bigger
		// decision, which it is not.
		Padding(3, 4, 3, 8).
		Border(1, border.Color()).
		Radius(5).
		Background(fill.Color()).
		KeepFocus()
	button.Children(func() {
		text := ui.Text(c, label).FontSize(10).TextColor(ink.Color())
		if how == dbclient.TxEndCommit {
			text.FontWeight(500)
		}
		keycap(c, keys, 9., capSurface, capBorder, ink)
	})
	if !ready {
		return button.Opacity(0.5)
	}
	if button.Hovered() {
		if how == dbclient.TxEndCommit {
			button.Background(colors.AccentDeep.Color())
		} else {
			button.BorderColor(colors.TextFaint.Color())
		}
	}
	button.Cursor(ui.CursorPointer)
	button.OnClick(func() { s.endTransaction(how) })
	return button
}

// runButton is one button for all three verbs, as the comp draws it:
// **run** filled in the accent, **stop** outlined in clay over paper,
// **terminate** filled in clay. The escalation is the point — the button
// that ends a backend must not look like the button that starts a query.
//
// **It also moves, and it moves in exactly one way: its own fill.**
// While a run is out the fill breathes — a little of the button's ink
// mixed in and back out again, `RUN_BREATH` for the cycle. When the
// rows land the same mix eases out to nothing, so a run reads as one
// gesture that starts and finishes rather than as an effect that plays.
// And a press mixes the other way, toward the app's ink.
//
// Nothing travels across the button, nothing blooms out of it, and
// nothing moves by a pixel. See `RUN_BREATH` for why the comp's
// sweeping front and looping band are not here.
//
// The breath and the settle are GPUI animations rather than ticks of
// the shell's timer. `with_animation` asks for its own frames, restarts
// when the element's id changes — which is the whole of how a landed
// result gets its exhale — and holds still when the platform says the
// user wants less motion.
func (s *Shell) runButton(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) ui.Element {
	phase := tab.run.phase()
	verb, keys := "Run", "⌘⏎"
	switch phase {
	case RunPhaseStop:
		verb, keys = "Stop", "⌘."
	case RunPhaseTerminate:
		verb, keys = "Terminate", "⌘."
	}
	pressed := s.runPressed
	// Paper under clay for "stop": the one state where the button is
	// outlined rather than filled, so a run in flight reads as a
	// question rather than as a command already given.
	var border, resting, ink, capSurface, capBorder theme.Hsla
	switch phase {
	case RunPhaseStop:
		border, resting, ink, capSurface, capBorder = colors.EnvProd, colors.Window,
			colors.EnvProdText, colors.EnvProdSurface, colors.EnvProdInner
	case RunPhaseTerminate:
		border, resting, ink, capSurface, capBorder = colors.EnvProd, colors.EnvProd,
			colors.OnAccent, colors.KeyOnFillSurface, colors.KeyOnFillBorder
	default:
		border, resting, ink, capSurface, capBorder = colors.AccentFill, colors.AccentFill,
			colors.OnAccent, colors.KeyOnFillSurface, colors.KeyOnFillBorder
	}
	// One formula for all three of the fill's moods, so that the
	// breath, the settle and the press cannot drift into three
	// unrelated effects: mix something into the resting fill.
	//
	// **Which way it mixes is the whole of what it says.** Toward the
	// button's own ink is lighter on a filled button and warmer on the
	// outlined one — that is "working", and it reads on every state
	// without a tone per state. Toward the app's ink is darker
	// everywhere, and that is "held down".
	fill := resting
	if pressed {
		fill = resting.Blend(colors.Text.Opacity(runPressDepth))
	}

	button := ui.Row(c.Key("run-query")).
		Shrink(0).
		Height(30).
		AlignItems(ui.Center).
		Gap(8).
		Padding(0, 5, 0, 12).
		Border(1, border.Color()).
		Radius(7).
		Background(fill.Color()).
		Cursor(ui.CursorPointer).
		KeepFocus()
	// **The press is the click here, and `on_click` is not used.**
	// GPUI keeps a click's half-finished state under the element's
	// id, and this element's id path carries whichever animation is
	// running — so a button held down across the moment a result
	// lands would come up under a different id and lose the click.
	// `run_pressed` is on the shell, where no animation can move it.
	//
	// Go port: the press, the release over the button and the release
	// anywhere else (`on_mouse_up_out`) all reach the element that took
	// the press. Whether the release is over the button is read off the
	// box it had in the frame before.
	bounds := button.Bounds()
	button.HandleInput(func(ev ui.InputEvent) bool {
		switch ev.Kind {
		case ui.InputPointerDown:
			if ev.Button != 0 {
				return false
			}
			s.runPressed = true
			return true
		case ui.InputPointerMove:
			// A move with no button held is a release nothing here
			// heard about.
			if s.runPressed && ev.Button != 0 {
				s.releaseRunButton()
			}
			return false
		case ui.InputPointerUp:
			if ev.Button != 0 {
				return false
			}
			over := !ev.Canceled && ev.X >= 0 && ev.Y >= 0 && ev.X < bounds.W && ev.Y < bounds.H
			if over && s.runPressed {
				s.releaseRunButton()
				s.toggleRun()
			} else {
				// A press that comes up somewhere else is not a click,
				// but it is still the end of the press — and nothing
				// else would say so, which would leave the button held
				// down for good.
				s.releaseRunButton()
			}
			return true
		}
		return false
	})
	button.Children(func() {
		if phase == RunPhaseReady {
			uikit.PlayGlyph(c, ink)
		} else {
			uikit.StopGlyph(c, ink)
		}
		// Held at one width across the verbs, so that a run turning
		// out to be slow does not resize the button under the
		// pointer that is reaching for it.
		width := runLabelWidth
		if phase == RunPhaseTerminate {
			width = terminateLabelWidth
		}
		ui.Box(c).
			// Fixed *and* `flex_none`: a width alone still leaves a
			// flex item shrinkable, so a toolbar tight for room
			// would take the pixels back out of the word.
			Shrink(0).
			Width(width).
			FontSize(12.5).
			FontWeight(600).
			TextColor(ink.Color()).
			Children(func() { ui.Text(c, verb).NoWrap() })
		// The keycap does not move on a press: the tone says the press
		// landed, and a keycap sinking a pixel is the one thing on this
		// button that would still read as a mechanism.
		keycap(c, keys, 10.5, capSurface, capBorder, ink)
	})

	// A press holds the button still at its pressed tone: what is under
	// the pointer must not also be breathing.
	if pressed {
		return button
	}
	if tab.run.inFlight() {
		// Phase-locked to the app's own clock, so a breath does
		// not restart from nothing every time something else on
		// screen rebuilds this element.
		breath := uikit.NewAnimation(runBreath).
			RepeatSynced().
			WithEasing(uikit.PulsatingBetween(0., 1.))
		anim := uikit.StartPaintAnimation(c, button, "run-breath", breath)
		return breatheFill(button, func(p *ui.Painter) theme.Hsla {
			return resting.Blend(ink.Opacity(runBreathDepth * anim.Delta(p, runBreathFps)))
		})
	}
	// `landed == 0` is a tab that has never answered, and the first
	// paint of every window is exactly that: nothing to exhale.
	if tab.landed == 0 {
		return button
	}
	settle := uikit.NewAnimation(runSettle).WithEasing(uikit.EaseOutQuint())
	anim := uikit.StartPaintAnimation(c, button, fmt.Sprintf("run-settle-%d", tab.landed), settle)
	return breatheFill(button, func(p *ui.Painter) theme.Hsla {
		return resting.Blend(ink.Opacity(runSettleDepth * (1. - anim.Delta(p, 0))))
	})
}

// runBreathFps is how often the breath repaints. It is a change of tone
// over `RUN_BREATH`, 2.6 seconds, and 30 frames a second is as smooth to
// the eye as the display's 120.
const runBreathFps = 30

// breatheFill paints the run button's moving fill inside its own `Draw`,
// from the painter's clock, rather than as the element's background.
//
// Go port: GPUI's `with_animation` re-ran the element's closure every
// frame, and the port's `AnimationDelta` did the same by asking for a
// frame — a build of the whole shell at the display's rate for as long as
// a run was out, all to change one button's tone. The view now builds the
// button once, and the frames the breath and the settle ask for are
// repaints that paint it again with a new tone. The fill sits inside the
// 1px border, where the background would have been, and the border the
// element paints with its background stays as it was.
func breatheFill(button ui.Element, fill func(p *ui.Painter) theme.Hsla) ui.Element {
	return button.Draw(func(p *ui.Painter, r ui.Rect) {
		inner := ui.Rect{X: r.X + 1, Y: r.Y + 1, W: r.W - 2, H: r.H - 2}
		p.Fill(inner, fill(p).Color(), 6)
	})
}

func (s *Shell) queryPane(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) {
	// The design's four result lines. Each one says what is true of the
	// run *now*, so the line and the button never disagree.
	var label, summary string
	var ink theme.Hsla
	total := ""
	if tab.timing != nil {
		total = uikit.FormatMillis(tab.timing.totalMs)
	}
	switch tab.run.kind {
	// The driver streams the rows, but it hands the result over in
	// one piece at the end, so there is still no row count to report
	// while a run is out. The line says what it can: the run is
	// alive, and how to end it. A count would need the driver to
	// report progress, not merely to stream.
	// With no backend the tab's session is still opening, and the
	// line must not claim the server has anything: it does not.
	case RunRunning:
		label, ink = "Running", colors.AccentDeep
		if tab.run.flight.backend == nil {
			summary = "opening this tab's session · ⌘. calls it off"
		} else {
			summary = "the server has the statement · ⌘. stops it"
		}
	case RunCancelling:
		label, ink = "Cancelling", colors.AccentDeep
		if backend := tab.run.flight.backend; backend == nil {
			summary = "the run was called off before it left"
		} else {
			summary = fmt.Sprintf("cancel sent to backend pid %d · "+
				"waiting for the server to acknowledge", int32(*backend))
		}
	case RunCancelled:
		label, ink = "Cancelled", colors.Error
		summary = fmt.Sprintf("stopped after %s · no rows kept", uikit.FormatSeconds(tab.run.elapsed))
	default:
		switch {
		case tab.hasResult:
			statements := ""
			if tab.statementsRun > 1 {
				statements = fmt.Sprintf("%d statements · last result · ", tab.statementsRun)
			}
			// One buffer can do both — `UPDATE …; SELECT …;` — and the
			// line has to say both: the grid below shows the query's
			// rows and says nothing whatever about the rows the update
			// wrote.
			changed := ""
			if tab.changed != nil {
				changed = changedCopy(*tab.changed) + " · "
			}
			label, ink = "Result", colors.TextMuted
			// The whole wait, and only that. The split into server and
			// lag lives on the status strip, which is already the line
			// about what is on screen — saying it twice would crowd
			// both.
			summary = fmt.Sprintf("%s%d rows · %d columns · %s%s",
				statements, len(tab.data.Rows), len(tab.data.Columns), changed, total)
		// The run worked and there is no result set: every statement in
		// the buffer answered with a count. "RESULT" would be the wrong
		// word over a pane with no result in it — the statement is done,
		// and what it did is the count.
		case tab.changed != nil:
			statements := ""
			if tab.statementsRun > 1 {
				statements = fmt.Sprintf("%d statements · ", tab.statementsRun)
			}
			label, ink = "Done", colors.TextMuted
			summary = fmt.Sprintf("%s%s · %s", statements, changedCopy(*tab.changed), total)
		default:
			label, summary, ink = "Result", "not run yet", colors.TextMuted
		}
	}

	// Query toolbar. The run button leads, because it is what the
	// toolbar is for; the tab's name and the session's mode used to
	// lead, and both now live in the title bar over it.
	ui.Row(c.Key("query-toolbar")).
		Height(46).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(10).
		Padding(0, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Hairline.Color()).
		Children(func() {
			s.runButton(c, tab, colors)
			s.txSwitch(c, tab, colors)
			s.runTimer(c, tab, colors)
			// **Everything after the controls gives up room before
			// any of them does.** `min_w(0)` on each child is what
			// makes that possible: a text element's automatic
			// minimum is its own text, so without it these cannot
			// shrink at all, and the row overflows rather than
			// truncating. It is the same pairing every cell in the
			// grid needs.
			ui.Row(c).
				Grow(1).Basis(0).
				MinWidth(0).
				AlignItems(ui.Center).
				Justify(ui.End).
				Gap(10).
				FontSize(12).
				Children(func() {
					sessionMark(c, tab, colors)
					// With several statements in the buffer, say which
					// one a run would send, so ⌘⏎ never comes as a
					// surprise.
					if scope, ok := runScope(tab); ok {
						ui.Box(c).
							MinWidth(0).
							TextColor(colors.TextMuted.Color()).
							Children(func() { ui.Text(c, scope).SingleLine() })
					}
				})
		})
	ui.Column(c.Key("query-editor")).
		// For the divider, which hangs off this box's bottom edge
		// and paints after the editor, so the border between the
		// editor and the result is where the drag begins.
		Height(s.editorHeight).
		Shrink(0).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.BorderStrong.Color()).
		Children(func() {
			tab.editor.View(c)
			s.paneHandle(c, DividerEditor, colors)
		})
	// First under the editor, because it is about what a statement up
	// there did to the database — the one answer the result pane has
	// no way to show.
	s.schemaStrip(c, tab, colors)
	// Under the editor, because it is about the statement above it —
	// and above the run's own error strip, which is about a statement
	// that was actually sent.
	syntaxStrip(c, tab, colors)
	if tab.error != nil {
		s.errorStrip(c, tab.id, *tab.error, colors)
	}
	// Directly over the result, because it is about what the runs have
	// done rather than about the statement above them.
	s.txBar(c, tab, colors)
	// Result header
	header := ui.Row(c.Key("result-header")).
		Height(40).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(10).
		Padding(0, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Border.Color()).
		Background(colors.Panel.Color())
	// Go port: the column-find popover hangs out of this row over the
	// grid (see columnFindControl).
	if s.columnFindOpen() != nil {
		header.ZIndex(1)
	}
	header.Children(func() {
		// A plain result is named in ink; a run that is out
		// or was stopped names itself in its own tone.
		labelInk := ink
		if ink == colors.TextMuted {
			labelInk = colors.Text
		}
		ui.Text(c, label).
			FontSize(13).
			FontWeight(600).
			TextColor(labelInk.Color())
		ui.Box(c).
			MinWidth(0).
			Font(theme.MonoFontFamily).
			FontSize(11.5).
			TextColor(ink.Color()).
			Children(func() { ui.Text(c, summary).SingleLine() })
		capNote(c, tab, colors)
		ui.Box(c).Grow(1).Basis(0)
		// The way to a column of the result, on the line that
		// describes the result — not on the toolbar above the
		// editor, which is about the statement.
		s.columnFindControl(c, colors)
	})
	s.resultBody(c, tab.id, colors)
}

// historyPane is the history screen: a heading, the comp's two chips, and
// the list of runs under their day.
func (s *Shell) historyPane(c *ui.Context, tab *HistoryTab, colors *theme.ThemeColors) {
	tabID := tab.id

	ui.Row(c.Key("history-header")).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(10).
		Padding(9, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Hairline.Color()).
		Children(func() {
			ui.Text(c, "Query history").
				FontSize(12).
				FontWeight(500).
				TextColor(colors.Text.Color())
			ui.Text(c, fmt.Sprintf("%s · last %d days", s.sessionName(), historyDays)).
				FontSize(11).
				TextColor(colors.TextMuted.Color())
			ui.Box(c).Grow(1).Basis(0)
			s.historySearchBox(c, tab, colors)
			HistoryFilterChip(c, "mine-only", "mine only", tab.userOnly, colors).
				KeepFocus().
				OnClick(func() { s.toggleHistoryFilter(tabID, false) })
			HistoryFilterChip(c, "errors-only", "errors", tab.errorsOnly, colors).
				KeepFocus().
				OnClick(func() { s.toggleHistoryFilter(tabID, true) })
		})
	if tab.error != nil {
		s.errorStrip(c, tab.id, *tab.error, colors)
	}
	s.historyList(c, tab, colors)
}

// historySearchBox is the history screen's search line, drawn the way the
// sidebar's filter line is: a hairline box, the magnifier, the bare field,
// and the ⌘F cap while it is empty — a gesture nothing on screen names is
// a gesture nobody finds.
func (s *Shell) historySearchBox(c *ui.Context, tab *HistoryTab, colors *theme.ThemeColors) {
	field := s.historySearch(tab)
	ui.Row(c.Key("history-search")).
		Width(280).
		Height(28).
		AlignItems(ui.Center).
		Gap(8).
		Padding(0, 6, 0, 9).
		Border(1, colors.Border.Color()).
		Radius(7).
		Background(colors.Window.Color()).
		Children(func() {
			uikit.DrawIcon(c, uikit.IconSearch, 12., colors.TextMuted)
			ui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
				field.View(c)
			})
			if field.IsEmpty() {
				uikit.Keycap(c, "⌘F")
			}
		})
}

func (s *Shell) historyList(c *ui.Context, tab *HistoryTab, colors *theme.ThemeColors) ui.Element {
	if len(tab.rows) == 0 {
		note := "nothing run yet on this connection"
		if tab.userOnly || tab.errorsOnly {
			note = "no runs match these filters"
		}
		if tab.search != nil && !tab.search.IsEmpty() {
			note = "no statement holds “" + tab.search.Trimmed() + "”"
		}
		return ui.Column(c.Key("history")).
			Grow(1).Basis(0).
			MinHeight(0).
			Padding(14, 16, 0, 16).
			FontSize(11).
			TextColor(colors.TextFaint.Color()).
			Children(func() { ui.Text(c, note) })
	}

	rows := tab.rows
	open := func(statement string) { s.newQueryWith(statement) }
	return ui.List(c.Key("history"), &tab.scroll.List, len(rows), func(ix int) {
		HistoryRowView(c, ix, rows[ix], open, colors)
	}).
		Grow(1).Basis(0).
		MinHeight(0).
		Padding(6, 16, 0, 16).
		TrackScroll(&tab.scroll.Handle.State)
}

func (s *Shell) placeholder(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	var heading, detail string
	failed := false
	switch status := s.status.(type) {
	case StatusConnecting:
		heading, detail = "connecting", status.To
	case StatusFailed:
		heading, detail = status.Error.Kind.Headline(), status.Error.Detail
		failed = true
	default:
		heading, detail = "nothing open", "pick a table on the left, or start a query"
	}
	headingInk, detailInk := colors.Text, colors.TextMuted
	if failed {
		headingInk, detailInk = colors.Error, colors.Error
	}

	return ui.Column(c.Key("placeholder")).
		Grow(1).Basis(0).
		AlignItems(ui.Center).
		Justify(ui.Center).
		Gap(8).
		Children(func() {
			ui.Text(c, heading).
				FontSize(12).
				FontWeight(500).
				TextColor(headingInk.Color())
			ui.Box(c).
				MaxWidth(560).
				FontSize(11).
				TextColor(detailInk.Color()).
				Children(func() { ui.Text(c, detail) })
		})
}

func (s *Shell) statusStrip(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	divider := func() { ui.Box(c).Shrink(0).Width(1).Height(14).Background(colors.Border.Color()) }
	var tab Tab
	if s.active < len(s.tabs) {
		tab = s.tabs[s.active]
	}
	var rangeText string
	var timing *Timing
	paging := false
	switch tab := tab.(type) {
	case *TableTab:
		paging = true
		first := tab.page*PageSize + 1
		last := tab.page*PageSize + len(tab.data.Rows)
		switch {
		case len(tab.data.Rows) == 0:
			rangeText = "no rows"
		case tab.approxRows != nil:
			rangeText = fmt.Sprintf("rows %d–%d of ~%s", first, last, uikit.FormatCount(*tab.approxRows))
		default:
			rangeText = fmt.Sprintf("rows %d–%d", first, last)
		}
		timing = tab.timing
	case *QueryTab:
		// The strip says how much is on screen. With no result set
		// there is nothing on screen to count, so it says what the
		// run did instead — and never `0 rows`, which reported a
		// working `UPDATE` as an empty table.
		switch {
		case tab.hasResult:
			rangeText = fmt.Sprintf("%d rows", len(tab.data.Rows))
		case tab.changed != nil:
			rangeText = changedCopy(*tab.changed)
		}
		timing = tab.timing
	case *HistoryTab:
		switch tab.runs {
		case 0:
			rangeText = "no runs"
		case 1:
			rangeText = "1 run"
		default:
			rangeText = fmt.Sprintf("%d runs", tab.runs)
		}
	}

	// What is marked, and the key that takes it. The strip is where the
	// count of a selection belongs: it is already the line that says how
	// much is on screen.
	var marks string
	hasMarks := false
	if tab != nil {
		if selection := tab.Selection(); selection != nil {
			marks, hasMarks = selection.Summary()
		}
	}
	// Only over a result there is something to do to. A tab still
	// waiting on its first run has no columns and no keys worth listing.
	keys := len(s.resultColumns()) != 0

	var statusInk, statusDot theme.Hsla
	var statusWord string
	switch s.status.(type) {
	case StatusConnected:
		statusInk, statusDot, statusWord = colors.EnvDevText, colors.Ok, "On lookout"
	case StatusFailed:
		statusInk, statusDot, statusWord = colors.Error, colors.ErrorMark, "Off duty"
	default:
		statusInk, statusDot, statusWord = colors.TextMuted, colors.Idle, "Connecting"
	}

	return ui.Row(c.Key("status-strip")).
		Height(30).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(12).
		Padding(0, 14).
		BorderWidth(1, 0, 0, 0).
		BorderColor(colors.Border.Color()).
		Background(colors.Panel.Color()).
		FontSize(12).
		TextColor(colors.TextMuted.Color()).
		Children(func() {
			ui.Text(c, rangeText).
				NoWrap().
				Font(theme.MonoFontFamily).
				FontSize(11.5).
				TextColor(colors.TextSecondary.Color())
			if paging {
				divider()
				s.pageLink(c, "Previous", false, colors)
				s.pageLink(c, "Next", true, colors)
			}
			if hasMarks {
				divider()
				ui.Text(c, marks).NoWrap().TextColor(colors.AccentDeep.Color())
			}
			// The keys the result answers to, as the comp's own footer
			// lists them. It is the only place ⏎ and ⌘J are written
			// down, and a gesture nothing on screen names is a gesture
			// nobody finds.
			if keys {
				divider()
				ui.Row(c).
					Grow(1).Basis(0).
					MinWidth(0).
					Clip().
					AlignItems(ui.Center).
					Gap(12).
					Children(func() {
						for _, hint := range [][2]string{
							{"↑↓←→", "move"},
							{"⌘J", "column"},
							{"⏎", "value"},
							{"space", "pick"},
							{"⌘C", "copy"},
						} {
							uikit.KeyHint(c, hint[0], hint[1])
						}
					})
			} else {
				ui.Box(c).Grow(1).Basis(0)
			}
			if timing != nil {
				ui.Row(c).
					Shrink(0).
					AlignItems(ui.Center).
					Gap(8).
					Font(theme.MonoFontFamily).
					FontSize(11.5).
					Children(func() {
						// The split of the wait, drawn: the server's part
						// in the accent, the lag in the track. A number
						// pair says the same thing, but the question is
						// usually "was it the database?", and a bar
						// answers it before the numbers are read.
						if share, ok := timing.serverShare(); ok {
							ui.Row(c).
								Shrink(0).
								Width(56).
								Height(6).
								AlignItems(ui.Stretch).
								Radius(3).
								Clip().
								Background(colors.Sunk.Color()).
								Children(func() {
									ui.Box(c).WidthPercent(share * 100).Background(colors.Accent.Color())
								})
						}
						ui.Text(c, timing.summary()).NoWrap()
					})
			}
			divider()
			ui.Row(c).
				Shrink(0).
				AlignItems(ui.Center).
				Gap(6).
				TextColor(statusInk.Color()).
				Children(func() {
					uikit.StatusDot(c, statusDot)
					ui.Text(c, statusWord).NoWrap()
				})
		})
}

// noResultNote is what the result pane says where a tab has no result
// set to paint.
//
// A tab that has run nothing is told which key runs it: the pane is the
// largest empty thing on screen, and a gesture nothing names is a gesture
// nobody finds. A tab whose statement changed rows is told nothing here —
// the count is already on the line above, and repeating it in the middle
// of the pane would say the same thing twice in one glance.
func noResultNote(c *ui.Context, tab Tab, colors *theme.ThemeColors) {
	query, ok := tab.(*QueryTab)
	if !ok {
		return
	}
	if query.landed > 0 || query.error != nil || query.run.kind != RunIdle {
		return
	}
	ui.Box(c).
		Padding(14, 16, 0, 16).
		FontSize(11).
		TextColor(colors.TextFaint.Color()).
		Children(func() { ui.Text(c, "nothing run yet · ⌘⏎ runs the statement") })
}

// deltaRowsView is one row of the report per thing that changed: a
// relation that came or went, or a column of one that stayed. Tables
// first, then their columns, in the order the delta has them.
//
// Go port: Rust's `delta_rows` handed back a `Vec<Div>`; here the rows are
// built into the current parent, as the Rust's caller added them.
func deltaRowsView(c *ui.Context, delta *introspect.Delta, colors *theme.ThemeColors) {
	rows := 0
	total := 0
	push := func(sign Sign, name, kind, note string) {
		deltaRow(c, sign, name, kind, note, colors)
		rows++
	}
	for _, relation := range delta.Relations {
		noun := "table"
		if relation.Kind == introspect.TableKindView {
			noun = "view"
		}
		switch change := relation.Change.(type) {
		case introspect.RelationAdded:
			total++
			if rows < deltaRows {
				push(SignPlus, fmt.Sprintf("%s.%s", relation.Schema, relation.Name), noun, columnCount(len(change.Columns)))
			}
		case introspect.RelationDropped:
			total++
			if rows < deltaRows {
				push(SignMinus, fmt.Sprintf("%s.%s", relation.Schema, relation.Name), noun, columnCount(len(change.Columns)))
			}
		case introspect.RelationAltered:
			for _, column := range change.Columns {
				total++
				if rows >= deltaRows {
					continue
				}
				switch column := column.(type) {
				case introspect.ColumnAdded:
					push(SignPlus, column.Column.Name, column.Column.DataType, columnNote(&column.Column))
				case introspect.ColumnDropped:
					push(SignMinus, column.Column.Name, column.Column.DataType, columnNote(&column.Column))
				case introspect.ColumnChanged:
					before, after := column.Before, column.After
					if before.DataType == after.DataType {
						push(SignTilde, after.Name, after.DataType,
							fmt.Sprintf("was %s · now %s", columnNote(&before), columnNote(&after)))
					} else {
						push(SignTilde, after.Name, fmt.Sprintf("%s → %s", before.DataType, after.DataType),
							columnNote(&after))
					}
				}
			}
		}
	}
	// Then what else changed: an index, a key, a routine. Named with what
	// it sits in, because a bare `orders_user` could be anybody's.
	for i := range delta.Objects {
		object := &delta.Objects[i]
		total++
		if rows >= deltaRows {
			continue
		}
		var sign Sign
		var note string
		switch change := object.Change.(type) {
		case introspect.ObjectAdded:
			sign, note = SignPlus, change.Detail
		case introspect.ObjectDropped:
			sign, note = SignMinus, change.Detail
		case introspect.ObjectChanged:
			sign, note = SignTilde, fmt.Sprintf("%s → %s", change.Before, change.After)
		}
		push(sign, object.Qualified(), object.Kind.Noun(), note)
	}
	if total > rows {
		ui.Box(c).
			Padding(0, 9).
			FontSize(10).
			TextColor(colors.DdlMuted.Color()).
			Children(func() { ui.Text(c, fmt.Sprintf("… and %d more", total-rows)) })
	}
}

func deltaRow(c *ui.Context, sign Sign, name, kind, note string, colors *theme.ThemeColors) ui.Element {
	var glyph string
	var surface, mark, nameInk, kindInk, noteInk theme.Hsla
	switch sign {
	case SignPlus:
		glyph = "+"
		surface, mark, nameInk, kindInk, noteInk = colors.DeltaAddSurface, colors.DeltaAdd,
			colors.Text, colors.DeltaAddType, colors.DeltaAddText
	case SignMinus:
		glyph = "−"
		surface, mark, nameInk, kindInk, noteInk = colors.DeltaDropSurface, colors.EnvProd,
			colors.Error, colors.DeltaDropType, colors.Error
	default:
		glyph = "~"
		surface, mark, nameInk, kindInk, noteInk = colors.Window, colors.DdlText,
			colors.Text, colors.DdlText, colors.DdlMuted
	}
	return ui.Row(c).
		AlignItems(ui.Center).
		Gap(10).
		Padding(4, 9).
		Radius(5).
		Background(surface.Color()).
		Children(func() {
			ui.Box(c).
				Width(10).
				Shrink(0).
				TextAlign(ui.Center).
				FontSize(12).
				FontWeight(600).
				TextColor(mark.Color()).
				Children(func() { ui.Text(c, glyph) })
			nameText := ui.Box(c).
				Width(150).
				Shrink(0).
				FontSize(11).
				FontWeight(500).
				TextColor(nameInk.Color())
			nameText.Children(func() {
				text := ui.Text(c, name).SingleLine()
				// Struck through, as the comp strikes a dropped column:
				// the name is still worth reading, and it is gone.
				if sign == SignMinus {
					text.Strikethrough()
				}
			})
			ui.Box(c).
				Width(130).
				Shrink(0).
				FontSize(11).
				TextColor(kindInk.Color()).
				Children(func() { ui.Text(c, kind).SingleLine() })
			ui.Box(c).
				MinWidth(0).
				FontSize(10).
				TextColor(noteInk.Color()).
				Children(func() { ui.Text(c, note).SingleLine() })
		})
}

// sessionMark is the mark that this tab's session was given back — the
// one taking-back the user did not ask for: the sweep took the connection
// back, so a `search_path` set an hour ago is gone, and a mark is the
// difference between that and a mystery.
//
// An open transaction used to be a badge here as well. It is not any more,
// because the transaction bar says it in a whole strip and offers the two
// ways out — and the toolbar's mode switch carries the same dot. Three
// marks for one state is two too many.
func sessionMark(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) {
	if !tab.sessionEnded {
		return
	}
	// Shrinkable, not `flex_none`: it is the longest thing on the
	// toolbar's left side, and a note about a connection that reset
	// must not be what pushes the run button off the edge.
	ui.Box(c).
		MinWidth(0).
		TextColor(colors.TextMuted.Color()).
		Children(func() { ui.Text(c, "Session ended · a run opens a new one").SingleLine() })
}

// capNote is the clause the result line adds when the memory cap ended
// the read.
//
// It is in the accent's family, not the error's: nothing failed, and the
// rows on screen are real rows. The app declined to hold the rest. Only an
// idle run says it — while a run is out, the grid still shows the result
// before it, and that result's cap is not news about this one.
func capNote(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) {
	if !tab.truncated || tab.run.inFlight() {
		return
	}
	ui.Text(c, fmt.Sprintf(
		"capped at %d MB · select fewer columns, or add a LIMIT",
		dbclient.MaxBytes/1024/1024,
	)).
		NoWrap().
		FontWeight(500).
		TextColor(colors.AccentDeep.Color())
}

// syntaxStrip is what the last syntax check found, under the pane the
// marks are in.
//
// **The squiggle says where and this says what**, and the caret is what
// keeps the two about the same error: walking onto a mark is how the user
// asks what it says. With the caret nowhere near one, the first is what
// is spoken for, and the count on the right says how many there are —
// otherwise a buffer with three faults would read as a buffer with one.
//
// It is **not** [`Shell::error_strip`]. That one answers a run: the server
// was sent a statement and refused it. This one is about a statement
// nobody has sent, so it takes the panel's own ground and says its piece
// in the error ink rather than lighting a whole strip up like a failure.
func syntaxStrip(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) {
	found := tab.editor.Diagnostics()
	ix := spokenFor(found, tab.editor.Cursor())
	if ix >= len(found) {
		return
	}
	diagnostic := found[ix]
	ui.Row(c.Key("syntax-strip")).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(8).
		Padding(7, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Hairline.Color()).
		Background(colors.Panel.Color()).
		FontSize(11).
		TextColor(colors.Error.Color()).
		Children(func() {
			ui.Box(c).
				Grow(1).Basis(0).
				MinWidth(0).
				Children(func() { ui.Text(c, diagnostic.Message).SingleLine() })
			if len(found) > 1 {
				ui.Text(c, fmt.Sprintf("%d of %d", ix+1, len(found))).
					TextColor(colors.TextFaint.Color())
			}
		})
}
