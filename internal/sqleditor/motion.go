package sqleditor

// Cursor motion over a text buffer: character, word and line boundaries.
//
// All offsets are byte offsets into the buffer and always land on a
// character boundary. A newline stops word motion, the way Zed's editor
// does with `ignore_newlines: false` — in a query buffer, crossing a line
// break by accident is worse than one extra keypress.

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// isAlphanumeric is Rust's `char::is_alphanumeric`: Alphabetic or Numeric.
func isAlphanumeric(ch rune) bool {
	return unicode.In(ch, unicode.L, unicode.Nl, unicode.Other_Alphabetic, unicode.Nd, unicode.No)
}

// isUppercase is Rust's `char::is_uppercase`: the Uppercase property.
func isUppercase(ch rune) bool {
	return unicode.In(ch, unicode.Lu, unicode.Other_Uppercase)
}

// isLowercase is Rust's `char::is_lowercase`: the Lowercase property.
func isLowercase(ch rune) bool {
	return unicode.In(ch, unicode.Ll, unicode.Other_Lowercase)
}

// isWhitespace is Rust's `char::is_whitespace`: the White_Space property.
func isWhitespace(ch rune) bool {
	return unicode.Is(unicode.White_Space, ch)
}

// isWordChar is what counts as part of a word. `_` and `$` belong to SQL
// identifiers; `.` does not, so `public.users` is two words.
func isWordChar(ch rune) bool {
	return isAlphanumeric(ch) || ch == '_' || ch == '$'
}

func previousBoundary(text string, offset int) int {
	if offset <= 0 {
		return 0
	}
	_, size := utf8.DecodeLastRuneInString(text[:offset])
	return offset - size
}

func nextBoundary(text string, offset int) int {
	if offset >= len(text) {
		return len(text)
	}
	_, size := utf8.DecodeRuneInString(text[offset:])
	return offset + size
}

func charBefore(text string, offset int) (rune, bool) {
	if offset <= 0 {
		return 0, false
	}
	ch, _ := utf8.DecodeLastRuneInString(text[:offset])
	return ch, true
}

func charAt(text string, offset int) (rune, bool) {
	if offset >= len(text) {
		return 0, false
	}
	ch, _ := utf8.DecodeRuneInString(text[offset:])
	return ch, true
}

// previousWordStart is the start of the word before `offset`, as ⌥← gives
// on macOS.
func previousWordStart(text string, offset int) int {
	ix := offset
	if ix == 0 {
		return 0
	}
	// Sitting right after a line break: step over it and stop, so ⌥←
	// lands at the end of the line above rather than skipping a word.
	if ch, ok := charBefore(text, ix); ok && ch == '\n' {
		return previousBoundary(text, ix)
	}
	for {
		ch, ok := charBefore(text, ix)
		if !ok || ch == '\n' || isWordChar(ch) {
			break
		}
		ix = previousBoundary(text, ix)
	}
	for {
		ch, ok := charBefore(text, ix)
		if !ok || !isWordChar(ch) {
			break
		}
		ix = previousBoundary(text, ix)
	}
	return ix
}

// nextWordEnd is the end of the word after `offset`, as ⌥→ gives on
// macOS.
func nextWordEnd(text string, offset int) int {
	ix := offset
	if ix >= len(text) {
		return len(text)
	}
	if ch, ok := charAt(text, ix); ok && ch == '\n' {
		return nextBoundary(text, ix)
	}
	for {
		ch, ok := charAt(text, ix)
		if !ok || ch == '\n' || isWordChar(ch) {
			break
		}
		ix = nextBoundary(text, ix)
	}
	for {
		ch, ok := charAt(text, ix)
		if !ok || !isWordChar(ch) {
			break
		}
		ix = nextBoundary(text, ix)
	}
	return ix
}

// wordAt is the word around `offset`, for a double click. Falls back to
// the single character under the cursor when that character is not
// word-like.
func wordAt(text string, offset int) Range {
	at, atOK := charAt(text, offset)
	before, beforeOK := charBefore(text, offset)
	inside := (atOK && isWordChar(at)) || (beforeOK && isWordChar(before))
	if !inside {
		return Range{offset, min(nextBoundary(text, offset), len(text))}
	}
	start := offset
	for {
		ch, ok := charBefore(text, start)
		if !ok || !isWordChar(ch) {
			break
		}
		start = previousBoundary(text, start)
	}
	end := offset
	for {
		ch, ok := charAt(text, end)
		if !ok || !isWordChar(ch) {
			break
		}
		end = nextBoundary(text, end)
	}
	return Range{start, end}
}

