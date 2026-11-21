// The ⌘K palette: one search line over everything this session can open
// — the tables and views in the catalog, and the queries this connection
// has already run.
//
// It does **not** search columns. A column is part of a result, not
// something to open, and the palette's every row ends in "open this": a
// column row could only offer its table, which the table row already
// does. The way to a column is ⌘J over the result that has it.
//
// The palette holds no data of its own. The shell hands it the catalog it
// introspected and the runs it read back from the local file, and this
// module turns them into a flat list of rows. That keeps the matching
// pure, so it can be tested without a window, and it keeps the palette
// honest: it can only offer what the session already has.
//
// Rows are flattened the way the sidebar and the history list are —
// headings and results share one height — because `uniform_list` measures
// one row and lays out only what is on screen. Arrow keys then move the
// selection by index, and the list scrolls to it.
//
// Matching a **name** is [`fuzzy`]'s rule, shared with the ⌘J column find
// and the SQL editor's completion panel: a query names the starts of a
// name's words, so `mast_cl` finds `master_client_reference` and `mcr`
// finds it by its initials. It is not an open fuzzy score — nothing
// matches loose in the middle of a word — so the hit is still a few runs
// of characters the row can underline honestly.
//
// Matching a **statement** is a plain case-insensitive substring, and that
// difference is on purpose. A name is a path of words, which is what the
// word-start rule reads; a run's statement is prose, long and full of
// word starts, so the same rule over it would answer a short query with
// everything the connection has ever run. IntelliJ draws the line in the
// same place: its hump matcher is for symbols, and find-in-text is a
// substring.
//
// A **dot in the query names a path**. Every result carries its name in
// parts — the schema and the relation — and the typed parts are lined up
// with the *end* of that path. So `task` finds every `task` in the
// database, `sample_dev_sample.task` finds the one, and `dev.ta` finds it
// without typing it out. It is one rule; nothing about it is
// special-cased per section.
//
// Port notes: every package-level name here that would be generic in
// package main (`build`, `Row`, `Scope`, `Pick`, …) carries the module's
// name as a prefix, `palette::build` → `PaletteBuild`. The Rust
// `NaiveDate` is a `time.Time` holding the local calendar day (see
// `naiveDateOf` in history.go).

package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/fuzzy"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// PaletteAction is the palette's actions: `actions!(palette, [Toggle,
// SelectPrev, SelectNext, OpenInNewTab])`.
type PaletteAction int

const (
	PaletteToggle PaletteAction = iota
	PaletteSelectPrev
	PaletteSelectNext
	PaletteOpenInNewTab
)

// PaletteKeyContext is the context the palette's own keys are scoped to.
// It sits below the shell's context, so a key bound in both — ⌘⏎ — goes
// to the palette while the palette is open and to the shell when it is
// not.
const PaletteKeyContext = "Palette"

// PaletteShellKeyBindings and PaletteKeyBindings are the key bindings for
// the palette. ⌘K is scoped to the shell rather than to the palette,
// because it has to work when the palette is closed as well.
//
// The Rust `key_bindings()` was one table carrying a context per binding.
// A uikit table is dispatched on the element that wears its context, so
// the one table is two here: this one for the `Shell` context, and
// PaletteKeyBindings for PaletteKeyContext.
func PaletteShellKeyBindings() []uikit.KeyBinding[PaletteAction] {
	return []uikit.KeyBinding[PaletteAction]{
		uikit.Bind("cmd-k", PaletteToggle),
	}
}

// PaletteKeyBindings is the PaletteKeyContext half of the Rust
// `key_bindings()`; see PaletteShellKeyBindings.
func PaletteKeyBindings() []uikit.KeyBinding[PaletteAction] {
	return []uikit.KeyBinding[PaletteAction]{
		uikit.Bind("up", PaletteSelectPrev),
		uikit.Bind("down", PaletteSelectNext),
		uikit.Bind("ctrl-p", PaletteSelectPrev),
		uikit.Bind("ctrl-n", PaletteSelectNext),
		uikit.Bind("cmd-enter", PaletteOpenInNewTab),
	}
}

// The dialog's geometry, from the comp.
const (
	PaletteWidth      float32 = 660.
	PaletteListHeight float32 = 404.
	PaletteTopMargin  float32 = 96.
)

// PaletteInputFontSize: the search line reads a size larger than the rest
// of the app: it is the one thing on the screen the user is looking at.
const PaletteInputFontSize = theme.SizeInput

// PaletteRowHeight is one height for headings and results alike, so the
// list can virtualize.
const PaletteRowHeight float32 = 28.

