// Package storage is the local app state: connection profiles, window
// layout, query history, and the tabs each connection had open.
// Stored in a SQLite file under the user data directory (Zed `db` pattern).
//
// The Go build opens the very file the Rust build wrote: the same path,
// the same schema, the same `migrate` steps and the same JSON in
// `catalog_cache`, so a user who moves from one build to the other keeps
// every profile, every tab and every run.
package storage

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store is one handle on the local file. Rust held one rusqlite
// `Connection`; the Go store holds a `database/sql` pool capped at one
// connection, so every call and every transaction is on that one handle
// exactly as before, and the pragmas set on it hold for all of them.
type Store struct {
	conn *sql.DB
}

// historyLimit is how many runs one connection keeps. The history is a
// convenience, not an audit log, so the file stays small: every insert
// drops the oldest rows above this count.
const historyLimit int = 500

// TabLimit is how many tabs one connection restores. A session that has
// been left open for a week can hold hundreds of tabs, and reopening all
// of them would page every table in the database at once. The newest tabs
// — the end of the strip — are the ones kept.
const TabLimit int = 100

// SavedConnection is a saved profile plus what the connections screen
// remembers about it: when it was last opened, and what the last
// successful probe saw. The server description is cached so the screen has
// something to show without reaching the server at all.
type SavedConnection struct {
	Profile dbclient.Profile
	// LastOpened is unix seconds.
	LastOpened *int64
	// Server description, as the driver reports it ("PG 16.2").
	Server *string
	// Env is the environment tag the user gave the connection ("prod",
	// "staging"). Free text; the connections screen groups by it. It is
	// a label on the saved row, not a connection parameter, so it lives
	// here and not on `Profile`.
	Env *string
}

// RunSource is who asked for a statement. The history screen can hide the
// pages the app itself runs, so "mine only" means the statements the user
// wrote.
type RunSource int

const (
	// RunSourceUser is a statement the user ran from a query tab.
	RunSourceUser RunSource = iota
	// RunSourceApp is a statement the app built for itself: a table page,
	// a count.
	RunSourceApp
)

// String is the variant name, as Rust's derived Debug prints it.
func (s RunSource) String() string {
	switch s {
	case RunSourceUser:
		return "User"
	case RunSourceApp:
		return "App"
	}
	return fmt.Sprintf("RunSource(%d)", int(s))
}

// QueryRun is one remembered run, as the history screen paints it.
// `ElapsedMs` and `RowCount` are nil exactly when the run failed, and
// `Error` is non-nil exactly then.
type QueryRun struct {
	Id        int64
	Statement string
	// RanAt is unix seconds.
	RanAt     int64
	Source    RunSource
	ElapsedMs *uint64
	RowCount  *uint64
	// Affected is rows the run changed, as the server counted them. Its
	// own column rather than a value in `row_count`, because rows returned
	// and rows changed are different answers and a row that meant one of
	// them where the reader expected the other would be worse than no
	// column at all. nil for everything that returned rows, and for every
	// run an older build wrote.
	Affected *uint64
	Error    *string
}

// NewRun is a run on its way into the store.
type NewRun struct {
	// Scope is which connection ran it: a profile id, or a key derived
	// from the URL the app was started with. History is per connection,
	// so a staging query never shows up under production.
	Scope     string
	Statement string
	// RanAt is unix seconds.
	RanAt     int64
	Source    RunSource
	ElapsedMs *uint64
	RowCount  *uint64
	Affected  *uint64
	Error     *string
}

// HistoryFilter is what the history screen asks for: the two filter
// chips, the window it shows, and how many rows it is willing to paint.
type HistoryFilter struct {
	// UserOnly hides the statements the app built for itself.
	UserOnly bool
	// ErrorsOnly shows only the runs that failed.
	ErrorsOnly bool
	// Since is the oldest run to return, unix seconds. nil reads the
	// whole table.
	Since *int64
	Limit int
}

// SavedTab is one tab as the file remembers it: enough to open it again,
// and nothing more. A result set is never kept — a restored query tab
// shows its statement and waits, because the app does not re-run a
// statement behind the user.
//
// One of SavedTabTable, SavedTabQuery or SavedTabHistory.
type SavedTab interface {
	isSavedTab()
}

