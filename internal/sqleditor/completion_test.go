package sqleditor

import (
	"reflect"
	"slices"
	"testing"
)

func ptr(s string) *string { return &s }

func testVocabulary() *Vocabulary {
	return NewVocabulary([]Name{
		{Name: "public", Detail: "schema", Kind: KindSchema, Owner: nil},
		{Name: "users", Detail: "table", Kind: KindRelation, Owner: ptr("public")},
		{Name: "sessions", Detail: "table", Kind: KindRelation, Owner: ptr("public")},
		{Name: "email", Detail: "text", Kind: KindColumn, Owner: ptr("users")},
		{Name: "seen_on", Detail: "date", Kind: KindColumn, Owner: ptr("users")},
	})
}

func labels(completions []Completion) []string {
	out := []string{}
	for _, c := range completions {
		out = append(out, c.Label)
	}
	return out
}

func expectLabels(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestThePrefixIsTheWordEndingAtTheCaret(t *testing.T) {
	text := "select ema"
	if got := prefixRange(text, len(text)); got != (Range{7, 10}) {
		t.Errorf("got %v", got)
	}
	// Caret on a space completes nothing.
	if got := prefixRange("select ", 7); got != (Range{7, 7}) {
		t.Errorf("got %v", got)
	}
	// Mid-word, only what is behind the caret counts.
	if got := prefixRange("select email", 10); got != (Range{7, 10}) {
		t.Errorf("got %v", got)
	}
}

func TestADotQualifiesTheWordAfterIt(t *testing.T) {
	text := "from public.us"
	prefix := prefixRange(text, len(text))
	if got := text[prefix.Start:prefix.End]; got != "us" {
		t.Errorf("prefix = %q", got)
	}
	qualifier, ok := qualifierRange(text, prefix.Start)
	if !ok {
		t.Fatalf("no qualifier")
	}
	if got := text[qualifier.Start:qualifier.End]; got != "public" {
		t.Errorf("qualifier = %q", got)
	}

	// Nothing typed yet after the dot still qualifies.
	text = "from public."
	prefix = prefixRange(text, len(text))
	if !prefix.IsEmpty() {
		t.Errorf("prefix not empty: %v", prefix)
	}
	qualifier, ok = qualifierRange(text, prefix.Start)
	if !ok {
		t.Fatalf("no qualifier")
	}
	if got := text[qualifier.Start:qualifier.End]; got != "public" {
		t.Errorf("qualifier = %q", got)
	}

	// A bare word is not qualified.
	if _, ok := qualifierRange("from users", 5); ok {
		t.Errorf("a bare word was qualified")
	}
}

func TestAQualifierNarrowsToItsChildren(t *testing.T) {
	vocabulary := testVocabulary()
	// A table offers its columns, and nothing else.
	expectLabels(t, labels(vocabulary.Candidates("", ptr("users"))), []string{"email", "seen_on"})
	// A schema offers its relations.
	expectLabels(t, labels(vocabulary.Candidates("", ptr("public"))), []string{"users", "sessions"})
	// A keyword never appears behind a dot.
	for _, c := range vocabulary.Candidates("se", ptr("users")) {
		if c.Kind == KindKeyword {
			t.Errorf("keyword %q behind a dot", c.Label)
		}
	}
}

func TestAnUnknownQualifierFallsBackToEverything(t *testing.T) {
	// `userz.` is a typo; an empty panel would just look broken.
	candidates := testVocabulary().Candidates("em", ptr("userz"))
	expectLabels(t, labels(candidates), []string{"email"})
}

func TestKeywordsAndNamesShareTheUnqualifiedList(t *testing.T) {
	candidates := testVocabulary().Candidates("se", nil)
	got := labels(candidates)
	for _, want := range []string{"select", "sessions", "seen_on"} {
		if !slices.Contains(got, want) {
			t.Errorf("%q missing from %v", want, got)
		}
	}
	// Shortest first.
	if got[0] != "set" {
		t.Errorf("first = %q, want set (%v)", got[0], got)
	}
}

func TestMatchingIgnoresCase(t *testing.T) {
	expectLabels(t, labels(testVocabulary().Candidates("EMA", ptr("USERS"))), []string{"email"})
}

func TestTheListStaysShort(t *testing.T) {
	candidates := testVocabulary().Candidates("", nil)
	if len(candidates) > MaxCandidates {
		t.Errorf("%d candidates", len(candidates))
	}
	// Nothing typed and nothing qualifying: names only, no keywords.
	for _, c := range candidates {
		if c.Kind == KindKeyword {
			t.Errorf("keyword %q with nothing typed", c.Label)
		}
	}
}

func TestNothingMatchesAWordTheDatabaseDoesNotHave(t *testing.T) {
	if got := testVocabulary().Candidates("zzz", nil); len(got) != 0 {
		t.Errorf("got %v", labels(got))
	}
}

// The word under the caret names the parts of a name, so a long column
// is offered without typing it from the front. Before this the panel
// matched on the prefix alone and offered nothing at all here.
func TestAWordMayNameThePartsOfAName(t *testing.T) {
	var list []Name
	for _, name := range []string{"master_client_reference", "master_state_type_code"} {
		list = append(list, Name{Name: name, Detail: "text", Kind: KindColumn, Owner: ptr("masters")})
	}
	vocabulary := NewVocabulary(list)
	expectLabels(t, labels(vocabulary.Candidates("mast_cl", ptr("masters"))), []string{"master_client_reference"})
	expectLabels(t, labels(vocabulary.Candidates("client", ptr("masters"))), []string{"master_client_reference"})
	// The closest match leads: `mast` starts one name and sits inside
	// the middle of neither.
	expectLabels(t, labels(vocabulary.Candidates("mast", ptr("masters"))), []string{"master_state_type_code", "master_client_reference"})
}

// A name is written the way a statement must spell it, and a quote the
// user already opened — or one already after the word — is used, not
// doubled.
func TestAcceptingANameQuotesItWhenItMustBe(t *testing.T) {
	name := func(label string) Completion { return Completion{Label: label, Kind: KindRelation} }
	cases := []struct {
		content string
		r       Range
		c       Completion
		want    string
	}{
		{"select * from us", Range{14, 16}, name("users"), "users"},
		{"select * from Us", Range{14, 16}, name("Users"), `"Users"`},
		{"select * from or", Range{14, 16}, name("order"), `"order"`},
		{`select * from "Us`, Range{15, 17}, name("Users"), `"Users"`},
		{`select * from "us"`, Range{15, 17}, name("users"), `"users`},
		{"sel", Range{0, 3}, Completion{Label: "select", Kind: KindKeyword}, "select"},
	}
	for _, c := range cases {
		if got := completionText(c.c, c.content, c.r); got != c.want {
			t.Errorf("%q over %q = %s, want %s", c.c.Label, c.content, got, c.want)
		}
	}
}
