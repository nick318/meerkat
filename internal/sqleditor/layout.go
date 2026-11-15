package sqleditor

// What the editor keeps of a line between frames, and how it places the
// characters of one that is too long to shape whole.
//
// **A frame shapes and walks what is on screen, across as well as
// down.** `display::visible_rows` already kept the rows off screen out of
// a frame; a single line can be the problem on its own. A pasted dump is
// one `INSERT` of two hundred kilobytes, and the first port shaped all of
// it whenever it changed (ten milliseconds and seventy megabytes, on every
// keystroke into it), then walked every glyph once for each of its forty
// thousand colour runs on every frame. So:
//
//   - A line is kept between frames by its text, in a [lineCache]: its
//     token spans and its shape, so a line that did not change is neither
//     tokenized nor shaped again.
//   - A **long line on the mono grid** — every character one fixed
//     advance wide, which JetBrains Mono gives every character it has —
//     is never shaped whole. Its x positions are columns times the
//     advance ([gridLine]), so a caret, a selection or a click anywhere
//     on it costs arithmetic, and only the columns inside the clip, with
//     a margin, are shaped and painted.
//   - Any other line is shaped whole, as before — a wide character or a
//     fallback font moves it off the grid, and only shaping knows where —
//     but painting still starts at the first glyph inside the clip: the
//     glyphs ascend in x, so the window is a binary search.
//
// The tab expansion is unchanged: the grid counts columns in the
// expanded text that `display::Expanded` makes, as the shaping does.

import (
	"math"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
)

// gridMinBytes is how long an expanded line has to be before it is placed
// by the grid rather than shaped whole. Below it, shaping the line costs
// less than the bookkeeping, and the shaped positions are exact for any
// font; above it, the line is longer than any pane is wide.
const gridMinBytes = 512

// gridStride is how many characters apart a non-ASCII grid line keeps a
// byte offset, so a column is a binary search and a short walk rather
// than a walk from the start of the line.
const gridStride = 64

// windowAlign is the column a grid line's shaped window starts and ends
// on a multiple of, so a scroll of a few pixels paints the window it
// already shaped. windowMargin is the columns shaped past either edge of
// the clip, so a ligature or an overhang at the edge still draws whole.
const (
	windowAlign  = 64
	windowMargin = 4
)

// glyphPad is how far past the clip a glyph may start and still be
// painted: a glyph's ink can reach past its advance.
const glyphPad = 2 * fontSize

// shapedLine is GPUI's `ShapedLine` for one font: the glyphs and the byte
// offset each starts at. (The same port as `uikit`'s, which keeps it
// private to the text field.)
type shapedLine struct {
	text   string
	font   ui.Font
	glyphs []ui.Glyph
	// index is the byte offset of each glyph's cluster (GPUI's
	// `glyph.index`).
	index []int
	width float32
	// ascending: `index` never goes down, as in a line with no
	// right-to-left run, so a byte's glyph is a binary search.
	ascending bool
	// xAscending: the glyphs' x never goes down — what mygo promises —
	// so the glyph under a point is a binary search.
	xAscending bool
}

func shapeLine(text string, font ui.Font) *shapedLine {
	l := &shapedLine{text: text, font: font, ascending: true, xAscending: true}
	if text == "" {
		return l
	}
	l.glyphs = ui.Shape(text, font)
	l.index = make([]int, len(l.glyphs))
	// The glyphs count runes and the index counts bytes. An ASCII line
	// is the same count twice, and needs no table between them.
	ascii := isASCII(text)
	var byteOf []int
	if !ascii {
		byteOf = make([]int, 0, len(text)+1)
		for ix := range text {
			byteOf = append(byteOf, ix)
		}
		byteOf = append(byteOf, len(text))
	}
	for i, g := range l.glyphs {
		if ascii {
			l.index[i] = min(g.Cluster, len(text))
		} else {
			l.index[i] = byteOf[min(g.Cluster, len(byteOf)-1)]
		}
		l.width = max(l.width, g.X+g.Advance)
		if i > 0 {
			if l.index[i] < l.index[i-1] {
				l.ascending = false
			}
			if g.X < l.glyphs[i-1].X {
				l.xAscending = false
			}
		}
	}
	return l
}

// shiftedBy moves the line's byte offsets `by` along: a window shaped out
// of a longer line, indexed in that line's offsets.
func (l *shapedLine) shiftedBy(by int) *shapedLine {
	for i := range l.index {
		l.index[i] += by
	}
	return l
}

func (l *shapedLine) len() int { return len(l.text) }

// xForIndex is GPUI's `x_for_index`: the x of the first glyph at or past
// the byte offset, else the line's width.
func (l *shapedLine) xForIndex(index int) float32 {
	if l.ascending {
		i := sort.SearchInts(l.index, index)
		if i < len(l.glyphs) {
			return l.glyphs[i].X
		}
		return l.width
	}
	for i, g := range l.glyphs {
		if l.index[i] >= index {
			return g.X
		}
	}
	return l.width
}

