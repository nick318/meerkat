// Package dbsqlite is the SQLite driver, backed by modernc.org/sqlite
// through database/sql. (The Rust build used sqlx.)
package dbsqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"

	"modernc.org/sqlite"
)

// connectOptions are what sqlx's `SqliteConnectOptions` set on every
// connection it opened, carried as modernc's DSN pragmas: foreign keys
// enforced (sqlx turns them on, SQLite leaves them off) and a five-second
// busy timeout.
const connectOptions = "_pragma=foreign_keys%28ON%29&_pragma=busy_timeout%285000%29"

// maxConnections is sqlx's default pool size.
const maxConnections = 10

// SqliteConnection is one open SQLite file (or in-memory database) and its
// pool.
type SqliteConnection struct {
	dbclient.ConnectionDefaults
	pool   *sql.DB
	limits dbclient.Limits
}

// Open opens the file at path. A missing file is an error: the app opens
// databases, it does not make them.
func Open(ctx context.Context, path string) (*SqliteConnection, error) {
	pool, err := connect(ctx, fileDSN(path))
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	return &SqliteConnection{
		pool:   pool,
		limits: dbclient.DefaultLimits(),
	}, nil
}

// inMemorySeq names each in-memory database apart, as sqlx's
// `IN_MEMORY_DB_SEQ` does.
var inMemorySeq atomic.Uint64

// OpenInMemory opens a fresh in-memory database. Every connection of the
// pool shares it — sqlx's `sqlite::memory:` is a shared-cache database
// under a name of its own, and so is this — so a table one statement made
// is there for the next, whichever pooled connection runs it.
func OpenInMemory(ctx context.Context) (*SqliteConnection, error) {
	seqno := inMemorySeq.Add(1) - 1
	pool, err := connect(ctx, fmt.Sprintf(
		"file:sqlx-in-memory-%d?mode=memory&cache=shared&%s", seqno, connectOptions,
	))
	if err != nil {
		return nil, err
	}
	return &SqliteConnection{
		pool:   pool,
		limits: dbclient.DefaultLimits(),
	}, nil
}

// fileDSN is sqlx's `sqlite://{path}` with `create_if_missing(false)`:
// opened read-write, never created.
func fileDSN(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String() + "?mode=rw&" + connectOptions
}

// connect opens the pool and one connection on it, as
// `SqlitePool::connect_with` does: a file that cannot be opened fails
// here, not on the first query.
func connect(ctx context.Context, dsn string) (*sql.DB, error) {
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(maxConnections)
	// Idle connections stay: an in-memory database lives exactly as long
	// as one connection to it is open.
	pool.SetMaxIdleConns(maxConnections)
	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// WithLimits holds a smaller result than the app's own budget. See the
// Postgres driver's version: it is for the tests.
func (c *SqliteConnection) WithLimits(limits dbclient.Limits) *SqliteConnection {
	c.limits = limits
	return c
}

// Close closes the pool. Rust closed it when the last handle dropped.
func (c *SqliteConnection) Close() error {
	return c.pool.Close()
}

// sqliteSession is one tab's pinned connection. The same shape as the
// Postgres one and for the same reason — a `PRAGMA` and a temp table
// belong to the connection that made them — minus everything about
// cancelling: the rows come from a file on this machine, so there is no
// backend to reach into.
type sqliteSession struct {
	dbclient.SessionDefaults
	mu sync.Mutex
	// conn is nil once the session is closed.
	conn   *sql.Conn
	limits dbclient.Limits
}

func (s *sqliteSession) Backend() (dbclient.RunId, bool) {
	return 0, false
}

func (s *sqliteSession) Execute(ctx context.Context, sqlText string) (dbclient.QueryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return dbclient.QueryResult{}, errors.New("this tab's session is closed")
	}
	// The count rides on the connection, not the rows: see `collect`. It
	// is the same reasoning as `collect_capped` in `dbpostgres`; it holds
	// here too.
	return collect(ctx, s.conn, sqlText, s.limits)
}

