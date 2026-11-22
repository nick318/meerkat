// The shell's pure policy: every free function of `shell.rs` that paints
// nothing, in the order the Rust file has them. Each is a function of
// plain values, so it is argued with in shell_test.go rather than in a
// running window.

package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/query"
	"github.com/nick318/meerkat/internal/sqleditor"
	"github.com/nick318/meerkat/internal/uikit"
)

// stepStop is where one press of ↑ or ↓ lands in the sidebar's filtered
// list. `stops` are the rows a cursor may sit on, and `from` is where it
// sits now.
//
// **The line the user is typing on is a position in this ring**, and it
// is `None`. ↑ off the first name goes back to it and ↓ off the last name
// comes round to it, so the way out of the list is the key that walked
// into it. A cursor that could only be escaped by deleting the line would
// be a trap, and the line is where the next keystroke has to land: the
// user is still typing a name.
func stepStop(stops []int, from *int, forward bool) (int, bool) {
	if len(stops) == 0 {
		return 0, false
	}
	here := -1
	if from != nil {
		for i, stop := range stops {
			if stop == *from {
				here = i
				break
			}
		}
	}
	switch {
	case here < 0 && forward:
		return stops[0], true
	case here < 0 && !forward:
		return stops[len(stops)-1], true
	case forward:
		if here+1 < len(stops) {
			return stops[here+1], true
		}
		return 0, false
	case here == 0:
		return 0, false
	default:
		return stops[here-1], true
	}
}

// stepWrapping is where one step lands. It wraps at both ends, because the
// tabs are a ring: one press past the last tab is how the user comes back
// to the first.
func stepWrapping(len, from int, forward bool) int {
	if len == 0 {
		return 0
	}
	if forward {
		return (from + 1) % len
	}
	return (from + len - 1) % len
}

// scopeOf is which connection a session's history belongs to. A saved
// connection has an id already; a URL from the command line is keyed by
// the URL with its password taken out, so the history file never holds
// one.
func scopeOf(target Target) string {
	switch target := target.(type) {
	case TargetProfile:
		return target.Profile.Id
	case TargetUrl:
		return "url:" + redact(target.Url)
	}
	panic("shell: unknown Target")
}

// runScope says when a run would send only part of the buffer.
func runScope(tab *QueryTab) (string, bool) {
	if _, ok := tab.editor.SelectedText(); ok {
		return "Runs the selection", true
	}
	return "", false
}

// vocabularyOf is every schema, relation and column in the catalog, so the
// query editor can colour the names the database really has and complete
// them as the user types.
//
// Ownership is what makes `users.` offer that table's columns: a column
// is owned by its relation, a relation by its schema.
func vocabularyOf(catalog *introspect.Catalog) *sqleditor.Vocabulary {
	var names []sqleditor.Name
	for si := range catalog.Schemas {
		schema := &catalog.Schemas[si]
		names = append(names, sqleditor.Name{
			Name:   schema.Name,
			Detail: "schema",
			Kind:   sqleditor.KindSchema,
			Owner:  nil,
		})
		for ti := range schema.Tables {
			table := &schema.Tables[ti]
			detail := "table"
			if table.Kind == introspect.TableKindView {
				detail = "view"
			}
			schemaName := schema.Name
			names = append(names, sqleditor.Name{
				Name:   table.Name,
				Detail: detail,
				Kind:   sqleditor.KindRelation,
				Owner:  &schemaName,
			})
			for _, column := range table.Columns {
				tableName := table.Name
				names = append(names, sqleditor.Name{
					Name: column.Name,
					// The declared type, as the design's panel shows it.
					Detail: column.DataType,
					Kind:   sqleditor.KindColumn,
					Owner:  &tableName,
				})
			}
		}
	}
	return sqleditor.NewVocabulary(names)
}

// statementMeta is the statistic beside one statement that landed: what
// it answered with, and what it took. The comp's own lines — `6 rows · 34
// ms`, `1,204 rows deleted · 61 ms`, `altered · 12 ms` — and the rule is
// the result line's: **columns say there was a result set, never the
// count**, so a `SELECT` that matched nothing reads `no rows` and an
// `UPDATE` that matched nothing reads `no rows updated`. A shape-changing
// statement takes its verb alone; the panel under the editor says what it
// changed once the catalog has been read.
func statementMeta(verb *query.DdlVerb, statement string, result *dbclient.QueryResult, ms uint64) string {
	took := uikit.FormatMillis(ms)
	if verb != nil {
		return fmt.Sprintf("%s · %s", verb.Past(), took)
	}
	if len(result.Columns) != 0 {
		switch n := len(result.Rows); n {
		case 0:
			return fmt.Sprintf("no rows · %s", took)
		case 1:
			return fmt.Sprintf("1 row · %s", took)
		default:
			return fmt.Sprintf("%s rows · %s", uikit.FormatCount(uint64(n)), took)
		}
	}
	if command, ok := query.CommandVerbOf(statement); ok {
		return fmt.Sprintf("%s · %s", changedCopy(Changed{
			rows: result.RowsAffected,
			verb: &command,
		}), took)
	}
	switch n := result.RowsAffected; n {
	// No result set, no verb, no count: a `SET`, a `BEGIN` the user
	// typed, a `VACUUM`. "done" is all the server said.
	case 0:
		return fmt.Sprintf("done · %s", took)
	case 1:
		return fmt.Sprintf("1 row affected · %s", took)
	default:
		return fmt.Sprintf("%s rows affected · %s", uikit.FormatCount(n), took)
	}
}

