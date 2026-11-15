// Package fuzzy matches a name against what the user has typed so far.
//
// Three places in the app ask the same question — the ⌘J column find, the
// ⌘K palette with the sidebar filter behind it, and the SQL editor's
// completion panel — and before this crate each answered it differently: a
// substring, a substring per path part, a prefix. So `mast_cl` found
// `master_client_reference` in none of them, and the same name was found
// three ways depending on which line the user was typing into.
//
// # The rule
//
// A database name is a path of words with the separators left in —
// `master_client_reference`, `LAST_SEEN`, `masterClientReference` — and a
// user who types part of one is nearly always typing the **starts of its
// words**. That is the whole idea IntelliJ's `MinusculeMatcher` is built
// on, and it is what this crate copies:
//
//  1. The query is cut into **fragments** at everything that is not a
//     letter or a digit, and the separators themselves are dropped. So
//     `mast_cl`, `mast cl` and `mast-cl` all ask the same question, and
//     `mast_cl` finds `masterClientReference` as well — a case change is a
//     word boundary too, so the typed `_` has something to line up with.
//  2. The **first** fragment may sit anywhere inside the name, because
//     `client` is a fair way to ask for `master_client_reference`.
//  3. Every fragment after it must **begin a word** of the name.
//  4. Inside a fragment a character either follows the one before it or
//     begins a word. That is what lets `mcr` find
//     `master_client_reference` by its initials.
//
// Rule 4 is the one that keeps this from turning into a fuzzy finder. An
// unrestricted subsequence — fzf's rule — answers a three-letter query
// with half of a hundred-column result, and a jump list that long is not
// a jump. Every character either continues a word or starts one, so
// nothing matches by accident in the middle of a name.
//
// # Ranking
//
// A gate says which names match; it does not say which one the user meant.
// The score is [fzf's], because the shape of it is well tested: a match is
// worth [ScoreMatch], a word start is worth more, a character
// immediately after the last one is worth more again, and a skip costs —
// [GapStart] for opening the gap and [GapExtend] per character after
// it, so one long jump is cheaper than three short ones and a tight match
// always beats a scattered one. The first character's word-start bonus is
// doubled, because where a query begins says most about what it meant, and
// the name's own first character is worth a shade more than an inner word
// start — which is what puts `master_state_type_code` above
// `invoice_master_name` for `mast`.
//
// The alignment is worked out with a dynamic program rather than greedily,
// because the greedy answer is wrong often enough to notice: `cl` in
// `client_cluster` should land on whichever of the two scores better, not
// on the first one found.
//
// Callers break ties themselves, and should break them on the length of
// the name: two names matched the same way, and the shorter one is more of
// what the user typed.
//
// # Case
//
// Case is ignored until the user shows they mean it. A query with both an
// upper-case and a lower-case letter is read as **strict**: `Cl` then asks
// for a capital `C` or a word starting with one, which is how a camel-case
// name is picked out of a set that also holds a lower-case one. A query
// typed all in one case asks nothing about case at all, so `EMAIL` still
// finds `email` — a name the database spells in capitals is found by a
// name typed in lower case, and the other way round.
//
// Every range this crate hands back is a byte range on character
// boundaries of the string that was passed in, because the match is
// walked over `char_indices` rather than over bytes. The palette paints
// those ranges into the label it builds, so a range that fell inside a
// character would panic on the slice.
//
// # Finding text
//
// A find line over a buffer or a result asks a different question —
// where is this text, not which name was meant — and [Needle] answers it
// with a plain substring under the same case rule.
//
// [fzf's]: https://github.com/junegunn/fzf/blob/master/src/algo/algo.go
package fuzzy

import (
	"math"
	"unicode"
	"unicode/utf8"
)

// Range is a byte range `Start..End` into a string, the port of
// `std::ops::Range<usize>`.
type Range struct {
	Start int
	End   int
}

// IsEmpty reports whether the range holds nothing, as `Range::is_empty`.
func (r Range) IsEmpty() bool {
	return !(r.Start < r.End)
}

// ScoreMatch is what one character of the query is worth when it lands.
const ScoreMatch int32 = 16

// GapStart is opening a gap: the query skipped over part of the name.
//
// It costs more than the word start it buys ([BonusBoundary]) plus the
// tight step it gave up ([BonusConsecutive]), and that is a rule rather
// than a number picked by feel: **a name matched in one piece always beats
// a name matched in two**. Otherwise `users` would rank
// `user_settings` — four characters and then a jump onto a word start —
// above `users` itself, which is the name the user typed.
const GapStart int32 = -8

// GapExtend is each further character of that gap.
const GapExtend int32 = -1

// BonusBoundary is a character that starts a word: one after a separator.
const BonusBoundary int32 = 8