// Begin is the same statement the Postgres session sends, and for the same
// reason it does not go through `Execute`: a boundary is not a run.
//
// SQLite leaves `Session.InTransaction` unanswered, so the app has only
// what it sent to go on here — see that method. It is why a second
// `BEGIN`, which SQLite refuses outright, must never be asked for.
func (s *sqliteSession) Begin(ctx context.Context) error {
	return s.boundary(ctx, "BEGIN")
}

// EndTransaction answers with `how`: SQLite refuses a `COMMIT` it cannot
// carry out rather than quietly rolling back, so what was asked for is
// what happened.
func (s *sqliteSession) EndTransaction(ctx context.Context, how dbclient.TxEnd) (dbclient.TxEnd, error) {
	if err := s.boundary(ctx, how.SQL()); err != nil {
		return how, err
	}
	return how, nil
}

// IsClosed is true once `Close` has run. The database is a file on this
// machine, so there is no peer to vanish underneath an open connection.
// A run holds the lock for as long as it is out, and a busy session is not
// a closed one, so it does not wait for the run.
func (s *sqliteSession) IsClosed() bool {
	if !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()
	return s.conn == nil
}

// Introspect reads the catalog down this tab's own connection, which is
// the only one that sees the databases this tab attached and the temp
// tables it made.
func (s *sqliteSession) Introspect(ctx context.Context) (*introspect.Catalog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil, errors.New("this tab's session is closed")
	}
	catalog, err := readCatalog(ctx, s.conn)
	if err != nil {
		return nil, err
	}
	return &catalog, nil
}

func (s *sqliteSession) Close(ctx context.Context) {
	s.mu.Lock()
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn == nil {
		return
	}
	_, err := conn.ExecContext(ctx, "ROLLBACK")
	var refused *sqlite.Error
	switch {
	case err == nil:
		conn.Close()
	// SQLite answers "cannot rollback - no transaction is active",
	// which is the ordinary case and not a reason to throw the
	// connection away. Only a connection that failed to *answer*
	// leaves the pool.
	case errors.As(err, &refused):
		conn.Close()
	default:
		detach(conn)
	}
}

// boundary is a transaction boundary, on this session's own connection:
// the transaction belongs to the connection that opened it.
func (s *sqliteSession) boundary(ctx context.Context, sqlText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return errors.New("this tab's session is closed")
	}
	if _, err := s.conn.ExecContext(ctx, sqlText); err != nil {
		return fmt.Errorf("SQLite refused %s: %w", sqlText, err)
	}
	return nil
}

