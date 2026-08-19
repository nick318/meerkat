//! PostgreSQL driver, backed by sqlx.
//!
//! Editing is not built yet: [`Connection::apply`] returns an error,
//! exactly as the SQLite driver does. A *typed* statement, though, goes to
//! the server verbatim, so a session the user marked read-only asks the
//! server for a read-only one — see `connect_with`. Introspection reads
//! `pg_catalog` rather than
//! `information_schema` for tables and columns, because `pg_catalog` also
//! carries the `reltuples` row estimate and `format_type` renders the type
//! names the way `psql` shows them. Primary keys come from
//! `information_schema`, which already reports the key column order.

use anyhow::Context as _;
use async_trait::async_trait;
use db_client::{
    Connection, Limits, Profile, QueryResult, Result, RowChange, RowSink, RunId, Session, Stop,
    Value,
};
use futures::TryStreamExt as _;
use introspect::{Catalog, Column, Schema, Table, TableKind};
use sqlx::postgres::{PgConnectOptions, PgPool, PgPoolOptions, PgRow};
use sqlx::{Column as _, Executor as _, Row as _, TypeInfo as _, ValueRef as _};
use std::str::FromStr;
use std::sync::Arc;
use std::time::Duration;

/// Two pools, and the split is the point.
///
/// `APP_CONNECTIONS` serves what the app does for itself: introspection,
/// table pages, and — the one that matters — `pg_cancel_backend`. Stopping
/// a run must never wait for a free connection, because the connection it
/// would be waiting for is the very statement the user asked to stop.
///
/// `db_client::MAX_SESSIONS` is the ceiling on live tabs, one pinned
/// connection each, and the app knows the same number — see it there.
/// Keeping them in a pool of their own is what stops a wall of open tabs
/// from starving the app's own work; a reserve written as a comment over
/// one shared pool would be a reserve until the day it was not.
const APP_CONNECTIONS: u32 = 3;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

/// SQLSTATE for `query_canceled`. The server answers a statement
/// `pg_cancel_backend` reached with it — and a statement its own
/// `statement_timeout` ran out on, which is the same event from the
/// server's side: it gave the statement up.
const QUERY_CANCELED: &str = "57014";

/// How long a run may take before the server gives it up, when nothing else
/// has an opinion. A viewer must not leave a statement on a shared server
/// for ever because a window is open somewhere.
const STATEMENT_TIMEOUT: &str = "30s";

pub struct PostgresConnection {
    /// The app's own work: introspection, table pages, and every cancel.
    pool: PgPool,
    /// One pinned connection per open session. See `APP_CONNECTIONS`.
    sessions: PgPool,
    label: Label,
    limits: Limits,
    /// What a run's `statement_timeout` is set to **when nothing else has
    /// set one**, asked for beside the backend id: once per session, or
    /// once per run on the pooled path, where every run is a new backend.
    ///
    /// Postgres says where a setting came from: `pg_settings.source` reads
    /// `default` only when nothing anywhere named a value. A role
    /// (`ALTER ROLE ... SET`) reads `user`, a database `database`, the
    /// config file `configuration file`, a startup option in the connection
    /// URL `client`, and a `SET` the user typed `session`. So the app can
    /// tell its own silence apart from somebody's choice, and it defers to
    /// every one of those — including a deliberate `0`, which `default`
    /// would never report.
    ///
    /// This is why the timeout is **not** a startup option, though
    /// `default_transaction_read_only` is. A startup option outranks
    /// `ALTER ROLE`, so asking for one would quietly overrule the DBA — and
    /// it is fixed before the connect, so it cannot be conditional on what
    /// the server turns out to say. The read-only flag has the opposite
    /// need: it is a promise, so it must survive `RESET ALL`, and there is
    /// nothing on the server to defer to.
    ///
    /// The trade is that a `RESET ALL` washes the guard off that pooled
    /// connection. The next run puts it back, because every run asks — a
    /// timeout is a guard, not a promise.
    statement_timeout: String,
}

/// What the header and the sidebar card say about this connection. The
/// pool does not hand these back once it is built, so keep them.
#[derive(Debug, Clone)]
pub struct Label {
    pub database: String,
    pub host: String,
    pub port: u16,
}

impl PostgresConnection {
    /// Connect from a `postgres://user:password@host:port/database` URL.
    /// A URL carries no saved settings, so the session is read-only: the
    /// app must not be a way to write to a database by accident.
    pub async fn connect(url: &str) -> Result<Self> {
        Self::connect_url(url, true).await
    }

    /// The same, saying outright whether the session may write. Only the
    /// driver's own tests need a writable one from a URL — the app opens a
    /// writable session from a profile, where the user chose it.
    pub async fn connect_url(url: &str, read_only: bool) -> Result<Self> {
        let options = PgConnectOptions::from_str(url)
            .with_context(|| format!("not a valid PostgreSQL URL: {url}"))?;
        Self::connect_with(options, read_only).await
    }

    /// Connect from a saved profile. The password comes from the OS
    /// keychain, keyed by the profile id; a profile without a stored
    /// password connects without one (trust, peer, or `.pgpass`).
    pub async fn connect_profile(profile: &Profile) -> Result<Self> {
        let mut options = PgConnectOptions::new().database(&profile.database);
        if let Some(host) = &profile.host {
            options = options.host(host);
        }
        if let Some(port) = profile.port {
            options = options.port(port);
        }
        if let Some(user) = &profile.user {
            options = options.username(user);
        }
        if let Some(password) = secrets::get_password(&profile.id)? {
            options = options.password(&password);
        }
        Self::connect_with(options, profile.read_only).await
    }

