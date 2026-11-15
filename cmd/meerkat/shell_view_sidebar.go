// The sidebar: the filter line, the catalog list and its rows, and the
// foot with the way into the history (shell.rs, agent K3b's part — see
// docs/tasks/shell-split.md).

package main

import (
	"fmt"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

func (s *Shell) sidebar(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	// The title bar names the session; the foot says where it is. A
	// named profile takes the title, so the database moves down here
	// rather than leaving the screen: the user still has to know which
	// database of that host is open.
	var host string
	_, connected := s.status.(StatusConnected)
	_, failed := s.status.(StatusFailed)
	switch {
	case s.label != nil && connected:
		whereItIs := fmt.Sprintf("%s:%d", s.label.Host, s.label.Port)
		if s.name != nil && s.label.Database != "" {
			host = fmt.Sprintf("%s · %s", s.label.Database, whereItIs)
		} else {
			host = whereItIs
		}
	case failed:
		host = "not connected"
	default:
		host = "connecting…"
	}

	return ui.Column(c.Key("sidebar")).
		Width(s.sidebarWidth).
		Shrink(0).
		BorderWidth(0, 1, 0, 0).
		BorderColor(colors.Border.Color()).
		Background(colors.Panel.Color()).
		Children(func() {
			s.catalogFilterRow(c, colors)
			s.catalogList(c, colors)
			ui.Column(c).
				Shrink(0).
				Gap(2).
				Padding(6, 6, 8, 6).
				BorderWidth(1, 0, 0, 0).
				BorderColor(colors.Border.Color()).
				FontSize(12).
				TextColor(colors.TextMuted.Color()).
				Children(func() {
					ui.Row(c).
						AlignItems(ui.Center).
						Justify(ui.SpaceBetween).
						Gap(8).
						Children(func() {
							// The comp's way into the history, kept where it
							// puts it: the foot of the sidebar.
							history := ui.Row(c.Key("query-history")).
								Height(28).
								AlignItems(ui.Center).
								Gap(7).
								Padding(0, 8).
								Radius(7).
								TextColor(colors.TextSecondary.Color()).
								Cursor(ui.CursorPointer)
							if history.Hovered() {
								history.Background(colors.Sunk.Color())
							}
							history.OnClick(func() { s.openHistory() })
							history.Children(func() {
								uikit.DrawIcon(c, uikit.IconHistory, 13., colors.TextMuted)
								ui.Text(c, "Query history")
							})
							ui.Box(c).Padding(0, 6, 0, 0).Children(func() {
								ui.Text(c, s.tableTotal())
							})
						})
					ui.Box(c).
						Padding(0, 8).
						Font(theme.MonoFontFamily).
						FontSize(11).
						Children(func() { ui.Text(c, host).SingleLine() })
				})
		})
}

func (s *Shell) tableTotal() string {
	switch s.relationTotal {
	case 0:
		return "no tables"
	case 1:
		return "1 relation"
	default:
		return fmt.Sprintf("%d relations", s.relationTotal)
	}
}

// catalogFilterRow is the filter line over the catalog list. It searches
// the names the session already holds, so a keystroke costs a substring
// scan and never a query.
func (s *Shell) catalogFilterRow(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	filtering := !s.catalogFilter.IsEmpty()
	// ↑↓ walk the list below while this line has the focus. The
	// context sits here rather than on the shell, or the two keys
	// would be taken from the grid whenever nothing is typed.
	row := ui.Column(c.Key("catalog-filter")).
		Shrink(0).
		Padding(10)
	if action, ok := uikit.DispatchKeys(row, catalogFilterViewKeys); ok {
		s.handleAction(action)
	}
	return row.Children(func() {
		ui.Row(c).
			Height(30).
			AlignItems(ui.Center).
			Gap(8).
			Padding(0, 5, 0, 9).
			Border(1, colors.Border.Color()).
			Radius(7).
			Background(colors.Window.Color()).
			Children(func() {
				uikit.DrawIcon(c, uikit.IconSearch, 12., colors.TextMuted)
				ui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
					s.catalogFilter.View(c)
				})
				// ⌘E reaches this line from anywhere, and a gesture
				// nothing on screen names is a gesture nobody finds.
				// It gives way to the clear mark, which is about the
				// line the user is already on.
				if !filtering {
					uikit.Keycap(c, "⌘E")
				}
				// The way out of a filter for the mouse. It appears only
				// when there is something to clear.
				if filtering {
					clear := ui.Row(c.Key("clear-filter")).
						Shrink(0).
						Size(20, 20).
						AlignItems(ui.Center).
						Justify(ui.Center).
						Radius(4).
						FontSize(14).
						TextColor(colors.TextMuted.Color()).
						Cursor(ui.CursorPointer)
					if clear.Hovered() {
						clear.Background(colors.Sunk.Color()).TextColor(colors.Text.Color())
					}
					clear.OnClick(func() {
						s.catalogFilter.Clear()
						s.rebuildCatalogRows()
					})
					clear.Children(func() { ui.Text(c, "×") })
				}
			})
	})
}

