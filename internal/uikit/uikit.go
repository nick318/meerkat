// Package uikit is the small component kit on top of mygo's native UI,
// modeled on Zed's `ui` crate (the Rust crate `ui`, renamed so it does not
// collide with mygo's `ui`). Implements the warm-paper design language:
// hairline rules, bordered pills, one ochre accent. Grows as Meerkat needs
// more controls.
//
// Go port notes, which every function below follows:
//
//   - A GPUI builder that returned a `Div` for the caller to finish returns
//     a `ui.Element` here, already built into the current parent; the
//     caller chains more styles on it and, where the Rust added children,
//     calls `.Children`. A `…_bare` variant leaves `.Children` to the
//     caller, as the Rust left the child.
//   - `.hover(|s| …)` is `if el.Hovered() { … }` on the same element.
//   - GPUI's `blur_radius` is a Gaussian sigma; mygo's `Shadow` blur is
//     CSS's, twice the sigma (mygo internal/gpu/gpu.go:549, `sigma :=
//     op.Blur / 2`). So every blur here is the Rust value times two.
//   - Colors are `theme` tokens (GPUI `Hsla`), painted through
//     `Hsla.Color()`.
package uikit

import (
	"fmt"
	"math"
	"strconv"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/theme"
)

// TitleBarHeight is the height of the bar each screen draws where the
// system's title bar was. The traffic lights sit in its left end, so it
// has to be tall enough to centre them: `main` places them 15px down.
const TitleBarHeight float32 = 44.

// TrafficLightsWidth is the room the traffic lights take at the bar's left
// end.
const TrafficLightsWidth float32 = 78.

// TitleBar is the bar at the top of a screen, which is also the window's
// title bar: a press on it and a move drags the window, a double click
// zooms it. The caller lays out what is in it (`.Children`); the traffic
// lights are painted over its first `TrafficLightsWidth` pixels by the
// system.
//
// The Rust carried a `TitleDrag` that armed the drag on a bare press and
// let each control in the bar `claim` its own press, because GPUI bubbles a
// mouse-down from the deepest hitbox outwards and the bar would otherwise
// carry the window off whenever a tab was pressed and the pointer wobbled.
// mygo needs neither: `DragWindow` moves the window only when the innermost
// element under the press that takes the pointer is the bar itself (mygo
// ui/input.go:549-560 picks the innermost interactive element, and :602
// starts the window drag only for a `DragWindow` target that is nothing
// else), and a double click on it zooms as a title bar does. Any control
// that is clickable or focusable — a tab, a button — takes its own press.
// So `TitleDrag` and `claim` are gone, and the window is opened with
// `TitleBarStyle: mygo.TitleBarHidden` and `TrafficLightPosition` (14, 15).
func TitleBar(c *ui.Context, id any) ui.Element {
	return ui.Row(c.Key(id)).
		Height(TitleBarHeight).
		Shrink(0).
		AlignItems(ui.Center).
		Padding(0, 0, 0, TrafficLightsWidth).
		DragWindow()
}

// Keycap is a key written on a cap: the way every shortcut in the app is
// named, in a button, a footer or a hint. It sits on `Raised` with a deeper
// bottom edge, so it reads as a key and not as a chip.
func Keycap(c *ui.Context, key string) ui.Element {
	colors := &theme.Current().Colors
	return ui.Box(c).
		Shrink(0).
		Padding(0, 5).
		BorderWidth(1, 1, 2, 1).
		BorderColor(colors.Border.Color()).
		Radius(4).
		Background(colors.Raised.Color()).
		Font(theme.MonoFontFamily).
		FontSize(10.5).
		FixedLineHeight(15).
		TextColor(colors.TextSecondary.Color()).
		Children(func() { ui.Text(c, key) })
}

// KeyHint is a key and what it does, for a footer or a status strip:
// `⏎ open`.
func KeyHint(c *ui.Context, key, label string) ui.Element {
	return ui.Row(c).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(5).
		Children(func() {
			Keycap(c, key)
			ui.Text(c, label)
		})
}

// ControlRadius is the radius of a control: a button, a field, a chip.
const ControlRadius float32 = 7.

// SurfaceRadius is the radius of a surface that holds controls. Outer
// radius is inner radius plus the padding between them, or the two curves
// pull apart at the corner: a 7px control three pixels inside its card
// wants 10.
const SurfaceRadius float32 = 10.

// BoxShadow is one layer of a shadow, with GPUI's numbers: offset, a blur
// *radius* (a sigma) and a spread.
type BoxShadow struct {
	OffsetX, OffsetY float32
	BlurRadius       float32
	SpreadRadius     float32
	Color            theme.Hsla
}