// Column widths from the comp: glyph, name, meta, trailing.
const (
	paletteGlyphWidth    float32 = 16.
	paletteMetaWidth     float32 = 84.
	paletteTrailingWidth float32 = 96.
)

// How many results each section shows when the palette is searching
// everything, and how many one section shows on its own. The list is a
// way in, not a report: past a screenful the user should narrow instead.
const (
	paletteSectionCap = 6
	paletteScopeCap   = 40
)

// How long a name may read before it is cut. A statement runs far past
// the name column, and the column that holds it is about this wide.
const paletteLabelChars = 68

// PaletteScope is which of the three lists the palette is searching.
type PaletteScope int

const (
	PaletteScopeAll PaletteScope = iota
	PaletteScopeTables
	PaletteScopeHistory
)

// PaletteScopes is the chips, in the order the comp draws them
// (`Scope::ALL`).
var PaletteScopes = [3]PaletteScope{PaletteScopeAll, PaletteScopeTables, PaletteScopeHistory}

// Label is the chip's word.
func (s PaletteScope) Label() string {
	switch s {
	case PaletteScopeAll:
		return "all"
	case PaletteScopeTables:
		return "tables"
	case PaletteScopeHistory:
		return "history"
	}
	return ""
}

// Prefix is the prefix that picks this scope from the search line itself.
func (s PaletteScope) Prefix() (string, bool) {
	switch s {
	case PaletteScopeTables:
		return "t:", true
	case PaletteScopeHistory:
		return "h:", true
	}
	return "", false
}

func (s PaletteScope) showsTables() bool {
	return s == PaletteScopeAll || s == PaletteScopeTables
}

func (s PaletteScope) showsHistory() bool {
	return s == PaletteScopeAll || s == PaletteScopeHistory
}

// PaletteParse reads the scope out of the query itself. A typed `t:` wins
// over the chip that is lit, because the user just said what they meant.
func PaletteParse(query string, chip PaletteScope) (PaletteScope, string) {
	for _, scope := range PaletteScopes {
		if prefix, ok := scope.Prefix(); ok {
			if rest, found := strings.CutPrefix(query, prefix); found {
				return scope, strings.TrimLeftFunc(rest, unicode.IsSpace)
			}
		}
	}
	return chip, strings.TrimLeftFunc(query, unicode.IsSpace)
}

// PalettePickKind says which of the two a PalettePick is.
type PalettePickKind int

const (
	// PalettePickTable opens (or focuses) a table or view.
	PalettePickTable PalettePickKind = iota
	// PalettePickQuery opens a statement in a query tab. It is never
	// re-run behind the user, for the same reason the history screen
	// does not re-run one.
	PalettePickQuery
)

// PalettePick is what opening a row does. Everything the palette offers
// ends in one of these two, so the shell needs no knowledge of the row
// that produced it.
//
// The Rust enum `Pick { Table { schema, table }, Query(String) }`: Schema
// and Table are set for PalettePickTable, Statement for PalettePickQuery.
type PalettePick struct {
	Kind      PalettePickKind
	Schema    string
	Table     string
	Statement string
}

// OnPick is called with the row the user picked, and whether ⌘⏎ asked for
// a tab of its own rather than the tab that is already open.
type OnPick func(pick PalettePick, newTab bool)

// PaletteRow is the list, flattened: a heading per section, then that
// section's rows. One of PaletteHeader or *PaletteItem.
type PaletteRow interface {
	// Pick is what opening the row does; false for a heading.
	Pick() (PalettePick, bool)
}

// PaletteHeader is `Row::Header`.
type PaletteHeader struct {
	Label string
	Count string
}

// PaletteItem is one result, ready to paint (`Row::Item(Item)`).
type PaletteItem struct {
	label string
	// The parts of `label` the search matched, as byte ranges in order
	// and never overlapping. Empty when the query is empty, or when the
	// hits fell outside the part of a long label that is on screen.
	hits  []fuzzy.Range
	glyph paletteGlyph
	meta  string
	// What the last column says when the row is not selected: a clock for
	// a run, nothing for a relation.
	trailing string
	failed   bool
	pick     PalettePick
	// The real path this result sits at — schema and relation — and
	// which part of it the query addressed. The
	// search line completes from these; the row paints only `label`.
	// Empty for a run, which is prose rather than a path.
	path  []string
	named int
}

// PaletteRank is the Rust `(usize, i32, usize)` sort key: how far out the
// query aligned, what the match cost, how long the name is. Go has no
// ordered tuples, so Less compares it the way Rust compares a tuple.
type PaletteRank struct {
	Out  int
	Cost int32
	Len  int
}