// catalogList is the sidebar list. A catalog can run to thousands of
// relations, so the rows are virtualized: the flattened list is built
// once, when the catalog lands, and only the rows on screen are laid out.
func (s *Shell) catalogList(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	if len(s.catalogRows) == 0 {
		// The catalog is a request of its own, so a connected
		// session can still be waiting for one. A filter that
		// matched nothing empties the list as well, and says so
		// rather than reporting an empty catalog.
		var note string
		_, connecting := s.status.(StatusConnecting)
		_, failed := s.status.(StatusFailed)
		switch {
		case len(s.catalogGroups) != 0:
			note = "nothing goes by that name"
		case failed:
			note = "no catalog"
		case s.catalogLoading:
			note = "reading the catalog…"
		case connecting:
			note = "connecting…"
		case s.catalogError != nil:
			note = "no catalog"
		default:
			note = "no relations"
		}
		return ui.Column(c.Key("catalog")).
			Grow(1).Basis(0).
			MinHeight(0).
			Padding(6, 16, 0, 16).
			FontSize(12).
			TextColor(colors.TextMuted.Color()).
			Children(func() { ui.Text(c, note) })
	}

	// The row the active tab came from, whichever kind of tab that is:
	// a query tab opened from the sidebar marks its row too.
	var active *[2]string
	if s.active < len(s.tabs) {
		switch tab := s.tabs[s.active].(type) {
		case *TableTab:
			active = &[2]string{tab.schema, tab.table}
		case *QueryTab:
			active = tab.relation
		}
	}
	rows := s.catalogRows
	cursor := s.catalogSelected
	scroll := &s.catalogScroll

	return ui.Box(c.Key("catalog-pane")).
		Grow(1).Basis(0).
		MinHeight(0).
		Children(func() {
			list := ui.List(c.Key("catalog"), &scroll.List, len(rows), func(ix int) {
				catalogRow(c, s, ix, rows[ix], active, cursor != nil && *cursor == ix, colors)
			}).
				Fill().
				Padding(0, 6).
				TrackScroll(&scroll.Handle.State).
				Scrollbars(false)
			scroll.Handle.Bounds = list.Bounds()
			// The bar sits outside the scrolling list, or it would scroll
			// away with it. The list keeps a plain handle inside its own,
			// and that is the one the scrollbar reads.
			if bar, ok := uikit.NewScrollbar(true, &scroll.Handle, &s.catalogDrag, colors.TextFaint, colors.TextMuted); ok {
				ui.Box(c).
					Absolute().
					Top(0).
					Right(0).
					FillHeight().
					Width(uikit.Thickness).
					Children(func() { bar.View(c) })
			}
		})
}

// headerRow is the shell of a header row, up to its chevron: the caller
// adds the label and the count. `indent` is what puts the level on the
// tree.
func headerRow(c *ui.Context, ix int, id string, indent float32, open bool, colors *theme.ThemeColors) ui.Element {
	row := ui.Row(c.Key(fmt.Sprintf("%s-%d", id, ix))).
		Height(catalogRowHeight).
		AlignItems(ui.Center).
		Gap(treeGap).
		Padding(0, 8, 0, indent).
		Radius(6).
		Cursor(ui.CursorPointer)
	if row.Hovered() {
		row.Background(colors.Sunk.Color())
	}
	return row
}

