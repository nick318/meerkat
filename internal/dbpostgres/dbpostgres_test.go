package dbpostgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"
)

func countedOf(calls int64, execMs float64) counted {
	return counted{
		calls:  calls,
		execMs: execMs,
		planMs: 0.,
	}
}

func TestTheServersCursorIsConvertedToAByteOffset(t *testing.T) {
	check := func(sql string, position int, want int, wantOK bool) {
		t.Helper()
		got, ok := byteOffset(sql, position)
		if ok != wantOK || (ok && got != want) {
			t.Fatalf("byteOffset(%q, %d) = (%d, %v), want (%d, %v)", sql, position, got, ok, want, wantOK)
		}
	}
	// One-based characters in, zero-based bytes out.
	check("select 1", 1, 0, true)
	check("select 1", 8, 7, true)
	// Past the last character is the end of the statement, which is
	// what "syntax error at end of input" reports.
	check("select 1", 9, 8, true)
	check("select 1", 10, 0, false)
	// A position of zero is no position at all.
	check("select 1", 0, 0, false)
}

// The whole reason the conversion exists: a multi-byte character
// ahead of the error moves the byte offset past the character count.
func TestAMultiByteCharacterMovesTheOffset(t *testing.T) {
	got, ok := byteOffset("select 'héllo', frm", 17)
	if !ok || got != 17 {
		t.Fatalf("got (%d, %v), want (17, true)", got, ok)
	}
}

// The case the whole delta exists for: one execution landed between
// the two readings, so its own time is knowable and is reported as a
// measurement rather than as an average.
func TestOneExecutionBetweenTwoReadingsIsExact(t *testing.T) {
	before := countedOf(4, 40.)
	timing := between(&before, countedOf(5, 52.5))
	if timing == nil {
		t.Fatal("no timing")
	}
	if timing.ExecMs != 12.5 {
		t.Fatalf("exec_ms = %v", timing.ExecMs)
	}
	if !timing.Exact {
		t.Fatal("not exact")
	}
}

// `pg_stat_statements` is cluster-wide, so somebody else running the
// same statement lands in the same window. The mean over the window is
// still worth showing; calling it this run's time would not be.
func TestSeveralExecutionsInTheWindowAreAMean(t *testing.T) {
	before := countedOf(4, 40.)
	timing := between(&before, countedOf(7, 70.))
	if timing == nil {
		t.Fatal("no timing")
	}
	if timing.ExecMs != 10. {
		t.Fatalf("exec_ms = %v", timing.ExecMs)
	}
	if timing.Exact {
		t.Fatal("exact")
	}
}

// The first run of a statement in a tab. Nothing to subtract from, so
// the answer is the mean over every execution the server has counted —
// and this reading is what makes the next run exact.
func TestTheFirstReadingIsTheMeanSoFar(t *testing.T) {
	timing := between(nil, countedOf(10, 250.))
	if timing == nil {
		t.Fatal("no timing")
	}
	if timing.ExecMs != 25. {
		t.Fatalf("exec_ms = %v", timing.ExecMs)
	}
	if timing.Exact {
		t.Fatal("exact")
	}
}

// Two readings with nothing between them describe no run at all. The
// server did not count the statement — it may not have finished being
// counted yet — and a stale total must not be shown as a fresh one.
func TestNoExecutionBetweenTheReadingsAnswersNothing(t *testing.T) {
	before := countedOf(4, 40.)
	if between(&before, countedOf(4, 40.)) != nil {
		t.Fatal("answered something")
	}
}

// The view was reset, or the entry evicted under
// `pg_stat_statements.max`. There is no delta across that, so the
// reading is taken as a fresh baseline rather than subtracted into a
// negative time.
func TestAResetViewDoesNotProduceANegativeTime(t *testing.T) {
	before := countedOf(900, 9000.)
	timing := between(&before, countedOf(2, 30.))
	if timing == nil {
		t.Fatal("no timing")
	}
	if timing.ExecMs != 15. {
		t.Fatalf("exec_ms = %v", timing.ExecMs)
	}
	if timing.Exact {
		t.Fatal("exact")
	}
	if between(&before, countedOf(0, 0.)) != nil {
		t.Fatal("answered something")
	}
}

// `total_plan_time` is `0` both when planning was free and when
// `pg_stat_statements.track_planning` is off, which is the default. A
// zero is dropped rather than reported as a measurement.
func TestPlanningThatWasNotTrackedIsNotReported(t *testing.T) {
	before := countedOf(1, 10.)
	untracked := between(&before, countedOf(2, 22.))
	if untracked == nil {
		t.Fatal("no timing")
	}
	if untracked.PlanMs != nil {
		t.Fatalf("plan_ms = %v", *untracked.PlanMs)
	}

	tracked := between(
		&counted{
			calls:  1,
			execMs: 10.,
			planMs: 1.,
		},
		counted{
			calls:  2,
			execMs: 22.,
			planMs: 1.5,
		},
	)
	if tracked == nil {
		t.Fatal("no timing")
	}
	if tracked.PlanMs == nil || *tracked.PlanMs != 0.5 {
		t.Fatalf("plan_ms = %v", tracked.PlanMs)
	}
}

func label() Label {
	return Label{
		Database: "shop",
		Host:     "db.internal",
		Port:     5432,
	}
}

func TestASocketErrorIsClassifiedByItsKind(t *testing.T) {
	l := label()
	// Go port: the socket's own error, as the dialer reports it.
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	if kind := connectError(refused, &l).Kind; kind != dbclient.ConnectFailureRefused {
		t.Fatalf("kind = %v", kind)
	}
}

func TestThePoolsDeadlineIsATimeoutThatNamesTheServer(t *testing.T) {
	l := label()
	late := connectError(errPoolTimedOut, &l)
	if late.Kind != dbclient.ConnectFailureTimeout {
		t.Fatalf("kind = %v", late.Kind)
	}
	if !strings.HasPrefix(late.Detail, "db.internal:5432 did not answer within 10 s") {
		t.Fatalf("detail = %q", late.Detail)
	}
	if strings.Contains(late.Detail, "pool") {
		t.Fatalf("detail names a pool: %q", late.Detail)
	}
}

func TestATlsErrorIsTlsFromEitherLayer(t *testing.T) {
	l := label()
	// Go port: pgconn's own words when the server answers the TLS
	// request with "no" (sqlx said "server does not support TLS").
	outright := errors.New("server refused TLS connection")
	if kind := connectError(outright, &l).Kind; kind != dbclient.ConnectFailureTls {
		t.Fatalf("outright: kind = %v", kind)
	}
	// Go port: a certificate the handshake would not accept, as Go's TLS
	// stack reports it (rustls said "invalid peer certificate:
	// UnknownIssuer").
	handshake := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	if kind := connectError(handshake, &l).Kind; kind != dbclient.ConnectFailureTls {
		t.Fatalf("handshake: kind = %v", kind)
	}
}

// No server needed: a port nothing listens on is refused by the
// system itself. Before the knock this took the pool's whole ten
// seconds and came back as a timeout.
func TestAClosedPortIsRefusedAtOnce(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	started := time.Now()
	url := fmt.Sprintf("postgres://nobody@127.0.0.1:%d/nothing", port)
	_, err = Connect(context.Background(), url)
	if err == nil {
		t.Fatal("nothing listens on that port")
	}
	failure := dbclient.ConnectErrorOf(err)
	if failure.Kind != dbclient.ConnectFailureRefused {
		t.Fatalf("kind = %v (%s)", failure.Kind, failure.Detail)
	}
	if !strings.Contains(failure.Detail, fmt.Sprintf("127.0.0.1:%d", port)) {
		t.Fatalf("detail = %q", failure.Detail)
	}
	if time.Since(started) >= 5*time.Second {
		t.Fatal("too slow")
	}
}

// These tests need a live server. Set `MEERKAT_TEST_PG_URL` to a
// database the test may create and drop a schema in; without it the
// test reports that it was skipped.
func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("MEERKAT_TEST_PG_URL")
	if url == "" {
		t.Skip("skipped: set MEERKAT_TEST_PG_URL to run the PostgreSQL tests")
	}
	return url
}

