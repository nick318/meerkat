// SQL the app writes for itself.
//
// Only identifiers that came back from introspection are ever spliced in,
// and they are always quoted. Nothing the user types reaches these
// builders — a typed query goes to the driver verbatim instead.

package main

import (
	"fmt"
	"strings"

	"github.com/nick318/meerkat/internal/introspect"
)

// PageSize is rows fetched per page of a table view.
const PageSize = 500

// QuoteIdent quotes an identifier for Postgres, doubling any embedded
// quote.
func QuoteIdent(name string) string {
	return fmt.Sprintf("\"%s\"", strings.ReplaceAll(name, "\"", "\"\""))
}

// PageQuery is `SELECT * FROM "schema"."table" ORDER BY <key> LIMIT n
// OFFSET n`.
//
// Ordering keeps paging stable: without it Postgres may hand back the
// same row on two pages. The primary key is the natural order; a view or
// a keyless table falls back to its first column, and a table with no
// columns at all gets no ORDER BY.
func PageQuery(schema string, table *introspect.Table, page int) string {
	sql := fmt.Sprintf(
		"SELECT * FROM %s.%s",
		QuoteIdent(schema),
		QuoteIdent(table.Name),
	)
	if order, ok := sqlOrderBy(table); ok {
		sql += fmt.Sprintf(" ORDER BY %s", order)
	}
	sql += fmt.Sprintf(" LIMIT %d OFFSET %d", PageSize, page*PageSize)
	return sql
}

// BrowseQuery is `SELECT * FROM "schema"."table" LIMIT 500;` — what a
// click in the sidebar puts in a query tab.
//
// No ORDER BY: this statement is the user's from the moment it lands,
// and paging is not what it is for. It ends in a semicolon because that
// is how the line would be typed, and it needs no catalog entry, so a
// relation can be opened from a cached sidebar.
func BrowseQuery(schema, table string) string {
	return fmt.Sprintf(
		"SELECT * FROM %s.%s LIMIT %d;",
		QuoteIdent(schema),
		QuoteIdent(table),
		PageSize,
	)
}

// Rust: `order_by`.
func sqlOrderBy(table *introspect.Table) (string, bool) {
	var columns []string
	if table.HasPrimaryKey() {
		columns = table.PrimaryKey
	} else if len(table.Columns) > 0 {
		columns = []string{table.Columns[0].Name}
	}
	if len(columns) == 0 {
		return "", false
	}
	quoted := make([]string, 0, len(columns))
	for _, name := range columns {
		quoted = append(quoted, QuoteIdent(name))
	}
	return strings.Join(quoted, ", "), true
}