// SavedTabTable is the paged table view, on the page it was left on.
type SavedTabTable struct {
	Schema string
	Table  string
	Page   int
}

// SavedTabQuery is a query tab: its statement, the name on the strip, the
// relation it was opened on when it came from the sidebar, and which way
// it commits. The mode is kept and the transaction is not: a restored tab
// has no session, so there is nothing open to come back to.
type SavedTabQuery struct {
	Title     string
	Statement string
	// Relation is Rust's `Option<(String, String)>`: schema, then
	// relation.
	Relation *[2]string
	TxMode   dbclient.TxMode
}

// SavedTabHistory is the query-history tab. It is a view of this file, so
// it carries no state worth keeping.
type SavedTabHistory struct{}

func (SavedTabTable) isSavedTab()   {}
func (SavedTabQuery) isSavedTab()   {}
func (SavedTabHistory) isSavedTab() {}

// SavedTabs is what one connection had open, in strip order, and which of
// those tabs was in front.
type SavedTabs struct {
	Tabs   []SavedTab
	Active int
}

// DefaultHistoryFilter is Rust's `impl Default for HistoryFilter`.
func DefaultHistoryFilter() HistoryFilter {
	return HistoryFilter{
		UserOnly:   false,
		ErrorsOnly: false,
		Since:      nil,
		Limit:      historyLimit,
	}
}

// OpenDefault opens (and migrates) the app database at the default
// location.
func OpenDefault() (*Store, error) {
	dir, err := dataDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return Open(filepath.Join(dir, "meerkat.sqlite"))
}

// Open opens (and migrates) the app database at path.
func Open(path string) (*Store, error) {
	conn, err := sql.Open("sqlite", dsn(path))
	if err == nil {
		// One connection, kept for the life of the store: the Rust store
		// was one rusqlite handle, and a pragma is a property of the
		// connection that ran it.
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)
		conn.SetConnMaxLifetime(0)
		conn.SetConnMaxIdleTime(0)
		err = conn.Ping()
	}
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	// **Every window opens its own handle on this one file**, so two
	// windows can be writing it at the same moment: a run lands in one
	// while the other saves its tab strip. WAL lets a reader and a
	// writer through together, and the busy timeout is what turns the
	// remaining overlap into a short wait instead of `SQLITE_BUSY` —
	// the default timeout is zero, which fails on the spot. Neither is
	// a tuning knob: without them a second window makes the first one
	// lose writes.
	if _, err := conn.Exec(
		`PRAGMA journal_mode = WAL;
		 PRAGMA busy_timeout = 2000;`,
	); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to set the store's pragmas: %w", err)
	}
	if _, err := conn.Exec(
		`CREATE TABLE IF NOT EXISTS profiles (
                id TEXT PRIMARY KEY,
                name TEXT NOT NULL,
                engine TEXT NOT NULL,
                host TEXT,
                port INTEGER,
                database TEXT NOT NULL,
                user TEXT
            );
            CREATE TABLE IF NOT EXISTS query_history (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                scope TEXT NOT NULL,
                statement TEXT NOT NULL,
                ran_at INTEGER NOT NULL,
                source TEXT NOT NULL,
                elapsed_ms INTEGER,
                row_count INTEGER,
                affected INTEGER,
                error TEXT
            );
            CREATE INDEX IF NOT EXISTS query_history_scope_time
                ON query_history (scope, ran_at DESC);
            CREATE TABLE IF NOT EXISTS catalog_cache (
                scope TEXT PRIMARY KEY,
                catalog TEXT NOT NULL,
                cached_at INTEGER NOT NULL
            );
            CREATE TABLE IF NOT EXISTS ui_state (
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS open_tabs (
                scope TEXT NOT NULL,
                position INTEGER NOT NULL,
                kind TEXT NOT NULL,
                title TEXT,
                schema TEXT,
                relation TEXT,
                page INTEGER,
                statement TEXT,
                active INTEGER NOT NULL DEFAULT 0,
                PRIMARY KEY (scope, position)
            );`,
	); err != nil {
		conn.Close()
		return nil, err
	}
	store := &Store{conn: conn}
	if err := store.migrate(); err != nil {
		conn.Close()
		return nil, err
	}
	return store, nil
}

