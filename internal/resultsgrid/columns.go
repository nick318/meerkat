// Finding a column of a result by name.
//
// A wide result is one the pane cannot show whole: a hundred columns is a
// hundred lanes to scroll past, and the name is usually the only thing the
// user knows about the one they want. So the search is over the column
// *names* the result came back with, and the answer is where each one sits
// — an index into the same lanes [Selection] and the grid count
// in, so the caller can put the cursor there and scroll to it.
//
// The rule is [fuzzy]'s, which is the ⌘K palette's and the SQL editor's
// as well: a query names the **starts of a name's words**, so `mast_cl`
// finds `master_client_reference` — the case this replaced a plain
// substring for. A substring could only be typed by somebody who already
// knew where in the name to start reading, which is not what a search line
// over a hundred columns is for.
//
// **The answer is ranked, not in result order.** A search that offers the
// right column fourth is one the user reads before they can use it, and
// the match itself says which one is closest to what was typed; lane 34
// coming before lane 4 is what a ranked answer looks like. Ties go to the
// shorter name and then to the earlier lane, so the order is settled by
// the result rather than by how the matching happened to run.
//
// A column's **type** is not part of it. A result carries its column names
// and its values; the driver reports no type per column, so a search over
// type names would only work on the tabs that came from the catalog, and a
// search that answers differently depending on where the tab came from is
// worse than one that does not offer it.

package resultsgrid

import (
	"cmp"
	"slices"
	"unicode/utf8"

	"github.com/nick318/meerkat/internal/fuzzy"
)

// FindColumns answers with the columns whose names answer `needle`, best
// first.
//
// An empty needle matches every column, in the result's own order, so a
// search line that has just opened lists the result from its first lane
// rather than showing nothing until a character is typed.
func FindColumns(columns []string, needle string) []int {
	pattern := fuzzy.NewPattern(needle)
	// Nothing typed, nothing to rank: the result's own order is the only
	// order there is, and sorting an unranked list by name length would
	// shuffle the lanes for no reason.
	if pattern.IsEmpty() {
		all := make([]int, 0, len(columns))
		for index := range columns {
			all = append(all, index)
		}
		return all
	}
	type hit struct {
		index  int
		score  int32
		length int
	}
	var found []hit
	for index, name := range columns {
		match := pattern.Score(name)
		if match == nil {
			continue
		}
		found = append(found, hit{index, match.Score, utf8.RuneCountInString(name)})
	}
	// Best score first; then the shorter name, which is more of what was
	// typed; then the lane the result puts first.
	slices.SortStableFunc(found, func(a, b hit) int {
		return cmp.Or(
			cmp.Compare(-a.score, -b.score),
			cmp.Compare(a.length, b.length),
			cmp.Compare(a.index, b.index),
		)
	})
	out := make([]int, 0, len(found))
	for _, h := range found {
		out = append(out, h.index)
	}
	return out
}
