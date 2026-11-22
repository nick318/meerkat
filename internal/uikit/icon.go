package uikit

// Line icons, drawn as stroked paths.
//
// The app carries no image assets, so every glyph is geometry. The older
// marks here — the padlock, the play triangle, the magnifier — are boxes
// and filled paths, because they predate GPUI's `PathBuilder::stroke`.
// Those were one-offs; the sidebar, the tab strip and the rail need a
// family, and a family has to share a pen. So every icon is drawn in a
// 14-unit box with one stroke width, scaled to the size asked for, and a
// 12px icon beside a 16px one reads as the same hand.
//
// Go port: mygo's `ui.Path` has lines and curves but no SVG arc command,
// while `ui.ParsePath` reads SVG path data with arcs. So each icon is
// written as the path data GPUI's `PathBuilder` calls describe — the same
// points and the same `arc_to(radii, rotation, large_arc, sweep, to)`
// arguments, which are SVG's `A` — at the asked scale, parsed once and
// kept. GPUI strokes with lyon's defaults, flat ends and mitred corners,
// where mygo's strokes are round; the pen sets them back.

import (
	"fmt"
	"strings"
	"sync"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/theme"
)

// iconGrid is the box every icon is drawn in, in its own units. (Rust:
// `GRID`.)
const iconGrid float32 = 14.

// iconStroke is the pen, in grid units. At 14px that is 1.3px, the stroke
// the comp's icons are drawn with. (Rust: `STROKE`.)
const iconStroke float32 = 1.3

// Icon names a line icon.
type Icon int

const (
	// IconTable is a table: a frame with a header rule and a first column.
	IconTable Icon = iota
	// IconView is a view: the same frame, with its body left open.
	IconView
	// IconQuery is a query tab: a prompt and a cursor line.
	IconQuery
	// IconHistory is the history tab and the history button: a clock face.
	IconHistory
	// IconSchema is a schema: three stacked sheets.
	IconSchema
	IconChevronRight
	IconChevronDown
	IconPlus
	// IconKey is a primary key column.
	IconKey
	// IconCheck is a statement that landed.
	IconCheck
	// IconDatabase is a saved connection.
	IconDatabase
	// IconSearch is a search line.
	IconSearch
	// IconLock is a column that does not take an edit.
	IconLock
	// IconPencil is a result that takes an edit.
	IconPencil
)

// DrawIcon is an icon at `size` pixels square, stroked in `color`. (Rust:
// `icon(kind, size, color)`; Go cannot have a function named like the
// type `Icon`, so the function that builds the element is `DrawIcon`.)
func DrawIcon(c *ui.Context, kind Icon, size float32, color theme.Hsla) ui.Element {
	return ui.Box(c).
		Size(size, size).
		Shrink(0).
		Draw(func(p *ui.Painter, r ui.Rect) { paintIcon(kind, r, color, p) })
}

type iconKey struct {
	kind  Icon
	scale float32
}

// iconPaths keeps the parsed outlines, by icon and scale, in the icon's own
// box from (0, 0). Draw may run on any Tester's goroutine, hence the
// sync.Map.
var iconPaths sync.Map

func paintIcon(kind Icon, bounds ui.Rect, color theme.Hsla, p *ui.Painter) {
	scale := bounds.W / iconGrid
	key := iconKey{kind, scale}
	cached, ok := iconPaths.Load(key)
	if !ok {
		path := ui.MustParsePath(iconData(kind, scale))
		path.Cap(ui.ButtCap).Join(ui.MiterJoin).MiterLimit(4)
		cached, _ = iconPaths.LoadOrStore(key, path)
	}
	path := cached.(*ui.Path)
	p.Transform(ui.Translation(bounds.X, bounds.Y), 0, 0, func() {
		p.StrokePath(path, iconStroke*scale, color.Color())
	})
}

// pen writes the PathBuilder calls as SVG path data, scaled.
type pen struct {
	b     strings.Builder
	scale float32
}

func (w *pen) at(x, y float32) string {
	return fmt.Sprintf("%g %g", x*w.scale, y*w.scale)
}

func (w *pen) moveTo(x, y float32) { fmt.Fprintf(&w.b, "M%s ", w.at(x, y)) }
func (w *pen) lineTo(x, y float32) { fmt.Fprintf(&w.b, "L%s ", w.at(x, y)) }
func (w *pen) close()              { w.b.WriteString("Z ") }

