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
    /// Roughly what this value costs the process, for [`Limits::max_bytes`].
    /// The discriminant plus whatever the value put on the heap; the `Vec`
    /// holding the row is not counted. It is a budget, not an audit.
    pub fn size_bytes(&self) -> usize {
        std::mem::size_of::<Value>()
            + match self {
                Value::Text(s) => s.capacity(),
                Value::Bytes(b) => b.capacity(),
                _ => 0,
            }
    }

    /// Cut a value down to `max` bytes, for [`Limits::max_cell_bytes`].
    ///
    /// A cut `Text` ends in `…`, so a truncated cell says so rather than
    /// quietly reading as the whole value. `Bytes` needs no marker:
    /// [`Value::display`] already renders a prefix and an ellipsis. The cut
    /// lands on a character boundary, or the `String` would not be UTF-8.
    pub fn cap(self, max: usize) -> Self {
        match self {
            Value::Text(mut s) if s.len() > max => {
                let mut end = max;
                while !s.is_char_boundary(end) {
                    end -= 1;
                }
                s.truncate(end);
                s.push('…');
                Value::Text(s)
            }
            Value::Bytes(mut b) if b.len() > max => {
                b.truncate(max);
                Value::Bytes(b)
            }
            other => other,
        }
    }

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
    /// Set when the memory cap stopped the read before the server ran out
    /// of rows. `false` means the result is the whole result.
    pub truncated: bool,
}

/// How much of a result the app will hold.
///
/// The cap is on **memory, not rows**. A row count is only a proxy for what
/// actually runs the process out of room, and a poor one: ten thousand
/// two-column rows cost about a megabyte, and ten thousand rows of wide
/// `jsonb` cost gigabytes. The same number cannot bound both, so the bound
/// is the resource itself.
///
/// A typed statement goes to the server verbatim — nothing rewrites it to
/// add a `LIMIT`, because a rewrite breaks on CTEs, `UNION` and statements
/// that are not `SELECT`, and it would put SQL in the history that the user
/// never wrote. So the bound is on what comes *back*.
#[derive(Debug, Clone, Copy)]
pub struct Limits {
    /// Decoded bytes kept in one result.
    ///
    /// Sized so that a real working set fits without ever meeting the cap:
    /// a row of a dozen ordinary columns costs roughly 500 bytes here (a
    /// `Value` is 32 bytes whatever it holds, plus the row's own `Vec` and
    /// whatever the strings put on the heap), so 120,000 such rows come to
    /// about 60 MB — a quarter of the budget. What the cap is for is the
    /// other shape: a `SELECT *` over a table of wide `jsonb` or `bytea`,
    /// where a few thousand rows would take the process down.
    pub max_bytes: usize,
    /// Bytes kept of any one value. One wide `bytea` or `jsonb` column must
    /// not eat the whole budget, and a cell larger than this cannot be read
    /// in a grid anyway.
    pub max_cell_bytes: usize,
}

pub const MAX_BYTES: usize = 256 * 1024 * 1024;
pub const MAX_CELL_BYTES: usize = 1024 * 1024;

impl Default for Limits {
    fn default() -> Self {
        Self { max_bytes: MAX_BYTES, max_cell_bytes: MAX_CELL_BYTES }
    }
}

/// Collects a result as the driver streams it, and says when to stop.
///
/// The caps live here rather than in each driver so that both engines
/// answer to one policy, and so the rule can be tested without a server.
///
/// The rule: a row that would carry the result *past* the cap is dropped,
/// and the result is marked truncated. A result that ends exactly on the
/// cap is therefore whole, not truncated. The first row is always kept, or
/// a single huge row would come back as an empty grid.
pub struct RowSink {
    limits: Limits,
    columns: Vec<String>,
    rows: Vec<Vec<Value>>,
    bytes: usize,
    truncated: bool,
}

impl RowSink {
    pub fn new(limits: Limits) -> Self {
        Self { limits, columns: Vec::new(), rows: Vec::new(), bytes: 0, truncated: false }
    }

    /// Name the columns, which the driver reads off the first row it gets.
    pub fn columns(&mut self, columns: Vec<String>) {
        self.columns = columns;
    }

    pub fn has_columns(&self) -> bool {
        !self.columns.is_empty()
    }