// ddlMeta is the statistic beside a shape-changing statement once the
// catalog has been read back, when the run held exactly one and the
// report can be laid at its door: `altered · +2 −1 columns · 12 ms`.
// `None` leaves the line as it was — nothing the model carries changed,
// or nothing worth counting did.
func ddlMeta(delta *introspect.Delta, verb query.DdlVerb, ms uint64) (string, bool) {
	added, dropped, changed := 0, 0, 0
	for _, relation := range delta.Relations {
		if altered, ok := relation.Change.(introspect.RelationAltered); ok {
			for _, column := range altered.Columns {
				switch column.(type) {
				case introspect.ColumnAdded:
					added++
				case introspect.ColumnDropped:
					dropped++
				case introspect.ColumnChanged:
					changed++
				}
			}
		}
	}
	var parts []string
	if added > 0 {
		parts = append(parts, fmt.Sprintf("+%d", added))
	}
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("−%d", dropped))
	}
	if changed > 0 {
		parts = append(parts, fmt.Sprintf("~%d", changed))
	}
	if len(parts) == 0 {
		// A table that came or went is the header's news; whatever came
		// or went with it is not the statement's either.
		if len(delta.Relations) != 0 {
			return "", false
		}
		return objectMeta(delta.Objects, verb, ms)
	}
	noun := "columns"
	if added+dropped+changed == 1 {
		noun = "column"
	}
	return fmt.Sprintf("%s · %s %s · %s", verb.Past(), strings.Join(parts, " "), noun, uikit.FormatMillis(ms)), true
}

// objectMeta is the statistic for a statement that moved no column but did
// make or drop something else: `created · +1 index · 12 ms`. The noun is
// the object's own when they are all of one sort, which a `CREATE INDEX`
// or a `DROP FUNCTION` always is, and `objects` when a script mixed them.
func objectMeta(objects []introspect.ObjectDelta, verb query.DdlVerb, ms uint64) (string, bool) {
	if len(objects) == 0 {
		return "", false
	}
	first := &objects[0]
	added, dropped, changed := 0, 0, 0
	for _, object := range objects {
		switch object.Change.(type) {
		case introspect.ObjectAdded:
			added++
		case introspect.ObjectDropped:
			dropped++
		case introspect.ObjectChanged:
			changed++
		}
	}
	var parts []string
	if added > 0 {
		parts = append(parts, fmt.Sprintf("+%d", added))
	}
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("−%d", dropped))
	}
	if changed > 0 {
		parts = append(parts, fmt.Sprintf("~%d", changed))
	}
	oneKind := true
	for _, object := range objects {
		if object.Kind != first.Kind {
			oneKind = false
			break
		}
	}
	var noun string
	switch {
	case oneKind && len(objects) == 1:
		noun = first.Kind.Noun()
	case oneKind:
		noun = first.Kind.Plural()
	default:
		noun = "objects"
	}
	return fmt.Sprintf("%s · %s %s · %s", verb.Past(), strings.Join(parts, " "), noun, uikit.FormatMillis(ms)), true
}

// schemaNote is the line under the schema report, which is the one thing
// about a shape change worth saying in a sentence: whether it can still be
// taken back. Inside an open transaction it can, and the bar's two buttons
// are the way; committed, a drop is gone from here. `dropped` are the
// names the delta removed, qualified the way the sidebar shows them.
func schemaNote(held bool, dropped []string) string {
	switch {
	case held && len(dropped) == 0:
		return "held in the open transaction — rollback takes it back, commit makes it permanent"
	case held && len(dropped) == 1:
		return fmt.Sprintf("held in the open transaction — rollback puts %s back, commit makes the drop permanent", dropped[0])
	case held:
		return "held in the open transaction — rollback puts them back, commit makes the drops permanent"
	case len(dropped) == 0:
		return "committed on its own"
	case len(dropped) == 1:
		return fmt.Sprintf("committed on its own — %s is gone and cannot be recovered from here", dropped[0])
	default:
		return fmt.Sprintf("committed on its own — %s are gone and cannot be recovered from here", strings.Join(dropped, ", "))
	}
}

// notConnectedCopy is what a tab says when it wanted the server and there
// was none.
//
// **It repeats the reason.** A bare "not connected" under a connect that
// failed sends the user looking for what the strip over the pane already
// knows, so the line carries the class and the server's own words; while
// the connect is still out it says where it is going, because "yet" with
// no host is a wait with nothing to check.
func notConnectedCopy(status Status) string {
	switch status := status.(type) {
	case StatusFailed:
		return fmt.Sprintf("Not connected · %s · %s", status.Error.Kind.Headline(), status.Error.Detail)
	case StatusConnecting:
		return fmt.Sprintf("Not connected yet · still connecting to %s", status.To)
	case StatusConnected:
		return notConnected
	}
	panic("shell: unknown Status")
}