// dsn is the driver's name for the file at path. The busy timeout rides
// in the name as well as in the pragma `Open` runs, so a connection the
// pool ever has to open again carries it too; WAL is a property of the
// file and needs no repeating.
func dsn(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String() + "?_pragma=busy_timeout%282000%29"
}

// Close gives the file back. Rust closed the handle when the `Store` was
// dropped; Go has to be told.
func (s *Store) Close() error {
	return s.conn.Close()
}

// migrate adds the columns added after the first release. Each one is
// added on its own, so an older file keeps its rows.
func (s *Store) migrate() error {
	// `tables` and `views` were dropped from the screen; an older file
	// keeps those columns, and nothing reads them.
	//
	// `read_only` arrives NULL on every row an older build saved, and
	// `list_connections` reads NULL as on: a connection saved before
	// the app could be told to be careful is one nobody said could
	// write.
	//
	// A `tx_mode` column here is what a build that asked the *profile*
	// who commits left behind. The mode belongs to a tab, so nothing
	// reads it any more; an older file keeps the column, as it keeps
	// `tables` and `views`.
	if err := s.addColumns(
		"profiles",
		[][2]string{
			{"last_opened", "INTEGER"},
			{"server", "TEXT"},
			{"env", "TEXT"},
			{"read_only", "INTEGER"},
			// The connection string's parameters a profile has no
			// field for, query-encoded: `sslmode`, `connect_timeout`…
			{"params", "TEXT"},
		},
	); err != nil {
		return err
	}
	// A query tab remembers which way it commits, so a tab switched to
	// manual comes back manual. NULL is auto.
	if err := s.addColumns("open_tabs", [][2]string{{"tx_mode", "TEXT"}}); err != nil {
		return err
	}
	// Rows changed, for the runs that answer with a count instead of a
	// result set. NULL on every row an older build wrote, which is the
	// same thing it means on a new one: this run reported no count.
	if err := s.addColumns("query_history", [][2]string{{"affected", "INTEGER"}}); err != nil {
		return err
	}
	return nil
}

func (s *Store) addColumns(table string, columns [][2]string) error {
	existing, err := func() ([]string, error) {
		rows, err := s.conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var (
				cid       int64
				name      string
				kind      sql.RawBytes
				notNull   int64
				dfltValue sql.RawBytes
				pk        int64
			)
			if err := rows.Scan(&cid, &name, &kind, &notNull, &dfltValue, &pk); err != nil {
				return nil, err
			}
			names = append(names, name)
		}
		return names, rows.Err()
	}()
	if err != nil {
		return err
	}
	for _, c := range columns {
		column, kind := c[0], c[1]
		found := false
		for _, name := range existing {
			if name == column {
				found = true
				break
			}
		}
		if !found {
			if _, err := s.conn.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, kind)); err != nil {
				return err
			}
		}
	}
	return nil
}

// SaveProfile saves a profile, keeping whatever the connections screen has
// already learned about it. A plain `INSERT OR REPLACE` would throw the
// counts and the last-opened time away on every edit.
func (s *Store) SaveProfile(p *dbclient.Profile) error {
	var port any
	if p.Port != nil {
		port = int64(*p.Port)
	}
	_, err := s.conn.Exec(
		`INSERT INTO profiles
                (id, name, engine, host, port, database, user, read_only, params)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
             ON CONFLICT(id) DO UPDATE SET
                name = excluded.name,
                engine = excluded.engine,
                host = excluded.host,
                port = excluded.port,
                database = excluded.database,
                user = excluded.user,
                read_only = excluded.read_only,
                params = excluded.params`,
		p.Id,
		p.Name,
		engineStr(p.Engine),
		optString(p.Host),
		port,
		p.Database,
		optString(p.User),
		p.ReadOnly,
		p.Params,
	)
	return err
}

// ListProfiles is every saved profile, in `ListConnections` order.
func (s *Store) ListProfiles() ([]dbclient.Profile, error) {
	connections, err := s.ListConnections()
	if err != nil {
		return nil, err
	}
	profiles := make([]dbclient.Profile, 0, len(connections))
	for _, c := range connections {
		profiles = append(profiles, c.Profile)
	}
	return profiles, nil
}

