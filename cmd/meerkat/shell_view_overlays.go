// The shell's overlays — the close confirmation, the value peek and the
// ⌘K palette — and the small parts every view shares: the page links,
// the keycaps (shell.rs, agent K3b's part — see docs/tasks/shell-split.md).

package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// overlayShadow is the lift every dialog of the shell wears: the
// palette's, the peek's and the confirmation's.
func overlayShadow(el ui.Element, colors *theme.ThemeColors) ui.Element {
	return uikit.BoxShadow{OffsetX: 0, OffsetY: 24, BlurRadius: 60, SpreadRadius: -20, Color: colors.Shadow}.Apply(el)
}

// confirmOverlay is the close confirmation. The palette's pattern — an
// absolutely positioned child of the shell, never a window of its own —
// so answering it hands the focus straight back to the workspace.
//
// The scrim does **not** dismiss on a click, unlike the palette's. A
// stray click must not be an answer to a question about ending server
// work; ⎋ is the way out, and it says so.
func (s *Shell) confirmOverlay(c *ui.Context, colors *theme.ThemeColors) {
	confirm := s.confirm
	if confirm == nil {
		return
	}
	copy := confirmCopy(confirm.what, confirm.running, confirm.open, confirm.staged...)
	// Every tab the close would touch, named once even when it is in
	// both lists.
	var named []string
	names := append(slices.Clone(confirm.running), confirm.open...)
	for _, tab := range confirm.staged {
		names = append(names, tab.name)
	}
	for _, name := range names {
		if !slices.Contains(named, name) {
			named = append(named, name)
		}
	}

	scrim := ui.Row(c.Key("confirm-scrim")).
		Absolute().
		Top(0).
		Left(0).
		Fill().
		Justify(ui.Center).
		AlignItems(ui.Start).
		Padding(confirmTopMargin, 0, 0, 0).
		Background(colors.Overlay.Color())
	// `.occlude()`: the scrim takes the press so nothing under it hears
	// it, and does nothing with it.
	scrim.OnClick(func() {})
	scrim.Children(func() {
		card := ui.Column(c.Key("confirm")).
			Focusable().
			TabStop(false).
			Bind(&confirm.focus).
			Width(confirmWidth).
			Gap(10).
			Padding(18).
			Border(1, colors.BorderStrong.Color()).
			Radius(10).
			Background(colors.Elevated.Color())
		card.OnClick(func() {})
		overlayShadow(card, colors)
		if action, ok := uikit.DispatchKeys(card, confirmViewKeys); ok {
			s.handleAction(action)
		}
		card.Children(func() {
			ui.Text(c, copy.Title).
				FontSize(theme.SizeEmphasis).
				FontWeight(600).
				TextColor(colors.Text.Color())
			for _, line := range copy.Body {
				ui.Text(c, line).
					FontSize(theme.SizeBody).
					TextColor(colors.TextSecondary.Color())
			}
			// With more than one tab at stake, name them: the
			// user is about to end work they cannot see from
			// here.
			if len(named) > 1 {
				ui.Box(c).
					FontSize(theme.SizeMeta).
					TextColor(colors.TextMuted.Color()).
					Children(func() { ui.Text(c, strings.Join(named, ", ")).SingleLine() })
			}
			ui.Row(c).
				AlignItems(ui.Center).
				Gap(8).
				Padding(4, 0, 0, 0).
				Children(func() {
					ui.Box(c).
						Grow(1).Basis(0).
						FontSize(theme.SizeMeta).
						TextColor(colors.TextFaint.Color()).
						Children(func() { ui.Text(c, "⌘⏎ confirms · ⎋ or ⏎ keeps") })
					keep := ui.Box(c.Key("confirm-keep")).
						Padding(5, 12).
						Border(1, colors.BorderStrong.Color()).
						Radius(6).
						FontSize(theme.SizeBody).
						TextColor(colors.Text.Color()).
						Cursor(ui.CursorPointer)
					if keep.Hovered() {
						keep.Background(colors.Panel.Color())
					}
					keep.OnClick(func() { s.closeConfirm() })
					keep.Children(func() { ui.Text(c, copy.Keep) })
					// Clay and filled, the tone the
					// terminate button wears: the control
					// that ends server work must not look
					// like the one that keeps it.
					act := ui.Box(c.Key("confirm-act")).
						Padding(5, 12).
						Border(1, colors.EnvProd.Color()).
						Radius(6).
						Background(colors.EnvProd.Color()).
						FontSize(theme.SizeBody).
						FontWeight(500).
						TextColor(colors.Window.Color()).
						Cursor(ui.CursorPointer)
					act.OnClick(func() {
						confirm := s.confirm
						if confirm == nil {
							return
						}
						s.confirm = nil
						s.proceedClose(confirm.what)
					})
					act.Children(func() { ui.Text(c, copy.Act) })
				})
		})
	})
}