// closestIndexForX is GPUI's `closest_index_for_x`: the character boundary
// closest to x.
func (l *shapedLine) closestIndexForX(x float32) int {
	if l.xAscending {
		i := sort.Search(len(l.glyphs), func(i int) bool { return l.glyphs[i].X >= x })
		if i < len(l.glyphs) {
			prevIndex := 0
			var prevX float32
			if i > 0 {
				prevIndex, prevX = l.index[i-1], l.glyphs[i-1].X
			}
			if l.glyphs[i].X-x < x-prevX {
				return l.index[i]
			}
			return prevIndex
		}
		return l.pastTheEnd(x)
	}
	prevIndex := 0
	var prevX float32
	for i, g := range l.glyphs {
		if g.X >= x {
			if g.X-x < x-prevX {
				return l.index[i]
			}
			return prevIndex
		}
		prevIndex = l.index[i]
		prevX = g.X
	}
	return l.pastTheEnd(x)
}

// pastTheEnd is `closest_index_for_x` for a point past the last glyph's
// left edge.
func (l *shapedLine) pastTheEnd(x float32) int {
	if l.len() == 1 {
		if x > l.width/2. {
			return 1
		}
		return 0
	}
	return l.len()
}

// visible is the glyphs whose left edge lies in `from..=to`, as a range
// of indices: what a paint clipped to that stretch has to walk.
func (l *shapedLine) visible(from, to float32) (int, int) {
	if !l.xAscending {
		return 0, len(l.glyphs)
	}
	g0 := sort.Search(len(l.glyphs), func(i int) bool { return l.glyphs[i].X >= from })
	g1 := sort.Search(len(l.glyphs), func(i int) bool { return l.glyphs[i].X > to })
	return g0, max(g1, g0)
}

// gridLine places the characters of one expanded line at a fixed advance
// apiece: column `n` starts at `n × advance`.
type gridLine struct {
	advance float32
	// ascii: every byte is a character, so a column is its byte offset.
	ascii bool
	// columns is the line's width in characters.
	columns int
	// marks holds the byte offset of every `gridStride`th character, for a
	// line that is not ASCII.
	marks []int
	n     int
}

func newGridLine(text string, advance float32, ascii bool) *gridLine {
	g := &gridLine{advance: advance, ascii: ascii, n: len(text)}
	if ascii {
		g.columns = len(text)
		return g
	}
	for ix := range text {
		if g.columns%gridStride == 0 {
			g.marks = append(g.marks, ix)
		}
		g.columns++
	}
	return g
}

// column is how many characters of `text` start before byte `b`: the
// column of the first character at or past it.
func (g *gridLine) column(text string, b int) int {
	b = min(max(b, 0), g.n)
	if g.ascii {
		return b
	}
	k := sort.Search(len(g.marks), func(i int) bool { return g.marks[i] >= b }) - 1
	if k < 0 {
		return 0
	}
	col := k * gridStride
	for ix := g.marks[k]; ix < b; ix++ {
		if utf8.RuneStart(text[ix]) {
			col++
		}
	}
	return col
}

// byteAt is the byte character `col` starts at, or the line's length past
// its last.
func (g *gridLine) byteAt(text string, col int) int {
	if col <= 0 {
		return 0
	}
	if col >= g.columns {
		return g.n
	}
	if g.ascii {
		return col
	}
	ix := g.marks[col/gridStride]
	for left := col % gridStride; left > 0; left-- {
		_, size := utf8.DecodeRuneInString(text[ix:])
		ix += size
	}
	return ix
}

// xForIndex is `shapedLine.xForIndex` on the grid: the x of the first
// character at or past byte `b`, else the line's width.
func (g *gridLine) xForIndex(text string, b int) float32 {
	return float32(g.column(text, b)) * g.advance
}

// closestIndexForX is `shapedLine.closestIndexForX` on the grid, with the
// same rule at both ends: the nearer of the two character edges around
// `x`, the start of the line before it, and the end of the line past the
// last character's left edge.
func (g *gridLine) closestIndexForX(text string, x float32) int {
	// The first character whose left edge is at or past x.
	i := 0
	if x > 0 {
		i = int(math.Ceil(float64(x / g.advance)))
		for i > 0 && float32(i-1)*g.advance >= x {
			i--
		}
		for i < g.columns && float32(i)*g.advance < x {
			i++
		}
	}
	if i >= g.columns {
		if g.n == 1 {
			if x > float32(g.columns)*g.advance/2. {
				return 1
			}
			return 0
		}
		return g.n
	}
	prevIndex := 0
	var prevX float32
	if i > 0 {
		prevIndex, prevX = g.byteAt(text, i-1), float32(i-1)*g.advance
	}
	if float32(i)*g.advance-x < x-prevX {
		return g.byteAt(text, i)
	}
	return prevIndex
}

// lineCache is what is kept of one expanded line from one frame to the
// next, keyed by its text.
type lineCache struct {
	text string
	font ui.Font
	// The line's token spans, for `vocabulary`.
	spans      []span
	vocabulary *Vocabulary
	spansValid bool
	// grid, for a long line every character of which is on the grid;
	// nil for a line that is shaped whole.
	grid *gridLine
	// The whole line shaped, for a line off the grid; made when first
	// asked for.
	shaped *shapedLine
	// The window of a grid line last shaped, and the bytes it covers.
	window               *shapedLine
	windowFrom, windowTo int
	windowFromColumn     int
}

