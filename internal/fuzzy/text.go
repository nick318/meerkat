// Finding a piece of text inside a longer one: the ⌘F line over the SQL
// editor's buffer and over the values of a result.
//
// **This is not the name matcher, on purpose.** [Pattern] answers
// "which name did the user mean", and reads a query as the starts of a
// name's words. A find line answers "where is this text", over prose, SQL
// and values the user did not name — the word-start rule over a statement
// full of word starts would light up half the buffer. So a needle here is
// a plain substring, the way every editor's find bar reads one, and the
// palette's statement search draws the line in the same place.
//
// **Case follows the crate's own rule**, so one habit serves every search
// line in the app: case is ignored until the needle is written in both
// cases. `select` finds `SELECT`, `EMAIL` finds `email`, and `userId` asks
// for exactly that spelling.
//
// Every range handed back is a byte range on character boundaries of the
// haystack, because the walk is over `char_indices`: the editor paints the
// ranges and selects them, and a range inside a character would panic on
// the slice.

package fuzzy

import (
	"math"
	"unicode/utf8"
)

// Needle is a find line's text, read once so a long haystack is not
// charged for reading it again per position.
type Needle struct {
	chars []rune
	// Whether the case of the needle is a demand. See the module docs.
	strictCase bool
}

// NewNeedle reads a find line's text.
func NewNeedle(text string) Needle {
	strictCase := anyRune(text, isUppercase) && anyRune(text, isLowercase)
	var chars []rune
	if strictCase {
		chars = []rune(text)
	} else {
		for _, ch := range text {
			chars = append(chars, fold(ch))
		}
	}
	return Needle{chars: chars, strictCase: strictCase}
}

// IsEmpty reports an empty needle. An empty needle finds nothing. A find
// line that has just opened has not asked a question yet, and lighting
// every position of the buffer would answer one nobody asked.
func (n Needle) IsEmpty() bool {
	return len(n.chars) == 0
}

// FindAll is every place the needle sits in `haystack`, left to right and
// never overlapping: `aa` in `aaaa` is two hits, not three, so stepping
// from one hit to the next always moves past the text it selected.
func (n Needle) FindAll(haystack string) []Range {
	found, _ := n.FindCapped(haystack, math.MaxInt)
	return found
}

// FindCapped is the first `limit` hits, and whether there were more. A
// one-letter needle over a long buffer hits nearly everywhere, and past a
// few thousand hits the count is all anybody reads.
func (n Needle) FindCapped(haystack string, limit int) ([]Range, bool) {
	found := []Range{}
	if n.IsEmpty() {
		return found, false
	}
	from := 0
	for {
		hit, ok := n.findFrom(haystack, from)
		if !ok {
			break
		}
		if len(found) == limit {
			return found, true
		}
		from = hit.End
		found = append(found, hit)
	}
	return found, false
}

// IsIn reports whether the needle is anywhere in `haystack`.
func (n Needle) IsIn(haystack string) bool {
	if n.IsEmpty() {
		return false
	}
	_, ok := n.findFrom(haystack, 0)
	return ok
}

// The first hit that starts at or after byte `from`.
func (n Needle) findFrom(haystack string, from int) (Range, bool) {
	// `haystack.get(from..)?`: past the end, or off a character boundary,
	// is no hit.
	if from > len(haystack) || (from < len(haystack) && !utf8.RuneStart(haystack[from])) {
		return Range{}, false
	}
	rest := haystack[from:]
	for start := range rest {
		end := start
		tail := rest[start:]
		landed := true
		for _, want := range n.chars {
			if len(tail) == 0 {
				landed = false
				break
			}
			ch, size := utf8.DecodeRuneInString(tail)
			tail = tail[size:]
			if !n.same(want, ch) {
				landed = false
				break
			}
			end += size
		}
		if landed {
			return Range{Start: from + start, End: from + end}, true
		}
	}
	return Range{}, false
}

func (n Needle) same(want, ch rune) bool {
	if n.strictCase {
		return want == ch
	}
	return want == fold(ch)
}

// One character folded to one character, so a fold never changes how many
// characters a hit covers and its range in the original stays right. The
// name matcher folds the same way.
//
// The Rust module has a `fold` of its own with exactly this body; in Go the
// two files share one package, so this file uses `fold` from fuzzy.go.
