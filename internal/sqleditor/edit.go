package sqleditor

// Edits that work on whole lines: indent, comment, move, duplicate, join.
//
// Each one is a plain function over the text and the selection, answering
// with the [Edit] to make — the range to replace, what to put there, and
// where the selection ends up — so the rules are argued with in a test
// rather than in a running window. The editor applies an `Edit` as one
// undo step and decides nothing else about it.
//
// The keys are Zed's, because that is the editor these habits come from:
// ⇥ and ⇧⇥ over a selection, ⌘/, ⌥↑ and ⌥↓, ⇧⌥↑ and ⇧⌥↓, ⇧⌘K, ⌃J, ⌃T.
//
// Go port: Rust's `Option<Edit>` is `*Edit`, nil for `None`.

import (
	"strings"
)

// Edit is one replacement, and the selection it leaves.
type Edit struct {
	Range     Range
	Text      string
	Selection Range
}

// comment is SQL's line comment, with the space every formatter puts
// after it. (Rust: `COMMENT`.)
const comment = "-- "

// lineSpan is the lines a selection touches, from the first one's start to
// the last one's end. A selection that stops at the very start of a line
// does not touch that line: ⇧↓ from a line's start selects one line, not
// two.
func lineSpan(text string, selection Range) Range {
	start := lineStart(text, selection.Start)
	last := selection.End
	if last > selection.Start && strings.HasSuffix(text[:last], "\n") {
		last--
	}
	return Range{start, lineEnd(text, last)}
}

// replacement is one `(Range<usize>, String)` of a patch.
type replacement struct {
	r    Range
	text string
}

// patch is replacements in the buffer's own offsets, in order and never
// touching. (Rust: `struct Patch(Vec<(Range<usize>, String)>)`.)
type patch []replacement

// mapOffset is where `offset` lands once the patch is applied. A point at
// an insertion moves past it, so a caret at a line's start stays with the
// text when that line is indented — unless it `sticks`, which is the
// start of a selection: the marker put in front of a selected line
// belongs inside the selection. (Rust: `Patch::map`.)
func (p patch) mapOffset(offset int, sticks bool) int {
	delta := 0
	for _, rep := range p {
		atInsertion := rep.r.IsEmpty() && offset == rep.r.Start
		if offset >= rep.r.End && !(sticks && atInsertion) {
			delta += len(rep.text) - (rep.r.End - rep.r.Start)
		} else if offset > rep.r.Start {
			return rep.r.Start + delta + len(rep.text)
		} else {
			break
		}
	}
	return offset + delta
}

func (p patch) intoEdit(text string, selection Range) *Edit {
	if len(p) == 0 {
		return nil
	}
	first := p[0].r.Start
	last := p[len(p)-1].r.End
	sel := Range{p.mapOffset(selection.Start, !selection.IsEmpty()), p.mapOffset(selection.End, false)}
	var replaced strings.Builder
	at := first
	for _, rep := range p {
		replaced.WriteString(text[at:rep.r.Start])
		replaced.WriteString(rep.text)
		at = rep.r.End
	}
	return &Edit{
		Range:     Range{first, last},
		Text:      replaced.String(),
		Selection: sel,
	}
}

// lineStartsIn is the start offset of every line in `span`.
func lineStartsIn(text string, span Range) []int {
	starts := []int{span.Start}
	inner := text[span.Start:span.End]
	for ix := 0; ix < len(inner); ix++ {
		if inner[ix] == '\n' {
			starts = append(starts, span.Start+ix+1)
		}
	}
	return starts
}

// indent is ⇥ over a selection, and ⌘]: one `unit` in front of every line
// the selection touches. A blank line is left blank.
func indent(text string, selection Range, unit string) *Edit {
	span := lineSpan(text, selection)
	var p patch
	for _, start := range lineStartsIn(text, span) {
		if isBlank(text[start:lineEnd(text, start)]) {
			continue
		}
		p = append(p, replacement{Range{start, start}, unit})
	}
	return p.intoEdit(text, selection)
}

// outdent is ⇧⇥ and ⌘[: one `unit` of leading space off every line the
// selection touches, or one tab, or as much as the line has.
func outdent(text string, selection Range, unit string) *Edit {
	span := lineSpan(text, selection)
	var p patch
	for _, start := range lineStartsIn(text, span) {
		line := text[start:]
		taken := 0
		if strings.HasPrefix(line, "\t") {
			taken = 1
		} else {
			for taken < len(unit) && taken < len(line) && line[taken] == ' ' {
				taken++
			}
		}
		if taken > 0 {
			p = append(p, replacement{Range{start, start + taken}, ""})
		}
	}
	return p.intoEdit(text, selection)
}

// toggleComment is ⌘/: comment the lines the selection touches, or
// uncomment them when every one of them already is. The marker goes in at
// the shallowest indent among them, so a commented block keeps its shape.
func toggleComment(text string, selection Range) *Edit {
	span := lineSpan(text, selection)
	starts := lineStartsIn(text, span)
	type lineAt struct {
		start int
		line  string
	}
	lines := make([]lineAt, 0, len(starts))
	for _, start := range starts {
		lines = append(lines, lineAt{start, text[start:lineEnd(text, start)]})
	}
	indentOf := func(line string) int { return len(line) - len(strings.TrimLeft(line, " \t")) }
	var live []lineAt
	for _, l := range lines {
		if !isBlank(l.line) {
			live = append(live, l)
		}
	}

	commented := len(live) > 0
	for _, l := range live {
		if !strings.HasPrefix(l.line[indentOf(l.line):], "--") {
			commented = false
			break
		}
	}
	var p patch
	if commented {
		for _, l := range live {
			at := l.start + indentOf(l.line)
			marker := 2
			if strings.HasPrefix(text[at:], comment) {
				marker = len(comment)
			}
			p = append(p, replacement{Range{at, at + marker}, ""})
		}
	} else if len(live) == 0 {
		// Nothing but blank lines: the caret's own line takes the marker,
		// so ⌘/ on an empty line starts a comment.
		p = patch{{Range{selection.Start, selection.Start}, comment}}
	} else {
		depth := indentOf(live[0].line)
		for _, l := range live[1:] {
			depth = min(depth, indentOf(l.line))
		}
		for _, l := range live {
			p = append(p, replacement{Range{l.start + depth, l.start + depth}, comment})
		}
	}
	return p.intoEdit(text, selection)
}

