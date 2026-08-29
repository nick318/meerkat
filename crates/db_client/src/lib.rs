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

/// Who ends a transaction: the server after every statement, or the user.
///
/// `Auto` is what a connection does with nothing asked of it — every
/// statement commits as it succeeds, which is Postgres's own default and
/// the only thing a viewer needs. `Manual` holds one transaction open
/// across the runs of a tab, so a set of statements lands together or not
/// at all, and the user says which.
///
/// It is a property of a **tab**, and of nothing else, because a
/// transaction belongs to one connection and a tab is one connection. A
/// connection has no say in it: "manual on prod" is a wish about one set of
/// statements, not about every tab that database will ever open, and a
/// setting that opened every tab holding a transaction would hold them open
/// behind a user who wanted one. A tab opens on `Auto`, the query toolbar
/// switches it, and the tab remembers its own mode.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
pub enum TxMode {
    /// Every statement commits on its own, as soon as it succeeds.
    #[default]
    Auto,
    /// The app opens a transaction on the tab's next run and holds it until
    /// the user commits or rolls it back.
    Manual,
}

impl TxMode {
    /// The word the toolbar, the store and the settings note all use, so
    /// the mode cannot be called one thing on screen and another on disk.
    pub fn as_str(self) -> &'static str {
        match self {
            TxMode::Auto => "auto",
            TxMode::Manual => "manual",
        }
    }

    /// Read a stored mode. Anything unrecognised — and a tab an older build
    /// saved, which has nothing here at all — is `Auto`: the mode a tab has
    /// when nobody asked for the other one.
    pub fn parse(text: Option<&str>) -> Self {
        match text {
            Some("manual") => TxMode::Manual,
            _ => TxMode::Auto,
        }
    }
}

/// How a transaction ends. The two are one statement apart and opposite in
/// every other way, so the app names which one happened rather than saying
/// "the transaction closed".
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TxEnd {
    Commit,
    Rollback,
}

impl TxEnd {
    pub fn sql(self) -> &'static str {
        match self {
            TxEnd::Commit => "COMMIT",
            TxEnd::Rollback => "ROLLBACK",
        }
    }
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

/// One statement's answer.
///
/// **`columns` is what says whether there was a result set at all.** A
/// statement that changes rows describes no columns — Postgres answers the
/// describe with `NoData` — so an empty `columns` means the statement came
/// back as a count, and `rows_affected` is the whole of what it said. A
/// `SELECT` that matched nothing still names its columns, so an empty
/// result set and a command are never confused: the one paints a grid with
/// headers and no rows, the other paints no grid.
///
/// The count cannot stand in for that test on its own, because Postgres
/// counts a `SELECT` too — the tag for one is `SELECT 5`. So
/// `rows_affected` is only ever read where `columns` is empty.
#[derive(Debug, Clone, Default)]
pub struct QueryResult {
    pub columns: Vec<String>,
    pub rows: Vec<Vec<Value>>,
    /// What the server's own completion tag counted. Meaningful only where
    /// `columns` is empty — see the note above.
    pub rows_affected: u64,
    /// Set when the memory cap stopped the read before the server ran out
    /// of rows. `false` means the result is the whole result.
    pub truncated: bool,
    /// Where the run's time went, as far as the client could see it.
    pub wire: Wire,
}

/// Why the server refused a statement it was asked to prepare.
///
/// **The two are told apart because one of them can be wrong.** A syntax
/// error is context-free: the scanner and the grammar refuse the same text
/// on any connection, whoever is looking, so it is true of the statement
/// itself. A name error is true of **this** connection — the one the check
/// went down — and a caller that knows the buffer may know better. See
/// [`CheckError`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Refusal {
    /// The scanner or the grammar would not read it.
    Syntax,
    /// A relation, column, function or type the server could not resolve.
    Name,
}

/// A statement the server refused to *parse*.
///
/// **A name error is only as good as the connection it was asked on**, and
/// that is what [`Refusal`] is for. The check resolves names against
/// whatever connection carried it, so a temp table, a `SET search_path`
/// and a table created by an earlier statement of the same buffer are all
/// things it can be wrong about. Two rules keep it honest, and neither of
/// them belongs to a driver: the check goes down the **tab's own session**
/// when the tab has one, which is where a temp table and a `search_path`
/// actually live; and the app drops name errors for statements that follow
/// one which changes what names resolve to. What is left is a name the
/// server would refuse the moment the statement ran.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CheckError {
    /// Which of the two this is, and so how much it can be trusted.
    pub refusal: Refusal,
    /// The server's own words, unabridged.
    pub message: String,
    /// Byte offset into the statement that was checked, where the server
    /// put its cursor. `None` when it named no position — the message is
    /// then all there is, and the caller marks the statement whole.
    ///
    /// Bytes, not characters: Postgres counts the position in characters
    /// and the driver converts, because every offset above this layer is a
    /// byte offset into the text the app holds.
    pub offset: Option<usize>,
}