// Less is the tuple's lexicographic order.
func (r PaletteRank) Less(other PaletteRank) bool {
	if r.Out != other.Out {
		return r.Out < other.Out
	}
	if r.Cost != other.Cost {
		return r.Cost < other.Cost
	}
	return r.Len < other.Len
}

// A result before the palette knows how much of its name the row will
// show. That is not decided per result: it depends on what the whole
// section found, so it waits for [`dress`].
type paletteCandidate struct {
	// The name in parts, outermost first: the schema, then the relation.
	parts []string
	// Where the query hit inside each part: one list of runs per part, and
	// an empty list for a part the query never addressed.
	hits [][]fuzzy.Range
	// The outermost part the query itself named. Everything before it the
	// user did not ask about.
	named int
	// The outermost part worth showing when nothing forces the whole
	// path: a relation hides its schema.
	from int
	// Whether the search line may be completed from this path. A run
	// carries a statement, not a path, so it may not.
	completes bool
	// How far out the query aligned, what the match cost — a negated
	// score, so lower is still better — and how long the name is. A table
	// the query named directly comes before one its schema matched, and
	// `users` before `active_users_7d`.
	rank     PaletteRank
	glyph    paletteGlyph
	meta     string
	trailing string
	failed   bool
	pick     PalettePick
}

type paletteGlyph int

const (
	paletteGlyphTable paletteGlyph = iota
	paletteGlyphView
	paletteGlyphRun
)

// Pick is `Row::pick` for a heading: nothing.
func (PaletteHeader) Pick() (PalettePick, bool) {
	return PalettePick{}, false
}

// Pick is `Row::pick` for a result.
func (item *PaletteItem) Pick() (PalettePick, bool) {
	return item.pick, true
}

// PaletteResults is what one search found: the rows to paint, and how
// many results matched before the caps above cut the list down.
type PaletteResults struct {
	Rows    []PaletteRow
	Matches int
}

// PaletteBuild searches the session and flattens what it found.
//
// `runs` is the history newest first, as the store returns it; the order
// within each section is the order the caller handed it in, so the newest
// query and the first table of the catalog stay where they were.
//
// `catalog` may be nil (`None`). `today` is a calendar day, as
// HistoryToday returns it.
func PaletteBuild(
	catalog *introspect.Catalog,
	runs []storage.QueryRun,
	scope PaletteScope,
	needle string,
	today time.Time,
) PaletteResults {
	cap := paletteScopeCap
	if scope == PaletteScopeAll {
		cap = paletteSectionCap
	}
	var rows []PaletteRow
	matches := 0

	if scope.showsTables() {
		found := paletteTables(catalog, needle)
		matches += len(found)
		schemas := paletteSchemasOf(found)
		items, qualified := paletteDress(found)
		// The schema is named once: on the rows if they carry it, in the
		// heading if they do not.
		heading := "TABLES"
		if !qualified && len(schemas) == 1 {
			heading = fmt.Sprintf("TABLES · %s", paletteASCIIUpper(schemas[0]))
		}
		rows = palettePushSection(rows, heading, items, cap)
	}
	if scope.showsHistory() {
		found := paletteHistory(runs, needle, today)
		matches += len(found)
		items, _ := paletteDress(found)
		rows = palettePushSection(rows, "RECENT QUERIES", items, cap)
	}

	return PaletteResults{Rows: rows, Matches: matches}
}

// A heading, then that section's rows, cut to the cap. An empty section
// leaves no heading behind: a palette showing `TABLES 0` teaches nothing.
func palettePushSection(rows []PaletteRow, label string, found []*PaletteItem, cap int) []PaletteRow {
	if len(found) == 0 {
		return rows
	}
	total := len(found)
	shown := min(total, cap)
	count := strconv.Itoa(total)
	if shown < total {
		count = fmt.Sprintf("%d of %d", shown, total)
	}
	rows = append(rows, PaletteHeader{Label: label, Count: count})
	for _, item := range found[:shown] {
		rows = append(rows, item)
	}
	return rows
}

// The distinct schemas a section's results came from, in catalog order.
// A run belongs to the connection rather than to a schema, and carries
// only one part, so it contributes none.
func paletteSchemasOf(found []paletteCandidate) []string {
	var schemas []string
	for _, candidate := range found {
		if len(candidate.parts) <= 1 {
			continue
		}
		seen := false
		for _, schema := range schemas {
			if schema == candidate.parts[0] {
				seen = true
				break
			}
		}
		if !seen {
			schemas = append(schemas, candidate.parts[0])
		}
	}
	return schemas
}

