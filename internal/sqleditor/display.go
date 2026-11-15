package sqleditor

// What the buffer looks like on screen, where that differs from its bytes.
//
// Two things differ. A **tab** is one byte and several columns: the text
// system shapes it as a glyph of no useful width, so a pasted script
// indented with tabs painted its caret somewhere the text was not. The
// buffer keeps the tab — rewriting it would change a string literal that
// holds one — and the line is shaped from an expanded copy, with the two
// sets of offsets mapped through [Expanded].
//
// And a buffer is **mostly off screen**: a pasted script of a thousand
// lines used to be shaped whole and given a gutter row per line on every
// frame, which is where the lag after a paste came from. [visibleRows]
// is the window both the gutter and the text paint.
//
// Go port: Rust's `Cow<'_, str>` is a plain string. A borrowed one is the
// very string passed in (same bytes, no copy), which the tests check by
// its data pointer.

import (
	"math"
	"strings"
	"unicode/utf8"
)

// TabSize is the columns between tab stops. (Rust: `TAB_SIZE`.)
const TabSize = 4

// overscan is the rows shaped past each edge of the pane, so a scroll
// gesture that lands between two frames has text to show and a drag past
// the edge has a row to reach for. (Rust: `OVERSCAN`.)
const overscan = 4

// tabStop is one tab of a line: its byte index, and how many columns it
// takes.
type tabStop struct {
	at, width int
}

// Expanded is one line with its tabs expanded to spaces, and the way back.
type Expanded struct {
	// Byte index of each tab in the line, and how many columns it takes.
	tabs []tabStop
}

// NewExpanded is the text to shape for `line`, and the map between its
// offsets and the line's own. A line without a tab is borrowed as it is.
// (Rust: `Expanded::new`.)
func NewExpanded(line string) (string, Expanded) {
	if !strings.Contains(line, "\t") {
		return line, Expanded{}
	}
	var text strings.Builder
	text.Grow(len(line) + TabSize)
	var tabs []tabStop
	column := 0
	for ix, ch := range line {
		if ch == '\t' {
			width := TabSize - column%TabSize
			tabs = append(tabs, tabStop{ix, width})
			text.WriteString(strings.Repeat(" ", width))
			column += width
		} else {
			text.WriteRune(ch)
			column++
		}
	}
	return text.String(), Expanded{tabs: tabs}
}

// ToDisplay is a byte offset in the line, as an offset in the shaped text.
func (e Expanded) ToDisplay(ix int) int {
	sum := 0
	for _, tab := range e.tabs {
		if !(tab.at < ix) {
			break
		}
		sum += tab.width - 1
	}
	return ix + sum
}

// ToBuffer is an offset in the shaped text, as a byte offset in the line.
// A point inside a tab's run of spaces snaps to whichever side of the tab
// is nearer, as a click on one does in any editor.
func (e Expanded) ToBuffer(display int) int {
	shift := 0
	for _, tab := range e.tabs {
		start := tab.at + shift
		if display <= start {
			break
		}
		if display < start+tab.width {
			if display-start < start+tab.width-display {
				return tab.at
			}
			return tab.at + 1
		}
		shift += tab.width - 1
	}
	return display - shift
}

// columnOf is how many columns `line` takes up to byte `ix`, tabs
// expanded.
func columnOf(line string, ix int) int {
	column := 0
	for at, ch := range line {
		if at >= ix {
			break
		}
		if ch == '\t' {
			column += TabSize - column%TabSize
		} else {
			column++
		}
	}
	return column
}

// indexAtColumn is the byte in `line` nearest to `column`, tabs expanded:
// the way ↑ and ↓ keep the caret under where it was.
func indexAtColumn(line string, column int) int {
	atColumn := 0
	for ix, ch := range line {
		width := 1
		if ch == '\t' {
			width = TabSize - atColumn%TabSize
		}
		if atColumn+width > column {
			// Past the middle of a tab belongs to its far side.
			if column-atColumn > width/2 {
				return ix + utf8.RuneLen(ch)
			}
			return ix
		}
		atColumn += width
	}
	return len(line)
}

// widestLine is the widest line of `text`, in columns.
func widestLine(text string) int {
	widest := 0
	for line := range strings.SplitSeq(text, "\n") {
		widest = max(widest, columnOf(line, len(line)))
	}
	return widest
}

// visibleRows is the rows a pane `viewport` tall shows when scrolled
// `scrollTop` into content whose first row sits `padding` down, with a few
// to spare. A pane that has not been measured yet shows the first
// screenful.
func visibleRows(scrollTop, viewport, padding, lineHeight float32, count int) Range {
	if viewport <= 0. {
		return Range{0, min(count, 120)}
	}
	first := toUsize(float32(math.Floor(float64((scrollTop - padding) / lineHeight))))
	last := toUsize(float32(math.Ceil(float64((scrollTop + viewport - padding) / lineHeight))))
	first = min(max(first-overscan, 0), count)
	return Range{first, max(min(last+overscan, count), first)}
}

// toUsize is Rust's `.max(0.) as usize` on an f32: negative and NaN go to
// zero, the rest truncate.
func toUsize(v float32) int {
	if !(v > 0) {
		return 0
	}
	return int(v)
}

// normalize makes text from outside the buffer's own: every line break is
// `\n`. A script copied out of a Windows tool carries `\r\n`, and the `\r`
// would sit in the buffer as a character nothing paints and the caret
// steps over.
func normalize(text string) string {
	if !strings.Contains(text, "\r") {
		return text
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}
