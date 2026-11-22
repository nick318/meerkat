// The shell's frame: the environment ring, the title row and its tabs,
// the mode mark, the pane handles and the drag surfaces (shell.rs, agent
// K3b's part — see docs/tasks/shell-split.md).

package main

import (
	"fmt"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// The environment ring's width. The frame's `border_3` must agree.
const ringWidth float32 = 3.

// transparentBlack is GPUI's `transparent_black()`: a border that keeps
// its pixel of layout and paints nothing.
var transparentBlack = theme.Hsla{}

// The tables dispatched on the shell's root element, which wore the Rust
// `key_context("Shell")`: the shell's own keys, and the Shell halves of
// the tables that were split by context (mapping sheet §8).
var shellViewKeys = concatBindings(
	ShellKeyBindings(),
	ColumnFindShellKeyBindings(),
	ResultFindShellKeyBindings(),
	CatalogFilterShellKeyBindings(),
)

// The palette's Shell half (⌘K), dispatched on the same root.
var paletteShellViewKeys = PaletteShellKeyBindings()

// The tables dispatched on the grid's element, which wore
// `GRID_KEY_CONTEXT`.
var gridViewKeys = concatBindings(
	GridKeyBindings(),
	ResultFindGridKeyBindings(),
	PeekGridKeyBindings(),
	EditGridKeyBindings(),
)

// The grid's element while a cell editor is open in it: see
// CellEditorKeyBindings.
var cellEditorViewKeys = CellEditorKeyBindings()

// The overlays' and the inner contexts' own tables.
var (
	columnFindViewKeys    = ColumnFindKeyBindings()
	catalogFilterViewKeys = CatalogFilterKeyBindings()
	peekViewKeys          = PeekKeyBindings()
	confirmViewKeys       = ConfirmKeyBindings()
	previewViewKeys       = PreviewKeyBindings()
	editGuardViewKeys     = EditGuardKeyBindings()
	paletteViewKeys       = PaletteKeyBindings()
)

func concatBindings[A comparable](tables ...[]uikit.KeyBinding[A]) []uikit.KeyBinding[A] {
	var all []uikit.KeyBinding[A]
	for _, table := range tables {
		all = append(all, table...)
	}
	return all
}

// frameOverlay is the environment frame, painted a second time over the
// content. The panels at the bottom corners are square and paint after the
// shell's borders, so without this pass they would cover the curved ring
// and hairline. No background and no listeners: it colors pixels only.
//
// Both layers are positioned absolutely in the same frame, never laid
// out inside a border: border layout rounds to layout pixels, which
// slid the hairline's curve off the ring's and let the panel show
// through as a light arc at each corner. The hairline is also drawn a
// point wide of itself, tucked under the ring, so the two curves
// overlap instead of meeting edge to edge — an abutting seam leaks
// background on fractional display scales, an overlapped one cannot.
// The ring paints last and keeps the visible hairline to one point.
//
// Go port: "no listeners" is `PassThrough` — mygo hit-tests the topmost
// painted element, and a frame over the whole window would otherwise
// take every press.
func frameOverlay(c *ui.Context, env Env, radius float32, colors *theme.ThemeColors) ui.Element {
	tuck := ringWidth - 1.
	return ui.Box(c.Key("env-frame")).
		Absolute().
		Top(0).Left(0).Right(0).Bottom(0).
		PassThrough().
		Children(func() {
			inner := maxF32(radius-tuck, 0.)
			ui.Box(c).
				Absolute().
				Top(tuck).Left(tuck).Right(tuck).Bottom(tuck).
				PassThrough().
				Border(2, env.Inner(colors).Color()).
				Radius(0, 0, inner, inner)
			ui.Box(c).
				Absolute().
				Top(0).Left(0).Right(0).Bottom(0).
				PassThrough().
				Border(3, env.Ring(colors).Color()).
				Radius(0, 0, radius, radius)
		})
}

// View is `impl Render for Shell`.
func (s *Shell) View(c *ui.Context) ui.Element {
	colors := &theme.Current().Colors

	// The comp's environment frame: a tagged session wears its ring
	// around the whole window, with a hairline of the same family
	// just inside it. An untagged session wears nothing.
	//
	// macOS masks the window to rounded bottom corners, so a square
	// ring loses its corners to that mask. The ring and the hairline
	// curve to the same radius instead — except in fullscreen, where
	// the mask is square. The panels at the bottom corners stay
	// square and paint after the borders, so `frame_overlay` repaints
	// both curves on top of them.
	//
	// The ring's border lives on `framed`, never on `root`: an
	// absolutely positioned child is placed in its parent's padding
	// box, so a border on `root` would shove `frame_overlay` inward
	// by the ring's own width. `root` still rounds its background,
	// which keeps the corners outside the ring dark under the mask.
	radius := windowCornerRadius()
	if s.window != nil && s.window.IsFullScreen() {
		radius = 0.
	}
	innerRadius := maxF32(radius-ringWidth, 0.)

	root := ui.Column(c.Key("shell")).
		Focusable().
		TabStop(false).
		Bind(&s.focusHandle).
		Fill().
		Background(colors.Window.Color()).
		Font(theme.UiFontFamily).
		TextColor(colors.TextBody.Color())
	if s.env != nil {
		root.Radius(0, 0, radius, radius)
	}
	// The `Shell` key context and its `on_action`s.
	if action, ok := uikit.DispatchKeys(root, shellViewKeys); ok {
		s.handleAction(action)
	}
	if action, ok := uikit.DispatchKeys(root, paletteShellViewKeys); ok {
		s.handlePaletteAction(action)
	}

	return root.Children(func() {
		framed := ui.Column(c.Key("framed")).Fill()
		if s.env != nil {
			framed.Border(3, s.env.Ring(colors).Color()).Radius(0, 0, radius, radius)
		}
		framed.Children(func() {
			content := ui.Column(c.Key("content")).Fill()
			if s.env != nil {
				content.Border(1, s.env.Inner(colors).Color()).
					Radius(0, 0, innerRadius, innerRadius)
			}
			content.Children(func() {
				s.titleRow(c, colors)
				ui.Row(c.Key("workspace")).
					Grow(1).Basis(0).
					MinHeight(0).
					AlignItems(ui.Stretch).
					// For the divider, which is positioned off the
					// sidebar's width. It is the row's *last* child, so
					// its hitbox paints over both neighbours and wins the
					// three pixels it overhangs each of them by.
					Children(func() {
						s.sidebar(c, colors)
						ui.Column(c.Key("main")).
							Grow(1).Basis(0).
							MinWidth(0).
							Children(func() {
								s.connectStrip(c, colors)
								s.pane(c, colors)
								// While a cell is being edited the strip is
								// about the edit: what it was and the keys.
								if !s.editingStatus(c, colors) {
									s.statusStrip(c, colors)
								}
							})
						s.paneHandle(c, DividerSidebar, colors)
					})
			})
		})
		if s.env != nil {
			frameOverlay(c, *s.env, radius, colors)
		}
		// **Under every overlay**, and painted before them for that
		// reason: the toast is a notice that arrives on its own, and
		// a card floating over the close dialog's scrim would offer a
		// restart in the middle of a question about ending runs.
		// 46px clears the status strip: a card over the row that
		// reports the run would hide the answer to the last question
		// the user asked.
		toast(c, colors, 46.)
		// Under the palette and the dialog, both of which clear it on
		// the way up, so the order only settles the one frame where two
		// could be painted.
		s.columnFindOverlay(c, colors)
		s.peekOverlay(c, colors)
		s.previewOverlay(c, colors)
		s.editGuardOverlay(c, colors)
		s.paletteOverlay(c, colors)
		// Last, so it paints over the palette on the one frame where
		// both could be up.
		s.confirmOverlay(c, colors)
		// `pane_drag_surface` paints nothing and takes no room; in Go it
		// is the handle's own input (see paneDragSurface).
	})
}

// titleRow is the one bar over the workspace, which is also the window's
// title bar. The first edition stacked three here — the system's empty
// title bar, a breadcrumb bar and the tab strip, a hundred pixels before
// the first line of SQL — and each said part of one thing: which database,
// which tab. So the bar holds, left to right, the traffic lights, the way
// back to the connections screen under the session's name, the tabs, the
// palette, and the two marks that say what this window may do to the
// database.
//
// The session's part is as wide as the sidebar under it, so the tabs
// start over the pane they switch. A tagged session tints the whole bar
// with the environment's wash, as the breadcrumb bar did: the frame warns
// at the edges and the bar warns where the eye is.
//
// Go port: `TitleDrag::claim` is gone (see uikit.TitleBar): every control
// here is clickable, which is what keeps its press from the bar.
func (s *Shell) titleRow(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	barBg, barRule := colors.Panel, colors.Border
	if s.env != nil {
		barBg, barRule = s.env.Surface(colors), s.env.Inner(colors)
	}
	var dot theme.Hsla
	switch s.status.(type) {
	case StatusConnected:
		dot = colors.Ok
	case StatusFailed:
		dot = colors.ErrorMark
	default:
		dot = colors.Idle
	}
	sessionWidth := maxF32(s.sidebarWidth-uikit.TrafficLightsWidth, 96.)

	return uikit.TitleBar(c, "title-bar").
		Gap(8).
		Padding(0, 12, 0, uikit.TrafficLightsWidth).
		BorderWidth(0, 0, 1, 0).
		BorderColor(barRule.Color()).
		Background(barBg.Color()).
		FontSize(theme.SizeEmphasis).
		Children(func() {
			ui.Row(c).
				Width(sessionWidth - 8.).
				Shrink(0).
				AlignItems(ui.Center).
				Children(func() {
					button := ui.Row(c.Key("connections")).
						Height(tabHeight).
						MaxWidthPercent(100).
						AlignItems(ui.Center).
						Gap(8).
						Padding(0, 8).
						Radius(7).
						Cursor(ui.CursorPointer)
					if button.Hovered() {
						button.Background(colors.Sunk.Color())
					}
					button.OnClick(func() {
						// Leaving drops every tab at once, so it asks
						// about all of them — `proceed_close` writes
						// the strip back before the shell goes.
						if s.guardClose(Close{Kind: CloseKindShell}) {
							s.proceedClose(Close{Kind: CloseKindShell})
						}
					})
					button.Children(func() {
						uikit.StatusDot(c, dot).Size(7, 7).Radius(3.5).Shrink(0)
						ui.Box(c).
							MinWidth(0).
							FontWeight(600).
							TextColor(colors.Text.Color()).
							Children(func() { ui.Text(c, s.sessionName()).SingleLine() })
						uikit.DrawIcon(c, uikit.IconChevronDown, 11., colors.TextMuted)
					})
				})
			ui.Box(c).Shrink(0).Width(1).Height(20).Background(barRule.Color())
			s.tabsView(c, colors)
			palette := ui.Row(c.Key("palette-button")).
				Shrink(0).
				Width(232).
				Height(28).
				AlignItems(ui.Center).
				Gap(8).
				Padding(0, 5, 0, 10).
				Border(1, colors.Border.Color()).
				Radius(7).
				Background(colors.Window.Color()).
				FontSize(theme.SizeBody).
				TextColor(colors.TextMuted.Color()).
				Cursor(ui.CursorPointer)
			if palette.Hovered() {
				palette.BorderColor(colors.BorderStrong.Color())
			}
			palette.OnClick(func() { s.togglePalette() })
			palette.Children(func() {
				uikit.DrawIcon(c, uikit.IconSearch, 12., colors.TextMuted)
				ui.Box(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
					ui.Text(c, "Search tables and history").SingleLine()
				})
				uikit.Keycap(c, "⌘K")
			})
			if env := s.env; env != nil {
				// The badge names the frame, so the tint never has to be
				// decoded from memory. It sits on the paper with a ring of
				// the frame's colour rather than filled with it: a sand or
				// green fill under white text is too faint to read.
				ui.Row(c.Key("env-badge")).
					Shrink(0).
					Height(22).
					AlignItems(ui.Center).
					Gap(6).
					Padding(0, 8).
					Border(1, env.Ring(colors).Color()).
					Radius(6).
					Background(colors.Raised.Color()).
					FontSize(theme.SizeMeta).
					FontWeight(600).
					TextColor(env.Text(colors).Color()).
					Children(func() {
						uikit.StatusDot(c, env.Ring(colors))
						ui.Text(c, env.AsStr())
					})
			}
			s.modeMark(c, colors)
		})
}