    async fn connect_with(options: PgConnectOptions, read_only: bool) -> Result<Self> {
        let label = Label {
            database: options.get_database().unwrap_or_default().to_string(),
            host: options.get_host().to_string(),
            port: options.get_port(),
        };
        // A read-only session is the *server's* promise, not the app's: the
        // startup packet asks for `default_transaction_read_only`, and
        // `INSERT`, `UPDATE`, `DELETE` and DDL then come back as errors
        // from Postgres itself. Nothing here reads the SQL, so there is no
        // pattern to slip past.
        //
        // It goes in the startup options rather than a `SET` after connect
        // because `RESET ALL` — which `DISCARD ALL` runs, and a pool may —
        // restores a parameter to the value the session *started* with. A
        // `SET` would be washed away by exactly the kind of reset a pool
        // does between one tab and the next; a startup option survives it.
        //
        // A connection pooler that refuses the `options` startup parameter
        // fails the connect. That is the right way round: a read-only
        // session that cannot be asked for must not open at all.
        let options = if read_only {
            options.options([("default_transaction_read_only", "on")])
        } else {
            options
        };
        let pool = PgPoolOptions::new()
            .max_connections(APP_CONNECTIONS)
            .acquire_timeout(CONNECT_TIMEOUT)
            .connect_with(options.clone())
            .await
            .with_context(|| format!("failed to connect to {}", label.host))?;
        // The same options, so a session's startup packet is the app's:
        // one read-only flag, one set of parameters, no second story about
        // what this connection may do. Lazily connected — a session pool
        // that opened eight sockets to serve nobody would be worse than
        // the pooling it replaces.
        let sessions = PgPoolOptions::new()
            .max_connections(db_client::MAX_SESSIONS as u32)
            .acquire_timeout(CONNECT_TIMEOUT)
            .connect_lazy_with(options);
        Ok(Self {
            pool,
            sessions,
            label,
            limits: Limits::default(),
            statement_timeout: STATEMENT_TIMEOUT.to_string(),
        })
    }

    /// Hold a smaller result than the app's own budget. Only the tests need
    /// this: a cap of a few kilobytes is reached in a query that takes no
    /// time, where the real one would want gigabytes of fixture.
    pub fn with_limits(mut self, limits: Limits) -> Self {
        self.limits = limits;
        self
    }

    /// Give up a run sooner than [`STATEMENT_TIMEOUT`] would. For the tests,
    /// which cannot wait out thirty seconds to watch one land.
    pub fn with_statement_timeout(mut self, timeout: &str) -> Self {
        self.statement_timeout = timeout.to_string();
        self
    }

    pub fn label(&self) -> &Label {
        &self.label
    }

    /// Hand the server's connections back now. Dropping the pool closes
    /// them eventually; a connections screen that probes several databases
    /// in a row should not wait for that.
    pub async fn close(&self) {
        self.pool.close().await;
    }
}

/// Split a `postgres://user:password@host:port/database` URL into a saved
/// profile and the password. The password is handed back separately
/// because it belongs in the OS keychain, never in the profiles file.
///
/// The URL is parsed by sqlx, so anything the driver would accept at
/// connect time is accepted here, and a typo is caught while the user is
/// still looking at the form.
pub fn profile_from_url(id: &str, name: &str, url: &str) -> Result<(Profile, Option<String>)> {
    let options = PgConnectOptions::from_str(url)
        .with_context(|| format!("not a valid PostgreSQL URL: {url}"))?;
    let database = options
        .get_database()
        .filter(|database| !database.is_empty())
        .context("the URL names no database")?
        .to_string();
    let user = options.get_username().to_string();

    let profile = Profile {
        id: id.to_string(),
        // An unnamed connection goes by its database, as the design's
        // rows do: `meerkat_prod` over the URL it came from.
        name: if name.trim().is_empty() { database.clone() } else { name.trim().to_string() },
        engine: db_client::Engine::Postgres,
        host: Some(options.get_host().to_string()),
        port: Some(options.get_port()),
        database,
        user: (!user.is_empty()).then_some(user),
        // A URL says nothing about how careful to be, so the caller sets
        // the flag from the form. Read-only is the value a new connection
        // starts on.
        read_only: true,
    };
    Ok((profile, password_in(url)))
}

/// The password inside a URL's credentials, if it carries one.
fn password_in(url: &str) -> Option<String> {
    let rest = url.split_once("://")?.1;
    let (credentials, _) = rest.split_once('@')?;
    let (_, password) = credentials.split_once(':')?;
    (!password.is_empty()).then(|| password.to_string())
}

/// `16.2 (Debian 16.2-1.pgdg120+2)` is more than a connection row needs.
fn short_version(reported: &str) -> &str {
    reported.split_whitespace().next().unwrap_or(reported)
}

/// One row of the table listing, before columns and keys are attached.
type TableRow = (String, String, String, Option<i64>);
/// One row of the column listing: schema, table, column, type, nullable, default.
type ColumnRow = (String, String, String, String, bool, Option<String>);
/// One row of the primary-key listing: schema, table, column.
type KeyRow = (String, String, String);

#[async_trait]
impl Connection for PostgresConnection {
    async fn introspect(&self) -> Result<Catalog> {
        let tables: Vec<TableRow> = sqlx::query_as(
            "SELECT n.nspname, c.relname, c.relkind::text, c.reltuples::bigint \
             FROM pg_class c \
             JOIN pg_namespace n ON n.oid = c.relnamespace \
             WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') \
               AND n.nspname NOT IN ('pg_catalog', 'information_schema') \
               AND n.nspname NOT LIKE 'pg_toast%' \
               AND n.nspname NOT LIKE 'pg_temp%' \
             ORDER BY n.nspname, c.relname",
        )
        .fetch_all(&self.pool)
        .await
        .context("failed to list tables")?;

        let columns: Vec<ColumnRow> = sqlx::query_as(
            "SELECT n.nspname, c.relname, a.attname, \
                    format_type(a.atttypid, a.atttypmod), \
                    NOT a.attnotnull, \
                    pg_get_expr(d.adbin, d.adrelid) \
             FROM pg_attribute a \
             JOIN pg_class c ON c.oid = a.attrelid \
             JOIN pg_namespace n ON n.oid = c.relnamespace \
             LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum \
             WHERE a.attnum > 0 AND NOT a.attisdropped \
               AND c.relkind IN ('r', 'p', 'v', 'm', 'f') \
               AND n.nspname NOT IN ('pg_catalog', 'information_schema') \
               AND n.nspname NOT LIKE 'pg_toast%' \
               AND n.nspname NOT LIKE 'pg_temp%' \
             ORDER BY n.nspname, c.relname, a.attnum",
        )
        .fetch_all(&self.pool)
        .await
        .context("failed to list columns")?;

        // `key_column_usage.ordinal_position` already gives key order.
        let keys: Vec<KeyRow> = sqlx::query_as(
            "SELECT tc.table_schema, tc.table_name, kcu.column_name \
             FROM information_schema.table_constraints tc \
             JOIN information_schema.key_column_usage kcu \
               ON kcu.constraint_name = tc.constraint_name \
              AND kcu.constraint_schema = tc.constraint_schema \
             WHERE tc.constraint_type = 'PRIMARY KEY' \
               AND tc.table_schema NOT IN ('pg_catalog', 'information_schema') \
             ORDER BY tc.table_schema, tc.table_name, kcu.ordinal_position",
        )
        .fetch_all(&self.pool)
        .await
        .context("failed to list primary keys")?;

        Ok(build_catalog(tables, columns, keys))
    }

