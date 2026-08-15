//! Local app state: connection profiles, window layout, query history.
//! Stored in a SQLite file under the user data directory (Zed `db` pattern).

use anyhow::{Context as _, Result};
use db_client::{Engine, Profile};
use rusqlite::Connection;
use std::path::PathBuf;

pub struct Store {
    conn: Connection,
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
        Ok(Self { conn })
    }

    pub fn save_profile(&self, p: &Profile) -> Result<()> {
        self.conn.execute(
            "INSERT OR REPLACE INTO profiles (id, name, engine, host, port, database, user)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
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
        let mut stmt = self.conn.prepare(
            "SELECT id, name, engine, host, port, database, user FROM profiles ORDER BY name",
        )?;
        let rows = stmt.query_map([], |row| {
            Ok(Profile {
                id: row.get(0)?,
                name: row.get(1)?,
                engine: parse_engine(&row.get::<_, String>(2)?),
                host: row.get(3)?,
                port: row.get(4)?,
                database: row.get(5)?,
                user: row.get(6)?,
            })
        })?;
        Ok(rows.collect::<std::result::Result<_, _>>()?)
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
}