// tabsView is the tabs, inside the title bar. They scroll inside their
// part of the bar rather than pushing the palette off the end, and each
// keeps a width between the two bounds: a long table name is truncated,
// and a short one is not squeezed to its text.
//
// Go port: Rust's `tabs`; the name is the Shell's field.
func (s *Shell) tabsView(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	return ui.ScrollHorizontal(c.Key("tabs")).
		Grow(1).Basis(0).
		MinWidth(0).
		Scrollbars(false).
		Children(func() {
			ui.Row(c).
				AlignItems(ui.Center).
				Gap(4).
				Children(func() {
					for ix, tab := range s.tabs {
						s.tabItem(c, ix, tab, colors)
					}
					add := ui.Row(c.Key("new-query")).
						Shrink(0).
						Size(28, 28).
						AlignItems(ui.Center).
						Justify(ui.Center).
						Radius(7).
						Cursor(ui.CursorPointer)
					if add.Hovered() {
						add.Background(colors.Sunk.Color())
					}
					add.OnClick(func() { s.newQuery() })
					add.Children(func() {
						uikit.DrawIcon(c, uikit.IconPlus, 12., colors.TextMuted)
					})
				})
		})
}

func (s *Shell) tabItem(c *ui.Context, ix int, tab Tab, colors *theme.ThemeColors) {
	isActive := ix == s.active
	id := tab.ID()
	var glyph uikit.Icon
	switch tab := tab.(type) {
	case *TableTab:
		if tab.kind == introspect.TableKindView {
			glyph = uikit.IconView
		} else {
			glyph = uikit.IconTable
		}
	case *QueryTab:
		if tab.relation != nil {
			glyph = uikit.IconTable
		} else {
			glyph = uikit.IconQuery
		}
	case *HistoryTab:
		glyph = uikit.IconHistory
	}
	item := ui.Row(c.Key(fmt.Sprintf("tab-%d", id))).
		Height(tabHeight).
		Shrink(0).
		MinWidth(tabMinWidth).
		MaxWidth(tabMaxWidth).
		AlignItems(ui.Center).
		Gap(8).
		Padding(0, 6, 0, 11).
		Radius(7).
		FontSize(theme.SizeEmphasis).
		Cursor(ui.CursorPointer)
	item.OnClick(func() {
		s.activate(ix)
		// The tab that was clicked takes the focus, each kind the
		// way it takes it when it opens: a query tab into its
		// editor, a table tab into its grid. Leaving the focus
		// where it was would leave it on a control the click has
		// just taken off the screen.
		s.focusActiveTab()
	})
	if isActive {
		item.Border(1, colors.Border.Color()).Background(colors.Window.Color())
		uikit.BoxShadow{OffsetX: 0, OffsetY: 1, BlurRadius: 2, Color: colors.ShadowSoft}.Apply(item)
	} else {
		item.Border(1, transparentBlack.Color())
		if item.Hovered() {
			item.Background(colors.Sunk.Color())
		}
	}
	item.Children(func() {
		glyphInk := colors.TextMuted
		if isActive {
			glyphInk = colors.Accent
		}
		uikit.DrawIcon(c, glyph, 13., glyphInk)
		// The title is what gives way when the tab is at its
		// widest, so it takes the room the glyph and the ×
		// leave and truncates inside it.
		//
		// Go port: `flex_1` here is grow and shrink with the
		// content as its basis, so the tab sizes to its title between
		// the two bounds as Taffy's max-content sizing gave it.
		weight, ink := 400, colors.TextSecondary
		if isActive {
			weight, ink = 500, colors.Text
		}
		ui.Box(c).
			Grow(1).Shrink(1).
			MinWidth(0).
			FontWeight(weight).
			TextColor(ink.Color()).
			Children(func() { ui.Text(c, tab.Title()).SingleLine() })
		// A tab holding changes not yet applied says so from the strip,
		// so a tab the user walked away from does not hide them.
		if query, ok := tab.(*QueryTab); ok && query.staged() > 0 {
			ui.Box(c.Key(fmt.Sprintf("staged-%d", id))).
				Shrink(0).
				Size(7, 7).
				Radius(3.5).
				Background(colors.Edit.Color())
		}
		closeInk := colors.TextFaint
		if isActive {
			closeInk = colors.TextMuted
		}
		close := ui.Row(c.Key(fmt.Sprintf("close-%d", id))).
			Shrink(0).
			Size(18, 18).
			AlignItems(ui.Center).
			Justify(ui.Center).
			Radius(4).
			FontSize(theme.SizeInput).
			TextColor(closeInk.Color()).
			Cursor(ui.CursorPointer)
		if close.Hovered() {
			close.Background(colors.Sunk.Color()).TextColor(colors.Error.Color())
		}
		close.OnClick(func() {
			what := closeIntent(len(s.tabs), id)
			if s.guardClose(what) {
				s.proceedClose(what)
			}
		})
		close.Children(func() { ui.Text(c, "×") })
	})
}