// Apply adds the shadow to el, doubling the blur for mygo's CSS blur.
func (s BoxShadow) Apply(el ui.Element) ui.Element {
	return el.Shadow(s.OffsetX, s.OffsetY, 2*s.BlurRadius, s.SpreadRadius, s.Color.Color())
}

// WithShadows adds every layer to el, in order.
func WithShadows(el ui.Element, shadows []BoxShadow) ui.Element {
	for _, s := range shadows {
		s.Apply(el)
	}
	return el
}

// ControlShadow is the lift under a control that sits on the paper: one
// hairline of shadow, tight under its bottom edge. A border alone draws a
// box; the shadow is what says the box can be pressed.
func ControlShadow() []BoxShadow {
	colors := &theme.Current().Colors
	return []BoxShadow{{OffsetX: 0, OffsetY: 1, BlurRadius: 1.5, Color: colors.ShadowSoft}}
}

// SurfaceShadow is the lift under a surface that floats over the workspace
// — a card, a menu, a dialog. Two layers, as a real shadow has: a tight
// one that seats the edge and a wide soft one that says how far up it is.
func SurfaceShadow() []BoxShadow {
	colors := &theme.Current().Colors
	return []BoxShadow{
		{OffsetX: 0, OffsetY: 1, BlurRadius: 2, Color: colors.ShadowSoft},
		{OffsetX: 0, OffsetY: 8, BlurRadius: 24, SpreadRadius: -4, Color: colors.ShadowSoft},
	}
}

// Card is a bordered card surface sitting on a panel (connection card,
// inputs).
func Card(c *ui.Context) ui.Element {
	colors := &theme.Current().Colors
	el := ui.Box(c).
		Background(colors.Elevated.Color()).
		Border(1, colors.BorderStrong.Color()).
		Radius(SurfaceRadius)
	return WithShadows(el, SurfaceShadow())
}

// ToolbarButton is a small bordered toolbar button ("filter", "export",
// "format").
func ToolbarButton(c *ui.Context, text string) ui.Element {
	return ToolbarButtonBare(c).Children(func() { ui.Text(c, text) })
}

// ToolbarButtonBare is the same button with no word in it, for a button
// whose label is an element of its own: a word that fades as it changes
// has to be animated apart from the box around it, and GPUI gives one
// animation to one element.
func ToolbarButtonBare(c *ui.Context) ui.Element {
	colors := &theme.Current().Colors
	el := ui.Row(c).
		Height(28).
		AlignItems(ui.Center).
		Gap(7).
		Padding(0, 10).
		Border(1, colors.Border.Color()).
		Radius(ControlRadius).
		Background(colors.Raised.Color())
	WithShadows(el, ControlShadow())
	el.FontSize(12).
		TextColor(colors.TextSecondary.Color()).
		Cursor(ui.CursorPointer)
	if el.Hovered() {
		el.Background(colors.Window.Color()).BorderColor(colors.BorderStrong.Color())
	}
	return el
}

// AccentButton is the one filled button in a view ("Connect", "Save",
// "Restart now"). The fill is `AccentFill`, not `Accent`: white on the
// accent itself is 3.8:1, under what a label needs.
func AccentButton(c *ui.Context, text string) ui.Element {
	return AccentButtonBare(c).Children(func() { ui.Text(c, text) })
}

// AccentButtonBare is the same button with nothing in it, for one that
// carries a keycap.
func AccentButtonBare(c *ui.Context) ui.Element {
	colors := &theme.Current().Colors
	el := ui.Row(c).
		Height(28).
		AlignItems(ui.Center).
		Gap(7).
		Padding(0, 11).
		Radius(ControlRadius).
		Background(colors.AccentFill.Color())
	WithShadows(el, ControlShadow())
	el.FontSize(12).
		FontWeight(600).
		TextColor(colors.OnAccent.Color()).
		Cursor(ui.CursorPointer)
	if el.Hovered() {
		el.Background(colors.AccentDeep.Color())
	}
	return el
}

// KeycapOnFill is a keycap that sits *inside* a filled button: the cap has
// to read over the fill, so it is a veil of the ink rather than a tone of
// its own.
func KeycapOnFill(c *ui.Context, key string) ui.Element {
	colors := &theme.Current().Colors
	return ui.Box(c).
		Shrink(0).
		Padding(0, 5).
		BorderWidth(1, 1, 2, 1).
		BorderColor(colors.KeyOnFillBorder.Color()).
		Radius(4).
		Background(colors.KeyOnFillSurface.Color()).
		Font(theme.MonoFontFamily).
		FontSize(10.5).
		FixedLineHeight(15).
		FontWeight(500).
		Children(func() { ui.Text(c, key) })
}