// moveLines is ⌥↑ and ⌥↓: carry the selection's lines over the line beside
// them.
func moveLines(text string, selection Range, down bool) *Edit {
	span := lineSpan(text, selection)
	block := text[span.Start:span.End]
	if down {
		if span.End == len(text) {
			return nil
		}
		next := Range{span.End + 1, lineEnd(text, span.End+1)}
		shift := next.End - next.Start + 1
		return &Edit{
			Range:     Range{span.Start, next.End},
			Text:      text[next.Start:next.End] + "\n" + block,
			Selection: Range{selection.Start + shift, selection.End + shift},
		}
	}
	if span.Start == 0 {
		return nil
	}
	previous := Range{lineStart(text, span.Start-1), span.Start - 1}
	shift := previous.End - previous.Start + 1
	return &Edit{
		Range:     Range{previous.Start, span.End},
		Text:      block + "\n" + text[previous.Start:previous.End],
		Selection: Range{selection.Start - shift, selection.End - shift},
	}
}

// duplicateLines is ⇧⌥↑ and ⇧⌥↓: a second copy of the selection's lines.
// Down leaves the selection on the lower copy and up on the upper, which
// is where the user was looking either way.
func duplicateLines(text string, selection Range, down bool) *Edit {
	span := lineSpan(text, selection)
	copied := text[span.Start:span.End] + "\n"
	shift := 0
	if down {
		shift = len(copied)
	}
	return &Edit{
		Range:     Range{span.Start, span.Start},
		Text:      copied,
		Selection: Range{selection.Start + shift, selection.End + shift},
	}
}

// deleteLines is ⇧⌘K: the selection's lines, gone, with the caret kept in
// its column on the line that takes their place.
func deleteLines(text string, selection Range) *Edit {
	span := lineSpan(text, selection)
	column := selection.Start - span.Start
	var r Range
	var landing int
	if span.End < len(text) {
		// The line below moves up into the gap.
		below := lineEnd(text, span.End+1) - (span.End + 1)
		r, landing = Range{span.Start, span.End + 1}, span.Start+min(column, below)
	} else if span.Start > 0 {
		// The last line of the buffer: its break goes with it.
		above := lineStart(text, span.Start-1)
		width := span.Start - 1 - above
		r, landing = Range{span.Start - 1, span.End}, above+min(column, width)
	} else {
		r, landing = span, 0
	}
	if r.IsEmpty() {
		return nil
	}
	// The landing line is in the old text; keep it on a character.
	for {
		at := landing
		if landing >= r.Start {
			at = landing + (r.End - r.Start)
		}
		if isCharBoundary(text, at) {
			break
		}
		landing--
	}
	return &Edit{
		Range:     r,
		Text:      "",
		Selection: Range{landing, landing},
	}
}

// joinLines is ⌃J: pull the next line up onto this one, with one space
// where the break and the indent were. A selection over several lines
// joins all of them.
func joinLines(text string, selection Range) *Edit {
	span := lineSpan(text, selection)
	var breaks []int
	inner := text[span.Start:span.End]
	for ix := 0; ix < len(inner); ix++ {
		if inner[ix] == '\n' {
			breaks = append(breaks, span.Start+ix)
		}
	}
	if len(breaks) == 0 {
		if span.End == len(text) {
			return nil
		}
		breaks = append(breaks, span.End)
	}
	caret := selection.Start
	var p patch
	for _, at := range breaks {
		head := text[lineStart(text, at):at]
		from := at - (len(head) - len(strings.TrimRight(head, " \t")))
		tail := text[at+1 : lineEnd(text, at+1)]
		to := at + 1 + (len(tail) - len(strings.TrimLeft(tail, " \t")))
		// Nothing to keep apart when either side is empty.
		glue := " "
		if isBlank(head) || isBlank(tail) {
			glue = ""
		}
		caret = from
		p = append(p, replacement{Range{from, to}, glue})
	}
	// The caret lands on the last join, where the two lines now meet.
	landing := p.mapOffset(caret, true)
	edit := p.intoEdit(text, selection)
	if edit == nil {
		return nil
	}
	edit.Selection = Range{landing, landing}
	return edit
}

// transpose is ⌃T: swap the characters either side of the caret and step
// past them. At the end of a line it swaps the two before it, as macOS
// does.
func transpose(text string, caret int) *Edit {
	atLineEnd := caret == len(text) || strings.HasPrefix(text[caret:], "\n")
	var left, middle, right int
	if atLineEnd {
		middle = previousBoundary(text, caret)
		left, right = previousBoundary(text, middle), caret
	} else {
		left, middle, right = previousBoundary(text, caret), caret, nextBoundary(text, caret)
	}
	if left == middle || middle == right || strings.Contains(text[left:right], "\n") {
		return nil
	}
	return &Edit{
		Range:     Range{left, right},
		Text:      text[middle:right] + text[left:middle],
		Selection: Range{right, right},
	}
}