// ListConnections is every saved connection, most recently opened first. A
// profile that has never been opened sorts after the ones that have, by
// name.
func (s *Store) ListConnections() ([]SavedConnection, error) {
	rows, err := s.conn.Query(
		`SELECT id, name, engine, host, port, database, user,
                    last_opened, server, env, read_only, params
             FROM profiles
             ORDER BY last_opened IS NULL, last_opened DESC, name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	connections := []SavedConnection{}
	for rows.Next() {
		var (
			id, name, engine, database string
			host, user, server, env    sql.NullString
			params                     sql.NullString
			port, lastOpened, readOnly sql.NullInt64
		)
		if err := rows.Scan(&id, &name, &engine, &host, &port, &database, &user,
			&lastOpened, &server, &env, &readOnly, &params); err != nil {
			return nil, err
		}
		portValue, err := optU16(port)
		if err != nil {
			return nil, err
		}
		connections = append(connections, SavedConnection{
			Profile: dbclient.Profile{
				Id:       id,
				Name:     name,
				Engine:   parseEngine(engine),
				Host:     fromNullString(host),
				Port:     portValue,
				Database: database,
				User:     fromNullString(user),
				// A row an older build wrote has no answer here, and
				// the careful reading of no answer is "do not write".
				ReadOnly: !readOnly.Valid || readOnly.Int64 != 0,
				Params:   params.String,
			},
			LastOpened: fromNullInt64(lastOpened),
			Server:     fromNullString(server),
			Env:        fromNullString(env),
		})
	}
	return connections, rows.Err()
}

// Flag is a small on/off remembered across launches ("group by env"). The
// `ui_state` table is a key-value bag for exactly this: screen state too
// small to deserve a table, too annoying to lose.
func (s *Store) Flag(key string, def bool) bool {
	var value string
	if err := s.conn.QueryRow("SELECT value FROM ui_state WHERE key = ?1", key).Scan(&value); err != nil {
		return def
	}
	return value == "1"
}

// SetFlag remembers an on/off under key.
func (s *Store) SetFlag(key string, value bool) error {
	text := "0"
	if value {
		text = "1"
	}
	_, err := s.conn.Exec(
		`INSERT INTO ui_state (key, value) VALUES (?1, ?2)
             ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, text,
	)
	return err
}

// UiValue is the same bag, for a value that is not an on/off — a pane size
// the user dragged, remembered across launches. A missing key or an
// unreadable file both answer false: the caller has a default, and screen
// state is never worth an error.
func (s *Store) UiValue(key string) (string, bool) {
	var value string
	if err := s.conn.QueryRow("SELECT value FROM ui_state WHERE key = ?1", key).Scan(&value); err != nil {
		return "", false
	}
	return value, true
}