// arcTo is `PathBuilder::arc_to(radii, x_rotation, large_arc, sweep, to)`,
// whose arguments are SVG's elliptical arc.
func (w *pen) arcTo(rx, ry, rotation float32, largeArc, sweep bool, x, y float32) {
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	fmt.Fprintf(&w.b, "A%g %g %g %d %d %s ", rx*w.scale, ry*w.scale, rotation, flag(largeArc), flag(sweep), w.at(x, y))
}

// iconData is the Rust `paint` body, written as path data.
func iconData(kind Icon, scale float32) string {
	w := &pen{scale: scale}
	// A closed circle as two half arcs: one arc cannot end where it began.
	circle := func(cx, cy, r float32) {
		w.moveTo(cx-r, cy)
		w.arcTo(r, r, 0, false, true, cx+r, cy)
		w.arcTo(r, r, 0, false, true, cx-r, cy)
		w.close()
	}
	rect := func(x, y, wd, h float32) {
		w.moveTo(x, y)
		w.lineTo(x+wd, y)
		w.lineTo(x+wd, y+h)
		w.lineTo(x, y+h)
		w.close()
	}
	switch kind {
	case IconTable:
		rect(1.5, 2., 11., 10.)
		w.moveTo(1.5, 5.5)
		w.lineTo(12.5, 5.5)
		w.moveTo(5.5, 5.5)
		w.lineTo(5.5, 12.)
	case IconView:
		rect(1.5, 2., 11., 10.)
		w.moveTo(1.5, 5.5)
		w.lineTo(12.5, 5.5)
		w.moveTo(4.5, 8.8)
		w.lineTo(9.5, 8.8)
	case IconQuery:
		w.moveTo(2.5, 4.)
		w.lineTo(5.5, 7.)
		w.lineTo(2.5, 10.)
		w.moveTo(7.5, 10.5)
		w.lineTo(11.5, 10.5)
	case IconHistory:
		circle(7., 7., 5.2)
		w.moveTo(7., 4.2)
		w.lineTo(7., 7.)
		w.lineTo(9., 8.4)
	case IconSchema:
		w.moveTo(7., 1.8)
		w.lineTo(12.3, 4.4)
		w.lineTo(7., 7.)
		w.lineTo(1.7, 4.4)
		w.close()
		w.moveTo(1.7, 7.)
		w.lineTo(7., 9.6)
		w.lineTo(12.3, 7.)
		w.moveTo(1.7, 9.6)
		w.lineTo(7., 12.2)
		w.lineTo(12.3, 9.6)
	case IconChevronRight:
		w.moveTo(5.3, 3.5)
		w.lineTo(8.8, 7.)
		w.lineTo(5.3, 10.5)
	case IconChevronDown:
		w.moveTo(3.5, 5.3)
		w.lineTo(7., 8.8)
		w.lineTo(10.5, 5.3)
	case IconPlus:
		w.moveTo(7., 2.5)
		w.lineTo(7., 11.5)
		w.moveTo(2.5, 7.)
		w.lineTo(11.5, 7.)
	case IconKey:
		circle(4.6, 7., 2.6)
		w.moveTo(7.2, 7.)
		w.lineTo(12.5, 7.)
		w.moveTo(10.8, 7.)
		w.lineTo(10.8, 9.4)
	case IconCheck:
		w.moveTo(3., 7.3)
		w.lineTo(5.8, 10.)
		w.lineTo(11., 4.3)
	case IconDatabase:
		w.moveTo(2., 3.8)
		w.arcTo(5., 1.9, 0, false, true, 12., 3.8)
		w.arcTo(5., 1.9, 0, false, true, 2., 3.8)
		w.close()
		w.moveTo(2., 3.8)
		w.lineTo(2., 10.2)
		w.arcTo(5., 1.9, 0, false, false, 12., 10.2)
		w.lineTo(12., 3.8)
		w.moveTo(2., 7.)
		w.arcTo(5., 1.9, 0, false, false, 12., 7.)
	case IconSearch:
		circle(6., 6., 4.)
		w.moveTo(9., 9.)
		w.lineTo(12.3, 12.3)
	case IconLock:
		rect(2.5, 6.5, 9., 6.)
		w.moveTo(4.5, 6.5)
		w.lineTo(4.5, 4.5)
		w.arcTo(2.5, 2.5, 0, false, true, 9.5, 4.5)
		w.lineTo(9.5, 6.5)
	case IconPencil:
		w.moveTo(9.5, 2.5)
		w.lineTo(11.5, 4.5)
		w.lineTo(5., 11.)
		w.lineTo(3., 11.)
		w.lineTo(3., 9.)
		w.close()
	}
	return w.b.String()
}
