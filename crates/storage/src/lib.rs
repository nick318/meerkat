//! Local app state: connection profiles, window layout, query history,
//! and the tabs each connection had open.
//! Stored in a SQLite file under the user data directory (Zed `db` pattern).

use anyhow::{Context as _, Result};
use db_client::{Engine, Profile, TxMode};
use introspect::Catalog;
use rusqlite::{Connection, OptionalExtension as _};
use std::path::PathBuf;

pub struct Store {
    conn: Connection,
}

/// How many runs one connection keeps. The history is a convenience, not
/// an audit log, so the file stays small: every insert drops the oldest
/// rows above this count.
const HISTORY_LIMIT: usize = 500;

/// How many tabs one connection restores. A session that has been left
/// open for a week can hold hundreds of tabs, and reopening all of them
/// would page every table in the database at once. The newest tabs — the
/// end of the strip — are the ones kept.
pub const TAB_LIMIT: usize = 100;

/// A saved profile plus what the connections screen remembers about it:
/// when it was last opened, and what the last successful probe saw. The
/// server description is cached so the screen has something to show
/// without reaching the server at all.
#[derive(Debug, Clone)]
pub struct SavedConnection {
    pub profile: Profile,
    /// Unix seconds.
    pub last_opened: Option<i64>,
    /// Server description, as the driver reports it ("PG 16.2").
    pub server: Option<String>,
    /// The environment tag the user gave the connection ("prod",
    /// "staging"). Free text; the connections screen groups by it. It is
    /// a label on the saved row, not a connection parameter, so it lives
    /// here and not on `Profile`.
    pub env: Option<String>,
}

/// Who asked for a statement. The history screen can hide the pages the
/// app itself runs, so "mine only" means the statements the user wrote.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RunSource {
    /// A statement the user ran from a query tab.
    User,
    /// A statement the app built for itself: a table page, a count.
    App,
}

/// One remembered run, as the history screen paints it. `elapsed_ms` and
/// `row_count` are `None` exactly when the run failed, and `error` is
/// `Some` exactly then.
#[derive(Debug, Clone)]
pub struct QueryRun {
    pub id: i64,
    pub statement: String,
    /// Unix seconds.
    pub ran_at: i64,
    pub source: RunSource,
    pub elapsed_ms: Option<u64>,
    pub row_count: Option<u64>,
    /// Rows the run changed, as the server counted them. Its own column
    /// rather than a value in `row_count`, because rows returned and rows
    /// changed are different answers and a row that meant one of them where
    /// the reader expected the other would be worse than no column at all.
    /// `None` for everything that returned rows, and for every run an older
    /// build wrote.
    pub affected: Option<u64>,
    pub error: Option<String>,
}

/// A run on its way into the store.
#[derive(Debug, Clone)]
pub struct NewRun<'a> {
    /// Which connection ran it: a profile id, or a key derived from the
    /// URL the app was started with. History is per connection, so a
    /// staging query never shows up under production.
    pub scope: &'a str,
    pub statement: &'a str,
    /// Unix seconds.
    pub ran_at: i64,
    pub source: RunSource,
    pub elapsed_ms: Option<u64>,
    pub row_count: Option<u64>,
    pub affected: Option<u64>,
    pub error: Option<&'a str>,
}

/// What the history screen asks for: the two filter chips, the window it
/// shows, and how many rows it is willing to paint.
#[derive(Debug, Clone, Copy)]
pub struct HistoryFilter {
    /// Hide the statements the app built for itself.
    pub user_only: bool,
    /// Show only the runs that failed.
    pub errors_only: bool,
    /// Oldest run to return, unix seconds. `None` reads the whole table.
    pub since: Option<i64>,
    pub limit: usize,
}

/// One tab as the file remembers it: enough to open it again, and
/// nothing more. A result set is never kept — a restored query tab shows
/// its statement and waits, because the app does not re-run a statement
/// behind the user.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SavedTab {
    /// The paged table view, on the page it was left on.
    Table {
        schema: String,
        table: String,
        page: usize,
    },
    /// A query tab: its statement, the name on the strip, the relation it
    /// was opened on when it came from the sidebar, and which way it
    /// commits. The mode is kept and the transaction is not: a restored tab
    /// has no session, so there is nothing open to come back to.
    Query {
        title: String,
        statement: String,
        relation: Option<(String, String)>,
        tx_mode: TxMode,
    },
    /// The query-history tab. It is a view of this file, so it carries
    /// no state worth keeping.
    History,
}

/// What one connection had open, in strip order, and which of those tabs
/// was in front.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SavedTabs {
    pub tabs: Vec<SavedTab>,
    pub active: usize,
}

