package sqleditor

import (
	"reflect"
	"testing"
)

func names(list []string) *Vocabulary {
	out := make([]Name, 0, len(list))
	for _, name := range list {
		out = append(out, Name{
			Name:   name,
			Detail: "table",
			Kind:   KindRelation,
			Owner:  nil,
		})
	}
	return NewVocabulary(out)
}

type kindPair struct {
	text  string
	token Token
}

func kinds(line string) []kindPair {
	var out []kindPair
	for _, s := range spans(line, &Vocabulary{}) {
		out = append(out, kindPair{line[s.Range.Start:s.Range.End], s.Token})
	}
	return out
}

func expectKinds(t *testing.T, got, want []kindPair) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSpansCoverTheWholeLine(t *testing.T) {
	for _, line := range []string{
		"select u.id, count(o.id) as n",
		"  on o.user_id = u.id and o.created_at > '2026-05-15'",
		"-- a comment",
		"",
		"select 'héllo wörld' as greeting",
	} {
		total := 0
		for _, s := range spans(line, &Vocabulary{}) {
			total += s.Range.End - s.Range.Start
		}
		if total != len(line) {
			t.Errorf("%s: total %d, want %d", line, total, len(line))
		}
	}
}

func TestKeywordsAreCaseInsensitive(t *testing.T) {
	expectKinds(t, kinds("SELECT"), []kindPair{{"SELECT", TokenKeyword}})
	expectKinds(t, kinds("Select"), []kindPair{{"Select", TokenKeyword}})
	// A column that merely starts like a keyword is not a keyword.
	expectKinds(t, kinds("selected"), []kindPair{{"selected", TokenPlain}})
}

func TestLiteralsAndCommentsTakeTheirOwnColour(t *testing.T) {
	expectKinds(t, kinds("where x = 'a''b' -- why"), []kindPair{
		{"where", TokenKeyword},
		{" x = ", TokenPlain},
		{"'a''b'", TokenLiteral},
		{" ", TokenPlain},
		{"-- why", TokenComment},
	})
}

func TestAQuotedIdentifierIsNotALiteral(t *testing.T) {
	expectKinds(t, kinds("from \"order\""), []kindPair{{"from", TokenKeyword}, {" \"order\"", TokenPlain}})
}

func TestCatalogNamesAreMarkedAndTyposAreNot(t *testing.T) {
	vocabulary := names([]string{"public", "users", "email"})
	line := "select email from public.userz"
	var marked []string
	for _, s := range spans(line, vocabulary) {
		if s.Token == TokenIdentifier {
			marked = append(marked, line[s.Range.Start:s.Range.End])
		}
	}
	if !reflect.DeepEqual(marked, []string{"email", "public"}) {
		t.Errorf("got %v", marked)
	}
}

func TestCatalogNamesAreMatchedCaseInsensitively(t *testing.T) {
	vocabulary := names([]string{"Users"})
	if !vocabulary.Contains("USERS") {
		t.Errorf("USERS not found")
	}
	if !vocabulary.Contains("users") {
		t.Errorf("users not found")
	}
	// One character is noise, not information.
	if names([]string{"x"}).Contains("x") {
		t.Errorf("x found")
	}
}

func TestAKeywordThatIsAlsoAColumnStaysAKeyword(t *testing.T) {
	vocabulary := names([]string{"order"})
	if got := spans("order", vocabulary); !reflect.DeepEqual(got, []span{{Range{0, 5}, TokenKeyword}}) {
		t.Errorf("got %v", got)
	}
}

func TestAnUnterminatedQuoteStopsAtTheLineEnd(t *testing.T) {
	expectKinds(t, kinds("'oops"), []kindPair{{"'oops", TokenLiteral}})
}