// SectionLabel is a section's name ("Saved connections", "Tables").
// Sentence case, in the chrome's font: the first edition wrote these in
// 9px capitals in the faintest grey, which is a label nobody can read.
func SectionLabel(c *ui.Context, text string) ui.Element {
	colors := &theme.Current().Colors
	return ui.Box(c).
		FontSize(11.5).
		FontWeight(600).
		TextColor(colors.TextMuted.Color()).
		Children(func() { ui.Text(c, text) })
}

// StatusDot is a small status dot (green = connected, sand = idle).
func StatusDot(c *ui.Context, color theme.Hsla) ui.Element {
	return ui.Box(c).Size(6, 6).Radius(3).Background(color.Color())
}

// FormatCount gives compact row counts for the sidebar: `640`, `18.4k`,
// `1.2m`. Estimates, so three significant figures are plenty.
func FormatCount(count uint64) string {
	switch {
	case count <= 999:
		return strconv.FormatUint(count, 10)
	case count <= 999_999:
		return trimZero(float64(count)/1_000., "k")
	case count <= 999_999_999:
		return trimZero(float64(count)/1_000_000., "m")
	default:
		return trimZero(float64(count)/1_000_000_000., "b")
	}
}

func trimZero(value float64, suffix string) string {
	if value >= 100. || roundRust(fract(value)*10.) == 0. {
		return fmt.Sprintf("%d%s", uint64(roundRust(value)), suffix)
	}
	return fmt.Sprintf("%s%s", fixed1(value), suffix)
}

// FormatMillis gives query timings: `34 ms` up to a second, then `1.9 s`.
func FormatMillis(millis uint64) string {
	if millis < 1_000 {
		return fmt.Sprintf("%d ms", millis)
	}
	return fmt.Sprintf("%s s", fixed1(float64(millis)/1_000.))
}

// FormatMillisFrac gives a timing that came back as a fraction of a
// millisecond, which is what the server reports its own work as.
//
// Under ten milliseconds the fraction is most of the number — `0.4 ms` and
// `4.4 ms` both round to the same useless answer — so one decimal is kept
// there and dropped above it, where it says nothing.
func FormatMillisFrac(millis float64) string {
	if millis < 10. {
		return fmt.Sprintf("%s ms", fixed1(millis))
	}
	return FormatMillis(uint64(roundRust(millis)))
}

// FormatSeconds gives a clock that is still running: always seconds with
// one decimal, from `0.1 s` up. Unlike `FormatMillis` it never switches
// units, because a timer that changed shape as it passed a second would
// read as a glitch.
func FormatSeconds(millis uint64) string {
	return fmt.Sprintf("%s s", fixed1(float64(millis)/1_000.))
}

// Rust's numeric formatting and rounding, where Go's differ.

// roundRust is Rust's f64::round: half away from zero (Go's math.Round).
func roundRust(v float64) float64 { return math.Round(v) }

// fract is Rust's f64::fract: the part after the point, with its sign.
func fract(v float64) float64 { return v - math.Trunc(v) }

// fixed1 is Rust's `{:.1}`. Both Rust and Go print the exact decimal value
// of the double rounded half to even on that exact value, so Go's 'f'
// formatting is the same.
func fixed1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// MeerkatMark is the meerkat itself: two ears, a head, two eyes and a
// muzzle, drawn as plain rectangles so the app carries no image assets.
// The comp draws it at 42px; every part scales from that.
func MeerkatMark(c *ui.Context, size float32) ui.Element {
	colors := &theme.Current().Colors
	// Every offset below is the comp's 42px geometry, in units of the
	// requested size.
	unit := func(value float32) float32 { return value / 42. * size }
	ear := func(left bool) {
		ear := ui.Box(c).
			Absolute().
			Top(0).
			Size(unit(13.), unit(13.)).
			Radius(unit(13.) / 2).
			Background(colors.MarkEars.Color())
		if left {
			ear.Left(unit(2.))
		} else {
			ear.Right(unit(2.))
		}
	}
	eye := func(offset float32) {
		ui.Box(c).
			Absolute().
			Left(unit(offset)).
			Top(unit(18.)).
			Size(unit(5.), unit(5.)).
			Radius(unit(5.) / 2).
			Background(colors.MarkInk.Color())
	}

	return ui.Box(c).
		Shrink(0).
		Size(size, size).
		Children(func() {
			ear(true)
			ear(false)
			ui.Box(c).
				Absolute().
				Left(unit(4.)).
				Top(unit(7.)).
				Size(unit(34.), unit(34.)).
				Radius(unit(34.) / 2).
				Background(colors.MarkFace.Color())
			eye(12.)
			eye(25.)
			ui.Box(c).
				Absolute().
				Left(unit(18.)).
				Top(unit(27.)).
				Width(unit(7.)).
				Height(unit(5.)).
				Radius(unit(5.) / 2).
				Background(colors.MarkMuzzle.Color())
		})
}