// The last pass over a section, once its results are known: settle how
// much of each name the row shows, join it, and cut the long ones down to
// the name column.
//
// The order matters. Clipping first would put the ellipsis in the wrong
// place and could drop a hit that joining then shifts.
//
// Also reports whether the rows ended up carrying their schema, so the
// heading can stop naming it.
func paletteDress(found []paletteCandidate) ([]*PaletteItem, bool) {
	// A section whose results run over more than one schema shows every
	// schema. With a single schema the heading names it already, and
	// repeating it on every row says nothing.
	spread := len(paletteSchemasOf(found)) > 1
	// Unless the query named the schema: someone who typed one wants to
	// read it back.
	named := false
	for _, candidate := range found {
		if len(candidate.parts) > 1 && candidate.named == 0 {
			named = true
			break
		}
	}

	items := make([]*PaletteItem, 0, len(found))
	for _, candidate := range found {
		from := 0
		if !spread {
			from = min(candidate.from, candidate.named)
		}
		var path []string
		if candidate.completes {
			path = append([]string(nil), candidate.parts...)
		}

		var label strings.Builder
		var hits []fuzzy.Range
		for ix, part := range candidate.parts[from:] {
			runs := candidate.hits[from+ix]
			if label.Len() > 0 {
				label.WriteByte('.')
			}
			for _, run := range runs {
				if run.IsEmpty() {
					continue
				}
				hits = append(hits, fuzzy.Range{Start: label.Len() + run.Start, End: label.Len() + run.End})
			}
			label.WriteString(part)
		}

		text, clipped := paletteClip(label.String(), hits, paletteLabelChars)
		items = append(items, &PaletteItem{
			label:    text,
			hits:     clipped,
			glyph:    candidate.glyph,
			meta:     candidate.meta,
			trailing: candidate.trailing,
			failed:   candidate.failed,
			pick:     candidate.pick,
			path:     path,
			named:    candidate.named,
		})
	}
	return items, spread || named
}

// The relations the query names, ranked by where the hit sits and then by
// how short the name is: `users` before `active_users_7d`.
func paletteTables(catalog *introspect.Catalog, needle string) []paletteCandidate {
	if catalog == nil {
		return nil
	}
	var found []paletteCandidate
	query := newPalettePathQuery(needle)
	for _, schema := range catalog.Schemas {
		for _, table := range schema.Tables {
			probe := [2]string{schema.Name, table.Name}
			// A bare schema name answers with the tables it holds.
			aligned, ok := query.locate(probe[:])
			if !ok {
				continue
			}
			parts := []string{schema.Name, table.Name}
			named := aligned.named
			glyph := paletteGlyphTable
			// A relation says what it is, not how big it is. The
			// estimate belongs to the sidebar; here it only makes one
			// row in a list of near-identical names louder than the
			// rest.
			meta := ""
			if table.Kind == introspect.TableKindView {
				glyph = paletteGlyphView
				meta = "view"
			}
			found = append(found, paletteCandidate{
				rank:  paletteRankOf(&aligned, len(parts), table.Name),
				parts: parts,
				hits:  aligned.hits,
				named: named,
				// A relation hides its schema: the heading has it.
				from:      1,
				completes: true,
				glyph:     glyph,
				meta:      meta,
				trailing:  "",
				failed:    false,
				pick: PalettePick{
					Kind:   PalettePickTable,
					Schema: schema.Name,
					Table:  table.Name,
				},
			})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].rank.Less(found[j].rank) })
	return found
}

// The runs whose statement holds the query, newest first — the order the
// store already returned them in, which is the order that matters here.
//
// A statement is one part, not a path: a dot in the query is matched
// literally, because `public.users` is how the statement itself reads.
func paletteHistory(runs []storage.QueryRun, needle string, today time.Time) []paletteCandidate {
	var found []paletteCandidate
	for _, run := range runs {
		statement := paletteOneLine(run.Statement)
		hit, ok := paletteFind(statement, needle)
		if !ok {
			continue
		}
		// The earlier the query sits in the statement, the better: a
		// cost, the way a table's negated score is.
		cost := int32(min(hit.Start, math.MaxInt32))
		meta := ""
		switch {
		case run.Error != nil:
			meta = "error"
		case run.ElapsedMs != nil:
			meta = uikit.FormatMillis(*run.ElapsedMs)
		}
		found = append(found, paletteCandidate{
			rank:      PaletteRank{Out: 0, Cost: cost, Len: len(statement)},
			parts:     []string{statement},
			hits:      [][]fuzzy.Range{{hit}},
			named:     0,
			from:      0,
			completes: false,
			glyph:     paletteGlyphRun,
			meta:      meta,
			trailing:  paletteClock(run.RanAt, today),
			failed:    run.Error != nil,
			pick:      PalettePick{Kind: PalettePickQuery, Statement: run.Statement},
		})
	}
	return found
}