    async fn server_version(&self) -> Result<String> {
        let (version,): (String,) = sqlx::query_as("SHOW server_version")
            .fetch_one(&self.pool)
            .await
            .context("failed to read the server version")?;
        Ok(format!("PG {}", short_version(&version)))
    }

    /// The app's own statements — table pages — on a pooled connection,
    /// which is a different backend every time. So the id is asked for
    /// per run here, where a session asks once: the cap has to be able to
    /// cancel the statement it stopped reading, even when nothing is
    /// watching the run.
    async fn execute(&self, sql: &str) -> Result<QueryResult> {
        let mut conn = self.pool.acquire().await.context("no connection to run the statement")?;
        let backend = prepare_run(&mut *conn, &self.statement_timeout).await?;
        let result = collect_capped(&mut conn, &self.pool, backend, sql, self.limits).await;
        // The connection goes back to the pool here.
        drop(conn);
        result
    }

    /// One pinned connection out of the session pool, its backend read
    /// once, and its timeout asked for once. Everything a tab runs after
    /// this goes down that connection, which is what makes a `SET`, a
    /// `BEGIN` and a temp table mean anything from one run to the next.
    async fn open_session(&self) -> Result<Arc<dyn Session>> {
        let mut conn = self
            .sessions
            .acquire()
            .await
            .context("no connection left for another session — close a tab")?;
        let backend = prepare_run(&mut *conn, &self.statement_timeout).await?;
        Ok(Arc::new(PostgresSession {
            app: self.pool.clone(),
            conn: futures::lock::Mutex::new(Some(conn)),
            backend,
            limits: self.limits,
        }))
    }

    /// Always on the **app** pool, never a session's: the connection being
    /// stopped is busy, and waiting for it would be waiting for the thing
    /// the user just asked to stop. That reserve is why the two pools are
    /// separate.
    async fn stop(&self, run: RunId, how: Stop) -> Result<bool> {
        cancel(&self.pool, run, how).await
    }

    async fn apply(&self, _changes: &[RowChange]) -> Result<u64> {
        anyhow::bail!("in-place editing is not implemented yet (Phase 2)")
    }
}

/// One tab's pinned connection.
///
/// `execute` takes `&self` because the tab shares the session behind an
/// `Arc`, and sqlx wants `&mut PgConnection`, so the connection sits behind
/// an async mutex. It is never contended in practice: a tab runs one
/// statement at a time. The `Option` is what `close` empties.
struct PostgresSession {
    /// The *app's* pool, not the session's. A cancel must go out on a
    /// connection that is not the one being cancelled.
    app: PgPool,
    conn: futures::lock::Mutex<Option<sqlx::pool::PoolConnection<sqlx::Postgres>>>,
    backend: RunId,
    limits: Limits,
}

#[async_trait]
impl Session for PostgresSession {
    fn backend(&self) -> Option<RunId> {
        Some(self.backend)
    }

    async fn execute(&self, sql: &str) -> Result<QueryResult> {
        let mut held = self.conn.lock().await;
        let conn = held.as_mut().context("this tab's session is closed")?;
        collect_capped(conn, &self.app, self.backend, sql, self.limits).await
    }

    /// Asked of `pg_stat_activity` from the **app** pool, never of the
    /// session itself. Two reasons: the session's connection may be busy
    /// with a statement, and asking it would have to queue behind exactly
    /// the run whose state is in question; and a transaction that has gone
    /// wrong refuses every statement until it is rolled back, so a session
    /// in the state most worth reporting is the one that could not answer.
    ///
    /// `idle in transaction` and `idle in transaction (aborted)` both
    /// count — a rollback is what leaves either one.
    async fn in_transaction(&self) -> Result<bool> {
        let state: Option<String> =
            sqlx::query_scalar("SELECT state FROM pg_stat_activity WHERE pid = $1")
                .bind(self.backend.0)
                .fetch_optional(&self.app)
                .await
                .context("failed to read what the session is doing")?
                .flatten();
        Ok(state.is_some_and(|state| state.starts_with("idle in transaction")))
    }

    async fn close(&self) {
        let Some(mut conn) = self.conn.lock().await.take() else { return };
        // Sent whether or not a transaction is open, because it costs one
        // round trip to send and one to ask. Outside a transaction Postgres
        // answers "there is no transaction in progress" and carries on, so
        // the session never has to know which it is in.
        match sqlx::query("ROLLBACK").execute(&mut *conn).await {
            // Clean, so the pool may have it back.
            Ok(_) => drop(conn),
            // Not clean, or not answering. Take it out of the pool for
            // good rather than hand the next tab a connection whose state
            // nobody knows; dropping the detached connection closes it,
            // and closing is itself a rollback.
            Err(_) => drop(conn.detach()),
        }
    }
}

