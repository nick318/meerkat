//! Driver-agnostic database access.
//!
//! Every engine (SQLite, PostgreSQL, ...) implements [`Connection`].
//! The rest of the app talks only to this trait, so the UI never knows
//! which engine is behind a tab.

use async_trait::async_trait;
use introspect::Catalog;
use serde::{Deserialize, Serialize};
use std::sync::Arc;

pub type Result<T> = anyhow::Result<T>;

/// A saved connection profile. The password is NOT stored here — it lives
/// in the OS keychain, keyed by `id` (see the `secrets` crate).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Profile {
    pub id: String,
    pub name: String,
    pub engine: Engine,
    pub host: Option<String>,
    pub port: Option<u16>,
    pub database: String,
    pub user: Option<String>,
    /// Whether the session this profile opens refuses to write. It is a
    /// connection parameter, not a label: the driver asks the server for a
    /// read-only session, so the server is what enforces it. A profile
    /// this build reads without the flag is read-only, because the safe
    /// reading of a missing answer is the careful one.
    #[serde(default = "read_only_default")]
    pub read_only: bool,
}

fn read_only_default() -> bool {
    true
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Engine {
    Sqlite,
    Postgres,
    Mysql,
}

/// One cell value, decoded into a small closed set the grid can render.
#[derive(Debug, Clone, PartialEq)]
pub enum Value {
    Null,
    Bool(bool),
    Int(i64),
    Float(f64),
    Text(String),
    Bytes(Vec<u8>),
}

impl Value {
    /// Display form for the results grid.
    pub fn display(&self) -> String {
        match self {
            Value::Null => "NULL".to_string(),
            Value::Bool(b) => b.to_string(),
            Value::Int(i) => i.to_string(),
            Value::Float(f) => f.to_string(),
            Value::Text(s) => s.clone(),
            Value::Bytes(b) => format!("0x{}", hex_prefix(b, 16)),
        }
    }
}

fn hex_prefix(bytes: &[u8], max: usize) -> String {
    let mut out = String::new();
    for b in bytes.iter().take(max) {
        out.push_str(&format!("{b:02x}"));
    }
    if bytes.len() > max {
        out.push('…');
    }
    out
}

#[derive(Debug, Clone, Default)]
pub struct QueryResult {
    pub columns: Vec<String>,
    pub rows: Vec<Vec<Value>>,
    /// Rows affected, for statements that return no rows.
    pub rows_affected: u64,
}

/// One pending edit against a table. Commit generates parameterized
/// `UPDATE`/`INSERT`/`DELETE` statements and runs them in one transaction.
#[derive(Debug, Clone)]
pub enum RowChange {
    Update {
        table: String,
        /// Primary-key columns and their CURRENT values, used in `WHERE`.
        key: Vec<(String, Value)>,
        /// Edited columns: (column, old value, new value). Old values also
        /// go into `WHERE` for optimistic concurrency.
        set: Vec<(String, Value, Value)>,
    },
    Insert {
        table: String,
        values: Vec<(String, Value)>,
    },
    Delete {
        table: String,
        key: Vec<(String, Value)>,
    },
}

/// A statement the server is running, as the server itself names it.
/// PostgreSQL uses the backend process id; an engine with no way to reach
/// into a running statement never hands one out.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RunId(pub i32);

/// Called with a run's id the moment the driver knows it, which is before
/// the statement finishes — that is the whole point. The caller keeps it to
/// stop the run later. It takes `&RunId` by value and may be called once
/// per statement of a multi-statement run, so it is `Fn`, not `FnOnce`.
pub type ReportRun = Arc<dyn Fn(RunId) + Send + Sync>;

/// How firmly to stop a run.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Stop {
    /// Ask the server to abandon the statement. The connection lives, and
    /// the statement comes back as an error.
    Cancel,
    /// Close the whole backend. The connection dies with it, which is why
    /// it is the second press and never the first.
    Terminate,
}

/// A live connection to one database.
#[async_trait]
pub trait Connection: Send + Sync {
    /// Read the full schema model for the schema tree.
    async fn introspect(&self) -> Result<Catalog>;

    /// A short description of the server, as the connections screen shows
    /// it next to the connection: "PG 16.2", "SQLite 3.45".
    async fn server_version(&self) -> Result<String>;

    /// Run one SQL statement and collect its result.
    /// TODO: switch to a row stream with a fetch cap.
    async fn execute(&self, sql: &str) -> Result<QueryResult>;

    /// The same, saying which run it is as soon as the server has told the
    /// driver, so the caller can stop it while it is still going. An engine
    /// that cannot be interrupted keeps the default and reports nothing.
    async fn execute_reporting(&self, sql: &str, _report: ReportRun) -> Result<QueryResult> {
        self.execute(sql).await
    }

    /// Stop a run this connection started. `Ok(false)` means the server had
    /// nothing to stop — the statement finished on its own first, which is
    /// a race, not an error. An engine with no cancellation always answers
    /// `Ok(false)`.
    async fn stop(&self, _run: RunId, _how: Stop) -> Result<bool> {
        Ok(false)
    }

    /// Apply a changeset in one transaction. Returns rows affected.
    /// Must roll back and report a conflict when an `Update` matches 0 rows.
    async fn apply(&self, changes: &[RowChange]) -> Result<u64>;
}