// The name's own first character. Worth a shade more than an inner word
// start, which is what sorts `master_state_type_code` above
// `invoice_master_name` for `mast`: both matched a whole word from its
// start, and only one of them is that word.
const bonusNameStart int32 = 10

// A word start marked by a case change or a letter/digit change rather
// than by a separator. Worth a shade less, so `_c` outranks `C`.
const bonusCamel int32 = 7

// BonusConsecutive is a character that follows the one the query matched
// before it.
const BonusConsecutive int32 = 4

// The first character carries its word-start bonus twice: a name the query
// starts is a better answer than one it hits in the middle.
const firstCharMultiplier int32 = 2

// The query and the name agree on the case of this character.
const bonusCase int32 = 2

// Well below any real score, and far enough from `i32::MIN` that the gap
// penalties can be subtracted from it without wrapping.
const none int32 = math.MinInt32 / 2

// Match is where a query landed in a name, and how well.
type Match struct {
	Score int32
	// The matched runs, in order and never touching: two ranges next to
	// each other are one range. Empty when the query is empty.
	Ranges []Range
}

// Pattern is a query, cut into fragments once so a long list of names is
// not charged for it per name.
//
// **A Pattern is not safe for concurrent use.** Scoring a name lays it
// out and fills an alignment table, and both live in buffers the pattern
// keeps from one name to the next, so a list of three thousand names costs
// three buffers rather than six thousand. Copies of a Pattern share those
// buffers. Every caller scores on the UI thread, one name at a time; one
// that wants to score from two goroutines compiles a pattern for each.
type Pattern struct {
	chars []typedChar
	// Whether an upper-case character in the query is a demand. See the
	// module docs: only a query written in both cases means it.
	strictCase bool
	// What scoring a name reuses; nil until the first name is scored.
	scratch *scratch
}

// scratch is what one score fills and the next one overwrites: the name
// laid out, the alignment table, and the places walked back out of it.
// Nothing in it outlives the call that filled it — a `Match` gets ranges
// of its own.
type scratch struct {
	glyphs []glyph
	table  []cell
	places []int
}

type typedChar struct {
	folded rune
	typed  rune
	// This character opens a fragment, so it may only land on the start of
	// a word.
	opens bool
}

// NewPattern cuts a query into fragments.
func NewPattern(query string) Pattern {
	strictCase := anyRune(query, isUppercase) && anyRune(query, isLowercase)
	var chars []typedChar
	opens := true
	for _, typed := range query {
		// Anything that is not part of a word ends one. The separator
		// itself is dropped: what it says is "a word starts after me",
		// and the name may say that with a `_` or with a capital.
		if !isAlphanumeric(typed) {
			opens = true
			continue
		}
		chars = append(chars, typedChar{
			folded: fold(typed),
			typed:  typed,
			opens:  opens,
		})
		opens = false
	}
	return Pattern{chars: chars, strictCase: strictCase, scratch: &scratch{}}
}

// IsEmpty reports an empty query. An empty query matches everything, so a
// search line that has just opened lists what it is searching rather than
// nothing.
func (p Pattern) IsEmpty() bool {
	return len(p.chars) == 0
}

// Score is how well `name` answers this query, or nil when it does not.
func (p Pattern) Score(name string) *Match {
	if p.IsEmpty() {
		return &Match{
			Score:  0,
			Ranges: []Range{},
		}
	}
	// A cheap ordered-subsequence scan first. It allocates nothing and
	// throws out most of a large vocabulary, which the table below
	// cannot do without building itself first.
	if !p.couldMatch(name) {
		return nil
	}
	work := p.scratch
	if work == nil {
		work = &scratch{}
	}
	laid := layOut(name, work.glyphs[:0])
	work.glyphs = laid.chars
	return p.align(&laid, work)
}

// Matches reports whether `name` answers this query.
func (p Pattern) Matches(name string) bool {
	return p.Score(name) != nil
}

// Are the query's characters even present, in order? A necessary
// condition for the real match and a much cheaper one.
func (p Pattern) couldMatch(name string) bool {
	next := 0
	for _, ch := range name {
		if next >= len(p.chars) {
			return true
		}
		if p.chars[next].folded == fold(ch) {
			next++
		}
	}
	return next >= len(p.chars)
}

