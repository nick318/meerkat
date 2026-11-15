package main

import (
	"strings"
	"testing"

	"github.com/nick318/meerkat/internal/introspect"
)

func sqlTable(name string, columns []string, primaryKey []string) *introspect.Table {
	table := &introspect.Table{
		Name:       name,
		Kind:       introspect.TableKindTable,
		PrimaryKey: append([]string{}, primaryKey...),
		ApproxRows: nil,
	}
	for _, c := range columns {
		table.Columns = append(table.Columns, introspect.Column{
			Name:     c,
			DataType: "text",
			Nullable: true,
			Default:  nil,
		})
	}
	return table
}

func TestEmbeddedQuotesAreDoubled(t *testing.T) {
	if got := QuoteIdent("users"); got != "\"users\"" {
		t.Fatalf("got %q", got)
	}
	if got := QuoteIdent("we\"ird"); got != "\"we\"\"ird\"" {
		t.Fatalf("got %q", got)
	}
	// The classic injection attempt closes the quote; doubling it
	// leaves one harmless identifier.
	if got := QuoteIdent("x\"; DROP TABLE users; --"); got != "\"x\"\"; DROP TABLE users; --\"" {
		t.Fatalf("got %q", got)
	}
}

func TestPagesOrderByThePrimaryKey(t *testing.T) {
	users := sqlTable("users", []string{"id", "email"}, []string{"id"})
	if got := PageQuery("public", users, 0); got != "SELECT * FROM \"public\".\"users\" ORDER BY \"id\" LIMIT 500 OFFSET 0" {
		t.Fatalf("got %q", got)
	}
	if got := PageQuery("public", users, 3); got != "SELECT * FROM \"public\".\"users\" ORDER BY \"id\" LIMIT 500 OFFSET 1500" {
		t.Fatalf("got %q", got)
	}
}

func TestABrowsedRelationReadsAsATypedStatement(t *testing.T) {
	if got := BrowseQuery("public", "users"); got != "SELECT * FROM \"public\".\"users\" LIMIT 500;" {
		t.Fatalf("got %q", got)
	}
	// Quoted like everything else the app splices in.
	if got := BrowseQuery("we\"ird", "users"); got != "SELECT * FROM \"we\"\"ird\".\"users\" LIMIT 500;" {
		t.Fatalf("got %q", got)
	}
}

func TestCompositeKeysKeepTheirOrder(t *testing.T) {
	items := sqlTable("order_items", []string{"order_id", "line"}, []string{"order_id", "line"})
	if got := PageQuery("shop", items, 0); !strings.Contains(got, "ORDER BY \"order_id\", \"line\"") {
		t.Fatalf("got %q", got)
	}
}

func TestKeylessRelationsFallBackToTheFirstColumn(t *testing.T) {
	view := sqlTable("mrr_by_month", []string{"month", "mrr"}, nil)
	if got := PageQuery("public", view, 0); !strings.Contains(got, "ORDER BY \"month\"") {
		t.Fatalf("got %q", got)
	}

	empty := sqlTable("nothing", nil, nil)
	if got := PageQuery("public", empty, 0); got != "SELECT * FROM \"public\".\"nothing\" LIMIT 500 OFFSET 0" {
		t.Fatalf("got %q", got)
	}
}
