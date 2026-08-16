//! Local app state: connection profiles, window layout, query history.
//! Stored in a SQLite file under the user data directory (Zed `db` pattern).

use anyhow::{Context as _, Result};
use db_client::{Engine, Profile};
use rusqlite::Connection;
use std::path::PathBuf;

pub struct Store {
    conn: Connection,
}

/// How many runs one connection keeps. The history is a convenience, not
/// an audit log, so the file stays small: every insert drops the oldest
/// rows above this count.
const HISTORY_LIMIT: usize = 500;

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

impl Default for HistoryFilter {
    fn default() -> Self {
        Self { user_only: false, errors_only: false, since: None, limit: HISTORY_LIMIT }
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
                error TEXT
            );
            CREATE INDEX IF NOT EXISTS query_history_scope_time
                ON query_history (scope, ran_at DESC);",
        )?;
        let store = Self { conn };
        store.migrate()?;
        Ok(store)
    }

    /// Columns added after the first release. Each one is added on its own,
    /// so an older profiles file keeps its rows.
    fn migrate(&self) -> Result<()> {
        let existing: Vec<String> = {
            let mut stmt = self.conn.prepare("PRAGMA table_info(profiles)")?;
            let rows = stmt.query_map([], |row| row.get::<_, String>(1))?;
            rows.collect::<std::result::Result<_, _>>()?
        };
        // `tables` and `views` were dropped from the screen; an older file
        // keeps those columns, and nothing reads them.
        for (column, kind) in [("last_opened", "INTEGER"), ("server", "TEXT")] {
            if !existing.iter().any(|name| name == column) {
                self.conn
                    .execute_batch(&format!("ALTER TABLE profiles ADD COLUMN {column} {kind}"))?;
            }
        }
        Ok(())
    }

    /// Save a profile, keeping whatever the connections screen has already
    /// learned about it. A plain `INSERT OR REPLACE` would throw the
    /// counts and the last-opened time away on every edit.
    pub fn save_profile(&self, p: &Profile) -> Result<()> {
        self.conn.execute(
            "INSERT INTO profiles (id, name, engine, host, port, database, user)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
             ON CONFLICT(id) DO UPDATE SET
                name = excluded.name,
                engine = excluded.engine,
                host = excluded.host,
                port = excluded.port,
                database = excluded.database,
                user = excluded.user",
            rusqlite::params![
                p.id,
                p.name,
                engine_str(p.engine),
                p.host,
                p.port,
                p.database,
                p.user
            ],
        )?;
        Ok(())
    }

    pub fn list_profiles(&self) -> Result<Vec<Profile>> {
        Ok(self.list_connections()?.into_iter().map(|c| c.profile).collect())
    }

    /// Every saved connection, most recently opened first. A profile that
    /// has never been opened sorts after the ones that have, by name.
    pub fn list_connections(&self) -> Result<Vec<SavedConnection>> {
        let mut stmt = self.conn.prepare(
            "SELECT id, name, engine, host, port, database, user,
                    last_opened, server
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
                },
                last_opened: row.get(7)?,
                server: row.get(8)?,
            })
        })?;
        Ok(rows.collect::<std::result::Result<_, _>>()?)
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
        self.conn.execute("DELETE FROM profiles WHERE id = ?1", [id])?;
        Ok(())
    }

    // --- query history ---------------------------------------------------

    /// Remember one run, then drop this connection's oldest runs above
    /// `HISTORY_LIMIT`. A failed run is remembered too: the run the user
    /// most wants to find again is usually the one that broke.
    pub fn record_query(&self, run: NewRun<'_>) -> Result<()> {
        self.conn.execute(
            "INSERT INTO query_history
                (scope, statement, ran_at, source, elapsed_ms, row_count, error)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            rusqlite::params![
                run.scope,
                run.statement,
                run.ran_at,
                source_str(run.source),
                run.elapsed_ms,
                run.row_count,
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
            "SELECT id, statement, ran_at, source, elapsed_ms, row_count, error
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
                    error: row.get(6)?,
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
            })
            .unwrap();

        let profiles = store.list_profiles().unwrap();
        assert_eq!(profiles.len(), 1);
        assert_eq!(profiles[0].engine, Engine::Postgres);
        assert_eq!(profiles[0].port, Some(5432));
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
            error: None,
        }
    }

    #[test]
    fn history_reads_back_newest_first_and_per_connection() {
        let store = store_at("history.sqlite");
        store.record_query(run("prod", "select 1", 100, RunSource::User)).unwrap();
        store.record_query(run("prod", "select 2", 200, RunSource::User)).unwrap();
        store.record_query(run("staging", "select 3", 300, RunSource::User)).unwrap();

        let seen: Vec<String> = store
            .list_history("prod", HistoryFilter::default())
            .unwrap()
            .into_iter()
            .map(|run| run.statement)
            .collect();
        // A staging query never shows up under production.
        assert_eq!(seen, ["select 2", "select 1"]);
    }

    #[test]
    fn the_filters_narrow_the_history() {
        let store = store_at("history-filters.sqlite");
        store.record_query(run("prod", "select 1", 100, RunSource::User)).unwrap();
        store.record_query(run("prod", "select * from users", 150, RunSource::App)).unwrap();
        store
            .record_query(NewRun {
                elapsed_ms: None,
                row_count: None,
                error: Some("relation \"user_setings\" does not exist"),
                ..run("prod", "select * from user_setings", 200, RunSource::User)
            })
            .unwrap();

        let user_only = HistoryFilter { user_only: true, ..Default::default() };
        assert_eq!(store.list_history("prod", user_only).unwrap().len(), 2);

        let errors_only = HistoryFilter { errors_only: true, ..Default::default() };
        let failed = store.list_history("prod", errors_only).unwrap();
        assert_eq!(failed.len(), 1);
        assert!(failed[0].error.is_some());
        assert_eq!(failed[0].elapsed_ms, None);

        let recent = HistoryFilter { since: Some(150), ..Default::default() };
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
        let kept = store.list_history("prod", HistoryFilter::default()).unwrap();
        assert_eq!(kept.len(), HISTORY_LIMIT);
        // The oldest runs are the ones that go.
        assert_eq!(kept[0].statement, format!("select {}", HISTORY_LIMIT + 19));
        assert_eq!(kept[HISTORY_LIMIT - 1].statement, format!("select {}", 20));
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
