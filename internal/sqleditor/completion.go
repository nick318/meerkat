package sqleditor

// What the editor can offer to finish the word under the caret: the
// names in the connected database, and SQL's own keywords.
//
// Everything here is pure text work over the buffer, so it is all
// testable without a window.

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nick318/meerkat/internal/fuzzy"
	"github.com/nick318/meerkat/internal/query"
)

// MaxCandidates is how many candidates the panel shows. The design's panel
// holds a handful; a longer list is a menu, not a hint.
const MaxCandidates = 8

// Kind is what a name is. The order is the Rust derive's `Ord`.
type Kind int

const (
	KindRelation Kind = iota
	KindColumn
	KindSchema
	KindKeyword
)

// String names the kind as Rust's Debug did.
func (k Kind) String() string {
	switch k {
	case KindRelation:
		return "Relation"
	case KindColumn:
		return "Column"
	case KindSchema:
		return "Schema"
	case KindKeyword:
		return "Keyword"
	}
	return "Kind(?)"
}

// Name is one name the editor knows about.
type Name struct {
	Name string
	// Shown greyed to the right of the name: `table`, `int8`, `schema`.
	Detail string
	Kind   Kind
	// The relation a column belongs to, or the schema a relation belongs
	// to. This is what lets `users.` offer only that table's columns.
	// Nil is `None`.
	Owner *string
}

type Completion struct {
	Label  string
	Detail string
	Kind   Kind
	// The parts of `label` the typed word matched, as byte ranges in order
	// and never overlapping. The panel marks these rather than the label's
	// first characters: a match sits anywhere inside a name, so marking
	// the head would say the user typed something they did not.
	Matched []Range
}

// Vocabulary is the names in the connected database, folded to lower case
// for lookup because Postgres folds unquoted identifiers the same way.
// The zero value is Rust's `Vocabulary::default()`.
type Vocabulary struct {
	entries []entry
	lowered map[string]struct{}
}

type entry struct {
	name    string
	lowered string
	detail  string
	kind    Kind
	owner   *string
}

// toASCIILowercase is Rust's `str::to_ascii_lowercase`: ASCII letters
// only.
func toASCIILowercase(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// NewVocabulary is Rust's `Vocabulary::new`.
func NewVocabulary(names []Name) *Vocabulary {
	entries := make([]entry, 0, len(names))
	for _, name := range names {
		var owner *string
		if name.Owner != nil {
			lowered := toASCIILowercase(*name.Owner)
			owner = &lowered
		}
		entries = append(entries, entry{
			lowered: toASCIILowercase(name.Name),
			owner:   owner,
			name:    name.Name,
			detail:  name.Detail,
			kind:    name.Kind,
		})
	}
	lowered := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		lowered[e.lowered] = struct{}{}
	}
	return &Vocabulary{entries: entries, lowered: lowered}
}

// Contains says whether the database has this name. Used by the tokenizer
// to colour real names differently from typos.
func (v *Vocabulary) Contains(word string) bool {
	// A one-character name would light up half the buffer for no
	// information; `id` and longer is where this starts to help.
	if len(word) <= 1 {
		return false
	}
	_, ok := v.lowered[toASCIILowercase(word)]
	return ok
}

func (v *Vocabulary) IsEmpty() bool {
	return len(v.entries) == 0
}