// must is Rust's `.unwrap()`: the value, or the test fails with the error.
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func connectTest(t *testing.T, url string, readOnly bool) *PostgresConnection {
	t.Helper()
	// Values are the server's own text, in the session's zone, so the
	// tests pin the zone rather than inherit the test server's.
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	url += separator + "options[TimeZone]=UTC"
	conn := must(ConnectURL(context.Background(), url, readOnly))
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func execOK(t *testing.T, run func(context.Context, string) (dbclient.QueryResult, error), sql string) dbclient.QueryResult {
	t.Helper()
	return must(run(context.Background(), sql))
}

func execErr(t *testing.T, run func(context.Context, string) (dbclient.QueryResult, error), sql string) string {
	t.Helper()
	_, err := run(context.Background(), sql)
	if err == nil {
		t.Fatalf("%s did not fail", sql)
	}
	return err.Error()
}

func wantValue(t *testing.T, got dbclient.Value, want dbclient.Value) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func text(s string) dbclient.Value { return dbclient.ValueText(s) }

func TestIntrospectAndQuery(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	// This one builds the fixture, so it is the writable session.
	conn := connectTest(t, url, false)

	execOK(t, conn.Execute, "DROP SCHEMA IF EXISTS meerkat_test CASCADE")
	execOK(t, conn.Execute, "CREATE SCHEMA meerkat_test")
	execOK(t, conn.Execute,
		`CREATE TABLE meerkat_test.users (
                 id bigint PRIMARY KEY,
                 email text NOT NULL,
                 name text,
                 mrr numeric(10, 2),
                 active boolean DEFAULT true,
                 tags jsonb,
                 ref uuid,
                 created_at timestamptz,
                 seen_on date
             )`)
	execOK(t, conn.Execute,
		`INSERT INTO meerkat_test.users VALUES
                 (1, 'ada@example.com', 'Ada', 1280.00, true, '{"a":1}',
                  '00000000-0000-0000-0000-000000000001',
                  timestamptz '2026-08-15 09:12:00+00', date '2026-08-15'),
                 (2, 'grace@example.com', NULL, NULL, false, NULL, NULL, NULL, NULL)`)
	execOK(t, conn.Execute, "ANALYZE meerkat_test.users")

	catalog := must(conn.Introspect(ctx))
	var schema *introspect.Schema
	for i := range catalog.Schemas {
		if catalog.Schemas[i].Name == "meerkat_test" {
			schema = &catalog.Schemas[i]
		}
	}
	if schema == nil {
		t.Fatal("test schema is missing from the catalog")
	}
	table := &schema.Tables[0]
	if table.Name != "users" {
		t.Fatalf("name = %q", table.Name)
	}
	if table.Kind != introspect.TableKindTable {
		t.Fatalf("kind = %v", table.Kind)
	}
	if len(table.PrimaryKey) != 1 || table.PrimaryKey[0] != "id" {
		t.Fatalf("primary key = %v", table.PrimaryKey)
	}
	if len(table.Columns) != 9 {
		t.Fatalf("columns = %d", len(table.Columns))
	}
	if table.Columns[1].Name != "email" {
		t.Fatalf("column 1 = %q", table.Columns[1].Name)
	}
	if table.Columns[1].DataType != "text" {
		t.Fatalf("column 1 type = %q", table.Columns[1].DataType)
	}
	if table.Columns[1].Nullable {
		t.Fatal("email is nullable")
	}
	if !table.Columns[2].Nullable {
		t.Fatal("name is not nullable")
	}
	if table.ApproxRows == nil || *table.ApproxRows != 2 {
		t.Fatalf("approx rows = %v", table.ApproxRows)
	}

	result := execOK(t, conn.Execute,
		`SELECT id, email, name, mrr, active, tags, ref, created_at, seen_on
                 FROM meerkat_test.users ORDER BY id`)
	if result.Columns[0] != "id" {
		t.Fatalf("column 0 = %q", result.Columns[0])
	}
	if len(result.Rows) != 2 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
	wantValue(t, result.Rows[0][0], dbclient.ValueInt(1))
	wantValue(t, result.Rows[0][1], text("ada@example.com"))
	// The server's own rendering keeps the declared scale.
	wantValue(t, result.Rows[0][3], text("1280.00"))
	wantValue(t, result.Rows[0][7], text("2026-08-15 09:12:00+00"))
	wantValue(t, result.Rows[0][4], dbclient.ValueBool(true))
	wantValue(t, result.Rows[0][5], text("{\"a\": 1}"))
	wantValue(t, result.Rows[0][6], text("00000000-0000-0000-0000-000000000001"))
	wantValue(t, result.Rows[0][8], text("2026-08-15"))
	wantValue(t, result.Rows[1][2], dbclient.ValueNull{})
	wantValue(t, result.Rows[1][3], dbclient.ValueNull{})

	execOK(t, conn.Execute, "DROP SCHEMA meerkat_test CASCADE")
}

// Everything beside the columns: the indexes, keys, constraints and
// triggers of a table, and the sequences, routines and types of the
// schema — and nothing a key's own enforcement, a `serial` or a range
// type made for itself.
func TestIntrospectReadsEveryObject(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)

	execOK(t, conn.Execute, "DROP SCHEMA IF EXISTS meerkat_objects CASCADE")
	for _, statement := range []string{
		"CREATE SCHEMA meerkat_objects",
		"CREATE TYPE meerkat_objects.mood AS ENUM ('sad', 'ok', 'happy')",
		"CREATE DOMAIN meerkat_objects.price AS numeric(10, 2) CHECK (VALUE >= 0)",
		"CREATE TYPE meerkat_objects.pair AS (a integer, b text)",
		"CREATE SEQUENCE meerkat_objects.ticket AS integer",
		"CREATE TYPE meerkat_objects.span AS RANGE (subtype = integer)",
		"CREATE TABLE meerkat_objects.events (id serial PRIMARY KEY)",
		"CREATE TABLE meerkat_objects.parted (id integer PRIMARY KEY) PARTITION BY RANGE (id)",
		`CREATE TABLE meerkat_objects.parted_a PARTITION OF meerkat_objects.parted
                 FOR VALUES FROM (0) TO (10)`,
		`CREATE TABLE meerkat_objects.parted_b PARTITION OF meerkat_objects.parted
                 FOR VALUES FROM (10) TO (20)`,
		"CREATE TABLE meerkat_objects.pointer (id integer REFERENCES meerkat_objects.parted)",
		"CREATE TABLE meerkat_objects.users (id bigint PRIMARY KEY, email text UNIQUE)",
		`CREATE TABLE meerkat_objects.orders (
                 id bigint PRIMARY KEY,
                 user_id bigint REFERENCES meerkat_objects.users (id),
                 total meerkat_objects.price,
                 CONSTRAINT positive CHECK (id > 0)
             )`,
		"CREATE INDEX orders_user ON meerkat_objects.orders (user_id)",
		`CREATE FUNCTION meerkat_objects.touch() RETURNS trigger LANGUAGE plpgsql
                 AS $$ BEGIN RETURN NEW; END $$`,
		`CREATE FUNCTION meerkat_objects.double(x integer) RETURNS integer
                 LANGUAGE sql AS 'SELECT x * 2'`,
		"CREATE PROCEDURE meerkat_objects.noop() LANGUAGE sql AS 'SELECT 1'",
		`CREATE TRIGGER touch BEFORE INSERT OR UPDATE ON meerkat_objects.orders
                 FOR EACH ROW EXECUTE FUNCTION meerkat_objects.touch()`,
	} {
		execOK(t, conn.Execute, statement)
	}

	catalog := must(conn.Introspect(ctx))
	var schema *introspect.Schema
	for i := range catalog.Schemas {
		if catalog.Schemas[i].Name == "meerkat_objects" {
			schema = &catalog.Schemas[i]
		}
	}
	if schema == nil {
		t.Fatal("test schema is missing from the catalog")
	}
	table := func(name string) *introspect.Table {
		for i := range schema.Tables {
			if schema.Tables[i].Name == name {
				return &schema.Tables[i]
			}
		}
		t.Fatalf("no table %s", name)
		return nil
	}
	orders := table("orders")

	type indexView struct {
		name, definition string
		primary          bool
	}
	var indexes []indexView
	for _, i := range orders.Indexes {
		indexes = append(indexes, indexView{i.Name, i.Definition, i.Primary})
	}
	wantIndexes := []indexView{
		{"orders_pkey", "btree (id)", true},
		{"orders_user", "btree (user_id)", false},
	}
	if fmt.Sprint(indexes) != fmt.Sprint(wantIndexes) {
		t.Fatalf("indexes = %v", indexes)
	}

	if len(orders.ForeignKeys) != 1 {
		t.Fatalf("foreign keys = %d", len(orders.ForeignKeys))
	}
	if detail := orders.ForeignKeys[0].Detail(); detail != "(user_id) → meerkat_objects.users(id)" {
		t.Fatalf("foreign key = %q", detail)
	}
	// The check is a constraint; the key and the primary key are not
	// listed a second time.
	if len(orders.Constraints) != 1 {
		t.Fatalf("constraints = %d", len(orders.Constraints))
	}
	if orders.Constraints[0].Kind != introspect.ConstraintKindCheck {
		t.Fatalf("constraint kind = %v", orders.Constraints[0].Kind)
	}
	if orders.Constraints[0].Definition != "CHECK (id > 0)" {
		t.Fatalf("constraint = %q", orders.Constraints[0].Definition)
	}

	// The foreign key's enforcement triggers are internal and left out.
	// The range type's constructors are not listed as routines either.
	if len(orders.Triggers) != 1 {
		t.Fatalf("triggers = %d", len(orders.Triggers))
	}
	if orders.Triggers[0].Timing != "before insert or update" {
		t.Fatalf("timing = %q", orders.Triggers[0].Timing)
	}
	if orders.Triggers[0].Function != "meerkat_objects.touch()" {
		t.Fatalf("function = %q", orders.Triggers[0].Function)
	}

	// A key into a partitioned table is one key, not one per partition.
	pointer := table("pointer")
	var keys []string
	for _, k := range pointer.ForeignKeys {
		keys = append(keys, k.Name)
	}
	if fmt.Sprint(keys) != fmt.Sprint([]string{"pointer_id_fkey"}) {
		t.Fatalf("keys = %v", keys)
	}

	users := table("users")
	if users.Constraints[0].Kind != introspect.ConstraintKindUnique {
		t.Fatalf("users constraint = %v", users.Constraints[0].Kind)
	}

	// The `serial` column's own sequence is the column's, not listed.
	var sequences []string
	for _, s := range schema.Sequences {
		sequences = append(sequences, s.Name)
	}
	if fmt.Sprint(sequences) != fmt.Sprint([]string{"ticket"}) {
		t.Fatalf("sequences = %v", sequences)
	}
	if schema.Sequences[0].DataType != "integer" {
		t.Fatalf("sequence type = %q", schema.Sequences[0].DataType)
	}

	var routines []string
	for i := range schema.Routines {
		routines = append(routines, schema.Routines[i].Detail())
	}
	wantRoutines := []string{"(x integer) → integer", "procedure · ()", "() → trigger"}
	if strings.Join(routines, "|") != strings.Join(wantRoutines, "|") {
		t.Fatalf("routines = %q", routines)
	}

	var types []string
	for i := range schema.Types {
		types = append(types, schema.Types[i].Detail())
	}
	wantTypes := []string{
		"enum · sad, ok, happy",
		"composite · a integer, b text",
		"domain · numeric(10,2)",
		"range · integer",
	}
	if strings.Join(types, "|") != strings.Join(wantTypes, "|") {
		t.Fatalf("types = %q", types)
	}

	execOK(t, conn.Execute, "DROP SCHEMA meerkat_objects CASCADE")
}

func TestATriggerSaysWhenItFires(t *testing.T) {
	// ROW | BEFORE | INSERT | UPDATE
	if got := triggerTiming(1 | 2 | 4 | 16); got != "before insert or update" {
		t.Fatalf("got %q", got)
	}
	// AFTER DELETE, once per statement.
	if got := triggerTiming(8); got != "after delete · statement" {
		t.Fatalf("got %q", got)
	}
	// INSTEAD OF INSERT on a view, per row.
	if got := triggerTiming(1 | 4 | 64); got != "instead of insert" {
		t.Fatalf("got %q", got)
	}
}

