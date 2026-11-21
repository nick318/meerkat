package query

import (
	"slices"
	"testing"
)

// The buffers psql cuts in a place the first edition did not, and the
// shapes beside them that must not move.
func TestTheSplitIsPsqls(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		want []string
	}{
		// A backslash escapes the quote in an escape string, and only
		// there.
		{"escape string", `select E'it\'s; here'; select 2`,
			[]string{`select E'it\'s; here'`, "select 2"}},
		{"lower-case escape string", `select e'a\\'; select 2`,
			[]string{`select e'a\\'`, "select 2"}},
		{"escape string with a doubled quote", `select E'a''; b'; select 2`,
			[]string{`select E'a''; b'`, "select 2"}},
		{"a name ending in e is not an escape", `select xE'a\'; select 2`,
			[]string{`select xE'a\'`, "select 2"}},
		{"a plain string reads the backslash as itself", `select 'a\'; select 2`,
			[]string{`select 'a\'`, "select 2"}},
		{"an escape after a bracket", `select(E'\';'); select 2`,
			[]string{`select(E'\';')`, "select 2"}},

		// A `$` inside a name is part of the name.
		{"dollars inside a name", "select a$b$c from t; select 2",
			[]string{"select a$b$c from t", "select 2"}},
		{"a name ending in a dollar", "select price$usd$ from t; select 2",
			[]string{"select price$usd$ from t", "select 2"}},
		{"a parameter", "select $1; select $2",
			[]string{"select $1", "select $2"}},
		{"a parameter followed by a dollar", "select $1$ ; select 2",
			[]string{"select $1$", "select 2"}},
		{"a non-ASCII tag", "select $é$ a; b $é$; select 2",
			[]string{"select $é$ a; b $é$", "select 2"}},
		{"a tag with digits", "select $t1$ ; $t1$; select 2",
			[]string{"select $t1$ ; $t1$", "select 2"}},
		{"a nested tag inside a body", "select $$ $a$ ; $$; select 2",
			[]string{"select $$ $a$ ; $$", "select 2"}},
		{"an unterminated body runs to the end", "select $x$ a; select 2",
			[]string{"select $x$ a; select 2"}},
		{"a number before a dollar", "select 1$a$;$a$; select 2",
			[]string{"select 1$a$;$a$", "select 2"}},

		// A SQL-standard body.
		{"begin atomic", "CREATE FUNCTION f() RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT 1; SELECT 2; END; SELECT 3",
			[]string{"CREATE FUNCTION f() RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT 1; SELECT 2; END", "SELECT 3"}},
		{"or replace procedure", "create or replace procedure p() begin atomic insert into t values (1); end; select 3",
			[]string{"create or replace procedure p() begin atomic insert into t values (1); end", "select 3"}},
		{"a case inside the body", "create function f(x int) returns int begin atomic select case when x > 0 then 1 else 0 end; end; select 3",
			[]string{"create function f(x int) returns int begin atomic select case when x > 0 then 1 else 0 end; end", "select 3"}},
		{"a bracketed case inside the body", "create function f() returns int begin atomic select (case when true then 1 end); end; select 3",
			[]string{"create function f() returns int begin atomic select (case when true then 1 end); end", "select 3"}},
		{"a case outside a function", "select case when a then 'x; y' end from t; select 2",
			[]string{"select case when a then 'x; y' end from t", "select 2"}},
		{"a transaction block is three statements", "begin; select 1; end",
			[]string{"begin", "select 1", "end"}},
		{"a dollar-quoted body has no begin to count", "create function f() returns int as $$ begin return 1; end $$ language plpgsql; select 2",
			[]string{"create function f() returns int as $$ begin return 1; end $$ language plpgsql", "select 2"}},
		{"create table is not a routine", "create table t (a int); begin; select 1",
			[]string{"create table t (a int)", "begin", "select 1"}},
		{"the counters reset per statement", "create function f() returns int begin atomic select 1; end; begin; select 2",
			[]string{"create function f() returns int begin atomic select 1; end", "begin", "select 2"}},

		// Brackets.
		{"a rule", "CREATE RULE r AS ON INSERT TO t DO ALSO (INSERT INTO a VALUES (1); INSERT INTO b VALUES (2)); SELECT 3",
			[]string{"CREATE RULE r AS ON INSERT TO t DO ALSO (INSERT INTO a VALUES (1); INSERT INTO b VALUES (2))", "SELECT 3"}},
		{"a stray closing bracket", "select 1); select 2",
			[]string{"select 1)", "select 2"}},
		{"a bracket inside a string", "select '('; select 2",
			[]string{"select '('", "select 2"}},

		// A byte order mark.
		{"a leading byte order mark", "\uFEFFselect 1; select 2",
			[]string{"select 1", "select 2"}},
		{"a byte order mark before a routine", "\uFEFFcreate function f() returns int begin atomic select 1; end; select 2",
			[]string{"create function f() returns int begin atomic select 1; end", "select 2"}},
		{"nothing but a byte order mark", "\uFEFF  \n", nil},

		// Comments alone.
		{"a trailing line comment", "SELECT 1; -- trailing",
			[]string{"SELECT 1"}},
		{"a trailing block comment", "SELECT 1; /* block */",
			[]string{"SELECT 1"}},
		{"nothing but a comment", "-- note", nil},
		{"comments between semicolons", "select 1; -- a\n /* b */ ; select 2",
			[]string{"select 1", "select 2"}},
		{"a comment above a statement stays with it", "select 1;\n-- next\nselect 2",
			[]string{"select 1", "-- next\nselect 2"}},
		{"empty statements", ";;select 1;;", []string{"select 1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Statements(c.text)
			if !slices.Equal(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
				t.Fatalf("Statements(%q) = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

// The ranges are byte offsets into the buffer as it was, byte order mark
// and all, so the gutter can still find the lines.
func TestARangeSkipsTheByteOrderMark(t *testing.T) {
	text := "\uFEFFselect 1;\nselect 2"
	got := StatementRanges(text)
	want := []Range{{3, 11}, {13, 21}}
	if !slices.Equal(got, want) {
		t.Fatalf("ranges = %v, want %v", got, want)
	}
	for _, rng := range got {
		if text[rng.Start:rng.End] != Statements(text)[slices.Index(got, rng)] {
			t.Fatalf("range %v does not hold its statement", rng)
		}
	}
}

func TestANameChangeIsReadPastTheFirstWord(t *testing.T) {
	for _, c := range []struct {
		statement string
		want      bool
	}{
		{"select * into new_table from t", true},
		{"SELECT a, b INTO TEMP t2 FROM t", true},
		{"with x as (select 1) select * into t from x", true},
		{"/* copy */ select * into t from s", true},
		{"discard all", true},
		{"DISCARD TEMP", true},
		{"explain analyze create table t as select 1", true},
		{"EXPLAIN ANALYZE VERBOSE select * into t from s", true},
		{"explain (analyze, buffers) create table t as select 1", true},
		{"explain (ANALYZE true) create table t as select 1", true},
		{"explain analyse create table t as select 1", true},

		{"select * from t", false},
		{"select 'into' from t", false},
		{"select \"into\" from t", false},
		{"select 1 -- into\n from t", false},
		{"select * from t where a in (select b from u)", false},
		{"insert into t select * from u", false},
		{"with x as (select 1) insert into t select * from x", false},
		{"with x as (select 1) merge into t using x on true when matched then do nothing", false},
		{"explain create table t as select 1", false},
		{"explain (analyze false) create table t as select 1", false},
		{"explain (costs off) create table t as select 1", false},
		{"explain select * into t from s", false},
	} {
		if got := ChangesNames(c.statement); got != c.want {
			t.Errorf("ChangesNames(%q) = %v, want %v", c.statement, got, c.want)
		}
	}
}

func TestTheDdlVerbIsReadPastExplainAnalyze(t *testing.T) {
	eqDdl(t, "explain analyze create table t as select 1", DdlVerbCreate, true)
	eqDdl(t, "EXPLAIN (ANALYZE, BUFFERS) CREATE MATERIALIZED VIEW v AS SELECT 1", DdlVerbCreate, true)
	eqDdl(t, "explain analyze verbose /* x */ create table t as select 1", DdlVerbCreate, true)
	eqDdl(t, "explain create table t as select 1", 0, false)
	eqDdl(t, "explain (analyze off) create table t as select 1", 0, false)
	eqDdl(t, "explain analyze select 1", 0, false)
	eqDdl(t, "explain", 0, false)
	eqDdl(t, "explain (analyze", 0, false)
	// The command verb is the wording of a count and is left alone.
	eqCommand(t, "explain analyze delete from t", 0, false)
}