// peekLayout is the card's shaped text, kept from one frame to the next:
// shaping four thousand characters is not a cost to pay per frame, and
// the text, the mark and the ink change only on a press or a drag.
type peekLayout struct {
	text     string
	start    int
	end      int
	ink      theme.Hsla
	layout   *ui.TextLayout
	selected theme.Hsla
}

// peekValueWidth is the width the card's text wraps at: the card, less
// its two borders and the value's padding either side.
const peekValueWidth = peekWidth - 2. - 2.*15.

// peekOverlay is the value peek: one cell's whole value, over the grid it
// came from.
//
// The scrim carries **no wash**. The palette's dims the workspace
// because the palette is a place the user has gone to; this card is a
// second look at something already on screen, and dimming the result
// behind it would hide what is being looked at. It still occludes, so a
// click cannot reach the grid underneath — and a click on it closes the
// card, which is what a click outside a peek means.
func (s *Shell) peekOverlay(c *ui.Context, colors *theme.ThemeColors) {
	peek := s.peek
	if peek == nil {
		return
	}
	column, row, value, ok := s.peekValue()
	if !ok {
		return
	}
	characters := len([]rune(value))
	// What is painted is bounded; what ⌘C copies is not.
	shown := takeChars(value, peekChars)
	cut := characters > peekChars
	// What is marked is painted by the text itself, as a highlight
	// over a byte range, because only the shaped text knows where a
	// character sits: a wash drawn by the card would have to lay the
	// string out a second time to work that out.
	marked := peek.selection
	var meta string
	switch {
	case characters == 1:
		meta = "1 character"
	case !cut:
		meta = fmt.Sprintf("%s characters", uikit.FormatCount(uint64(characters)))
	default:
		meta = fmt.Sprintf("first %s of %s characters",
			uikit.FormatCount(uint64(peekChars)), uikit.FormatCount(uint64(characters)))
	}
	// The word is what the grid paints for an absence, and it reads as
	// an absence there; it has to read the same here.
	ink := colors.TextBody
	if value == "NULL" {
		ink = colors.TextFaint
	}

	scrim := ui.Row(c.Key("peek-scrim")).
		Absolute().
		Top(0).
		Left(0).
		Fill().
		Justify(ui.Center).
		AlignItems(ui.Start).
		Padding(peekTopMargin, 0, 0, 0)
	scrim.OnClick(func() { s.closePeek() })
	scrim.Children(func() {
		card := ui.Column(c.Key("peek")).
			Focusable().
			TabStop(false).
			Bind(&peek.focus).
			Width(peekWidth).
			Clip().
			Border(1, colors.BorderStrong.Color()).
			Radius(10).
			Background(colors.Elevated.Color())
		// A click in the card is not a click on the scrim, so it must
		// not close it.
		card.OnClick(func() {})
		overlayShadow(card, colors)
		if action, ok := uikit.DispatchKeys(card, peekViewKeys); ok {
			s.handleAction(action)
		}

		// The layout is what turns a pointer into an offset, and the
		// handlers below hold **this** frame's layout, never a stale
		// one.
		cache := ui.Local(card, "peek-layout", func() peekLayout { return peekLayout{} })
		start, end := -1, -1
		if marked.Start != marked.End && marked.End <= len(shown) {
			start, end = marked.Start, marked.End
		}
		if cache.layout == nil || cache.text != shown || cache.start != start || cache.end != end ||
			cache.ink != ink || cache.selected != colors.Selection {
			spans := peekSpans(shown, start, end, ink, colors.Selection)
			*cache = peekLayout{
				text:     shown,
				start:    start,
				end:      end,
				ink:      ink,
				selected: colors.Selection,
				// In the mono, as the grid paints it: a uuid, a
				// `jsonb` blob or a column of digits reads by its
				// columns, and the cell it came from spelled it so.
				layout: ui.ShapeRichText(spans, ui.Font{Family: theme.MonoFontFamily, Size: theme.SizeBody}, peekValueWidth),
			}
		}
		layout := cache.layout
		_, textHeight := layout.Size()

		card.Children(func() {
			ui.Row(c).
				Shrink(0).
				AlignItems(ui.Center).
				Gap(10).
				Padding(13, 15).
				BorderWidth(0, 0, 1, 0).
				BorderColor(colors.Border.Color()).
				Children(func() {
					ui.Box(c).
						MinWidth(0).
						FontSize(theme.SizeEmphasis).
						FontWeight(600).
						TextColor(colors.Text.Color()).
						Children(func() { ui.Text(c, column).SingleLine() })
					ui.Text(c, fmt.Sprintf("row %d · %s", row, meta)).
						NoWrap().
						FontSize(theme.SizeBody).
						TextColor(colors.TextMuted.Color())
					ui.Box(c).Grow(1).Basis(0)
					closer := dialogKeyBadge(c.Key("close-peek"), "esc", colors).Cursor(ui.CursorPointer)
					if closer.Hovered() {
						closer.TextColor(colors.Accent.Color())
					}
					closer.OnClick(func() { s.closePeek() })
				})
			ui.Scroll(c.Key("peek-value")).
				Height(minF32(textHeight+2.*13., peekMaxHeight)).
				Shrink(0).
				Cursor(ui.CursorText).
				Children(func() {
					ui.Box(c).Padding(13, 15).Children(func() {
						offset := func(x, y float32) int { return offsetAt(layout, shown, x, y) }
						ui.Box(c.Key("peek-text")).
							Width(peekValueWidth).
							Height(textHeight).
							Cursor(ui.CursorText).
							HandleInput(func(ev ui.InputEvent) bool {
								if ev.Kind == ui.InputPointerDown {
									if ev.Button != 0 {
										return false
									}
									s.peekPress(offset(ev.X, ev.Y), ev.Clicks)
									return true
								}
								// Everything after the press: see
								// peekDragSurface.
								return s.peekDragSurface(ev, offset)
							}).
							Draw(func(p *ui.Painter, r ui.Rect) {
								p.TextLayout(layout, r.X, r.Y, ink.Color())
							})
					})
				})
			hint := "⌘C copies this value"
			switch {
			case start >= 0:
				hint = "⌘C copies what is marked"
			case cut:
				hint = "⌘C copies the whole value"
			}
			ui.Row(c).
				Shrink(0).
				AlignItems(ui.Center).
				Gap(14).
				Padding(9, 15).
				BorderWidth(1, 0, 0, 0).
				BorderColor(colors.Border.Color()).
				Background(colors.Panel.Color()).
				FontSize(theme.SizeBody).
				TextColor(colors.TextMuted.Color()).
				Children(func() {
					ui.Text(c, hint)
					ui.Box(c).Grow(1).Basis(0)
					ui.Text(c, "drag to mark · ⌘A all · ⏎ or ⎋ closes").
						TextColor(colors.TextFaint.Color())
				})
		})
	})
}