func TestEveryTypeReadsAsPostgresPrintsIt(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))
	for _, statement := range []string{
		"CREATE TYPE pg_temp.mood AS ENUM ('ok', 'not ok')",
		"CREATE TYPE pg_temp.pair AS (a int, b text)",
	} {
		execOK(t, session.Execute, statement)
	}

	// Each of these either painted `<TYPE>` or failed the whole result
	// when the run asked for every column in binary.
	cases := []struct {
		sql  string
		want dbclient.Value
	}{
		{"point(1, 2)", text("(1,2)")},
		{"interval '1 day 02:03:04'", text("1 day 02:03:04")},
		{"'192.168.0.1/24'::inet", text("192.168.0.1/24")},
		{"'::1'::cidr", text("::1/128")},
		{"'08:00:2b:01:02:03'::macaddr", text("08:00:2b:01:02:03")},
		{"B'1010'", text("1010")},
		{"'12:00:00+02'::timetz", text("12:00:00+02")},
		{"int4range(1, 10)", text("[1,10)")},
		{"'{[1,3),[5,7)}'::int4multirange", text("{[1,3),[5,7)}")},
		{"row(1, 'a')", text("(1,a)")},
		{"row(1, 'x')::pg_temp.pair", text("(1,x)")},
		{"'ok'::pg_temp.mood", text("ok")},
		{"'pg_class'::regclass", text("pg_class")},
		{"'a'::\"char\"", text("a")},
		{"'<a>x</a>'::xml", text("<a>x</a>")},
		{"'infinity'::timestamp", text("infinity")},
		{"'-infinity'::timestamptz", text("-infinity")},
		{"'infinity'::date", text("infinity")},
		{"'0001-01-01 BC'::date", text("0001-01-01 BC")},
		{"'24:00:00'::time", text("24:00:00")},
		{"'NaN'::numeric", text("NaN")},
		{"'Infinity'::numeric", text("Infinity")},
		{"'-Infinity'::numeric", text("-Infinity")},
		{"0.000001::numeric", text("0.000001")},
		{"'Infinity'::float8", text("Infinity")},
		{"0.1::float4", text("0.1")},
		{"'{\"n\": 10.00}'::jsonb", text(`{"n": 10.00}`)},
		{"12345::oid", dbclient.ValueInt(12345)},
		{"4294967295::oid", dbclient.ValueInt(4294967295)},
		{"5::int2", dbclient.ValueInt(5)},
		{"'\\x00ff'::bytea", dbclient.ValueBytes{0x00, 0xff}},
		{"true", dbclient.ValueBool(true)},
	}
	for _, c := range cases {
		result, err := session.Execute(ctx, "SELECT "+c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if !result.Rows[0][0].Equal(c.want) {
			t.Errorf("%s = %#v, want %#v", c.sql, result.Rows[0][0], c.want)
		}
	}
}

// A transaction id is read in binary and stays a number.
func TestTransactionIdsAndTsvectorsReadAsValues(t *testing.T) {
	url := testURL(t)
	conn := connectTest(t, url, true)

	result := execOK(t, conn.Execute,
		`SELECT '42'::xid AS x,
                        '4294967300'::xid8 AS x8,
                        to_tsvector('english', 'The quick brown fox') AS v,
                        setweight(to_tsvector('english', 'quick fox'), 'A') AS w,
                        'a''b'::tsvector AS q`)
	wantValue(t, result.Rows[0][0], dbclient.ValueInt(42))
	wantValue(t, result.Rows[0][1], dbclient.ValueInt(4294967300))
	wantValue(t, result.Rows[0][2], text("'brown':3 'fox':4 'quick':2"))
	// The weight is the top two bits of the position, so a wrong
	// reading would put the letter on the wrong lexeme or lose it.
	wantValue(t, result.Rows[0][3], text("'fox':2A 'quick':1A"))
	wantValue(t, result.Rows[0][4], text("'a''b'"))
}

// An array is the server's own text, so each cell here must read the
// way `psql` prints it — braces, quotes and all.
func TestArraysReadAsPostgresPrintsThem(t *testing.T) {
	url := testURL(t)
	conn := connectTest(t, url, true)

	result := execOK(t, conn.Execute,
		`SELECT ARRAY['a', 'b c', NULL, '', 'NULL', 'x,y', 'q"q', 'b\s']::text[],
                          '{}'::text[],
                          '{{1,2},{3,4}}'::int4[],
                          '[0:1]={7,8}'::int8[],
                          ARRAY[true, false],
                          ARRAY[1.50, -0.001, 12345678.9, 0]::numeric[],
                          ARRAY['NaN', 'Infinity']::float8[],
                          ARRAY['2024-02-29', 'infinity']::date[],
                          ARRAY['1999-12-31 23:59:59.5']::timestamp[],
                          ARRAY['2024-01-01 00:00:00+00']::timestamptz[],
                          ARRAY['24:00:00', '08:30:00']::time[],
                          ARRAY['a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11']::uuid[],
                          ARRAY['{"k": 1}']::jsonb[],
                          ARRAY['\x0102']::bytea[],
                          ARRAY['abc']::varchar[],
                          ARRAY[point(1, 2)],
                          ARRAY['10000-01-01', '0001-01-01 BC']::date[],
                          ARRAY['0001-01-01 00:00:00 BC']::timestamp[],
                          ARRAY[0.1, 1234567]::float4[],
                          ARRAY[1e300, 1e-05]::float8[],
                          '1 3'::int2vector`)
	cells := []string{
		`{a,"b c",NULL,"","NULL","x,y","q\"q","b\\s"}`,
		"{}",
		"{{1,2},{3,4}}",
		"[0:1]={7,8}",
		"{t,f}",
		"{1.50,-0.001,12345678.9,0}",
		"{NaN,Infinity}",
		"{2024-02-29,infinity}",
		`{"1999-12-31 23:59:59.5"}`,
		`{"2024-01-01 00:00:00+00"}`,
		"{24:00:00,08:30:00}",
		"{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}",
		`{"{\"k\": 1}"}`,
		`{"\\x0102"}`,
		"{abc}",
		`{"(1,2)"}`,
		`{10000-01-01,"0001-01-01 BC"}`,
		`{"0001-01-01 00:00:00 BC"}`,
		"{0.1,1.234567e+06}",
		"{1e+300,1e-05}",
		"1 3",
	}
	if len(result.Rows[0]) != len(cells) {
		t.Fatalf("cells = %d, want %d", len(result.Rows[0]), len(cells))
	}
	for i, cell := range cells {
		wantValue(t, result.Rows[0][i], text(cell))
	}
}

// An enum has no fixed name to match on, and a domain is its base
// type, so an array of either — and a domain over an array — is read
// through the type's kind.
func TestArraysOfEnumsAndDomainsReadTheirElements(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))
	for _, statement := range []string{
		"CREATE TYPE pg_temp.mood AS ENUM ('ok', 'not ok')",
		"CREATE DOMAIN pg_temp.small AS int4 CHECK (VALUE < 100)",
		"CREATE DOMAIN pg_temp.tags AS text[]",
	} {
		execOK(t, session.Execute, statement)
	}

	result := execOK(t, session.Execute,
		`SELECT ARRAY['ok', 'not ok']::pg_temp.mood[],
                        ARRAY[1, 2]::pg_temp.small[],
                        ARRAY['a', 'b']::pg_temp.tags`)
	want := []dbclient.Value{
		text(`{ok,"not ok"}`),
		text("{1,2}"),
		text("{a,b}"),
	}
	if len(result.Rows[0]) != len(want) {
		t.Fatalf("cells = %d", len(result.Rows[0]))
	}
	for i := range want {
		wantValue(t, result.Rows[0][i], want[i])
	}
}

func TestEmptyResultKeepsItsColumns(t *testing.T) {
	url := testURL(t)
	conn := connectTest(t, url, true)

	result := execOK(t, conn.Execute, "SELECT 1 AS x WHERE false")
	if len(result.Rows) != 0 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
	if fmt.Sprint(result.Columns) != fmt.Sprint([]string{"x"}) {
		t.Fatalf("columns = %v", result.Columns)
	}
}

// A statement that changes rows comes back as a count and no result
// set, and the count is the only thing it ever says. `fetch` threw the
// completion tag away, so such a statement answered with nothing at
// all — no rows, no columns and no number.
func TestAStatementThatChangesRowsReportsHowMany(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	// Writable, because this writes.
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))

	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_affected (id int)")
	inserted := execOK(t, session.Execute, "INSERT INTO meerkat_affected VALUES (1), (2), (3)")
	if inserted.RowsAffected != 3 {
		t.Fatalf("inserted = %d", inserted.RowsAffected)
	}
	// No result set: the columns are what the app reads to know there
	// is no grid to paint.
	if len(inserted.Columns) != 0 {
		t.Fatalf("%v", inserted.Columns)
	}

	updated := execOK(t, session.Execute, "UPDATE meerkat_affected SET id = id + 1 WHERE id > 1")
	if updated.RowsAffected != 2 {
		t.Fatalf("updated = %d", updated.RowsAffected)
	}

	// The reading that matters: the statement worked and matched
	// nothing, which is a different answer from an empty table.
	matchedNothing := execOK(t, session.Execute, "DELETE FROM meerkat_affected WHERE id = 999")
	if matchedNothing.RowsAffected != 0 {
		t.Fatalf("matched nothing = %d", matchedNothing.RowsAffected)
	}
	if len(matchedNothing.Columns) != 0 {
		t.Fatalf("%v", matchedNothing.Columns)
	}

	// A statement with a `RETURNING` clause is a result set as well as
	// a count, and it keeps both.
	returning := execOK(t, session.Execute, "DELETE FROM meerkat_affected RETURNING id")
	if returning.RowsAffected != 3 {
		t.Fatalf("returning = %d", returning.RowsAffected)
	}
	if fmt.Sprint(returning.Columns) != fmt.Sprint([]string{"id"}) {
		t.Fatalf("columns = %v", returning.Columns)
	}
	if len(returning.Rows) != 3 {
		t.Fatalf("rows = %d", len(returning.Rows))
	}
}

// A `SELECT` is counted by the server too — its tag is `SELECT 5` — so
// the count cannot be what says a statement changed anything. The
// columns are, which is why the app reads `rows_affected` only where
// there are none.
func TestAQueryIsCountedAsWellAndKeepsItsColumns(t *testing.T) {
	url := testURL(t)
	conn := connectTest(t, url, true)

	result := execOK(t, conn.Execute, "SELECT * FROM generate_series(1, 5)")
	if result.RowsAffected != 5 {
		t.Fatalf("rows affected = %d", result.RowsAffected)
	}
	if len(result.Columns) != 1 {
		t.Fatalf("columns = %d", len(result.Columns))
	}
}

