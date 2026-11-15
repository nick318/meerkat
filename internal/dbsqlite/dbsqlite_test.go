package dbsqlite

import (
	"context"
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