// LockGlyph is the padlock on the session's read-only mark: a body with a
// shackle over it, drawn as rectangles like the meerkat, because the app
// carries no image assets. The comp draws it 8 wide by 9 tall beside a
// 10px label.
//
// The shackle is a bordered box whose bottom edge is covered by the body
// that overlaps it, which is why nothing here has to paint three sides of
// a border: GPUI's border widths come in whole pixels, and a 1px ring on
// a 5px box is the whole drawing.
func LockGlyph(c *ui.Context, color theme.Hsla) ui.Element {
	return ui.Box(c).
		Shrink(0).
		Width(8).
		Height(9).
		Children(func() {
			ui.Box(c).
				Absolute().
				Left(1.5).
				Top(0).
				Width(5).
				Height(5).
				Border(1, color.Color()).
				Radius(3, 3, 0, 0)
			ui.Box(c).
				Absolute().
				Left(0).
				Bottom(0).
				Width(8).
				Height(5.5).
				Radius(1.5).
				Background(color.Color())
		})
}

// PlayGlyph is the play triangle on the run button. GPUI's borders come in
// whole pixels on all four sides, so the CSS trick of a zero-sized box
// with one coloured border does not translate; this fills a real
// three-point path instead, which is also the only shape in the app that
// needs one.
func PlayGlyph(c *ui.Context, color theme.Hsla) ui.Element {
	return ui.Box(c).
		Width(8).
		Height(10).
		Shrink(0).
		Draw(func(p *ui.Painter, r ui.Rect) {
			var path ui.Path
			path.MoveTo(r.X, r.Y)
			path.LineTo(r.X, r.Y+r.H)
			path.LineTo(r.X+r.W, r.Y+r.H/2.)
			path.Close()
			p.FillPath(&path, color.Color())
		})
}

// SearchGlyph is the magnifier: on the "column" button, and in the search
// line the button opens.
//
// The lens is a bordered circle, the way every other glyph here is drawn
// from boxes. The handle cannot be: GPUI's borders come in whole pixels on
// all four sides and a box cannot be turned, so the one diagonal in the
// app is a filled path, as the play triangle is.
func SearchGlyph(c *ui.Context, color theme.Hsla) ui.Element {
	return ui.Box(c).
		Shrink(0).
		Width(11).
		Height(11).
		Children(func() {
			ui.Box(c).
				Absolute().
				Left(0).
				Top(0).
				Size(8, 8).
				Radius(4).
				Border(1, color.Color())
			ui.Box(c).
				Absolute().
				Left(6).
				Top(6).
				Children(func() { lensHandle(c, color) })
		})
}

// lensHandle is the magnifier's handle: a stroke from one corner of a small
// box to the other, as a four-point path, because a line has to have a
// width.
func lensHandle(c *ui.Context, color theme.Hsla) ui.Element {
	const size float32 = 5.
	const width float32 = 1.3
	return ui.Box(c).
		Width(size).
		Height(size).
		Shrink(0).
		Draw(func(p *ui.Painter, r ui.Rect) {
			var path ui.Path
			path.MoveTo(r.X+0., r.Y+width)
			path.LineTo(r.X+width, r.Y+0.)
			path.LineTo(r.X+size, r.Y+size-width)
			path.LineTo(r.X+size-width, r.Y+size)
			path.Close()
			p.FillPath(&path, color.Color())
		})
}

// StopGlyph is the stop square, which the run button wears while a
// statement is out.
func StopGlyph(c *ui.Context, color theme.Hsla) ui.Element {
	return ui.Box(c).Size(9, 9).Shrink(0).Radius(1.5).Background(color.Color())
}

// Switch is the comp's switch: a track with the knob at whichever end the
// state is. `on` fills the track with the accent, `off` leaves it the
// colour of a strong border — the same reading as a checkbox, in the space
// of a word.
func Switch(c *ui.Context, on bool) ui.Element {
	colors := &theme.Current().Colors
	fill := colors.BorderStrong
	if on {
		fill = colors.AccentFill
	}
	track := ui.Row(c).
		Width(30).
		Height(17).
		Shrink(0).
		AlignItems(ui.Center).
		Radius(17. / 2).
		Padding(2).
		Background(fill.Color())
	if on {
		track.Justify(ui.End)
	}
	return track.Children(func() {
		knob := ui.Box(c).
			Size(13, 13).
			Radius(13. / 2).
			Background(colors.Raised.Color())
		WithShadows(knob, ControlShadow())
	})
}

// TableGlyph is a tiny square glyph marking a table row in lists (accent
// when active).
func TableGlyph(c *ui.Context, active bool) ui.Element {
	colors := &theme.Current().Colors
	fill := colors.TextFaint
	if active {
		fill = colors.Accent
	}
	return ui.Box(c).Size(5, 5).Background(fill.Color())
}