// The client's own split, against a real server. Every part of it has
// to be there for the app to say anything but a wall clock.
func TestAResultCarriesWhereItsTimeWent(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))

	wire := execOK(t, session.Execute, "SELECT * FROM generate_series(1, 20000)").Wire
	if wire.LinkMs == nil {
		t.Fatal("the session did not time its round trip")
	}
	if wire.FirstRowMs == nil {
		t.Fatal("no row was timed")
	}
	if wire.FetchMs == nil {
		t.Fatal("the fetch was not timed")
	}

	// A statement with no rows has no fetch to measure, and `0` would
	// be a different claim from "there were none".
	none := execOK(t, session.Execute, "SELECT 1 WHERE false").Wire
	if none.FirstRowMs != nil {
		t.Fatalf("first row = %d", *none.FirstRowMs)
	}
	if none.FetchMs != nil {
		t.Fatalf("fetch = %d", *none.FetchMs)
	}
}

// The server's own figure, end to end: `pg_stat_activity.query_id`
// naming the statement, the join to `pg_stat_statements`, and the
// delta between two readings turning running totals into one run.
//
// It self-skips where the extension is not installed, which is the
// same answer the app gives there: the client's split still stands.
func TestTheServerReportsWhatItSpent(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))

	execOK(t, session.Execute, "SELECT pg_sleep(0.2)")
	first := must(session.ServerTiming(ctx))
	if first == nil {
		t.Skip("skipped: this server has no pg_stat_statements")
	}
	// No earlier reading, so this is the mean over every execution the
	// server has counted — and it must say so.
	if first.Exact {
		t.Fatal("a first reading cannot be one run's own time")
	}

	// With a baseline in hand, the next run of the same statement is
	// exact, and it is the sleep the statement asked for.
	execOK(t, session.Execute, "SELECT pg_sleep(0.2)")
	second := must(session.ServerTiming(ctx))
	if second == nil {
		t.Fatal("no second reading")
	}
	if !second.Exact {
		t.Fatal("one run between two readings is exact")
	}
	if second.ExecMs < 150. || second.ExecMs >= 600. {
		t.Fatalf("a 200 ms sleep was reported as %v ms", second.ExecMs)
	}
}

// The whole of the checker: the server refuses the text and names
// the character it stopped at, and nothing runs.
func TestASyntaxErrorComesBackWithItsPosition(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)

	if got := must(conn.Check(ctx, "SELECT 1")); got != nil {
		t.Fatalf("got %+v", got)
	}

	sql := "SELECT 1 FROM"
	checked := must(conn.Check(ctx, sql))
	if checked == nil {
		t.Fatal("no error for an unfinished statement")
	}
	if !strings.Contains(checked.Message, "syntax error") {
		t.Fatal(checked.Message)
	}
	// "at end of input": one past the last character.
	if checked.Offset == nil || *checked.Offset != len(sql) {
		t.Fatalf("offset = %v", checked.Offset)
	}

	sql = "SELECT 1 frm t"
	checked = must(conn.Check(ctx, sql))
	if checked == nil {
		t.Fatal("no error for a broken statement")
	}
	if checked.Offset == nil || sql[*checked.Offset:] != "t" {
		t.Fatalf("offset = %v", checked.Offset)
	}
}

// A name the server cannot resolve comes back too, told apart from a
// syntax error because it is only true of the connection it was asked
// on.
func TestAnUnknownNameIsReportedAsAName(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)

	sql := "SELECT * FROM public.no_such_table_here"
	checked := must(conn.Check(ctx, sql))
	if checked == nil {
		t.Fatal("no error for a table that is not there")
	}
	if checked.Refusal != dbclient.RefusalName {
		t.Fatalf("refusal = %v", checked.Refusal)
	}
	if checked.Offset == nil || sql[*checked.Offset:] != "public.no_such_table_here" {
		t.Fatalf("offset = %v", checked.Offset)
	}

	// A column, a function and a type answer the same way.
	for _, sql := range []string{
		"SELECT no_such_column FROM pg_class",
		"SELECT no_such_fn(1)",
		"SELECT 1::no_such_ty",
	} {
		checked := must(conn.Check(ctx, sql))
		if checked == nil {
			t.Fatalf("%s was not refused", sql)
		}
		if checked.Refusal != dbclient.RefusalName {
			t.Fatalf("%s: refusal = %v", sql, checked.Refusal)
		}
	}
}

// **The statements `check` goes quiet about.** Postgres resolves no
// names for a utility statement until it runs one, so the parse says
// nothing and `to_regclass` is what answers instead.
func TestAUtilityStatementParsesWhateverItNames(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)

	for _, sql := range []string{
		"DROP TABLE public.no_such_table_here",
		"ALTER TABLE public.no_such_table_here ADD COLUMN c int",
		"TRUNCATE public.no_such_table_here",
	} {
		if got := must(conn.Check(ctx, sql)); got != nil {
			t.Fatalf("%s: %+v", sql, got)
		}
	}
	exists, known, err := conn.RelationExists(ctx, "public.no_such_table_here")
	if err != nil || !known || exists {
		t.Fatalf("got (%v, %v, %v), want Some(false)", exists, known, err)
	}
}

// `to_regclass` resolves a name the way the parser does, which is the
// whole reason it is the call rather than a query over `pg_class`.
func TestTheServerSaysWhetherItHasARelation(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)

	want := func(name string, wantExists bool) {
		t.Helper()
		exists, known, err := conn.RelationExists(ctx, name)
		if err != nil || !known || exists != wantExists {
			t.Fatalf("%s: got (%v, %v, %v), want Some(%v)", name, exists, known, err, wantExists)
		}
	}
	want("pg_catalog.pg_class", true)
	// Unqualified, so it goes through `search_path`.
	want("pg_class", true)
	// Unquoted parts fold to lower case, as SQL does.
	want("PG_CLASS", true)
	// A quoted one does not, so this is a different name.
	want("\"PG_CLASS\"", false)
	want("public.no_such_table_here", false)
	// A name that is not a name answers rather than raising.
	want("a b c", false)
}

// A bare `$1` is a placeholder the user typed on purpose, not a name
// they got wrong. The server refuses to infer its type, and that
// refusal is not a mark.
func TestAPlaceholderIsNotAnError(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)

	if got := must(conn.Check(ctx, "SELECT $1")); got != nil {
		t.Fatalf("got %+v", got)
	}
}

// **The reason a session checks for itself.** A temp table lives on
// one connection, so the pool cannot see it and the tab that made it
// can. Marking it as missing would be a warning about nothing.
func TestASessionResolvesWhatItMadeItself(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	// Read-only refuses a temp table, so this one asks to write.
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))
	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_check_probe (id int)")

	sql := "SELECT * FROM meerkat_check_probe"
	if got := must(session.Check(ctx, sql)); got != nil {
		t.Fatalf("session: %+v", got)
	}
	// The pool is a different connection, and it cannot see it.
	pooled := must(conn.Check(ctx, sql))
	if pooled == nil {
		t.Fatal("the pool saw a temp table")
	}
	if pooled.Refusal != dbclient.RefusalName {
		t.Fatalf("refusal = %v", pooled.Refusal)
	}

	// And the same split for the name a `DROP TABLE` would be marked
	// on, which is the whole reason the probe follows the session too.
	exists, known, err := session.RelationExists(ctx, "meerkat_check_probe")
	if err != nil || !known || !exists {
		t.Fatalf("session: got (%v, %v, %v), want Some(true)", exists, known, err)
	}
	exists, known, err = conn.RelationExists(ctx, "meerkat_check_probe")
	if err != nil || !known || exists {
		t.Fatalf("pool: got (%v, %v, %v), want Some(false)", exists, known, err)
	}

	session.Close(ctx)
}

// Nothing is executed: the statement is prepared and thrown away.
func TestCheckingAWriteWritesNothing(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)

	if got := must(conn.Check(ctx, "DROP TABLE IF EXISTS pg_class")); got != nil {
		t.Fatalf("got %+v", got)
	}
	// Still there, so nothing ran.
	result := execOK(t, conn.Execute, "SELECT count(*) FROM pg_class")
	if len(result.Rows) != 1 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
}

func TestErrorsAreReportedNotPanicked(t *testing.T) {
	url := testURL(t)
	conn := connectTest(t, url, true)

	message := execErr(t, conn.Execute, "SELECT * FROM no_such_table_here")
	if !strings.Contains(message, "no_such_table_here") {
		t.Fatal(message)
	}
}

func TestAReadOnlySessionRefusesDdlAndStillReads(t *testing.T) {
	url := testURL(t)
	conn := connectTest(t, url, true)

	// The server is what refuses it, so the message is the server's.
	message := execErr(t, conn.Execute, "CREATE TABLE meerkat_read_only_probe (id int)")
	if !strings.Contains(message, "read-only transaction") {
		t.Fatal(message)
	}

	// Reading is the whole point of the session, and it still works.
	wantValue(t, execOK(t, conn.Execute, "SELECT 1 AS x").Rows[0][0], dbclient.ValueInt(1))
}

type runOutcome struct {
	result dbclient.QueryResult
	err    error
}

// The whole point of the backend id: a statement that would run for
// half a minute is stopped in the middle, from another connection,
// with only the id the session named when it opened.
func TestARunningStatementCanBeCancelledFromOutside(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))
	// The whole race is gone: a session names its backend before it is
	// ever asked to run anything, so a stop has an id to aim at from
	// the moment the run leaves. There is nothing to poll for.
	id, ok := session.Backend()
	if !ok {
		t.Fatal("no backend")
	}

	runner := make(chan runOutcome, 1)
	go func() {
		result, err := session.Execute(ctx, "SELECT pg_sleep(30)")
		runner <- runOutcome{result, err}
	}()
	started := time.Now()
	// Let the statement reach the server before asking it to stop.
	time.Sleep(100 * time.Millisecond)

	if !must(conn.Stop(ctx, id, dbclient.StopCancel)) {
		t.Fatal("the server refused the cancel")
	}
	outcome := <-runner
	if outcome.err == nil {
		t.Fatal("the statement was not stopped")
	}
	if !strings.Contains(outcome.err.Error(), "canceling statement") {
		t.Fatal(outcome.err)
	}
	// It slept for 30 seconds and this test did not.
	if time.Since(started) >= 10*time.Second {
		t.Fatal("the cancel did not land")
	}

	// The session outlives a cancelled statement, as the tab holding
	// it expects: a cancel ends the statement, not the connection.
	wantValue(t, execOK(t, session.Execute, "SELECT 1 AS x").Rows[0][0], dbclient.ValueInt(1))
}

