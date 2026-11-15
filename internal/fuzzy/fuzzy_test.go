package fuzzy

import (
	"slices"
	"testing"
	"unicode/utf8"
)

func ranges(t *testing.T, name, query string) []Range {
	t.Helper()
	hit := Score(name, query)
	if hit == nil {
		t.Fatalf("the query matches: %q in %q", query, name)
	}
	return hit.Ranges
}

func mustScore(t *testing.T, name, query string) int32 {
	t.Helper()
	hit := Score(name, query)
	if hit == nil {
		t.Fatalf("matches: %q in %q", query, name)
	}
	return hit.Score
}

func eqRanges(t *testing.T, got, want []Range) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("ranges = %v, want %v", got, want)
	}
}

func r(start, end int) Range { return Range{Start: start, End: end} }

// The case this crate was written for: a query that names two words of
// a name by their starts.
func TestAQueryNamesTheStartsOfTheWords(t *testing.T) {
	if !Matches("master_client_reference", "mast_cl") {
		t.Fatal("mast_cl")
	}
	eqRanges(t,
		ranges(t, "master_client_reference", "mast_cl"),
		[]Range{r(0, 4), r(7, 9)},
	)
	if !Matches("master_client_reference", "master_client") {
		t.Fatal("master_client")
	}
	if !Matches("master_client_reference", "mas_cli_ref") {
		t.Fatal("mas_cli_ref")
	}
}

// A case change is a word boundary, so the same query walks a name
// written in camel case. The typed separator has something to line up
// with even though the name holds none.
func TestACaseChangeIsAWordBoundary(t *testing.T) {
	if !Matches("masterClientReference", "mast_cl") {
		t.Fatal("mast_cl")
	}
	eqRanges(t, ranges(t, "masterClientReference", "mast_cl"), []Range{r(0, 4), r(6, 8)})
	if !Matches("col2_name", "col_2") {
		t.Fatal("col_2")
	}
}

// Initials, which rule 4 in the module docs is what allows.
func TestARunOfWordStartsIsAMatch(t *testing.T) {
	if !Matches("master_client_reference", "mcr") {
		t.Fatal("mcr")
	}
	eqRanges(t,
		ranges(t, "master_client_reference", "mcr"),
		[]Range{r(0, 1), r(7, 8), r(14, 15)},
	)
	if !Matches("LAST_SEEN", "ls") {
		t.Fatal("ls")
	}
}

// The first fragment sits anywhere: asking for a name by the word in
// the middle of it is a fair way to ask.
func TestTheFirstFragmentMaySitAnywhere(t *testing.T) {
	if !Matches("master_client_reference", "client") {
		t.Fatal("client")
	}
	eqRanges(t, ranges(t, "master_client_reference", "client"), []Range{r(7, 13)})
	if !Matches("master_client_reference", "ent_ref") {
		t.Fatal("ent_ref")
	}
}

// The gate. Nothing lands in the middle of a word unless it follows
// the character before it, so a query cannot pick letters out of a
// name it does not name.
func TestNothingMatchesInTheMiddleOfAWord(t *testing.T) {
	// `a`, `s` — then `e` is loose inside `master`.
	if Score("master_client_reference", "ase") != nil {
		t.Fatal("ase")
	}
	if Score("master_client_reference", "mtr") != nil {
		t.Fatal("mtr")
	}
	// A typed separator asks for a word boundary the name has not got.
	if Score("mastcl", "mast_cl") != nil {
		t.Fatal("mast_cl")
	}
	if Score("email", "zzz") != nil {
		t.Fatal("zzz")
	}
	if Score("id", "identifier") != nil {
		t.Fatal("identifier")
	}
}

// A name the query starts beats one it hits in the middle, and a
// tighter match beats a scattered one.
func TestACloserMatchScoresHigher(t *testing.T) {
	front := mustScore(t, "master_state_type_code", "mast")
	middle := mustScore(t, "invoice_master_name", "mast")
	if !(front > middle) {
		t.Fatalf("%d should beat %d", front, middle)
	}

	tight := mustScore(t, "master_client_reference", "mast_cl")
	loose := mustScore(t, "master_client_reference", "mcr")
	if !(tight > loose) {
		t.Fatalf("%d should beat %d", tight, loose)
	}
}