    /// Take one decoded row. `false` means the budget is spent and the
    /// driver must stop reading — the row was not kept.
    pub fn push(&mut self, values: Vec<Value>) -> bool {
        let values: Vec<Value> = values
            .into_iter()
            .map(|value| value.cap(self.limits.max_cell_bytes))
            .collect();
        // The row's own `Vec` counts as well. It is 24 bytes against a
        // narrow row's 32-per-value, so leaving it out would undercount a
        // two-column result by a third — and a narrow result is exactly the
        // shape that reaches a large row count.
        let size: usize =
            std::mem::size_of::<Vec<Value>>() + values.iter().map(Value::size_bytes).sum::<usize>();

        if !self.rows.is_empty() && self.bytes + size > self.limits.max_bytes {
            self.truncated = true;
            return false;
        }

        self.bytes += size;
        self.rows.push(values);
        true
    }

    /// What the result holds so far, by the same reckoning as the cap.
    pub fn bytes(&self) -> usize {
        self.bytes
    }

    pub fn truncated(&self) -> bool {
        self.truncated
    }

    pub fn finish(self) -> QueryResult {
        QueryResult {
            columns: self.columns,
            rows: self.rows,
            rows_affected: 0,
            truncated: self.truncated,
        }
    }
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

/// One tab's connection to the server, held for as long as the tab is.
///
/// **The tab is a session in the user's head, so it has to be one on the
/// wire.** Statements sent on whichever pooled connection happened to be
/// free cannot see each other's `SET`, cannot share a `BEGIN`, and cannot
/// read a temp table the statement before it made. Nothing short of one
/// connection per tab fixes that: replaying the settings would mean
/// parsing the user's SQL, and no amount of parsing replays a temp table
/// or an open transaction.
///
/// A session is one connection, so it runs one statement at a time. That
/// is already what a tab does — ⌘⏎ while a run is out does nothing.
#[async_trait]
pub trait Session: Send + Sync {
    /// The backend this session is on, known from the moment it opens
    /// rather than one round trip into each run. An engine with nothing to
    /// cancel reports `None`.
    fn backend(&self) -> Option<RunId>;

    /// Run one statement on this session's own connection.
    async fn execute(&self, sql: &str) -> Result<QueryResult>;

    /// Whether this connection is sitting inside a transaction the user
    /// began — the one thing a close cannot take back for them.
    ///
    /// The answer is worth **caching against the last run**, and the cache
    /// is exact rather than approximate: the connection is pinned to one
    /// tab, so nothing but that tab's own statements can change what it is
    /// in. Ask once after each run and the answer holds until the next.
    async fn in_transaction(&self) -> Result<bool> {
        Ok(false)
    }

    /// Roll back and hand the connection back. `execute` fails afterwards.
    ///
    /// The rollback is not about the *server*: closing a socket already
    /// rolls a transaction back, at once, with no timeout involved. It is
    /// about the **pool** — the connection is about to be reused, and it
    /// must not carry the user's half-finished transaction into whatever
    /// acquires it next.
    async fn close(&self);
}

/// How many sessions may be open at once — one pinned connection each.
///
/// It is here rather than in a driver because both ends need the same
/// number: the driver sizes its session pool by it, and the app has to
/// know when it is about to ask for one too many. Waiting for the pool to
/// time out instead would be a ten-second pause and then a message about
/// connections, for something the app could see coming.
pub const MAX_SESSIONS: usize = 8;

/// A live connection to one database.
#[async_trait]
pub trait Connection: Send + Sync {
    /// Open a session for one tab. See [`Session`] for why a tab needs one
    /// of its own rather than the pool everything else uses.
    async fn open_session(&self) -> Result<Arc<dyn Session>>;

    /// Read the full schema model for the schema tree.
    async fn introspect(&self) -> Result<Catalog>;

    /// A short description of the server, as the connections screen shows
    /// it next to the connection: "PG 16.2", "SQLite 3.45".
    async fn server_version(&self) -> Result<String>;

    /// Run one SQL statement and collect its result, up to [`Limits`]. A
    /// result the caps cut short still comes back as `Ok`, with
    /// [`QueryResult::truncated`] set: the statement did not fail, the app
    /// declined to hold the rest.
    async fn execute(&self, sql: &str) -> Result<QueryResult>;

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

#[cfg(test)]
mod tests {
    use super::*;

    fn small() -> Limits {
        Limits { max_bytes: 4096, max_cell_bytes: 8 }
    }

