package main

// What editing a result looks like: the toolbar's chip, the change bar,
// the editor's status line, and the two dialogs. The design reference is
// the "Meerkat inline editing" canvas; see docs/tasks/inline-edit.md.

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/resultsgrid"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// editView is the marks the grid paints for the tab's result; nil when the
// result takes no edit, which paints the grid exactly as before.
func (s *Shell) editView(tab *QueryTab) *resultsgrid.EditView {
	e := tab.edit
	if e == nil {
		return nil
	}
	rule := s.editRule(tab)
	if !rule.OK() {
		return nil
	}
	base := e.base.Rows
	fault := e.edits.Fault()
	view := &resultsgrid.EditView{
		Row: func(row int) (resultsgrid.RowMark, int) {
			ref := e.edits.Ref(row)
			number := ref.Index + 1
			if fault != nil && fault.Row == ref {
				if fault.Conflict {
					return resultsgrid.RowConflict, number
				}
				return resultsgrid.RowRefused, number
			}
			switch {
			case ref.Added:
				return resultsgrid.RowAdded, 0
			case e.edits.IsDeleted(ref.Index):
				return resultsgrid.RowDeleted, number
			case e.edits.RowEdited(ref.Index):
				return resultsgrid.RowEdited, number
			}
			return resultsgrid.RowPlain, number
		},
		Cell: func(row, column int) resultsgrid.CellMark {
			ref := e.edits.Ref(row)
			if fault != nil && !fault.Conflict && fault.Row == ref && fault.Column == column {
				return resultsgrid.CellRefused
			}
			if ref.Added {
				if e.edits.IsDefault(ref, column) {
					return resultsgrid.CellDefault
				}
				return resultsgrid.CellPlain
			}
			if _, staged := e.edits.Value(ref, column, base); staged {
				return resultsgrid.CellStaged
			}
			if column >= len(rule.Columns) || !rule.Columns[column].Editable ||
				e.cut[dbclient.CellAt{Row: ref.Index, Column: column}] {
				return resultsgrid.CellLocked
			}
			return resultsgrid.CellPlain
		},
	}
	view.Columns = make([]resultsgrid.ColumnMark, len(rule.Columns))
	for i, role := range rule.Columns {
		view.Columns[i] = resultsgrid.ColumnMark{Key: role.Key, Locked: !role.Editable}
	}
	if ed := e.cell; ed != nil {
		cell := ed.cell
		view.Editing = &cell
		view.Editor = func(c *ui.Context) {
			ui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() { ed.field.View(c) })
		}
	}
	return view
}

// editChip is the toolbar's word on whether the result takes an edit.
func (s *Shell) editChip(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) {
	if tab.edit == nil || tab.run.inFlight() {
		return
	}
	rule := s.editRule(tab)
	var surface, border, ink, dot theme.Hsla
	var text string
	switch rule.Blocker {
	case dbclient.EditOK:
		surface, border, ink = colors.EditBar, colors.EditBorder, colors.EditText
		chip := ui.Row(c.Key("edit-chip")).
			Shrink(0).
			AlignItems(ui.Center).
			Gap(6).
			Padding(3, 9).
			Border(1, border.Color()).
			Radius(5).
			Background(surface.Color()).
			FontSize(theme.SizeBody).
			TextColor(ink.Color())
		chip.Children(func() {
			uikit.DrawIcon(c, uikit.IconPencil, 12, colors.Edit)
			ui.Text(c, "editable ·").NoWrap()
			ui.Text(c, rule.Target.Qualified()).Font(theme.MonoFontFamily).NoWrap()
			keys := make([]string, 0, len(rule.Key))
			for _, column := range rule.Key {
				keys = append(keys, tab.data.Columns[column])
			}
			ui.Text(c, "· key").NoWrap()
			ui.Text(c, strings.Join(keys, ", ")).Font(theme.MonoFontFamily).NoWrap()
		})
		return
	case dbclient.BlockedNoTable, dbclient.BlockedPending:
		return
	case dbclient.BlockedReadOnly:
		surface, border, ink, dot = colors.EnvDevSurface, colors.EnvDevInner, colors.EnvDevText, colors.EnvDev
	case dbclient.BlockedTxFailed:
		surface, border, ink, dot = colors.ErrorSurface, colors.ErrorBorder, colors.Error, colors.Error
	default:
		surface, border, ink, dot = colors.ModeOffSurface, colors.ModeOffBorder, colors.TextSecondary, colors.TextFaint
	}
	text = editBlockerCopy(rule)
	ui.Row(c.Key("edit-chip")).
		MinWidth(0).
		AlignItems(ui.Center).
		Gap(6).
		Padding(3, 9).
		Border(1, border.Color()).
		Radius(5).
		Background(surface.Color()).
		FontSize(theme.SizeBody).
		TextColor(ink.Color()).
		Children(func() {
			uikit.StatusDot(c, dot)
			ui.Text(c, text).SingleLine()
		})
}