// modeMark is the comp's mode mark: a padlock and one word, in the top bar
// beside the environment badge. It is the answer to "can this window
// change the database", and it is on screen at all times because that
// question must never need a click.
//
// Read-only wears the dev family's green and read-write the prod family's
// clay — the same warning the frame gives, in the same tones, so the two
// marks agree with each other. A session that never connected has nothing
// to be read-only against and goes grey.
func (s *Shell) modeMark(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	label, surface, border, ink := s.modeTones(colors)
	return ui.Row(c.Key("mode-mark")).
		Shrink(0).
		Height(22).
		AlignItems(ui.Center).
		Gap(6).
		Padding(0, 8).
		Border(1, border.Color()).
		Radius(6).
		Background(surface.Color()).
		Children(func() {
			uikit.LockGlyph(c, ink)
			ui.Text(c, label).
				FontSize(theme.SizeMeta).
				FontWeight(600).
				TextColor(ink.Color())
		})
}

// modeTones is the mark's label and its three tones. A session that never
// connected is grey whichever mode it asked for.
func (s *Shell) modeTones(colors *theme.ThemeColors) (string, theme.Hsla, theme.Hsla, theme.Hsla) {
	if _, failed := s.status.(StatusFailed); failed {
		return "Offline", colors.ModeOffSurface, colors.ModeOffBorder, colors.ModeOffText
	}
	if s.readOnly {
		return "Read-only", colors.EnvDevSurface, colors.EnvDevInner, colors.EnvDevText
	}
	return "Read-write", colors.EnvProdSurface, colors.EnvProdInner, colors.EnvProdText
}