// The second press. A terminated backend takes its connection with
// it, so the runner hears about it as a broken connection rather than
// as a cancelled statement — which is why the app only offers this
// after a cancel has already been asked for.
func TestARunningStatementCanBeTerminated(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))
	id, _ := session.Backend()

	runner := make(chan runOutcome, 1)
	go func() {
		result, err := session.Execute(ctx, "SELECT pg_sleep(30)")
		runner <- runOutcome{result, err}
	}()
	started := time.Now()
	time.Sleep(100 * time.Millisecond)

	if !must(conn.Stop(ctx, id, dbclient.StopTerminate)) {
		t.Fatal("the server refused the terminate")
	}
	if outcome := <-runner; outcome.err == nil {
		t.Fatal("the statement outlived its backend")
	}
	if time.Since(started) >= 10*time.Second {
		t.Fatal("the terminate did not land")
	}

	// The app pool is still good afterwards: it throws the dead
	// connection away and opens another.
	wantValue(t, execOK(t, conn.Execute, "SELECT 1 AS x").Rows[0][0], dbclient.ValueInt(1))
}

// The requirement the cap is sized for: a narrow result of 120,000
// rows is not a large result, and nothing must cut it short.
func TestAHundredAndTwentyThousandNarrowRowsComeBackWhole(t *testing.T) {
	url := testURL(t)
	conn := connectTest(t, url, true)

	result := execOK(t, conn.Execute, "SELECT g, g::text AS label FROM generate_series(1, 120000) g")
	if len(result.Rows) != 120_000 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
	if result.Truncated {
		t.Fatal("120,000 narrow rows were capped")
	}
}

// The shape that used to take the app down. The read stops at the
// budget, the statement comes back `Ok` marked truncated rather than as
// an error, and the connection is still good afterwards — the cancel
// that ended it must not poison the pool.
func TestAResultPastTheBudgetIsCappedNotFailed(t *testing.T) {
	url := testURL(t)
	limits := dbclient.Limits{
		MaxBytes:     64 * 1024,
		MaxCellBytes: dbclient.MaxCellBytes,
	}
	conn := connectTest(t, url, true).WithLimits(limits)

	// Ten million rows, of which the budget holds a few hundred.
	result := execOK(t, conn.Execute, "SELECT g, repeat('x', 100) FROM generate_series(1, 10000000) g")
	if !result.Truncated {
		t.Fatal("the result was not marked truncated")
	}
	if len(result.Rows) == 0 {
		t.Fatal("a capped result must still show rows")
	}
	if len(result.Rows) >= 10_000_000 {
		t.Fatal("the whole result came back")
	}
	if len(result.Columns) != 2 {
		t.Fatalf("columns = %d", len(result.Columns))
	}

	wantValue(t, execOK(t, conn.Execute, "SELECT 1 AS x").Rows[0][0], dbclient.ValueInt(1))
}

// One value must not eat the whole budget. The cut value says it was
// cut, so the cell is not read as the whole string.
func TestOneHugeValueIsCutToTheCellCap(t *testing.T) {
	url := testURL(t)
	limits := dbclient.Limits{
		MaxBytes:     dbclient.MaxBytes,
		MaxCellBytes: 4096,
	}
	conn := connectTest(t, url, true).WithLimits(limits)

	result := execOK(t, conn.Execute, "SELECT repeat('x', 2000000) AS wide")
	cell := result.Rows[0][0]
	textCell, ok := cell.(dbclient.ValueText)
	if !ok {
		t.Fatalf("not text: %#v", cell)
	}
	if len(textCell) != 4096+len("…") {
		t.Fatalf("len = %d", len(textCell))
	}
	if !strings.HasSuffix(string(textCell), "…") {
		t.Fatal("does not end in …")
	}
	// One row, and it fitted: the cell cap is not the byte cap.
	if result.Truncated {
		t.Fatal("truncated")
	}
}

// A run nobody set a timeout for gets the app's, and the server is
// what ends it — the message is the server's own.
func TestARunGetsTheAppsTimeoutWhenNothingElseSetOne(t *testing.T) {
	url := testURL(t)
	conn := connectTest(t, url, true).WithStatementTimeout("100ms")

	message := execErr(t, conn.Execute, "SELECT pg_sleep(5)")
	if !strings.Contains(message, "statement timeout") {
		t.Fatal(message)
	}
	// The connection is still good: a timeout ends the statement, not
	// the backend.
	wantValue(t, execOK(t, conn.Execute, "SELECT 1 AS x").Rows[0][0], dbclient.ValueInt(1))
}

// The question the guard has to answer: was this the user's choice or
// nobody's? A timeout in the connection URL reads as `client` in
// `pg_settings.source`, so the app leaves it alone — even though its
// own is a hundred times shorter.
func TestATimeoutTheSessionWasOpenedWithIsLeftAlone(t *testing.T) {
	url := testURL(t)
	joiner := "?"
	if strings.Contains(url, "?") {
		joiner = "&"
	}
	url = fmt.Sprintf("%s%soptions=-c%%20statement_timeout%%3D20s", url, joiner)
	conn := connectTest(t, url, true).WithStatementTimeout("100ms")

	// Well past the app's 100 ms, well inside the session's own 20 s.
	execOK(t, conn.Execute, "SELECT pg_sleep(0.5)")
	result := execOK(t, conn.Execute, "SELECT setting, source FROM pg_settings WHERE name = 'statement_timeout'")
	wantValue(t, result.Rows[0][0], text("20000"))
	wantValue(t, result.Rows[0][1], text("client"))
}

// What the session is for. Two statements a buffer sends in turn are
// one connection apart, so the second sees what the first set. On the
// pool this was luck: each statement took whichever connection was
// free, and `SET` reached whichever backend that was.
func TestAStatementSeesWhatTheStatementBeforeItSet(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))

	execOK(t, session.Execute, "SET statement_timeout = '5min'")
	result := execOK(t, session.Execute, "SELECT setting, source FROM pg_settings WHERE name = 'statement_timeout'")
	wantValue(t, result.Rows[0][0], text("300000"))
	wantValue(t, result.Rows[0][1], text("session"))

	// ...and a second session is a second connection, so it is not
	// carrying the first one's settings.
	other := must(conn.OpenSession(ctx))
	result = execOK(t, other.Execute, "SELECT setting FROM pg_settings WHERE name = 'statement_timeout'")
	if result.Rows[0][0].Equal(text("300000")) {
		t.Fatal("the second session carries the first one's setting")
	}
}

// A transaction spans statements, which is the thing the pool could
// not do at all: `BEGIN` on one connection and the next statement on
// another is not a transaction, it is two.
func TestATransactionSpansTheStatementsOfASession(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	// Writable, because a temp table is a write.
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))

	execOK(t, session.Execute, "BEGIN")
	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_session_probe (id int)")
	execOK(t, session.Execute, "INSERT INTO meerkat_session_probe VALUES (1), (2)")
	result := execOK(t, session.Execute, "SELECT count(*) FROM meerkat_session_probe")
	wantValue(t, result.Rows[0][0], dbclient.ValueInt(2))

	execOK(t, session.Execute, "ROLLBACK")
	// The table went with the transaction, and the session is still
	// good enough to say so.
	message := execErr(t, session.Execute, "SELECT * FROM meerkat_session_probe")
	if !strings.Contains(message, "meerkat_session_probe") {
		t.Fatal(message)
	}
}

// Closing rolls back, and the point is the **pool**, not the server:
// a closing socket already rolls a transaction back. This connection
// is about to be reused, and must not carry the user's half-finished
// transaction to whatever acquires it next.
func TestClosingASessionRollsItsTransactionBack(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))
	backend, ok := session.Backend()
	if !ok {
		t.Fatal("a Postgres session knows its backend")
	}
	pid := int32(backend)

	execOK(t, session.Execute, "BEGIN")
	execOK(t, session.Execute, "SELECT 1")
	if state := backendState(t, conn, pid); state != "idle in transaction" {
		t.Fatalf("state = %q", state)
	}

	session.Close(ctx)
	if state := backendState(t, conn, pid); state != "idle" {
		t.Fatalf("state = %q", state)
	}

	// And it says so rather than pretending to run.
	message := execErr(t, session.Execute, "SELECT 1")
	if !strings.Contains(message, "closed") {
		t.Fatal(message)
	}
}

// What the close dialog asks before it asks the user. It is read off the
// status byte the server sends after every statement, so it answers about
// a session that has stopped being able to answer for itself.
func TestASessionSaysWhetherItIsInATransaction(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))

	if must(session.InTransaction(ctx)) {
		t.Fatal("in a transaction before BEGIN")
	}
	execOK(t, session.Execute, "BEGIN")
	if !must(session.InTransaction(ctx)) {
		t.Fatal("not in a transaction after BEGIN")
	}

	// A transaction that has gone wrong refuses every statement until
	// it is rolled back, so the session in the state most worth
	// reporting is the one that cannot report it. Asking elsewhere is
	// what makes this answerable at all.
	execErr(t, session.Execute, "SELECT no_such_column")
	if !must(session.InTransaction(ctx)) {
		t.Fatal("not in a transaction after a failed statement")
	}

	execOK(t, session.Execute, "ROLLBACK")
	if must(session.InTransaction(ctx)) {
		t.Fatal("in a transaction after ROLLBACK")
	}
}

// After a DDL lands the shell reads the catalog down the tab's session
// and asks whether a transaction is open, both at once. The catalog read
// is the app's own work and opens nothing, so it must not read as a
// transaction — it did, when the answer came from `pg_stat_activity`,
// and a committed `ALTER` in auto mode wore a commit bar. Inside a
// block, the same read must not make the transaction look closed.
func TestTheAppsOwnCatalogReadIsNotATransaction(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))
	defer session.Close(ctx)
	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_probe_shape (id int)")

	// Every answer given while the read is out, polled as fast as the
	// probe can go so a one-statement window is not missed.
	during := func() []bool {
		done := make(chan error)
		go func() {
			_, err := session.Introspect(ctx)
			done <- err
		}()
		var seen []bool
		for {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
				return seen
			default:
				seen = append(seen, must(session.InTransaction(ctx)))
			}
		}
	}

	for range 5 {
		execOK(t, session.Execute, "ALTER TABLE meerkat_probe_shape ADD COLUMN IF NOT EXISTS c int")
		for _, open := range during() {
			if open {
				t.Fatal("an auto-commit session read as in a transaction during the catalog read")
			}
		}
	}

	execOK(t, session.Execute, "BEGIN")
	execOK(t, session.Execute, "ALTER TABLE meerkat_probe_shape ADD COLUMN IF NOT EXISTS d int")
	for _, open := range during() {
		if !open {
			t.Fatal("an open transaction read as closed during the catalog read")
		}
	}
	execOK(t, session.Execute, "ROLLBACK")
	if must(session.InTransaction(ctx)) {
		t.Fatal("in a transaction after ROLLBACK")
	}
}

