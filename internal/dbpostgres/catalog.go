package dbpostgres

// The introspection half of `crates/db_postgres/src/lib.rs`: the row
// shapes, `read_catalog`, `build_catalog`, `by_table` and
// `trigger_timing`, in the Rust file's order.

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/nick318/meerkat/internal/introspect"
)

// tableRow is one row of the table listing, before columns and keys are
// attached.
type tableRow struct {
	schema, table, relkind string
	reltuples              *int64
}

// columnRow is one row of the column listing: schema, table, column, type,
// nullable, default.
type columnRow struct {
	schema, table, name, dataType string
	nullable                      bool
	defaultValue                  *string
}

// keyRow is one row of the primary-key listing: schema, table, column.
type keyRow struct {
	schema, table, column string
}

// indexRow is one index: schema, table, index, method and key, unique,
// primary.
type indexRow struct {
	schema, table, name, definition string
	unique, primary                 bool
}

// constraintRow is one constraint: schema, table, name, `contype`,
// definition, columns, and for a foreign key the referenced schema, table
// and columns.
type constraintRow struct {
	schema, table, name, kind, definition string
	key                                   []string
	targetSchema, targetTable             *string
	targetKey                             []string
}

// triggerRow is one trigger: schema, table, name, `tgtype`, the function
// it runs.
type triggerRow struct {
	schema, table, name string
	tgtype              int32
	function            string
}

// sequenceRow is one sequence: schema, name, type.
type sequenceRow struct {
	schema, name, dataType string
}

// routineRow is one routine: schema, name, `prokind`, identity arguments,
// result.
type routineRow struct {
	schema, name, kind, arguments string
	returns                       *string
}

// typeRow is one type: schema, name, `typtype`, what it is made of.
type typeRow struct {
	schema, name, kind string
	definition         *string
}