// SetUiValue remembers value under key.
func (s *Store) SetUiValue(key, value string) error {
	_, err := s.conn.Exec(
		`INSERT INTO ui_state (key, value) VALUES (?1, ?2)
             ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// EnvOf is the environment tag of one profile, for the shell to read on
// its way in: the frame it wears is decided once, when the session opens.
// An unknown id reads as untagged.
func (s *Store) EnvOf(id string) (*string, error) {
	var env sql.NullString
	err := s.conn.QueryRow("SELECT env FROM profiles WHERE id = ?1", id).Scan(&env)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fromNullString(env), nil
}

// SetEnv tags a connection with an environment ("prod", "staging"), or
// takes the tag away with nil. The tag is display metadata, so it is
// written beside the profile rather than through `SaveProfile`.
func (s *Store) SetEnv(id string, env *string) error {
	_, err := s.conn.Exec(
		"UPDATE profiles SET env = ?2 WHERE id = ?1",
		id, optString(env),
	)
	return err
}

// MarkOpened remembers that the user opened this connection, so it sorts
// to the top next time. `at` is unix seconds.
func (s *Store) MarkOpened(id string, at int64) error {
	_, err := s.conn.Exec(
		"UPDATE profiles SET last_opened = ?2 WHERE id = ?1",
		id, at,
	)
	return err
}

// RecordProbe caches what a successful probe saw, so the next launch can
// paint the row without a probe of its own.
func (s *Store) RecordProbe(id, server string) error {
	_, err := s.conn.Exec(
		"UPDATE profiles SET server = ?2 WHERE id = ?1",
		id, server,
	)
	return err
}

// DeleteProfile forgets a profile.
func (s *Store) DeleteProfile(id string) error {
	_, err := s.conn.Exec("DELETE FROM profiles WHERE id = ?1", id)
	return err
}

// --- catalog cache ----------------------------------------------------

// CachedCatalog is the catalog this connection had the last time it was
// open, as JSON. The shell paints the sidebar from it while the real
// introspection runs, so a large database opens at once instead of after
// a round trip. A row that no longer parses — the model changed under it
// — is dropped and reported as a miss, never as an error.
func (s *Store) CachedCatalog(scope string) (*introspect.Catalog, error) {
	var text string
	err := s.conn.QueryRow(
		"SELECT catalog FROM catalog_cache WHERE scope = ?1",
		scope,
	).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var catalog introspect.Catalog
	if err := json.Unmarshal([]byte(text), &catalog); err != nil {
		if err := s.ForgetCatalog(scope); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return &catalog, nil
}

// CacheCatalog keeps the catalog a successful introspection produced. One
// row per connection, overwritten every time, so the file holds the
// current shape and nothing older.
func (s *Store) CacheCatalog(scope string, catalog *introspect.Catalog, at int64) error {
	text, err := catalogJSON(catalog)
	if err != nil {
		return err
	}
	_, err = s.conn.Exec(
		`INSERT INTO catalog_cache (scope, catalog, cached_at)
             VALUES (?1, ?2, ?3)
             ON CONFLICT(scope) DO UPDATE SET
                catalog = excluded.catalog,
                cached_at = excluded.cached_at`,
		scope, text, at,
	)
	return err
}

// catalogJSON is `serde_json::to_string(catalog)`, byte for byte: no HTML
// escaping (serde writes `>` as it is) and no trailing newline (which
// `Encoder.Encode` adds).
func catalogJSON(catalog *introspect.Catalog) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(catalog); err != nil {
		return "", err
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// ForgetCatalog drops the cached catalog of one connection.
func (s *Store) ForgetCatalog(scope string) error {
	_, err := s.conn.Exec("DELETE FROM catalog_cache WHERE scope = ?1", scope)
	return err
}

// --- open tabs --------------------------------------------------------

// SavedTabs is the tabs this connection had open, in strip order. A row
// the model no longer understands is dropped rather than reported: a
// session must open even when the file holds something older.
func (s *Store) SavedTabs(scope string) (SavedTabs, error) {
	rows, err := s.conn.Query(
		`SELECT kind, title, schema, relation, page, statement, active, tx_mode
               FROM open_tabs
              WHERE scope = ?1
              ORDER BY position`,
		scope,
	)
	if err != nil {
		return SavedTabs{}, err
	}
	defer rows.Close()

	var tabs SavedTabs
	for rows.Next() {
		var (
			kind                                    string
			title, schema, relation, statement, txm sql.NullString
			page                                    sql.NullInt64
			active                                  int64
		)
		if err := rows.Scan(&kind, &title, &schema, &relation, &page, &statement, &active, &txm); err != nil {
			return SavedTabs{}, err
		}
		var tab SavedTab
		switch kind {
		case "table":
			if !schema.Valid || !relation.Valid {
				continue
			}
			p := int64(0)
			if page.Valid {
				p = page.Int64
			}
			tab = SavedTabTable{
				Schema: schema.String,
				Table:  relation.String,
				Page:   int(max(p, 0)),
			}
		case "query":
			var rel *[2]string
			if schema.Valid && relation.Valid {
				rel = &[2]string{schema.String, relation.String}
			}
			tab = SavedTabQuery{
				Title:     title.String,
				Statement: statement.String,
				Relation:  rel,
				TxMode:    dbclient.ParseTxMode(fromNullString(txm)),
			}
		case "history":
			tab = SavedTabHistory{}
		default:
			continue
		}
		if active != 0 {
			tabs.Active = len(tabs.Tabs)
		}
		tabs.Tabs = append(tabs.Tabs, tab)
	}
	if err := rows.Err(); err != nil {
		return SavedTabs{}, err
	}
	return tabs, nil
}

// SaveTabs replaces what this connection had open. The whole set is
// written at once, in one transaction, because a half-written strip is
// worse than the strip it replaced.
//
// Above `TabLimit` the oldest tabs — the front of the strip — go, and
// `active` moves with what is left.
func (s *Store) SaveTabs(scope string, tabs []SavedTab, active int) error {
	dropped := saturatingSub(len(tabs), TabLimit)
	kept := tabs[dropped:]
	active = saturatingSub(active, dropped)

	tx, err := s.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM open_tabs WHERE scope = ?1", scope); err != nil {
		return err
	}
	for position, tab := range kept {
		var kind string
		var title, schema, relation, page, statement, txMode any
		switch tab := tab.(type) {
		case SavedTabTable:
			kind = "table"
			schema = tab.Schema
			relation = tab.Table
			page = int64(tab.Page)
		case SavedTabQuery:
			kind = "query"
			title = tab.Title
			if tab.Relation != nil {
				schema = tab.Relation[0]
				relation = tab.Relation[1]
			}
			statement = tab.Statement
			txMode = tab.TxMode.AsStr()
		case SavedTabHistory:
			kind = "history"
		default:
			return fmt.Errorf("unknown saved tab %T", tab)
		}
		isActive := int64(0)
		if position == active {
			isActive = 1
		}
		if _, err := tx.Exec(
			`INSERT INTO open_tabs
                    (scope, position, kind, title, schema, relation, page, statement, active,
                     tx_mode)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)`,
			scope,
			int64(position),
			kind,
			title,
			schema,
			relation,
			page,
			statement,
			isActive,
			txMode,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- query history ---------------------------------------------------

// RecordQuery remembers one run, then drops this connection's oldest runs
// above `historyLimit`. A failed run is remembered too: the run the user
// most wants to find again is usually the one that broke.
func (s *Store) RecordQuery(run NewRun) error {
	elapsed, err := optU64(run.ElapsedMs)
	if err != nil {
		return err
	}
	rowCount, err := optU64(run.RowCount)
	if err != nil {
		return err
	}
	affected, err := optU64(run.Affected)
	if err != nil {
		return err
	}
	if _, err := s.conn.Exec(
		`INSERT INTO query_history
                (scope, statement, ran_at, source, elapsed_ms, row_count, affected, error)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)`,
		run.Scope,
		run.Statement,
		run.RanAt,
		sourceStr(run.Source),
		elapsed,
		rowCount,
		affected,
		optString(run.Error),
	); err != nil {
		return err
	}
	return s.pruneHistory(run.Scope)
}

func (s *Store) pruneHistory(scope string) error {
	_, err := s.conn.Exec(
		`DELETE FROM query_history
              WHERE scope = ?1
                AND id NOT IN (
                    SELECT id FROM query_history
                     WHERE scope = ?1
                     ORDER BY ran_at DESC, id DESC
                     LIMIT ?2)`,
		scope, int64(historyLimit),
	)
	return err
}

// ListHistory is one connection's runs, newest first. The filters are
// bound rather than spliced, so one prepared statement serves every chip
// combination.
func (s *Store) ListHistory(scope string, filter HistoryFilter) ([]QueryRun, error) {
	since := int64(0)
	if filter.Since != nil {
		since = *filter.Since
	}
	// A zero limit is a filter nobody filled in, not a request for no
	// runs: the Rust `HistoryFilter::default()` carried the limit, and Go's
	// zero value does not. ⌘K read its runs through a bare
	// `HistoryFilter{}` and got `LIMIT 0` — an empty RECENT QUERIES over a
	// full history. So zero reads the default cap, as the Rust default did.
	limit := int64(filter.Limit)
	if limit <= 0 {
		limit = int64(historyLimit)
	}
	rows, err := s.conn.Query(
		`SELECT id, statement, ran_at, source, elapsed_ms, row_count, affected, error
               FROM query_history
              WHERE scope = ?1
                AND (?2 = 0 OR source = 'user')
                AND (?3 = 0 OR error IS NOT NULL)
                AND (?4 = 0 OR ran_at >= ?5)
              ORDER BY ran_at DESC, id DESC
              LIMIT ?6`,
		scope,
		boolInt(filter.UserOnly),
		boolInt(filter.ErrorsOnly),
		boolInt(filter.Since != nil),
		since,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []QueryRun{}
	for rows.Next() {
		var (
			run                         QueryRun
			source                      string
			elapsed, rowCount, affected sql.NullInt64
			errorText                   sql.NullString
		)
		if err := rows.Scan(&run.Id, &run.Statement, &run.RanAt, &source,
			&elapsed, &rowCount, &affected, &errorText); err != nil {
			return nil, err
		}
		run.Source = parseSource(source)
		if run.ElapsedMs, err = fromNullU64(elapsed); err != nil {
			return nil, err
		}
		if run.RowCount, err = fromNullU64(rowCount); err != nil {
			return nil, err
		}
		if run.Affected, err = fromNullU64(affected); err != nil {
			return nil, err
		}
		run.Error = fromNullString(errorText)
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func sourceStr(source RunSource) string {
	switch source {
	case RunSourceUser:
		return "user"
	default:
		return "app"
	}
}

func parseSource(s string) RunSource {
	switch s {
	case "user":
		return RunSourceUser
	default:
		return RunSourceApp
	}
}

func engineStr(engine dbclient.Engine) string {
	switch engine {
	case dbclient.EngineSqlite:
		return "sqlite"
	case dbclient.EnginePostgres:
		return "postgres"
	case dbclient.EngineMysql:
		return "mysql"
	}
	return "sqlite"
}

func parseEngine(s string) dbclient.Engine {
	switch s {
	case "postgres":
		return dbclient.EnginePostgres
	case "mysql":
		return dbclient.EngineMysql
	default:
		return dbclient.EngineSqlite
	}
}

// dataDir is `dirs::data_dir()` joined with `meerkat`: the same folder
// the Rust build used, so the Go build opens the file it wrote.
func dataDir() (string, error) {
	base, err := userDataDir()
	if err != nil {
		return "", fmt.Errorf("no user data directory: %w", err)
	}
	return filepath.Join(base, "meerkat"), nil
}

// userDataDir is `dirs::data_dir()` (dirs 6): `~/Library/Application
// Support` on macOS, the roaming AppData on Windows, and
// `$XDG_DATA_HOME` (when absolute) or `~/.local/share` elsewhere.
// `os.UserConfigDir` agrees on the first two and not on the third, which
// is why this is spelled out.
func userDataDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		return os.UserConfigDir()
	case "darwin", "ios":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support"), nil
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); dir != "" && filepath.IsAbs(dir) {
			return dir, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share"), nil
	}
}