// chevron is the mark that says a row folds out, and which way it is.
func chevron(c *ui.Context, open bool, colors *theme.ThemeColors) ui.Element {
	glyph := uikit.IconChevronRight
	if open {
		glyph = uikit.IconChevronDown
	}
	return ui.Row(c).
		Shrink(0).
		Width(treeGlyph).
		Justify(ui.Center).
		Children(func() { uikit.DrawIcon(c, glyph, treeGlyph, colors.TextMuted) })
}

// countLabel is how many entries sit under a header, at its right edge.
func countLabel(c *ui.Context, count int, colors *theme.ThemeColors) ui.Element {
	return ui.Box(c).
		Shrink(0).
		Font(theme.MonoFontFamily).
		FontSize(11).
		TextColor(colors.TextMuted.Color()).
		Children(func() { ui.Text(c, uikit.FormatCount(uint64(count))) })
}

// leafMark is a leaf's glyph, as wide as a relation's so the names after
// both line up. A column is a dot, the key's own columns in the accent —
// the one thing about a column worth seeing before reading it. Everything
// else is one character in the faint ink, because the folder above it has
// already said what it is and the glyph only has to tell the kinds apart
// in a schema's own sections.
func leafMark(c *ui.Context, mark Mark, colors *theme.ThemeColors) ui.Element {
	dot := func(ink theme.Hsla) {
		ui.Box(c).Size(4, 4).Radius(2).Background(ink.Color())
	}
	glyph := func(text string) {
		ui.Text(c, text).FontSize(10).TextColor(colors.TextMuted.Color())
	}
	return ui.Row(c).
		Shrink(0).
		Width(treeGlyph).
		Justify(ui.Center).
		AlignItems(ui.Center).
		Children(func() {
			switch mark {
			case MarkColumn:
				dot(colors.TextFaint)
			case MarkKey:
				uikit.DrawIcon(c, uikit.IconKey, treeGlyph, colors.Accent)
			case MarkIndex:
				glyph("≡")
			case MarkForeignKey:
				glyph("→")
			case MarkConstraint:
				glyph("✓")
			case MarkTrigger:
				glyph("↯")
			case MarkSequence:
				glyph("#")
			case MarkRoutine:
				glyph("ƒ")
			case MarkType:
				glyph("τ")
			}
		})
}