// fetchAll is sqlx's `fetch_all` over `query_as`: every row, scanned, or
// the error.
func fetchAll[R any](ctx context.Context, conn *pgx.Conn, sql string, scan func(pgx.Rows) (R, error)) ([]R, error) {
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []R
	for rows.Next() {
		row, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// orDefault is `.unwrap_or_default()` on a `fetch_all`.
func orDefault[R any](rows []R, err error) []R {
	if err != nil {
		return nil
	}
	return rows
}

// readCatalog runs the catalog reads, on whichever connection is handed
// in.
//
// One function for the pool and for a session, so the two cannot drift
// into two catalogs: a session reads it to see its own uncommitted DDL,
// and must see exactly what the sidebar would see once that commits.
//
// Ten statements, each over one system catalog. A join of all of them
// would multiply the rows — every column by every index by every
// trigger — so the tree is put together here instead, by `build_catalog`.
// Every read skips the system schemas the same way, and the routines and
// types skip what an extension installed: `CREATE EXTENSION pgcrypto`
// puts forty functions into `public`, and none of them is the user's.
//
// **The relations are the catalog; everything else is extra.** The
// tables, their columns and their keys fail the read when they fail, as
// they always have. The objects after them are read *best-effort*: a
// server that refuses one of those reads — a fork, a server older than
// the catalog column asked for — answers with an empty list rather than
// costing the sidebar its tables. What needs a catalog newer than
// PostgreSQL 10 is not sent to an older server at all, so it cannot
// abort a transaction the session read is running inside.
func readCatalog(ctx context.Context, conn *pgx.Conn) (introspect.Catalog, error) {
	tables, err := fetchAll(ctx, conn,
		"SELECT n.nspname, c.relname, c.relkind::text, c.reltuples::bigint "+
			"FROM pg_class c "+
			"JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') "+
			"AND n.nspname NOT IN ('pg_catalog', 'information_schema') "+
			"AND n.nspname NOT LIKE 'pg_toast%' "+
			"AND n.nspname NOT LIKE 'pg_temp%' "+
			"ORDER BY n.nspname, c.relname",
		func(rows pgx.Rows) (tableRow, error) {
			var r tableRow
			err := rows.Scan(&r.schema, &r.table, &r.relkind, &r.reltuples)
			return r, err
		})
	if err != nil {
		return introspect.Catalog{}, fmt.Errorf("failed to list tables: %w", err)
	}

	columns, err := fetchAll(ctx, conn,
		"SELECT n.nspname, c.relname, a.attname, "+
			"format_type(a.atttypid, a.atttypmod), "+
			"NOT a.attnotnull, "+
			"pg_get_expr(d.adbin, d.adrelid) "+
			"FROM pg_attribute a "+
			"JOIN pg_class c ON c.oid = a.attrelid "+
			"JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum "+
			"WHERE a.attnum > 0 AND NOT a.attisdropped "+
			"AND c.relkind IN ('r', 'p', 'v', 'm', 'f') "+
			"AND n.nspname NOT IN ('pg_catalog', 'information_schema') "+
			"AND n.nspname NOT LIKE 'pg_toast%' "+
			"AND n.nspname NOT LIKE 'pg_temp%' "+
			"ORDER BY n.nspname, c.relname, a.attnum",
		func(rows pgx.Rows) (columnRow, error) {
			var r columnRow
			err := rows.Scan(&r.schema, &r.table, &r.name, &r.dataType, &r.nullable, &r.defaultValue)
			return r, err
		})
	if err != nil {
		return introspect.Catalog{}, fmt.Errorf("failed to list columns: %w", err)
	}

	// `key_column_usage.ordinal_position` already gives key order.
	keys, err := fetchAll(ctx, conn,
		"SELECT tc.table_schema, tc.table_name, kcu.column_name "+
			"FROM information_schema.table_constraints tc "+
			"JOIN information_schema.key_column_usage kcu "+
			"ON kcu.constraint_name = tc.constraint_name "+
			"AND kcu.constraint_schema = tc.constraint_schema "+
			"WHERE tc.constraint_type = 'PRIMARY KEY' "+
			"AND tc.table_schema NOT IN ('pg_catalog', 'information_schema') "+
			"ORDER BY tc.table_schema, tc.table_name, kcu.ordinal_position",
		func(rows pgx.Rows) (keyRow, error) {
			var r keyRow
			err := rows.Scan(&r.schema, &r.table, &r.column)
			return r, err
		})
	if err != nil {
		return introspect.Catalog{}, fmt.Errorf("failed to list primary keys: %w", err)
	}

	var version int32
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		return introspect.Catalog{}, fmt.Errorf("failed to read the server version: %w", err)
	}

	// `pg_get_indexdef` writes the whole `CREATE INDEX`; the part after
	// `USING` is the method and the key, which is what differs between
	// two indexes of one table.
	indexes := orDefault(fetchAll(ctx, conn,
		"SELECT n.nspname, c.relname, ic.relname, "+
			"coalesce(substring(pg_get_indexdef(i.indexrelid) FROM ' USING (.*)$'), "+
			"pg_get_indexdef(i.indexrelid)), "+
			"i.indisunique, i.indisprimary "+
			"FROM pg_index i "+
			"JOIN pg_class c ON c.oid = i.indrelid "+
			"JOIN pg_class ic ON ic.oid = i.indexrelid "+
			"JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"WHERE c.relkind IN ('r', 'p', 'm', 'f') "+
			"AND n.nspname NOT IN ('pg_catalog', 'information_schema') "+
			"AND n.nspname NOT LIKE 'pg_toast%' "+
			"AND n.nspname NOT LIKE 'pg_temp%' "+
			"ORDER BY n.nspname, c.relname, ic.relname",
		func(rows pgx.Rows) (indexRow, error) {
			var r indexRow
			err := rows.Scan(&r.schema, &r.table, &r.name, &r.definition, &r.unique, &r.primary)
			return r, err
		}))

	// Foreign keys, unique, check and exclusion constraints in one read.
	// The primary key is left out — `keys` has it in key order — and so
	// is a domain's check, which belongs to no table. The column arrays
	// are walked `WITH ORDINALITY` so a two-column key keeps its order.
	//
	// A key that references a partitioned table is cloned once for every
	// partition it reaches, on the same referencing table; those clones
	// carry a parent on that same table, and `\d` hides them too.
	// `conparentid` is PostgreSQL 11's, and a server before it has no
	// clones to hide.
	clones := ""
	if version >= 110_000 {
		clones = "AND NOT EXISTS (SELECT 1 FROM pg_constraint parent " +
			"WHERE parent.oid = con.conparentid " +
			"AND parent.conrelid = con.conrelid)"
	}
	constraints := orDefault(fetchAll(ctx, conn, fmt.Sprintf(
		"SELECT n.nspname, c.relname, con.conname, con.contype::text, "+
			"pg_get_constraintdef(con.oid, true), "+
			"ARRAY(SELECT a.attname::text "+
			"FROM unnest(con.conkey) WITH ORDINALITY AS k(num, ord) "+
			"JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.num "+
			"ORDER BY k.ord), "+
			"fn.nspname::text, fc.relname::text, "+
			"ARRAY(SELECT a.attname::text "+
			"FROM unnest(con.confkey) WITH ORDINALITY AS k(num, ord) "+
			"JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.num "+
			"ORDER BY k.ord) "+
			"FROM pg_constraint con "+
			"JOIN pg_class c ON c.oid = con.conrelid "+
			"JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"LEFT JOIN pg_class fc ON fc.oid = con.confrelid "+
			"LEFT JOIN pg_namespace fn ON fn.oid = fc.relnamespace "+
			"WHERE con.contype IN ('f', 'u', 'c', 'x') %s "+
			"AND n.nspname NOT IN ('pg_catalog', 'information_schema') "+
			"AND n.nspname NOT LIKE 'pg_toast%%' "+
			"AND n.nspname NOT LIKE 'pg_temp%%' "+
			"ORDER BY n.nspname, c.relname, con.conname",
		clones),
		func(rows pgx.Rows) (constraintRow, error) {
			var r constraintRow
			err := rows.Scan(&r.schema, &r.table, &r.name, &r.kind, &r.definition, &r.key,
				&r.targetSchema, &r.targetTable, &r.targetKey)
			return r, err
		}))

	// `tgisinternal` is what a foreign key's own enforcement triggers
	// carry; they are the key, not something the user wrote.
	triggers := orDefault(fetchAll(ctx, conn,
		"SELECT n.nspname, c.relname, t.tgname, t.tgtype::int, t.tgfoid::regproc::text "+
			"FROM pg_trigger t "+
			"JOIN pg_class c ON c.oid = t.tgrelid "+
			"JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"WHERE NOT t.tgisinternal "+
			"AND n.nspname NOT IN ('pg_catalog', 'information_schema') "+
			"AND n.nspname NOT LIKE 'pg_toast%' "+
			"AND n.nspname NOT LIKE 'pg_temp%' "+
			"ORDER BY n.nspname, c.relname, t.tgname",
		func(rows pgx.Rows) (triggerRow, error) {
			var r triggerRow
			err := rows.Scan(&r.schema, &r.table, &r.name, &r.tgtype, &r.function)
			return r, err
		}))

	// A sequence a column owns — a `serial`'s, an identity's — is how that
	// column counts, not something the user made beside it. Listing it
	// would also have every `CREATE TABLE … (id serial)` report a new
	// sequence beside its new table. `pg_sequence` is PostgreSQL 10's.
	var sequences []sequenceRow
	if version >= 100_000 {
		sequences = orDefault(fetchAll(ctx, conn,
			"SELECT n.nspname, c.relname, format_type(s.seqtypid, NULL) "+
				"FROM pg_sequence s "+
				"JOIN pg_class c ON c.oid = s.seqrelid "+
				"JOIN pg_namespace n ON n.oid = c.relnamespace "+
				"WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') "+
				"AND n.nspname NOT LIKE 'pg_toast%' "+
				"AND n.nspname NOT LIKE 'pg_temp%' "+
				"AND NOT EXISTS (SELECT 1 FROM pg_depend d "+
				"WHERE d.classid = 'pg_class'::regclass "+
				"AND d.objid = c.oid AND d.refobjsubid > 0 "+
				"AND d.deptype IN ('a', 'i')) "+
				"ORDER BY n.nspname, c.relname",
			func(rows pgx.Rows) (sequenceRow, error) {
				var r sequenceRow
				err := rows.Scan(&r.schema, &r.name, &r.dataType)
				return r, err
			}))
	}

	// `prokind` is PostgreSQL 11's. The routines a `CREATE TYPE … AS RANGE`
	// makes for itself depend on the type (`deptype = 'i'`) and are the
	// type's, not the user's, so they are skipped with the extensions'.
	var routines []routineRow
	if version >= 110_000 {
		routines = orDefault(fetchAll(ctx, conn,
			"SELECT n.nspname, p.proname, p.prokind::text, "+
				"pg_get_function_identity_arguments(p.oid), "+
				"CASE WHEN p.prokind = 'p' THEN NULL ELSE pg_get_function_result(p.oid) END "+
				"FROM pg_proc p "+
				"JOIN pg_namespace n ON n.oid = p.pronamespace "+
				"WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') "+
				"AND n.nspname NOT LIKE 'pg_toast%' "+
				"AND n.nspname NOT LIKE 'pg_temp%' "+
				"AND NOT EXISTS (SELECT 1 FROM pg_depend d "+
				"WHERE d.classid = 'pg_proc'::regclass "+
				"AND d.objid = p.oid AND d.deptype IN ('e', 'i')) "+
				"ORDER BY n.nspname, p.proname, 4",
			func(rows pgx.Rows) (routineRow, error) {
				var r routineRow
				err := rows.Scan(&r.schema, &r.name, &r.kind, &r.arguments, &r.returns)
				return r, err
			}))
	}

	// Every table has a composite type of its own; only a composite made
	// with `CREATE TYPE … AS (…)` has a `pg_class` row of kind `c`.
	types := orDefault(fetchAll(ctx, conn,
		"SELECT n.nspname, t.typname, t.typtype::text, "+
			"CASE t.typtype "+
			"WHEN 'e' THEN (SELECT string_agg(e.enumlabel, ', ' ORDER BY e.enumsortorder) "+
			"FROM pg_enum e WHERE e.enumtypid = t.oid) "+
			"WHEN 'd' THEN format_type(t.typbasetype, t.typtypmod) "+
			"WHEN 'r' THEN (SELECT format_type(r.rngsubtype, NULL) "+
			"FROM pg_range r WHERE r.rngtypid = t.oid) "+
			"WHEN 'c' THEN (SELECT string_agg(a.attname || ' ' "+
			"|| format_type(a.atttypid, a.atttypmod), "+
			"', ' ORDER BY a.attnum) "+
			"FROM pg_attribute a "+
			"WHERE a.attrelid = t.typrelid "+
			"AND a.attnum > 0 AND NOT a.attisdropped) "+
			"END "+
			"FROM pg_type t "+
			"JOIN pg_namespace n ON n.oid = t.typnamespace "+
			"WHERE t.typtype IN ('e', 'd', 'r', 'c') "+
			"AND (t.typtype <> 'c' "+
			"OR (SELECT c.relkind FROM pg_class c WHERE c.oid = t.typrelid) = 'c') "+
			"AND n.nspname NOT IN ('pg_catalog', 'information_schema') "+
			"AND n.nspname NOT LIKE 'pg_toast%' "+
			"AND n.nspname NOT LIKE 'pg_temp%' "+
			"AND NOT EXISTS (SELECT 1 FROM pg_depend d "+
			"WHERE d.classid = 'pg_type'::regclass "+
			"AND d.objid = t.oid AND d.deptype = 'e') "+
			"ORDER BY n.nspname, t.typname",
		func(rows pgx.Rows) (typeRow, error) {
			var r typeRow
			err := rows.Scan(&r.schema, &r.name, &r.kind, &r.definition)
			return r, err
		}))

	return buildCatalog(catalogRows{
		tables:      tables,
		columns:     columns,
		keys:        keys,
		indexes:     indexes,
		constraints: constraints,
		triggers:    triggers,
		sequences:   sequences,
		routines:    routines,
		types:       types,
	}), nil
}

// catalogRows is everything `read_catalog` read, before it is put into a
// tree.
type catalogRows struct {
	tables      []tableRow
	columns     []columnRow
	keys        []keyRow
	indexes     []indexRow
	constraints []constraintRow
	triggers    []triggerRow
	sequences   []sequenceRow
	routines    []routineRow
	types       []typeRow
}

// tableKey is Rust's `(String, String)`: a schema and a table.
type tableKey struct {
	schema, table string
}

// buildCatalog puts the rows into schemas. A schema is listed when
// anything at all sits in it, so a schema of nothing but functions has a
// place in the tree too. Every read is ordered by schema name, and a
// `BTreeMap` keeps that order: `name` compares bytewise, as a Rust string
// does.
//
// Every per-table list is grouped by its table once, up front, so a
// catalog of thousands of tables is put together in one pass over each
// list rather than one pass per table.
func buildCatalog(rows catalogRows) introspect.Catalog {
	// Go port: a map plus its keys sorted bytewise, for the `BTreeMap`.
	schemas := make(map[string]*introspect.Schema)
	schema := func(name string) *introspect.Schema {
		if found, ok := schemas[name]; ok {
			return found
		}
		created := &introspect.Schema{Name: name}
		schemas[name] = created
		return created
	}

	columns := byTable(rows.columns, func(r columnRow) (tableKey, introspect.Column) {
		return tableKey{r.schema, r.table}, introspect.Column{
			Name:     r.name,
			DataType: r.dataType,
			Nullable: r.nullable,
			Default:  r.defaultValue,
		}
	})
	keys := byTable(rows.keys, func(r keyRow) (tableKey, string) {
		return tableKey{r.schema, r.table}, r.column
	})
	indexes := byTable(rows.indexes, func(r indexRow) (tableKey, introspect.Index) {
		return tableKey{r.schema, r.table}, introspect.Index{
			Name:       r.name,
			Definition: r.definition,
			Unique:     r.unique,
			Primary:    r.primary,
		}
	})
	foreignKeys := make(map[tableKey][]introspect.ForeignKey)
	constraints := make(map[tableKey][]introspect.Constraint)
	for _, r := range rows.constraints {
		key := tableKey{r.schema, r.table}
		var kind introspect.ConstraintKind
		switch r.kind {
		case "f":
			foreignKeys[key] = append(foreignKeys[key], introspect.ForeignKey{
				Name:          r.name,
				Columns:       r.key,
				TargetSchema:  deref(r.targetSchema),
				TargetTable:   deref(r.targetTable),
				TargetColumns: r.targetKey,
			})
			continue
		case "u":
			kind = introspect.ConstraintKindUnique
		case "c":
			kind = introspect.ConstraintKindCheck
		case "x":
			kind = introspect.ConstraintKindExclusion
		default:
			continue
		}
		constraints[key] = append(constraints[key], introspect.Constraint{
			Name:       r.name,
			Kind:       kind,
			Definition: r.definition,
		})
	}
	triggers := byTable(rows.triggers, func(r triggerRow) (tableKey, introspect.Trigger) {
		return tableKey{r.schema, r.table}, introspect.Trigger{
			Name:     r.name,
			Timing:   triggerTiming(r.tgtype),
			Function: fmt.Sprintf("%s()", r.function),
		}
	})

	for _, r := range rows.tables {
		key := tableKey{r.schema, r.table}
		kind := introspect.TableKindTable
		switch r.relkind {
		// Views and materialized views read the same to a viewer.
		case "v", "m":
			kind = introspect.TableKindView
		}
		// `reltuples` is -1 when the table was never analyzed. Report
		// nothing rather than a zero the user would read as "empty".
		var approxRows *uint64
		if r.reltuples != nil && *r.reltuples >= 0 {
			n := uint64(*r.reltuples)
			approxRows = &n
		}
		table := introspect.Table{
			Name:        key.table,
			Kind:        kind,
			Columns:     columns[key],
			PrimaryKey:  keys[key],
			ApproxRows:  approxRows,
			Indexes:     indexes[key],
			ForeignKeys: foreignKeys[key],
			Constraints: constraints[key],
			Triggers:    triggers[key],
		}
		delete(columns, key)
		delete(keys, key)
		delete(indexes, key)
		delete(foreignKeys, key)
		delete(constraints, key)
		delete(triggers, key)
		s := schema(key.schema)
		s.Tables = append(s.Tables, table)
	}
	for _, r := range rows.sequences {
		s := schema(r.schema)
		s.Sequences = append(s.Sequences, introspect.Sequence{Name: r.name, DataType: r.dataType})
	}
	for _, r := range rows.routines {
		kind := introspect.RoutineKindFunction
		switch r.kind {
		case "p":
			kind = introspect.RoutineKindProcedure
		case "a":
			kind = introspect.RoutineKindAggregate
		case "w":
			kind = introspect.RoutineKindWindow
		}
		s := schema(r.schema)
		s.Routines = append(s.Routines, introspect.Routine{
			Name:      r.name,
			Kind:      kind,
			Arguments: r.arguments,
			Returns:   r.returns,
		})
	}
	for _, r := range rows.types {
		kind := introspect.TypeKindComposite
		switch r.kind {
		case "e":
			kind = introspect.TypeKindEnum
		case "d":
			kind = introspect.TypeKindDomain
		case "r":
			kind = introspect.TypeKindRange
		}
		s := schema(r.schema)
		s.Types = append(s.Types, introspect.UserType{
			Name:       r.name,
			Kind:       kind,
			Definition: deref(r.definition),
		})
	}

	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	catalog := introspect.Catalog{Schemas: make([]introspect.Schema, 0, len(names))}
	for _, name := range names {
		catalog.Schemas = append(catalog.Schemas, *schemas[name])
	}
	return catalog
}

// byTable groups one listing by the table each row belongs to, keeping
// the order the listing had within each table. `split` names the table
// and makes the model value out of the rest of the row.
func byTable[R, T any](rows []R, split func(R) (tableKey, T)) map[tableKey][]T {
	grouped := make(map[tableKey][]T)
	for _, row := range rows {
		key, value := split(row)
		grouped[key] = append(grouped[key], value)
	}
	return grouped
}

// triggerTiming says when a trigger fires, read off `pg_trigger.tgtype`,
// whose bits are fixed in `pg_trigger.h`: `before insert or update`, and
// `· statement` for one that fires once per statement rather than once
// per row.
func triggerTiming(tgtype int32) string {
	const (
		row      int32 = 1 << 0
		before   int32 = 1 << 1
		insert   int32 = 1 << 2
		delete   int32 = 1 << 3
		update   int32 = 1 << 4
		truncate int32 = 1 << 5
		instead  int32 = 1 << 6
	)

	when := "after"
	if tgtype&instead != 0 {
		when = "instead of"
	} else if tgtype&before != 0 {
		when = "before"
	}
	var events []string
	for _, e := range []struct {
		bit   int32
		event string
	}{
		{insert, "insert"},
		{update, "update"},
		{delete, "delete"},
		{truncate, "truncate"},
	} {
		if tgtype&e.bit != 0 {
			events = append(events, e.event)
		}
	}
	timing := fmt.Sprintf("%s %s", when, joinStrings(events, " or "))
	if tgtype&row == 0 {
		timing += " · statement"
	}
	return timing
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for k, part := range parts {
		if k > 0 {
			out += sep
		}
		out += part
	}
	return out
}

// deref is `Option::unwrap_or_default` on a string.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