// What manual mode is made of. The app opens the transaction, the
// statements of several runs land inside it, and the user's word ends
// it — so the boundaries have to work without going through `execute`,
// which is the path with the cap and the timing on it.
func TestASessionOpensAndEndsATransactionOnRequest(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))

	if err := session.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if !must(session.InTransaction(ctx)) {
		t.Fatal("not in a transaction after begin")
	}
	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_manual_probe (id int)")
	execOK(t, session.Execute, "INSERT INTO meerkat_manual_probe VALUES (1)")
	// Still open across the runs, which is the whole of manual mode.
	if !must(session.InTransaction(ctx)) {
		t.Fatal("not in a transaction across the runs")
	}

	if _, err := session.EndTransaction(ctx, dbclient.TxEndRollback); err != nil {
		t.Fatal(err)
	}
	if must(session.InTransaction(ctx)) {
		t.Fatal("in a transaction after the rollback")
	}
	message := execErr(t, session.Execute, "SELECT * FROM meerkat_manual_probe")
	if !strings.Contains(message, "meerkat_manual_probe") {
		t.Fatal(message)
	}

	// And a commit keeps what the transaction did. A temp table lives
	// as long as the session, so it is proof enough without writing to
	// anything the server keeps.
	if err := session.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_manual_kept (id int)")
	if _, err := session.EndTransaction(ctx, dbclient.TxEndCommit); err != nil {
		t.Fatal(err)
	}
	if must(session.InTransaction(ctx)) {
		t.Fatal("in a transaction after the commit")
	}
	execOK(t, session.Execute, "SELECT * FROM meerkat_manual_kept")
}

// A table created inside an open transaction exists for the connection
// that made it and for nobody else. The session's own catalog read is
// what lets the app say what a `CREATE` did before it is committed;
// the pool's read is what keeps that table out of the sidebar until
// it is. Its own schema, so it cannot trip over the fixture the other
// tests build and drop.
func TestASessionReadsTheCatalogItCanSee(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	execOK(t, conn.Execute, "DROP SCHEMA IF EXISTS meerkat_shape CASCADE")
	execOK(t, conn.Execute, "CREATE SCHEMA meerkat_shape")
	session := must(conn.OpenSession(ctx))

	if err := session.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	execOK(t, session.Execute, "CREATE TABLE meerkat_shape.pending (id int, note text)")

	hasPending := func(catalog *introspect.Catalog) bool {
		for _, s := range catalog.Schemas {
			if s.Name == "meerkat_shape" {
				for _, table := range s.Tables {
					if table.Name == "pending" {
						return true
					}
				}
				return false
			}
		}
		return false
	}
	seen := must(session.Introspect(ctx))
	if seen == nil {
		t.Fatal("a session reads the catalog")
	}
	if !hasPending(seen) {
		t.Fatal("the session sees its own table")
	}
	pooled := must(conn.Introspect(ctx))
	if hasPending(&pooled) {
		t.Fatal("the pool does not, until it commits")
	}

	if _, err := session.EndTransaction(ctx, dbclient.TxEndRollback); err != nil {
		t.Fatal(err)
	}
	after := must(session.Introspect(ctx))
	if after == nil {
		t.Fatal("a session reads the catalog")
	}
	if hasPending(after) {
		t.Fatal("rolled back, so gone for the session too")
	}

	execOK(t, conn.Execute, "DROP SCHEMA meerkat_shape CASCADE")
}

// Ending a transaction nobody opened is not an error. The app sends it
// rather than asking first, and Postgres answers "there is no
// transaction in progress" and carries on.
func TestEndingATransactionThatIsNotOpenIsNotAnError(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))
	if _, err := session.EndTransaction(ctx, dbclient.TxEndCommit); err != nil {
		t.Fatal(err)
	}
	if _, err := session.EndTransaction(ctx, dbclient.TxEndRollback); err != nil {
		t.Fatal(err)
	}
	// The session is unharmed and still answers questions.
	execOK(t, session.Execute, "SELECT 1")
}

// A session may reach its last `Arc` **anywhere**, and for this app
// that anywhere is the UI thread: leaving a workspace drops the shell,
// every tab, and every session, on a mouse-up.
//
// sqlx returns a pooled connection to its pool by spawning onto tokio
// when the last handle drops, and spawning off the runtime panics —
// inside an Objective-C callback that cannot unwind, so the process
// aborts. A session has to survive being dropped by whoever is
// holding it last.
//
// Go port: there is no drop and no runtime to spawn onto, so the nearest
// test is this — a session abandoned without `Close`, its last reference
// let go on another goroutine. Nothing may panic, and nothing may leak
// into the pool: the collector's cleanup takes the abandoned connection
// out of the pool's hands rather than handing it back.
func TestASessionMayBeDroppedOffTheRuntime(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	handoff := make(chan dbclient.Session, 1)
	func() {
		session := must(conn.OpenSession(ctx))
		execOK(t, session.Execute, "SELECT 1")
		handoff <- session
	}()

	// Another goroutine holds the last reference, and lets it go.
	done := make(chan struct{})
	go func() {
		session := <-handoff
		_ = session
		close(done)
	}()
	<-done

	// The abandoned connection is taken out of the pool, not given back.
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		stat := conn.sessions.Stat()
		if stat.AcquiredConns() == 0 {
			if stat.IdleConns() != 0 {
				t.Fatalf("the abandoned connection went back into the pool: %d idle", stat.IdleConns())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the abandoned session still holds its connection: %d acquired", stat.AcquiredConns())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The pool is unharmed and still hands out sessions.
	session := must(conn.OpenSession(ctx))
	execOK(t, session.Execute, "SELECT 1")
}

// A statement that fails must not cost the session. The tab keeps it
// across an error, so a typo does not silently end a transaction.
func TestAFailedStatementLeavesTheSessionUsable(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))

	execErr(t, session.Execute, "SELECT * FROM no_such_table_here")
	wantValue(t, execOK(t, session.Execute, "SELECT 1 AS x").Rows[0][0], dbclient.ValueInt(1))
}

// backendState is what another connection sees this backend doing. It is
// read from the *app* pool, so it answers whether or not the session is
// busy.
func backendState(t *testing.T, conn *PostgresConnection, pid int32) string {
	t.Helper()
	result := execOK(t, conn.Execute, fmt.Sprintf("SELECT state FROM pg_stat_activity WHERE pid = %d", pid))
	return result.Rows[0][0].Display()
}

func TestAReadOnlySessionSurvivesAResetOfItsParameters(t *testing.T) {
	url := testURL(t)
	conn := connectTest(t, url, true)

	// `RESET ALL` puts every parameter back to what the session
	// started with. The flag is a startup option, so this is exactly
	// what it must survive — a `SET` after connect would not.
	execOK(t, conn.Execute, "RESET ALL")
	message := execErr(t, conn.Execute, "CREATE TABLE meerkat_read_only_probe (id int)")
	if !strings.Contains(message, "read-only transaction") {
		t.Fatal(message)
	}
}

func TestAUrlBecomesAProfileAndAKeychainPassword(t *testing.T) {
	profile, password, err := ProfileFromURL(
		"p1",
		"prod",
		"postgres://ada:hunter2@db.internal:5432/meerkat",
	)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Name != "prod" {
		t.Fatalf("name = %q", profile.Name)
	}
	if profile.Host == nil || *profile.Host != "db.internal" {
		t.Fatalf("host = %v", profile.Host)
	}
	if profile.Port == nil || *profile.Port != 5432 {
		t.Fatalf("port = %v", profile.Port)
	}
	if profile.Database != "meerkat" {
		t.Fatalf("database = %q", profile.Database)
	}
	if profile.User == nil || *profile.User != "ada" {
		t.Fatalf("user = %v", profile.User)
	}
	// The password never reaches the profile itself.
	if password == nil || *password != "hunter2" {
		t.Fatalf("password = %v", password)
	}

	profile, password, err = ProfileFromURL("p2", "  ", "postgres://ada@db.internal/analytics")
	if err != nil {
		t.Fatal(err)
	}
	// No name given: the database names the connection.
	if profile.Name != "analytics" {
		t.Fatalf("name = %q", profile.Name)
	}
	if password != nil {
		t.Fatalf("password = %q", *password)
	}
}

func TestAUrlWithoutADatabaseIsRejected(t *testing.T) {
	_, _, err := ProfileFromURL("p1", "prod", "postgres://ada@db.internal")
	if err == nil || !strings.Contains(err.Error(), "names no database") {
		t.Fatalf("err = %v", err)
	}
	_, _, err = ProfileFromURL("p1", "prod", "not a url")
	if err == nil || !strings.Contains(err.Error(), "not a valid PostgreSQL URL") {
		t.Fatalf("err = %v", err)
	}
}
func TestATransactionIdStaysUnsigned(t *testing.T) {
	wantValue(t, transactionID(7), dbclient.ValueInt(7))
	wantValue(t, transactionID(uint64(math.MaxUint32)), dbclient.ValueInt(4294967295))
	// Past `i64::MAX` there is no `Int` that says the truth.
	wantValue(t, transactionID(math.MaxUint64), text(fmt.Sprint(uint64(math.MaxUint64))))
}

func TestTheServerVersionReadsShort(t *testing.T) {
	if got := shortVersion("16.2 (Debian 16.2-1.pgdg120+2)"); got != "16.2" {
		t.Fatalf("got %q", got)
	}
	if got := shortVersion("15.6"); got != "15.6" {
		t.Fatalf("got %q", got)
	}
}

// --- Extra regression tests for the session fixes: a check inside a
// transaction, a commit that rolled back, the cap inside a transaction, a
// dead session, a transaction whose statement is running, and the columns
// of an empty result on the session's own connection. ---