// peekSpans cuts the painted text at the mark's two byte offsets, so the
// marked run carries the selection's wash (GPUI's `HighlightStyle` over a
// byte range). `start` < 0 marks nothing.
func peekSpans(text string, start, end int, ink, selected theme.Hsla) []ui.Span {
	plain := func(s string) ui.Span {
		return ui.Span{Text: s, Font: theme.MonoFontFamily, Size: theme.SizeBody, Color: ink.Color()}
	}
	if start < 0 || start >= end || end > len(text) {
		return []ui.Span{plain(text)}
	}
	var spans []ui.Span
	if start > 0 {
		spans = append(spans, plain(text[:start]))
	}
	mark := plain(text[start:end])
	mark.Background = selected.Color()
	spans = append(spans, mark)
	if end < len(text) {
		spans = append(spans, plain(text[end:]))
	}
	return spans
}

// paletteOverlay is the palette, over everything. It is an absolutely
// positioned child of the shell rather than a window of its own, so it
// cannot outlive the workspace it searches, and closing it hands the
// focus straight back.
func (s *Shell) paletteOverlay(c *ui.Context, colors *theme.ThemeColors) {
	palette := s.palette
	if palette == nil {
		return
	}
	scrim := ui.Row(c.Key("palette-scrim")).
		Absolute().
		Top(0).
		Left(0).
		Fill().
		Justify(ui.Center).
		AlignItems(ui.Start).
		Padding(PaletteTopMargin, 0, 0, 0).
		Background(colors.Overlay.Color())
	// Nothing behind the scrim may be clicked or hovered
	// through it, or the grid reacts to a click meant to
	// dismiss the palette.
	scrim.OnClick(func() { s.closePalette() })
	scrim.Children(func() {
		card := ui.Column(c.Key("palette")).
			Width(PaletteWidth).
			Clip().
			Border(1, colors.BorderStrong.Color()).
			Radius(10).
			Background(colors.Elevated.Color()).
			// A press in the dialog leaves the focus on its search
			// line, as GPUI left it.
			KeepFocus()
		// A click inside the dialog is not a click on the
		// scrim, so it must not close it.
		card.OnClick(func() {})
		overlayShadow(card, colors)
		if action, ok := uikit.DispatchKeys(card, paletteViewKeys); ok {
			s.handlePaletteAction(action)
		}
		card.Children(func() {
			s.paletteHeader(c, palette, colors)
			s.paletteChips(c, palette, colors)
			s.paletteList(c, palette, colors)
			s.paletteFooter(c, palette, colors)
		})
	})
}