// addRowButton is "+ row ⌘I" on the result's header line.
func (s *Shell) addRowButton(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) {
	if tab.edit == nil || !s.editRule(tab).OK() {
		return
	}
	button := ui.Row(c.Key("add-row")).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(6).
		Padding(3, 4, 3, 8).
		Border(1, colors.BorderStrong.Color()).
		Radius(5).
		Background(colors.Elevated.Color()).
		Cursor(ui.CursorPointer).
		KeepFocus()
	if button.Hovered() {
		button.BorderColor(colors.TextFaint.Color())
	}
	button.OnClick(func() { s.addRow() })
	button.Children(func() {
		ui.Text(c, "+ row").FontSize(theme.SizeBody).TextColor(colors.TextBody.Color())
		keycap(c, "⌘I", theme.SizeCaption, colors.Window, colors.ModeOffBorder, colors.TextSecondary)
	})
}

// changeBar is the bar over the result while something is staged, and
// the answer to the last apply after it. It sits where the transaction
// bar sits, and one bar is up at a time: true says it painted, and the
// transaction bar stays down.
func (s *Shell) changeBar(c *ui.Context, tab *QueryTab, colors *theme.ThemeColors) bool {
	e := tab.edit
	if e == nil {
		return false
	}
	staged := e.edits.Counts()
	answer := e.answer
	var title, sub string
	var surface, border, ink, subInk, dot theme.Hsla
	glyph := ""
	switch {
	case answer != nil && answer.refused:
		title, sub = refusedCopy(answer, staged.Changes())
		surface, border, ink, subInk, dot = colors.ErrorSurface, colors.ErrorBorder, colors.Error, colors.ErrorSecondary, colors.Error
		glyph = "!"
	case !e.edits.Empty():
		title, sub = changeBarCopy(staged)
		surface, border, ink, subInk, dot = colors.EditBar, colors.EditBorder, colors.EditText, colors.EditMuted, colors.Edit
		if e.applying {
			sub = "applying · this tab's session has the statements"
		}
	case answer != nil && answer.committed:
		title, sub = appliedCopy(answer)
		surface, border, ink, subInk, dot = colors.OkSurface, colors.EnvDevInner, colors.EnvDevText, colors.TextMuted, colors.Ok
		glyph = "✓"
	default:
		return false
	}
	ready := !e.applying && !tab.run.inFlight() && !tab.txEnding

	ui.Row(c.Key("change-bar")).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(10).
		Padding(8, 12, 8, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(border.Color()).
		Background(surface.Color()).
		Children(func() {
			if glyph != "" {
				ui.Box(c).
					Size(16, 16).
					Shrink(0).
					Radius(8).
					Background(dot.Color()).
					AlignItems(ui.Center).
					Justify(ui.Center).
					FontSize(theme.SizeCaption).
					FontWeight(700).
					TextColor(colors.Window.Color()).
					Children(func() { ui.Text(c, glyph) })
			} else {
				uikit.StatusDot(c, dot)
			}
			ui.Column(c).Grow(1).Basis(0).MinWidth(0).Gap(1).Children(func() {
				ui.Text(c, title).
					FontSize(theme.SizeEmphasis).
					FontWeight(600).
					TextColor(ink.Color()).
					SingleLine()
				ui.Text(c, sub).
					FontSize(theme.SizeMeta).
					TextColor(subInk.Color()).
					SingleLine()
			})
			switch {
			case answer != nil && answer.refused:
				if fault := e.edits.Fault(); fault != nil && fault.Conflict {
					barLink(c, "reload row", colors.EnvStagingText, func() { s.reloadRow() })
				}
				barLink(c, "preview SQL", ink, func() { s.openPreview() })
				barButton(c, "discard", "⇧⌘R", false, ready, colors, func() { s.discardEdits() })
				barButton(c, "apply again", "⌘S", true, ready, colors, func() { s.applyEdits(nil) })
			case !e.edits.Empty():
				barLink(c, "preview SQL", ink, func() { s.openPreview() })
				barButton(c, "discard", "⇧⌘R", false, ready, colors, func() { s.discardEdits() })
				barButton(c, fmt.Sprintf("apply %d %s", staged.Changes(), plural(staged.Changes(), "change", "changes")),
					"⌘S", true, ready, colors, func() { s.applyEdits(nil) })
			default:
				barLink(c, "copy the SQL", ink, func() { s.copyAppliedSQL() })
			}
		})
	return true
}

// barLink is a quiet word-button on a bar.
func barLink(c *ui.Context, label string, ink theme.Hsla, onClick func()) {
	link := ui.Box(c.Key("bar-link-"+label)).
		Shrink(0).
		Padding(3, 6).
		FontSize(theme.SizeBody).
		TextColor(ink.Color()).
		Underline().
		Cursor(ui.CursorPointer).
		KeepFocus()
	link.OnClick(onClick)
	link.Children(func() { ui.Text(c, label).NoWrap() })
}

// barButton is the change bar's two buttons: discard outlined over paper,
// apply filled in slate — the button the eye lands on is the one that
// writes, and it is not the run's ochre.
func barButton(c *ui.Context, label, keys string, filled, ready bool, colors *theme.ThemeColors, onClick func()) {
	border, fill, ink, capSurface, capBorder := colors.EditBorder, colors.Raised,
		colors.TextBody, colors.Window, colors.ModeOffBorder
	if filled {
		border, fill, ink, capSurface, capBorder = colors.Edit, colors.Edit,
			colors.Window, colors.KeyOnFillSurface, colors.KeyOnFillBorder
	}
	button := ui.Row(c.Key("bar-button-"+keys)).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(7).
		Padding(4, 5, 4, 10).
		Border(1, border.Color()).
		Radius(6).
		Background(fill.Color()).
		KeepFocus()
	button.Children(func() {
		text := ui.Text(c, label).FontSize(theme.SizeBody).TextColor(ink.Color()).NoWrap()
		if filled {
			text.FontWeight(600)
		}
		keycap(c, keys, theme.SizeCaption, capSurface, capBorder, ink)
	})
	if !ready {
		button.Opacity(0.5)
		return
	}
	if button.Hovered() {
		if filled {
			button.Background(colors.EditText.Color())
		} else {
			button.BorderColor(colors.TextFaint.Color())
		}
	}
	button.Cursor(ui.CursorPointer)
	button.OnClick(onClick)
}

// editingStatus is the status strip while a cell editor is open: what is
// being edited, what it was, and the editor's keys. False when no editor
// is open, and the strip says what it always says.
func (s *Shell) editingStatus(c *ui.Context, colors *theme.ThemeColors) bool {
	tab, ok := s.activeTab().(*QueryTab)
	if !ok || tab.edit == nil || tab.edit.cell == nil {
		return false
	}
	ed := tab.edit.cell
	rule := s.editRule(tab)
	if rule.Target == nil {
		return false
	}
	was := editText(ed.was)
	if _, null := ed.was.(dbclient.ValueNull); null {
		was = "NULL"
	}
	if len(was) > 60 {
		was = was[:57] + "…"
	}
	ui.Row(c.Key("editing-status")).
		Height(28).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(14).
		Padding(0, 12).
		BorderWidth(1, 0, 0, 0).
		BorderColor(colors.Border.Color()).
		Background(colors.Panel.Color()).
		FontSize(theme.SizeMeta).
		TextColor(colors.TextSecondary.Color()).
		Children(func() {
			ui.Row(c).MinWidth(0).Gap(4).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "editing").NoWrap()
				ui.Text(c, rule.Target.Name+"."+ed.column.Name).Font(theme.MonoFontFamily).TextColor(colors.TextBody.Color()).NoWrap()
				ui.Text(c, "· "+ed.column.Type+" · was").NoWrap()
				ui.Text(c, was).Font(theme.MonoFontFamily).SingleLine()
			})
			// An enum offers its labels, and a boolean its three keys, in
			// the line: what ⏎ would keep is the one that wears the mark.
			if labels := ed.column.Labels; labels != nil {
				matches := enumMatches(labels, ed.field.Text())
				ui.Row(c).MinWidth(0).Gap(4).AlignItems(ui.Center).Children(func() {
					for i, label := range matches {
						if i >= 8 {
							break
						}
						item := ui.Box(c).Padding(1, 6).Radius(4).Font(theme.MonoFontFamily).TextColor(colors.TextBody.Color())
						if i == ed.pick%max(len(matches), 1) {
							item.Background(colors.MatchStrong.Color())
						}
						item.Children(func() { ui.Text(c, label).NoWrap() })
					}
				})
			}
			if ed.column.Bool {
				ui.Text(c, "t true · f false · n NULL").NoWrap()
			}
			ui.Box(c).Grow(1).Basis(0)
			for _, hint := range [][2]string{{"⏎", "keep"}, {"⎋", "put back"}, {"⇥", "keep, next cell"}, {"⌥⌘N", "NULL"}, {"⇧⏎", "value card"}} {
				ui.Row(c).Shrink(0).Gap(4).Children(func() {
					ui.Text(c, hint[0]).FontWeight(600).TextColor(colors.TextBody.Color()).NoWrap()
					ui.Text(c, hint[1]).NoWrap()
				})
			}
		})
	return true
}