impl Default for HistoryFilter {
    fn default() -> Self {
        Self {
            user_only: false,
            errors_only: false,
            since: None,
            limit: HISTORY_LIMIT,
        }
    }
}

impl Store {
    /// Open (and migrate) the app database at the default location.
    pub fn open_default() -> Result<Self> {
        let dir = data_dir()?;
        std::fs::create_dir_all(&dir)?;
        Self::open(dir.join("meerkat.sqlite"))
    }

    pub fn open(path: PathBuf) -> Result<Self> {
        let conn = Connection::open(&path)
            .with_context(|| format!("failed to open {}", path.display()))?;
        // **Every window opens its own handle on this one file**, so two
        // windows can be writing it at the same moment: a run lands in one
        // while the other saves its tab strip. WAL lets a reader and a
        // writer through together, and the busy timeout is what turns the
        // remaining overlap into a short wait instead of `SQLITE_BUSY` —
        // the default timeout is zero, which fails on the spot. Neither is
        // a tuning knob: without them a second window makes the first one
        // lose writes.
        conn.execute_batch(
            "PRAGMA journal_mode = WAL;
             PRAGMA busy_timeout = 2000;",
        )
        .context("failed to set the store's pragmas")?;
        conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS profiles (
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
            );",
        )?;
        let store = Self { conn };
        store.migrate()?;
        Ok(store)
    }

    /// Columns added after the first release. Each one is added on its own,
    /// so an older file keeps its rows.
    fn migrate(&self) -> Result<()> {
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
        self.add_columns(
            "profiles",
            &[
                ("last_opened", "INTEGER"),
                ("server", "TEXT"),
                ("env", "TEXT"),
                ("read_only", "INTEGER"),
            ],
        )?;
        // A query tab remembers which way it commits, so a tab switched to
        // manual comes back manual. NULL is auto.
        self.add_columns("open_tabs", &[("tx_mode", "TEXT")])?;
        // Rows changed, for the runs that answer with a count instead of a
        // result set. NULL on every row an older build wrote, which is the
        // same thing it means on a new one: this run reported no count.
        self.add_columns("query_history", &[("affected", "INTEGER")])?;
        Ok(())
    }

    fn add_columns(&self, table: &str, columns: &[(&str, &str)]) -> Result<()> {
        let existing: Vec<String> = {
            let mut stmt = self.conn.prepare(&format!("PRAGMA table_info({table})"))?;
            let rows = stmt.query_map([], |row| row.get::<_, String>(1))?;
            rows.collect::<std::result::Result<_, _>>()?
        };
        for (column, kind) in columns {
            if !existing.iter().any(|name| name == column) {
                self.conn
                    .execute_batch(&format!("ALTER TABLE {table} ADD COLUMN {column} {kind}"))?;
            }
        }
        Ok(())
    }

    /// Save a profile, keeping whatever the connections screen has already
    /// learned about it. A plain `INSERT OR REPLACE` would throw the
    /// counts and the last-opened time away on every edit.
    pub fn save_profile(&self, p: &Profile) -> Result<()> {
        self.conn.execute(
            "INSERT INTO profiles
                (id, name, engine, host, port, database, user, read_only)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
             ON CONFLICT(id) DO UPDATE SET
                name = excluded.name,
                engine = excluded.engine,
                host = excluded.host,
                port = excluded.port,
                database = excluded.database,
                user = excluded.user,
                read_only = excluded.read_only",
            rusqlite::params![
                p.id,
                p.name,
                engine_str(p.engine),
                p.host,
                p.port,
                p.database,
                p.user,
                p.read_only
            ],
        )?;
        Ok(())
    }

    pub fn list_profiles(&self) -> Result<Vec<Profile>> {
        Ok(self
            .list_connections()?
            .into_iter()
            .map(|c| c.profile)
            .collect())
    }

    /// Every saved connection, most recently opened first. A profile that
    /// has never been opened sorts after the ones that have, by name.
    pub fn list_connections(&self) -> Result<Vec<SavedConnection>> {
        let mut stmt = self.conn.prepare(
            "SELECT id, name, engine, host, port, database, user,
                    last_opened, server, env, read_only
             FROM profiles
             ORDER BY last_opened IS NULL, last_opened DESC, name",
        )?;
        let rows = stmt.query_map([], |row| {
            Ok(SavedConnection {
                profile: Profile {
                    id: row.get(0)?,
                    name: row.get(1)?,
                    engine: parse_engine(&row.get::<_, String>(2)?),
                    host: row.get(3)?,
                    port: row.get(4)?,
                    database: row.get(5)?,
                    user: row.get(6)?,
                    // A row an older build wrote has no answer here, and
                    // the careful reading of no answer is "do not write".
                    read_only: row.get::<_, Option<bool>>(10)?.unwrap_or(true),
                },
                last_opened: row.get(7)?,
                server: row.get(8)?,
                env: row.get(9)?,
            })
        })?;
        Ok(rows.collect::<std::result::Result<_, _>>()?)
    }

    /// A small on/off remembered across launches ("group by env"). The
    /// `ui_state` table is a key-value bag for exactly this: screen
    /// state too small to deserve a table, too annoying to lose.
    pub fn flag(&self, key: &str, default: bool) -> bool {
        self.conn
            .query_row("SELECT value FROM ui_state WHERE key = ?1", [key], |row| {
                row.get::<_, String>(0)
            })
            .optional()
            .ok()
            .flatten()
            .map(|value| value == "1")
            .unwrap_or(default)
    }

    pub fn set_flag(&self, key: &str, value: bool) -> Result<()> {
        self.conn.execute(
            "INSERT INTO ui_state (key, value) VALUES (?1, ?2)
             ON CONFLICT(key) DO UPDATE SET value = excluded.value",
            rusqlite::params![key, if value { "1" } else { "0" }],
        )?;
        Ok(())
    }

    /// The same bag, for a value that is not an on/off — a pane size the
    /// user dragged, remembered across launches. A missing key or an
    /// unreadable file both answer `None`: the caller has a default, and
    /// screen state is never worth an error.
    pub fn ui_value(&self, key: &str) -> Option<String> {
        self.conn
            .query_row("SELECT value FROM ui_state WHERE key = ?1", [key], |row| {
                row.get::<_, String>(0)
            })
            .optional()
            .ok()
            .flatten()
    }

    pub fn set_ui_value(&self, key: &str, value: &str) -> Result<()> {
        self.conn.execute(
            "INSERT INTO ui_state (key, value) VALUES (?1, ?2)
             ON CONFLICT(key) DO UPDATE SET value = excluded.value",
            rusqlite::params![key, value],
        )?;
        Ok(())
    }

    /// The environment tag of one profile, for the shell to read on its
    /// way in: the frame it wears is decided once, when the session
    /// opens. An unknown id reads as untagged.
    pub fn env_of(&self, id: &str) -> Result<Option<String>> {
        Ok(self
            .conn
            .query_row("SELECT env FROM profiles WHERE id = ?1", [id], |row| {
                row.get(0)
            })
            .optional()?
            .flatten())
    }

    /// Tag a connection with an environment ("prod", "staging"), or take
    /// the tag away with `None`. The tag is display metadata, so it is
    /// written beside the profile rather than through `save_profile`.
    pub fn set_env(&self, id: &str, env: Option<&str>) -> Result<()> {
        self.conn.execute(
            "UPDATE profiles SET env = ?2 WHERE id = ?1",
            rusqlite::params![id, env],
        )?;
        Ok(())
    }

    /// Remember that the user opened this connection, so it sorts to the
    /// top next time. `at` is unix seconds.
    pub fn mark_opened(&self, id: &str, at: i64) -> Result<()> {
        self.conn.execute(
            "UPDATE profiles SET last_opened = ?2 WHERE id = ?1",
            rusqlite::params![id, at],
        )?;
        Ok(())
    }

    /// Cache what a successful probe saw, so the next launch can paint the
    /// row without a probe of its own.
    pub fn record_probe(&self, id: &str, server: &str) -> Result<()> {
        self.conn.execute(
            "UPDATE profiles SET server = ?2 WHERE id = ?1",
            rusqlite::params![id, server],
        )?;
        Ok(())
    }

    pub fn delete_profile(&self, id: &str) -> Result<()> {
        self.conn
            .execute("DELETE FROM profiles WHERE id = ?1", [id])?;
        Ok(())
    }

    // --- catalog cache ----------------------------------------------------

    /// The catalog this connection had the last time it was open, as JSON.
    /// The shell paints the sidebar from it while the real introspection
    /// runs, so a large database opens at once instead of after a round
    /// trip. A row that no longer parses — the model changed under it —
    /// is dropped and reported as a miss, never as an error.
    pub fn cached_catalog(&self, scope: &str) -> Result<Option<Catalog>> {
        let json: Option<String> = self
            .conn
            .query_row(
                "SELECT catalog FROM catalog_cache WHERE scope = ?1",
                [scope],
                |row| row.get(0),
            )
            .optional()?;
        let Some(json) = json else { return Ok(None) };
        match serde_json::from_str(&json) {
            Ok(catalog) => Ok(Some(catalog)),
            Err(_) => {
                self.forget_catalog(scope)?;
                Ok(None)
            }
        }
    }

    /// Keep the catalog a successful introspection produced. One row per
    /// connection, overwritten every time, so the file holds the current
    /// shape and nothing older.
    pub fn cache_catalog(&self, scope: &str, catalog: &Catalog, at: i64) -> Result<()> {
        let json = serde_json::to_string(catalog)?;
        self.conn.execute(
            "INSERT INTO catalog_cache (scope, catalog, cached_at)
             VALUES (?1, ?2, ?3)
             ON CONFLICT(scope) DO UPDATE SET
                catalog = excluded.catalog,
                cached_at = excluded.cached_at",
            rusqlite::params![scope, json, at],
        )?;
        Ok(())
    }

    pub fn forget_catalog(&self, scope: &str) -> Result<()> {
        self.conn
            .execute("DELETE FROM catalog_cache WHERE scope = ?1", [scope])?;
        Ok(())
    }

    // --- open tabs --------------------------------------------------------

    /// The tabs this connection had open, in strip order. A row the model
    /// no longer understands is dropped rather than reported: a session
    /// must open even when the file holds something older.
    pub fn saved_tabs(&self, scope: &str) -> Result<SavedTabs> {
        let mut stmt = self.conn.prepare(
            "SELECT kind, title, schema, relation, page, statement, active, tx_mode
               FROM open_tabs
              WHERE scope = ?1
              ORDER BY position",
        )?;
        let rows = stmt.query_map([scope], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, Option<String>>(1)?,
                row.get::<_, Option<String>>(2)?,
                row.get::<_, Option<String>>(3)?,
                row.get::<_, Option<i64>>(4)?,
                row.get::<_, Option<String>>(5)?,
                row.get::<_, i64>(6)?,
                row.get::<_, Option<String>>(7)?,
            ))
        })?;

        let mut tabs = SavedTabs::default();
        for row in rows {
            let (kind, title, schema, relation, page, statement, active, tx_mode) = row?;
            let tab = match kind.as_str() {
                "table" => match (schema, relation) {
                    (Some(schema), Some(table)) => SavedTab::Table {
                        schema,
                        table,
                        page: page.unwrap_or(0).max(0) as usize,
                    },
                    _ => continue,
                },
                "query" => SavedTab::Query {
                    title: title.unwrap_or_default(),
                    statement: statement.unwrap_or_default(),
                    relation: schema.zip(relation),
                    tx_mode: TxMode::parse(tx_mode.as_deref()),
                },
                "history" => SavedTab::History,
                _ => continue,
            };
            if active != 0 {
                tabs.active = tabs.tabs.len();
            }
            tabs.tabs.push(tab);
        }
        Ok(tabs)
    }

    /// Replace what this connection had open. The whole set is written at
    /// once, in one transaction, because a half-written strip is worse
    /// than the strip it replaced.
    ///
    /// Above `TAB_LIMIT` the oldest tabs — the front of the strip — go,
    /// and `active` moves with what is left.
    pub fn save_tabs(&self, scope: &str, tabs: &[SavedTab], active: usize) -> Result<()> {
        let dropped = tabs.len().saturating_sub(TAB_LIMIT);
        let kept = &tabs[dropped..];
        let active = active.saturating_sub(dropped);

        let tx = self.conn.unchecked_transaction()?;
        tx.execute("DELETE FROM open_tabs WHERE scope = ?1", [scope])?;
        for (position, tab) in kept.iter().enumerate() {
            let (kind, title, schema, relation, page, statement, tx_mode) = match tab {
                SavedTab::Table {
                    schema,
                    table,
                    page,
                } => (
                    "table",
                    None,
                    Some(schema.as_str()),
                    Some(table.as_str()),
                    Some(*page as i64),
                    None,
                    None,
                ),
                SavedTab::Query {
                    title,
                    statement,
                    relation,
                    tx_mode,
                } => (
                    "query",
                    Some(title.as_str()),
                    relation.as_ref().map(|(schema, _)| schema.as_str()),
                    relation.as_ref().map(|(_, table)| table.as_str()),
                    None,
                    Some(statement.as_str()),
                    Some(tx_mode.as_str()),
                ),
                SavedTab::History => ("history", None, None, None, None, None, None),
            };
            tx.execute(
                "INSERT INTO open_tabs
                    (scope, position, kind, title, schema, relation, page, statement, active,
                     tx_mode)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)",
                rusqlite::params![
                    scope,
                    position as i64,
                    kind,
                    title,
                    schema,
                    relation,
                    page,
                    statement,
                    (position == active) as i64,
                    tx_mode,
                ],
            )?;
        }
        tx.commit()?;
        Ok(())
    }

    // --- query history ---------------------------------------------------

    /// Remember one run, then drop this connection's oldest runs above
    /// `HISTORY_LIMIT`. A failed run is remembered too: the run the user
    /// most wants to find again is usually the one that broke.
    pub fn record_query(&self, run: NewRun<'_>) -> Result<()> {
        self.conn.execute(
            "INSERT INTO query_history
                (scope, statement, ran_at, source, elapsed_ms, row_count, affected, error)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
            rusqlite::params![
                run.scope,
                run.statement,
                run.ran_at,
                source_str(run.source),
                run.elapsed_ms,
                run.row_count,
                run.affected,
                run.error,
            ],
        )?;
        self.prune_history(run.scope)?;
        Ok(())
    }

    fn prune_history(&self, scope: &str) -> Result<()> {
        self.conn.execute(
            "DELETE FROM query_history
              WHERE scope = ?1
                AND id NOT IN (
                    SELECT id FROM query_history
                     WHERE scope = ?1
                     ORDER BY ran_at DESC, id DESC
                     LIMIT ?2)",
            rusqlite::params![scope, HISTORY_LIMIT as i64],
        )?;
        Ok(())
    }

    /// One connection's runs, newest first. The filters are bound rather
    /// than spliced, so one prepared statement serves every chip
    /// combination.
    pub fn list_history(&self, scope: &str, filter: HistoryFilter) -> Result<Vec<QueryRun>> {
        let mut stmt = self.conn.prepare(
            "SELECT id, statement, ran_at, source, elapsed_ms, row_count, affected, error
               FROM query_history
              WHERE scope = ?1
                AND (?2 = 0 OR source = 'user')
                AND (?3 = 0 OR error IS NOT NULL)
                AND (?4 = 0 OR ran_at >= ?5)
              ORDER BY ran_at DESC, id DESC
              LIMIT ?6",
        )?;
        let rows = stmt.query_map(
            rusqlite::params![
                scope,
                filter.user_only as i64,
                filter.errors_only as i64,
                filter.since.is_some() as i64,
                filter.since.unwrap_or_default(),
                filter.limit as i64,
            ],
            |row| {
                Ok(QueryRun {
                    id: row.get(0)?,
                    statement: row.get(1)?,
                    ran_at: row.get(2)?,
                    source: parse_source(&row.get::<_, String>(3)?),
                    elapsed_ms: row.get(4)?,
                    row_count: row.get(5)?,
                    affected: row.get(6)?,
                    error: row.get(7)?,
                })
            },
        )?;
        Ok(rows.collect::<std::result::Result<_, _>>()?)
    }
}

