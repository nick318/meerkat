//! Local app state: connection profiles, window layout, query history.
//! Stored in a SQLite file under the user data directory (Zed `db` pattern).

use anyhow::{Context as _, Result};
use db_client::{Engine, Profile};
use rusqlite::Connection;
use std::path::PathBuf;

pub struct Store {
    conn: Connection,
}

/// A saved profile plus what the connections screen remembers about it:
/// when it was last opened, and what the last successful probe saw. The
/// counts are cached so the screen has something to show before it has
/// reached the server.
#[derive(Debug, Clone)]
pub struct SavedConnection {
    pub profile: Profile,
    /// Unix seconds.
    pub last_opened: Option<i64>,
    pub tables: Option<u32>,
    pub views: Option<u32>,
    /// Server description, as the driver reports it ("PG 16.2").
    pub server: Option<String>,
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
            );",
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
        for (column, kind) in [
            ("last_opened", "INTEGER"),
            ("tables", "INTEGER"),
            ("views", "INTEGER"),
            ("server", "TEXT"),
        ] {
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
                    last_opened, tables, views, server
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
                tables: row.get(8)?,
                views: row.get(9)?,
                server: row.get(10)?,
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
    /// row before the server answers.
    pub fn record_probe(
        &self,
        id: &str,
        tables: u32,
        views: u32,
        server: &str,
    ) -> Result<()> {
        self.conn.execute(
            "UPDATE profiles SET tables = ?2, views = ?3, server = ?4 WHERE id = ?1",
            rusqlite::params![id, tables, views, server],
        )?;
        Ok(())
    }

    pub fn delete_profile(&self, id: &str) -> Result<()> {
        self.conn.execute("DELETE FROM profiles WHERE id = ?1", [id])?;
        Ok(())
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
        store.record_probe("p1", 14, 3, "PG 16.2").unwrap();
        store.mark_opened("p1", 1_700_000_000).unwrap();

        // Renaming the profile must not lose what the probe learned.
        profile.name = "Prod".into();
        store.save_profile(&profile).unwrap();

        let saved = store.list_connections().unwrap();
        assert_eq!(saved[0].profile.name, "Prod");
        assert_eq!(saved[0].tables, Some(14));
        assert_eq!(saved[0].views, Some(3));
        assert_eq!(saved[0].server.as_deref(), Some("PG 16.2"));
        assert_eq!(saved[0].last_opened, Some(1_700_000_000));
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