    fn row(n: i64) -> Vec<Value> {
        vec![Value::Int(n)]
    }

    /// The point of the whole design: what bounds a result is its size, so
    /// a narrow result runs to a row count no one would cap it at. Two
    /// `bigint` columns against the real budget: well past 120,000 rows.
    #[test]
    fn a_narrow_result_reaches_a_large_row_count() {
        let mut sink = RowSink::new(Limits::default());
        let mut kept = 0;
        while sink.push(vec![Value::Int(kept), Value::Int(kept)]) {
            kept += 1;
            if kept > 200_000 {
                break;
            }
        }
        assert!(kept > 120_000, "only {kept} narrow rows fit the budget");
        assert!(!sink.truncated(), "a narrow result of {kept} rows was capped");
    }

    /// The shape the cap is for. Rows of a megabyte each stop long before
    /// any row count would have saved the process.
    #[test]
    fn a_wide_result_is_capped_early() {
        let mut sink = RowSink::new(Limits::default());
        let mut kept = 0;
        while sink.push(vec![Value::Text("x".repeat(MAX_CELL_BYTES))]) {
            kept += 1;
            assert!(kept < 1000, "a megabyte a row should not reach {kept} rows");
        }
        assert!(sink.truncated());
        assert!(sink.bytes() <= MAX_BYTES);
    }

    #[test]
    fn a_result_that_ends_on_the_cap_is_whole() {
        // Room for exactly three of these rows and nothing more.
        let one = std::mem::size_of::<Vec<Value>>() + std::mem::size_of::<Value>();
        let mut sink = RowSink::new(Limits { max_bytes: one * 3, max_cell_bytes: 8 });
        for n in 0..3 {
            assert!(sink.push(row(n)), "row {n} was refused");
        }
        let result = sink.finish();
        assert_eq!(result.rows.len(), 3);
        assert!(!result.truncated);
    }

    #[test]
    fn the_row_past_the_cap_is_dropped_and_reported() {
        let one = std::mem::size_of::<Vec<Value>>() + std::mem::size_of::<Value>();
        let mut sink = RowSink::new(Limits { max_bytes: one * 3, max_cell_bytes: 8 });
        for n in 0..3 {
            assert!(sink.push(row(n)));
        }
        assert!(!sink.push(row(3)), "the fourth row was kept");
        let result = sink.finish();
        assert_eq!(result.rows.len(), 3);
        assert!(result.truncated);
    }

    /// A single value bigger than the whole budget must still render. An
    /// empty grid would say the query returned nothing, which is a lie.
    #[test]
    fn the_first_row_is_kept_however_big_it_is() {
        let mut sink = RowSink::new(Limits { max_bytes: 16, max_cell_bytes: 1024 });
        assert!(sink.push(vec![Value::Text("x".repeat(500))]));
        assert!(!sink.push(vec![Value::Text("x".repeat(500))]));
        let result = sink.finish();
        assert_eq!(result.rows.len(), 1);
        assert!(result.truncated);
    }

    #[test]
    fn a_cut_cell_says_it_was_cut() {
        let mut sink = RowSink::new(small());
        sink.push(vec![Value::Text("abcdefghijkl".to_string())]);
        let result = sink.finish();
        assert_eq!(result.rows[0][0], Value::Text("abcdefgh…".to_string()));
    }

    /// The cut lands on a character boundary, or the `String` is not UTF-8
    /// and the truncate panics.
    #[test]
    fn a_cut_cell_stays_utf8() {
        let mut sink = RowSink::new(small());
        // Three-byte characters, so byte 8 sits inside one.
        sink.push(vec![Value::Text("日本語です".to_string())]);
        let result = sink.finish();
        assert_eq!(result.rows[0][0], Value::Text("日本…".to_string()));
    }

    #[test]
    fn bytes_are_cut_without_a_marker() {
        // Wider than the 16 bytes `display` renders, as the real cap is.
        let limits = Limits { max_cell_bytes: 32, ..small() };
        let mut sink = RowSink::new(limits);
        sink.push(vec![Value::Bytes(vec![0xAB; 400])]);
        let result = sink.finish();
        assert_eq!(result.rows[0][0], Value::Bytes(vec![0xAB; 32]));
        // `display` is what marks it, as it does for any long byte string.
        assert!(result.rows[0][0].display().ends_with('…'));
    }
}