// PaletteCompletion is what the search line would become if the selected
// row's name were accepted: its real path, as far as the query has
// reached, with a trailing dot when there is another part to go. False
// when there is nothing to add.
//
// It **replaces** what was typed rather than appending to it, because a
// hit sits anywhere inside a name: `dev` completes to
// `sample_dev_sample.`, which no amount of appending would reach. That
// is also why the caller only paints a hint when the completion happens
// to start with what the user typed — the rest of the time accepting it
// rewrites the line, and a hint would have lied about that.
//
// An empty query completes nothing. There is a selected row, but the
// user has said nothing for it to finish.
func PaletteCompletion(rows []PaletteRow, selected int, needle string) (string, bool) {
	if needle == "" {
		return "", false
	}
	if selected < 0 || selected >= len(rows) {
		return "", false
	}
	item, ok := rows[selected].(*PaletteItem)
	if !ok {
		return "", false
	}
	if len(item.path) == 0 {
		return "", false
	}
	return paletteFinish(item.path, item.named, needle)
}

// PaletteGhost is what the search line paints faint beside what was
// typed, for a completion ⇥ would take. Empty when there is nothing to
// offer.
//
// **Both forms say the same thing in the shape the line can be read in.**
// A completion that carries on from the query is painted as the rest of
// the word, so the line reads as one name: `sample_dev_` then `sample.`.
// One that does not carry on is the ordinary case, because a hit sits
// anywhere inside a name — `sam_dev` finds `sample_dev_sample` and
// finishes to a string that begins nowhere near it. Painting that as a
// suffix would read `sam_devsample_dev_sample.`, which is not a name and
// not what ⇥ does. So it is painted after a gap and behind the ⇥ the
// footer already names, as the *replacement* it is.
//
// Saying nothing at all was the older answer, and it hid the completion
// exactly when the user could least guess it: the schema they never
// typed is the part worth showing.
func PaletteGhost(needle, completed string) string {
	if rest, ok := strings.CutPrefix(completed, needle); ok {
		return rest
	}
	return fmt.Sprintf("  ⇥ %s", completed)
}

// PaletteCompletePath finishes a query from one path, the way the palette
// finishes it from the selected row. The sidebar's filter asks for this,
// so ⇥ walks a name in the same steps wherever it is typed.
func PaletteCompletePath(parts []string, needle string) (string, bool) {
	if needle == "" {
		return "", false
	}
	aligned, ok := paletteFindPath(parts, needle)
	if !ok {
		return "", false
	}
	return paletteFinish(parts, aligned.named, needle)
}

// The line the query becomes when it takes the next part of `path`.
// `named` is the part the query addressed; everything before it the user
// did not ask about and does not get.
func paletteFinish(path []string, named int, needle string) (string, bool) {
	// One past the part being typed. A query cannot reach deeper than the
	// path it matched, but a stale selection could still say so.
	reached := named + len(strings.Split(needle, "."))
	if reached > len(path) {
		return "", false
	}

	completed := strings.Join(path[named:reached], ".")
	if reached < len(path) {
		completed += "."
	}
	if completed == needle {
		return "", false
	}
	return completed, true
}

// Where a dotted query hits a name, part by part, and which part of the
// path it started at.
//
// The typed parts line up with the *end* of the path first, so a query
// says as much of the name as the user cares to type and no more. Every
// typed part must hit its own part; a query with more parts than the name
// has cannot match at all.
//
// A query that comes up empty at the end tries again further out, which
// is how a bare schema name finds the tables it holds.
//
//	parts:  [sample_dev_sample, task]
//	"task"                     ^hit
//	"dev.ta"      ^hit         ^hit
//	"dev_sample"  ^hit  (slid out one part, so the whole schema answers)
func paletteFindPath(parts []string, needle string) (paletteAligned, bool) {
	return newPalettePathQuery(needle).locate(parts)
}

// palettePathQuery is a query cut at its dots, with each part compiled
// into a `fuzzy.Pattern` once. The palette and the sidebar match one
// query against every relation in the catalog, and compiling the parts
// per name — which is what `fuzzy.Score` does — cost a pattern and an
// alignment table per name on every keystroke. Like the patterns it
// holds, it is for one thread: the UI's.
type palettePathQuery struct {
	typed []fuzzy.Pattern
	// The matches of one alignment, kept until it is known to hold.
	matches []*fuzzy.Match
}