func (s *Shell) paletteHeader(c *ui.Context, palette *Palette, colors *theme.ThemeColors) ui.Element {
	return ui.Row(c).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(10).
		Padding(13, 15).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Border.Color()).
		Children(func() {
			uikit.MeerkatMark(c, 20.)
			ui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() { palette.query.View(c) })
			closer := dialogKeyBadge(c.Key("close-palette"), "esc", colors).Cursor(ui.CursorPointer)
			if closer.Hovered() {
				closer.TextColor(colors.Accent.Color())
			}
			closer.OnClick(func() { s.closePalette() })
		})
}

func (s *Shell) paletteChips(c *ui.Context, palette *Palette, colors *theme.ThemeColors) ui.Element {
	// The chip that is lit is the one the search is actually running
	// in, so a typed `t:` lights the tables chip too.
	scope, _ := PaletteParse(palette.query.Text(), palette.chip)
	return ui.Row(c).
		Shrink(0).
		Gap(3).
		Padding(8, 12).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Hairline.Color()).
		Background(colors.Panel.Color()).
		Children(func() {
			for _, chip := range PaletteScopes {
				PaletteScopeChip(c, chip, chip == scope, colors).
					OnClick(func() { s.setPaletteScope(chip) })
			}
		})
}

func (s *Shell) paletteList(c *ui.Context, palette *Palette, colors *theme.ThemeColors) ui.Element {
	if len(palette.rows) == 0 {
		note := "reading the catalog…"
		if s.catalog != nil {
			note = "nothing here goes by that name"
		}
		return ui.Box(c).
			Height(PaletteListHeight).
			Shrink(0).
			Padding(14, 17, 0, 17).
			FontSize(theme.SizeBody).
			TextColor(colors.TextFaint.Color()).
			Children(func() { ui.Text(c, note) })
	}

	rows := palette.rows
	selected := palette.selected
	onPick := func(pick PalettePick, newTab bool) { s.openPick(pick, newTab) }

	return ui.Column(c).
		Height(PaletteListHeight).
		Shrink(0).
		Padding(8).
		Children(func() {
			ui.List(c.Key("palette-rows"), &palette.scroll.List, len(rows), func(ix int) {
				PaletteRowView(c, ix, rows[ix], ix == selected, onPick, colors)
			}).
				Grow(1).Basis(0).
				MinHeight(0).
				TrackScroll(&palette.scroll.Handle.State)
		})
}

func (s *Shell) paletteFooter(c *ui.Context, palette *Palette, colors *theme.ThemeColors) ui.Element {
	hint := func(keys, what string) {
		ui.Row(c).Gap(5).Children(func() {
			ui.Text(c, keys).TextColor(colors.TextSecondary.Color())
			ui.Text(c, what)
		})
	}
	_, needle := PaletteParse(palette.query.Text(), palette.chip)
	// What the search found, in the comp's own words. With nothing
	// typed there is no count worth saying, only what to do next.
	var found string
	switch {
	case needle == "":
		found = "type to search tables and history"
	case palette.matches == 0:
		found = fmt.Sprintf("meerkat found nothing for “%s”", needle)
	case palette.matches == 1:
		found = fmt.Sprintf("meerkat found 1 match for “%s”", needle)
	default:
		found = fmt.Sprintf("meerkat found %d matches for “%s”", palette.matches, needle)
	}

	return ui.Row(c).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(14).
		Padding(9, 15).
		BorderWidth(1, 0, 0, 0).
		BorderColor(colors.Border.Color()).
		Background(colors.Panel.Color()).
		FontSize(theme.SizeBody).
		TextColor(colors.TextMuted.Color()).
		Children(func() {
			hint("↑↓", "move")
			hint("⇥", "complete")
			hint("⏎", "open")
			hint("⌘⏎", "new tab")
			ui.Box(c).Grow(1).Basis(0)
			ui.Box(c).
				MinWidth(0).
				TextColor(colors.TextFaint.Color()).
				Children(func() { ui.Text(c, found).SingleLine() })
		})
}