func lineStart(text string, offset int) int {
	if ix := strings.LastIndexByte(text[:offset], '\n'); ix >= 0 {
		return ix + 1
	}
	return 0
}

func lineEnd(text string, offset int) int {
	if ix := strings.IndexByte(text[offset:], '\n'); ix >= 0 {
		return offset + ix
	}
	return len(text)
}

// indentEnd is where the line's own text starts: past the leading spaces
// and tabs.
func indentEnd(text string, offset int) int {
	start := lineStart(text, offset)
	n := 0
	for start+n < len(text) && (text[start+n] == ' ' || text[start+n] == '\t') {
		n++
	}
	return start + n
}

// smartLineStart is ⌘← and Home, the way Zed's `stop_at_indent` reads
// them: to the first character of the line's text, and from there to
// column zero. One key reaches both, and the first press lands where the
// typing starts.
func smartLineStart(text string, offset int) int {
	indent := indentEnd(text, offset)
	if offset == indent {
		return lineStart(text, offset)
	}
	return indent
}

// isSubwordChar: a subword ends where `_` or a step from lower to upper
// case cuts a name: `master_client` and `masterClient` are both two of
// them. SQL names are snake case, so this is the motion that walks one.
func isSubwordChar(ch rune) bool {
	return isWordChar(ch) && ch != '_'
}

// previousSubwordStart is the start of the subword before `offset`, as
// ⌃⌥← gives in Zed.
func previousSubwordStart(text string, offset int) int {
	ix := offset
	if ix == 0 {
		return 0
	}
	if ch, ok := charBefore(text, ix); ok && ch == '\n' {
		return previousBoundary(text, ix)
	}
	for {
		ch, ok := charBefore(text, ix)
		if !ok || ch == '\n' || isSubwordChar(ch) {
			break
		}
		ix = previousBoundary(text, ix)
	}
	var taken rune
	hasTaken := false
	for {
		ch, ok := charBefore(text, ix)
		if !ok || !isSubwordChar(ch) {
			break
		}
		// `fooBar`, walking left: `B` is taken, `o` is the word before.
		if hasTaken && isUppercase(taken) && isLowercase(ch) {
			break
		}
		ix = previousBoundary(text, ix)
		taken, hasTaken = ch, true
	}
	return ix
}

// nextSubwordEnd is the end of the subword after `offset`, as ⌃⌥→ gives
// in Zed.
func nextSubwordEnd(text string, offset int) int {
	ix := offset
	if ix >= len(text) {
		return len(text)
	}
	if ch, ok := charAt(text, ix); ok && ch == '\n' {
		return nextBoundary(text, ix)
	}
	for {
		ch, ok := charAt(text, ix)
		if !ok || ch == '\n' || isSubwordChar(ch) {
			break
		}
		ix = nextBoundary(text, ix)
	}
	var taken rune
	hasTaken := false
	for {
		ch, ok := charAt(text, ix)
		if !ok || !isSubwordChar(ch) {
			break
		}
		if hasTaken && isLowercase(taken) && isUppercase(ch) {
			break
		}
		ix = nextBoundary(text, ix)
		taken, hasTaken = ch, true
	}
	return ix
}

func isBlank(line string) bool {
	for _, ch := range line {
		if !isWhitespace(ch) {
			return false
		}
	}
	return true
}

// startOfParagraph is ⌃↑: the blank line that opens the paragraph the
// caret is in, or the top of the buffer. In a query buffer a paragraph is
// a statement.
func startOfParagraph(text string, offset int) int {
	start := lineStart(text, offset)
	seenText := false
	for {
		blank := isBlank(text[start:lineEnd(text, start)])
		if blank && seenText {
			return start
		}
		seenText = seenText || !blank
		if start == 0 {
			return 0
		}
		start = lineStart(text, start-1)
	}
}

// endOfParagraph is ⌃↓: the blank line that closes the paragraph, or the
// end of the buffer.
func endOfParagraph(text string, offset int) int {
	start := lineStart(text, offset)
	seenText := false
	for {
		end := lineEnd(text, start)
		blank := isBlank(text[start:end])
		if blank && seenText {
			return start
		}
		seenText = seenText || !blank
		if end == len(text) {
			return end
		}
		start = end + 1
	}
}
