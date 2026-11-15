package resultsgrid

import (
	"slices"
	"testing"
)

// columnNames is the Rust test module's `columns()`. Go port: renamed so it
// does not read as the `columns` locals the tests declare.
func columnNames() []string {
	return []string{"id", "email", "created_at", "LAST_SEEN", "deleted_at"}
}

func assertColumns(t *testing.T, got, want []int) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAHitAnywhereInTheNameCounts(t *testing.T) {
	assertColumns(t, FindColumns(columnNames(), "at"), []int{2, 4})
	assertColumns(t, FindColumns(columnNames(), "mail"), []int{1})
}

// The case this crate's matcher exists for: two words of a name, each
// named by its start.
func TestAQueryMayNameTheWordsOfAName(t *testing.T) {
	columns := []string{"master_state_type_code", "master_client_reference"}
	assertColumns(t, FindColumns(columns, "mast_cl"), []int{1})
	assertColumns(t, FindColumns(columns, "mcr"), []int{1})
}

// Case is not part of what the user is asking: a column the database
// spells in capitals is found by the name typed in lower case.
func TestCaseIsIgnoredBothWays(t *testing.T) {
	assertColumns(t, FindColumns(columnNames(), "last"), []int{3})
	assertColumns(t, FindColumns(columnNames(), "EMAIL"), []int{1})
}

// An open search line lists the result rather than nothing, and the
// space a user leaves while typing is not part of the name.
func TestAnEmptyLineMatchesEveryColumn(t *testing.T) {
	assertColumns(t, FindColumns(columnNames(), ""), []int{0, 1, 2, 3, 4})
	assertColumns(t, FindColumns(columnNames(), "  "), []int{0, 1, 2, 3, 4})
	assertColumns(t, FindColumns(columnNames(), " email "), []int{1})
}

func TestNothingMatchesNothing(t *testing.T) {
	if got := FindColumns(columnNames(), "sample"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if got := FindColumns([]string{}, "id"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// The closest match comes first, whatever lane it sits in — a search
// that answers with the right column fourth is one the user has to
// read before they can use it.
func TestMatchesComeBackRanked(t *testing.T) {
	columns := []string{"invoice_master_name", "master_state_type_code"}
	assertColumns(t, FindColumns(columns, "mast"), []int{1, 0})
}

// Equal matches keep the result's own order, so the list never depends
// on how the matching happened to run.
func TestAnEqualMatchKeepsTheLaneOrder(t *testing.T) {
	columns := []string{"zed", "zip"}
	assertColumns(t, FindColumns(columns, "z"), []int{0, 1})
}