func newPalettePathQuery(needle string) *palettePathQuery {
	parts := strings.Split(needle, ".")
	q := &palettePathQuery{
		typed:   make([]fuzzy.Pattern, len(parts)),
		matches: make([]*fuzzy.Match, len(parts)),
	}
	for ix, part := range parts {
		q.typed[ix] = fuzzy.NewPattern(part)
	}
	return q
}

// locate is paletteFindPath for a query compiled once.
func (q *palettePathQuery) locate(parts []string) (paletteAligned, bool) {
	typed := q.typed
	if len(typed) > len(parts) {
		return paletteAligned{}, false
	}
	flush := len(parts) - len(typed)

	// Innermost alignment first: `task` is a table before it is a schema.
	for named := flush; named >= 0; named-- {
		var score int32
		matched := true
		for ix, part := range typed {
			hit := part.Score(parts[named+ix])
			if hit == nil {
				matched = false
				break
			}
			score += hit.Score
			q.matches[ix] = hit
		}
		if !matched {
			continue
		}
		hits := make([][]fuzzy.Range, len(parts))
		for ix := range typed {
			hits[named+ix] = q.matches[ix].Ranges
			q.matches[ix] = nil
		}
		return paletteAligned{
			hits:  hits,
			named: named,
			typed: len(typed),
			score: score,
		}, true
	}
	return paletteAligned{}, false
}

// Where a query landed on one path.
type paletteAligned struct {
	// The matched runs inside each part, and nothing for a part the query
	// did not address.
	hits [][]fuzzy.Range
	// The outermost part the query itself named.
	named int
	// How many parts the query was written in. This says which parts were
	// addressed, which the hits cannot: a query ending in a dot addresses
	// a part it has matched nothing inside.
	typed int
	score int32
}

// PalettePathRank: does a path answer this query, and how well? The
// sidebar's filter asks the palette, so `schema.table` finds the same
// thing in both places — and, since the answer is the palette's own
// [`rank`], puts its hits in the same order too. False says the path does
// not answer at all.
//
// The key sorts ascending (PaletteRank.Less), and it is the palette's row
// order: an alignment further out first, then the score, then the length
// of the name.
func PalettePathRank(parts []string, needle string) (PaletteRank, bool) {
	return newPalettePathQuery(needle).rank(parts)
}

// rank is PalettePathRank for a query compiled once.
func (q *palettePathQuery) rank(parts []string) (PaletteRank, bool) {
	aligned, ok := q.locate(parts)
	if !ok {
		return PaletteRank{}, false
	}
	name := ""
	if len(parts) > 0 {
		name = parts[len(parts)-1]
	}
	return paletteRankOf(&aligned, len(parts), name), true
}

// How a result sorts: an alignment further out first, because a name the
// query hit directly beats a name its container matched; then how well the
// query matched, then how long the name is. (Rust: `rank`.)
func paletteRankOf(aligned *paletteAligned, parts int, name string) PaletteRank {
	innermost := aligned.named + aligned.typed - 1
	return PaletteRank{Out: parts - 1 - innermost, Cost: -aligned.score, Len: len(name)}
}

// A multi-line statement is one row here, so the newlines become spaces
// and the runs of whitespace collapse.
func paletteOneLine(statement string) string {
	return strings.Join(strings.Fields(statement), " ")
}

// `today 09:12`, `yest. 18:04`, then the date. The near days are named
// for the same reason the history screen names them.
func paletteClock(ranAt int64, today time.Time) string {
	at, ok := localTimestamp(ranAt)
	if !ok {
		return ""
	}
	clock := at.Format("15:04")
	switch naiveNumDays(today, naiveDateOf(at)) {
	case 0:
		return fmt.Sprintf("today %s", clock)
	case 1:
		return fmt.Sprintf("yest. %s", clock)
	default:
		return at.Format("2 Jan 15:04")
	}
}

// Where the query sits inside a **statement**, as a byte range, or false
// when it is not there at all. An empty query matches everything at offset
// zero but underlines nothing.
//
// A statement is prose, so this is a substring and not [`fuzzy`]'s
// word-start rule — see the module docs for why the two differ.
//
// The comparison lowercases ASCII only, which is what keeps the range
// usable: `to_ascii_lowercase` never changes a character's byte length,
// so an offset into the lowered copy is an offset into the original.
func paletteFind(haystack, needle string) (fuzzy.Range, bool) {
	if needle == "" {
		return fuzzy.Range{Start: 0, End: 0}, true
	}
	start := strings.Index(paletteASCIILower(haystack), paletteASCIILower(needle))
	if start < 0 {
		return fuzzy.Range{}, false
	}
	return fuzzy.Range{Start: start, End: start + len(needle)}, true
}