func (s *Shell) pageLink(c *ui.Context, label string, forward bool, colors *theme.ThemeColors) ui.Element {
	link := ui.Box(c.Key(label)).
		Shrink(0).
		TextColor(colors.TextSecondary.Color()).
		Cursor(ui.CursorPointer).
		KeepFocus()
	if link.Hovered() {
		link.TextColor(colors.AccentDeep.Color())
	}
	link.OnClick(func() { s.stepPage(forward) })
	return link.Children(func() { ui.Text(c, label).NoWrap() })
}

// offsetAt is where in the text a pointer landed. The layout answers with
// the nearest offset for a position off the end of a line, which is the
// one a drag reaching past the text is asking for.
//
// Go port: mygo's layout answers in UTF-16 offsets; every offset above it
// is a byte offset into `text`, the painted string.
func offsetAt(layout *ui.TextLayout, text string, x, y float32) int {
	return ui.UTF16ByteOffset(text, layout.IndexAt(ui.Point{X: x, Y: y}))
}

// keycap is a keycap in tones of the caller's choosing: the run button's
// and the transaction bar's caps sit on fills the plain `ui::keycap`
// cannot read on. Same shape as that one, so every key in the app looks
// like a key.
//
// The comp's keycap, as it appears **inside a button**: a small box with
// a 2px bottom edge, which is the whole of what makes it read as a key
// rather than as a chip.
//
// Every button in the app that carries a key uses this one — the run
// button's three verbs, and the transaction bar's commit and rollback.
// They are the only buttons that name a key, and a key drawn as a key on
// one and as trailing text on another would say they were different kinds
// of thing.
//
// The tones are the caller's, because they follow the **fill** rather than
// the key: `key_on_fill_*` over a filled button, paper over an outlined
// one. Both of the alpha pair carry alpha for exactly this reason — one
// cap has to work over ochre and over clay without a tone per state.
//
// **The padding is in proportion to the type**, the comp's 6 and 4 at its
// own 11px. A smaller cap has to be smaller in every direction: a 9px key
// in a box built for an 11px one is a small word in a big box, which reads
// as a mistake rather than as a smaller key. The bottom edge stays at 2px
// through all of it — it is what says "key", and 1.6px of it would only
// blur.
//
// [`key_badge`] is the flat cousin, for a key named in a hint or a footer
// rather than written on a button.
func keycap(c *ui.Context, keys string, size float32, surface, border, ink theme.Hsla) ui.Element {
	return ui.Box(c).
		Shrink(0).
		Padding(0, size*5./10.5).
		BorderWidth(1, 1, 2, 1).
		BorderColor(border.Color()).
		Radius(4).
		Background(surface.Color()).
		Font(theme.MonoFontFamily).
		FontSize(size).
		FixedLineHeight(size + 4.5).
		FontWeight(500).
		TextColor(ink.Color()).
		Children(func() { ui.Text(c, keys).NoWrap() })
}

// keyBadge is a keystroke, in the bordered pill the comp puts one in.
func keyBadge(c *ui.Context, keys string, colors *theme.ThemeColors) ui.Element {
	return keycap(c, keys, theme.SizeCaption, colors.Raised, colors.Border, colors.TextSecondary)
}

// dialogKeyBadge is the same badge in a dialog's header, a size up: it
// sits beside a title or a search line of 13 or 14px, and a 10px cap
// there reads as a speck rather than as the way out.
func dialogKeyBadge(c *ui.Context, keys string, colors *theme.ThemeColors) ui.Element {
	return keycap(c, keys, theme.SizeBody, colors.Raised, colors.Border, colors.TextSecondary)
}