// --- rusqlite's conversions ---------------------------------------------

func saturatingSub(a, b int) int {
	if a < b {
		return 0
	}
	return a - b
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func optString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// optU64 binds an `Option<u64>` the way rusqlite does: as an i64, and an
// error rather than a wrapped value when it does not fit.
func optU64(v *uint64) (any, error) {
	if v == nil {
		return nil, nil
	}
	if *v > math.MaxInt64 {
		return nil, fmt.Errorf("integer %d is out of range", *v)
	}
	return int64(*v), nil
}

func fromNullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func fromNullInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// fromNullU64 reads an `Option<u64>` the way rusqlite does: a negative
// value is an error, not a wrapped one.
func fromNullU64(v sql.NullInt64) (*uint64, error) {
	if !v.Valid {
		return nil, nil
	}
	if v.Int64 < 0 {
		return nil, fmt.Errorf("integer %d is out of range", v.Int64)
	}
	n := uint64(v.Int64)
	return &n, nil
}

// optU16 reads an `Option<u16>` the way rusqlite does.
func optU16(v sql.NullInt64) (*uint16, error) {
	if !v.Valid {
		return nil, nil
	}
	if v.Int64 < 0 || v.Int64 > math.MaxUint16 {
		return nil, fmt.Errorf("integer %d is out of range", v.Int64)
	}
	n := uint16(v.Int64)
	return &n, nil
}