// previewOverlay is the statements an apply would send, before it does.
func (s *Shell) previewOverlay(c *ui.Context, colors *theme.ThemeColors) {
	preview := s.preview
	if preview == nil {
		return
	}
	tab, ok := s.tabMut(preview.tab).(*QueryTab)
	if !ok || tab.edit == nil {
		s.preview = nil
		return
	}
	rule := s.editRule(tab)
	changes := tab.edit.edits.Counts().Changes()
	title := fmt.Sprintf("Apply %d %s", changes, plural(changes, "change", "changes"))
	target := ""
	if rule.Target != nil {
		target = rule.Target.Qualified()
	}
	scrim := ui.Row(c.Key("preview-scrim")).
		Absolute().
		Top(0).
		Left(0).
		Fill().
		Justify(ui.Center).
		AlignItems(ui.Start).
		Padding(confirmTopMargin, 0, 0, 0).
		Background(colors.Overlay.Color())
	scrim.OnClick(func() { s.closePreview() })
	scrim.Children(func() {
		card := ui.Column(c.Key("preview")).
			Focusable().
			TabStop(false).
			Bind(&preview.focus).
			Width(820).
			Clip().
			Border(1, colors.BorderStrong.Color()).
			Radius(10).
			Background(colors.Elevated.Color())
		card.OnClick(func() {})
		overlayShadow(card, colors)
		if action, ok := uikit.DispatchKeys(card, previewViewKeys); ok {
			s.handleAction(action)
		}
		card.Children(func() {
			ui.Row(c).Shrink(0).Gap(12).Padding(13, 15).AlignItems(ui.Start).
				BorderWidth(0, 0, 1, 0).BorderColor(colors.Border.Color()).
				Children(func() {
					ui.Column(c).Grow(1).Basis(0).Gap(3).Children(func() {
						ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
							ui.Text(c, title+" to").FontSize(theme.SizeEmphasis).FontWeight(600).TextColor(colors.Text.Color())
							ui.Text(c, target).Font(theme.MonoFontFamily).FontSize(theme.SizeEmphasis).FontWeight(600).TextColor(colors.Text.Color())
						})
						copy := "One transaction, down this tab's own session. In auto mode it commits when the last statement lands; any refusal rolls all of it back."
						if tab.inTransaction {
							copy = "Inside the transaction this tab holds, behind a savepoint of its own. Nothing commits until you do; a refusal rolls back to the savepoint."
						}
						ui.Text(c, copy).FontSize(theme.SizeBody).TextColor(colors.TextSecondary.Color())
					})
					dialogKeyBadge(c, "esc", colors)
				})
			ui.Scroll(c.Key("preview-sql")).Padding(8, 0).MaxHeight(420).Font(theme.MonoFontFamily).FontSize(theme.SizeBody).
				Children(func() {
					for i, statement := range preview.sql {
						wash, rail := colors.EditBar, colors.EditBorder
						switch {
						case strings.HasPrefix(statement, "INSERT"):
							wash, rail = colors.EditAddedSurface, colors.EnvDevInner
						case strings.HasPrefix(statement, "DELETE"):
							wash, rail = colors.EditDeletedSurface, colors.EnvProdInner
						}
						for j, line := range strings.Split(statement, "\n") {
							ui.Row(c.Key(fmt.Sprintf("preview-%d-%d", i, j))).
								Shrink(0).
								MinHeight(22).
								AlignItems(ui.Center).
								Background(wash.Color()).
								Children(func() {
									ui.Box(c).Width(2).FillHeight().Background(rail.Color())
									ui.Text(c, line).TextColor(colors.TextBody.Color()).Padding(0, 14).NoWrap()
								})
						}
					}
				})
			ui.Row(c).Shrink(0).Gap(16).Padding(8, 15).AlignItems(ui.Center).
				BorderWidth(1, 0, 0, 0).BorderColor(colors.Hairline.Color()).
				Background(colors.Panel.Color()).
				FontSize(theme.SizeMeta).TextColor(colors.TextSecondary.Color()).
				Children(func() {
					ui.Text(c, "each row is named by its primary key; a row that is gone rolls the apply back").SingleLine()
					ui.Box(c).Grow(1).Basis(0)
					ui.Text(c, "values go as parameters; shown here as literals").NoWrap()
				})
			ui.Row(c).Shrink(0).Gap(10).Padding(9, 15).AlignItems(ui.Center).
				BorderWidth(1, 0, 0, 0).BorderColor(colors.Border.Color()).
				Children(func() {
					barLink(c, "copy into a new query tab", colors.AccentDeep, func() { s.previewIntoTab() })
					ui.Box(c).Grow(1).Basis(0)
					cancel := ui.Box(c.Key("preview-cancel")).
						Padding(5, 12).
						Border(1, colors.BorderStrong.Color()).
						Radius(6).
						FontSize(theme.SizeBody).
						TextColor(colors.Text.Color()).
						Cursor(ui.CursorPointer)
					cancel.OnClick(func() { s.closePreview() })
					cancel.Children(func() { ui.Text(c, "cancel") })
					barButton(c, "apply", "⌘⏎", true, !tab.edit.applying, colors, func() { s.applyEdits(nil) })
				})
		})
	})
}