// windowCornerRadius is the radius macOS masks the window's bottom corners
// to. The environment frame curves to the same radius, or the mask would
// cut its corners off. Tahoe (Darwin 25, macOS 26) rounds windows to
// 16pt; the versions before it used 10pt. There is no public API for
// the value, so it is read off the OS version once.
func windowCornerRadius() float32 {
	if runtime.GOOS != "darwin" {
		return 0.
	}
	return windowCornerRadiusOnce()
}

var windowCornerRadiusOnce = sync.OnceValue(func() float32 {
	out, _ := exec.Command("uname", "-r").Output()
	release := string(out)
	head, _, _ := strings.Cut(release, ".")
	major, err := strconv.ParseUint(strings.TrimSpace(head), 10, 32)
	if err != nil {
		major = 0
	}
	if major >= 25 {
		return 16.
	}
	return 10.
})

// endpoint is where the connect is going, as `host:port` — the two things
// a person checks first when it does not arrive. A profile with neither
// set means the driver's own defaults, so those are what is said. A URL is
// cut down to its authority, which also leaves the user and the password
// out.
func endpoint(target Target) string {
	switch target := target.(type) {
	case TargetProfile:
		host := "localhost"
		if target.Profile.Host != nil {
			host = *target.Profile.Host
		}
		port := uint16(5432)
		if target.Profile.Port != nil {
			port = *target.Profile.Port
		}
		return fmt.Sprintf("%s:%d", host, port)
	case TargetUrl:
		url := target.Url
		rest := url
		if _, after, found := strings.Cut(url, "://"); found {
			rest = after
		}
		authority := rest
		if ix := strings.IndexAny(rest, "/?"); ix >= 0 {
			authority = rest[:ix]
		}
		host := authority
		if ix := strings.LastIndex(authority, "@"); ix >= 0 {
			host = authority[ix+1:]
		}
		switch {
		case host == "":
			return "localhost:5432"
		// A bracketed IPv6 address carries colons of its own.
		case strings.HasSuffix(host, "]") || !strings.Contains(host, ":"):
			return host + ":5432"
		default:
			return host
		}
	}
	panic("shell: unknown Target")
}

// connectionName is the name the session goes by. A profile's own name,
// empty names aside; a command-line URL has none, and the database name
// stands in for it.
func connectionName(target Target) *string {
	if profile, ok := target.(TargetProfile); ok && strings.TrimFunc(profile.Profile.Name, unicode.IsSpace) != "" {
		name := profile.Profile.Name
		return &name
	}
	return nil
}

// catalogGroups reads the catalog the way the sidebar reads it: a schema,
// then its sections under it — `Tables`, `Views`, `Sequences`,
// `Routines`, `Types` — then the names under those. A section with
// nothing in it is left out rather than drawn empty, and so is a schema.
func catalogGroups(catalog *introspect.Catalog) []Group {
	var groups []Group
	for schemaIx := range catalog.Schemas {
		schema := &catalog.Schemas[schemaIx]
		relations := func(kind introspect.TableKind) []Entry {
			var entries []Entry
			for tableIx := range schema.Tables {
				table := &schema.Tables[tableIx]
				if table.Kind != kind {
					continue
				}
				entries = append(entries, &EntryRelation{
					name:    table.Name,
					kind:    kind,
					key:     relationKey(schema.Name, table.Name),
					tableIx: tableIx,
					parts:   nil,
				})
			}
			return entries
		}
		objects := func(leaves []Leaf) []Entry {
			var entries []Entry
			for _, leaf := range leaves {
				entries = append(entries, EntryObject{leaf: leaf})
			}
			return entries
		}
		var sequences, routines, types []Leaf
		for i := range schema.Sequences {
			s := &schema.Sequences[i]
			sequences = append(sequences, newLeaf(s.Name, s.Detail(), MarkSequence))
		}
		for i := range schema.Routines {
			r := &schema.Routines[i]
			routines = append(routines, newLeaf(r.Name, r.Detail(), MarkRoutine))
		}
		for i := range schema.Types {
			t := &schema.Types[i]
			types = append(types, newLeaf(t.Name, t.Detail(), MarkType))
		}
		candidates := []struct {
			label   string
			entries []Entry
		}{
			{"Tables", relations(introspect.TableKindTable)},
			{"Views", relations(introspect.TableKindView)},
			{"Sequences", objects(sequences)},
			{"Routines", objects(routines)},
			{"Types", objects(types)},
		}
		var sections []Section
		for _, candidate := range candidates {
			if len(candidate.entries) == 0 {
				continue
			}
			sections = append(sections, Section{
				key:     schema.Name + "\t" + candidate.label,
				label:   candidate.label,
				entries: candidate.entries,
			})
		}
		if len(sections) == 0 {
			continue
		}
		groups = append(groups, Group{
			key:      schema.Name,
			label:    schema.Name,
			schema:   schema.Name,
			sections: sections,
			catalog:  catalog,
			schemaIx: schemaIx,
		})
	}
	return groups
}

// catalogStops is the rows ↑↓ stop on: the relations, by their index into
// the list. A header opens and closes and a leaf opens nothing, so neither
// is a stop.
func catalogStops(rows []CatalogRow) []int {
	var stops []int
	for ix, row := range rows {
		if _, ok := row.(CatalogRowRelation); ok {
			stops = append(stops, ix)
		}
	}
	return stops
}