/// A session may reach its last handle on **any** thread, and in this app
/// that thread is usually the UI one: leaving a workspace drops the shell,
/// every tab, and every session, inside a mouse-up.
///
/// sqlx returns a pooled connection to its pool by spawning onto tokio
/// when the last handle drops. Spawning off the runtime panics — and it
/// panics inside an Objective-C callback that cannot unwind, so the whole
/// process aborts. Detaching takes the connection out of the pool's hands
/// first, and a bare `PgConnection` drops by closing its socket, which
/// needs no runtime at all.
///
/// So this is the *fallback*, not the ordinary path: `close` gives the
/// connection back properly, from inside a tokio task, and leaves nothing
/// here to do. What reaches this is a session nobody closed — which is a
/// session whose pool is being dropped in the same breath.
impl Drop for PostgresSession {
    fn drop(&mut self) {
        if let Some(conn) = self.conn.get_mut().take() {
            drop(conn.detach());
        }
    }
}

/// Ask the server for the backend id, and for the timeout in the same
/// round trip. `set_config(.., false)` is `SET`, and the `WHERE` is what
/// makes it conditional — see `statement_timeout` on the connection for
/// why it is asked for that way rather than as a startup option.
///
/// A session pays this once, at open. The pooled path pays it once per
/// run, because a pooled connection is a different backend each time.
async fn prepare_run(conn: &mut sqlx::PgConnection, timeout: &str) -> Result<RunId> {
    let (pid, _applied): (i32, Option<String>) = sqlx::query_as(
        "SELECT pg_backend_pid(), \
         (SELECT set_config('statement_timeout', $1, false) \
            WHERE (SELECT source FROM pg_settings \
                    WHERE name = 'statement_timeout') = 'default')",
    )
    .bind(timeout)
    .fetch_one(&mut *conn)
    .await
    .context("failed to read the backend id")?;
    Ok(RunId(pid))
}

/// Run one statement and collect it, up to `limits`.
///
/// The rows are **streamed** and decoded one at a time, so the process
/// holds one row plus whatever the sink has kept — never the whole result.
/// `fetch_all` held both the raw rows and the decoded ones at once, which
/// is how `SELECT *` over a large table took the app down.
///
/// `app` is the pool used to cancel and to describe: both must reach the
/// server while `conn` is busy with the statement, so neither may be `conn`.
async fn collect_capped(
    conn: &mut sqlx::PgConnection,
    app: &PgPool,
    backend: RunId,
    sql: &str,
    limits: Limits,
) -> Result<QueryResult> {
    let mut sink = RowSink::new(limits);
    {
        let mut rows = sqlx::query(sql).fetch(&mut *conn);
        while let Some(row) = rows.try_next().await? {
            if !sink.has_columns() {
                sink.columns(row.columns().iter().map(|c| c.name().to_string()).collect());
            }
            let mut values = Vec::with_capacity(row.columns().len());
            for i in 0..row.columns().len() {
                values.push(decode(&row, i)?);
            }
            if !sink.push(values) {
                // The budget is spent. Dropping the stream here would bound
                // *this* process and nothing else: sqlx must read every
                // remaining row off the wire before the connection can be
                // used again, so the server would go on building and
                // sending the whole result. Ask it to stop instead — the
                // same `pg_cancel_backend` the stop button sends.
                let stopped = cancel(app, backend, Stop::Cancel).await.unwrap_or(false);
                drain(&mut rows, stopped).await?;
                break;
            }
        }
    }

    if !sink.has_columns() {
        // No rows came back, so the row metadata cannot name the columns.
        // Ask the server to describe the statement instead, so an empty
        // result still renders its headers. DDL and other statements
        // without a result set simply describe to nothing.
        if let Ok(described) = app.describe(sql).await {
            sink.columns(described.columns().iter().map(|c| c.name().to_string()).collect());
        }
    }
    Ok(sink.finish())
}

/// `pg_cancel_backend` asks the backend to give up its statement, which
/// comes back to the runner as SQLSTATE 57014. `pg_terminate_backend`
/// closes the backend outright, and sqlx then throws that connection away —
/// which is why it is the second press.
async fn cancel(pool: &PgPool, run: RunId, how: Stop) -> Result<bool> {
    let sql = match how {
        Stop::Cancel => "SELECT pg_cancel_backend($1)",
        Stop::Terminate => "SELECT pg_terminate_backend($1)",
    };
    let accepted: Option<bool> = sqlx::query_scalar(sql)
        .bind(run.0)
        .fetch_one(pool)
        .await
        .context("failed to ask the server to stop the statement")?;
    Ok(accepted.unwrap_or(false))
}

/// Read out whatever the server still sends after a cancel, throwing it
/// away, until the statement ends. Nothing is decoded and nothing is kept,
/// so this costs bandwidth, not memory — and the connection comes back to
/// the pool usable rather than owing the server a result.
///
/// `stopped` says the cancel was accepted, and only then is `57014` the
/// answer we asked for rather than news. Any other error is the server's own
/// and is reported.
async fn drain(
    rows: &mut futures::stream::BoxStream<'_, sqlx::Result<PgRow>>,
    stopped: bool,
) -> Result<()> {
    loop {
        match rows.try_next().await {
            Ok(Some(_)) => continue,
            Ok(None) => return Ok(()),
            Err(error) if stopped && is_cancelled(&error) => return Ok(()),
            Err(error) => return Err(error.into()),
        }
    }
}

/// Whether an error is the server saying it gave up the statement.
fn is_cancelled(error: &sqlx::Error) -> bool {
    matches!(error, sqlx::Error::Database(db) if db.code().as_deref() == Some(QUERY_CANCELED))
}

fn build_catalog(tables: Vec<TableRow>, columns: Vec<ColumnRow>, keys: Vec<KeyRow>) -> Catalog {
    let mut schemas: Vec<Schema> = Vec::new();
    for (schema_name, table_name, relkind, reltuples) in tables {
        let table = Table {
            name: table_name.clone(),
            kind: match relkind.as_str() {
                // Views and materialized views read the same to a viewer.
                "v" | "m" => TableKind::View,
                _ => TableKind::Table,
            },
            columns: columns
                .iter()
                .filter(|(s, t, ..)| *s == schema_name && *t == table_name)
                .map(|(_, _, name, data_type, nullable, default)| Column {
                    name: name.clone(),
                    data_type: data_type.clone(),
                    nullable: *nullable,
                    default: default.clone(),
                })
                .collect(),
            primary_key: keys
                .iter()
                .filter(|(s, t, _)| *s == schema_name && *t == table_name)
                .map(|(_, _, column)| column.clone())
                .collect(),
            // `reltuples` is -1 when the table was never analyzed. Report
            // nothing rather than a zero the user would read as "empty".
            approx_rows: reltuples.filter(|n| *n >= 0).map(|n| n as u64),
        };
        match schemas.last_mut() {
            Some(schema) if schema.name == schema_name => schema.tables.push(table),
            _ => schemas.push(Schema { name: schema_name, tables: vec![table] }),
        }
    }
    Catalog { schemas }
}

