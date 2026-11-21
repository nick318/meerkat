package dbsqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"
)

func openInMemory(t *testing.T) *SqliteConnection {
	t.Helper()
	conn, err := OpenInMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func mustExecute(t *testing.T, run func(context.Context, string) (dbclient.QueryResult, error), sql string) dbclient.QueryResult {
	t.Helper()
	result, err := run(context.Background(), sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return result
}

func TestIntrospectAndQuery(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	mustExecute(t, conn.Execute, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL)")
	mustExecute(t, conn.Execute, "INSERT INTO users (id, name) VALUES (1, 'ada'), (2, 'grace')")

	catalog, err := conn.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	table := &catalog.Schemas[0].Tables[0]
	if table.Name != "users" {
		t.Fatalf("name = %q", table.Name)
	}
	if !reflect.DeepEqual(table.PrimaryKey, []string{"id"}) {
		t.Fatalf("primary key = %q", table.PrimaryKey)
	}
	if len(table.Columns) != 2 {
		t.Fatalf("columns = %d", len(table.Columns))
	}

	result := mustExecute(t, conn.Execute, "SELECT id, name FROM users ORDER BY id")
	if !reflect.DeepEqual(result.Columns, []string{"id", "name"}) {
		t.Fatalf("columns = %q", result.Columns)
	}
	if !result.Rows[0][1].Equal(dbclient.ValueText("ada")) {
		t.Fatalf("rows[0][1] = %#v", result.Rows[0][1])
	}
	if !result.Rows[1][0].Equal(dbclient.ValueInt(2)) {
		t.Fatalf("rows[1][0] = %#v", result.Rows[1][0])
	}
}

func TestIntrospectReadsIndexesKeysAndTriggers(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	for _, statement := range []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE)",
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users (id),
                 CONSTRAINT again FOREIGN KEY (user_id) REFERENCES users)`,
		"CREATE INDEX orders_user ON orders (user_id)",
		"CREATE TRIGGER stamp AFTER UPDATE OF user_id ON orders BEGIN SELECT 1; END",
	} {
		mustExecute(t, conn.Execute, statement)
	}
	catalog, err := conn.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tables := catalog.Schemas[0].Tables
	find := func(name string) *introspect.Table {
		for i := range tables {
			if tables[i].Name == name {
				return &tables[i]
			}
		}
		t.Fatalf("no table %q", name)
		return nil
	}
	orders := find("orders")

	if len(orders.Indexes) != 1 {
		t.Fatalf("indexes = %d", len(orders.Indexes))
	}
	if got := orders.Indexes[0].Detail(); got != "(user_id)" {
		t.Fatalf("index detail = %q", got)
	}
	if got := orders.ForeignKeys[0].Detail(); got != "(user_id) → main.users(id)" {
		t.Fatalf("key detail = %q", got)
	}
	// A key that names no target column reads the target's primary
	// key, and a second key over the same column gets a name of its own.
	var keys [][2]string
	for i := range orders.ForeignKeys {
		k := &orders.ForeignKeys[i]
		keys = append(keys, [2]string{k.Name, k.Detail()})
	}
	want := [][2]string{
		{"orders_user_id_fkey", "(user_id) → main.users(id)"},
		{"orders_user_id_fkey1", "(user_id) → main.users(id)"},
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %q", keys)
	}
	if orders.Triggers[0].Timing != "after update of user_id" {
		t.Fatalf("timing = %q", orders.Triggers[0].Timing)
	}

	// The unique constraint is the index SQLite made for it.
	users := find("users")
	if len(users.Indexes) != 1 {
		t.Fatalf("indexes = %d", len(users.Indexes))
	}
	if !users.Indexes[0].Unique {
		t.Fatal("the unique index is not unique")
	}
	if users.Indexes[0].Definition != "(email)" {
		t.Fatalf("definition = %q", users.Indexes[0].Definition)
	}
}

func TestATriggerReadsItsTimingOffItsOwnStatement(t *testing.T) {
	if got := triggerTiming("CREATE TRIGGER IF NOT EXISTS t BEFORE DELETE ON x BEGIN END"); got != "before delete" {
		t.Fatalf("got %q", got)
	}
	// No timing word means before.
	if got := triggerTiming("CREATE TRIGGER t INSERT ON x BEGIN END"); got != "before insert" {
		t.Fatalf("got %q", got)
	}
	if got := triggerTiming("not a trigger"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// The count off the completion tag, on the engine that needs no
// server. `fetch` dropped that tag, so an `UPDATE` used to answer with
// nothing whatever.
func TestAStatementThatChangesRowsReportsHowMany(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	session, err := conn.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}

	mustExecute(t, session.Execute, "CREATE TABLE t (id INTEGER)")
	inserted := mustExecute(t, session.Execute, "INSERT INTO t VALUES (1), (2), (3)")
	if inserted.RowsAffected != 3 {
		t.Fatalf("inserted = %d", inserted.RowsAffected)
	}
	if len(inserted.Columns) != 0 {
		t.Fatalf("columns = %q", inserted.Columns)
	}

	updated := mustExecute(t, session.Execute, "UPDATE t SET id = id + 1 WHERE id > 1")
	if updated.RowsAffected != 2 {
		t.Fatalf("updated = %d", updated.RowsAffected)
	}

	// Matched nothing, and says so with a count rather than an empty
	// table: the statement worked.
	nothing := mustExecute(t, session.Execute, "DELETE FROM t WHERE id = 999")
	if nothing.RowsAffected != 0 {
		t.Fatalf("deleted = %d", nothing.RowsAffected)
	}
	if len(nothing.Columns) != 0 {
		t.Fatalf("columns = %q", nothing.Columns)
	}
}

// A session is one connection, so a temp table made by one statement
// is there for the next — and gone once the session closes.
func TestASessionKeepsWhatItsStatementsMade(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	session, err := conn.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}

	mustExecute(t, session.Execute, "CREATE TEMP TABLE probe (id INTEGER)")
	mustExecute(t, session.Execute, "INSERT INTO probe VALUES (1), (2)")
	result := mustExecute(t, session.Execute, "SELECT count(*) FROM probe")
	if !result.Rows[0][0].Equal(dbclient.ValueInt(2)) {
		t.Fatalf("count = %#v", result.Rows[0][0])
	}

	session.Close(ctx)
	_, err = session.Execute(ctx, "SELECT 1")
	if err == nil {
		t.Fatal("a closed session ran a statement")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("%v", err)
	}
}

// See `watchSession`: the Rust test dropped the last handle to a session
// on a plain `std::thread`, which has no tokio runtime, because sqlx gives
// a pooled connection back by spawning onto one. Go has no drop; the
// closest thing is a session abandoned without `Close` on another
// goroutine. Nothing may panic, the abandoned connection must not stay
// checked out of the pool for ever, and the pool must go on serving.
func TestASessionMayBeDroppedOffTheRuntime(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	handoff := make(chan dbclient.Session, 1)
	func() {
		session, err := conn.OpenSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mustExecute(t, session.Execute, "SELECT 1")
		handoff <- session
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		session := <-handoff
		_ = session // dropped here, never closed
	}()
	<-done

	// Nothing leaks: once the session is unreachable its connection is
	// detached, so the pool has nothing checked out.
	deadline := time.Now().Add(5 * time.Second)
	for conn.pool.Stats().InUse != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the abandoned session still holds its connection: %+v", conn.pool.Stats())
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}

	session, err := conn.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExecute(t, session.Execute, "SELECT 1")
}

// --- Extra regression tests for the session API (Tag, EndTransaction,
// IsClosed). ---

// SQLite refuses a commit it cannot carry out, so what was asked for is
// what happened; it sends no command tag; and its session is closed only
// once `Close` has run.
func TestASqliteSessionAnswersTheSessionAPI(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	session, err := conn.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if session.IsClosed() {
		t.Fatal("a fresh session reads as closed")
	}
	if tag := mustExecute(t, session.Execute, "SELECT 1").Tag; tag != "" {
		t.Fatalf("tag = %q, want none", tag)
	}
	if err := session.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := session.EndTransaction(ctx, dbclient.TxEndCommit); err != nil || got != dbclient.TxEndCommit {
		t.Fatalf("commit: got (%v, %v)", got, err)
	}
	if err := session.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := session.EndTransaction(ctx, dbclient.TxEndRollback); err != nil || got != dbclient.TxEndRollback {
		t.Fatalf("rollback: got (%v, %v)", got, err)
	}
	session.Close(ctx)
	if !session.IsClosed() {
		t.Fatal("a closed session does not read as closed")
	}
}

// --- what is stored is what is shown ---

// rowValues runs a query on run and answers with its rows, failing the
// test when it does not run.
func rowValues(t *testing.T, run func(context.Context, string) (dbclient.QueryResult, error), sql string) [][]dbclient.Value {
	t.Helper()
	return mustExecute(t, run, sql).Rows
}

// wantValues checks each row's one value against want, by variant and by
// value: `ValueText("")` and `ValueBytes{}` must not read as each other.
func wantValues(t *testing.T, rows [][]dbclient.Value, want []dbclient.Value) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for i := range want {
		got := rows[i][0]
		if reflect.TypeOf(got) != reflect.TypeOf(want[i]) || !got.Equal(want[i]) {
			t.Errorf("row %d = %#v, want %#v", i, got, want[i])
		}
	}
}

// modernc parses the text of a column declared DATE, DATETIME or
// TIMESTAMP into a time and drops the text; see `undeclareTimes`. Every
// value must come back as it was written, whether it parses as a date,
// does not, or is not text at all — on the pool, on a session, through a
// view, and with the declared type in lower case.
func TestADateColumnShowsWhatIsStored(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	for _, statement := range []string{
		"CREATE TABLE stamps (seq INTEGER PRIMARY KEY, d DATE, dt datetime, ts TIMESTAMP)",
		`INSERT INTO stamps (seq, d, dt, ts) VALUES
			(1, '2024-01-01', '2024-01-01 10:00', '2024-01-01T10:00:00.500Z'),
			(2, 'tomorrow', '2024-01-01 10:00:00+02:00', '2024-01-01 10:00:00.000'),
			(3, 1700000000, 1.5, X'01'),
			(4, NULL, '', '2024-01-01')`,
		"CREATE VIEW stamp_view AS SELECT seq, d, dt, ts FROM stamps",
	} {
		mustExecute(t, conn.Execute, statement)
	}
	want := map[string][]dbclient.Value{
		"d": {
			dbclient.ValueText("2024-01-01"),
			dbclient.ValueText("tomorrow"),
			dbclient.ValueInt(1700000000),
			dbclient.ValueNull{},
		},
		"dt": {
			dbclient.ValueText("2024-01-01 10:00"),
			dbclient.ValueText("2024-01-01 10:00:00+02:00"),
			dbclient.ValueFloat(1.5),
			dbclient.ValueText(""),
		},
		"ts": {
			dbclient.ValueText("2024-01-01T10:00:00.500Z"),
			dbclient.ValueText("2024-01-01 10:00:00.000"),
			dbclient.ValueBytes{0x01},
			dbclient.ValueText("2024-01-01"),
		},
	}
	session, err := conn.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(ctx)
	for _, relation := range []string{"stamps", "stamp_view"} {
		for column, values := range want {
			query := "SELECT " + column + " FROM " + relation + " ORDER BY seq"
			wantValues(t, rowValues(t, conn.Execute, query), values)
			wantValues(t, rowValues(t, session.Execute, query), values)
		}
	}
}

// modernc reads no BOOLEAN column as a bool: what is stored there is an
// integer or a text, and is shown as one.
func TestABooleanColumnShowsWhatIsStored(t *testing.T) {
	conn := openInMemory(t)
	mustExecute(t, conn.Execute, "CREATE TABLE flags (seq INTEGER PRIMARY KEY, flag BOOLEAN)")
	mustExecute(t, conn.Execute, "INSERT INTO flags (seq, flag) VALUES (1, 2), (2, 1), (3, 0), (4, 'true')")
	wantValues(t, rowValues(t, conn.Execute, "SELECT flag FROM flags ORDER BY seq"), []dbclient.Value{
		dbclient.ValueInt(2),
		dbclient.ValueInt(1),
		dbclient.ValueInt(0),
		dbclient.ValueText("true"),
	})
}

// A value past what a float64 holds exactly must come back to the digit,
// whatever name the column's integer type has.
func TestALargeIntegerComesBackExact(t *testing.T) {
	conn := openInMemory(t)
	const big = 326335020369620480
	mustExecute(t, conn.Execute, `CREATE TABLE ids (a INTEGER, b BIGINT, c UNSIGNED BIG INT)`)
	mustExecute(t, conn.Execute, `INSERT INTO ids VALUES (326335020369620480, 326335020369620480, 326335020369620480)`)
	for _, column := range []string{"a", "b", "c"} {
		wantValues(t, rowValues(t, conn.Execute, "SELECT "+column+" FROM ids"), []dbclient.Value{dbclient.ValueInt(big)})
	}
	wantValues(t, rowValues(t, conn.Execute, "SELECT 326335020369620480"), []dbclient.Value{dbclient.ValueInt(big)})
}

// An empty blob, an empty string and NULL are three different values, in
// a column of no declared type and in a typed one.
func TestAnEmptyBlobAnEmptyStringAndNullStayApart(t *testing.T) {
	conn := openInMemory(t)
	mustExecute(t, conn.Execute, "CREATE TABLE empties (seq INTEGER PRIMARY KEY, bare, typed BLOB)")
	mustExecute(t, conn.Execute, "INSERT INTO empties VALUES (1, X'', X''), (2, '', ''), (3, NULL, NULL)")
	want := []dbclient.Value{
		dbclient.ValueBytes{},
		dbclient.ValueText(""),
		dbclient.ValueNull{},
	}
	for _, column := range []string{"bare", "typed"} {
		wantValues(t, rowValues(t, conn.Execute, "SELECT "+column+" FROM empties ORDER BY seq"), want)
	}
}

// --- the catalog ---

// `pragma_table_info` leaves a generated column out; the catalog must
// list it, marked generated. A virtual table's hidden columns are how
// the module takes arguments, and are left out.
func TestIntrospectListsGeneratedColumns(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	mustExecute(t, conn.Execute, `CREATE TABLE shapes (
		side INTEGER NOT NULL DEFAULT 1,
		area INTEGER GENERATED ALWAYS AS (side * side),
		perimeter INTEGER AS (side * 4) STORED)`)
	mustExecute(t, conn.Execute, "CREATE VIRTUAL TABLE notes USING fts5(title, body)")

	catalog, err := conn.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tables := catalog.Schemas[0].Tables
	columnsOf := func(name string) []introspect.Column {
		for i := range tables {
			if tables[i].Name == name {
				return tables[i].Columns
			}
		}
		t.Fatalf("no table %q", name)
		return nil
	}

	type column struct {
		name      string
		generated bool
		def       string
	}
	var got []column
	for _, c := range columnsOf("shapes") {
		entry := column{name: c.Name, generated: c.Generated != nil}
		if c.Generated != nil && *c.Generated != "" {
			t.Errorf("%s: generated = %q, want the empty expression", c.Name, *c.Generated)
		}
		if c.Default != nil {
			entry.def = *c.Default
		}
		got = append(got, entry)
	}
	want := []column{{"side", false, "1"}, {"area", true, ""}, {"perimeter", true, ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("columns = %+v, want %+v", got, want)
	}

	var names []string
	for _, c := range columnsOf("notes") {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"title", "body"}) {
		t.Fatalf("virtual table columns = %q", names)
	}
}

// Every attached database is a schema of its own, after `main` and in
// the order it was attached; `temp` comes last and only once it holds a
// relation. Names that need quoting — a schema with a `"` in it, tables
// with a `.`, a `'` or a `"` — are read whole, with their keys, indexes
// and triggers.
func TestIntrospectReadsEveryAttachedDatabase(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	mustExecute(t, conn.Execute, "CREATE TABLE users (id INTEGER PRIMARY KEY)")
	session, err := conn.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(ctx)

	dir := t.TempDir()
	quoteLiteral := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	for _, statement := range []string{
		"ATTACH DATABASE " + quoteLiteral(filepath.Join(dir, "aux.db")) + " AS aux",
		"ATTACH DATABASE " + quoteLiteral(filepath.Join(dir, "odd.db")) + ` AS "we""ird.db"`,
		`CREATE TABLE aux."o'neil" (id INTEGER PRIMARY KEY, label TEXT)`,
		`CREATE TABLE aux."foo.bar" (
			id INTEGER PRIMARY KEY,
			owner INTEGER REFERENCES "o'neil",
			doubled INTEGER AS (id * 2))`,
		`CREATE INDEX aux."foo.bar_owner" ON "foo.bar" (owner)`,
		`CREATE TRIGGER aux."foo.bar_stamp" AFTER INSERT ON "foo.bar" BEGIN SELECT 1; END`,
		`CREATE TABLE "we""ird.db"."say ""hi""" (x TEXT)`,
	} {
		mustExecute(t, session.Execute, statement)
	}

	schemaNames := func(catalog *introspect.Catalog) []string {
		var names []string
		for _, schema := range catalog.Schemas {
			names = append(names, schema.Name)
		}
		return names
	}
	catalog, err := session.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if catalog == nil {
		t.Fatal("a SQLite session cannot read its catalog")
	}
	if got, want := schemaNames(catalog), []string{"main", "aux", `we"ird.db`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("schemas = %q, want %q", got, want)
	}

	aux := catalog.Schemas[1]
	var tableNames []string
	for _, table := range aux.Tables {
		tableNames = append(tableNames, table.Name)
	}
	if !reflect.DeepEqual(tableNames, []string{"foo.bar", "o'neil"}) {
		t.Fatalf("aux tables = %q", tableNames)
	}
	fooBar, oNeil := aux.Tables[0], aux.Tables[1]
	if len(fooBar.Columns) != 3 || fooBar.Columns[2].Generated == nil {
		t.Fatalf("foo.bar columns = %+v", fooBar.Columns)
	}
	if !reflect.DeepEqual(oNeil.PrimaryKey, []string{"id"}) || len(oNeil.Columns) != 2 {
		t.Fatalf("o'neil = %+v", oNeil)
	}
	if len(fooBar.Indexes) != 1 || fooBar.Indexes[0].Name != "foo.bar_owner" || fooBar.Indexes[0].Definition != "(owner)" {
		t.Fatalf("foo.bar indexes = %+v", fooBar.Indexes)
	}
	if len(fooBar.ForeignKeys) != 1 {
		t.Fatalf("foo.bar keys = %+v", fooBar.ForeignKeys)
	}
	if got := fooBar.ForeignKeys[0].Detail(); got != "(owner) → aux.o'neil(id)" {
		t.Fatalf("key detail = %q", got)
	}
	if len(fooBar.Triggers) != 1 || fooBar.Triggers[0].Timing != "after insert" {
		t.Fatalf("foo.bar triggers = %+v", fooBar.Triggers)
	}
	weird := catalog.Schemas[2]
	if len(weird.Tables) != 1 || weird.Tables[0].Name != `say "hi"` || weird.Tables[0].Columns[0].Name != "x" {
		t.Fatalf("we\"ird.db = %+v", weird.Tables)
	}

	// A temp relation puts `temp` on the list, last.
	mustExecute(t, session.Execute, "CREATE TEMP TABLE scratch (n INTEGER)")
	catalog, err = session.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := schemaNames(catalog), []string{"main", "aux", `we"ird.db`, "temp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("schemas = %q, want %q", got, want)
	}

	// The pool's connections attached nothing.
	pooled, err := conn.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := schemaNames(&pooled); !reflect.DeepEqual(got, []string{"main"}) {
		t.Fatalf("pooled schemas = %q", got)
	}
}

// A table whose name looks like SQLite's own prefix to `LIKE` is still
// the user's: only `sqlite_` itself is SQLite's.
func TestIntrospectKeepsATableNamedLikeSqlitesOwn(t *testing.T) {
	ctx := context.Background()
	conn := openInMemory(t)
	mustExecute(t, conn.Execute, "CREATE TABLE sqlitex (id INTEGER)")
	mustExecute(t, conn.Execute, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT)")
	catalog, err := conn.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, table := range catalog.Schemas[0].Tables {
		names = append(names, table.Name)
	}
	if !reflect.DeepEqual(names, []string{"sqlitex", "t"}) {
		t.Fatalf("tables = %q", names)
	}
}