// sessionName is what to call this session on screen. The profile's name
// first, then the database the connection reports, then the app's own
// name.
func (s *Shell) sessionName() string {
	if s.name != nil {
		return *s.name
	}
	if s.label != nil && s.label.Database != "" {
		return s.label.Database
	}
	return "meerkat"
}

// --- resizable panes ------------------------------------------------------

// paneHandle is a divider's handle: a 7px hitbox straddling the 1px
// border it lives on. It paints nothing until hovered or dragged — the
// border under it is the mark, and the resize cursor is what names the
// gesture. `occlude` because it overhangs its neighbours by three pixels
// each way, and a press there must be a drag, never also a click on
// whatever is under it.
//
// Go port: `occlude` is nothing (mapping sheet §10); the handle is built
// after both neighbours, so it is the topmost element under the press.
func (s *Shell) paneHandle(c *ui.Context, divider Divider, colors *theme.ThemeColors) ui.Element {
	dragging := s.paneDrag != nil && s.paneDrag.divider == divider
	var handle ui.Element
	// Where the handle's box sits in its parent, as this frame lays it
	// out: what the pointer's position is measured from (see
	// paneDragSurface).
	var left, top float32
	switch divider {
	case DividerSidebar:
		left = s.sidebarWidth - handleHitbox/2.
		handle = ui.Row(c.Key(divider.key())).
			Absolute().
			Top(0).
			Left(left).
			FillHeight().
			Width(handleHitbox).
			AlignItems(ui.Stretch).
			Justify(ui.Center)
	default:
		top = s.editorHeight - handleHitbox/2.
		handle = ui.Column(c.Key(divider.key())).
			Absolute().
			Left(0).
			Bottom(-handleHitbox / 2.).
			FillWidth().
			Height(handleHitbox).
			Justify(ui.Center)
	}
	handle.Cursor(divider.cursor())
	hovered := handle.Hovered()
	viewportWidth, viewportHeight := c.Size()
	handle.HandleInput(func(ev ui.InputEvent) bool {
		if ev.Kind == ui.InputPointerDown {
			if ev.Button != 0 {
				return false
			}
			s.onDividerDown(divider, left+ev.X, top+ev.Y, ev.Clicks)
			return true
		}
		return s.paneDragSurface(ev, left, top, viewportWidth, viewportHeight)
	})
	return handle.Children(func() {
		line := ui.Box(c)
		switch divider {
		case DividerSidebar:
			line.Width(handleLine).FillHeight()
		default:
			line.Height(handleLine).FillWidth()
		}
		if hovered {
			line.Background(colors.Accent.Color())
		} else if dragging {
			line.Background(colors.AccentDeep.Color())
		}
	})
}