fn decode(row: &PgRow, i: usize) -> Result<Value> {
    let raw = row.try_get_raw(i)?;
    if raw.is_null() {
        return Ok(Value::Null);
    }
    let type_name = raw.type_info().name().to_string();
    drop(raw);

    Ok(match type_name.as_str() {
        "BOOL" => Value::Bool(row.try_get::<bool, _>(i)?),
        "INT2" => Value::Int(row.try_get::<i16, _>(i)? as i64),
        "INT4" => Value::Int(row.try_get::<i32, _>(i)? as i64),
        "INT8" => Value::Int(row.try_get::<i64, _>(i)?),
        "OID" => Value::Int(row.try_get::<i64, _>(i).unwrap_or_default()),
        "FLOAT4" => Value::Float(row.try_get::<f32, _>(i)? as f64),
        "FLOAT8" => Value::Float(row.try_get::<f64, _>(i)?),
        // NUMERIC has no lossless primitive, so render the decimal text.
        // sqlx rebuilds the scale from the wire digits and ignores the
        // declared one, so `numeric(10, 2)` 1280.00 renders as "1280".
        // The value stays exact; only trailing zeroes are lost.
        "NUMERIC" => Value::Text(row.try_get::<sqlx::types::BigDecimal, _>(i)?.to_string()),
        "TEXT" | "VARCHAR" | "BPCHAR" | "CHAR" | "NAME" | "CITEXT" | "UNKNOWN" => {
            Value::Text(row.try_get::<String, _>(i)?)
        }
        "UUID" => Value::Text(row.try_get::<sqlx::types::Uuid, _>(i)?.to_string()),
        "TIMESTAMPTZ" => Value::Text(format_offset_date_time(
            row.try_get::<sqlx::types::time::OffsetDateTime, _>(i)?,
        )),
        "TIMESTAMP" => Value::Text(format_primitive_date_time(
            row.try_get::<sqlx::types::time::PrimitiveDateTime, _>(i)?,
        )),
        "DATE" => Value::Text(format_date(row.try_get::<sqlx::types::time::Date, _>(i)?)),
        "TIME" => Value::Text(format_time(row.try_get::<sqlx::types::time::Time, _>(i)?)),
        "JSON" | "JSONB" => Value::Text(row.try_get::<serde_json::Value, _>(i)?.to_string()),
        "BYTEA" => Value::Bytes(row.try_get::<Vec<u8>, _>(i)?),
        // A viewer must never fall over on an exotic column: show the type
        // name instead of failing the whole result.
        _ => Value::Text(format!("<{type_name}>")),
    })
}

// The `time` crate's `Display` output is not ISO-8601, and pulling in a
// format description would add a direct dependency for three lines, so
// build the ISO strings by hand.

fn format_date(date: sqlx::types::time::Date) -> String {
    format!("{:04}-{:02}-{:02}", date.year(), date.month() as u8, date.day())
}

fn format_time(time: sqlx::types::time::Time) -> String {
    let (hour, minute, second, micro) = time.as_hms_micro();
    if micro == 0 {
        format!("{hour:02}:{minute:02}:{second:02}")
    } else {
        format!("{hour:02}:{minute:02}:{second:02}.{micro:06}")
    }
}

fn format_primitive_date_time(dt: sqlx::types::time::PrimitiveDateTime) -> String {
    format!("{} {}", format_date(dt.date()), format_time(dt.time()))
}