// A name matched in one piece beats one matched in two, whatever word
// starts the second piece happened to land on. The name the user typed
// has to come first.
func TestOnePieceBeatsTwo(t *testing.T) {
	whole := mustScore(t, "users", "users")
	split := mustScore(t, "user_settings", "users")
	if !(whole > split) {
		t.Fatalf("%d should beat %d", whole, split)
	}

	tight := mustScore(t, "order_items", "order_it")
	jumped := mustScore(t, "order_invoice_total", "order_it")
	if !(tight > jumped) {
		t.Fatalf("%d should beat %d", tight, jumped)
	}
}

// A separator the query typed and the name has is part of what
// matched, so the underline does not come back in pieces with the
// bars between them left out.
func TestAMatchedSeparatorIsPartOfTheHit(t *testing.T) {
	eqRanges(t,
		ranges(t, "sample_dev_sample", "sample_dev_sample"),
		[]Range{r(0, 17)},
	)
	eqRanges(t, ranges(t, "sample_dev_sample", "dev_sample"), []Range{r(7, 17)})
	// Only where the name really does end the word there. `mast` stops
	// inside `master`, so the two pieces stay apart.
	eqRanges(t,
		ranges(t, "master_client_reference", "mast_cl"),
		[]Range{r(0, 4), r(7, 9)},
	)
}

// The alignment is the best one, not the first one found: `cl` in
// `client_cluster` lands where it scores highest.
func TestTheBestAlignmentWinsNotTheFirst(t *testing.T) {
	// Both `cl`s start a word, so the earlier one wins on the tie.
	eqRanges(t, ranges(t, "client_cluster", "cl"), []Range{r(0, 2)})
	// Here only the second one does.
	eqRanges(t, ranges(t, "include_client", "cl"), []Range{r(8, 10)})
}

// Case is ignored while the query says nothing about it.
func TestCaseIsIgnoredUntilTheQueryMeansIt(t *testing.T) {
	if !Matches("email", "EMAIL") {
		t.Fatal("EMAIL")
	}
	if !Matches("LAST_SEEN", "last") {
		t.Fatal("last")
	}
	if !Matches("masterClientReference", "masterclientreference") {
		t.Fatal("masterclientreference")
	}
}

// A query written in both cases means the capital: it asks for a
// capital or for a word start.
func TestACapitalInAMixedQueryAsksForOne(t *testing.T) {
	if !Matches("master_client_reference", "mast_Cl") {
		t.Fatal("mast_Cl")
	}
	if !Matches("masterClientReference", "mastCl") {
		t.Fatal("mastCl")
	}
	if Score("include", "inCl") != nil {
		t.Fatal("inCl")
	}
}

// An open search line lists what it is searching.
func TestAnEmptyQueryMatchesEverything(t *testing.T) {
	hit := Score("email", "")
	if hit == nil {
		t.Fatal("an empty query matches")
	}
	if hit.Score != 0 {
		t.Fatalf("score = %d, want 0", hit.Score)
	}
	if len(hit.Ranges) != 0 {
		t.Fatalf("ranges = %v, want none", hit.Ranges)
	}
	if !Matches("email", "  ") {
		t.Fatal("blank query")
	}
	if !Matches("", "") {
		t.Fatal("empty name, empty query")
	}
	if Score("", "id") != nil {
		t.Fatal("empty name, id")
	}
}

// The ranges are byte ranges into the name as it was handed in, on
// character boundaries, so a caller may slice with them.
func TestTheRangesAreSafeToSlice(t *testing.T) {
	name := "größe_wert"
	hit := Score(name, "grö_we")
	if hit == nil {
		t.Fatal("matches")
	}
	for _, rng := range hit.Ranges {
		// `str::get` answers `None` off a boundary or past the end.
		ok := rng.Start <= rng.End && rng.End <= len(name) &&
			(rng.Start == len(name) || utf8.RuneStart(name[rng.Start])) &&
			(rng.End == len(name) || utf8.RuneStart(name[rng.End]))
		if !ok {
			t.Fatalf("%v splits a character", rng)
		}
	}
	eqRanges(t, hit.Ranges, []Range{r(0, 4), r(8, 10)})
}