// The best legal alignment of the query on the name.
//
// `table[j * n + i]` is the best alignment of the query's first `j + 1`
// characters that puts character `j` on the name's character `i`:
// [cell.score] is what it is worth, `none` where no such alignment
// is legal, and [cell.from] is where the character before it sat, so
// the ranges can be walked back out.
func (p Pattern) align(name *nameLayout, work *scratch) *Match {
	n, m := len(name.chars), len(p.chars)
	if m > n {
		return nil
	}
	if cap(work.table) < n*m {
		work.table = make([]cell, n*m)
	}
	table := work.table[:n*m]
	for k := range table {
		table[k] = cell{score: none, from: 0}
	}

	// The first character of the query may land anywhere.
	for i := 0; i < n; i++ {
		if points, ok := p.lands(0, name, i); ok {
			table[i].score = points
		}
	}

	for j := 1; j < m; j++ {
		opens := p.chars[j].opens
		row, above := j*n, (j-1)*n
		// The best score of a jump into `i`, with the gap already paid
		// for: every place the character before could have sat, bar the
		// one right behind, which is the tight alignment below.
		jump := none
		jumpFrom := 0
		for i := 1; i < n; i++ {
			if points, ok := p.lands(j, name, i); ok {
				// Tight: the character before it took the character
				// before it. A fragment's first character may only do
				// this where the name itself starts a word — a typed
				// `_` has to line up with something.
				tight := table[above+i-1].score
				if tight > none && (!opens || name.chars[i].bonus > 0) {
					table[row+i] = cell{
						score: tight + points + BonusConsecutive,
						from:  i - 1,
					}
				}
				// Or the query skipped ahead, which it may only do onto
				// the start of a word. This is the whole of the gate.
				if jump > none &&
					name.chars[i].bonus > 0 &&
					jump+points > table[row+i].score {
					table[row+i] = cell{
						score: jump + points,
						from:  jumpFrom,
					}
				}
			}
			// Roll the running jump forward one place, for the next
			// `i`: either the gap it already holds grows by one, or the
			// cell that has just fallen two behind opens a new one.
			opened := none
			if reachable := table[above+i-1].score; reachable > none {
				opened = reachable + GapStart
			}
			widened := none
			if jump > none {
				widened = jump + GapExtend
			}
			if opened >= widened {
				jump = opened
				jumpFrom = i - 1
			} else {
				jump = widened
			}
		}
	}

	// Where the last character of the query ended up. On a tie the
	// earliest place wins, so an equally good match nearer the front of
	// the name is the one reported.
	last := (m - 1) * n
	end, score := 0, table[last].score
	for i := 1; i < n; i++ {
		if table[last+i].score > score {
			end, score = i, table[last+i].score
		}
	}
	if score <= none {
		return nil
	}

	at := end
	places := work.places[:0]
	for j := m - 1; j >= 0; j-- {
		places = append(places, at)
		if j > 0 {
			at = table[j*n+at].from
		}
	}
	for a, b := 0, len(places)-1; a < b; a, b = a+1, b-1 {
		places[a], places[b] = places[b], places[a]
	}
	work.places = places
	return &Match{
		Score:  score,
		Ranges: name.runs(places, p.chars),
	}
}

// What the query's character `j` is worth on the name's character `i`,
// or false when it cannot sit there at all.
func (p Pattern) lands(j int, name *nameLayout, i int) (int32, bool) {
	typed := &p.chars[j]
	glyph := &name.chars[i]
	if glyph.folded != typed.folded {
		return 0, false
	}
	// A capital the user meant asks for a capital, or for a word start:
	// `Cl` finds `master_client_reference` and `masterClient`, and
	// leaves `include` alone.
	if p.strictCase &&
		isUppercase(typed.typed) &&
		!(isUppercase(glyph.ch) || glyph.bonus > 0) {
		return 0, false
	}
	weight := int32(1)
	if j == 0 {
		weight = firstCharMultiplier
	}
	points := ScoreMatch + glyph.bonus*weight
	if glyph.ch == typed.typed {
		points += bonusCase
	}
	return points, true
}

// One square of the table [Pattern.align] fills in.
type cell struct {
	score int32
	// Where the character before this one sat. Only meaningful on a cell
	// whose score is not `none`, which is the only kind the walk back
	// visits.
	from int
}

// Score is how well `name` answers `query`. Use [Pattern] instead when the
// same query is asked of many names.
func Score(name, query string) *Match {
	return NewPattern(query).Score(name)
}

// Matches reports whether `name` answers `query`.
func Matches(name, query string) bool {
	return NewPattern(query).Matches(name)
}

// A name laid out for matching.
//
// One vector rather than one per field, and the table in [Pattern.align]
// is one vector too. That is not a micro-optimisation to shrug at: a
// completion vocabulary is every column of every table, so a database of
// 3,000 relations is tens of thousands of names, matched again on every
// keystroke. What the matching costs there is mostly what it allocates.
type nameLayout struct {
	chars []glyph
}