// relationKey is the identity of a relation in the open set. A schema name
// cannot hold a tab, so no two relations share one.
func relationKey(schema, name string) string {
	return schema + "\t" + name
}

// relationParts is the columns and folders a relation folds out into. A
// folder with nothing in it is left out, as an empty section is.
func relationParts(table *introspect.Table) *Parts {
	var columns []Leaf
	for _, column := range table.Columns {
		mark := MarkColumn
		if table.IsKey(column.Name) {
			mark = MarkKey
		}
		columns = append(columns, newLeaf(column.Name, columnDetail(&column), mark))
	}
	var indexes, keys, constraints, triggers, partitions []Leaf
	for i := range table.Indexes {
		ix := &table.Indexes[i]
		indexes = append(indexes, newLeaf(ix.Name, ix.Detail(), MarkIndex))
	}
	for i := range table.ForeignKeys {
		k := &table.ForeignKeys[i]
		keys = append(keys, newLeaf(k.Name, k.Detail(), MarkForeignKey))
	}
	for i := range table.Constraints {
		c := &table.Constraints[i]
		constraints = append(constraints, newLeaf(c.Name, c.Detail(), MarkConstraint))
	}
	for i := range table.Triggers {
		t := &table.Triggers[i]
		triggers = append(triggers, newLeaf(t.Name, t.Detail(), MarkTrigger))
	}
	for _, p := range table.Partitions {
		partitions = append(partitions, newLeaf(p.Name, p.Bound, MarkPartition))
	}
	var folders []Folder
	for _, folder := range []Folder{
		{"Partitions", partitions},
		{"Indexes", indexes},
		{"Foreign keys", keys},
		{"Constraints", constraints},
		{"Triggers", triggers},
	} {
		if len(folder.leaves) != 0 {
			folders = append(folders, folder)
		}
	}
	return &Parts{columns: columns, folders: folders}
}

// catalogRows flattens the tree for `uniform_list`: a schema row, its
// section rows while it is open, and their entries while they are — and
// under a relation the user folded out, its columns and its folders.
//
// The levels remember themselves in two ways. A schema is closed until
// `open_schemas` holds it, because a catalog of thousands of relations is
// a wall of names otherwise; a section is open until `closed_sections`
// holds it, because a schema the user just opened was opened to see what
// is in it. A relation is closed until `open_relations` holds it, for the
// schema's reason: a schema of forty tables folded out would be a
// schema of four hundred columns. A relation's folders are sections, and
// open until closed like the rest.
//
// A `needle` narrows the list to the names it matches, by the same rule
// the palette uses: the word-start rule, and **a dot names a path**. So
// `address` finds every `address`, `dev.addr` finds the one in
// `sample_dev_sample`, and a bare schema name answers with everything
// under it. Whatever is left with nothing under it is dropped, header and
// all. While the filter is on, every schema and section that survived is
// drawn open whatever the sets say — a search that needs a second click
// to show its hits is not a search. A relation is not: the hit is its
// name, and folding every hit out would bury the next one.
//
// **The hits are ranked, and only while the filter is on.** The catalog's
// own order is alphabetical, which puts `master` in the middle of the
// thirteen names that hold the word, and a list whose best answer is
// eighth is one the user reads before they can use it. So a filtered
// section is sorted by `palette::path_rank` — the palette's own order, so
// the same query offers the same name first in both places — and ↑↓ walk
// it best-first. Unfiltered, nothing is ranked: with no query there is
// nothing to be closest to, and shuffling a schema's tables on every
// keystroke of an emptying line would be worse than alphabetical.
func catalogRows(
	groups []Group,
	openSchemas map[string]struct{},
	closedSections map[string]struct{},
	openRelations map[string]struct{},
	needle string,
) []CatalogRow {
	filtering := needle != ""
	// Compiled once for the whole catalog, not once per name.
	var query *palettePathQuery
	if filtering {
		query = newPalettePathQuery(needle)
	}
	var rows []CatalogRow
	for gi := range groups {
		group := &groups[gi]
		type found struct {
			section *Section
			matches []Entry
		}
		var hits []found
		for si := range group.sections {
			section := &group.sections[si]
			var matches []Entry
			if !filtering {
				matches = section.entries
			} else {
				type rankedEntry struct {
					rank  PaletteRank
					entry Entry
				}
				var ranked []rankedEntry
				for _, entry := range section.entries {
					path := [2]string{group.schema, entry.Name()}
					if rank, ok := query.rank(path[:]); ok {
						ranked = append(ranked, rankedEntry{rank, entry})
					}
				}
				// A stable sort, so two names the query cannot tell
				// apart stay in the catalog's own order.
				sort.SliceStable(ranked, func(a, b int) bool {
					return ranked[a].rank.Less(ranked[b].rank)
				})
				for _, r := range ranked {
					matches = append(matches, r.entry)
				}
			}
			if len(matches) != 0 {
				hits = append(hits, found{section, matches})
			}
		}
		count := 0
		for _, hit := range hits {
			count += len(hit.matches)
		}
		if count == 0 {
			continue
		}

		_, schemaOpen := openSchemas[group.key]
		open := filtering || schemaOpen
		rows = append(rows, CatalogRowSchema{
			key:   group.key,
			label: group.label,
			count: count,
			open:  open,
		})
		if !open {
			continue
		}
		for _, hit := range hits {
			_, closed := closedSections[hit.section.key]
			open := filtering || !closed
			rows = append(rows, CatalogRowSection{
				key:   hit.section.key,
				label: hit.section.label,
				count: len(hit.matches),
				open:  open,
				level: 1,
			})
			if !open {
				continue
			}
			for _, entry := range hit.matches {
				switch entry := entry.(type) {
				case *EntryRelation:
					_, open := openRelations[entry.key]
					rows = append(rows, CatalogRowRelation{
						schema: group.schema,
						name:   entry.name,
						kind:   entry.kind,
						open:   open,
					})
					if open {
						if entry.parts == nil {
							entry.parts = relationParts(&group.catalog.Schemas[group.schemaIx].Tables[entry.tableIx])
						}
						parts := entry.parts
						for _, leaf := range parts.columns {
							rows = append(rows, leaf.row(3))
						}
						for _, folder := range parts.folders {
							key := entry.key + "\t" + folder.label
							_, closed := closedSections[key]
							open := !closed
							rows = append(rows, CatalogRowSection{
								key:   key,
								label: folder.label,
								count: len(folder.leaves),
								open:  open,
								level: 3,
							})
							if open {
								for _, leaf := range folder.leaves {
									rows = append(rows, leaf.row(4))
								}
							}
						}
					}
				case EntryObject:
					rows = append(rows, entry.leaf.row(2))
				}
			}
		}
	}
	return rows
}