/// What a run cost, timed by the **client**, around the wire.
///
/// A wall clock around `execute` answers "how long until I could look at
/// this", which is the right number to show and the wrong number to reason
/// about: on a VPN most of it is the link, and the user cannot tell that
/// from a slow server. These three split it as far as the client can see,
/// with no help from the engine and no extra round trip.
///
/// **The split is honest about what it cannot separate.** Postgres emits no
/// row until it has one, so `first_row_ms` bounds the server's work — but
/// for a plan that streams (a plain sequential scan) the server keeps
/// working while the wire moves, so server time and fetch time genuinely
/// overlap and no client-side clock can tell them apart. For a plan that
/// blocks (a sort, a hash aggregate, `count(*)`) the first row comes at the
/// end, and `first_row_ms` *is* the whole of the server's work.
///
/// [`ServerTiming`] is the exact answer, when the server can be asked for
/// one. This is the answer that is always there.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Wire {
    /// One round trip to this server and back, measured on a statement
    /// whose own cost is nothing — the driver's `pg_backend_pid()` trip,
    /// which it makes anyway. So it is the link's latency alone, and it is
    /// free: no round trip is added to measure it.
    pub link_ms: Option<u128>,
    /// Statement sent → the first row off the wire. Subtract `link_ms` and
    /// what is left is the server's work, up to the caveat above.
    pub first_row_ms: Option<u128>,
    /// First row → last row: the result crossing the wire, and this process
    /// decoding it. `None` when no row ever came back.
    pub fetch_ms: Option<u128>,
}