// newLineCache works out how a line is placed: on the grid when it is
// long and the font and every character in it allow it.
func newLineCache(text string, font ui.Font) *lineCache {
	lc := &lineCache{text: text, font: font}
	if len(text) < gridMinBytes {
		return lc
	}
	advance, mono := fontGrid(font)
	if !mono {
		return lc
	}
	ascii := true
	for i := 0; i < len(text); i++ {
		if b := text[i]; b < 0x20 || b > 0x7e {
			ascii = false
			break
		}
	}
	if !ascii {
		for _, r := range text {
			if !(r >= 0x20 && r <= 0x7e) && !onGrid(font, r) {
				return lc
			}
		}
	}
	lc.grid = newGridLine(text, advance, ascii)
	return lc
}

// spansFor is the line's token spans with `vocabulary`'s names.
func (lc *lineCache) spansFor(vocabulary *Vocabulary) []span {
	if !lc.spansValid || lc.vocabulary != vocabulary {
		lc.spans = spans(lc.text, vocabulary)
		lc.vocabulary = vocabulary
		lc.spansValid = true
	}
	return lc.spans
}

// whole is the line shaped whole.
func (lc *lineCache) whole() *shapedLine {
	if lc.shaped == nil {
		lc.shaped = shapeLine(lc.text, lc.font)
	}
	return lc.shaped
}

// xForIndex is where byte `b` of the expanded line sits from its left
// edge, by the grid or by the shape.
func (lc *lineCache) xForIndex(b int) float32 {
	if lc.grid != nil {
		return lc.grid.xForIndex(lc.text, b)
	}
	return lc.whole().xForIndex(b)
}

// windowOf is the shaped window of a grid line that covers columns
// `from..to`, widened to the alignment, and the column it starts at.
func (lc *lineCache) windowOf(from, to int) (*shapedLine, int) {
	g := lc.grid
	from = max(from-windowMargin, 0) / windowAlign * windowAlign
	to = min((to+windowMargin+windowAlign-1)/windowAlign*windowAlign, g.columns)
	if to <= from {
		return &shapedLine{font: lc.font, ascending: true, xAscending: true}, from
	}
	b0, b1 := g.byteAt(lc.text, from), g.byteAt(lc.text, to)
	if lc.window == nil || lc.windowFrom != b0 || lc.windowTo != b1 {
		lc.window = shapeLine(lc.text[b0:b1], lc.font).shiftedBy(b0)
		lc.windowFrom, lc.windowTo, lc.windowFromColumn = b0, b1, from
	}
	return lc.window, lc.windowFromColumn
}

// isASCII reports whether every byte of `text` is below 0x80.
func isASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// The grid the editor's font draws on: its advance, whether it has one,
// and which characters outside printable ASCII keep to it. Asked of the
// text system once per font and character, not per frame.
var fontGrids struct {
	sync.Mutex
	font    ui.Font
	known   bool
	advance float32
	mono    bool
	runes   map[rune]bool
}

// gridProbe is what a font has to draw at one advance to count as mono.
const gridProbe = "iWM.l_"

// fontGrid is `font`'s advance, and whether its printable ASCII all draws
// at it. A font that fell back to a proportional face has no grid, and
// every line is shaped whole.
func fontGrid(font ui.Font) (float32, bool) {
	fontGrids.Lock()
	defer fontGrids.Unlock()
	return fontGridLocked(font)
}

func fontGridLocked(font ui.Font) (float32, bool) {
	if fontGrids.known && fontGrids.font == font {
		return fontGrids.advance, fontGrids.mono
	}
	fontGrids.font, fontGrids.known = font, true
	fontGrids.runes = map[rune]bool{}
	fontGrids.advance, fontGrids.mono = charWidth, false
	glyphs := ui.Shape("M", font)
	if len(glyphs) == 0 {
		return fontGrids.advance, false
	}
	advance := glyphs[0].Advance
	fontGrids.advance = advance
	probe := ui.Shape(gridProbe, font)
	mono := len(probe) == len(gridProbe)
	for _, g := range probe {
		if !sameAdvance(g.Advance, advance) {
			mono = false
		}
	}
	fontGrids.mono = mono
	return advance, mono
}

// onGrid reports whether `font` draws `r` alone, one glyph one advance
// wide. A combining mark, a wide character and anything from a fallback
// face do not.
func onGrid(font ui.Font, r rune) bool {
	fontGrids.Lock()
	defer fontGrids.Unlock()
	advance, mono := fontGridLocked(font)
	if !mono {
		return false
	}
	if ok, seen := fontGrids.runes[r]; seen {
		return ok
	}
	glyphs := ui.Shape(string(r), font)
	ok := len(glyphs) == 1 && glyphs[0].Runes <= 1 && sameAdvance(glyphs[0].Advance, advance)
	fontGrids.runes[r] = ok
	return ok
}

func sameAdvance(a, b float32) bool {
	return math.Abs(float64(a-b)) < 0.01
}