// A statement the server refuses to parse aborts the transaction it is
// parsed in. The check fences itself off with a savepoint, so a squiggle
// under a typo costs the user nothing they ran since `BEGIN`; and inside
// a transaction that has already failed it sends nothing at all.
func TestACheckInsideATransactionLeavesItOpen(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))
	defer session.Close(ctx)

	execOK(t, session.Execute, "BEGIN")
	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_check_tx (id int)")
	execOK(t, session.Execute, "INSERT INTO meerkat_check_tx VALUES (1)")

	syntax := must(session.Check(ctx, "SELEC 1"))
	if syntax == nil || syntax.Refusal != dbclient.RefusalSyntax {
		t.Fatalf("syntax: %+v", syntax)
	}
	name := must(session.Check(ctx, "SELECT * FROM meerkat_no_such_table"))
	if name == nil || name.Refusal != dbclient.RefusalName {
		t.Fatalf("name: %+v", name)
	}
	// The temp table made inside the transaction resolves: the check is
	// still on the session's own connection, inside its transaction.
	if got := must(session.Check(ctx, "SELECT * FROM meerkat_check_tx")); got != nil {
		t.Fatalf("valid: %+v", got)
	}

	// The transaction survived all three, and so did what it holds.
	if !must(session.InTransaction(ctx)) {
		t.Fatal("the check ended the transaction")
	}
	count := execOK(t, session.Execute, "SELECT count(*) FROM meerkat_check_tx")
	wantValue(t, count.Rows[0][0], dbclient.ValueInt(1))
	// No savepoint is left behind: the commit keeps the work.
	if got := must(session.EndTransaction(ctx, dbclient.TxEndCommit)); got != dbclient.TxEndCommit {
		t.Fatalf("the commit came back as %v", got)
	}
	count = execOK(t, session.Execute, "SELECT count(*) FROM meerkat_check_tx")
	wantValue(t, count.Rows[0][0], dbclient.ValueInt(1))

	// A transaction that has already failed: nothing is sent, nothing is
	// said, and the transaction is still the user's to roll back.
	execOK(t, session.Execute, "BEGIN")
	execErr(t, session.Execute, "SELECT no_such_column")
	if got := must(session.Check(ctx, "SELEC 1")); got != nil {
		t.Fatalf("a failed transaction answered a check: %+v", got)
	}
	if !must(session.InTransaction(ctx)) {
		t.Fatal("not in a transaction after the check")
	}
	execOK(t, session.Execute, "ROLLBACK")

	// Outside a transaction the check is unchanged.
	if got := must(session.Check(ctx, "SELEC 1")); got == nil {
		t.Fatal("no syntax error outside a transaction")
	}
	execOK(t, session.Execute, "SELECT 1")
}

// Postgres takes a `COMMIT` of a failed transaction without an error and
// answers with the tag `ROLLBACK`. The app's own commit reads that tag
// and says so; a typed one carries it on the result.
func TestACommitOfAFailedTransactionReportsARollback(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))
	defer session.Close(ctx)

	// The app's commit, over a transaction that failed.
	if err := session.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_lost (id int)")
	execErr(t, session.Execute, "SELECT no_such_column")
	got, err := session.EndTransaction(ctx, dbclient.TxEndCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got != dbclient.TxEndRollback {
		t.Fatalf("a failed transaction's commit came back as %v", got)
	}
	if must(session.InTransaction(ctx)) {
		t.Fatal("still in a transaction")
	}
	execErr(t, session.Execute, "SELECT * FROM meerkat_lost")

	// A healthy commit is a commit.
	if err := session.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_kept (id int)")
	if got := must(session.EndTransaction(ctx, dbclient.TxEndCommit)); got != dbclient.TxEndCommit {
		t.Fatalf("a healthy commit came back as %v", got)
	}
	execOK(t, session.Execute, "SELECT * FROM meerkat_kept")

	// A typed `COMMIT` reads the same way, off the result's tag.
	execOK(t, session.Execute, "BEGIN")
	execErr(t, session.Execute, "SELECT no_such_column")
	if tag := execOK(t, session.Execute, "COMMIT").Tag; tag != "ROLLBACK" {
		t.Fatalf("typed commit tag = %q", tag)
	}
	execOK(t, session.Execute, "BEGIN")
	if tag := execOK(t, session.Execute, "COMMIT").Tag; tag != "COMMIT" {
		t.Fatalf("typed healthy commit tag = %q", tag)
	}

	// And every run carries its tag as the server sent it.
	if tag := execOK(t, session.Execute, "SELECT * FROM generate_series(1, 5)").Tag; tag != "SELECT 5" {
		t.Fatalf("select tag = %q", tag)
	}
	execOK(t, session.Execute, "INSERT INTO meerkat_kept VALUES (1), (2), (3)")
	if tag := execOK(t, session.Execute, "UPDATE meerkat_kept SET id = id + 1").Tag; tag != "UPDATE 3" {
		t.Fatalf("update tag = %q", tag)
	}
	if tag := execOK(t, conn.Execute, "SELECT 1").Tag; tag != "SELECT 1" {
		t.Fatalf("pooled select tag = %q", tag)
	}
}

// A cancel aborts the transaction block the statement ran in, so the cap
// must not send one there: the rest is read and thrown away, the result
// is still marked truncated, and the transaction is still open and whole.
func TestTheCapInsideATransactionDoesNotAbortIt(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	limits := dbclient.Limits{
		MaxBytes:     64 * 1024,
		MaxCellBytes: dbclient.MaxCellBytes,
	}
	conn := connectTest(t, url, false).WithLimits(limits)
	session := must(conn.OpenSession(ctx))
	defer session.Close(ctx)

	execOK(t, session.Execute, "BEGIN")
	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_cap_tx (id int)")
	execOK(t, session.Execute, "INSERT INTO meerkat_cap_tx VALUES (1)")

	result := execOK(t, session.Execute, "SELECT g, repeat('x', 100) FROM generate_series(1, 200000) g")
	if !result.Truncated {
		t.Fatal("the result was not marked truncated")
	}
	if len(result.Rows) == 0 || len(result.Rows) >= 200_000 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
	// Read to its end, so the tag is the server's whole count.
	if result.Tag != "SELECT 200000" {
		t.Fatalf("tag = %q", result.Tag)
	}

	if !must(session.InTransaction(ctx)) {
		t.Fatal("the cap ended the transaction")
	}
	count := execOK(t, session.Execute, "SELECT count(*) FROM meerkat_cap_tx")
	wantValue(t, count.Rows[0][0], dbclient.ValueInt(1))
	if got := must(session.EndTransaction(ctx, dbclient.TxEndCommit)); got != dbclient.TxEndCommit {
		t.Fatalf("the commit after the cap came back as %v", got)
	}

	// Outside a transaction the cap still cancels, and the session lives.
	result = execOK(t, session.Execute, "SELECT g, repeat('x', 100) FROM generate_series(1, 10000000) g")
	if !result.Truncated {
		t.Fatal("not truncated outside a transaction")
	}
	wantValue(t, execOK(t, session.Execute, "SELECT 1 AS x").Rows[0][0], dbclient.ValueInt(1))
}

// A session whose backend was terminated is dead, and says so once the
// run that found out has come back; a closed session says so at once.
func TestATerminatedSessionReportsClosed(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)

	session := must(conn.OpenSession(ctx))
	if session.IsClosed() {
		t.Fatal("a fresh session reads as closed")
	}
	id, _ := session.Backend()
	// This test's own backend, and no other.
	if !must(conn.Stop(ctx, id, dbclient.StopTerminate)) {
		t.Fatal("the server refused the terminate")
	}
	// Give the backend a moment to go; the next statement is what tells
	// the client.
	time.Sleep(200 * time.Millisecond)
	execErr(t, session.Execute, "SELECT 1")
	if !session.IsClosed() {
		t.Fatal("a terminated session does not read as closed")
	}
	session.Close(ctx)
	if !session.IsClosed() {
		t.Fatal("closing it changed the answer")
	}

	other := must(conn.OpenSession(ctx))
	execOK(t, other.Execute, "SELECT 1")
	if other.IsClosed() {
		t.Fatal("a live session reads as closed")
	}
	other.Close(ctx)
	if !other.IsClosed() {
		t.Fatal("a closed session does not read as closed")
	}
}

// A transaction whose statement is still running is still a transaction.
// Reading `idle in transaction*` off the backend's state said it was not.
func TestInTransactionWhileAStatementRuns(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))
	defer session.Close(ctx)

	execOK(t, session.Execute, "BEGIN")
	runner := make(chan runOutcome, 1)
	go func() {
		result, err := session.Execute(ctx, "SELECT pg_sleep(2)")
		runner <- runOutcome{result, err}
	}()
	time.Sleep(500 * time.Millisecond)
	if !must(session.InTransaction(ctx)) {
		t.Fatal("a transaction whose statement runs reads as not in a transaction")
	}
	if outcome := <-runner; outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if !must(session.InTransaction(ctx)) {
		t.Fatal("not in a transaction after the statement")
	}
	execOK(t, session.Execute, "ROLLBACK")
	if must(session.InTransaction(ctx)) {
		t.Fatal("in a transaction after ROLLBACK")
	}
}

// An empty result is named by the connection that ran it. The app pool
// cannot see a session's temp table, so naming the columns there lost
// them.
func TestAnEmptyResultKeepsTheSessionsColumns(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))
	defer session.Close(ctx)

	execOK(t, session.Execute, "CREATE TEMP TABLE meerkat_empty_probe (a int, b text)")
	result := execOK(t, session.Execute, "SELECT * FROM meerkat_empty_probe")
	if len(result.Rows) != 0 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
	if fmt.Sprint(result.Columns) != fmt.Sprint([]string{"a", "b"}) {
		t.Fatalf("columns = %v", result.Columns)
	}
	// A statement with no result set still has none.
	updated := execOK(t, session.Execute, "UPDATE meerkat_empty_probe SET a = 1")
	if len(updated.Columns) != 0 {
		t.Fatalf("update columns = %v", updated.Columns)
	}
}