// catalogRow is one row of the flattened list. Free-standing because the
// list's render closure outlives the borrow of the shell it was built
// from; it reaches the shell again through a weak handle when a row is
// clicked.
//
// Go port: the shell is the pointer itself, and every click is an
// `OnClick`, which runs after the build — a toggle rebuilds the rows the
// list is iterating.
func catalogRow(
	c *ui.Context,
	shell *Shell,
	ix int,
	row CatalogRow,
	active *[2]string,
	cursor bool,
	colors *theme.ThemeColors,
) ui.Element {
	switch row := row.(type) {
	// The two header levels differ in where they sit and how loud
	// they read; the chevron, the count and the click are the same.
	case CatalogRowSchema:
		key := row.key
		header := headerRow(c, ix, "schema", chevronAt(0), row.open, colors)
		header.OnClick(func() { shell.toggleSchema(key) })
		return header.Children(func() {
			// The chevron is the whole affordance: a closed row shows
			// nothing else that says it can be opened.
			chevron(c, row.open, colors)
			uikit.DrawIcon(c, uikit.IconSchema, treeGlyph, colors.TextMuted)
			ui.Box(c).
				Grow(1).Basis(0).
				MinWidth(0).
				Font(theme.MonoFontFamily).
				FontSize(12.5).
				FontWeight(400).
				TextColor(colors.Text.Color()).
				Children(func() { ui.Text(c, row.label).SingleLine() })
			countLabel(c, row.count, colors)
		})
	case CatalogRowSection:
		key := row.key
		header := headerRow(c, ix, "section", chevronAt(row.level), row.open, colors)
		header.OnClick(func() { shell.toggleSection(key) })
		return header.Children(func() {
			chevron(c, row.open, colors)
			ui.Box(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
				uikit.SectionLabel(c, row.label).FontWeight(400)
			})
			countLabel(c, row.count, colors)
		})
	case CatalogRowLeaf:
		return ui.Row(c).
			Height(catalogRowHeight).
			AlignItems(ui.Center).
			Gap(treeGap).
			Padding(0, 8, 0, contentAt(row.level)).
			Font(theme.MonoFontFamily).
			Children(func() {
				leafMark(c, row.mark, colors)
				// The name gives way before the detail does, but not all
				// the way: a column's type is worth reading, and so is
				// its name.
				ui.Box(c).
					Shrink(1).
					MaxWidthPercent(60).
					MinWidth(0).
					FontSize(12).
					TextColor(colors.TextSecondary.Color()).
					Children(func() { ui.Text(c, row.name).SingleLine() })
				ui.Box(c).
					Grow(1).Basis(0).
					MinWidth(0).
					Padding(0, 0, 0, 3).
					FontSize(11).
					TextColor(colors.TextMuted.Color()).
					Children(func() { ui.Text(c, row.detail).SingleLine() })
			})
	case CatalogRowRelation:
		isActive := active != nil && active[0] == row.schema && active[1] == row.name
		// Two rows read loud, and for the same reason: this is the one
		// being pointed at. The tab's row says where the result on
		// screen came from, the cursor's says what ⏎ would open.
		lit := isActive || cursor
		key := relationKey(row.schema, row.name)
		schemaName, tableName := row.schema, row.name

		item := ui.Row(c.Key(fmt.Sprintf("relation-%d", ix))).
			Height(catalogRowHeight).
			AlignItems(ui.Center).
			// Indented under its section's label, so the schema and
			// the section a relation sits in read off the left edge.
			Padding(0, 8, 0, chevronAt(2)).
			Radius(6).
			Cursor(ui.CursorPointer)
		item.OnClick(func() { shell.browseTable(schemaName, tableName) })
		switch {
		// The cursor's mark is the deeper of the two, and it has
		// to be: the row it is on is often the row the active tab
		// came from as well, and one mark cannot say both. It is
		// the mark the grid's own cursor wears, for the same
		// reason — it sits over `selection` and stays visible.
		case cursor:
			item.Background(colors.MatchStrong.Color())
		case isActive:
			item.Background(colors.Selection.Color())
		default:
			if item.Hovered() {
				item.Background(colors.Sunk.Color())
			}
		}
		return item.Children(func() {
			// The chevron folds the relation out, and must not also
			// open a tab on it: the press is taken here, before the
			// row's own click can hear it. It takes the whole step and
			// the row's height, because an 8px target is one the
			// pointer misses.
			//
			// Go port: the innermost clickable element under the press
			// takes it (mapping sheet §10), so the fold's own click is
			// all `stop_propagation` was.
			fold := ui.Row(c.Key(fmt.Sprintf("relation-fold-%d", ix))).
				Shrink(0).
				Width(treeStep).
				FillHeight().
				AlignItems(ui.Center)
			fold.OnClick(func() { shell.toggleRelation(key) })
			fold.Children(func() { chevron(c, row.open, colors) })
			glyph := uikit.IconTable
			if row.kind == introspect.TableKindView {
				glyph = uikit.IconView
			}
			glyphInk := colors.TextMuted
			if lit {
				glyphInk = colors.Accent
			}
			ui.Box(c).Shrink(0).Margin(0, treeGap, 0, 0).Children(func() {
				uikit.DrawIcon(c, glyph, treeGlyph, glyphInk)
			})
			weight, ink := 400, colors.TextSecondary
			if lit {
				weight, ink = 500, colors.Text
			}
			ui.Box(c).
				Grow(1).Basis(0).
				MinWidth(0).
				Font(theme.MonoFontFamily).
				FontSize(12.5).
				FontWeight(weight).
				TextColor(ink.Color()).
				Children(func() { ui.Text(c, row.name).SingleLine() })
		})
	}
	return ui.Element{}
}
