package fuzzy

// Not a port of a Rust test: a Pattern reuses its buffers from one name to
// the next, and nothing a score returns may depend on what it scored
// before.

import (
	"reflect"
	"testing"
)

func TestAPatternScoresEveryNameAsAFreshOneWould(t *testing.T) {
	names := []string{
		"master_client_reference",
		"m",
		"masterClientReference",
		"invoice_master_name",
		"a_much_longer_name_than_any_other_in_this_list_master_client",
		"mcr",
		"",
		"include_client",
		"MASTER_CLIENT",
	}
	for _, query := range []string{"mast_cl", "mcr", "cl", "Cl", "m", "EMAIL", ""} {
		shared := NewPattern(query)
		// Twice round, so every name follows a longer and a shorter one.
		for round := 0; round < 2; round++ {
			for _, name := range names {
				got := shared.Score(name)
				want := NewPattern(query).Score(name)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%q in %q: shared pattern %+v, fresh %+v", query, name, got, want)
				}
			}
		}
	}
}

func TestAZeroPatternStillScores(t *testing.T) {
	var p Pattern
	if got := p.Score("users"); got == nil || got.Score != 0 {
		t.Errorf("zero pattern: %+v", got)
	}
}