// clampToBoundary pulls an offset onto a character boundary of `text`, at
// or before it.
func clampToBoundary(text string, offset int) int {
	offset = min(offset, len(text))
	for offset > 0 && offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset--
	}
	return offset
}

// isAlphanumeric is Rust's `char::is_alphanumeric`: Alphabetic (L, Nl,
// Other_Alphabetic) or Numeric (Nd, Nl, No).
func isAlphanumeric(ch rune) bool {
	return unicode.In(ch, unicode.L, unicode.Nl, unicode.Other_Alphabetic, unicode.Nd, unicode.No)
}

// wordAt is the word under `offset`, for a double click.
//
// A word here is a run of letters, digits and underscores, which is what
// a name, an identifier or a UUID's parts are made of. Land anywhere
// else — a comma, a brace, a space — and the mark is that one character:
// the click said "this", and guessing a wider run around punctuation
// marks text the user did not point at.
func wordAt(text string, offset int) query.Range {
	offset = clampToBoundary(text, offset)
	word := func(c rune) bool { return isAlphanumeric(c) || c == '_' }
	if offset >= len(text) {
		return query.Range{Start: offset, End: offset}
	}
	here, size := utf8.DecodeRuneInString(text[offset:])
	if !word(here) {
		return query.Range{Start: offset, End: offset + size}
	}
	start := offset
	for start > 0 {
		c, size := utf8.DecodeLastRuneInString(text[:start])
		if !word(c) {
			break
		}
		start -= size
	}
	end := len(text)
	for ix, c := range text[offset:] {
		if !word(c) {
			end = offset + ix
			break
		}
	}
	return query.Range{Start: start, End: end}
}

// lineAt is the line under `offset`, for a triple click. The newline
// itself is left out: a mark that carried it would paste as a line break
// the user cannot see they copied.
func lineAt(text string, offset int) query.Range {
	offset = clampToBoundary(text, offset)
	start := 0
	if ix := strings.LastIndexByte(text[:offset], '\n'); ix >= 0 {
		start = ix + 1
	}
	end := len(text)
	if ix := strings.IndexByte(text[offset:], '\n'); ix >= 0 {
		end = offset + ix
	}
	return query.Range{Start: start, End: end}
}