// editGuardOverlay is the question a run asks over staged changes.
func (s *Shell) editGuardOverlay(c *ui.Context, colors *theme.ThemeColors) {
	guard := s.editGuard
	if guard == nil {
		return
	}
	tab, ok := s.tabMut(guard.tab).(*QueryTab)
	if !ok {
		s.editGuard = nil
		return
	}
	staged := tab.staged()
	scrim := ui.Row(c.Key("edit-guard-scrim")).
		Absolute().
		Top(0).
		Left(0).
		Fill().
		Justify(ui.Center).
		AlignItems(ui.Start).
		Padding(confirmTopMargin, 0, 0, 0).
		Background(colors.Overlay.Color())
	scrim.OnClick(func() {})
	scrim.Children(func() {
		card := ui.Column(c.Key("edit-guard")).
			Focusable().
			TabStop(false).
			Bind(&guard.focus).
			Width(confirmWidth+80).
			Gap(10).
			Padding(18).
			Border(1, colors.BorderStrong.Color()).
			Radius(10).
			Background(colors.Elevated.Color())
		card.OnClick(func() {})
		overlayShadow(card, colors)
		if action, ok := uikit.DispatchKeys(card, editGuardViewKeys); ok {
			s.handleAction(action)
		}
		card.Children(func() {
			ui.Text(c, fmt.Sprintf("Run again over %d staged %s?", staged, plural(staged, "change", "changes"))).
				FontSize(theme.SizeEmphasis).
				FontWeight(600).
				TextColor(colors.Text.Color())
			ui.Text(c, "A run replaces the rows on screen, and staged changes name rows of this result.").
				FontSize(theme.SizeBody).
				TextColor(colors.TextSecondary.Color())
			ui.Row(c).AlignItems(ui.Center).Gap(8).Padding(4, 0, 0, 0).Children(func() {
				stay := ui.Box(c.Key("guard-stay")).
					Padding(5, 12).
					Border(1, colors.BorderStrong.Color()).
					Radius(6).
					FontSize(theme.SizeBody).
					TextColor(colors.Text.Color()).
					Cursor(ui.CursorPointer)
				stay.OnClick(func() { s.answerEditGuard(false, false) })
				stay.Children(func() { ui.Text(c, "stay ⏎") })
				ui.Box(c).Grow(1).Basis(0)
				barButton(c, "apply, then run", "⌘S", true, true, colors, func() { s.answerEditGuard(true, false) })
				discard := ui.Row(c.Key("guard-discard")).
					AlignItems(ui.Center).
					Gap(7).
					Padding(4, 5, 4, 10).
					Border(1, colors.EnvProd.Color()).
					Radius(6).
					Background(colors.EnvProd.Color()).
					Cursor(ui.CursorPointer)
				discard.OnClick(func() { s.answerEditGuard(false, true) })
				discard.Children(func() {
					ui.Text(c, "discard and run").FontSize(theme.SizeBody).FontWeight(600).TextColor(colors.Window.Color()).NoWrap()
					keycap(c, "⌘⏎", theme.SizeCaption, colors.KeyOnFillSurface, colors.KeyOnFillBorder, colors.Window)
				})
			})
		})
	})
}