// detach closes a pinned connection instead of giving it back to the
// pool: database/sql discards a connection whose last call reported
// `driver.ErrBadConn`. It is sqlx's `PoolConnection::detach`.
func detach(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// The Rust build needed `impl Drop for SqliteSession` because sqlx returns
// a pooled connection by spawning onto tokio, and the last handle to a
// session is usually dropped on the UI thread, which has no runtime.
// database/sql needs no runtime to give a connection back, but a session
// abandoned without `Close` would hold its connection checked out for
// ever. So the same fallback rides on the garbage collector: once nothing
// holds the session, its connection is detached — closed, never pooled,
// exactly as the Rust `Drop` did. A session that was closed properly
// leaves the cleanup nothing to do (its `*sql.Conn` is already done).
func watchSession(session *sqliteSession, conn *sql.Conn) {
	runtime.AddCleanup(session, detach, conn)
}

// --- the catalog reads ---

// querier is what a catalog read runs on: the pool, or one pinned
// connection. `ATTACH` and a temp table belong to the connection that
// made them, so a read that is to see them has to stay on one connection
// from the database list to the last pragma — see `readCatalog`.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// quoteIdent quotes a name for SQLite the way the standard says, doubling
// any `"` inside it. It is only for a schema name in front of
// `sqlite_master`, the one place a name cannot be bound: every other
// name goes to the pragma functions as a parameter.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// readCatalog reads one schema per database the connection has open:
// `main` first, then each `ATTACH`ed one in the order it was attached,
// and `temp` last, and only when it holds a relation — SQLite may list it
// on any connection, and an empty schema of that name would be noise in
// the sidebar.
//
// SQLite has no sequences, stored routines or user types, so a schema
// holds relations and nothing else.
func readCatalog(ctx context.Context, q querier) (introspect.Catalog, error) {
	var names []string
	if err := queryAll(ctx, q,
		"SELECT name FROM pragma_database_list ORDER BY seq",
		nil,
		func(rows *sql.Rows) error {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			names = append(names, name)
			return nil
		},
	); err != nil {
		return introspect.Catalog{}, err
	}
	hasTemp := slices.Contains(names, "temp")
	names = slices.DeleteFunc(names, func(name string) bool { return name == "temp" })
	if hasTemp {
		names = append(names, "temp")
	}

	schemas := make([]introspect.Schema, 0, len(names))
	for _, name := range names {
		tables, err := readTables(ctx, q, name)
		if err != nil {
			return introspect.Catalog{}, err
		}
		if name == "temp" && len(tables) == 0 {
			continue
		}
		schemas = append(schemas, introspect.Schema{
			Name:      name,
			Tables:    tables,
			Sequences: []introspect.Sequence{},
			Routines:  []introspect.Routine{},
			Types:     []introspect.UserType{},
		})
	}
	return introspect.Catalog{Schemas: schemas}, nil
}

// readTables reads the relations of one schema, each with its columns,
// keys, indexes and triggers.
//
// `sqlite_` is SQLite's own prefix, matched with its `_` escaped: a bare
// `_` in `LIKE` is any character, and would hide a user's `sqlitex`.
func readTables(ctx context.Context, q querier, schema string) ([]introspect.Table, error) {
	type named struct{ name, kind string }
	var names []named
	if err := queryAll(ctx, q,
		"SELECT name, type FROM "+quoteIdent(schema)+".sqlite_master "+
			`WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite\_%' ESCAPE '\' `+
			"ORDER BY name",
		nil,
		func(rows *sql.Rows) error {
			var n named
			if err := rows.Scan(&n.name, &n.kind); err != nil {
				return err
			}
			names = append(names, n)
			return nil
		},
	); err != nil {
		return nil, err
	}

	tables := make([]introspect.Table, 0, len(names))
	for _, n := range names {
		columns, primaryKey, err := readColumns(ctx, q, schema, n.name)
		if err != nil {
			return nil, err
		}
		kind := introspect.TableKindTable
		if n.kind == "view" {
			kind = introspect.TableKindView
		}
		indexes, err := readIndexes(ctx, q, schema, n.name)
		if err != nil {
			return nil, err
		}
		foreignKeys, err := readForeignKeys(ctx, q, schema, n.name)
		if err != nil {
			return nil, err
		}
		triggers, err := readTriggers(ctx, q, schema, n.name)
		if err != nil {
			return nil, err
		}
		tables = append(tables, introspect.Table{
			Name:       n.name,
			Kind:       kind,
			Columns:    columns,
			PrimaryKey: primaryKey,
			// SQLite keeps no row estimate; a per-table COUNT(*) would
			// scan every table on connect, so report nothing.
			ApproxRows:  nil,
			Indexes:     indexes,
			ForeignKeys: foreignKeys,
			// SQLite keeps a check only in the `CREATE TABLE` text, and
			// reading it back out would mean parsing SQL. A unique
			// constraint is an index here, and is listed there.
			Constraints: []introspect.Constraint{},
			Triggers:    triggers,
		})
	}
	return tables, nil
}

// Values of `pragma_table_xinfo`'s `hidden`. `pragma_table_info` is the
// same read with every hidden column left out, generated ones among them,
// so a table with a generated column used to show one column short.
const (
	columnVirtualHidden = 1 // a virtual table's own column: FTS5's `rank`
	columnVirtual       = 2 // GENERATED ALWAYS AS (…) VIRTUAL
	columnStored        = 3 // GENERATED ALWAYS AS (…) STORED
)

// readColumns reads a relation's columns, and its primary key in key
// order. A virtual table's hidden columns are left out: they are how the
// module takes arguments, not something a row holds, and `SELECT *` does
// not return them either.
//
// A generated column says so with an empty expression. SQLite keeps the
// expression only in the `CREATE TABLE` text, and the driver does not
// parse SQL to get it.
func readColumns(ctx context.Context, q querier, schema, table string) ([]introspect.Column, []string, error) {
	type ordered struct {
		ord int64
		col string
	}
	var pk []ordered
	columns := []introspect.Column{}
	if err := queryAll(ctx, q,
		`SELECT name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?1, ?2)`,
		[]any{table, schema},
		func(rows *sql.Rows) error {
			var (
				col, dataType        string
				notNull, ord, hidden int64
				def                  sql.NullString
			)
			if err := rows.Scan(&col, &dataType, &notNull, &def, &ord, &hidden); err != nil {
				return err
			}
			if hidden == columnVirtualHidden {
				return nil
			}
			if ord > 0 {
				pk = append(pk, ordered{ord, col})
			}
			var defPtr *string
			if def.Valid {
				d := def.String
				defPtr = &d
			}
			var generated *string
			if hidden == columnVirtual || hidden == columnStored {
				expression := ""
				generated = &expression
			}
			columns = append(columns, introspect.Column{
				Name:      col,
				DataType:  dataType,
				Nullable:  notNull == 0,
				Default:   defPtr,
				Generated: generated,
			})
			return nil
		},
	); err != nil {
		return nil, nil, err
	}
	slices.SortStableFunc(pk, func(a, b ordered) int {
		switch {
		case a.ord < b.ord:
			return -1
		case a.ord > b.ord:
			return 1
		}
		return 0
	})
	primaryKey := make([]string, 0, len(pk))
	for _, p := range pk {
		primaryKey = append(primaryKey, p.col)
	}
	return columns, primaryKey, nil
}

// readIndexes is a table's indexes, the implicit ones a `UNIQUE` or a
// `PRIMARY KEY` makes among them. `origin` says which: `pk`, `u`, or `c`
// for one made with `CREATE INDEX`.
func readIndexes(ctx context.Context, q querier, schema, table string) ([]introspect.Index, error) {
	type entry struct {
		name    string
		unique  int64
		origin  string
		partial int64
	}
	var list []entry
	if err := queryAll(ctx, q,
		`SELECT name, "unique", origin, partial FROM pragma_index_list(?1, ?2) ORDER BY name`,
		[]any{table, schema},
		func(rows *sql.Rows) error {
			var e entry
			if err := rows.Scan(&e.name, &e.unique, &e.origin, &e.partial); err != nil {
				return err
			}
			list = append(list, e)
			return nil
		},
	); err != nil {
		return nil, err
	}
	indexes := make([]introspect.Index, 0, len(list))
	for _, e := range list {
		// A key over an expression names no column, and reads `NULL`.
		var key []string
		if err := queryAll(ctx, q,
			"SELECT name FROM pragma_index_info(?1, ?2) ORDER BY seqno",
			[]any{e.name, schema},
			func(rows *sql.Rows) error {
				var column sql.NullString
				if err := rows.Scan(&column); err != nil {
					return err
				}
				if column.Valid {
					key = append(key, column.String)
				} else {
					key = append(key, "<expression>")
				}
				return nil
			},
		); err != nil {
			return nil, err
		}
		definition := fmt.Sprintf("(%s)", strings.Join(key, ", "))
		if e.partial != 0 {
			definition += " WHERE …"
		}
		indexes = append(indexes, introspect.Index{
			Name:       e.name,
			Definition: definition,
			Unique:     e.unique != 0,
			Primary:    e.origin == "pk",
		})
	}
	return indexes, nil
}

// readForeignKeys is a table's foreign keys. SQLite names none of them,
// so each is named the way Postgres would have named it:
// `orders_user_id_fkey`, and `orders_user_id_fkey1` for a second key over
// the same columns, which Postgres would have named that way too.
//
// A key's target is always in the table's own schema: SQLite resolves
// `REFERENCES` in the database the table lives in, and has no syntax to
// name another.
func readForeignKeys(ctx context.Context, q querier, schema, table string) ([]introspect.ForeignKey, error) {
	type numbered struct {
		id  int64
		key introspect.ForeignKey
	}
	var keys []numbered
	if err := queryAll(ctx, q,
		`SELECT id, "table", "from", "to" FROM pragma_foreign_key_list(?1, ?2) `+
			`ORDER BY id, seq`,
		[]any{table, schema},
		func(rows *sql.Rows) error {
			var (
				id           int64
				target, from string
				to           sql.NullString
			)
			if err := rows.Scan(&id, &target, &from, &to); err != nil {
				return err
			}
			if len(keys) == 0 || keys[len(keys)-1].id != id {
				keys = append(keys, numbered{
					id: id,
					key: introspect.ForeignKey{
						Name:          "",
						Columns:       []string{},
						TargetSchema:  schema,
						TargetTable:   target,
						TargetColumns: []string{},
					},
				})
			}
			key := &keys[len(keys)-1].key
			key.Columns = append(key.Columns, from)
			if to.Valid {
				key.TargetColumns = append(key.TargetColumns, to.String)
			}
			return nil
		},
	); err != nil {
		return nil, err
	}
	named := make([]introspect.ForeignKey, 0, len(keys))
	for _, k := range keys {
		key := k.key
		// No `to` means the key names the target's primary key, which
		// is where the columns have to be read from.
		if len(key.TargetColumns) != len(key.Columns) {
			primary := []string{}
			if err := queryAll(ctx, q,
				"SELECT name FROM pragma_table_info(?1, ?2) WHERE pk > 0 ORDER BY pk",
				[]any{key.TargetTable, schema},
				func(rows *sql.Rows) error {
					var name string
					if err := rows.Scan(&name); err != nil {
						return err
					}
					primary = append(primary, name)
					return nil
				},
			); err != nil {
				return nil, err
			}
			key.TargetColumns = primary
		}
		base := fmt.Sprintf("%s_%s_fkey", table, strings.Join(key.Columns, "_"))
		name := base
		n := 0
		for slices.ContainsFunc(named, func(other introspect.ForeignKey) bool { return other.Name == name }) {
			n++
			name = fmt.Sprintf("%s%d", base, n)
		}
		key.Name = name
		named = append(named, key)
	}
	return named, nil
}

// readTriggers is a table's triggers, read from the schema the table is
// in. A `TEMP` trigger on a table of `main` lives in `temp`'s list and is
// not shown: its `tbl_name` does not say which schema's table it means.
func readTriggers(ctx context.Context, q querier, schema, table string) ([]introspect.Trigger, error) {
	triggers := []introspect.Trigger{}
	err := queryAll(ctx, q,
		"SELECT name, sql FROM "+quoteIdent(schema)+".sqlite_master "+
			"WHERE type = 'trigger' AND tbl_name = ?1 ORDER BY name",
		[]any{table},
		func(rows *sql.Rows) error {
			var (
				name    string
				sqlText sql.NullString
			)
			if err := rows.Scan(&name, &sqlText); err != nil {
				return err
			}
			timing := ""
			if sqlText.Valid {
				timing = triggerTiming(sqlText.String)
			}
			triggers = append(triggers, introspect.Trigger{
				Name:   name,
				Timing: timing,
				// The body is inline in the trigger, not a function.
				Function: "",
			})
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return triggers, nil
}

// triggerTiming is when a trigger fires, read off its own `CREATE
// TRIGGER`: the words between its name and `ON`, lowercased — `before
// insert`, `after update of email`. SQLite keeps nothing else to read it
// from, and a statement this walk cannot follow says nothing rather than
// something wrong.
func triggerTiming(sqlText string) string {
	words := strings.Fields(sqlText)
	at := slices.IndexFunc(words, func(word string) bool { return eqIgnoreASCIICase(word, "trigger") })
	if at < 0 {
		return ""
	}
	rest := words[at+1:]
	if len(rest) >= 3 &&
		eqIgnoreASCIICase(rest[0], "if") &&
		eqIgnoreASCIICase(rest[1], "not") &&
		eqIgnoreASCIICase(rest[2], "exists") {
		rest = rest[3:]
	}
	// The name, which a quoted name with a space in it would break.
	if len(rest) < 1 {
		return ""
	}
	rest = rest[1:]
	on := slices.IndexFunc(rest, func(word string) bool { return eqIgnoreASCIICase(word, "on") })
	if on < 0 {
		return ""
	}
	timing := strings.ToLower(strings.Join(rest[:on], " "))
	// A trigger with no timing word fires before the statement.
	if strings.HasPrefix(timing, "before") || strings.HasPrefix(timing, "after") || strings.HasPrefix(timing, "instead") {
		return timing
	}
	return "before " + timing
}

// eqIgnoreASCIICase is Rust's `str::eq_ignore_ascii_case`: only ASCII
// letters fold, unlike `strings.EqualFold`.
func eqIgnoreASCIICase(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func (c *SqliteConnection) OpenSession(ctx context.Context) (dbclient.Session, error) {
	conn, err := c.pool.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("no connection for the session: %w", err)
	}
	session := &sqliteSession{
		conn:   conn,
		limits: c.limits,
	}
	watchSession(session, conn)
	return session, nil
}

// Introspect reads the catalog on one pooled connection, held from the
// database list to the last pragma, so every read agrees on which
// databases are attached. An `ATTACH` is per connection: one a tab ran on
// its session is seen by that session's `Introspect`, not here.
func (c *SqliteConnection) Introspect(ctx context.Context) (introspect.Catalog, error) {
	conn, err := c.pool.Conn(ctx)
	if err != nil {
		return introspect.Catalog{}, err
	}
	defer conn.Close()
	return readCatalog(ctx, conn)
}

func (c *SqliteConnection) ServerVersion(ctx context.Context) (string, error) {
	var version string
	if err := c.pool.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return "", fmt.Errorf("failed to read the SQLite version: %w", err)
	}
	return fmt.Sprintf("SQLite %s", version), nil
}

// Execute is streamed and capped by memory, as the Postgres driver is.
// There is no cancel to send: the rows come from a file on this machine,
// so dropping the stream ends the work rather than leaving a server to
// finish a result nobody will read.
//
// `dbclient.Wire` is left empty on purpose, here and on the session.
// There is no link to measure and no server to blame: the rows come off a
// local file, so the wall clock around the call is the whole story and
// splitting it would invent two numbers out of one.
func (c *SqliteConnection) Execute(ctx context.Context, sqlText string) (dbclient.QueryResult, error) {
	// The count is read off the connection the statement ran on, so the
	// statement and the count share one pooled connection.
	conn, err := c.pool.Conn(ctx)
	if err != nil {
		return dbclient.QueryResult{}, err
	}
	defer conn.Close()
	return collect(ctx, conn, sqlText, c.limits)
}

func (c *SqliteConnection) Apply(context.Context, []dbclient.RowChange) (uint64, error) {
	return 0, errors.New("in-place editing is not implemented yet (Phase 2)")
}

// collect runs one buffer on conn and streams its rows into a capped sink.
//
// sqlx's `fetch_many` handed over each statement's completion and with it
// `sqlite3_changes()`, the only place SQLite states how many rows an
// `UPDATE` changed; `fetch` dropped it, and so does `database/sql`, whose
// rows carry no count. The connection does: `total_changes()` counts every
// row its statements have changed since it opened, so the difference
// across the run is the run's count, read on the same connection the
// statement ran on.
func collect(ctx context.Context, conn *sql.Conn, sqlText string, limits dbclient.Limits) (dbclient.QueryResult, error) {
	before, err := totalChanges(ctx, conn)
	if err != nil {
		return dbclient.QueryResult{}, err
	}
	sink := dbclient.NewRowSink(limits)
	if err := conn.Raw(func(driverConn any) error {
		return stream(ctx, driverConn, sqlText, sink)
	}); err != nil {
		return dbclient.QueryResult{}, err
	}
	after, err := totalChanges(ctx, conn)
	if err != nil {
		return dbclient.QueryResult{}, err
	}
	affected := uint64(0)
	if after > before {
		affected = uint64(after - before)
	}
	return sink.Finish(affected), nil
}

// stream runs sqlText on the driver's own connection and pushes its rows
// into sink. It goes under database/sql rather than through it because
// the driver's rows have to be reached before the first row is read: see
// `undeclareTimes`.
//
// database/sql would also stop a run whose context ended, between two
// rows; the loop checks the context there for the same reason.
func stream(ctx context.Context, driverConn any, sqlText string, sink *dbclient.RowSink) error {
	queryer, ok := driverConn.(driver.QueryerContext)
	if !ok {
		return fmt.Errorf("the SQLite driver cannot run a query on %T", driverConn)
	}
	rows, err := queryer.QueryContext(ctx, sqlText, nil)
	if err != nil {
		return err
	}
	undeclareTimes(rows)
	columns := rows.Columns()
	dest := make([]driver.Value, len(columns))
	for {
		if err := ctx.Err(); err != nil {
			rows.Close()
			return err
		}
		err := rows.Next(dest)
		if err == io.EOF {
			break
		}
		if err != nil {
			rows.Close()
			return err
		}
		if !sink.HasColumns() {
			sink.Columns(slices.Clone(columns))
		}
		values := make([]dbclient.Value, 0, len(columns))
		for i := range dest {
			values = append(values, decode(dest[i]))
		}
		if !sink.Push(values) {
			break
		}
	}
	return rows.Close()
}

// undeclareTimes makes modernc read a column declared `DATE`, `DATETIME`
// or `TIMESTAMP` as the column it is: SQLite stores no date type, and
// what such a column holds is the text, number or blob that was written
// to it.
//
// modernc parses the text of such a column into a `time.Time` and does
// not keep the text, so `'2024-01-01'` came back as `2024-01-01
// 00:00:00+00:00` and a `'2024-01-01T10:00:00.500Z'` lost its zeros and
// its `Z` — a viewer showing something other than what is stored. No DSN
// option turns it off: `_texttotime` is the same parse for columns with
// no declared type, and `_inttotime` is the integer one, which is off by
// default. Which columns it parses is decided in `rows.Next` from
// `rows.decltypes`, the declared types read once when the statement is
// prepared, so blanking those three names there is a column with no
// declared type to `Next`, and its text comes back as the string it is.
//
// The field is modernc's own and unexported; `TestADateColumnShowsWhatIsStored`
// fails the day a bump of the pinned module renames it. Until then a
// missing field leaves the parse on — dates reformatted, never a failed
// run — and `decode` writes the time in modernc's own layout.
func undeclareTimes(rows driver.Rows) {
	value := reflect.ValueOf(rows)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return
	}
	declared := value.Elem().FieldByName("decltypes")
	if !declared.IsValid() || declared.Kind() != reflect.Slice || declared.Type().Elem().Kind() != reflect.String {
		return
	}
	for i := range declared.Len() {
		slot := declared.Index(i)
		switch slot.String() {
		case "DATE", "DATETIME", "TIMESTAMP":
			reflect.NewAt(slot.Type(), unsafe.Pointer(slot.UnsafeAddr())).Elem().SetString("")
		}
	}
}

func totalChanges(ctx context.Context, conn *sql.Conn) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx, "SELECT total_changes()").Scan(&n)
	return n, err
}