// Candidates for `prefix` — the word under the caret — restricted to
// `qualifier`'s children when the caret sits after `something.`, and
// ranked best first. A nil qualifier is `None`.
//
// It is called a prefix because that is where the caret is, not
// because a name has to start with it: matching is [fuzzy]'s, the
// same rule the ⌘J column find and the ⌘K palette answer to, so
// `mast_cl` offers `master_client_reference`. A viewer's columns are
// long compound names, and typing one from the front is exactly the
// work completion is meant to save.
//
// An empty prefix is allowed: that is what `users.` should offer.
func (v *Vocabulary) Candidates(prefix string, qualifier *string) []Completion {
	prefix = toASCIILowercase(prefix)
	var owner *string
	if qualifier != nil {
		lowered := toASCIILowercase(*qualifier)
		owner = &lowered
	}

	// A qualifier the database does not know is a typo, not a filter:
	// fall back to everything rather than showing an empty panel.
	qualified := false
	if owner != nil {
		for _, e := range v.entries {
			if e.owner != nil && *e.owner == *owner {
				qualified = true
				break
			}
		}
	}

	pattern := fuzzy.NewPattern(prefix)
	var candidates []ranked
	for _, e := range v.entries {
		if qualified && !sameOwner(e.owner, owner) {
			continue
		}
		hit := pattern.Score(e.name)
		if hit == nil {
			continue
		}
		candidates = append(candidates, ranked{
			score: hit.Score,
			completion: Completion{
				Label:   e.name,
				Detail:  e.detail,
				Kind:    e.kind,
				Matched: fromFuzzy(hit.Ranges),
			},
		})
	}

	// Keywords are not owned by anything, so a qualified caret never
	// wants them, and neither does a caret with nothing typed yet.
	//
	// They are matched on the **prefix**, not the way a name is. A
	// keyword is one word with no parts to walk, so there is nothing
	// for the word-start rule to find, and matching one anywhere inside
	// would answer `em` with `temp` — a word the user was not typing.
	// The score itself is the same one the names are given, or a
	// keyword and a name matched the same way would not sort against
	// each other.
	if !qualified && prefix != "" {
		for _, keyword := range query.Keywords {
			if !strings.HasPrefix(keyword, prefix) {
				continue
			}
			hit := pattern.Score(keyword)
			if hit == nil {
				continue
			}
			candidates = append(candidates, ranked{
				score: hit.Score,
				completion: Completion{
					Label:   keyword,
					Detail:  "keyword",
					Kind:    KindKeyword,
					Matched: fromFuzzy(hit.Ranges),
				},
			})
		}
	}

	// Best match first, then the shortest label: the closest thing to
	// what was typed. Ties go to database names before keywords, then
	// alphabetically, so the order never depends on how the catalog
	// happened to be walked.
	slices.SortStableFunc(candidates, func(a, b ranked) int {
		return cmp.Or(
			cmp.Compare(b.score, a.score),
			cmp.Compare(len(a.completion.Label), len(b.completion.Label)),
			cmp.Compare(a.completion.Kind, b.completion.Kind),
			strings.Compare(a.completion.Label, b.completion.Label),
		)
	})
	out := make([]Completion, 0, len(candidates))
	for _, r := range candidates {
		// `dedup_by`: a run of the same label and kind keeps its first.
		if n := len(out); n > 0 && out[n-1].Label == r.completion.Label && out[n-1].Kind == r.completion.Kind {
			continue
		}
		out = append(out, r.completion)
	}
	if len(out) > MaxCandidates {
		out = out[:MaxCandidates]
	}
	return out
}

// sameOwner is Rust's `entry.owner == owner` over two `Option<String>`.
func sameOwner(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// fromFuzzy carries the matcher's ranges over into the editor's.
func fromFuzzy(ranges []fuzzy.Range) []Range {
	out := make([]Range, len(ranges))
	for i, r := range ranges {
		out[i] = Range(r)
	}
	return out
}

// ranked is a candidate with the score that decides where it sits in the
// panel.
type ranked struct {
	score      int32
	completion Completion
}

// prefixRange is the word being typed at `offset`: the run of identifier
// characters ending there. Empty when the caret is not on a word.
func prefixRange(text string, offset int) Range {
	start := offset
	for start > 0 {
		ch, size := utf8.DecodeLastRuneInString(text[:start])
		if !isWordChar(ch) {
			break
		}
		start -= size
	}
	return Range{start, offset}
}

// qualifierRange is the name qualifying the word that starts at
// `prefixStart`, when the caret sits after `schema.` or `table.`.
func qualifierRange(text string, prefixStart int) (Range, bool) {
	before, ok := strings.CutSuffix(text[:prefixStart], ".")
	if !ok {
		return Range{}, false
	}
	r := prefixRange(text, len(before))
	if r.IsEmpty() {
		return Range{}, false
	}
	return r, true
}

// completionText is what accepting a completion writes over `r`: a
// keyword as it is, and a name as a statement has to spell it — in
// double quotes when Postgres would fold it or read it as a key word, and
// always when the user has already opened a quote before it. A closing
// quote already after the word is not written twice.
func completionText(c Completion, content string, r Range) string {
	if c.Kind == KindKeyword {
		return c.Label
	}
	opened := r.Start > 0 && content[r.Start-1] == '"'
	if !opened && !query.NeedsQuotes(c.Label) {
		return c.Label
	}
	text := query.QuoteIdent(c.Label)
	if r.End < len(content) && content[r.End] == '"' {
		text = text[:len(text)-1]
	}
	return text
}