// peekDragSurface is the listeners a marking drag runs on, for as long as
// the button is down.
//
// It is the pane divider's own pattern, and here the reason is the
// grid's: a `div` hears a move only while its hitbox is hovered, and
// the pointer marking a line runs off the card in the first few
// pixels. It paints nothing and takes no room; it exists to hang
// listeners from.
//
// Go port: the window listener is mygo's pointer capture — the text that
// took the press gets every move and the release wherever the pointer
// goes, and its I-beam holds for the press. This is the half of its
// `HandleInput` after the press; `offset` turns a position into an
// offset in the painted text.
func (s *Shell) peekDragSurface(ev ui.InputEvent, offset func(x, y float32) int) bool {
	if s.peek == nil || s.peek.anchor == nil {
		return false
	}
	switch ev.Kind {
	case ui.InputPointerMove:
		// A release nothing here heard about must not leave the card
		// marking text for ever.
		if ev.Button != 0 {
			s.endPeekDrag()
			return false
		}
		s.peekDrag(offset(ev.X, ev.Y))
		return true
	case ui.InputPointerUp:
		if ev.Button == 0 {
			s.endPeekDrag()
			return true
		}
	}
	return false
}

// paneDragSurface is the listeners a drag hangs on the window, alive only
// while one is out. The handle is 7px and the pointer leaves it on the
// first frame, so handlers bound to its hitbox would go deaf exactly when
// they are needed — the grid's `DragSurface` reasoning, and this is its
// pattern: an element that paints nothing and takes no room.
//
// Go port: mygo's pointer capture gives the handle that took the press
// every move and the release, wherever the pointer goes, and the handle's
// cursor holds for the press — so this is the half of the handle's
// `HandleInput` after the press. The position is the handle's own plus
// where its box sat in this frame's layout (`left`, `top`), so it stays
// put while the handle moves with the pane; the drag only reads its
// travel.
func (s *Shell) paneDragSurface(ev ui.InputEvent, left, top, viewportWidth, viewportHeight float32) bool {
	if s.paneDrag == nil {
		return false
	}
	switch ev.Kind {
	case ui.InputPointerMove:
		// A button that came up somewhere nothing here heard about
		// ends the drag, rather than leaving the pane glued to the
		// pointer.
		if ev.Button != 0 {
			s.endPaneDrag()
			return false
		}
		s.dragPaneTo(left+ev.X, top+ev.Y, viewportWidth, viewportHeight)
		return true
	case ui.InputPointerUp:
		if ev.Button == 0 {
			s.endPaneDrag()
			return true
		}
	}
	return false
}
