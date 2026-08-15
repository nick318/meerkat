//! Driver-agnostic database access.
//!
//! Every engine (SQLite, PostgreSQL, ...) implements [`Connection`].
//! The rest of the app talks only to this trait, so the UI never knows
//! which engine is behind a tab.

use async_trait::async_trait;
use introspect::Catalog;
use serde::{Deserialize, Serialize};

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

/// A live connection to one database.
#[async_trait]
pub trait Connection: Send + Sync {
    /// Read the full schema model for the schema tree.
    async fn introspect(&self) -> Result<Catalog>;

    /// A short description of the server, as the connections screen shows
    /// it next to the connection: "PG 16.2", "SQLite 3.45".
    async fn server_version(&self) -> Result<String>;

    /// Run one SQL statement and collect its result.
    /// TODO: switch to a row stream with a fetch cap and cancellation.
    async fn execute(&self, sql: &str) -> Result<QueryResult>;

    /// Apply a changeset in one transaction. Returns rows affected.
    /// Must roll back and report a conflict when an `Update` matches 0 rows.
    async fn apply(&self, changes: &[RowChange]) -> Result<u64>;
}