// A role that may only read sees the primary keys, a partitioned table
// lists its partitions under itself, and a generated or identity column
// says so rather than passing its expression off as a default.
func TestIntrospectReadsKeysPartitionsAndGeneratedColumns(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	for _, statement := range []string{
		"DROP SCHEMA IF EXISTS meerkat_parts CASCADE",
		"CREATE SCHEMA meerkat_parts",
		`CREATE TABLE meerkat_parts.events (
                 id bigint GENERATED ALWAYS AS IDENTITY,
                 at date NOT NULL,
                 n int,
                 twice int GENERATED ALWAYS AS (n * 2) STORED,
                 PRIMARY KEY (id, at)
             ) PARTITION BY RANGE (at)`,
		"CREATE TABLE meerkat_parts.events_2024 PARTITION OF meerkat_parts.events FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')",
		"CREATE TABLE meerkat_parts.events_rest PARTITION OF meerkat_parts.events DEFAULT",
	} {
		execOK(t, conn.Execute, statement)
	}
	t.Cleanup(func() { _, _ = conn.Execute(context.Background(), "DROP SCHEMA IF EXISTS meerkat_parts CASCADE") })

	session := must(conn.OpenSession(ctx))
	t.Cleanup(func() { session.Close(context.Background()) })
	// A role of its own, made and dropped inside one transaction, so a
	// failed run leaves nothing behind. Making a role needs the right to;
	// a server that refuses skips that half.
	asReader := true
	for _, statement := range []string{
		"BEGIN",
		"CREATE ROLE meerkat_parts_reader",
		"GRANT USAGE ON SCHEMA meerkat_parts TO meerkat_parts_reader",
		"GRANT SELECT ON ALL TABLES IN SCHEMA meerkat_parts TO meerkat_parts_reader",
		"SET ROLE meerkat_parts_reader",
	} {
		if _, err := session.Execute(ctx, statement); err != nil {
			t.Logf("reading as the owner: %v", err)
			_, _ = session.Execute(ctx, "ROLLBACK")
			asReader = false
			break
		}
	}
	catalog := must(session.Introspect(ctx))
	if asReader {
		execOK(t, session.Execute, "ROLLBACK")
	}

	var schema *introspect.Schema
	for i := range catalog.Schemas {
		if catalog.Schemas[i].Name == "meerkat_parts" {
			schema = &catalog.Schemas[i]
		}
	}
	if schema == nil {
		t.Fatal("no meerkat_parts schema")
	}
	if len(schema.Tables) != 1 || schema.Tables[0].Name != "events" {
		t.Fatalf("tables = %+v, want the parent alone", schema.Tables)
	}
	events := schema.Tables[0]
	if got := strings.Join(events.PrimaryKey, ","); got != "id,at" {
		t.Fatalf("primary key = %q", got)
	}
	if len(events.Partitions) != 2 {
		t.Fatalf("partitions = %+v", events.Partitions)
	}
	if p := events.Partitions[0]; p.Name != "events_2024" || p.Bound != "FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')" {
		t.Fatalf("partition = %+v", p)
	}
	if p := events.Partitions[1]; p.Name != "events_rest" || p.Bound != "DEFAULT" {
		t.Fatalf("partition = %+v", p)
	}
	columns := map[string]introspect.Column{}
	for _, column := range events.Columns {
		columns[column.Name] = column
	}
	if c := columns["id"]; c.Identity == nil || *c.Identity != "always" || c.Default != nil {
		t.Fatalf("id = %+v", c)
	}
	if c := columns["twice"]; c.Generated == nil || *c.Generated != "(n * 2)" || c.Default != nil {
		t.Fatalf("twice = %+v", c)
	}
	if c := columns["n"]; c.Generated != nil || c.Identity != nil {
		t.Fatalf("n = %+v", c)
	}
}

// A notice, a warning and a notification come back beside the result
// they arrived with, and a `COPY` with the client at one end neither
// hangs nor loses its rows.
func TestARunCarriesWhatTheServerSaidAndCopyIsReadNotWaitedOn(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, false)
	session := must(conn.OpenSession(ctx))
	t.Cleanup(func() { session.Close(context.Background()) })

	result := execOK(t, session.Execute,
		"DO $$ BEGIN RAISE NOTICE 'hello %', 1; RAISE WARNING 'careful'; END $$")
	if got := strings.Join(result.Messages, "\n"); got != "NOTICE: hello 1\nWARNING: careful" {
		t.Fatalf("messages = %q", got)
	}
	execOK(t, session.Execute, "LISTEN meerkat_channel")
	result = execOK(t, session.Execute, "NOTIFY meerkat_channel, 'ping'")
	if len(result.Messages) != 1 || !strings.HasPrefix(result.Messages[0], "NOTIFY meerkat_channel from backend ") ||
		!strings.HasSuffix(result.Messages[0], ": ping") {
		t.Fatalf("messages = %q", result.Messages)
	}
	// A statement that says nothing carries nothing.
	if result := execOK(t, session.Execute, "SELECT 1"); len(result.Messages) != 0 {
		t.Fatalf("messages = %q", result.Messages)
	}

	execOK(t, session.Execute, "CREATE TEMP TABLE copied (a int, b text)")
	execOK(t, session.Execute, "INSERT INTO copied VALUES (1, 'x'), (2, NULL)")
	result = execOK(t, session.Execute, "COPY copied TO STDOUT")
	if len(result.Columns) != 1 || len(result.Rows) != 2 {
		t.Fatalf("copy out = %+v", result)
	}
	wantValue(t, result.Rows[0][0], text("1\tx"))
	wantValue(t, result.Rows[1][0], text("2\t\\N"))
	if result.Tag != "COPY 2" {
		t.Fatalf("tag = %q", result.Tag)
	}
	result = execOK(t, session.Execute, "COPY (SELECT 'a;b' AS v) TO STDOUT WITH (FORMAT csv, HEADER)")
	if len(result.Rows) != 2 {
		t.Fatalf("copy out = %+v", result.Rows)
	}

	timed, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := session.Execute(timed, "copy copied from stdin"); err == nil || !strings.Contains(err.Error(), "FROM STDIN") {
		t.Fatalf("copy in = %v", err)
	}
	// The session is still good for the next statement.
	execOK(t, session.Execute, "SELECT count(*) FROM copied")
}

func TestCopyStdioReadsWhichEndTheClientIsAt(t *testing.T) {
	cases := map[string]copyEnd{
		"COPY t FROM STDIN":                          copyIn,
		"copy t (a, b) from stdin with (format csv)": copyIn,
		"COPY (SELECT 1) TO STDOUT":                  copyOut,
		"/* c */ COPY t TO stdout":                   copyOut,
		"COPY stdin FROM '/tmp/x'":                   copyNone,
		"COPY t TO '/tmp/stdout'":                    copyNone,
		"COPY (SELECT 'from stdin') TO '/tmp/x'":     copyNone,
		"SELECT 'copy t from stdin'":                 copyNone,
	}
	for sql, want := range cases {
		if got := copyStdio(sql); got != want {
			t.Errorf("%s = %v, want %v", sql, got, want)
		}
	}
}

// Every form of connection string a libpq client accepts, and an error
// that never repeats the URL it could not read.
func TestConnectionStringsReadAsLibpqReadsThem(t *testing.T) {
	t.Setenv("PGHOST", "")
	os.Unsetenv("PGHOST")
	t.Setenv("PGPORT", "5432")
	parse := func(text string) connectOptions {
		t.Helper()
		options, err := parseConnectURL(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		return options
	}
	hosts := func(o connectOptions) string {
		var out []string
		for _, h := range o.hosts() {
			out = append(out, fmt.Sprintf("%s:%d", h.host, h.port))
		}
		return strings.Join(out, ",")
	}

	o := parse("postgres://bar%5E:%5Dfoo@db/testdb%5B")
	if o.username != "bar^" || deref(o.password) != "]foo" || deref(o.database) != "testdb[" {
		t.Fatalf("decoded = %q %q %q", o.username, deref(o.password), deref(o.database))
	}
	if got := hosts(parse("postgresql://h1:5433,h2,[::1]:5434/db")); got != "h1:5433,h2:5432,::1:5434" {
		t.Fatalf("hosts = %q", got)
	}
	if o := parse("postgres://%2Ftmp:5433/db"); o.socket == nil || *o.socket != "/tmp" || o.port != 5433 {
		t.Fatalf("socket = %v port = %d", o.socket, o.port)
	}
	o = parse(`host=db.internal port=6432 dbname='my db' user=ada password='it\'s' connect_timeout=3 target_session_attrs=read-write sslmode=require`)
	if hosts(o) != "db.internal:6432" || deref(o.database) != "my db" || o.username != "ada" || deref(o.password) != "it's" ||
		o.connectTimeout != 3*time.Second || deref(o.targetSessionAttrs) != "read-write" || deref(o.sslMode) != "require" {
		t.Fatalf("key/value = %+v", o)
	}
	if got := hosts(parse("host=h1,h2 port=5433,5434 dbname=x")); got != "h1:5433,h2:5434" {
		t.Fatalf("hosts = %q", got)
	}
	if o := parse(`password='a\'b c'`); deref(o.password) != "a'b c" {
		t.Fatalf("password = %q", deref(o.password))
	}

	for _, bad := range []string{
		"postgres://u:hunter#2@db/x",
		"postgres://u:pa%zzword@db/x",
	} {
		_, err := parseConnectURL(bad)
		if err == nil {
			t.Fatalf("%s parsed", bad)
		}
		if strings.Contains(err.Error(), "hunter") || strings.Contains(err.Error(), "pa%zz") {
			t.Fatalf("the error repeats the password: %v", err)
		}
	}
	if _, err := parseConnectURL("mysql://db/x"); err == nil || !strings.Contains(err.Error(), "PostgreSQL") {
		t.Fatalf("mysql = %v", err)
	}
}

// A saved connection keeps what its URL said beyond the fields it has: a
// host list, a socket, `sslmode`, and the password as the server checks
// it, not as the URL spelled it.
func TestAProfileKeepsWhatItsUrlSaid(t *testing.T) {
	profile, password, err := ProfileFromURL("p", "", "postgres://ada:p%40ss@h1:5433,h2:5434/db?sslmode=verify-full&application_name=me")
	if err != nil {
		t.Fatal(err)
	}
	if password == nil || *password != "p@ss" {
		t.Fatalf("password = %v", password)
	}
	if profile.Host == nil || *profile.Host != "h1:5433,h2:5434" || profile.Port != nil {
		t.Fatalf("host = %v port = %v", profile.Host, profile.Port)
	}
	if profile.Params != "application_name=me&sslmode=verify-full" {
		t.Fatalf("params = %q", profile.Params)
	}
	profile, _, err = ProfileFromURL("p", "", "postgres:///db?host=/var/run/postgresql")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Host == nil || *profile.Host != "/var/run/postgresql" {
		t.Fatalf("socket = %v", profile.Host)
	}
}

// A timestamp reads in the session's own zone: the app sends no zone of
// its own, so a role's or a database's setting — or a `SET` — is what the
// grid shows, as `psql` shows it.
func TestATimestampReadsInTheSessionsZone(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	conn := connectTest(t, url, true)
	session := must(conn.OpenSession(ctx))
	t.Cleanup(func() { session.Close(context.Background()) })
	execOK(t, session.Execute, "SET TIME ZONE 'Asia/Kolkata'")
	execOK(t, session.Execute, "SET DateStyle = 'ISO, DMY'")
	result := execOK(t, session.Execute,
		"SELECT '2024-01-01 12:00:00+00'::timestamptz, '03/04/2024'::date")
	wantValue(t, result.Rows[0][0], text("2024-01-01 17:30:00+05:30"))
	wantValue(t, result.Rows[0][1], text("2024-04-03"))
}
