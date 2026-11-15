// Package dbsqlite is the SQLite driver, backed by modernc.org/sqlite
// through database/sql. (The Rust build used sqlx.)
package dbsqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

func (s *sqliteSession) EndTransaction(ctx context.Context, how dbclient.TxEnd) error {
	return s.boundary(ctx, how.SQL())
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

// --- the per-table reads `introspect` makes beside `pragma_table_info` ---

// indexes is a table's indexes, the implicit ones a `UNIQUE` or a
// `PRIMARY KEY` makes among them. `origin` says which: `pk`, `u`, or `c`
// for one made with `CREATE INDEX`.
func (c *SqliteConnection) indexes(ctx context.Context, table string) ([]introspect.Index, error) {
	type entry struct {
		name    string
		unique  int64
		origin  string
		partial int64
	}
	var list []entry
	if err := queryAll(ctx, c.pool,
		`SELECT name, "unique", origin, partial FROM pragma_index_list(?1) ORDER BY name`,
		[]any{table},
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
		if err := queryAll(ctx, c.pool,
			"SELECT name FROM pragma_index_info(?1) ORDER BY seqno",
			[]any{e.name},
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

// foreignKeys is a table's foreign keys. SQLite names none of them, so
// each is named the way Postgres would have named it:
// `orders_user_id_fkey`, and `orders_user_id_fkey1` for a second key over
// the same columns, which Postgres would have named that way too.
func (c *SqliteConnection) foreignKeys(ctx context.Context, table string) ([]introspect.ForeignKey, error) {
	type numbered struct {
		id  int64
		key introspect.ForeignKey
	}
	var keys []numbered
	if err := queryAll(ctx, c.pool,
		`SELECT id, "table", "from", "to" FROM pragma_foreign_key_list(?1) `+
			`ORDER BY id, seq`,
		[]any{table},
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
						TargetSchema:  "main",
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
			if err := queryAll(ctx, c.pool,
				"SELECT name FROM pragma_table_info(?1) WHERE pk > 0 ORDER BY pk",
				[]any{key.TargetTable},
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

func (c *SqliteConnection) triggers(ctx context.Context, table string) ([]introspect.Trigger, error) {
	triggers := []introspect.Trigger{}
	err := queryAll(ctx, c.pool,
		"SELECT name, sql FROM sqlite_master "+
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

func (c *SqliteConnection) Introspect(ctx context.Context) (introspect.Catalog, error) {
	type named struct{ name, kind string }
	var names []named
	if err := queryAll(ctx, c.pool,
		"SELECT name, type FROM sqlite_master "+
			"WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' "+
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
		return introspect.Catalog{}, err
	}

	tables := make([]introspect.Table, 0, len(names))
	for _, n := range names {
		type ordered struct {
			ord int64
			col string
		}
		var pk []ordered
		columns := []introspect.Column{}
		if err := queryAll(ctx, c.pool,
			`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?1)`,
			[]any{n.name},
			func(rows *sql.Rows) error {
				var (
					col, dataType string
					notNull, ord  int64
					def           sql.NullString
				)
				if err := rows.Scan(&col, &dataType, &notNull, &def, &ord); err != nil {
					return err
				}
				if ord > 0 {
					pk = append(pk, ordered{ord, col})
				}
				var defPtr *string
				if def.Valid {
					d := def.String
					defPtr = &d
				}
				columns = append(columns, introspect.Column{
					Name:     col,
					DataType: dataType,
					Nullable: notNull == 0,
					Default:  defPtr,
				})
				return nil
			},
		); err != nil {
			return introspect.Catalog{}, err
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

		kind := introspect.TableKindTable
		if n.kind == "view" {
			kind = introspect.TableKindView
		}
		indexes, err := c.indexes(ctx, n.name)
		if err != nil {
			return introspect.Catalog{}, err
		}
		foreignKeys, err := c.foreignKeys(ctx, n.name)
		if err != nil {
			return introspect.Catalog{}, err
		}
		triggers, err := c.triggers(ctx, n.name)
		if err != nil {
			return introspect.Catalog{}, err
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

	// SQLite has no sequences, stored routines or user types, so the
	// one schema holds relations and nothing else.
	return introspect.Catalog{
		Schemas: []introspect.Schema{{
			Name:      "main",
			Tables:    tables,
			Sequences: []introspect.Sequence{},
			Routines:  []introspect.Routine{},
			Types:     []introspect.UserType{},
		}},
	}, nil
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
	rows, err := conn.QueryContext(ctx, sqlText)
	if err != nil {
		return dbclient.QueryResult{}, err
	}
	columns, err := rows.Columns()
	if err != nil {
		rows.Close()
		return dbclient.QueryResult{}, err
	}
	raw := make([]any, len(columns))
	dest := make([]any, len(columns))
	for i := range raw {
		dest[i] = &raw[i]
	}
	for rows.Next() {
		if !sink.HasColumns() {
			sink.Columns(slices.Clone(columns))
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return dbclient.QueryResult{}, err
		}
		values := make([]dbclient.Value, 0, len(columns))
		for i := range raw {
			values = append(values, decode(raw[i]))
		}
		if !sink.Push(values) {
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return dbclient.QueryResult{}, err
	}
	if err := rows.Close(); err != nil {
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

func totalChanges(ctx context.Context, conn *sql.Conn) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx, "SELECT total_changes()").Scan(&n)
	return n, err
}

// decode turns what the driver read into a value. The Rust build matched
// on the value's own storage class (sqlx names it `INTEGER`, `REAL`,
// `BLOB` or `TEXT`), which is exactly what modernc hands back as a Go type.
func decode(raw any) dbclient.Value {
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
		return dbclient.ValueBytes(v)
	case string:
		return dbclient.ValueText(v)
	case time.Time:
		// modernc parses the text of a column declared DATE, DATETIME
		// or TIMESTAMP into a time, which sqlx never did, and does not
		// keep the text it parsed. This is its own first layout, the one
		// it writes a time in.
		return dbclient.ValueText(v.Format("2006-01-02 15:04:05.999999999-07:00"))
	default:
		return dbclient.ValueText(fmt.Sprint(v))
	}
}

// queryAll runs a catalog read on the pool and hands each row to each.
// sqlx's `fetch_all`: every row is read before the next query goes out.
func queryAll(ctx context.Context, pool *sql.DB, query string, args []any, each func(*sql.Rows) error) error {
	rows, err := pool.QueryContext(ctx, query, args...)
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
