package resultsgrid

import (
	"slices"
	"testing"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/fuzzy"
)

// findData is the Rust test module's `data()`. Go port: renamed, as
// selection_test.go has a `data()` of its own in the same package.
func findData() *GridData {
	text := func(value string) dbclient.Value { return dbclient.ValueText(value) }
	return NewGridData(
		[]string{"id", "email", "note"},
		[][]dbclient.Value{
			{dbclient.ValueInt(1), text("ada@example.com"), dbclient.ValueNull{}},
			{
				dbclient.ValueInt(2),
				text("grace@example.org"),
				text("Ada's friend"),
			},
			{dbclient.ValueInt(12), text("linus@example.com"), text("")},
		},
	)
}

func needle(text string) *fuzzy.Needle {
	n := fuzzy.NewNeedle(text)
	return &n
}

func found(text string) []Cell {
	hits, _ := FindCells(findData(), needle(text), 100)
	return hits
}

func assertCells(t *testing.T, got, want []Cell, message string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v: %s", got, want, message)
	}
}

func TestHitsComeBackInReadingOrder(t *testing.T) {
	assertCells(t,
		found("ada"),
		[]Cell{NewCell(0, 1), NewCell(1, 2)},
		"row by row, and left to right within a row",
	)
}

// A number is found by its digits, as it is painted.
func TestAValueThatIsNotTextIsFoundByWhatItReads(t *testing.T) {
	assertCells(t, found("2"), []Cell{NewCell(1, 0), NewCell(2, 0)}, "")
	assertCells(t, found("null"), []Cell{NewCell(0, 2)}, "")
}

func TestNothingTypedFindsNothing(t *testing.T) {
	if got := found(""); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestTheSearchStopsAtItsLimitAndSaysSo(t *testing.T) {
	hits, capped := FindCells(findData(), needle("example"), 2)
	assertCells(t, hits, []Cell{NewCell(0, 1), NewCell(1, 1)}, "")
	if !capped {
		t.Fatal("capped")
	}

	hits, capped = FindCells(findData(), needle("example"), 3)
	if len(hits) != 3 {
		t.Fatalf("len = %d", len(hits))
	}
	if capped {
		t.Fatal("a search that ends exactly on the limit found everything")
	}
}

// A grown needle reads only the cells the shorter one found, and
// answers what a full search would.
func TestAGrownNeedleIsRefinedFromTheHitsItHad(t *testing.T) {
	data := findData()
	hits, _ := FindCells(data, needle("ex"), 100)
	longer := needle("example.com")
	full, _ := FindCells(data, longer, 100)
	assertCells(t, RefineCells(data, hits, longer), full, "")
	strict := needle("Ada's")
	ada, _ := FindCells(data, needle("ada"), 100)
	assertCells(t, RefineCells(data, ada, strict), []Cell{NewCell(1, 2)}, "")
}

func TestARaggedRowIsSearchedOnlyAsFarAsTheHeader(t *testing.T) {
	data := NewGridData(
		[]string{"a"},
		[][]dbclient.Value{{dbclient.ValueText("x"), dbclient.ValueText("needle")}},
	)
	hits, _ := FindCells(data, needle("needle"), 10)
	if len(hits) != 0 {
		t.Fatalf("got %v", hits)
	}
}
