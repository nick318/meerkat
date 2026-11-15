package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nick318/meerkat/internal/fuzzy"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/storage"
)

func paletteColumn(name, dataType string) introspect.Column {
	return introspect.Column{
		Name:     name,
		DataType: dataType,
		Nullable: true,
		Default:  nil,
	}
}

func paletteTable(name string, kind introspect.TableKind, columns []introspect.Column) introspect.Table {
	approx := uint64(18_412)
	return introspect.Table{
		Name:       name,
		Kind:       kind,
		Columns:    columns,
		PrimaryKey: nil,
		ApproxRows: &approx,
	}
}

func paletteCatalog() *introspect.Catalog {
	return &introspect.Catalog{
		Schemas: []introspect.Schema{{
			Name: "public",
			Tables: []introspect.Table{
				paletteTable("users", introspect.TableKindTable, []introspect.Column{paletteColumn("id", "int8")}),
				paletteTable("user_settings", introspect.TableKindTable, nil),
				paletteTable("accoustics_test", introspect.TableKindTable, nil),
				paletteTable("active_users_7d", introspect.TableKindView, nil),
				paletteTable("orders", introspect.TableKindTable, []introspect.Column{paletteColumn("user_id", "int8")}),
			},
		}},
	}
}

// The shape that made the dotted query necessary: one table name
// repeated across many schemas.
func paletteSchemas() *introspect.Catalog {
	of := func(schema string) introspect.Schema {
		return introspect.Schema{
			Name: schema,
			Tables: []introspect.Table{
				paletteTable("task", introspect.TableKindTable, []introspect.Column{paletteColumn("name", "text")}),
				paletteTable("task_run", introspect.TableKindTable, nil),
			},
		}
	}
	return &introspect.Catalog{
		Schemas: []introspect.Schema{
			of("sample_dev_sample"),
			of("sample_big_dummy"),
			of("public"),
		},
	}
}

func paletteRun(statement string) storage.QueryRun {
	elapsed := uint64(34)
	rows := uint64(6)
	return storage.QueryRun{
		Id:        1,
		Statement: statement,
		RanAt:     time.Now().Unix(),
		Source:    storage.RunSourceUser,
		ElapsedMs: &elapsed,
		RowCount:  &rows,
		Affected:  nil,
		Error:     nil,
	}
}

func paletteLabels(results PaletteResults) []string {
	labels := []string{}
	for _, row := range results.Rows {
		switch row := row.(type) {
		case PaletteHeader:
			labels = append(labels, fmt.Sprintf("[%s %s]", row.Label, row.Count))
		case *PaletteItem:
			labels = append(labels, row.label)
		}
	}
	return labels
}

// What each hit underlines, so a test can read the highlight the way
// the row paints it.
func paletteUnderlined(item *PaletteItem) []string {
	out := []string{}
	for _, hit := range item.hits {
		out = append(out, item.label[hit.Start:hit.End])
	}
	return out
}

func paletteOnlyItem(t *testing.T, results PaletteResults) *PaletteItem {
	t.Helper()
	item, ok := results.Rows[1].(*PaletteItem)
	if !ok {
		t.Fatal("expected a result")
	}
	return item
}

func paletteToday() time.Time {
	return naiveDateOf(time.Now())
}