fn source_str(source: RunSource) -> &'static str {
    match source {
        RunSource::User => "user",
        RunSource::App => "app",
    }
}

fn parse_source(s: &str) -> RunSource {
    match s {
        "user" => RunSource::User,
        _ => RunSource::App,
    }
}

fn engine_str(engine: Engine) -> &'static str {
    match engine {
        Engine::Sqlite => "sqlite",
        Engine::Postgres => "postgres",
        Engine::Mysql => "mysql",
    }
}

fn parse_engine(s: &str) -> Engine {
    match s {
        "postgres" => Engine::Postgres,
        "mysql" => Engine::Mysql,
        _ => Engine::Sqlite,
    }
}

fn data_dir() -> Result<PathBuf> {
    Ok(dirs::data_dir()
        .context("no user data directory")?
        .join("meerkat"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn profile_round_trip() {
        let dir = std::env::temp_dir().join("meerkat-store-test");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("test.sqlite");
        let _ = std::fs::remove_file(&path);

        let store = Store::open(path).unwrap();
        store
            .save_profile(&Profile {
                id: "p1".into(),
                name: "Local PG".into(),
                engine: Engine::Postgres,
                host: Some("localhost".into()),
                port: Some(5432),
                database: "app".into(),
                user: Some("nick".into()),
                read_only: true,
            })
            .unwrap();

        let profiles = store.list_profiles().unwrap();
        assert_eq!(profiles.len(), 1);
        assert_eq!(profiles[0].engine, Engine::Postgres);
        assert_eq!(profiles[0].port, Some(5432));
    }

    #[test]
    fn a_ui_value_round_trips_and_a_missing_key_is_none() {
        let dir = std::env::temp_dir().join("meerkat-store-test");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("ui-value.sqlite");
        let _ = std::fs::remove_file(&path);

        let store = Store::open(path).unwrap();
        assert_eq!(store.ui_value("sidebar_width"), None);
        store.set_ui_value("sidebar_width", "312").unwrap();
        assert_eq!(store.ui_value("sidebar_width"), Some("312".to_string()));
        // A second write replaces, never duplicates: the bag is one value
        // per key.
        store.set_ui_value("sidebar_width", "204").unwrap();
        assert_eq!(store.ui_value("sidebar_width"), Some("204".to_string()));
    }

    #[test]
    fn a_probe_survives_the_next_save() {
        let dir = std::env::temp_dir().join("meerkat-store-test");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("probe.sqlite");
        let _ = std::fs::remove_file(&path);

        let store = Store::open(path).unwrap();
        let mut profile = Profile {
            id: "p1".into(),
            name: "Local PG".into(),
            engine: Engine::Postgres,
            host: Some("localhost".into()),
            port: Some(5432),
            database: "app".into(),
            user: Some("nick".into()),
            read_only: true,
        };
        store.save_profile(&profile).unwrap();
        store.record_probe("p1", "PG 16.2").unwrap();
        store.mark_opened("p1", 1_700_000_000).unwrap();

        // Renaming the profile must not lose what the probe learned.
        profile.name = "Prod".into();
        store.save_profile(&profile).unwrap();

        let saved = store.list_connections().unwrap();
        assert_eq!(saved[0].profile.name, "Prod");
        assert_eq!(saved[0].server.as_deref(), Some("PG 16.2"));
        assert_eq!(saved[0].last_opened, Some(1_700_000_000));
    }

    #[test]
    fn an_env_tag_survives_the_next_save_and_can_be_taken_away() {
        let store = store_at("env.sqlite");
        let profile = Profile {
            id: "p1".into(),
            name: "Local PG".into(),
            engine: Engine::Postgres,
            host: Some("localhost".into()),
            port: Some(5432),
            database: "app".into(),
            user: Some("nick".into()),
            read_only: true,
        };
        store.save_profile(&profile).unwrap();
        store.set_env("p1", Some("prod")).unwrap();

        // An edit writes the profile again; the tag must stay.
        store.save_profile(&profile).unwrap();
        assert_eq!(
            store.list_connections().unwrap()[0].env.as_deref(),
            Some("prod")
        );

        store.set_env("p1", None).unwrap();
        assert_eq!(store.list_connections().unwrap()[0].env, None);
    }

    #[test]
    fn the_read_only_flag_round_trips_and_an_older_row_reads_as_read_only() {
        let store = store_at("read_only.sqlite");
        let mut profile = Profile {
            id: "p1".into(),
            name: "Local PG".into(),
            engine: Engine::Postgres,
            host: Some("localhost".into()),
            port: Some(5432),
            database: "app".into(),
            user: Some("nick".into()),
            read_only: false,
        };
        store.save_profile(&profile).unwrap();
        assert!(!store.list_connections().unwrap()[0].profile.read_only);

        // An edit writes the flag as well, both ways round.
        profile.read_only = true;
        store.save_profile(&profile).unwrap();
        assert!(store.list_connections().unwrap()[0].profile.read_only);

        // A row an older build wrote has no answer in the column. The
        // careful reading is the one that wins.
        store
            .conn
            .execute("UPDATE profiles SET read_only = NULL", [])
            .unwrap();
        assert!(store.list_connections().unwrap()[0].profile.read_only);
    }

    fn store_at(name: &str) -> Store {
        let dir = std::env::temp_dir().join("meerkat-store-test");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join(name);
        let _ = std::fs::remove_file(&path);
        Store::open(path).unwrap()
    }

    fn run<'a>(scope: &'a str, statement: &'a str, ran_at: i64, source: RunSource) -> NewRun<'a> {
        NewRun {
            scope,
            statement,
            ran_at,
            source,
            elapsed_ms: Some(12),
            row_count: Some(3),
            affected: None,
            error: None,
        }
    }

    #[test]
    fn history_reads_back_newest_first_and_per_connection() {
        let store = store_at("history.sqlite");
        store
            .record_query(run("prod", "select 1", 100, RunSource::User))
            .unwrap();
        store
            .record_query(run("prod", "select 2", 200, RunSource::User))
            .unwrap();
        store
            .record_query(run("staging", "select 3", 300, RunSource::User))
            .unwrap();

        let seen: Vec<String> = store
            .list_history("prod", HistoryFilter::default())
            .unwrap()
            .into_iter()
            .map(|run| run.statement)
            .collect();
        // A staging query never shows up under production.
        assert_eq!(seen, ["select 2", "select 1"]);
    }

    /// Rows changed ride their own column, so a run that returned nothing
    /// and changed three rows reads back as exactly that — and a run that
    /// returned rows reads back with no count at all, rather than a zero
    /// that would say it changed nothing when it was never asked to.
    #[test]
    fn the_history_keeps_rows_changed_apart_from_rows_returned() {
        let store = store_at("history-affected.sqlite");
        store
            .record_query(NewRun {
                row_count: Some(0),
                affected: Some(3),
                ..run("prod", "update t set a = 1", 100, RunSource::User)
            })
            .unwrap();
        store
            .record_query(run("prod", "select 1", 90, RunSource::User))
            .unwrap();

        let seen = store
            .list_history("prod", HistoryFilter::default())
            .unwrap();
        assert_eq!(seen[0].affected, Some(3));
        assert_eq!(seen[0].row_count, Some(0));
        assert_eq!(seen[1].affected, None);
    }

    #[test]
    fn the_filters_narrow_the_history() {
        let store = store_at("history-filters.sqlite");
        store
            .record_query(run("prod", "select 1", 100, RunSource::User))
            .unwrap();
        store
            .record_query(run("prod", "select * from users", 150, RunSource::App))
            .unwrap();
        store
            .record_query(NewRun {
                elapsed_ms: None,
                row_count: None,
                error: Some("relation \"user_setings\" does not exist"),
                ..run("prod", "select * from user_setings", 200, RunSource::User)
            })
            .unwrap();

        let user_only = HistoryFilter {
            user_only: true,
            ..Default::default()
        };
        assert_eq!(store.list_history("prod", user_only).unwrap().len(), 2);

        let errors_only = HistoryFilter {
            errors_only: true,
            ..Default::default()
        };
        let failed = store.list_history("prod", errors_only).unwrap();
        assert_eq!(failed.len(), 1);
        assert!(failed[0].error.is_some());
        assert_eq!(failed[0].elapsed_ms, None);

        let recent = HistoryFilter {
            since: Some(150),
            ..Default::default()
        };
        assert_eq!(store.list_history("prod", recent).unwrap().len(), 2);
    }

    #[test]
    fn the_history_stops_growing() {
        let store = store_at("history-prune.sqlite");
        for ix in 0..(HISTORY_LIMIT as i64 + 20) {
            store
                .record_query(run("prod", &format!("select {ix}"), ix, RunSource::User))
                .unwrap();
        }
        let kept = store
            .list_history("prod", HistoryFilter::default())
            .unwrap();
        assert_eq!(kept.len(), HISTORY_LIMIT);
        // The oldest runs are the ones that go.
        assert_eq!(kept[0].statement, format!("select {}", HISTORY_LIMIT + 19));
        assert_eq!(kept[HISTORY_LIMIT - 1].statement, format!("select {}", 20));
    }

    #[test]
    fn the_catalog_cache_is_per_connection_and_overwritten() {
        let store = store_at("catalog.sqlite");
        let catalog = |table: &str| Catalog {
            schemas: vec![introspect::Schema {
                name: "public".into(),
                tables: vec![introspect::Table {
                    name: table.into(),
                    kind: introspect::TableKind::Table,
                    columns: Vec::new(),
                    primary_key: vec!["id".into()],
                    approx_rows: Some(12),
                }],
            }],
        };

        assert!(store.cached_catalog("prod").unwrap().is_none());
        store.cache_catalog("prod", &catalog("users"), 100).unwrap();
        store
            .cache_catalog("staging", &catalog("orders"), 100)
            .unwrap();
        // The second read of the same connection replaces the first.
        store
            .cache_catalog("prod", &catalog("people"), 200)
            .unwrap();

        let prod = store.cached_catalog("prod").unwrap().unwrap();
        assert_eq!(prod.schemas[0].tables[0].name, "people");
        assert_eq!(prod.schemas[0].tables[0].primary_key, ["id"]);
        let staging = store.cached_catalog("staging").unwrap().unwrap();
        assert_eq!(staging.schemas[0].tables[0].name, "orders");
    }

    #[test]
    fn a_catalog_the_model_no_longer_parses_is_a_miss() {
        let store = store_at("catalog-stale.sqlite");
        store
            .conn
            .execute(
                "INSERT INTO catalog_cache (scope, catalog, cached_at) VALUES (?1, ?2, ?3)",
                rusqlite::params!["prod", "{\"schemas\":\"whatever\"}", 100],
            )
            .unwrap();

        assert!(store.cached_catalog("prod").unwrap().is_none());
        // The row that cannot be read is dropped rather than re-read.
        let left: i64 = store
            .conn
            .query_row("SELECT count(*) FROM catalog_cache", [], |row| row.get(0))
            .unwrap();
        assert_eq!(left, 0);
    }

    #[test]
    fn the_open_tabs_come_back_in_strip_order() {
        let store = store_at("tabs.sqlite");
        let tabs = vec![
            SavedTab::Query {
                title: "query 1".into(),
                statement: "select 1".into(),
                relation: None,
                tx_mode: TxMode::Auto,
            },
            SavedTab::Query {
                title: "public.users".into(),
                statement: "select * from \"public\".\"users\" limit 500;".into(),
                relation: Some(("public".into(), "users".into())),
                // A tab that holds its own transactions comes back holding
                // them, so the mode rides in the row beside the statement.
                tx_mode: TxMode::Manual,
            },
            SavedTab::Table {
                schema: "public".into(),
                table: "orders".into(),
                page: 3,
            },
            SavedTab::History,
        ];
        store.save_tabs("prod", &tabs, 2).unwrap();
        store.save_tabs("staging", &[SavedTab::History], 0).unwrap();

        let read = store.saved_tabs("prod").unwrap();
        assert_eq!(read.tabs, tabs);
        assert_eq!(read.active, 2);
        // One connection's tabs never show up under another.
        assert_eq!(
            store.saved_tabs("staging").unwrap().tabs,
            [SavedTab::History]
        );
    }

    #[test]
    fn saving_the_tabs_replaces_the_last_set() {
        let store = store_at("tabs-replace.sqlite");
        let query = |sql: &str| SavedTab::Query {
            title: "query".into(),
            statement: sql.into(),
            relation: None,
            tx_mode: TxMode::Auto,
        };
        store
            .save_tabs("prod", &[query("select 1"), query("select 2")], 1)
            .unwrap();
        store.save_tabs("prod", &[query("select 3")], 0).unwrap();

        let read = store.saved_tabs("prod").unwrap();
        assert_eq!(read.tabs, [query("select 3")]);
        assert_eq!(read.active, 0);
    }

    #[test]
    fn only_the_newest_tabs_are_kept() {
        let store = store_at("tabs-limit.sqlite");
        let tabs: Vec<SavedTab> = (0..TAB_LIMIT + 10)
            .map(|ix| SavedTab::Query {
                title: format!("query {ix}"),
                statement: format!("select {ix}"),
                relation: None,
                tx_mode: TxMode::Auto,
            })
            .collect();
        store.save_tabs("prod", &tabs, tabs.len() - 1).unwrap();

        let read = store.saved_tabs("prod").unwrap();
        assert_eq!(read.tabs.len(), TAB_LIMIT);
        // The front of the strip goes, and the selection moves with what
        // is left.
        assert_eq!(read.tabs[0], tabs[10]);
        assert_eq!(read.active, TAB_LIMIT - 1);
    }

    #[test]
    fn a_tab_the_model_no_longer_understands_is_dropped() {
        let store = store_at("tabs-stale.sqlite");
        store
            .conn
            .execute(
                "INSERT INTO open_tabs (scope, position, kind, active) VALUES (?1, 0, 'diagram', 1)",
                ["prod"],
            )
            .unwrap();
        store
            .conn
            .execute(
                "INSERT INTO open_tabs (scope, position, kind, active) VALUES (?1, 1, 'history', 0)",
                ["prod"],
            )
            .unwrap();

        let read = store.saved_tabs("prod").unwrap();
        assert_eq!(read.tabs, [SavedTab::History]);
    }

    #[test]
    fn the_most_recently_opened_connection_comes_first() {
        let dir = std::env::temp_dir().join("meerkat-store-test");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("order.sqlite");
        let _ = std::fs::remove_file(&path);

        let store = Store::open(path).unwrap();
        for (id, name) in [("a", "alpha"), ("b", "beta"), ("c", "gamma")] {
            store
                .save_profile(&Profile {
                    id: id.into(),
                    name: name.into(),
                    engine: Engine::Postgres,
                    host: Some("localhost".into()),
                    port: Some(5432),
                    database: "app".into(),
                    user: None,
                    read_only: true,
                })
                .unwrap();
        }
        store.mark_opened("b", 100).unwrap();
        store.mark_opened("c", 200).unwrap();

        let order: Vec<String> = store
            .list_connections()
            .unwrap()
            .into_iter()
            .map(|c| c.profile.id)
            .collect();
        // Opened ones first, newest first; never opened last, by name.
        assert_eq!(order, ["c", "b", "a"]);
    }
}