fn format_offset_date_time(dt: sqlx::types::time::OffsetDateTime) -> String {
    let offset = dt.offset();
    let (hours, minutes, _) = offset.as_hms();
    let sign = if offset.is_negative() { '-' } else { '+' };
    format!(
        "{} {} {sign}{:02}:{:02}",
        format_date(dt.date()),
        format_time(dt.time()),
        hours.abs(),
        minutes.abs()
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use db_client::{MAX_BYTES, MAX_CELL_BYTES};

    /// These tests need a live server. Set `MEERKAT_TEST_PG_URL` to a
    /// database the test may create and drop a schema in; without it the
    /// test reports that it was skipped and passes.
    fn test_url() -> Option<String> {
        match std::env::var("MEERKAT_TEST_PG_URL") {
            Ok(url) if !url.is_empty() => Some(url),
            _ => {
                eprintln!("skipped: set MEERKAT_TEST_PG_URL to run the PostgreSQL tests");
                None
            }
        }
    }

    #[tokio::test]
    async fn introspect_and_query() {
        let Some(url) = test_url() else { return };
        // This one builds the fixture, so it is the writable session.
        let conn = PostgresConnection::connect_url(&url, false).await.unwrap();

        conn.execute("DROP SCHEMA IF EXISTS meerkat_test CASCADE").await.unwrap();
        conn.execute("CREATE SCHEMA meerkat_test").await.unwrap();
        conn.execute(
            "CREATE TABLE meerkat_test.users (
                 id bigint PRIMARY KEY,
                 email text NOT NULL,
                 name text,
                 mrr numeric(10, 2),
                 active boolean DEFAULT true,
                 tags jsonb,
                 ref uuid,
                 created_at timestamptz,
                 seen_on date
             )",
        )
        .await
        .unwrap();
        conn.execute(
            "INSERT INTO meerkat_test.users VALUES
                 (1, 'ada@example.com', 'Ada', 1280.00, true, '{\"a\":1}',
                  '00000000-0000-0000-0000-000000000001',
                  timestamptz '2026-08-15 09:12:00+00', date '2026-08-15'),
                 (2, 'grace@example.com', NULL, NULL, false, NULL, NULL, NULL, NULL)",
        )
        .await
        .unwrap();
        conn.execute("ANALYZE meerkat_test.users").await.unwrap();

        let catalog = conn.introspect().await.unwrap();
        let schema = catalog
            .schemas
            .iter()
            .find(|s| s.name == "meerkat_test")
            .expect("test schema is missing from the catalog");
        let table = &schema.tables[0];
        assert_eq!(table.name, "users");
        assert_eq!(table.kind, TableKind::Table);
        assert_eq!(table.primary_key, vec!["id".to_string()]);
        assert_eq!(table.columns.len(), 9);
        assert_eq!(table.columns[1].name, "email");
        assert_eq!(table.columns[1].data_type, "text");
        assert!(!table.columns[1].nullable);
        assert!(table.columns[2].nullable);
        assert_eq!(table.approx_rows, Some(2));

        let result = conn
            .execute(
                "SELECT id, email, name, mrr, active, tags, ref, created_at, seen_on
                 FROM meerkat_test.users ORDER BY id",
            )
            .await
            .unwrap();
        assert_eq!(result.columns[0], "id");
        assert_eq!(result.rows.len(), 2);
        assert_eq!(result.rows[0][0], Value::Int(1));
        assert_eq!(result.rows[0][1], Value::Text("ada@example.com".to_string()));
        // Trailing zeroes drop: see the NUMERIC arm of `decode`.
        assert_eq!(result.rows[0][3], Value::Text("1280".to_string()));
        assert_eq!(result.rows[0][7], Value::Text("2026-08-15 09:12:00 +00:00".to_string()));
        assert_eq!(result.rows[0][4], Value::Bool(true));
        assert_eq!(result.rows[0][5], Value::Text("{\"a\":1}".to_string()));
        assert_eq!(
            result.rows[0][6],
            Value::Text("00000000-0000-0000-0000-000000000001".to_string())
        );
        assert_eq!(result.rows[0][8], Value::Text("2026-08-15".to_string()));
        assert_eq!(result.rows[1][2], Value::Null);
        assert_eq!(result.rows[1][3], Value::Null);

        conn.execute("DROP SCHEMA meerkat_test CASCADE").await.unwrap();
    }

    #[tokio::test]
    async fn unknown_types_do_not_fail_the_result() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        // `point` has no mapping in the driver; it must render as a
        // placeholder instead of failing the query.
        let result = conn.execute("SELECT point(1, 2) AS p, 1 AS n").await.unwrap();
        assert_eq!(result.rows[0][0], Value::Text("<POINT>".to_string()));
        assert_eq!(result.rows[0][1], Value::Int(1));
    }

    #[tokio::test]
    async fn empty_result_keeps_its_columns() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        let result = conn.execute("SELECT 1 AS x WHERE false").await.unwrap();
        assert!(result.rows.is_empty());
        assert_eq!(result.columns, vec!["x".to_string()]);
    }

    #[tokio::test]
    async fn errors_are_reported_not_panicked() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        let error = conn.execute("SELECT * FROM no_such_table_here").await.unwrap_err();
        assert!(error.to_string().contains("no_such_table_here"), "{error}");
    }

    #[tokio::test]
    async fn a_read_only_session_refuses_ddl_and_still_reads() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        // The server is what refuses it, so the message is the server's.
        let error = conn
            .execute("CREATE TABLE meerkat_read_only_probe (id int)")
            .await
            .unwrap_err()
            .to_string();
        assert!(error.contains("read-only transaction"), "{error}");

        // Reading is the whole point of the session, and it still works.
        assert_eq!(conn.execute("SELECT 1 AS x").await.unwrap().rows[0][0], Value::Int(1));
    }

    /// The whole point of the backend id: a statement that would run for
    /// half a minute is stopped in the middle, from another connection,
    /// with only the id the session named when it opened.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_running_statement_can_be_cancelled_from_outside() {
        let Some(url) = test_url() else { return };
        let conn = std::sync::Arc::new(PostgresConnection::connect(&url).await.unwrap());
        let session = conn.open_session().await.unwrap();
        // The whole race is gone: a session names its backend before it is
        // ever asked to run anything, so a stop has an id to aim at from
        // the moment the run leaves. There is nothing to poll for.
        let id = session.backend().unwrap();

        let runner = {
            let session = session.clone();
            tokio::spawn(async move { session.execute("SELECT pg_sleep(30)").await })
        };
        let started = std::time::Instant::now();
        // Let the statement reach the server before asking it to stop.
        tokio::time::sleep(Duration::from_millis(100)).await;

        assert!(conn.stop(id, Stop::Cancel).await.unwrap(), "the server refused the cancel");
        let error = runner.await.unwrap().unwrap_err().to_string();
        assert!(error.contains("canceling statement"), "{error}");
        // It slept for 30 seconds and this test did not.
        assert!(started.elapsed() < Duration::from_secs(10), "the cancel did not land");

        // The session outlives a cancelled statement, as the tab holding
        // it expects: a cancel ends the statement, not the connection.
        assert_eq!(session.execute("SELECT 1 AS x").await.unwrap().rows[0][0], Value::Int(1));
    }

    /// The second press. A terminated backend takes its connection with
    /// it, so the runner hears about it as a broken connection rather than
    /// as a cancelled statement — which is why the app only offers this
    /// after a cancel has already been asked for.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_running_statement_can_be_terminated() {
        let Some(url) = test_url() else { return };
        let conn = std::sync::Arc::new(PostgresConnection::connect(&url).await.unwrap());
        let session = conn.open_session().await.unwrap();
        let id = session.backend().unwrap();

        let runner = {
            let session = session.clone();
            tokio::spawn(async move { session.execute("SELECT pg_sleep(30)").await })
        };
        let started = std::time::Instant::now();
        tokio::time::sleep(Duration::from_millis(100)).await;

        assert!(conn.stop(id, Stop::Terminate).await.unwrap());
        assert!(runner.await.unwrap().is_err(), "the statement outlived its backend");
        assert!(started.elapsed() < Duration::from_secs(10), "the terminate did not land");

        // The app pool is still good afterwards: it throws the dead
        // connection away and opens another.
        assert_eq!(conn.execute("SELECT 1 AS x").await.unwrap().rows[0][0], Value::Int(1));
    }

    /// The requirement the cap is sized for: a narrow result of 120,000
    /// rows is not a large result, and nothing must cut it short.
    #[tokio::test]
    async fn a_hundred_and_twenty_thousand_narrow_rows_come_back_whole() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        let result = conn
            .execute("SELECT g, g::text AS label FROM generate_series(1, 120000) g")
            .await
            .unwrap();
        assert_eq!(result.rows.len(), 120_000);
        assert!(!result.truncated, "120,000 narrow rows were capped");
    }

    /// The shape that used to take the app down. The read stops at the
    /// budget, the statement comes back `Ok` marked truncated rather than as
    /// an error, and the connection is still good afterwards — the cancel
    /// that ended it must not poison the pool.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_result_past_the_budget_is_capped_not_failed() {
        let Some(url) = test_url() else { return };
        let limits = Limits { max_bytes: 64 * 1024, max_cell_bytes: MAX_CELL_BYTES };
        let conn = PostgresConnection::connect(&url).await.unwrap().with_limits(limits);

        // Ten million rows, of which the budget holds a few hundred.
        let result = conn
            .execute("SELECT g, repeat('x', 100) FROM generate_series(1, 10000000) g")
            .await
            .unwrap();
        assert!(result.truncated, "the result was not marked truncated");
        assert!(!result.rows.is_empty(), "a capped result must still show rows");
        assert!(result.rows.len() < 10_000_000, "the whole result came back");
        assert_eq!(result.columns.len(), 2);

        assert_eq!(conn.execute("SELECT 1 AS x").await.unwrap().rows[0][0], Value::Int(1));
    }

    /// One value must not eat the whole budget. The cut value says it was
    /// cut, so the cell is not read as the whole string.
    #[tokio::test]
    async fn one_huge_value_is_cut_to_the_cell_cap() {
        let Some(url) = test_url() else { return };
        let limits = Limits { max_bytes: MAX_BYTES, max_cell_bytes: 4096 };
        let conn = PostgresConnection::connect(&url).await.unwrap().with_limits(limits);

        let result = conn.execute("SELECT repeat('x', 2000000) AS wide").await.unwrap();
        let cell = &result.rows[0][0];
        let Value::Text(text) = cell else { panic!("not text: {cell:?}") };
        assert_eq!(text.len(), 4096 + '…'.len_utf8());
        assert!(text.ends_with('…'));
        // One row, and it fitted: the cell cap is not the byte cap.
        assert!(!result.truncated);
    }

    /// A run nobody set a timeout for gets the app's, and the server is
    /// what ends it — the message is the server's own.
    #[tokio::test]
    async fn a_run_gets_the_apps_timeout_when_nothing_else_set_one() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap().with_statement_timeout("100ms");

        let error = conn.execute("SELECT pg_sleep(5)").await.unwrap_err().to_string();
        assert!(error.contains("statement timeout"), "{error}");
        // The connection is still good: a timeout ends the statement, not
        // the backend.
        assert_eq!(conn.execute("SELECT 1 AS x").await.unwrap().rows[0][0], Value::Int(1));
    }

    /// The question the guard has to answer: was this the user's choice or
    /// nobody's? A timeout in the connection URL reads as `client` in
    /// `pg_settings.source`, so the app leaves it alone — even though its
    /// own is a hundred times shorter.
    #[tokio::test]
    async fn a_timeout_the_session_was_opened_with_is_left_alone() {
        let Some(url) = test_url() else { return };
        let joiner = if url.contains('?') { '&' } else { '?' };
        let url = format!("{url}{joiner}options=-c%20statement_timeout%3D20s");
        let conn = PostgresConnection::connect(&url).await.unwrap().with_statement_timeout("100ms");

        // Well past the app's 100 ms, well inside the session's own 20 s.
        conn.execute("SELECT pg_sleep(0.5)").await.unwrap();
        let result = conn
            .execute("SELECT setting, source FROM pg_settings WHERE name = 'statement_timeout'")
            .await
            .unwrap();
        assert_eq!(result.rows[0][0], Value::Text("20000".to_string()));
        assert_eq!(result.rows[0][1], Value::Text("client".to_string()));
    }

    /// What the session is for. Two statements a buffer sends in turn are
    /// one connection apart, so the second sees what the first set. On the
    /// pool this was luck: each statement took whichever connection was
    /// free, and `SET` reached whichever backend that was.
    #[tokio::test]
    async fn a_statement_sees_what_the_statement_before_it_set() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();
        let session = conn.open_session().await.unwrap();

        session.execute("SET statement_timeout = '5min'").await.unwrap();
        let result = session
            .execute("SELECT setting, source FROM pg_settings WHERE name = 'statement_timeout'")
            .await
            .unwrap();
        assert_eq!(result.rows[0][0], Value::Text("300000".to_string()));
        assert_eq!(result.rows[0][1], Value::Text("session".to_string()));

        // ...and a second session is a second connection, so it is not
        // carrying the first one's settings.
        let other = conn.open_session().await.unwrap();
        let result = other
            .execute("SELECT setting FROM pg_settings WHERE name = 'statement_timeout'")
            .await
            .unwrap();
        assert_ne!(result.rows[0][0], Value::Text("300000".to_string()));
    }

    /// A transaction spans statements, which is the thing the pool could
    /// not do at all: `BEGIN` on one connection and the next statement on
    /// another is not a transaction, it is two.
    #[tokio::test]
    async fn a_transaction_spans_the_statements_of_a_session() {
        let Some(url) = test_url() else { return };
        // Writable, because a temp table is a write.
        let conn = PostgresConnection::connect_url(&url, false).await.unwrap();
        let session = conn.open_session().await.unwrap();

        session.execute("BEGIN").await.unwrap();
        session.execute("CREATE TEMP TABLE meerkat_session_probe (id int)").await.unwrap();
        session.execute("INSERT INTO meerkat_session_probe VALUES (1), (2)").await.unwrap();
        let result = session.execute("SELECT count(*) FROM meerkat_session_probe").await.unwrap();
        assert_eq!(result.rows[0][0], Value::Int(2));

        session.execute("ROLLBACK").await.unwrap();
        // The table went with the transaction, and the session is still
        // good enough to say so.
        let error =
            session.execute("SELECT * FROM meerkat_session_probe").await.unwrap_err().to_string();
        assert!(error.contains("meerkat_session_probe"), "{error}");
    }

    /// Closing rolls back, and the point is the **pool**, not the server:
    /// a closing socket already rolls a transaction back. This connection
    /// is about to be reused, and must not carry the user's half-finished
    /// transaction to whatever acquires it next.
    #[tokio::test]
    async fn closing_a_session_rolls_its_transaction_back() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();
        let session = conn.open_session().await.unwrap();
        let pid = session.backend().expect("a Postgres session knows its backend").0;

        session.execute("BEGIN").await.unwrap();
        session.execute("SELECT 1").await.unwrap();
        assert_eq!(backend_state(&conn, pid).await, "idle in transaction");

        session.close().await;
        assert_eq!(backend_state(&conn, pid).await, "idle");

        // And it says so rather than pretending to run.
        let error = session.execute("SELECT 1").await.unwrap_err().to_string();
        assert!(error.contains("closed"), "{error}");
    }

    /// What the close dialog asks before it asks the user. It is read from
    /// the app pool, so it answers about a session that has stopped being
    /// able to answer for itself.
    #[tokio::test]
    async fn a_session_says_whether_it_is_in_a_transaction() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();
        let session = conn.open_session().await.unwrap();

        assert!(!session.in_transaction().await.unwrap());
        session.execute("BEGIN").await.unwrap();
        assert!(session.in_transaction().await.unwrap());

        // A transaction that has gone wrong refuses every statement until
        // it is rolled back, so the session in the state most worth
        // reporting is the one that cannot report it. Asking elsewhere is
        // what makes this answerable at all.
        assert!(session.execute("SELECT no_such_column").await.is_err());
        assert!(session.in_transaction().await.unwrap());

        session.execute("ROLLBACK").await.unwrap();
        assert!(!session.in_transaction().await.unwrap());
    }

    /// A session may reach its last `Arc` **anywhere**, and for this app
    /// that anywhere is the UI thread: leaving a workspace drops the shell,
    /// every tab, and every session, on a mouse-up.
    ///
    /// sqlx returns a pooled connection to its pool by spawning onto tokio
    /// when the last handle drops, and spawning off the runtime panics —
    /// inside an Objective-C callback that cannot unwind, so the process
    /// aborts. A session has to survive being dropped by whoever is
    /// holding it last.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_session_may_be_dropped_off_the_runtime() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();
        let session = conn.open_session().await.unwrap();
        session.execute("SELECT 1").await.unwrap();

        // A thread with no runtime of its own: the UI thread, as far as
        // sqlx is concerned.
        std::thread::spawn(move || drop(session)).join().unwrap();

        // The pool is unharmed and still hands out sessions.
        conn.open_session().await.unwrap().execute("SELECT 1").await.unwrap();
    }

    /// A statement that fails must not cost the session. The tab keeps it
    /// across an error, so a typo does not silently end a transaction.
    #[tokio::test]
    async fn a_failed_statement_leaves_the_session_usable() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();
        let session = conn.open_session().await.unwrap();

        assert!(session.execute("SELECT * FROM no_such_table_here").await.is_err());
        assert_eq!(session.execute("SELECT 1 AS x").await.unwrap().rows[0][0], Value::Int(1));
    }

    /// What another connection sees this backend doing. It is read from
    /// the *app* pool, so it answers whether or not the session is busy.
    async fn backend_state(conn: &PostgresConnection, pid: i32) -> String {
        let result = conn
            .execute(&format!("SELECT state FROM pg_stat_activity WHERE pid = {pid}"))
            .await
            .unwrap();
        result.rows[0][0].display()
    }

    #[tokio::test]
    async fn a_read_only_session_survives_a_reset_of_its_parameters() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        // `RESET ALL` puts every parameter back to what the session
        // started with. The flag is a startup option, so this is exactly
        // what it must survive — a `SET` after connect would not.
        conn.execute("RESET ALL").await.unwrap();
        let error = conn
            .execute("CREATE TABLE meerkat_read_only_probe (id int)")
            .await
            .unwrap_err()
            .to_string();
        assert!(error.contains("read-only transaction"), "{error}");
    }

    #[test]
    fn a_url_becomes_a_profile_and_a_keychain_password() {
        let (profile, password) = profile_from_url(
            "p1",
            "prod",
            "postgres://ada:hunter2@db.internal:5432/meerkat",
        )
        .unwrap();
        assert_eq!(profile.name, "prod");
        assert_eq!(profile.host.as_deref(), Some("db.internal"));
        assert_eq!(profile.port, Some(5432));
        assert_eq!(profile.database, "meerkat");
        assert_eq!(profile.user.as_deref(), Some("ada"));
        // The password never reaches the profile itself.
        assert_eq!(password.as_deref(), Some("hunter2"));

        let (profile, password) =
            profile_from_url("p2", "  ", "postgres://ada@db.internal/analytics").unwrap();
        // No name given: the database names the connection.
        assert_eq!(profile.name, "analytics");
        assert_eq!(password, None);
    }

    #[test]
    fn a_url_without_a_database_is_rejected() {
        let error = profile_from_url("p1", "prod", "postgres://ada@db.internal").unwrap_err();
        assert!(error.to_string().contains("names no database"), "{error}");
        let error = profile_from_url("p1", "prod", "not a url").unwrap_err();
        assert!(error.to_string().contains("not a valid PostgreSQL URL"), "{error}");
    }


    #[test]
    fn the_server_version_reads_short() {
        assert_eq!(short_version("16.2 (Debian 16.2-1.pgdg120+2)"), "16.2");
        assert_eq!(short_version("15.6"), "15.6");
    }
}
