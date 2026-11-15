package query

import (
	"slices"
	"testing"
)

func texts(text string) []string {
	return Statements(text)
}

func marked(statement string, offset int) string {
	span := ErrorSpan(statement, offset)
	return statement[span.Start:span.End]
}

func eqTexts(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("statements = %q, want %q", got, want)
	}
}

func eqString(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTheKeywordListIsSortedForBisection(t *testing.T) {
	sorted := slices.Clone(Keywords)
	slices.Sort(sorted)
	if !slices.Equal(Keywords, sorted) {
		t.Fatalf("KEYWORDS = %q, want %q", Keywords, sorted)
	}
	if !(IsKeyword("SELECT") && IsKeyword("select")) {
		t.Fatal("select is a keyword")
	}
	if IsKeyword("selected") {
		t.Fatal("selected is not a keyword")
	}
}

// targeted is `Option<(&str, bool)>` in the Rust test helper.
type targeted struct {
	name     string
	ifExists bool
}

func target(statement string) *targeted {
	found := RelationTargetOf(statement)
	if found == nil {
		return nil
	}
	return &targeted{name: statement[found.Name.Start:found.Name.End], ifExists: found.IfExists}
}

func eqTarget(t *testing.T, got *targeted, name string, ifExists bool) {
	t.Helper()
	if got == nil || got.name != name || got.ifExists != ifExists {
		t.Fatalf("target = %+v, want (%q, %v)", got, name, ifExists)
	}
}

// The statements Postgres will not resolve a name for until it runs
// them, which is why the app has to name the relation itself.
func TestAUtilityStatementNamesItsRelation(t *testing.T) {
	dropped := target("drop table tenant_dev_tenant.effort1")
	eqTarget(t, dropped, "tenant_dev_tenant.effort1", false)
	eqTarget(t, target("DROP TABLE IF EXISTS t"), "t", true)
	eqTarget(t, target("drop materialized view mv"), "mv", false)
	eqTarget(t, target("alter table only t"), "t", false)
	eqTarget(t, target("truncate t"), "t", false)
	eqTarget(t, target("truncate table only t"), "t", false)
	eqTarget(t, target("-- go\n  drop view v"), "v", false)
	eqTarget(t,
		target("drop table \"My Table\""),
		"\"My Table\"", false,
	)
	// Only the first of a list; the rest go unchecked rather than
	// wrongly checked.
	eqTarget(t, target("drop table a, b"), "a", false)
}

// Anything the reader does not recognise answers `None`, which costs
// a mark rather than painting a wrong one.
func TestEverythingElseNamesNothing(t *testing.T) {
	for _, statement := range []string{
		// Not relations: they live in catalogs of their own.
		"drop function f(int)",
		"drop schema s",
		"drop type t",
		"drop role r",
		// Not the shape at all.
		"select * from t",
		"create table t (id int)",
		"alter",
		"",
		// `IF` without `EXISTS` is not this statement.
		"drop table if t",
	} {
		if got := RelationTargetOf(statement); got != nil {
			t.Fatalf("%s: %+v", statement, got)
		}
	}
}

// The word the parser had just read, which is where the mistake
// usually is.
func TestTheWordBeforeAnErrorIsTheOneThatWasRead(t *testing.T) {
	statement := "select * from t limi 100"
	rng, ok := WordBefore(statement, 21)
	if !ok {
		t.Fatal("a word before limi's error")
	}
	eqString(t, statement[rng.Start:rng.End], "limi")
	// A qualified name comes back whole, as it is marked whole.
	statement = "select * from a.b 100"
	rng, ok = WordBefore(statement, 18)
	if !ok {
		t.Fatal("a word before a.b's error")
	}
	eqString(t, statement[rng.Start:rng.End], "a.b")
	// Nothing that is not a name: a number, an operator, the front of
	// the statement.
	for _, c := range []struct {
		statement string
		offset    int
	}{
		{"select 100 200", 11},
		{"select x = = 1", 11},
		{"select 1", 0},
		{"  select 1", 2},
	} {
		if got, ok := WordBefore(c.statement, c.offset); ok {
			t.Fatalf("WordBefore(%q, %d) = %v, want none", c.statement, c.offset, got)
		}
	}
}

func TestAStatementThatCouldMakeANameIsReadAsOne(t *testing.T) {
	for _, statement := range []string{
		"create table t (id int)",
		"CREATE TEMP TABLE t AS SELECT 1",
		"drop table t",
		"alter table t add column c int",
		"set search_path to app",
		"-- first\n  create schema s",
	} {
		if !ChangesNames(statement) {
			t.Fatal(statement)
		}
	}
	for _, statement := range []string{
		"select * from t",
		"insert into t values (1)",
		"begin",
		"",
		"   ",
	} {
		if ChangesNames(statement) {
			t.Fatal(statement)
		}
	}
}

// **The case a viewer meets every day.** The server points at the
// front of `schema.table` and refuses the pair, so the mark has to
// carry the dots: `tenant_dev_tenant` alone is the half that is
// right.
func TestAQualifiedNameIsMarkedWhole(t *testing.T) {
	statement := "select * from tenant_dev_tenant.effor limit 100"
	eqString(t, marked(statement, 14), "tenant_dev_tenant.effor")
	eqString(t, marked("select a.b.c from t", 7), "a.b.c")
	eqString(t,
		marked("select \"my schema\".t from x", 7),
		"\"my schema\".t",
	)
	// A dot with no name after it is not part of the name, and `*` is
	// not a name part either.
	eqString(t, marked("select * from schema. limit 1", 14), "schema")
	eqString(t, marked("select t.* from t", 7), "t")
}

// An unfinished qualified name marks the part that is there.
func TestAnUnfinishedQualifiedNameMarksWhatWasTyped(t *testing.T) {
	eqString(t, marked("select * from schema.", 21), "schema.")
}

// The server names a point; the mark is the token it points at.
func TestTheErrorMarksTheTokenTheCursorLandedOn(t *testing.T) {
	statement := "select 1 frm users"
	eqString(t, marked(statement, 13), "users")
	eqString(t, marked(statement, 9), "frm")
}

// A statement the user has not finished: the point is past the last
// token, so the mark goes on the token itself.
func TestTheEndOfTheInputMarksTheLastToken(t *testing.T) {
	eqString(t, marked("select", 6), "select")
	// Trailing space is not the answer to "where did I stop".
	eqString(t, marked("select from  ", 13), "from")
	eqString(t, marked("select *", 8), "*")
}

// A string is one token however long it runs, and an unterminated one
// swallows the rest of the statement — which is exactly what is wrong
// with it.
func TestAStringIsMarkedWhole(t *testing.T) {
	eqString(t, marked("select 'a b c' from t", 7), "'a b c'")
	eqString(t, marked("select 'a''b' from t", 7), "'a''b'")
	eqString(t, marked("select 'oops from t", 7), "'oops from t")
	eqString(t, marked("select \"a b\" from t", 7), "\"a b\"")
}

// Punctuation is one character, and a multi-byte character is one
// character rather than one byte — a range that split one would not
// slice.
func TestPunctuationMarksOneCharacter(t *testing.T) {
	eqString(t, marked("select ) from t", 7), ")")
	eqString(t, marked("select § from t", 7), "§")
	eqString(t, marked("select §", 9), "§")
	// An offset that landed inside a character marks the character.
	eqString(t, marked("select §", 8), "§")
}

// A point on the space in front of a token still marks the token: an
// empty range paints nothing, and an error painted by nothing is an
// error the user never sees.
func TestTheMarkIsNeverEmpty(t *testing.T) {
	eqString(t, marked("select  frm", 6), "frm")
	for offset := 0; offset <= 12; offset++ {
		if ErrorSpan("select 1 frm", offset).IsEmpty() {
			t.Fatalf("offset %d", offset)
		}
	}
	// Nothing to mark in nothing.
	if got := ErrorSpan("", 0); got != (Range{Start: 0, End: 0}) {
		t.Fatalf("empty: %v", got)
	}
	if got := ErrorSpan("   ", 3); got != (Range{Start: 0, End: 0}) {
		t.Fatalf("blank: %v", got)
	}
}

// The offset is the server's, and a server that names one past the
// end must not panic the app.
func TestAnOffsetPastTheEndIsClamped(t *testing.T) {
	eqString(t, marked("select", 99), "select")
}

func TestARangeHoldsTheStatementItNames(t *testing.T) {
	// The gutter marks a statement by where it sits in the buffer, so
	// the range and the text have to be the same statement.
	text := "select 1;\n\nupdate t set a = 1;\n"
	ranges := StatementRanges(text)
	if want := []Range{{0, 8}, {11, 29}}; !slices.Equal(ranges, want) {
		t.Fatalf("ranges = %v, want %v", ranges, want)
	}
	var named []string
	for _, rng := range ranges {
		named = append(named, text[rng.Start:rng.End])
	}
	eqTexts(t, named, Statements(text))
}

func TestASemicolonSeparatesStatements(t *testing.T) {
	text := "select 1;\n\nselect 2;"
	eqTexts(t, texts(text), []string{"select 1", "select 2"})
}

func TestABufferWithoutSemicolonsIsOneStatement(t *testing.T) {
	eqTexts(t, texts("select 1"), []string{"select 1"})
	if got := texts("   \n\n  "); len(got) != 0 {
		t.Fatalf("blank buffer: %q", got)
	}
}

func TestASemicolonInsideAStringDoesNotSplit(t *testing.T) {
	text := "select ';' as a; select 2"
	eqTexts(t, texts(text), []string{"select ';' as a", "select 2"})
	eqTexts(t, texts("select 'it''s; fine'"), []string{"select 'it''s; fine'"})
}

func TestASemicolonInsideAQuotedIdentifierDoesNotSplit(t *testing.T) {
	text := "select * from \"we;ird\"; select 2"
	eqTexts(t, texts(text), []string{"select * from \"we;ird\"", "select 2"})
}

func TestASemicolonInsideACommentDoesNotSplit(t *testing.T) {
	text := "select 1 -- ; not a split\n; select 2"
	eqTexts(t, texts(text), []string{"select 1 -- ; not a split", "select 2"})

	text = "select /* ; nested /* ; */ still */ 1; select 2"
	eqTexts(t,
		texts(text),
		[]string{"select /* ; nested /* ; */ still */ 1", "select 2"},
	)
}

func TestASemicolonInsideADollarQuotedBodyDoesNotSplit(t *testing.T) {
	text := "create function f() returns int as $$ begin; return 1; end $$; select 2"
	eqTexts(t,
		texts(text),
		[]string{
			"create function f() returns int as $$ begin; return 1; end $$",
			"select 2",
		},
	)
	// A lone `$` is not a quote opener.
	eqTexts(t,
		texts("select 1 $ 2; select 3"),
		[]string{"select 1 $ 2", "select 3"},
	)
}

func eqTx(t *testing.T, sql string, want TxVerb, wantOK bool) {
	t.Helper()
	got, ok := TransactionVerb(sql)
	if ok != wantOK || (ok && got != want) {
		t.Fatalf("TransactionVerb(%q) = (%v, %v), want (%v, %v)", sql, got, ok, want, wantOK)
	}
}

func TestTheTransactionVerbsAreReadOffTheFirstWord(t *testing.T) {
	eqTx(t, "BEGIN", TxVerbBegin, true)
	eqTx(t, "begin transaction", TxVerbBegin, true)
	eqTx(t,
		"BEGIN ISOLATION LEVEL SERIALIZABLE",
		TxVerbBegin, true,
	)
	eqTx(t, "start transaction", TxVerbBegin, true)
	eqTx(t, "COMMIT", TxVerbCommit, true)
	eqTx(t, "end", TxVerbCommit, true)
	eqTx(t, "rollback", TxVerbRollback, true)
	eqTx(t, "ABORT", TxVerbRollback, true)
	eqTx(t, "select 1", 0, false)
	eqTx(t, "", 0, false)
}

// The two shapes that read like a verb and are not one. Getting either
// wrong would have the app add a `BEGIN` in front of a transaction
// that is already open, or drop the bar while one still is.
func TestAVerbThatOnlyLooksLikeOneIsNotReadAsOne(t *testing.T) {
	// `ROLLBACK TO SAVEPOINT` unwinds inside the transaction and leaves
	// it open, so it does not end anything.
	eqTx(t, "rollback to savepoint s1", 0, false)
	eqTx(t, "ROLLBACK TO s1", 0, false)
	// `START` on its own is not a transaction verb.
	eqTx(t, "start replication", 0, false)
}

// A comment above the word is part of the statement the splitter hands
// over, so the verb has to be found past it.
func TestACommentInFrontOfTheVerbIsSkipped(t *testing.T) {
	eqTx(t, "-- land it\nCOMMIT", TxVerbCommit, true)
	eqTx(t,
		"/* land it */ commit",
		TxVerbCommit, true,
	)
	eqTx(t, "-- nothing but a note", 0, false)
}

// A `begin` inside a function body is not a statement of the buffer's,
// so it never reaches the verb reader at all.
func TestABeginInsideAFunctionBodyIsNotATransaction(t *testing.T) {
	text := "create function f() returns int as $$ begin return 1; end $$"
	statements := texts(text)
	if len(statements) != 1 {
		t.Fatalf("statements = %q, want one", statements)
	}
	eqTx(t, statements[0], 0, false)
}

func eqCommand(t *testing.T, sql string, want CommandVerb, wantOK bool) {
	t.Helper()
	got, ok := CommandVerbOf(sql)
	if ok != wantOK || (ok && got != want) {
		t.Fatalf("CommandVerbOf(%q) = (%v, %v), want (%v, %v)", sql, got, ok, want, wantOK)
	}
}

// The verb decides one sentence in the result line. A `SELECT` must
// not read as one, or a query returning no rows would be reported as
// having matched nothing.
func TestTheCommandVerbsAreReadOffTheFirstWord(t *testing.T) {
	eqCommand(t,
		"update t set a = 1",
		CommandVerbUpdate, true,
	)
	eqCommand(t,
		"INSERT INTO t VALUES (1)",
		CommandVerbInsert, true,
	)
	eqCommand(t, "delete from t", CommandVerbDelete, true)
	eqCommand(t,
		"Merge into t using s on true",
		CommandVerbMerge, true,
	)
	eqCommand(t, "select 1", 0, false)
	eqCommand(t, "create index on t (a)", 0, false)
	eqCommand(t, "", 0, false)
	eqCommand(t, "-- update t set a = 1", 0, false)
	eqCommand(t,
		"/* note */ delete from t",
		CommandVerbDelete, true,
	)
}

// A statement that changes rows from inside a CTE takes the generic
// wording rather than a verb read out of the middle of it. Reading one
// there would mean parsing SQL, and the count is right either way.
func TestADataModifyingCteReadsAsNoVerb(t *testing.T) {
	eqCommand(t,
		"with moved as (delete from t returning *) insert into u select * from moved",
		0, false,
	)
}

func eqDdl(t *testing.T, sql string, want DdlVerb, wantOK bool) {
	t.Helper()
	got, ok := DdlVerbOf(sql)
	if ok != wantOK || (ok && got != want) {
		t.Fatalf("DdlVerbOf(%q) = (%v, %v), want (%v, %v)", sql, got, ok, want, wantOK)
	}
}

func TestTheDdlVerbIsTheFirstWordPastTheComments(t *testing.T) {
	eqDdl(t, "create table t (a int)", DdlVerbCreate, true)
	eqDdl(t,
		"-- widen it\nALTER TABLE t ADD COLUMN b text",
		DdlVerbAlter, true,
	)
	eqDdl(t, "/* gone */ drop view v", DdlVerbDrop, true)
	eqDdl(t, "truncate t", DdlVerbTruncate, true)
	eqDdl(t, "select * from create_log", 0, false)
	eqDdl(t, "update t set a = 1", 0, false)
	eqDdl(t, "", 0, false)
	eqString(t, DdlVerbAlter.Past(), "altered")
}

func TestAnUnterminatedQuoteSwallowsTheRest(t *testing.T) {
	// Better to send one broken statement and let the database report
	// it than to split in the middle of a string.
	eqTexts(t,
		texts("select 'oops; select 2"),
		[]string{"select 'oops; select 2"},
	)
}