// paletteASCIILower is `str::to_ascii_lowercase`: ASCII letters only, so
// no character changes its byte length.
func paletteASCIILower(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if 'A' <= ch && ch <= 'Z' {
			b[i] = ch + ('a' - 'A')
		}
	}
	return string(b)
}

// paletteASCIIUpper is `str::to_ascii_uppercase`.
func paletteASCIIUpper(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if 'a' <= ch && ch <= 'z' {
			b[i] = ch - ('a' - 'A')
		}
	}
	return string(b)
}

// Cut a long label down to the name column, keeping the matched part in
// view. A cut end is marked with an ellipsis, and the hits move with the
// text; a hit that fell outside the window is dropped rather than
// pointed at the wrong characters.
func paletteClip(label string, hits []fuzzy.Range, maxChars int) (string, []fuzzy.Range) {
	total := utf8.RuneCountInString(label)
	if total <= maxChars {
		return label, hits
	}

	// The window is placed around the first hit, with a little text before
	// it so it does not sit flush against the leading ellipsis.
	const lead = 8
	hitChar := 0
	if len(hits) > 0 {
		hitChar = utf8.RuneCountInString(label[:hits[0].Start])
	}
	startChar := 0
	if hitChar > max(maxChars-lead, 0) {
		startChar = hitChar - lead
	}
	endChar := min(startChar+maxChars, total)
	start, end := paletteCharOffset(label, startChar), paletteCharOffset(label, endChar)

	var text strings.Builder
	if startChar > 0 {
		text.WriteRune('…')
	}
	text.WriteString(label[start:end])
	if endChar < total {
		text.WriteRune('…')
	}

	shift := 0
	if startChar > 0 {
		shift = utf8.RuneLen('…')
	}
	var moved []fuzzy.Range
	for _, hit := range hits {
		if hit.Start >= start && hit.End <= end {
			moved = append(moved, fuzzy.Range{Start: hit.Start - start + shift, End: hit.End - start + shift})
		}
	}
	return text.String(), moved
}

// The byte offset of the nth character, or the end of the string.
func paletteCharOffset(text string, chars int) int {
	n := 0
	for offset := range text {
		if n == chars {
			return offset
		}
		n++
	}
	return len(text)
}

// --- painting ------------------------------------------------------------

// PaletteRowView is one row of the flattened list (Rust: `palette_row`).
// Free-standing for the same reason the catalog's and the history's rows
// are: the list's render closure outlives the borrow of the view it came
// from.
func PaletteRowView(
	c *ui.Context,
	ix int,
	row PaletteRow,
	selected bool,
	onPick OnPick,
	colors *theme.ThemeColors,
) ui.Element {
	switch row := row.(type) {
	case PaletteHeader:
		return ui.Row(c).
			Height(PaletteRowHeight).
			FillWidth().
			AlignItems(ui.End).
			Justify(ui.SpaceBetween).
			Padding(0, 9, 5, 9).
			Children(func() {
				uikit.SectionLabel(c, row.Label)
				ui.Box(c).
					FontSize(theme.SizeMeta).
					TextColor(colors.TextFaint.Color()).
					Children(func() { ui.Text(c, row.Count) })
			})
	case *PaletteItem:
		item := row
		pick := item.pick
		text := colors.TextBody
		if item.failed {
			text = colors.Error
		}
		meta := colors.TextMuted
		if item.failed {
			meta = colors.ErrorSecondary
		}

		card := ui.Row(c.Key(fmt.Sprintf("palette-row-%d", ix))).
			Height(PaletteRowHeight).
			FillWidth().
			AlignItems(ui.Center).
			Gap(10).
			Padding(0, 10).
			Radius(6).
			Cursor(ui.CursorPointer)
		card.OnClick(func() {
			onPick(pick, card.ClickModifiers()&ui.Cmd != 0)
		})
		card.Children(func() {
			ui.Box(c).Width(paletteGlyphWidth).Shrink(0).Children(func() {
				paletteGlyphElement(c, item.glyph, selected, colors)
			})
			weight := 400
			if selected {
				weight = 500
			}
			nameColor := text
			if selected && !item.failed {
				nameColor = colors.Text
			}
			ui.Row(c).
				Grow(1).Basis(0).
				MinWidth(0).
				AlignItems(ui.Stretch).
				Clip().
				// A relation's name and a run's statement are both
				// what the database said or the user typed, so the
				// mono; a size up from the grid, as the column find's
				// names are, because the list is the dialog.
				Font(theme.MonoFontFamily).
				FontSize(theme.SizeEmphasis).
				FontWeight(weight).
				TextColor(nameColor.Color()).
				Children(func() { paletteSpans(c, item, selected, colors) })
			ui.Box(c).
				Width(paletteMetaWidth).
				Shrink(0).
				FontSize(theme.SizeBody).
				TextColor(meta.Color()).
				Children(func() { ui.Text(c, item.meta).SingleLine() })
			trailingColor := colors.TextFaint
			if selected {
				trailingColor = colors.Accent
			}
			// The selected row is the one that says what ⏎
			// would do; the rest keep their own last column.
			trailing := item.trailing
			if selected {
				trailing = "open ⏎"
			}
			ui.Row(c).
				Width(paletteTrailingWidth).
				Shrink(0).
				AlignItems(ui.Stretch).
				Justify(ui.End).
				FontSize(theme.SizeBody).
				TextColor(trailingColor.Color()).
				Children(func() { ui.Text(c, trailing).SingleLine() })
		})

		if selected {
			if item.failed {
				card.Background(colors.ErrorSurface.Color())
			} else {
				card.Background(colors.Selection.Color())
			}
		} else if card.Hovered() {
			card.Background(colors.Hairline.Color())
		}
		return card
	}
	return ui.Element{}
}