// confirmCopy is what the close confirmation says; see ConfirmCopy.
func confirmCopy(what Close, running []string, open []string, staged ...StagedTab) ConfirmCopy {
	var title, verb, keep, act string
	switch what.Kind {
	case CloseKindTab:
		first := ""
		if len(running) != 0 {
			first = running[0]
		} else if len(open) != 0 {
			first = open[0]
		} else if len(staged) != 0 {
			first = staged[0].name
		}
		title, verb, keep, act = fmt.Sprintf("Close “%s”?", first), "Closing the tab", "Keep tab", "Close it"
	case CloseKindShell:
		title, verb, keep, act = "Leave this connection?", "Leaving", "Stay", "Leave anyway"
	// Two windows, two questions. Closing one window ends what that
	// window is doing; ⌘Q ends every window's, so it is the one that
	// says "quit".
	case CloseKindWindow:
		title, verb, keep, act = "Close this window?", "Closing the window", "Stay", "Close anyway"
	case CloseKindQuit:
		title, verb, keep, act = "Quit Meerkat?", "Quitting", "Stay", "Quit anyway"
	}

	// One line per thing at stake, because they are lost in different
	// ways: a run the server can be asked to give up, a transaction it
	// throws away. A close that said only "are you sure" would be telling
	// the user nothing they could not already see.
	var body []string
	if len(running) != 0 {
		if len(running) == 1 {
			body = append(body, fmt.Sprintf("“%s” is still running a statement. %s asks the server to stop it.", running[0], verb))
		} else {
			body = append(body, fmt.Sprintf("%d tabs are still running statements. %s asks the server to stop them.", len(running), verb))
		}
	}
	if len(open) != 0 {
		// "rolls it back" and nothing more dramatic: the session is
		// read-only unless the user said otherwise, so a rollback often
		// costs nothing but the transaction itself. Saying the work is
		// lost would be a warning the app cannot always stand behind.
		if len(open) == 1 {
			body = append(body, fmt.Sprintf("“%s” has a transaction open. %s rolls it back.", open[0], verb))
		} else {
			body = append(body, fmt.Sprintf("%d tabs have transactions open. %s rolls them back.", len(open), verb))
		}
	}
	// Staged changes are lost in a third way: nothing was sent, so the
	// server loses nothing, and the user loses what they typed.
	if len(staged) == 1 {
		body = append(body, fmt.Sprintf("“%s” has %d %s not applied. They live only in this tab; %s throws them away.",
			staged[0].name, staged[0].changes, plural(staged[0].changes, "change", "changes"), strings.ToLower(verb[:1])+verb[1:]))
	} else if len(staged) > 1 {
		body = append(body, fmt.Sprintf("%d tabs have changes not applied. %s throws them away.", len(staged), verb))
	}
	return ConfirmCopy{Title: title, Body: body, Keep: keep, Act: act}
}

// spare says whether a session may be taken back at all.
//
// A run in flight is using it. **A transaction is the one that must never
// be taken**: handing the connection back rolls the user's work away with
// nobody asking, which is exactly what the close dialog exists to refuse
// to do quietly. So a tab mid-transaction keeps its connection however
// long it sits there, and the cap is what gives way instead.
func spare(state *SessionState) bool {
	return !state.running && !state.inTransaction
}

// idleSessions is the sessions nobody has used for `after`.
//
// A laptop left open overnight must not hold every connection against a
// shared server for a window nobody is looking at. The cost of being
// wrong is small and visible: the tab says the session ended, and its
// next run opens another.
func idleSessions(states []SessionState, after time.Duration) []uint64 {
	ids := []uint64{}
	for i := range states {
		state := &states[i]
		if spare(state) && state.idle >= after {
			ids = append(ids, state.tabID)
		}
	}
	return ids
}

// evictable is which session to give back when one more is wanted: the
// one that has gone longest without a question. `None` means none may be
// taken, and the caller must refuse rather than pick a session that is
// holding something.
func evictable(states []SessionState) (uint64, bool) {
	var best *SessionState
	for i := range states {
		state := &states[i]
		if !spare(state) {
			continue
		}
		// `max_by_key` keeps the last of equal maxima.
		if best == nil || state.idle >= best.idle {
			best = state
		}
	}
	if best == nil {
		return 0, false
	}
	return best.tabID, true
}

// needsBegin says whether a run has to open a transaction before it sends
// anything.
//
// Three ways to answer no, and each of them matters: auto mode holds no
// transaction at all; one is open already, so a second `BEGIN` would be a
// warning from the server and a lie in the bar; and the buffer opens its
// own, which is the user saying where the transaction starts.
func needsBegin(mode dbclient.TxMode, inTransaction bool, first *query.TxVerb) bool {
	return mode == dbclient.TxModeManual && !inTransaction && (first == nil || *first != query.TxVerbBegin)
}

// diagnostics turns what the server said about each statement into marks
// on the buffer.
//
// The server answers about one statement and names a point inside it;
// this is where those become a range of the buffer the editor can paint —
// [`query::error_span`] grows the point into the token it landed on, and
// the statement's own start puts it back where the user typed it.
func diagnostics(
	sql string,
	ranges []query.Range,
	found []*dbclient.CheckError,
	cursor int,
	vocabulary *sqleditor.Vocabulary,
) []sqleditor.Diagnostic {
	var marks []sqleditor.Diagnostic
	// Set by the first statement that could have made a name the ones
	// after it use. The check asked a connection that has not run the
	// buffer, so from here on "does not exist" says nothing.
	namesMoved := false
	for i := 0; i < len(ranges) && i < len(found); i++ {
		statement := ranges[i]
		err := found[i]
		text := sql[statement.Start:statement.End]
		if err != nil && !(namesMoved && err.Refusal == dbclient.RefusalName) {
			if mark, ok := markFor(sql, statement, err, cursor, vocabulary); ok {
				marks = append(marks, mark)
			}
		}
		// After, not before: a `CREATE TABLE a.b` in a schema that is not
		// there is a name error about itself, and a real one.
		namesMoved = namesMoved || query.ChangesNames(text)
	}
	return marks
}

