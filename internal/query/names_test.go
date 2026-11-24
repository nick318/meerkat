package query

import "testing"

func TestNamesAreReadTheWayTheServerReadsThem(t *testing.T) {
	names := NamesIn([]string{
		`ALTER TABLE Orders ADD COLUMN note text`,
		`CREATE INDEX ix ON "Sales"."Big ""Q"" table" (a)`,
		`-- drop audit_log
DROP VIEW IF EXISTS dbo.sa_card_rate`,
	})
	cases := []struct {
		schema, name string
		want         bool
	}{
		// An unquoted name folds, and is any schema's.
		{"public", "orders", true},
		{"tenant_a", "orders", true},
		{"public", "Orders", false},
		// A column the statement adds is a name it mentions too.
		{"public", "note", true},
		// A quoted name keeps its case, and a doubled quote is one.
		{"Sales", `Big "Q" table`, true},
		{"sales", `Big "Q" table`, false},
		// A qualified name is that schema's only.
		{"dbo", "sa_card_rate", true},
		{"public", "sa_card_rate", false},
		// The comment before a statement is not part of it.
		{"public", "audit_log", false},
		// Strings and numbers are not names.
		{"public", "a", true},
		{"public", "zzz", false},
	}
	for _, c := range cases {
		if got := names.Mentions(c.schema, c.name); got != c.want {
			t.Errorf("Mentions(%q, %q) = %v, want %v", c.schema, c.name, got, c.want)
		}
	}
}

func TestASchemaStatementOwnsEverythingInTheSchema(t *testing.T) {
	names := NamesIn([]string{`DROP SCHEMA IF EXISTS "Old" CASCADE`})
	if !names.Mentions("Old", "orders") {
		t.Error("DROP SCHEMA should own every relation it takes with it")
	}
	if names.Mentions("old", "orders") || names.Mentions("public", "orders") {
		t.Error("DROP SCHEMA owns its own schema only")
	}
	if NamesIn([]string{`CREATE TABLE schema_log (a int)`}).Mentions("other", "orders") {
		t.Error("a table named like a keyword is not a schema statement")
	}
}

func TestALiteralIsNotAName(t *testing.T) {
	names := NamesIn([]string{`ALTER TABLE t ADD CHECK (kind <> 'orders' AND n > 5)`})
	if names.Mentions("public", "orders") {
		t.Error("a string literal must not read as a name")
	}
	if !names.Mentions("public", "kind") {
		t.Error("a column in a check is a name the statement mentions")
	}
}