/// What the **server** says one execution of a statement cost.
///
/// The client cannot work this out, and the server will not volunteer it:
/// the wire protocol carries no timing, `EXPLAIN ANALYZE` would mean
/// rewriting the user's statement and running it twice, and the log has it
/// but no client can read the log. `pg_stat_statements` is the one place a
/// client can ask, so this is `None` wherever that extension is not
/// installed.
///
/// It is read **after the result is already on screen**, on a connection
/// that is not the one that ran the statement, so asking for it costs the
/// run nothing.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct ServerTiming {
    /// Executor time, in milliseconds.
    pub exec_ms: f64,
    /// Planner time, when the server tracks it. `pg_stat_statements`
    /// reports `0` both for "planning was free" and for "planning was not
    /// tracked", so a zero is dropped rather than shown as a measurement.
    pub plan_ms: Option<f64>,
    /// Whether this is **this** execution's own time.
    ///
    /// `pg_stat_statements` counts, it does not log: one row holds the
    /// running totals for every execution of a statement, cluster-wide. The
    /// driver subtracts the totals it saw last time, so one run between two
    /// reads gives that run's own time exactly. It is `false` when there was
    /// nothing to subtract from (a mean over every execution ever) or when
    /// somebody else ran the same statement in the same window (a mean over
    /// the few that landed there). Either way the number is worth showing
    /// and must not be shown as a measurement of this run.
    pub exact: bool,
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

    /// `rows_affected` is the server's own count, off the statement's
    /// completion tag. The sink counts rows it kept, which is a different
    /// number and not the one to report: a statement that changed a
    /// thousand rows streams none of them back.
    pub fn finish(self, rows_affected: u64) -> QueryResult {
        QueryResult {
            columns: self.columns,
            rows: self.rows,
            rows_affected,
            truncated: self.truncated,
            // The sink counts bytes, not seconds. The driver is what holds
            // the clock, because only the driver knows when the statement
            // went out, and it fills this in on the way back.
            wire: Wire::default(),
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

    /// Whether this connection is sitting inside a transaction — one the
    /// user typed a `BEGIN` for, or one the app opened because the tab is
    /// in [`TxMode::Manual`]. It is the one thing a close cannot take back
    /// for them.
    ///
    /// The answer is worth **caching against the last run**, and the cache
    /// is exact rather than approximate: the connection is pinned to one
    /// tab, so nothing but that tab's own statements can change what it is
    /// in. Ask once after each run and the answer holds until the next.
    ///
    /// An engine that cannot answer says `false` for ever, and the app then
    /// has only what it sent to go on: no transaction bar, and no close
    /// warning. Every engine that lets a tab hold a transaction open owes
    /// this an answer.
    async fn in_transaction(&self) -> Result<bool> {
        Ok(false)
    }

    /// Whether one statement parses **on this connection**.
    ///
    /// The same question [`Connection::check`] answers, asked where the
    /// answer is right: a temp table this tab made, a `SET search_path` it
    /// ran and a schema it created are all on this connection and on no
    /// other. It costs the session one round trip and runs nothing.
    ///
    /// It waits for the connection like any other statement, so a caller
    /// must not send one while a run is out — the check would queue behind
    /// the very statement the user is waiting for.
    async fn check(&self, _sql: &str) -> Result<Option<CheckError>> {
        Ok(None)
    }

    /// The same question as [`Connection::relation_exists`], down the
    /// tab's own connection — so a temp table it made and a `search_path`
    /// it set are part of the answer.
    async fn relation_exists(&self, _name: &str) -> Result<Option<bool>> {
        Ok(None)
    }

    /// Open a transaction on this session.
    ///
    /// It is **not** `execute`. A transaction boundary is not a run: it has
    /// no result to cap, no timing worth reporting to the user and no
    /// columns to describe — and `execute` would pay a round trip on the
    /// app pool asking the server what columns `COMMIT` returns.
    ///
    /// The caller must not send this on a session already inside a
    /// transaction. Postgres answers a second `BEGIN` with a warning and
    /// SQLite with an error, and neither is worth finding out: the app
    /// knows what it opened, and [`Session::in_transaction`] is what
    /// corrects it.
    async fn begin(&self) -> Result<()>;

    /// End the transaction this session is in, either way.
    ///
    /// Sent whether or not one is open, for the reason [`Session::close`]
    /// sends its `ROLLBACK` that way: outside a transaction the server says
    /// so and carries on, which is cheaper than asking first.
    async fn end_transaction(&self, how: TxEnd) -> Result<()>;

    /// What the server says the statement this session ran **last** cost.
    ///
    /// Asked after the result is on screen, never before it — the whole
    /// point is that the number is worth having and not worth waiting for.
    /// Like [`Session::in_transaction`] it goes out on a connection that is
    /// not this session's, so it cannot queue behind the run it is about.
    ///
    /// `Ok(None)` is the ordinary answer, not a failure: the extension that
    /// keeps these numbers is not installed everywhere, and a statement the
    /// server has not counted yet has nothing to report.
    ///
    /// It describes **one statement**, so a caller that sent a buffer of
    /// several must not report it as the run's: only the last one is here.
    async fn server_timing(&self) -> Result<Option<ServerTiming>> {
        Ok(None)
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

    /// Ask the server whether one statement parses, without running it.
    ///
    /// **Nothing is executed.** The driver sends the statement to be
    /// prepared and throws the prepared statement away: the server scans
    /// it, runs it through the grammar, and answers. A `DELETE` checked
    /// this way deletes nothing, takes no lock worth the name and costs
    /// one round trip.
    ///
    /// One statement, never a buffer: the extended protocol refuses more
    /// than one command in a prepared statement, so the caller splits
    /// first — which it has to do anyway, being what the gutter marks are
    /// worked out from.
    ///
    /// `Ok(None)` is "nothing to say", and an engine that cannot check
    /// says it for ever.
    ///
    /// **This is the pooled answer.** Names resolve against a connection
    /// that is nobody's session, so it cannot see a temp table or a
    /// `search_path` a tab has set — [`Session::check`] is the one to
    /// prefer when the tab has a session. See [`CheckError`].
    async fn check(&self, _sql: &str) -> Result<Option<CheckError>> {
        Ok(None)
    }

    /// Whether the server has a relation of this name, as **it** resolves
    /// the name — schema-qualified or through `search_path`, and folding
    /// case the way SQL does.
    ///
    /// It exists because [`Connection::check`] goes quiet exactly where it
    /// would be most useful. Postgres resolves no names for a utility
    /// statement until it runs one, so `DROP TABLE nosuch` prepares
    /// happily and the check has nothing to report. The app reads the name
    /// out of the statement itself — `query::relation_target` — and asks
    /// this instead.
    ///
    /// `Ok(None)` is "cannot say", and it is not `Ok(false)`: an engine
    /// with no answer must not have its silence read as "it is not there".
    async fn relation_exists(&self, _name: &str) -> Result<Option<bool>> {
        Ok(None)
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
        let result = sink.finish(0);
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
        let result = sink.finish(0);
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
        let result = sink.finish(0);
        assert_eq!(result.rows.len(), 1);
        assert!(result.truncated);
    }

    #[test]
    fn a_cut_cell_says_it_was_cut() {
        let mut sink = RowSink::new(small());
        sink.push(vec![Value::Text("abcdefghijkl".to_string())]);
        let result = sink.finish(0);
        assert_eq!(result.rows[0][0], Value::Text("abcdefgh…".to_string()));
    }

    /// The cut lands on a character boundary, or the `String` is not UTF-8
    /// and the truncate panics.
    #[test]
    fn a_cut_cell_stays_utf8() {
        let mut sink = RowSink::new(small());
        // Three-byte characters, so byte 8 sits inside one.
        sink.push(vec![Value::Text("日本語です".to_string())]);
        let result = sink.finish(0);
        assert_eq!(result.rows[0][0], Value::Text("日本…".to_string()));
    }

    #[test]
    fn bytes_are_cut_without_a_marker() {
        // Wider than the 16 bytes `display` renders, as the real cap is.
        let limits = Limits { max_cell_bytes: 32, ..small() };
        let mut sink = RowSink::new(limits);
        sink.push(vec![Value::Bytes(vec![0xAB; 400])]);
        let result = sink.finish(0);
        assert_eq!(result.rows[0][0], Value::Bytes(vec![0xAB; 32]));
        // `display` is what marks it, as it does for any long byte string.
        assert!(result.rows[0][0].display().ends_with('…'));
    }
}