// markFor is one statement's answer as a mark on the buffer, or nothing
// when the statement is merely unfinished under the caret.
func markFor(
	sql string,
	statement query.Range,
	err *dbclient.CheckError,
	cursor int,
	vocabulary *sqleditor.Vocabulary,
) (sqleditor.Diagnostic, bool) {
	text := sql[statement.Start:statement.End]
	offset := 0
	if err.Offset != nil {
		offset = *err.Offset
	}
	at := statement.Start + offset
	if beingTyped(statement, at, cursor) {
		return sqleditor.Diagnostic{}, false
	}
	var span query.Range
	if err.Offset != nil {
		span = query.ErrorSpan(text, *err.Offset)
		span = reachBack(text, span, err.Refusal, vocabulary)
	} else {
		// The server named no position, so the statement as a whole is as
		// close as anything here can point.
		span = query.Range{Start: 0, End: len(text)}
	}
	r := sqleditor.Range{Start: statement.Start + span.Start, End: statement.Start + span.End}
	if r.IsEmpty() {
		return sqleditor.Diagnostic{}, false
	}
	return sqleditor.Diagnostic{
		Range:   r,
		Message: err.Message,
	}, true
}

// reachBack reaches a syntax error's mark back over the word the parser
// had just read, when nothing says that word was meant.
//
// **The server is right and one token late.** `… effort limi 100` is
// refused at `100`, because `limi` parsed perfectly well as a table
// alias, and `select 1 frm users` is refused at `users` for the same
// reason. A mark on the server's token alone points just past the typo
// every time, so it reaches back to cover the stretch the parser could
// not read — which always holds the mistake, wherever in it the mistake
// is.
//
// **Two things stop it, and they are the two ways a word can be meant.**
// A keyword is SQL's own, so `select from t` marks `from` and not the
// `select` in front of it. And a name the connection actually has is a
// name the user reached for on purpose: `select * from users 100` marks
// the `100`, because `users` is a table. What is left is a bare word this
// database has never heard of, sitting where the parser expected
// something else — which is a typo or it is nothing.
//
// Only a [`Refusal::Syntax`]. A name error already points at the name it
// could not find, and reaching back from one would swallow the word in
// front of a name that is merely missing.
func reachBack(
	text string,
	span query.Range,
	refusal dbclient.Refusal,
	vocabulary *sqleditor.Vocabulary,
) query.Range {
	if refusal != dbclient.RefusalSyntax {
		return span
	}
	before, ok := query.WordBefore(text, span.Start)
	if !ok {
		return span
	}
	word := text[before.Start:before.End]
	if query.IsKeyword(word) || vocabulary.Contains(word) {
		return span
	}
	return query.Range{Start: before.Start, End: span.End}
}

// beingTyped says whether an error is nothing but the statement being
// unfinished, under a caret that is still inside it.
//
// `select ` is a syntax error and is also what every query looks like a
// second after it is started. The server reports it at the **end of the
// input**, which is exactly where the caret is while it is being typed —
// so that pair, and only that pair, reads as "not finished" rather than
// "wrong". Move the caret away and the mark appears: the user has stopped
// writing this statement, and an unfinished one is then worth saying.
func beingTyped(statement query.Range, errorAt, cursor int) bool {
	return errorAt >= statement.End && statement.Start <= cursor && cursor <= statement.End
}

// reportVerbs is the verbs of a report's statements, each once, in the
// order they ran: `altered`, or `created, dropped`.
func reportVerbs(statements []ddlAt) string {
	var verbs []string
	for _, statement := range statements {
		past := statement.verb.Past()
		seen := false
		for _, verb := range verbs {
			if verb == past {
				seen = true
				break
			}
		}
		if !seen {
			verbs = append(verbs, past)
		}
	}
	return strings.Join(verbs, ", ")
}

// reportSubject is what the report is about, in the header — one relation
// by name, or a count — and the relation "reveal in schema tree" opens,
// which is the first one that is still there. `None` for both with
// nothing changed.
//
// An object of a relation reveals the relation it belongs to, which is
// always still there: the objects of a dropped relation are not listed.
//
// Go port: the reveal is Rust's `Option<(SharedString, SharedString)>`,
// schema then relation.
func reportSubject(delta *introspect.Delta) (*string, *[2]string) {
	var reveal *[2]string
	for _, relation := range delta.Relations {
		if _, dropped := relation.Change.(introspect.RelationDropped); !dropped {
			reveal = &[2]string{relation.Schema, relation.Name}
			break
		}
	}
	if reveal == nil {
		for _, object := range delta.Objects {
			if object.Relation != nil {
				reveal = &[2]string{object.Schema, *object.Relation}
				break
			}
		}
	}
	var subject *string
	relations, objects := delta.Relations, delta.Objects
	switch {
	case len(relations) == 0 && len(objects) == 0:
		subject = nil
	case len(relations) == 1 && len(objects) == 0:
		s := fmt.Sprintf("%s.%s", relations[0].Schema, relations[0].Name)
		subject = &s
	case len(relations) == 0 && len(objects) == 1:
		s := fmt.Sprintf("%s %s", objects[0].Kind.Noun(), objects[0].Qualified())
		subject = &s
	case len(objects) == 0:
		s := fmt.Sprintf("%d relations", len(relations))
		subject = &s
	default:
		s := fmt.Sprintf("%d changes", len(relations)+len(objects))
		subject = &s
	}
	return subject, reveal
}