func paletteAssertStrings(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAHitAtTheStartOfAShortNameRanksFirst(t *testing.T) {
	results := PaletteBuild(paletteCatalog(), nil, PaletteScopeTables, "us", paletteToday())
	paletteAssertStrings(t, paletteLabels(results),
		"[TABLES · PUBLIC 4]",
		// `users` and `user_settings` are hit at the start of the
		// name, shortest first. Then `active_users_7d`, where the
		// hit starts the word `users`, and last the one where it
		// sits inside a word and starts nothing.
		"users",
		"user_settings",
		"active_users_7d",
		"accoustics_test",
	)
	if results.Matches != 4 {
		t.Fatalf("matches = %d, want 4", results.Matches)
	}
}

func TestTheMatchedPartIsReportedAsARangeOfTheLabel(t *testing.T) {
	results := PaletteBuild(paletteCatalog(), nil, PaletteScopeTables, "US", paletteToday())
	item, ok := results.Rows[3].(*PaletteItem)
	if !ok {
		t.Fatal("expected a result")
	}
	if item.label != "active_users_7d" {
		t.Fatalf("label = %q", item.label)
	}
	// Case-insensitive, and the range points at the original casing.
	paletteAssertStrings(t, paletteUnderlined(item), "us")
	if !reflect.DeepEqual(item.hits, []fuzzy.Range{{Start: 7, End: 9}}) {
		t.Fatalf("hits = %v", item.hits)
	}
}

// The case the dotted query exists for: pick one `task` out of a
// schema among many, without typing the schema out.
func TestADotNamesTheSchemaAndTheTable(t *testing.T) {
	results := PaletteBuild(
		paletteSchemas(),
		nil,
		PaletteScopeTables,
		"sample_dev_sample.task",
		paletteToday(),
	)
	paletteAssertStrings(t, paletteLabels(results),
		"[TABLES 2]",
		"sample_dev_sample.task",
		"sample_dev_sample.task_run",
	)
	// Both halves are underlined, in order and without overlapping.
	paletteAssertStrings(t, paletteUnderlined(paletteOnlyItem(t, results)), "sample_dev_sample", "task")
}

func TestEachHalfOfADottedQueryMatchesOnItsOwn(t *testing.T) {
	// Neither half has to be the whole name.
	results := PaletteBuild(paletteSchemas(), nil, PaletteScopeTables, "dev.ta", paletteToday())
	paletteAssertStrings(t, paletteLabels(results),
		"[TABLES 2]",
		"sample_dev_sample.task",
		"sample_dev_sample.task_run",
	)
	paletteAssertStrings(t, paletteUnderlined(paletteOnlyItem(t, results)), "dev", "ta")

	// A trailing dot is a schema on its own: everything it holds.
	results = PaletteBuild(paletteSchemas(), nil, PaletteScopeTables, "big_dummy.", paletteToday())
	paletteAssertStrings(t, paletteLabels(results),
		"[TABLES 2]",
		"sample_big_dummy.task",
		"sample_big_dummy.task_run",
	)

	// A leading dot names no schema, so it matches every schema.
	results = PaletteBuild(paletteSchemas(), nil, PaletteScopeTables, ".task_run", paletteToday())
	if results.Matches != 3 {
		t.Fatalf("matches = %d, want 3", results.Matches)
	}
}

// A schema name on its own is a question about that schema, so it
// answers with the tables it holds rather than with nothing.
func TestABareSchemaNameAnswersWithItsTables(t *testing.T) {
	results := PaletteBuild(
		paletteSchemas(),
		nil,
		PaletteScopeTables,
		"sample_dev_sample",
		paletteToday(),
	)
	paletteAssertStrings(t, paletteLabels(results),
		"[TABLES 2]",
		"sample_dev_sample.task",
		"sample_dev_sample.task_run",
	)
	// The schema is what matched, so the schema is what is underlined.
	paletteAssertStrings(t, paletteUnderlined(paletteOnlyItem(t, results)), "sample_dev_sample")
}

// A name the query hit directly beats one its schema matched.
func TestATableOfThatNameComesBeforeASchemaOfThatName(t *testing.T) {
	catalog := &introspect.Catalog{
		Schemas: []introspect.Schema{
			{
				Name:   "task_archive",
				Tables: []introspect.Table{paletteTable("orders", introspect.TableKindTable, nil)},
			},
			{
				Name:   "public",
				Tables: []introspect.Table{paletteTable("task", introspect.TableKindTable, nil)},
			},
		},
	}
	results := PaletteBuild(catalog, nil, PaletteScopeTables, "task", paletteToday())
	paletteAssertStrings(t, paletteLabels(results), "[TABLES 2]", "public.task", "task_archive.orders")
}

// A column name is not something the palette offers. Every row of it
// ends in "open this", and a column has nothing of its own to open —
// the way to one is ⌘J over the result that holds it.
func TestAColumnNameFindsNothing(t *testing.T) {
	results := PaletteBuild(paletteCatalog(), nil, PaletteScopeAll, "user_id", paletteToday())
	if len(results.Rows) != 0 {
		t.Fatalf("rows = %v", paletteLabels(results))
	}
	if results.Matches != 0 {
		t.Fatalf("matches = %d", results.Matches)
	}
}

func TestAQueryWithMorePartsThanTheNameMatchesNothing(t *testing.T) {
	// A relation is two parts deep, so three cannot fit it.
	results := PaletteBuild(paletteSchemas(), nil, PaletteScopeTables, "a.b.c", paletteToday())
	if len(results.Rows) != 0 {
		t.Fatalf("rows = %v", paletteLabels(results))
	}
}

func TestADottedQueryShowsTheSchemaItNamed(t *testing.T) {
	// One schema matched, so nothing forces the full path — but the
	// user typed the schema, so the rows read it back and the heading
	// stops saying it.
	//
	// `user_settings` answers too — four characters and then the `s`
	// that starts `settings` — and it ranks below both names that hold
	// `users` whole, because a name matched in one piece beats a name
	// matched in two.
	results := PaletteBuild(
		paletteCatalog(),
		nil,
		PaletteScopeTables,
		"public.users",
		paletteToday(),
	)
	paletteAssertStrings(t, paletteLabels(results),
		"[TABLES 3]",
		"public.users",
		"public.user_settings",
		"public.active_users_7d",
	)

	// Without the dot, the heading carries the schema instead.
	results = PaletteBuild(paletteCatalog(), nil, PaletteScopeTables, "orders", paletteToday())
	paletteAssertStrings(t, paletteLabels(results), "[TABLES · PUBLIC 1]", "orders")
}

func TestANameSharedByTwoSchemasIsQualified(t *testing.T) {
	results := PaletteBuild(paletteSchemas(), nil, PaletteScopeTables, "task", paletteToday())
	if got := paletteLabels(results)[0]; got != "[TABLES 6]" {
		t.Fatalf("heading = %q", got)
	}
	if got := paletteLabels(results)[1]; got != "sample_dev_sample.task" {
		t.Fatalf("first = %q", got)
	}
	// The underline follows the name, not the schema in front of it.
	paletteAssertStrings(t, paletteUnderlined(paletteOnlyItem(t, results)), "task")
}

// A statement is prose, not a path: `public.users` is how the SQL
// itself reads, so the dot is matched literally.
func TestADotInAHistoryQueryIsMatchedLiterally(t *testing.T) {
	results := PaletteBuild(
		nil,
		[]storage.QueryRun{
			paletteRun("select * from public.users"),
			paletteRun("select * from public_users"),
		},
		PaletteScopeHistory,
		"public.users",
		paletteToday(),
	)
	paletteAssertStrings(t, paletteLabels(results), "[RECENT QUERIES 1]", "select * from public.users")
	paletteAssertStrings(t, paletteUnderlined(paletteOnlyItem(t, results)), "public.users")
}

// An empty query is a starting point, not a dump of the schema: the
// relations of one schema, under a heading that names it.
func TestAnEmptyQueryListsTheRelationsAndNothingDeeper(t *testing.T) {
	results := PaletteBuild(paletteCatalog(), nil, PaletteScopeAll, "", paletteToday())
	for _, label := range paletteLabels(results) {
		if strings.Contains(label, ".") {
			t.Fatalf("label %q holds a dot", label)
		}
	}
	found := false
	for _, label := range paletteLabels(results) {
		if label == "[TABLES · PUBLIC 5]" {
			found = true
		}
	}
	if !found {
		t.Fatalf("labels = %q", paletteLabels(results))
	}
}

func TestASectionThatFoundNothingLeavesNoHeading(t *testing.T) {
	results := PaletteBuild(paletteCatalog(), nil, PaletteScopeAll, "zzz", paletteToday())
	if len(results.Rows) != 0 {
		t.Fatalf("rows = %v", paletteLabels(results))
	}
	if results.Matches != 0 {
		t.Fatalf("matches = %d", results.Matches)
	}
}

func TestACappedSectionSaysHowManyItLeftOut(t *testing.T) {
	schema := introspect.Schema{
		Name:   "public",
		Tables: nil,
	}
	for ix := 0; ix < 9; ix++ {
		schema.Tables = append(schema.Tables, paletteTable(fmt.Sprintf("log_%d", ix), introspect.TableKindTable, nil))
	}
	results := PaletteBuild(
		&introspect.Catalog{
			Schemas: []introspect.Schema{schema},
		},
		nil,
		PaletteScopeAll,
		"log",
		paletteToday(),
	)
	if got := paletteLabels(results)[0]; got != "[TABLES · PUBLIC 6 of 9]" {
		t.Fatalf("heading = %q", got)
	}
	if len(results.Rows) != 1+paletteSectionCap {
		t.Fatalf("rows = %d", len(results.Rows))
	}
	// The count in the footer is what matched, not what fitted.
	if results.Matches != 9 {
		t.Fatalf("matches = %d", results.Matches)
	}
}

// The sidebar carries the row estimate. In a list of near-identical
// names it only makes one row louder than the rest.
func TestATableDoesNotCarryItsRowCount(t *testing.T) {
	results := PaletteBuild(paletteCatalog(), nil, PaletteScopeTables, "users", paletteToday())
	item := paletteOnlyItem(t, results)
	if item.label != "users" {
		t.Fatalf("label = %q", item.label)
	}
	if item.meta != "" {
		t.Fatalf("meta = %q", item.meta)
	}

	// A view still says that it is one: that is what it is, not how
	// big it is.
	results = PaletteBuild(paletteCatalog(), nil, PaletteScopeTables, "7d", paletteToday())
	if got := paletteOnlyItem(t, results).meta; got != "view" {
		t.Fatalf("meta = %q", got)
	}
}

func TestARunReadsAsOneLine(t *testing.T) {
	results := PaletteBuild(
		nil,
		[]storage.QueryRun{paletteRun("select *\n  from public.users\n  order by id")},
		PaletteScopeHistory,
		"order by",
		paletteToday(),
	)
	item := paletteOnlyItem(t, results)
	if item.label != "select * from public.users order by id" {
		t.Fatalf("label = %q", item.label)
	}
	if !strings.HasPrefix(item.trailing, "today ") {
		t.Fatalf("trailing = %q", item.trailing)
	}
	// Opening a run gives back the statement as it was written, not
	// the single line the palette painted.
	want := PalettePick{Kind: PalettePickQuery, Statement: "select *\n  from public.users\n  order by id"}
	if item.pick != want {
		t.Fatalf("pick = %+v", item.pick)
	}
}

// ⇥ walks in one part at a time: schema, then relation, then done.
func TestCompletingDrillsOnePartAtATime(t *testing.T) {
	step := func(needle string) (string, bool) {
		results := PaletteBuild(paletteSchemas(), nil, PaletteScopeTables, needle, paletteToday())
		selected := -1
		for ix, row := range results.Rows {
			if _, ok := row.Pick(); ok {
				selected = ix
				break
			}
		}
		if selected < 0 {
			t.Fatal("a result")
		}
		return PaletteCompletion(results.Rows, selected, needle)
	}

	// A hit in the middle of a schema still completes to the whole of
	// it, with the dot that leads into the relation.
	if got, ok := step("dev"); !ok || got != "sample_dev_sample." {
		t.Fatalf("dev → %q, %v", got, ok)
	}
	if got, ok := step("sample_dev_sample."); !ok || got != "sample_dev_sample.task" {
		t.Fatalf("sample_dev_sample. → %q, %v", got, ok)
	}
	// A path that is already whole has nothing left to add, which is
	// what frees ⇥ to go back to walking the chips.
	if got, ok := step("sample_dev_sample.task"); ok {
		t.Fatalf("sample_dev_sample.task → %q", got)
	}
}

func TestAPathCompletesTheSameWayOffABarePath(t *testing.T) {
	// What the sidebar's filter asks for: no rows, just the path of
	// the first match. It walks in the same steps as the palette.
	path := []string{"sample_dev_sample", "task"}
	if got, ok := PaletteCompletePath(path, "dev"); !ok || got != "sample_dev_sample." {
		t.Fatalf("dev → %q, %v", got, ok)
	}
	if got, ok := PaletteCompletePath(path, "sample_dev_sample."); !ok || got != "sample_dev_sample.task" {
		t.Fatalf("sample_dev_sample. → %q, %v", got, ok)
	}
	// A hit on the relation completes that name and stops there.
	if got, ok := PaletteCompletePath(path, "as"); !ok || got != "task" {
		t.Fatalf("as → %q, %v", got, ok)
	}
	// Nothing left to finish, and nothing to finish from.
	if got, ok := PaletteCompletePath(path, "sample_dev_sample.task"); ok {
		t.Fatalf("whole → %q", got)
	}
	if got, ok := PaletteCompletePath(path, ""); ok {
		t.Fatalf("empty → %q", got)
	}
	// A query the path does not answer completes nothing.
	if got, ok := PaletteCompletePath(path, "public.users"); ok {
		t.Fatalf("public.users → %q", got)
	}
}

func TestAHintThatCarriesOnIsASuffixAndTheRestAreMarked(t *testing.T) {
	// The line reads as one name, so the hint is the rest of it.
	if got := PaletteGhost("sample_dev_", "sample_dev_sample."); got != "sample." {
		t.Fatalf("got %q", got)
	}
	// `sam_dev` finds the schema by its word starts, so ⇥ replaces the
	// line. Painting the completion as a suffix would read
	// `sam_devsample_dev_sample.`, so it is marked as what it is.
	if got := PaletteGhost("sam_dev", "sample_dev_sample."); got != "  ⇥ sample_dev_sample." {
		t.Fatalf("got %q", got)
	}
	// The whole line typed out already: nothing carries on from it.
	if got := PaletteGhost("sample_dev_sample.", "sample_dev_sample.task"); got != "task" {
		t.Fatalf("got %q", got)
	}
}

func TestThereIsNothingToCompleteFromARunOrAnEmptyLine(t *testing.T) {
	results := PaletteBuild(
		nil,
		[]storage.QueryRun{paletteRun("select * from users")},
		PaletteScopeHistory,
		"users",
		paletteToday(),
	)
	// A statement is prose, not a path.
	if got, ok := PaletteCompletion(results.Rows, 1, "users"); ok {
		t.Fatalf("run → %q", got)
	}

	// An empty line has a selected row but nothing to finish.
	results = PaletteBuild(paletteSchemas(), nil, PaletteScopeTables, "", paletteToday())
	if got, ok := PaletteCompletion(results.Rows, 1, ""); ok {
		t.Fatalf("empty → %q", got)
	}

	// A heading is not a result.
	if got, ok := PaletteCompletion(results.Rows, 0, "task"); ok {
		t.Fatalf("heading → %q", got)
	}
}

func TestATypedPrefixPicksTheScopeOverTheChip(t *testing.T) {
	check := func(query string, chip PaletteScope, wantScope PaletteScope, wantRest string) {
		t.Helper()
		scope, rest := PaletteParse(query, chip)
		if scope != wantScope || rest != wantRest {
			t.Fatalf("PaletteParse(%q, %v) = (%v, %q), want (%v, %q)", query, chip, scope, rest, wantScope, wantRest)
		}
	}
	check("t: user", PaletteScopeHistory, PaletteScopeTables, "user")
	check("h:select", PaletteScopeAll, PaletteScopeHistory, "select")
	// `c:` was the columns scope and is not a prefix any more, so it
	// is searched for as text.
	check("c:id", PaletteScopeTables, PaletteScopeTables, "c:id")
	check("users", PaletteScopeTables, PaletteScopeTables, "users")
	check("  users", PaletteScopeAll, PaletteScopeAll, "users")
}

func TestALongLabelKeepsItsHitInView(t *testing.T) {
	long := fmt.Sprintf(
		"select %s from users where name = 'meerkat'",
		strings.Repeat("a, ", 30),
	)
	var hits []fuzzy.Range
	if hit, ok := paletteFind(long, "meerkat"); ok {
		hits = append(hits, hit)
	}
	text, hits := paletteClip(long, hits, paletteLabelChars)
	if !strings.HasPrefix(text, "…") {
		t.Fatalf("text = %q", text)
	}
	if utf8.RuneCountInString(text) > paletteLabelChars+2 {
		t.Fatalf("text runs %d chars", utf8.RuneCountInString(text))
	}
	if len(hits) == 0 {
		t.Fatal("the hit is what the window was placed around")
	}
	hit := hits[0]
	if got := text[hit.Start:hit.End]; got != "meerkat" {
		t.Fatalf("hit = %q", got)
	}
}

func TestAShortLabelIsLeftAlone(t *testing.T) {
	text, hits := paletteClip("users", []fuzzy.Range{{Start: 1, End: 4}}, paletteLabelChars)
	if text != "users" {
		t.Fatalf("text = %q", text)
	}
	if !reflect.DeepEqual(hits, []fuzzy.Range{{Start: 1, End: 4}}) {
		t.Fatalf("hits = %v", hits)
	}
}