// The name, cut into the matched parts and the text between them. A hit
// keeps a warm wash behind it, which is the only place in the app where
// text carries a background.
func paletteSpans(c *ui.Context, item *PaletteItem, selected bool, colors *theme.ThemeColors) {
	label := item.label
	if len(item.hits) == 0 {
		ui.Text(c, label).SingleLine()
		return
	}
	var wash theme.Hsla
	switch {
	case item.failed:
		wash = colors.MatchError
	case selected:
		wash = colors.MatchStrong
	default:
		wash = colors.MatchWash
	}

	at := 0
	for _, hit := range item.hits {
		if hit.Start > at {
			ui.Text(c, label[at:hit.Start]).NoWrap().Shrink(0)
		}
		ui.Box(c).
			Shrink(0).
			Radius(2).
			Background(wash.Color()).
			Children(func() { ui.Text(c, label[hit.Start:hit.End]).NoWrap() })
		at = hit.End
	}
	// The tail is the one span allowed to truncate: everything the search
	// matched has already been painted by the time it is reached.
	ui.Text(c, label[at:]).SingleLine()
}

// Rust: `glyph`.
func paletteGlyphElement(c *ui.Context, glyph paletteGlyph, selected bool, colors *theme.ThemeColors) ui.Element {
	tone := colors.TextFaint
	if selected {
		tone = colors.Accent
	}
	switch glyph {
	case paletteGlyphView:
		return ui.Box(c).Size(5, 5).Radius(2.5).Border(1, tone.Color())
	case paletteGlyphRun:
		// The letter the history screen is reached by.
		return ui.Box(c).
			FontSize(theme.SizeMeta).
			TextColor(tone.Color()).
			Children(func() { ui.Text(c, "h") })
	default:
		return uikit.TableGlyph(c, selected)
	}
}

// PaletteScopeChip is one of the scope chips over the list. The lit chip
// wears the accent wash; the rest are bare until hovered. The caller reads
// `Clicked()` off the element it returns, as the Rust caller hung
// `on_click` on it.
func PaletteScopeChip(c *ui.Context, scope PaletteScope, active bool, colors *theme.ThemeColors) ui.Element {
	weight := 400
	textColor := colors.TextSecondary
	if active {
		weight = 500
		textColor = colors.AccentDeep
	}
	chip := ui.Row(c.Key(fmt.Sprintf("scope-%s", scope.Label()))).
		AlignItems(ui.Center).
		Gap(6).
		Padding(6, 12).
		Radius(7).
		FontSize(theme.SizeEmphasis).
		FontWeight(weight).
		TextColor(textColor.Color()).
		Cursor(ui.CursorPointer).
		Children(func() {
			ui.Text(c, scope.Label())
			if prefix, ok := scope.Prefix(); ok {
				ui.Box(c).
					TextColor(colors.TextFaint.Color()).
					Children(func() { ui.Text(c, prefix) })
			}
		})
	if active {
		chip.Background(colors.Selection.Color())
	} else if chip.Hovered() {
		chip.Background(colors.Hairline.Color())
	}
	return chip
}