// decode turns what the driver read into a value. The Rust build matched
// on the value's own storage class (sqlx names it `INTEGER`, `REAL`,
// `BLOB` or `TEXT`), which is exactly what modernc hands back as a Go type.
//
// modernc reads no `BOOLEAN` column as a `bool` — a `2` stored there is
// the integer 2 — so a `bool` only arrives from a driver that changed
// its mind, and is shown as it says.
func decode(raw driver.Value) dbclient.Value {
	switch v := raw.(type) {
	case nil:
		return dbclient.ValueNull{}
	case bool:
		return dbclient.ValueBool(v)
	case int64:
		return dbclient.ValueInt(v)
	case float64:
		return dbclient.ValueFloat(v)
	case []byte:
		// modernc reads an empty blob as a nil slice. It is still a blob,
		// not SQL's NULL and not an empty string, so it stays `ValueBytes`
		// and is given a slice of its own.
		if v == nil {
			v = []byte{}
		}
		return dbclient.ValueBytes(v)
	case string:
		return dbclient.ValueText(v)
	case time.Time:
		// Only when `undeclareTimes` could not reach modernc's rows: the
		// stored text is gone by now, and this is modernc's own first
		// layout, the one it writes a time in.
		return dbclient.ValueText(v.Format("2006-01-02 15:04:05.999999999-07:00"))
	default:
		return dbclient.ValueText(fmt.Sprint(v))
	}
}

// queryAll runs a catalog read and hands each row to each.
// sqlx's `fetch_all`: every row is read before the next query goes out.
func queryAll(ctx context.Context, q querier, query string, args []any, each func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