// How many rows the report lists before it says `… and N more`. A
// `DROP SCHEMA … CASCADE` can take a hundred tables with it, and a panel
// a hundred rows tall is a panel that hides the result.
const deltaRows int = 8

func columnCount(n int) string {
	if n == 1 {
		return "1 column"
	}
	return fmt.Sprintf("%d columns", n)
}

// columnDetail is the faint line after a column's name in the sidebar: its
// type, and whether its values come from the column itself — a generated
// expression or an identity — since neither is a default the user can
// override by writing the column.
func columnDetail(column *introspect.Column) string {
	switch {
	case column.Generated != nil:
		return column.DataType + " · generated"
	case column.Identity != nil:
		return column.DataType + " · identity"
	}
	return column.DataType
}

// columnNote is the words after a column's type: what the catalog says
// about it that the type does not.
func columnNote(column *introspect.Column) string {
	nullable := "not null"
	if column.Nullable {
		nullable = "nullable"
	}
	if column.Generated != nil {
		if *column.Generated == "" {
			return fmt.Sprintf("%s · generated", nullable)
		}
		return fmt.Sprintf("%s · generated as %s", nullable, *column.Generated)
	}
	if column.Identity != nil {
		return fmt.Sprintf("%s · identity %s", nullable, *column.Identity)
	}
	if column.Default != nil {
		return fmt.Sprintf("%s · default %s", nullable, *column.Default)
	}
	return fmt.Sprintf("%s · no default", nullable)
}

func changedCopy(changed Changed) string {
	rows, verb := changed.rows, changed.verb
	switch {
	case verb != nil && rows == 0:
		return fmt.Sprintf("no rows %s", verb.Past())
	case verb != nil && rows == 1:
		return fmt.Sprintf("1 row %s", verb.Past())
	case verb != nil:
		return fmt.Sprintf("%s rows %s", uikit.FormatCount(rows), verb.Past())
	// No verb to name, so the neutral word: the server counted rows and
	// the app will not guess what it did to them.
	case rows == 0:
		return "no rows to show"
	case rows == 1:
		return "1 row affected"
	default:
		return fmt.Sprintf("%s rows affected", uikit.FormatCount(rows))
	}
}

// txCopy is what the transaction bar says: the state, and the line under
// it.
//
// `None` is the ordinary case — no transaction, and none just ended — and
// it means the bar is not painted at all. A strip that said "no
// transaction" would be a strip that is always there saying nothing.
func txCopy(state TxState) (title string, sub string, ok bool) {
	if state.open {
		title := "transaction open"
		if state.ending {
			return title, "ending it · waiting for the server", true
		}
		if state.running {
			// The session is one connection and the run has it, so the
			// buttons cannot be answered yet. Say which key clears the way.
			return title, "a run is out · ⌘. stops it first", true
		}
		var held string
		switch n := state.statements; n {
		// The transaction is open and nothing has landed in it: a run
		// that failed on its first statement leaves exactly this.
		case 0:
			held = "nothing has landed in it yet"
		case 1:
			held = "1 statement"
		default:
			held = fmt.Sprintf("%d statements", n)
		}
		// Rows, now that the server's count is read off the completion tag
		// — the comp asked for this line and the driver could not answer it
		// before. It is said only where there is something to say: a
		// transaction of nothing but `SELECT`s has touched no rows, and a
		// zero there would read as a warning about nothing.
		var touched string
		switch n := state.affected; n {
		case 0:
			touched = ""
		case 1:
			touched = " · 1 row touched"
		default:
			touched = fmt.Sprintf(" · %s rows touched", uikit.FormatCount(n))
		}
		return title, fmt.Sprintf("%s%s · nothing visible to anyone else yet", held, touched), true
	}
	if state.done == nil {
		return "", "", false
	}
	switch *state.done {
	// What happens *next* differs by mode, and the line has to be
	// right about it: in auto mode nothing opens another transaction.
	case dbclient.TxEndCommit:
		switch state.mode {
		case dbclient.TxModeManual:
			return "committed", "changes are live · a new transaction opens on your next run", true
		default:
			return "committed", "changes are live · statements commit on their own again", true
		}
	default:
		return "rolled back", "all changes discarded · nothing was written", true
	}
}

// spokenFor is which mark the strip speaks for: the one the caret is
// standing in, or the first. The caret at the very end of a mark counts as
// inside it — that is where it lands after the user types the word.
func spokenFor(diagnostics []sqleditor.Diagnostic, cursor int) int {
	for ix, diagnostic := range diagnostics {
		if diagnostic.Range.Start <= cursor && cursor <= diagnostic.Range.End {
			return ix
		}
	}
	return 0
}

// redact never paints a password, not even in the "could not connect"
// card.
func redact(url string) string {
	schemeEnd := strings.Index(url, "://")
	if schemeEnd < 0 {
		return url
	}
	scheme, rest := url[:schemeEnd+3], url[schemeEnd+3:]
	at := strings.IndexByte(rest, '@')
	if at < 0 {
		return url
	}
	credentials, host := rest[:at], rest[at:]
	if user, _, found := strings.Cut(credentials, ":"); found {
		return fmt.Sprintf("%s%s:•••%s", scheme, user, host)
	}
	return url
}