// One character of a name: as it is written, folded for comparison, where
// it sits in the original string, and what a match on it is worth.
type glyph struct {
	ch     rune
	folded rune
	at     Range
	// The word-start bonus, and `0` for a character that starts no word.
	// A positive value is what the gate reads as "a word starts here".
	bonus int32
}

// layOut lays a name out into `chars`, a buffer the pattern keeps and
// appends to from its start, so a list of names fills one buffer rather
// than allocating one each.
func layOut(text string, chars []glyph) nameLayout {
	var before rune
	hasBefore := false
	for at, ch := range text {
		chars = append(chars, glyph{
			ch:     ch,
			folded: fold(ch),
			at:     Range{Start: at, End: at + utf8.RuneLen(ch)},
			bonus:  bonus(before, hasBefore, ch),
		})
		before, hasBefore = ch, true
	}
	return nameLayout{chars: chars}
}

// The matched places as byte ranges, with anything adjacent joined:
// the palette underlines these, and two ranges that touch would draw
// as one anyway.
//
// A separator the query typed and the name has is joined over as well.
// The query said "a word ends here" and the name's `_` is where it
// ends, so it is part of what matched: `sample_dev_sample` typed whole
// underlines whole, rather than in three pieces with the bars between
// them left plain.
func (l *nameLayout) runs(places []int, typed []typedChar) []Range {
	var runs []Range
	for which, place := range places {
		at := l.chars[place].at
		joins := false
		if len(runs) > 0 {
			run := runs[len(runs)-1]
			if run.End == at.Start {
				joins = true
			} else if typed[which].opens {
				joins = true
				for between := places[which-1] + 1; between < place; between++ {
					if isAlphanumeric(l.chars[between].ch) {
						joins = false
						break
					}
				}
			}
		}
		if len(runs) > 0 && joins {
			runs[len(runs)-1].End = at.End
		} else {
			runs = append(runs, at)
		}
	}
	return runs
}

// Where a word starts. A separator starts nothing itself — it is the
// character *after* it that begins the word — and a case change or a step
// between letters and digits is a word start with no separator to show it,
// which is what makes `masterClient` and `col2` behave like `master_client`
// and `col_2`.
func bonus(before rune, hasBefore bool, ch rune) int32 {
	if !isAlphanumeric(ch) {
		return 0
	}
	if !hasBefore {
		return bonusNameStart
	}
	if !isAlphanumeric(before) {
		return BonusBoundary
	}
	if isLowercase(before) && isUppercase(ch) {
		return bonusCamel
	}
	if isNumeric(before) != isNumeric(ch) {
		return bonusCamel
	}
	return 0
}

// One character folded to one character, so a fold never changes how many
// characters a name has and the places found in the folded copy are the
// places in the original. Postgres folds unquoted identifiers to lower
// case, so this is the same question the server asks.
//
// `ch.to_lowercase().next()` in the Rust: the first character of the full
// lowercase mapping. The only unconditional mapping of more than one
// character is U+0130, whose first character is the simple mapping
// `unicode.ToLower` gives as well.
func fold(ch rune) rune {
	return unicode.ToLower(ch)
}

// The Rust character classes, which Go's `unicode` package does not spell
// the same way. Each one is the Unicode property the Rust method reads.

// isUppercase is `char::is_uppercase`: the derived Uppercase property,
// Lu plus Other_Uppercase.
func isUppercase(r rune) bool {
	return unicode.IsUpper(r) || unicode.Is(unicode.Other_Uppercase, r)
}

// isLowercase is `char::is_lowercase`: the derived Lowercase property,
// Ll plus Other_Lowercase.
func isLowercase(r rune) bool {
	return unicode.IsLower(r) || unicode.Is(unicode.Other_Lowercase, r)
}

// isAlphabetic is `char::is_alphabetic`: the derived Alphabetic property,
// Uppercase + Lowercase + Lt + Lm + Lo + Nl + Other_Alphabetic.
func isAlphabetic(r rune) bool {
	return unicode.IsLetter(r) ||
		unicode.Is(unicode.Nl, r) ||
		unicode.Is(unicode.Other_Alphabetic, r) ||
		unicode.Is(unicode.Other_Uppercase, r) ||
		unicode.Is(unicode.Other_Lowercase, r)
}

// isNumeric is `char::is_numeric`: the general categories Nd, Nl and No.
func isNumeric(r rune) bool {
	return unicode.IsNumber(r)
}

// isAlphanumeric is `char::is_alphanumeric`: Alphabetic or Numeric.
func isAlphanumeric(r rune) bool {
	return isAlphabetic(r) || isNumeric(r)
}

// anyRune is `str::chars().any(f)`.
func anyRune(s string, f func(rune) bool) bool {
	for _, r := range s {
		if f(r) {
			return true
		}
	}
	return false
}
