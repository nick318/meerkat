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
//! Indexes, constraints, triggers, sequences, routines and types come
//! from `pg_catalog` too, each rendered by the server's own `pg_get_*`
//! function so a definition reads the way `\d` prints it.

use anyhow::Context as _;
use async_trait::async_trait;
use db_client::{
    CheckError, Connection, Limits, Profile, QueryResult, Refusal, Result, RowChange, RowSink,
    RunId, ServerTiming, Session, Stop, TxEnd, Value, Wire,
};
use futures::TryStreamExt as _;
use introspect::{
    Catalog, Column, Constraint, ConstraintKind, ForeignKey, Index, Routine, RoutineKind, Schema,
    Sequence, Table, TableKind, Trigger, TypeKind, UserType,
};
use sqlx::postgres::{
    PgConnectOptions, PgDatabaseError, PgErrorPosition, PgPool, PgPoolOptions, PgQueryResult,
    PgRow, PgTypeInfo, PgTypeKind, PgValueFormat, PgValueRef,
};
use sqlx::{Column as _, Either, Executor as _, Row as _, TypeInfo as _, ValueRef as _};
use std::collections::{BTreeMap, HashMap};
use std::str::FromStr;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

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

/// SQLSTATE for `syntax_error`. The scanner and the grammar both refuse
/// with it — an unterminated string, a missing bracket, a keyword where a
/// name belongs — and nothing that needs the catalog does. That is what
/// makes it true of the statement rather than of the connection it was
/// asked on: see [`db_client::Refusal`].
const SYNTAX_ERROR: &str = "42601";
/// SQLSTATEs for a name the server could not resolve. `42P01` covers a
/// missing schema as well as a missing table — the server reports
/// `nosuchschema.t` as a relation that does not exist rather than as a bad
/// schema name — so four codes are the whole of it.
///
/// `42P18` (indeterminate datatype) is deliberately **not** here. A
/// statement holding a bare `$1` raises it, and a placeholder is a thing
/// the user typed on purpose, not a name they got wrong.
const NAME_ERRORS: &[&str] = &[
    // undefined_table, and a schema that does not exist with it.
    "42P01", // undefined_column.
    "42703", // undefined_function.
    "42883", // undefined_object, which is what a type that does not exist is.
    "42704",
];

/// Ask the server whether it has a relation of this name.
///
/// `to_regclass` is the whole of it, and it is the right call rather than
/// a query over `pg_class`: it resolves the name the way the *parser*
/// would — through `search_path` when it is unqualified, folding an
/// unquoted part to lower case and keeping a quoted one — and it answers
/// NULL rather than raising when there is nothing there, including for a
/// name that is not a name at all.
///
/// It is asked on whichever connection carried it, so a session sees its
/// own temp tables and its own `search_path`.
async fn relation_on(conn: &mut sqlx::PgConnection, name: &str) -> Result<Option<bool>> {
    let (found,): (bool,) = sqlx::query_as("SELECT to_regclass($1) IS NOT NULL")
        .bind(name)
        .fetch_one(conn)
        .await
        .context("failed to ask the server about a relation")?;
    Ok(Some(found))
}

/// Ask the server to prepare one statement, and throw the result away.
///
/// **Nothing runs.** `prepare_with` rather than `describe`, and the
/// difference is a round trip: `describe` follows the parse with a catalog
/// query for the nullability of each column, which nothing here reads.
/// sqlx keeps the prepared statement in the connection's own LRU cache and
/// sends a `Close` when it falls out of it, so a session of typing leaves a
/// bounded number of them on the server rather than one per keystroke.
///
/// Both callers share this, and the connection is the whole difference
/// between them: names resolve against whatever carried the question. See
/// [`db_client::CheckError`].
async fn check_statement(conn: &mut sqlx::PgConnection, sql: &str) -> Option<CheckError> {
    let error = match conn.prepare_with(sql, &[]).await {
        Ok(_) => return None,
        Err(sqlx::Error::Database(error)) => error,
        // A dropped connection is not the statement's fault, and the
        // caller has nowhere to put it: a check nobody asked for must not
        // raise an error over a query that has not been run.
        Err(_) => return None,
    };
    let error = error.try_downcast_ref::<PgDatabaseError>()?;
    let refusal = match error.code() {
        SYNTAX_ERROR => Refusal::Syntax,
        code if NAME_ERRORS.contains(&code) => Refusal::Name,
        // Everything else the parser can raise is about neither the text
        // nor a name — a transaction that has gone wrong, a permission,
        // an `$1` with no type to infer — and none of it is worth a mark
        // over a statement nobody has run.
        _ => return None,
    };
    // `Internal` names a position inside a statement the *server*
    // generated — the body of a PL/pgSQL function, say — which is not an
    // offset into anything the editor holds.
    let offset = match error.position() {
        Some(PgErrorPosition::Original(position)) => byte_offset(sql, position),
        _ => None,
    };
    Some(CheckError {
        refusal,
        message: error.message().to_string(),
        offset,
    })
}

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
        Self::connect_probe(profile, Password::Keychain).await
    }

    /// Connect from a profile, saying where the password comes from. It is
    /// what the connection form's "test" needs: the credentials it tests
    /// have not been saved, so there may be nothing in the keychain under
    /// that id to read, and "no password" is a different answer from "look
    /// one up".
    pub async fn connect_probe(profile: &Profile, password: Password<'_>) -> Result<Self> {
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
        match password {
            Password::Keychain => {
                if let Some(password) = secrets::get_password(&profile.id)? {
                    options = options.password(&password);
                }
            }
            Password::Given(Some(password)) => options = options.password(password),
            Password::Given(None) => {}
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

/// Where a connect gets its password.
///
/// The keychain is the ordinary answer — a saved profile keeps its password
/// there, keyed by its id — but the connection form tests credentials that
/// have not been saved, and for those there is nothing under that id to
/// read. `Given(None)` is then "connect without a password" (trust, peer or
/// `.pgpass`), which is a different request from "look one up".
pub enum Password<'a> {
    Keychain,
    Given(Option<&'a str>),
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
        name: if name.trim().is_empty() {
            database.clone()
        } else {
            name.trim().to_string()
        },
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

/// Where the server's cursor lands in the statement, in bytes.
///
/// Postgres counts the position in **characters** and counts from one; the
/// app counts bytes from zero, as everything above the driver does. A
/// position one past the last character — which is what "syntax error at
/// end of input" reports — is the length of the statement, and is a
/// position like any other: the caller has a rule for marking the end.
fn byte_offset(sql: &str, position: usize) -> Option<usize> {
    let index = position.checked_sub(1)?;
    sql.char_indices()
        .map(|(offset, _)| offset)
        .chain([sql.len()])
        .nth(index)
}

/// One row of the table listing, before columns and keys are attached.
type TableRow = (String, String, String, Option<i64>);
/// One row of the column listing: schema, table, column, type, nullable, default.
type ColumnRow = (String, String, String, String, bool, Option<String>);
/// One row of the primary-key listing: schema, table, column.
type KeyRow = (String, String, String);
/// One index: schema, table, index, method and key, unique, primary.
type IndexRow = (String, String, String, String, bool, bool);
/// One constraint: schema, table, name, `contype`, definition, columns,
/// and for a foreign key the referenced schema, table and columns.
type ConstraintRow = (
    String,
    String,
    String,
    String,
    String,
    Vec<String>,
    Option<String>,
    Option<String>,
    Vec<String>,
);
/// One trigger: schema, table, name, `tgtype`, the function it runs.
type TriggerRow = (String, String, String, i32, String);
/// One sequence: schema, name, type.
type SequenceRow = (String, String, String);
/// One routine: schema, name, `prokind`, identity arguments, result.
type RoutineRow = (String, String, String, String, Option<String>);
/// One type: schema, name, `typtype`, what it is made of.
type TypeRow = (String, String, String, Option<String>);

#[async_trait]
impl Connection for PostgresConnection {
    async fn introspect(&self) -> Result<Catalog> {
        let mut conn = self
            .pool
            .acquire()
            .await
            .context("no connection to read the catalog")?;
        read_catalog(&mut conn).await
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
        let mut conn = self
            .pool
            .acquire()
            .await
            .context("no connection to run the statement")?;
        let (backend, link) = prepare_run(&mut *conn, &self.statement_timeout).await?;
        let mut result = collect_capped(&mut conn, &self.pool, backend, sql, self.limits).await;
        // The connection goes back to the pool here.
        drop(conn);
        if let Ok(result) = &mut result {
            result.wire.link_ms = Some(link);
        }
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
        let (backend, link) = prepare_run(&mut *conn, &self.statement_timeout).await?;
        Ok(Arc::new(PostgresSession {
            app: self.pool.clone(),
            conn: futures::lock::Mutex::new(Some(conn)),
            backend,
            link_ms: link,
            counted: std::sync::Mutex::new(HashMap::new()),
            no_statement_stats: AtomicBool::new(false),
            limits: self.limits,
        }))
    }

    /// The pooled check, for a tab that has never run anything.
    ///
    /// It goes out on the **app** pool, like every other question the app
    /// asks for itself, and the cost of that is exactly what
    /// [`Refusal::Name`] warns about: this connection is nobody's session,
    /// so a temp table and a `search_path` are invisible to it. A tab with
    /// a session should ask [`Session::check`] instead.
    async fn check(&self, sql: &str) -> Result<Option<CheckError>> {
        let mut conn = self
            .pool
            .acquire()
            .await
            .context("no connection to check the statement")?;
        Ok(check_statement(&mut conn, sql).await)
    }

    async fn relation_exists(&self, name: &str) -> Result<Option<bool>> {
        let mut conn = self
            .pool
            .acquire()
            .await
            .context("no connection to ask about a name")?;
        relation_on(&mut conn, name).await
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
    /// One round trip to this server, measured at open on the trip that
    /// reads the backend id. A session is one connection to one host, so
    /// the number holds for every run on it; it is remeasured only by
    /// opening another session.
    link_ms: u128,
    /// The running totals `pg_stat_statements` reported for each statement
    /// this session has run, as of the last time it was asked.
    ///
    /// **This is what makes one execution's own time knowable.** The view
    /// counts rather than logs — one row of totals per statement, shared by
    /// every backend in the cluster — so a single reading can only give a
    /// mean. Two readings with one execution between them give that
    /// execution, and `calls` is what says how many landed in the window.
    counted: std::sync::Mutex<HashMap<i64, Counted>>,
    /// Set once the server has answered that it cannot report these
    /// numbers: no `pg_stat_statements`, or a server too old to carry
    /// `pg_stat_activity.query_id`. Asking again every run would be one
    /// wasted round trip per run, for ever, on every such server.
    no_statement_stats: AtomicBool,
    limits: Limits,
}

impl PostgresSession {
    /// Send a transaction boundary down this session's own connection.
    ///
    /// It has to be this connection and no other: a transaction belongs to
    /// the connection that opened it, so a `COMMIT` on a pooled one would
    /// commit nothing and say it worked.
    async fn boundary(&self, sql: &str) -> Result<()> {
        let mut held = self.conn.lock().await;
        let conn = held.as_mut().context("this tab's session is closed")?;
        sqlx::query(sql)
            .execute(&mut **conn)
            .await
            .with_context(|| format!("the server refused {sql}"))?;
        Ok(())
    }
}

/// One reading of a statement's running totals.
#[derive(Clone, Copy)]
struct Counted {
    calls: i64,
    exec_ms: f64,
    plan_ms: f64,
}

#[async_trait]
impl Session for PostgresSession {
    fn backend(&self) -> Option<RunId> {
        Some(self.backend)
    }

    async fn execute(&self, sql: &str) -> Result<QueryResult> {
        let mut held = self.conn.lock().await;
        let conn = held.as_mut().context("this tab's session is closed")?;
        let mut result = collect_capped(conn, &self.app, self.backend, sql, self.limits).await;
        if let Ok(result) = &mut result {
            result.wire.link_ms = Some(self.link_ms);
        }
        result
    }

    /// The same question, down the tab's own connection — so a temp
    /// table it made and a `search_path` it set are part of the answer.
    ///
    /// It takes the session's lock, which is why the caller must not send
    /// one while a run is out.
    async fn check(&self, sql: &str) -> Result<Option<CheckError>> {
        let mut held = self.conn.lock().await;
        let conn = held.as_mut().context("this tab's session is closed")?;
        Ok(check_statement(conn, sql).await)
    }

    async fn relation_exists(&self, name: &str) -> Result<Option<bool>> {
        let mut held = self.conn.lock().await;
        let conn = held.as_mut().context("this tab's session is closed")?;
        relation_on(conn, name).await
    }

    /// Read what the server counted for the statement this session ran
    /// last, and turn two readings into one execution's time.
    ///
    /// One round trip, and it goes out on the **app** pool for the reason
    /// `in_transaction` does: this session's connection may still be
    /// finishing with the very statement being asked about, and a
    /// transaction that has gone wrong refuses every statement until it is
    /// rolled back. It is also asked *after* the result is on screen, so
    /// the round trip is never in front of the user.
    ///
    /// `pg_stat_activity.query_id` is what names the statement. It needs
    /// `compute_query_id`, which `pg_stat_statements` turns on by itself,
    /// and it is retained on an idle backend — the column is that backend's
    /// *most recent* query, not only a running one. So the two views join
    /// on it and the driver never has to match SQL text, which would not
    /// match anyway: the view normalizes constants out.
    async fn server_timing(&self) -> Result<Option<ServerTiming>> {
        if self.no_statement_stats.load(Ordering::Relaxed) {
            return Ok(None);
        }
        let row: Option<(i64, i64, f64, f64)> = match sqlx::query_as(STATEMENT_STATS)
            .bind(self.backend.0)
            .fetch_optional(&self.app)
            .await
        {
            Ok(row) => row,
            // The server cannot answer this question and never will on this
            // connection: the extension is not installed, or the column is
            // not there to join on. That is not a failure worth reporting —
            // the timing the client measured itself still stands.
            Err(_) => {
                self.no_statement_stats.store(true, Ordering::Relaxed);
                return Ok(None);
            }
        };
        let Some((queryid, calls, exec_ms, plan_ms)) = row else {
            return Ok(None);
        };
        let now = Counted {
            calls,
            exec_ms,
            plan_ms,
        };
        let before = self
            .counted
            .lock()
            .map(|mut counted| counted.insert(queryid, now))
            .unwrap_or(None);
        Ok(between(before, now))
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
    /// The catalog down this tab's own connection, which is the one place
    /// a table it created inside an open transaction exists yet.
    async fn introspect(&self) -> Result<Option<Catalog>> {
        let mut held = self.conn.lock().await;
        let conn = held.as_mut().context("this tab's session is closed")?;
        read_catalog(conn).await.map(Some)
    }

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

    /// One statement, one round trip, and nothing else: no cap, no timing,
    /// no `describe` to ask what columns `BEGIN` returns. See
    /// [`Session::begin`] for why a boundary is not a run.
    async fn begin(&self) -> Result<()> {
        self.boundary("BEGIN").await
    }

    async fn end_transaction(&self, how: TxEnd) -> Result<()> {
        self.boundary(how.sql()).await
    }

    async fn close(&self) {
        let Some(mut conn) = self.conn.lock().await.take() else {
            return;
        };
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
///
/// It also **times the link, for free**. This statement reads two catalog
/// values and sets a string: the server's share of it rounds to nothing, so
/// what the clock around it measures is one round trip to this host and
/// back. A viewer over a VPN needs that number to make sense of any other —
/// and it must not cost a round trip of its own to get, or measuring the
/// lag would add to it.
async fn prepare_run(conn: &mut sqlx::PgConnection, timeout: &str) -> Result<(RunId, u128)> {
    let started = Instant::now();
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
    Ok((RunId(pid), started.elapsed().as_millis()))
}

/// The running totals for the statement a backend ran last.
///
/// `userid` and `dbid` are part of the join because `queryid` is not a key
/// on its own: the same normalized statement run by two roles, or against
/// two databases, is two rows, and summing them would report a stranger's
/// work as this session's.
const STATEMENT_STATS: &str = "\
SELECT s.queryid, s.calls, s.total_exec_time, s.total_plan_time \
  FROM pg_stat_activity a \
  JOIN pg_stat_statements s \
    ON s.queryid = a.query_id \
   AND s.userid = a.usesysid \
   AND s.dbid = (SELECT oid FROM pg_database WHERE datname = a.datname) \
 WHERE a.pid = $1";

/// Turn two readings of a statement's running totals into what one
/// execution cost. A plain function over numbers, so the three cases can be
/// argued with in a test rather than against a server.
///
/// - **One execution between the readings** is the answer asked for, and it
///   is exact.
/// - **Several** means somebody else ran the same statement in the same
///   window — the view is cluster-wide — so the honest answer is the mean
///   over them, marked inexact.
/// - **No earlier reading** leaves nothing to subtract, so the answer is
///   the mean over every execution the server has ever counted. That is the
///   first run of a statement in a tab: worth showing, never worth calling
///   this run's time. The reading just taken is what makes the *next* run
///   exact.
///
/// A `calls` that went *down* is the view being reset or the entry evicted
/// under `pg_stat_statements.max`. There is no delta to take across that,
/// so it answers nothing and lets the reading just taken be the baseline.
fn between(before: Option<Counted>, now: Counted) -> Option<ServerTiming> {
    let (exec, plan, calls, exact) = match before {
        Some(before) if now.calls > before.calls => (
            now.exec_ms - before.exec_ms,
            now.plan_ms - before.plan_ms,
            (now.calls - before.calls) as f64,
            now.calls - before.calls == 1,
        ),
        Some(before) if now.calls == before.calls => return None,
        // Either the first reading, or one taken across a reset.
        _ if now.calls > 0 => (now.exec_ms, now.plan_ms, now.calls as f64, false),
        _ => return None,
    };
    Some(ServerTiming {
        exec_ms: exec / calls,
        // A zero here says "planning was not tracked" as readily as it says
        // "planning was free", and the two are not the same claim.
        plan_ms: (plan > 0.).then(|| plan / calls),
        exact,
    })
}

/// Run one statement and collect it, up to `limits`.
///
/// The rows are **streamed** and decoded one at a time, so the process
/// holds one row plus whatever the sink has kept — never the whole result.
/// `fetch_all` held both the raw rows and the decoded ones at once, which
/// is how `SELECT *` over a large table took the app down.
///
/// It streams with `fetch_many` rather than `fetch`, because `fetch` yields
/// the rows and **throws the completion tag away** — and the tag is the
/// only place the number of rows an `UPDATE` changed is ever stated. A
/// statement that changes rows sends no row at all, so `fetch` answered one
/// with nothing whatever: no rows, no columns and no count. `fetch_many`
/// hands the tag over as `Either::Left`, which is where `rows_affected`
/// comes from.
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
    // Stamped before the statement goes out and again on the first row off
    // the wire. Postgres emits no row until it has one, so the gap bounds
    // the server's work — see `db_client::Wire` for what that does and does
    // not separate.
    let sent = Instant::now();
    let mut first_row = None;
    // The server's own count, off the completion tag. Summed rather than
    // assigned, because one call may answer with more than one tag.
    let mut affected = 0;
    {
        // `Executor::fetch_many` rather than `Query::fetch_many`, which is
        // deprecated for taking several statements in one string. This
        // sends one, through the extended protocol exactly as `fetch` did —
        // so the values still come back in binary and `off_the_wire` still
        // reads what it expects.
        let mut stream = (&mut *conn).fetch_many(sqlx::query(sql));
        while let Some(step) = stream.try_next().await? {
            let row = match step {
                // The statement finished. For an `UPDATE` this is the whole
                // of the answer; for a `SELECT` it repeats the row count,
                // which is why the caller reads it only where there are no
                // columns.
                Either::Left(done) => {
                    affected += done.rows_affected();
                    continue;
                }
                Either::Right(row) => row,
            };
            // Taken before the row is decoded, or this process's own
            // decoding would be counted as the server's time.
            first_row.get_or_insert_with(Instant::now);
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
                drain(&mut stream, stopped).await?;
                break;
            }
        }
    }

    // A statement that changed rows has none to name columns with and no
    // columns to describe — the describe would answer `NoData`. So the
    // count is what says not to ask: a round trip on the app pool for
    // headers that cannot exist is a round trip on every `UPDATE`.
    //
    // A count of zero is the case that still has to ask, because a
    // `SELECT` matching nothing reads exactly the same way here, and an
    // empty result set has headers worth painting.
    if !sink.has_columns() && affected == 0 {
        // No rows came back, so the row metadata cannot name the columns.
        // Ask the server to describe the statement instead, so an empty
        // result still renders its headers. DDL and other statements
        // without a result set simply describe to nothing.
        if let Ok(described) = app.describe(sql).await {
            sink.columns(
                described
                    .columns()
                    .iter()
                    .map(|c| c.name().to_string())
                    .collect(),
            );
        }
    }
    let mut result = sink.finish(affected);
    result.wire = wire_of(sent, first_row);
    Ok(result)
}

/// The client's own view of where a run's time went. `link_ms` is filled in
/// by the caller, which is the only side that knows which connection — and
/// so which measured round trip — the statement went down.
fn wire_of(sent: Instant, first_row: Option<Instant>) -> Wire {
    Wire {
        link_ms: None,
        first_row_ms: first_row.map(|at| at.duration_since(sent).as_millis()),
        // A statement that returned no rows has no fetch to measure. `0`
        // would read as "the rows crossed instantly", which is a different
        // claim from "there were none".
        fetch_ms: first_row.map(|at| at.elapsed().as_millis()),
    }
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
    rows: &mut futures::stream::BoxStream<'_, sqlx::Result<Either<PgQueryResult, PgRow>>>,
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

/// The catalog reads, on whichever connection is handed in.
///
/// One function for the pool and for a session, so the two cannot drift
/// into two catalogs: a session reads it to see its own uncommitted DDL,
/// and must see exactly what the sidebar would see once that commits.
///
/// Ten statements, each over one system catalog. A join of all of them
/// would multiply the rows — every column by every index by every
/// trigger — so the tree is put together here instead, by `build_catalog`.
/// Every read skips the system schemas the same way, and the routines and
/// types skip what an extension installed: `CREATE EXTENSION pgcrypto`
/// puts forty functions into `public`, and none of them is the user's.
///
/// **The relations are the catalog; everything else is extra.** The
/// tables, their columns and their keys fail the read when they fail, as
/// they always have. The objects after them are read *best-effort*: a
/// server that refuses one of those reads — a fork, a server older than
/// the catalog column asked for — answers with an empty list rather than
/// costing the sidebar its tables. What needs a catalog newer than
/// PostgreSQL 10 is not sent to an older server at all, so it cannot
/// abort a transaction the session read is running inside.
async fn read_catalog(conn: &mut sqlx::PgConnection) -> Result<Catalog> {
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
    .fetch_all(&mut *conn)
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
    .fetch_all(&mut *conn)
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
    .fetch_all(&mut *conn)
    .await
    .context("failed to list primary keys")?;

    let version: i32 = sqlx::query_scalar("SELECT current_setting('server_version_num')::int")
        .fetch_one(&mut *conn)
        .await
        .context("failed to read the server version")?;

    // `pg_get_indexdef` writes the whole `CREATE INDEX`; the part after
    // `USING` is the method and the key, which is what differs between
    // two indexes of one table.
    let indexes: Vec<IndexRow> = sqlx::query_as(
        "SELECT n.nspname, c.relname, ic.relname, \
                coalesce(substring(pg_get_indexdef(i.indexrelid) FROM ' USING (.*)$'), \
                         pg_get_indexdef(i.indexrelid)), \
                i.indisunique, i.indisprimary \
         FROM pg_index i \
         JOIN pg_class c ON c.oid = i.indrelid \
         JOIN pg_class ic ON ic.oid = i.indexrelid \
         JOIN pg_namespace n ON n.oid = c.relnamespace \
         WHERE c.relkind IN ('r', 'p', 'm', 'f') \
           AND n.nspname NOT IN ('pg_catalog', 'information_schema') \
           AND n.nspname NOT LIKE 'pg_toast%' \
           AND n.nspname NOT LIKE 'pg_temp%' \
         ORDER BY n.nspname, c.relname, ic.relname",
    )
    .fetch_all(&mut *conn)
    .await
    .unwrap_or_default();

    // Foreign keys, unique, check and exclusion constraints in one read.
    // The primary key is left out — `keys` has it in key order — and so
    // is a domain's check, which belongs to no table. The column arrays
    // are walked `WITH ORDINALITY` so a two-column key keeps its order.
    //
    // A key that references a partitioned table is cloned once for every
    // partition it reaches, on the same referencing table; those clones
    // carry a parent on that same table, and `\d` hides them too.
    // `conparentid` is PostgreSQL 11's, and a server before it has no
    // clones to hide.
    let clones = if version >= 110_000 {
        "AND NOT EXISTS (SELECT 1 FROM pg_constraint parent \
                          WHERE parent.oid = con.conparentid \
                            AND parent.conrelid = con.conrelid)"
    } else {
        ""
    };
    let constraints: Vec<ConstraintRow> = sqlx::query_as(&format!(
        "SELECT n.nspname, c.relname, con.conname, con.contype::text, \
                pg_get_constraintdef(con.oid, true), \
                ARRAY(SELECT a.attname::text \
                        FROM unnest(con.conkey) WITH ORDINALITY AS k(num, ord) \
                        JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.num \
                       ORDER BY k.ord), \
                fn.nspname::text, fc.relname::text, \
                ARRAY(SELECT a.attname::text \
                        FROM unnest(con.confkey) WITH ORDINALITY AS k(num, ord) \
                        JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.num \
                       ORDER BY k.ord) \
         FROM pg_constraint con \
         JOIN pg_class c ON c.oid = con.conrelid \
         JOIN pg_namespace n ON n.oid = c.relnamespace \
         LEFT JOIN pg_class fc ON fc.oid = con.confrelid \
         LEFT JOIN pg_namespace fn ON fn.oid = fc.relnamespace \
         WHERE con.contype IN ('f', 'u', 'c', 'x') {clones} \
           AND n.nspname NOT IN ('pg_catalog', 'information_schema') \
           AND n.nspname NOT LIKE 'pg_toast%' \
           AND n.nspname NOT LIKE 'pg_temp%' \
         ORDER BY n.nspname, c.relname, con.conname",
    ))
    .fetch_all(&mut *conn)
    .await
    .unwrap_or_default();

    // `tgisinternal` is what a foreign key's own enforcement triggers
    // carry; they are the key, not something the user wrote.
    let triggers: Vec<TriggerRow> = sqlx::query_as(
        "SELECT n.nspname, c.relname, t.tgname, t.tgtype::int, t.tgfoid::regproc::text \
         FROM pg_trigger t \
         JOIN pg_class c ON c.oid = t.tgrelid \
         JOIN pg_namespace n ON n.oid = c.relnamespace \
         WHERE NOT t.tgisinternal \
           AND n.nspname NOT IN ('pg_catalog', 'information_schema') \
           AND n.nspname NOT LIKE 'pg_toast%' \
           AND n.nspname NOT LIKE 'pg_temp%' \
         ORDER BY n.nspname, c.relname, t.tgname",
    )
    .fetch_all(&mut *conn)
    .await
    .unwrap_or_default();

    // A sequence a column owns — a `serial`'s, an identity's — is how that
    // column counts, not something the user made beside it. Listing it
    // would also have every `CREATE TABLE … (id serial)` report a new
    // sequence beside its new table. `pg_sequence` is PostgreSQL 10's.
    let sequences: Vec<SequenceRow> = if version >= 100_000 {
        sqlx::query_as(
            "SELECT n.nspname, c.relname, format_type(s.seqtypid, NULL) \
             FROM pg_sequence s \
             JOIN pg_class c ON c.oid = s.seqrelid \
             JOIN pg_namespace n ON n.oid = c.relnamespace \
             WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') \
               AND n.nspname NOT LIKE 'pg_toast%' \
               AND n.nspname NOT LIKE 'pg_temp%' \
               AND NOT EXISTS (SELECT 1 FROM pg_depend d \
                                WHERE d.classid = 'pg_class'::regclass \
                                  AND d.objid = c.oid AND d.refobjsubid > 0 \
                                  AND d.deptype IN ('a', 'i')) \
             ORDER BY n.nspname, c.relname",
        )
        .fetch_all(&mut *conn)
        .await
        .unwrap_or_default()
    } else {
        Vec::new()
    };

    // `prokind` is PostgreSQL 11's. The routines a `CREATE TYPE … AS RANGE`
    // makes for itself depend on the type (`deptype = 'i'`) and are the
    // type's, not the user's, so they are skipped with the extensions'.
    let routines: Vec<RoutineRow> = if version < 110_000 {
        Vec::new()
    } else {
        sqlx::query_as(
            "SELECT n.nspname, p.proname, p.prokind::text, \
                pg_get_function_identity_arguments(p.oid), \
                CASE WHEN p.prokind = 'p' THEN NULL ELSE pg_get_function_result(p.oid) END \
         FROM pg_proc p \
         JOIN pg_namespace n ON n.oid = p.pronamespace \
         WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') \
           AND n.nspname NOT LIKE 'pg_toast%' \
           AND n.nspname NOT LIKE 'pg_temp%' \
           AND NOT EXISTS (SELECT 1 FROM pg_depend d \
                            WHERE d.classid = 'pg_proc'::regclass \
                              AND d.objid = p.oid AND d.deptype IN ('e', 'i')) \
         ORDER BY n.nspname, p.proname, 4",
        )
        .fetch_all(&mut *conn)
        .await
        .unwrap_or_default()
    };

    // Every table has a composite type of its own; only a composite made
    // with `CREATE TYPE … AS (…)` has a `pg_class` row of kind `c`.
    let types: Vec<TypeRow> = sqlx::query_as(
        "SELECT n.nspname, t.typname, t.typtype::text, \
                CASE t.typtype \
                  WHEN 'e' THEN (SELECT string_agg(e.enumlabel, ', ' ORDER BY e.enumsortorder) \
                                   FROM pg_enum e WHERE e.enumtypid = t.oid) \
                  WHEN 'd' THEN format_type(t.typbasetype, t.typtypmod) \
                  WHEN 'r' THEN (SELECT format_type(r.rngsubtype, NULL) \
                                   FROM pg_range r WHERE r.rngtypid = t.oid) \
                  WHEN 'c' THEN (SELECT string_agg(a.attname || ' ' \
                                                   || format_type(a.atttypid, a.atttypmod), \
                                                   ', ' ORDER BY a.attnum) \
                                   FROM pg_attribute a \
                                  WHERE a.attrelid = t.typrelid \
                                    AND a.attnum > 0 AND NOT a.attisdropped) \
                END \
         FROM pg_type t \
         JOIN pg_namespace n ON n.oid = t.typnamespace \
         WHERE t.typtype IN ('e', 'd', 'r', 'c') \
           AND (t.typtype <> 'c' \
                OR (SELECT c.relkind FROM pg_class c WHERE c.oid = t.typrelid) = 'c') \
           AND n.nspname NOT IN ('pg_catalog', 'information_schema') \
           AND n.nspname NOT LIKE 'pg_toast%' \
           AND n.nspname NOT LIKE 'pg_temp%' \
           AND NOT EXISTS (SELECT 1 FROM pg_depend d \
                            WHERE d.classid = 'pg_type'::regclass \
                              AND d.objid = t.oid AND d.deptype = 'e') \
         ORDER BY n.nspname, t.typname",
    )
    .fetch_all(&mut *conn)
    .await
    .unwrap_or_default();

    Ok(build_catalog(CatalogRows {
        tables,
        columns,
        keys,
        indexes,
        constraints,
        triggers,
        sequences,
        routines,
        types,
    }))
}

/// Everything `read_catalog` read, before it is put into a tree.
struct CatalogRows {
    tables: Vec<TableRow>,
    columns: Vec<ColumnRow>,
    keys: Vec<KeyRow>,
    indexes: Vec<IndexRow>,
    constraints: Vec<ConstraintRow>,
    triggers: Vec<TriggerRow>,
    sequences: Vec<SequenceRow>,
    routines: Vec<RoutineRow>,
    types: Vec<TypeRow>,
}

/// Put the rows into schemas. A schema is listed when anything at all
/// sits in it, so a schema of nothing but functions has a place in the
/// tree too. Every read is ordered by schema name, and a `BTreeMap` keeps
/// that order: `name` compares bytewise, as a Rust string does.
///
/// Every per-table list is grouped by its table once, up front, so a
/// catalog of thousands of tables is put together in one pass over each
/// list rather than one pass per table.
fn build_catalog(rows: CatalogRows) -> Catalog {
    let mut schemas: BTreeMap<String, Schema> = BTreeMap::new();
    fn schema<'a>(schemas: &'a mut BTreeMap<String, Schema>, name: &str) -> &'a mut Schema {
        schemas.entry(name.to_string()).or_insert_with(|| Schema {
            name: name.to_string(),
            ..Default::default()
        })
    }

    let mut columns = by_table(
        rows.columns,
        |(s, t, name, data_type, nullable, default)| {
            (
                (s, t),
                Column {
                    name,
                    data_type,
                    nullable,
                    default,
                },
            )
        },
    );
    let mut keys = by_table(rows.keys, |(s, t, column)| ((s, t), column));
    let mut indexes = by_table(rows.indexes, |(s, t, name, definition, unique, primary)| {
        (
            (s, t),
            Index {
                name,
                definition,
                unique,
                primary,
            },
        )
    });
    let mut foreign_keys = HashMap::new();
    let mut constraints = HashMap::new();
    for (s, t, name, kind, definition, key, target_schema, target_table, target_key) in
        rows.constraints
    {
        let kind = match kind.as_str() {
            "f" => {
                foreign_keys
                    .entry((s, t))
                    .or_insert_with(Vec::new)
                    .push(ForeignKey {
                        name,
                        columns: key,
                        target_schema: target_schema.unwrap_or_default(),
                        target_table: target_table.unwrap_or_default(),
                        target_columns: target_key,
                    });
                continue;
            }
            "u" => ConstraintKind::Unique,
            "c" => ConstraintKind::Check,
            "x" => ConstraintKind::Exclusion,
            _ => continue,
        };
        constraints
            .entry((s, t))
            .or_insert_with(Vec::new)
            .push(Constraint {
                name,
                kind,
                definition,
            });
    }
    let mut triggers = by_table(rows.triggers, |(s, t, name, tgtype, function)| {
        (
            (s, t),
            Trigger {
                name,
                timing: trigger_timing(tgtype),
                function: format!("{function}()"),
            },
        )
    });

    for (schema_name, table_name, relkind, reltuples) in rows.tables {
        let key = (schema_name, table_name);
        let table = Table {
            name: key.1.clone(),
            kind: match relkind.as_str() {
                // Views and materialized views read the same to a viewer.
                "v" | "m" => TableKind::View,
                _ => TableKind::Table,
            },
            columns: columns.remove(&key).unwrap_or_default(),
            primary_key: keys.remove(&key).unwrap_or_default(),
            // `reltuples` is -1 when the table was never analyzed. Report
            // nothing rather than a zero the user would read as "empty".
            approx_rows: reltuples.filter(|n| *n >= 0).map(|n| n as u64),
            indexes: indexes.remove(&key).unwrap_or_default(),
            foreign_keys: foreign_keys.remove(&key).unwrap_or_default(),
            constraints: constraints.remove(&key).unwrap_or_default(),
            triggers: triggers.remove(&key).unwrap_or_default(),
        };
        schema(&mut schemas, &key.0).tables.push(table);
    }
    for (schema_name, name, data_type) in rows.sequences {
        schema(&mut schemas, &schema_name)
            .sequences
            .push(Sequence { name, data_type });
    }
    for (schema_name, name, kind, arguments, returns) in rows.routines {
        schema(&mut schemas, &schema_name).routines.push(Routine {
            name,
            kind: match kind.as_str() {
                "p" => RoutineKind::Procedure,
                "a" => RoutineKind::Aggregate,
                "w" => RoutineKind::Window,
                _ => RoutineKind::Function,
            },
            arguments,
            returns,
        });
    }
    for (schema_name, name, kind, definition) in rows.types {
        let kind = match kind.as_str() {
            "e" => TypeKind::Enum,
            "d" => TypeKind::Domain,
            "r" => TypeKind::Range,
            _ => TypeKind::Composite,
        };
        schema(&mut schemas, &schema_name).types.push(UserType {
            name,
            kind,
            definition: definition.unwrap_or_default(),
        });
    }
    Catalog {
        schemas: schemas.into_values().collect(),
    }
}

/// Group one listing by the table each row belongs to, keeping the order
/// the listing had within each table. `split` names the table and makes
/// the model value out of the rest of the row.
fn by_table<R, T>(
    rows: Vec<R>,
    split: impl Fn(R) -> ((String, String), T),
) -> HashMap<(String, String), Vec<T>> {
    let mut grouped: HashMap<(String, String), Vec<T>> = HashMap::new();
    for row in rows {
        let (key, value) = split(row);
        grouped.entry(key).or_default().push(value);
    }
    grouped
}

/// When a trigger fires, read off `pg_trigger.tgtype`, whose bits are
/// fixed in `pg_trigger.h`: `before insert or update`, and `· statement`
/// for one that fires once per statement rather than once per row.
fn trigger_timing(tgtype: i32) -> String {
    const ROW: i32 = 1 << 0;
    const BEFORE: i32 = 1 << 1;
    const INSERT: i32 = 1 << 2;
    const DELETE: i32 = 1 << 3;
    const UPDATE: i32 = 1 << 4;
    const TRUNCATE: i32 = 1 << 5;
    const INSTEAD: i32 = 1 << 6;

    let when = if tgtype & INSTEAD != 0 {
        "instead of"
    } else if tgtype & BEFORE != 0 {
        "before"
    } else {
        "after"
    };
    let events: Vec<&str> = [
        (INSERT, "insert"),
        (UPDATE, "update"),
        (DELETE, "delete"),
        (TRUNCATE, "truncate"),
    ]
    .into_iter()
    .filter(|(bit, _)| tgtype & bit != 0)
    .map(|(_, event)| event)
    .collect();
    let mut timing = format!("{when} {}", events.join(" or "));
    if tgtype & ROW == 0 {
        timing.push_str(" · statement");
    }
    timing
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
        // Everything sqlx has no decoder for: read the wire bytes.
        _ => off_the_wire(row.try_get_raw(i)?, &type_name)?,
    })
}

/// Render a value sqlx cannot decode, from the bytes the server sent.
///
/// sqlx knows the built-in types by OID and calls everything else by the
/// name it reads back from the catalog — `xid8`, `tsvector`. There is no
/// `Decode` for those, so `try_get` on any Rust type fails, and the old
/// fallback painted `<xid8>` in every cell of the column: the type name
/// is the one thing about the value the user already knew.
///
/// The wire carries enough to do better. A statement sent unprepared
/// comes back in **text** format, which is Postgres's own rendering of
/// the value and is right for every type there will ever be. A prepared
/// statement — which is the path a run takes — comes back in **binary**,
/// where each type is its own layout and nothing generic can be said, so
/// the ones worth reading are decoded by hand and the rest keep the type
/// name they had.
///
/// An **array** is one of those. sqlx decodes `text[]` into a `Vec`, but
/// only of one dimension and only of a Rust type named up front, so every
/// array went to the placeholder and a `text[]` column painted `<TEXT[]>`
/// in every cell. See [`array`].
fn off_the_wire(raw: PgValueRef<'_>, type_name: &str) -> Result<Value> {
    let format = raw.format();
    let type_info = raw.type_info();
    let bytes = raw.as_bytes().map_err(|error| anyhow::anyhow!("{error}"))?;
    if let PgValueFormat::Text = format {
        return Ok(Value::Text(String::from_utf8_lossy(bytes).into_owned()));
    }
    // A domain over an array is sent as the array it is.
    let mut array_type = type_info.as_ref();
    while let PgTypeKind::Domain(base) = array_type.kind() {
        array_type = base;
    }
    if let PgTypeKind::Array(element_type) = array_type.kind() {
        let items = array_wire(bytes)?;
        return Ok(Value::Text(if is_vector(array_type.name()) {
            vector(&items, element_type)
        } else {
            array(&items, element_type)
        }));
    }
    // The name comes from the catalog, so it arrives as the server spells
    // it; the built-in names sqlx uses are upper case.
    Ok(match type_name.to_ascii_lowercase().as_str() {
        "xid" => transaction_id(u32::from_be_bytes(fixed(bytes)?) as u64),
        "xid8" => transaction_id(u64::from_be_bytes(fixed(bytes)?)),
        "tsvector" => Value::Text(tsvector(bytes)?),
        _ => Value::Text(format!("<{type_name}>")),
    })
}

/// Postgres's own limit on an array's dimensions, `MAXDIM`. A header that
/// claims more is not an array the server wrote.
const MAX_ARRAY_DIMENSIONS: usize = 6;

/// An array as it crossed the wire: the length and the lower bound of
/// each dimension, and every element in row-major order, `None` for a
/// NULL.
struct ArrayWire<'a> {
    lengths: Vec<usize>,
    lowers: Vec<i32>,
    items: Vec<Option<&'a [u8]>>,
}

/// Split an array's binary form, as `array_send` writes it, into its
/// parts.
///
/// The wire form is a header — the number of dimensions, a has-nulls
/// flag, the element's OID, then a length and a lower bound for each
/// dimension — followed by every element, each a length (-1 for NULL)
/// and that many bytes. An empty array has no dimensions at all, so a
/// dimension of length zero is refused: the server never sends one, and
/// `[i32::MAX, 0]` would otherwise be two billion brace pairs around
/// nothing. Every element costs at least its four-byte length, which is
/// what bounds the count by the bytes that are actually there.
fn array_wire(bytes: &[u8]) -> Result<ArrayWire<'_>> {
    let mut wire = WireBytes(bytes);
    let dimensions = usize::try_from(wire.i32()?)
        .ok()
        .filter(|n| *n <= MAX_ARRAY_DIMENSIONS)
        .ok_or_else(|| anyhow::anyhow!("array has an impossible number of dimensions"))?;
    wire.take(4)?; // has nulls: every element says so itself
    wire.take(4)?; // the element's OID: sqlx already resolved the type

    let mut lengths = Vec::with_capacity(dimensions);
    let mut lowers = Vec::with_capacity(dimensions);
    for _ in 0..dimensions {
        let length = usize::try_from(wire.i32()?)
            .ok()
            .filter(|length| *length > 0)
            .ok_or_else(|| anyhow::anyhow!("array dimension has no length"))?;
        lengths.push(length);
        lowers.push(wire.i32()?);
    }
    let count = if dimensions == 0 {
        0
    } else {
        lengths
            .iter()
            .try_fold(1usize, |count, length| count.checked_mul(*length))
            .filter(|count| *count <= wire.0.len() / 4)
            .ok_or_else(|| anyhow::anyhow!("array promises more elements than it holds"))?
    };

    let mut items = Vec::with_capacity(count);
    for _ in 0..count {
        let length = wire.i32()?;
        items.push(if length == -1 {
            None
        } else {
            let length = usize::try_from(length)
                .map_err(|_| anyhow::anyhow!("array element has a negative length"))?;
            Some(wire.take(length)?)
        });
    }
    if !wire.0.is_empty() {
        anyhow::bail!("array has bytes past its last element");
    }
    Ok(ArrayWire {
        lengths,
        lowers,
        items,
    })
}

/// An array rendered the way `array_out` renders it: `{a,"b c",NULL}`,
/// `{{1,2},{3,4}}`, and `[0:1]={1,2}` when a dimension does not start
/// at 1.
///
/// The text form is the target because it is what the same statement
/// paints when it comes back unprepared, and a cell must not read two
/// ways depending on how the statement was sent. So a `bool[]` reads
/// `{t,f}`, as `psql` shows it, and not the way the scalar `true` does.
///
/// Each element is read by the type sqlx resolved for the column, through
/// [`element`]. One it cannot read keeps the type's name, so an array of
/// an exotic type still shows its shape — and a value this reader gets
/// wrong costs its own element, not the whole result: an error here would
/// fail every row of the run.
fn array(array: &ArrayWire<'_>, element_type: &PgTypeInfo) -> String {
    let mut out = String::new();
    if array.lowers.iter().any(|lower| *lower != 1) {
        for (lower, length) in array.lowers.iter().zip(&array.lengths) {
            let upper = i64::from(*lower) + *length as i64 - 1;
            out.push_str(&format!("[{lower}:{upper}]"));
        }
        out.push('=');
    }
    if array.lengths.is_empty() {
        out.push_str("{}");
    } else {
        array_level(
            &mut out,
            &array.lengths,
            &mut array.items.iter(),
            element_type,
        );
    }
    out
}

/// One level of braces: the elements themselves at the innermost
/// dimension, and a brace group per entry of every dimension outside it.
fn array_level<'a>(
    out: &mut String,
    lengths: &[usize],
    items: &mut impl Iterator<Item = &'a Option<&'a [u8]>>,
    element_type: &PgTypeInfo,
) {
    out.push('{');
    for n in 0..lengths[0] {
        if n > 0 {
            out.push(',');
        }
        if lengths.len() > 1 {
            array_level(out, &lengths[1..], items, element_type);
            continue;
        }
        match items.next() {
            Some(Some(bytes)) => array_item(out, &element_or_name(bytes, element_type)),
            _ => out.push_str("NULL"),
        }
    }
    out.push('}');
}

/// `int2vector` and `oidvector` are arrays on the wire — the catalog
/// files them as such, so sqlx calls them one — but their own output
/// functions print them space-separated with no braces: `1 2`, as
/// `pg_index.indkey` reads in `psql`.
fn is_vector(type_name: &str) -> bool {
    type_name.eq_ignore_ascii_case("int2vector") || type_name.eq_ignore_ascii_case("oidvector")
}

fn vector(array: &ArrayWire<'_>, element_type: &PgTypeInfo) -> String {
    array
        .items
        .iter()
        .map(|item| match item {
            Some(bytes) => element_or_name(bytes, element_type),
            None => "NULL".to_string(),
        })
        .collect::<Vec<_>>()
        .join(" ")
}

/// One element, quoted by `array_out`'s rule: an empty string, the word
/// `NULL` in any case, and anything holding a brace, a comma, a quote, a
/// backslash or white space goes in quotes, with a quote or a backslash
/// inside it escaped by a backslash. Otherwise `{"a,b"}` would read as
/// two elements and `{"NULL"}` as none.
fn array_item(out: &mut String, item: &str) {
    let quoted = item.is_empty()
        || item.eq_ignore_ascii_case("null")
        || item.chars().any(|character| {
            matches!(
                character,
                '{' | '}' | ',' | '"' | '\\' | ' ' | '\t' | '\n' | '\r' | '\x0b' | '\x0c'
            )
        });
    if !quoted {
        out.push_str(item);
        return;
    }
    out.push('"');
    for character in item.chars() {
        if character == '"' || character == '\\' {
            out.push('\\');
        }
        out.push(character);
    }
    out.push('"');
}

fn element_or_name(bytes: &[u8], type_info: &PgTypeInfo) -> String {
    element(bytes, type_info).unwrap_or_else(|_| format!("<{}>", type_info.name()))
}

/// One element of an array, from its binary form, as its type's own
/// output function prints it.
///
/// The scalar path has sqlx to decode a value, but an element has no
/// `PgValueRef` of its own to hand sqlx, so the common types are read by
/// hand here. The name is matched the way [`decode`] matches it — sqlx's
/// upper-case names for the built-ins — and the kind covers what has no
/// fixed name: an enum's element is its label, and a domain is its base
/// type.
fn element(bytes: &[u8], type_info: &PgTypeInfo) -> Result<String> {
    match type_info.kind() {
        PgTypeKind::Enum(_) => return Ok(String::from_utf8_lossy(bytes).into_owned()),
        PgTypeKind::Domain(base) => return element(bytes, base),
        _ => {}
    }
    let name = type_info.name();
    Ok(match name.to_ascii_uppercase().as_str() {
        "BOOL" => match bytes {
            [0] => "f".to_string(),
            [_] => "t".to_string(),
            _ => anyhow::bail!("expected 1 byte on the wire, got {}", bytes.len()),
        },
        "INT2" => i16::from_be_bytes(fixed(bytes)?).to_string(),
        "INT4" => i32::from_be_bytes(fixed(bytes)?).to_string(),
        "INT8" => i64::from_be_bytes(fixed(bytes)?).to_string(),
        "OID" | "XID" => u32::from_be_bytes(fixed(bytes)?).to_string(),
        "XID8" => u64::from_be_bytes(fixed(bytes)?).to_string(),
        "FLOAT4" => {
            let value = f32::from_be_bytes(fixed(bytes)?);
            float(value.into(), format!("{value:e}"), value.to_string(), 6)
        }
        "FLOAT8" => {
            let value = f64::from_be_bytes(fixed(bytes)?);
            float(value, format!("{value:e}"), value.to_string(), 15)
        }
        "NUMERIC" => numeric(bytes)?,
        // `"char"`, the one-byte type, is the name sqlx spells with its
        // quotes; `CHAR` is `bpchar`.
        "TEXT" | "VARCHAR" | "BPCHAR" | "CHAR" | "\"CHAR\"" | "NAME" | "CITEXT" | "UNKNOWN"
        | "JSON" | "XML" => String::from_utf8_lossy(bytes).into_owned(),
        // `jsonb` leads with a format version, which is 1.
        "JSONB" => match bytes.split_first() {
            Some((1, json)) => String::from_utf8_lossy(json).into_owned(),
            _ => anyhow::bail!("unknown jsonb format"),
        },
        "UUID" => sqlx::types::Uuid::from_slice(bytes)?.to_string(),
        "BYTEA" => {
            let mut hex = String::from("\\x");
            for byte in bytes {
                hex.push_str(&format!("{byte:02x}"));
            }
            hex
        }
        "DATE" => match i32::from_be_bytes(fixed(bytes)?) {
            i32::MAX => "infinity".to_string(),
            i32::MIN => "-infinity".to_string(),
            days => {
                let (date, era) = calendar_date(days.into());
                format!("{date}{era}")
            }
        },
        "TIME" => clock(i64::from_be_bytes(fixed(bytes)?))?,
        // `timestamptz` is stored in UTC and sqlx decodes the scalar as
        // UTC, so the element reads the same way, offset and all.
        "TIMESTAMP" | "TIMESTAMPTZ" => match i64::from_be_bytes(fixed(bytes)?) {
            i64::MAX => "infinity".to_string(),
            i64::MIN => "-infinity".to_string(),
            micros => {
                let (date, era) = calendar_date(micros.div_euclid(MICROS_PER_DAY));
                let time = clock(micros.rem_euclid(MICROS_PER_DAY))?;
                let offset = if name.eq_ignore_ascii_case("TIMESTAMPTZ") {
                    " +00:00"
                } else {
                    ""
                };
                format!("{date} {time}{offset}{era}")
            }
        },
        _ => format!("<{name}>"),
    })
}

const MICROS_PER_DAY: i64 = 86_400_000_000;

/// The date `days` after 2000-01-01 — day zero of every Postgres date and
/// timestamp on the wire — as Postgres prints it: the year in at least
/// four digits, and the era apart, because there is no year zero and a
/// year before 1 AD prints as `0001 … BC`.
///
/// The arithmetic is Howard Hinnant's `civil_from_days`, by hand rather
/// than through `time`: Postgres dates run to 5874897 AD and `time`
/// stops at 9999, so a date the server is happy to hold would come back
/// as an error.
fn calendar_date(days: i64) -> (String, &'static str) {
    // Days from 0000-03-01, the start of the proleptic cycle.
    let z = days + 10_957 + 719_468;
    let era = z.div_euclid(146_097);
    let day_of_era = z.rem_euclid(146_097);
    let year_of_era =
        (day_of_era - day_of_era / 1460 + day_of_era / 36_524 - day_of_era / 146_096) / 365;
    let day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let month_index = (5 * day_of_year + 2) / 153;
    let day = day_of_year - (153 * month_index + 2) / 5 + 1;
    let month = if month_index < 10 {
        month_index + 3
    } else {
        month_index - 9
    };
    let year = year_of_era + era * 400 + i64::from(month <= 2);
    if year <= 0 {
        (format!("{:04}-{month:02}-{day:02}", 1 - year), " BC")
    } else {
        (format!("{year:04}-{month:02}-{day:02}"), "")
    }
}

/// The time of day `micros` after midnight, the way `format_time` writes
/// the scalar: seconds, and the microseconds only when there are some.
/// `24:00:00` is a time Postgres accepts, so the end of the day is one.
fn clock(micros: i64) -> Result<String> {
    if !(0..=MICROS_PER_DAY).contains(&micros) {
        anyhow::bail!("time of day out of range");
    }
    let seconds = micros / 1_000_000;
    let (hour, minute, second) = (seconds / 3600, seconds / 60 % 60, seconds % 60);
    Ok(match micros % 1_000_000 {
        0 => format!("{hour:02}:{minute:02}:{second:02}"),
        micro => format!("{hour:02}:{minute:02}:{second:02}.{micro:06}"),
    })
}

/// A float the way Postgres prints it: the shortest digits that read
/// back as the same value, plain while the exponent is from -4 to one
/// below the type's digits (6 for `float4`, 15 for `float8`) and written
/// as `1e+15` outside that. `plain` and `scientific` are Rust's own
/// shortest renderings, taken from the value's own type, since a
/// `float4` widened to `f64` first would print as `0.10000000149011612`.
fn float(value: f64, scientific: String, plain: String, digits: i32) -> String {
    if value.is_nan() {
        return "NaN".to_string();
    }
    if value.is_infinite() {
        return if value > 0.0 { "Infinity" } else { "-Infinity" }.to_string();
    }
    let Some((mantissa, exponent)) = scientific.split_once('e') else {
        return plain;
    };
    let Ok(exponent) = exponent.parse::<i32>() else {
        return plain;
    };
    if (-4..digits).contains(&exponent) {
        plain
    } else {
        let sign = if exponent < 0 { '-' } else { '+' };
        format!("{mantissa}e{sign}{:02}", exponent.abs())
    }
}

/// `numeric`'s binary form, rendered the way `numeric_out` renders it.
///
/// The wire form is a digit count, a weight, a sign and a display scale,
/// then the digits in base 10000, the first one standing for
/// 10000^weight. A digit past the ones sent is zero. The display scale is
/// what keeps `1280.00` from reading as `1280`: the scalar path loses it
/// to `BigDecimal`, but an element has no need to.
fn numeric(bytes: &[u8]) -> Result<String> {
    let mut wire = WireBytes(bytes);
    let count = wire.u16()? as usize;
    let weight = i32::from(wire.u16()? as i16);
    let sign = wire.u16()?;
    let scale = wire.u16()? as usize;
    let digits = (0..count).map(|_| wire.u16()).collect::<Result<Vec<_>>>()?;
    if !wire.0.is_empty() {
        anyhow::bail!("numeric has bytes past its last digit");
    }
    let negative = match sign {
        0x0000 => false,
        0x4000 => true,
        0xC000 => return Ok("NaN".to_string()),
        0xD000 => return Ok("Infinity".to_string()),
        0xF000 => return Ok("-Infinity".to_string()),
        _ => anyhow::bail!("numeric has an unknown sign"),
    };
    let digit = |k: i32| {
        usize::try_from(k)
            .ok()
            .and_then(|k| digits.get(k).copied())
            .unwrap_or(0)
    };

    let mut out = String::new();
    if negative {
        out.push('-');
    }
    if weight < 0 {
        out.push('0');
    } else {
        out.push_str(&digit(0).to_string());
        for k in 1..=weight {
            out.push_str(&format!("{:04}", digit(k)));
        }
    }
    if scale > 0 {
        let mut fraction = String::new();
        let mut k = weight + 1;
        while fraction.len() < scale {
            fraction.push_str(&format!("{:04}", digit(k)));
            k += 1;
        }
        fraction.truncate(scale);
        out.push('.');
        out.push_str(&fraction);
    }
    Ok(out)
}

/// A cursor over the bytes of one value that fails, rather than panics,
/// when a count promises more than is there.
struct WireBytes<'a>(&'a [u8]);

impl<'a> WireBytes<'a> {
    fn take(&mut self, n: usize) -> Result<&'a [u8]> {
        if self.0.len() < n {
            anyhow::bail!("value ended early on the wire");
        }
        let (head, rest) = self.0.split_at(n);
        self.0 = rest;
        Ok(head)
    }

    fn i32(&mut self) -> Result<i32> {
        Ok(i32::from_be_bytes(fixed(self.take(4)?)?))
    }

    fn u16(&mut self) -> Result<u16> {
        Ok(u16::from_be_bytes(fixed(self.take(2)?)?))
    }
}

fn fixed<const N: usize>(bytes: &[u8]) -> Result<[u8; N]> {
    bytes
        .try_into()
        .map_err(|_| anyhow::anyhow!("expected {N} bytes on the wire, got {}", bytes.len()))
}

/// A transaction id is 64 bits *unsigned*, so a value past `i64::MAX` has
/// no `Value::Int` to go in. No cluster has ever counted that far, but
/// rendering it as a negative number would be a lie rather than a limit,
/// so those digits go through as text.
fn transaction_id(id: u64) -> Value {
    i64::try_from(id)
        .map(Value::Int)
        .unwrap_or_else(|_| Value::Text(id.to_string()))
}

/// `tsvector`'s binary form, rendered the way `tsvector_out` renders it:
/// `'fox':3 'quick':2A`.
///
/// The wire form is a count of lexemes, then for each one the lexeme as a
/// NUL-terminated string, a count of positions, and that many `u16`s
/// holding the position in the low 14 bits and the weight in the top two.
/// Weight 3 prints as `A` down to 1 as `C`; 0 is the default and prints as
/// nothing. A quote or a backslash inside a lexeme is doubled, as
/// Postgres doubles it, or the rendering could not be read back.
fn tsvector(mut bytes: &[u8]) -> Result<String> {
    fn take<'a>(bytes: &mut &'a [u8], n: usize) -> Result<&'a [u8]> {
        if bytes.len() < n {
            anyhow::bail!("tsvector ended early");
        }
        let (head, rest) = bytes.split_at(n);
        *bytes = rest;
        Ok(head)
    }

    let lexemes = u32::from_be_bytes(fixed(take(&mut bytes, 4)?)?);
    let mut out = String::new();
    for _ in 0..lexemes {
        let end = bytes
            .iter()
            .position(|byte| *byte == 0)
            .ok_or_else(|| anyhow::anyhow!("tsvector lexeme is not terminated"))?;
        let lexeme = String::from_utf8_lossy(take(&mut bytes, end)?).into_owned();
        take(&mut bytes, 1)?;

        if !out.is_empty() {
            out.push(' ');
        }
        out.push('\'');
        for character in lexeme.chars() {
            if character == '\'' || character == '\\' {
                out.push(character);
            }
            out.push(character);
        }
        out.push('\'');

        let positions = u16::from_be_bytes(fixed(take(&mut bytes, 2)?)?);
        for n in 0..positions {
            let entry = u16::from_be_bytes(fixed(take(&mut bytes, 2)?)?);
            out.push(if n == 0 { ':' } else { ',' });
            out.push_str(&(entry & 0x3fff).to_string());
            match entry >> 14 {
                3 => out.push('A'),
                2 => out.push('B'),
                1 => out.push('C'),
                _ => {}
            }
        }
    }
    Ok(out)
}

// The `time` crate's `Display` output is not ISO-8601, and pulling in a
// format description would add a direct dependency for three lines, so
// build the ISO strings by hand.

fn format_date(date: sqlx::types::time::Date) -> String {
    format!(
        "{:04}-{:02}-{:02}",
        date.year(),
        date.month() as u8,
        date.day()
    )
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

    fn counted(calls: i64, exec_ms: f64) -> Counted {
        Counted {
            calls,
            exec_ms,
            plan_ms: 0.,
        }
    }

    #[test]
    fn the_servers_cursor_is_converted_to_a_byte_offset() {
        // One-based characters in, zero-based bytes out.
        assert_eq!(byte_offset("select 1", 1), Some(0));
        assert_eq!(byte_offset("select 1", 8), Some(7));
        // Past the last character is the end of the statement, which is
        // what "syntax error at end of input" reports.
        assert_eq!(byte_offset("select 1", 9), Some(8));
        assert_eq!(byte_offset("select 1", 10), None);
        // A position of zero is no position at all.
        assert_eq!(byte_offset("select 1", 0), None);
    }

    /// The whole reason the conversion exists: a multi-byte character
    /// ahead of the error moves the byte offset past the character count.
    #[test]
    fn a_multi_byte_character_moves_the_offset() {
        assert_eq!(byte_offset("select 'héllo', frm", 17), Some(17));
    }

    /// The case the whole delta exists for: one execution landed between
    /// the two readings, so its own time is knowable and is reported as a
    /// measurement rather than as an average.
    #[test]
    fn one_execution_between_two_readings_is_exact() {
        let timing = between(Some(counted(4, 40.)), counted(5, 52.5)).unwrap();
        assert_eq!(timing.exec_ms, 12.5);
        assert!(timing.exact);
    }

    /// `pg_stat_statements` is cluster-wide, so somebody else running the
    /// same statement lands in the same window. The mean over the window is
    /// still worth showing; calling it this run's time would not be.
    #[test]
    fn several_executions_in_the_window_are_a_mean() {
        let timing = between(Some(counted(4, 40.)), counted(7, 70.)).unwrap();
        assert_eq!(timing.exec_ms, 10.);
        assert!(!timing.exact);
    }

    /// The first run of a statement in a tab. Nothing to subtract from, so
    /// the answer is the mean over every execution the server has counted —
    /// and this reading is what makes the next run exact.
    #[test]
    fn the_first_reading_is_the_mean_so_far() {
        let timing = between(None, counted(10, 250.)).unwrap();
        assert_eq!(timing.exec_ms, 25.);
        assert!(!timing.exact);
    }

    /// Two readings with nothing between them describe no run at all. The
    /// server did not count the statement — it may not have finished being
    /// counted yet — and a stale total must not be shown as a fresh one.
    #[test]
    fn no_execution_between_the_readings_answers_nothing() {
        assert!(between(Some(counted(4, 40.)), counted(4, 40.)).is_none());
    }

    /// The view was reset, or the entry evicted under
    /// `pg_stat_statements.max`. There is no delta across that, so the
    /// reading is taken as a fresh baseline rather than subtracted into a
    /// negative time.
    #[test]
    fn a_reset_view_does_not_produce_a_negative_time() {
        let timing = between(Some(counted(900, 9000.)), counted(2, 30.)).unwrap();
        assert_eq!(timing.exec_ms, 15.);
        assert!(!timing.exact);
        assert!(between(Some(counted(900, 9000.)), counted(0, 0.)).is_none());
    }

    /// `total_plan_time` is `0` both when planning was free and when
    /// `pg_stat_statements.track_planning` is off, which is the default. A
    /// zero is dropped rather than reported as a measurement.
    #[test]
    fn planning_that_was_not_tracked_is_not_reported() {
        let untracked = between(Some(counted(1, 10.)), counted(2, 22.)).unwrap();
        assert_eq!(untracked.plan_ms, None);

        let tracked = between(
            Some(Counted {
                calls: 1,
                exec_ms: 10.,
                plan_ms: 1.,
            }),
            Counted {
                calls: 2,
                exec_ms: 22.,
                plan_ms: 1.5,
            },
        )
        .unwrap();
        assert_eq!(tracked.plan_ms, Some(0.5));
    }

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

        conn.execute("DROP SCHEMA IF EXISTS meerkat_test CASCADE")
            .await
            .unwrap();
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
        assert_eq!(
            result.rows[0][1],
            Value::Text("ada@example.com".to_string())
        );
        // Trailing zeroes drop: see the NUMERIC arm of `decode`.
        assert_eq!(result.rows[0][3], Value::Text("1280".to_string()));
        assert_eq!(
            result.rows[0][7],
            Value::Text("2026-08-15 09:12:00 +00:00".to_string())
        );
        assert_eq!(result.rows[0][4], Value::Bool(true));
        assert_eq!(result.rows[0][5], Value::Text("{\"a\":1}".to_string()));
        assert_eq!(
            result.rows[0][6],
            Value::Text("00000000-0000-0000-0000-000000000001".to_string())
        );
        assert_eq!(result.rows[0][8], Value::Text("2026-08-15".to_string()));
        assert_eq!(result.rows[1][2], Value::Null);
        assert_eq!(result.rows[1][3], Value::Null);

        conn.execute("DROP SCHEMA meerkat_test CASCADE")
            .await
            .unwrap();
    }

    /// Everything beside the columns: the indexes, keys, constraints and
    /// triggers of a table, and the sequences, routines and types of the
    /// schema — and nothing a key's own enforcement, a `serial` or a range
    /// type made for itself.
    #[tokio::test]
    async fn introspect_reads_every_object() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect_url(&url, false).await.unwrap();

        conn.execute("DROP SCHEMA IF EXISTS meerkat_objects CASCADE")
            .await
            .unwrap();
        for statement in [
            "CREATE SCHEMA meerkat_objects",
            "CREATE TYPE meerkat_objects.mood AS ENUM ('sad', 'ok', 'happy')",
            "CREATE DOMAIN meerkat_objects.price AS numeric(10, 2) CHECK (VALUE >= 0)",
            "CREATE TYPE meerkat_objects.pair AS (a integer, b text)",
            "CREATE SEQUENCE meerkat_objects.ticket AS integer",
            "CREATE TYPE meerkat_objects.span AS RANGE (subtype = integer)",
            "CREATE TABLE meerkat_objects.events (id serial PRIMARY KEY)",
            "CREATE TABLE meerkat_objects.parted (id integer PRIMARY KEY) PARTITION BY RANGE (id)",
            "CREATE TABLE meerkat_objects.parted_a PARTITION OF meerkat_objects.parted
                 FOR VALUES FROM (0) TO (10)",
            "CREATE TABLE meerkat_objects.parted_b PARTITION OF meerkat_objects.parted
                 FOR VALUES FROM (10) TO (20)",
            "CREATE TABLE meerkat_objects.pointer (id integer REFERENCES meerkat_objects.parted)",
            "CREATE TABLE meerkat_objects.users (id bigint PRIMARY KEY, email text UNIQUE)",
            "CREATE TABLE meerkat_objects.orders (
                 id bigint PRIMARY KEY,
                 user_id bigint REFERENCES meerkat_objects.users (id),
                 total meerkat_objects.price,
                 CONSTRAINT positive CHECK (id > 0)
             )",
            "CREATE INDEX orders_user ON meerkat_objects.orders (user_id)",
            "CREATE FUNCTION meerkat_objects.touch() RETURNS trigger LANGUAGE plpgsql
                 AS $$ BEGIN RETURN NEW; END $$",
            "CREATE FUNCTION meerkat_objects.double(x integer) RETURNS integer
                 LANGUAGE sql AS 'SELECT x * 2'",
            "CREATE PROCEDURE meerkat_objects.noop() LANGUAGE sql AS 'SELECT 1'",
            "CREATE TRIGGER touch BEFORE INSERT OR UPDATE ON meerkat_objects.orders
                 FOR EACH ROW EXECUTE FUNCTION meerkat_objects.touch()",
        ] {
            conn.execute(statement).await.unwrap();
        }

        let catalog = conn.introspect().await.unwrap();
        let schema = catalog
            .schemas
            .iter()
            .find(|s| s.name == "meerkat_objects")
            .expect("test schema is missing from the catalog");
        let orders = schema.tables.iter().find(|t| t.name == "orders").unwrap();

        let indexes: Vec<(&str, &str, bool)> = (orders.indexes.iter())
            .map(|i| (i.name.as_str(), i.definition.as_str(), i.primary))
            .collect();
        assert_eq!(
            indexes,
            [
                ("orders_pkey", "btree (id)", true),
                ("orders_user", "btree (user_id)", false),
            ]
        );

        assert_eq!(orders.foreign_keys.len(), 1);
        assert_eq!(
            orders.foreign_keys[0].detail(),
            "(user_id) → meerkat_objects.users(id)"
        );
        // The check is a constraint; the key and the primary key are not
        // listed a second time.
        assert_eq!(orders.constraints.len(), 1);
        assert_eq!(orders.constraints[0].kind, ConstraintKind::Check);
        assert_eq!(orders.constraints[0].definition, "CHECK (id > 0)");

        // The foreign key's enforcement triggers are internal and left out.
        // The range type's constructors are not listed as routines either.
        assert_eq!(orders.triggers.len(), 1);
        assert_eq!(orders.triggers[0].timing, "before insert or update");
        assert_eq!(orders.triggers[0].function, "meerkat_objects.touch()");

        // A key into a partitioned table is one key, not one per partition.
        let pointer = schema.tables.iter().find(|t| t.name == "pointer").unwrap();
        let keys: Vec<&str> = (pointer.foreign_keys.iter())
            .map(|k| k.name.as_str())
            .collect();
        assert_eq!(keys, ["pointer_id_fkey"]);

        let users = schema.tables.iter().find(|t| t.name == "users").unwrap();
        assert_eq!(users.constraints[0].kind, ConstraintKind::Unique);

        // The `serial` column's own sequence is the column's, not listed.
        let sequences: Vec<&str> = schema.sequences.iter().map(|s| s.name.as_str()).collect();
        assert_eq!(sequences, ["ticket"]);
        assert_eq!(schema.sequences[0].data_type, "integer");

        let routines: Vec<String> = schema.routines.iter().map(|r| r.detail()).collect();
        assert_eq!(
            routines,
            ["(x integer) → integer", "procedure · ()", "() → trigger"]
        );

        let types: Vec<String> = schema.types.iter().map(|t| t.detail()).collect();
        assert_eq!(
            types,
            [
                "enum · sad, ok, happy",
                "composite · a integer, b text",
                "domain · numeric(10,2)",
                "range · integer",
            ]
        );

        conn.execute("DROP SCHEMA meerkat_objects CASCADE")
            .await
            .unwrap();
    }

    #[test]
    fn a_trigger_says_when_it_fires() {
        // ROW | BEFORE | INSERT | UPDATE
        assert_eq!(trigger_timing(1 | 2 | 4 | 16), "before insert or update");
        // AFTER DELETE, once per statement.
        assert_eq!(trigger_timing(8), "after delete · statement");
        // INSTEAD OF INSERT on a view, per row.
        assert_eq!(trigger_timing(1 | 4 | 64), "instead of insert");
    }

    #[tokio::test]
    async fn unknown_types_do_not_fail_the_result() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        // `point` has no mapping in the driver; it must render as a
        // placeholder instead of failing the query.
        let result = conn
            .execute("SELECT point(1, 2) AS p, 1 AS n")
            .await
            .unwrap();
        assert_eq!(result.rows[0][0], Value::Text("<POINT>".to_string()));
        assert_eq!(result.rows[0][1], Value::Int(1));
    }

    /// The wire readers, against the server that writes those bytes: the
    /// layouts are documented rather than versioned, so a test over a
    /// fixture alone would go on passing if the reading were wrong.
    #[tokio::test]
    async fn transaction_ids_and_tsvectors_read_as_values() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        let result = conn
            .execute(
                "SELECT '42'::xid AS x,
                        '4294967300'::xid8 AS x8,
                        to_tsvector('english', 'The quick brown fox') AS v,
                        setweight(to_tsvector('english', 'quick fox'), 'A') AS w,
                        'a''b'::tsvector AS q",
            )
            .await
            .unwrap();
        assert_eq!(result.rows[0][0], Value::Int(42));
        assert_eq!(result.rows[0][1], Value::Int(4294967300));
        assert_eq!(
            result.rows[0][2],
            Value::Text("'brown':3 'fox':4 'quick':2".to_string())
        );
        // The weight is the top two bits of the position, so a wrong
        // reading would put the letter on the wrong lexeme or lose it.
        assert_eq!(
            result.rows[0][3],
            Value::Text("'fox':2A 'quick':1A".to_string())
        );
        assert_eq!(result.rows[0][4], Value::Text("'a''b'".to_string()));
    }

    /// An array comes back in binary on a prepared run, and sqlx has no
    /// reading of it that fits every array, so it used to paint
    /// `<TEXT[]>`. Each cell here must read the way `psql` prints it —
    /// braces, quotes and all — except that a date or a time inside one
    /// reads the way the same value reads in a cell of its own.
    #[tokio::test]
    async fn arrays_read_as_postgres_prints_them() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        let result = conn
            .execute(
                r#"SELECT ARRAY['a', 'b c', NULL, '', 'NULL', 'x,y', 'q"q', 'b\s']::text[],
                          '{}'::text[],
                          '{{1,2},{3,4}}'::int4[],
                          '[0:1]={7,8}'::int8[],
                          ARRAY[true, false],
                          ARRAY[1.50, -0.001, 12345678.9, 0]::numeric[],
                          ARRAY['NaN', 'Infinity']::float8[],
                          ARRAY['2024-02-29', 'infinity']::date[],
                          ARRAY['1999-12-31 23:59:59.5']::timestamp[],
                          ARRAY['2024-01-01 00:00:00+00']::timestamptz[],
                          ARRAY['24:00:00', '08:30:00']::time[],
                          ARRAY['a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11']::uuid[],
                          ARRAY['{"k": 1}']::jsonb[],
                          ARRAY['\x0102']::bytea[],
                          ARRAY['abc']::varchar[],
                          ARRAY[point(1, 2)],
                          ARRAY['10000-01-01', '0001-01-01 BC']::date[],
                          ARRAY['0001-01-01 00:00:00 BC']::timestamp[],
                          ARRAY[0.1, 1234567]::float4[],
                          ARRAY[1e300, 1e-05]::float8[],
                          '1 3'::int2vector"#,
            )
            .await
            .unwrap();
        let cells: Vec<Value> = [
            r#"{a,"b c",NULL,"","NULL","x,y","q\"q","b\\s"}"#,
            "{}",
            "{{1,2},{3,4}}",
            "[0:1]={7,8}",
            "{t,f}",
            "{1.50,-0.001,12345678.9,0}",
            "{NaN,Infinity}",
            "{2024-02-29,infinity}",
            r#"{"1999-12-31 23:59:59.500000"}"#,
            r#"{"2024-01-01 00:00:00 +00:00"}"#,
            "{24:00:00,08:30:00}",
            "{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}",
            r#"{"{\"k\": 1}"}"#,
            r#"{"\\x0102"}"#,
            "{abc}",
            "{<POINT>}",
            r#"{10000-01-01,"0001-01-01 BC"}"#,
            r#"{"0001-01-01 00:00:00 BC"}"#,
            "{0.1,1.234567e+06}",
            "{1e+300,1e-05}",
            "1 3",
        ]
        .into_iter()
        .map(|cell| Value::Text(cell.to_string()))
        .collect();
        assert_eq!(result.rows[0], cells);
    }

    /// An enum has no fixed name to match on, and a domain is its base
    /// type, so an array of either — and a domain over an array — is read
    /// through the type's kind.
    #[tokio::test]
    async fn arrays_of_enums_and_domains_read_their_elements() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect_url(&url, false).await.unwrap();
        let session = conn.open_session().await.unwrap();
        for statement in [
            "CREATE TYPE pg_temp.mood AS ENUM ('ok', 'not ok')",
            "CREATE DOMAIN pg_temp.small AS int4 CHECK (VALUE < 100)",
            "CREATE DOMAIN pg_temp.tags AS text[]",
        ] {
            session.execute(statement).await.unwrap();
        }

        let result = session
            .execute(
                "SELECT ARRAY['ok', 'not ok']::pg_temp.mood[],
                        ARRAY[1, 2]::pg_temp.small[],
                        ARRAY['a', 'b']::pg_temp.tags",
            )
            .await
            .unwrap();
        assert_eq!(
            result.rows[0],
            vec![
                Value::Text(r#"{ok,"not ok"}"#.to_string()),
                Value::Text("{1,2}".to_string()),
                Value::Text("{a,b}".to_string()),
            ]
        );
    }

    #[tokio::test]
    async fn empty_result_keeps_its_columns() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        let result = conn.execute("SELECT 1 AS x WHERE false").await.unwrap();
        assert!(result.rows.is_empty());
        assert_eq!(result.columns, vec!["x".to_string()]);
    }

    /// A statement that changes rows comes back as a count and no result
    /// set, and the count is the only thing it ever says. `fetch` threw the
    /// completion tag away, so such a statement answered with nothing at
    /// all — no rows, no columns and no number.
    #[tokio::test]
    async fn a_statement_that_changes_rows_reports_how_many() {
        let Some(url) = test_url() else { return };
        // Writable, because this writes.
        let conn = PostgresConnection::connect_url(&url, false).await.unwrap();
        let session = conn.open_session().await.unwrap();

        session
            .execute("CREATE TEMP TABLE meerkat_affected (id int)")
            .await
            .unwrap();
        let inserted = session
            .execute("INSERT INTO meerkat_affected VALUES (1), (2), (3)")
            .await
            .unwrap();
        assert_eq!(inserted.rows_affected, 3);
        // No result set: the columns are what the app reads to know there
        // is no grid to paint.
        assert!(inserted.columns.is_empty(), "{:?}", inserted.columns);

        let updated = session
            .execute("UPDATE meerkat_affected SET id = id + 1 WHERE id > 1")
            .await
            .unwrap();
        assert_eq!(updated.rows_affected, 2);

        // The reading that matters: the statement worked and matched
        // nothing, which is a different answer from an empty table.
        let matched_nothing = session
            .execute("DELETE FROM meerkat_affected WHERE id = 999")
            .await
            .unwrap();
        assert_eq!(matched_nothing.rows_affected, 0);
        assert!(matched_nothing.columns.is_empty());

        // A statement with a `RETURNING` clause is a result set as well as
        // a count, and it keeps both.
        let returning = session
            .execute("DELETE FROM meerkat_affected RETURNING id")
            .await
            .unwrap();
        assert_eq!(returning.rows_affected, 3);
        assert_eq!(returning.columns, vec!["id".to_string()]);
        assert_eq!(returning.rows.len(), 3);
    }

    /// A `SELECT` is counted by the server too — its tag is `SELECT 5` — so
    /// the count cannot be what says a statement changed anything. The
    /// columns are, which is why the app reads `rows_affected` only where
    /// there are none.
    #[tokio::test]
    async fn a_query_is_counted_as_well_and_keeps_its_columns() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        let result = conn
            .execute("SELECT * FROM generate_series(1, 5)")
            .await
            .unwrap();
        assert_eq!(result.rows_affected, 5);
        assert_eq!(result.columns.len(), 1);
    }

    /// The client's own split, against a real server. Every part of it has
    /// to be there for the app to say anything but a wall clock.
    #[tokio::test]
    async fn a_result_carries_where_its_time_went() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();
        let session = conn.open_session().await.unwrap();

        let wire = session
            .execute("SELECT * FROM generate_series(1, 20000)")
            .await
            .unwrap()
            .wire;
        assert!(
            wire.link_ms.is_some(),
            "the session did not time its round trip"
        );
        assert!(wire.first_row_ms.is_some(), "no row was timed");
        assert!(wire.fetch_ms.is_some(), "the fetch was not timed");

        // A statement with no rows has no fetch to measure, and `0` would
        // be a different claim from "there were none".
        let none = session.execute("SELECT 1 WHERE false").await.unwrap().wire;
        assert_eq!(none.first_row_ms, None);
        assert_eq!(none.fetch_ms, None);
    }

    /// The server's own figure, end to end: `pg_stat_activity.query_id`
    /// naming the statement, the join to `pg_stat_statements`, and the
    /// delta between two readings turning running totals into one run.
    ///
    /// It self-skips where the extension is not installed, which is the
    /// same answer the app gives there: the client's split still stands.
    #[tokio::test]
    async fn the_server_reports_what_it_spent() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();
        let session = conn.open_session().await.unwrap();

        session.execute("SELECT pg_sleep(0.2)").await.unwrap();
        let Some(first) = session.server_timing().await.unwrap() else {
            eprintln!("skipped: this server has no pg_stat_statements");
            return;
        };
        // No earlier reading, so this is the mean over every execution the
        // server has counted — and it must say so.
        assert!(!first.exact, "a first reading cannot be one run's own time");

        // With a baseline in hand, the next run of the same statement is
        // exact, and it is the sleep the statement asked for.
        session.execute("SELECT pg_sleep(0.2)").await.unwrap();
        let second = session
            .server_timing()
            .await
            .unwrap()
            .expect("no second reading");
        assert!(second.exact, "one run between two readings is exact");
        assert!(
            (150. ..600.).contains(&second.exec_ms),
            "a 200 ms sleep was reported as {} ms",
            second.exec_ms
        );
    }

    /// The whole of the checker: the server refuses the text and names
    /// the character it stopped at, and nothing runs.
    #[tokio::test]
    async fn a_syntax_error_comes_back_with_its_position() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        assert_eq!(conn.check("SELECT 1").await.unwrap(), None);

        let sql = "SELECT 1 FROM";
        let error = conn
            .check(sql)
            .await
            .unwrap()
            .expect("no error for an unfinished statement");
        assert!(error.message.contains("syntax error"), "{}", error.message);
        // "at end of input": one past the last character.
        assert_eq!(error.offset, Some(sql.len()));

        let sql = "SELECT 1 frm t";
        let error = conn
            .check(sql)
            .await
            .unwrap()
            .expect("no error for a broken statement");
        assert_eq!(&sql[error.offset.unwrap()..], "t");
    }

    /// A name the server cannot resolve comes back too, told apart from a
    /// syntax error because it is only true of the connection it was asked
    /// on.
    #[tokio::test]
    async fn an_unknown_name_is_reported_as_a_name() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        let sql = "SELECT * FROM public.no_such_table_here";
        let error = conn
            .check(sql)
            .await
            .unwrap()
            .expect("no error for a table that is not there");
        assert_eq!(error.refusal, Refusal::Name);
        assert_eq!(&sql[error.offset.unwrap()..], "public.no_such_table_here");

        // A column, a function and a type answer the same way.
        for sql in [
            "SELECT no_such_column FROM pg_class",
            "SELECT no_such_fn(1)",
            "SELECT 1::no_such_ty",
        ] {
            let error = conn
                .check(sql)
                .await
                .unwrap()
                .expect("{sql} was not refused");
            assert_eq!(error.refusal, Refusal::Name, "{sql}");
        }
    }

    /// **The statements `check` goes quiet about.** Postgres resolves no
    /// names for a utility statement until it runs one, so the parse says
    /// nothing and `to_regclass` is what answers instead.
    #[tokio::test]
    async fn a_utility_statement_parses_whatever_it_names() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        for sql in [
            "DROP TABLE public.no_such_table_here",
            "ALTER TABLE public.no_such_table_here ADD COLUMN c int",
            "TRUNCATE public.no_such_table_here",
        ] {
            assert_eq!(conn.check(sql).await.unwrap(), None, "{sql}");
        }
        assert_eq!(
            conn.relation_exists("public.no_such_table_here")
                .await
                .unwrap(),
            Some(false)
        );
    }

    /// `to_regclass` resolves a name the way the parser does, which is the
    /// whole reason it is the call rather than a query over `pg_class`.
    #[tokio::test]
    async fn the_server_says_whether_it_has_a_relation() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        assert_eq!(
            conn.relation_exists("pg_catalog.pg_class").await.unwrap(),
            Some(true)
        );
        // Unqualified, so it goes through `search_path`.
        assert_eq!(conn.relation_exists("pg_class").await.unwrap(), Some(true));
        // Unquoted parts fold to lower case, as SQL does.
        assert_eq!(conn.relation_exists("PG_CLASS").await.unwrap(), Some(true));
        // A quoted one does not, so this is a different name.
        assert_eq!(
            conn.relation_exists("\"PG_CLASS\"").await.unwrap(),
            Some(false)
        );
        assert_eq!(
            conn.relation_exists("public.no_such_table_here")
                .await
                .unwrap(),
            Some(false)
        );
        // A name that is not a name answers rather than raising.
        assert_eq!(conn.relation_exists("a b c").await.unwrap(), Some(false));
    }

    /// A bare `$1` is a placeholder the user typed on purpose, not a name
    /// they got wrong. The server refuses to infer its type, and that
    /// refusal is not a mark.
    #[tokio::test]
    async fn a_placeholder_is_not_an_error() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        assert_eq!(conn.check("SELECT $1").await.unwrap(), None);
    }

    /// **The reason a session checks for itself.** A temp table lives on
    /// one connection, so the pool cannot see it and the tab that made it
    /// can. Marking it as missing would be a warning about nothing.
    #[tokio::test]
    async fn a_session_resolves_what_it_made_itself() {
        let Some(url) = test_url() else { return };
        // Read-only refuses a temp table, so this one asks to write.
        let conn = PostgresConnection::connect_url(&url, false).await.unwrap();
        let session = conn.open_session().await.unwrap();
        session
            .execute("CREATE TEMP TABLE meerkat_check_probe (id int)")
            .await
            .unwrap();

        let sql = "SELECT * FROM meerkat_check_probe";
        assert_eq!(session.check(sql).await.unwrap(), None);
        // The pool is a different connection, and it cannot see it.
        let pooled = conn
            .check(sql)
            .await
            .unwrap()
            .expect("the pool saw a temp table");
        assert_eq!(pooled.refusal, Refusal::Name);

        // And the same split for the name a `DROP TABLE` would be marked
        // on, which is the whole reason the probe follows the session too.
        assert_eq!(
            session
                .relation_exists("meerkat_check_probe")
                .await
                .unwrap(),
            Some(true)
        );
        assert_eq!(
            conn.relation_exists("meerkat_check_probe").await.unwrap(),
            Some(false)
        );

        session.close().await;
    }

    /// Nothing is executed: the statement is prepared and thrown away.
    #[tokio::test]
    async fn checking_a_write_writes_nothing() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        assert_eq!(
            conn.check("DROP TABLE IF EXISTS pg_class").await.unwrap(),
            None
        );
        // Still there, so nothing ran.
        let result = conn.execute("SELECT count(*) FROM pg_class").await.unwrap();
        assert_eq!(result.rows.len(), 1);
    }

    #[tokio::test]
    async fn errors_are_reported_not_panicked() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();

        let error = conn
            .execute("SELECT * FROM no_such_table_here")
            .await
            .unwrap_err();
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
        assert_eq!(
            conn.execute("SELECT 1 AS x").await.unwrap().rows[0][0],
            Value::Int(1)
        );
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

        assert!(
            conn.stop(id, Stop::Cancel).await.unwrap(),
            "the server refused the cancel"
        );
        let error = runner.await.unwrap().unwrap_err().to_string();
        assert!(error.contains("canceling statement"), "{error}");
        // It slept for 30 seconds and this test did not.
        assert!(
            started.elapsed() < Duration::from_secs(10),
            "the cancel did not land"
        );

        // The session outlives a cancelled statement, as the tab holding
        // it expects: a cancel ends the statement, not the connection.
        assert_eq!(
            session.execute("SELECT 1 AS x").await.unwrap().rows[0][0],
            Value::Int(1)
        );
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
        assert!(
            runner.await.unwrap().is_err(),
            "the statement outlived its backend"
        );
        assert!(
            started.elapsed() < Duration::from_secs(10),
            "the terminate did not land"
        );

        // The app pool is still good afterwards: it throws the dead
        // connection away and opens another.
        assert_eq!(
            conn.execute("SELECT 1 AS x").await.unwrap().rows[0][0],
            Value::Int(1)
        );
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
        let limits = Limits {
            max_bytes: 64 * 1024,
            max_cell_bytes: MAX_CELL_BYTES,
        };
        let conn = PostgresConnection::connect(&url)
            .await
            .unwrap()
            .with_limits(limits);

        // Ten million rows, of which the budget holds a few hundred.
        let result = conn
            .execute("SELECT g, repeat('x', 100) FROM generate_series(1, 10000000) g")
            .await
            .unwrap();
        assert!(result.truncated, "the result was not marked truncated");
        assert!(
            !result.rows.is_empty(),
            "a capped result must still show rows"
        );
        assert!(result.rows.len() < 10_000_000, "the whole result came back");
        assert_eq!(result.columns.len(), 2);

        assert_eq!(
            conn.execute("SELECT 1 AS x").await.unwrap().rows[0][0],
            Value::Int(1)
        );
    }

    /// One value must not eat the whole budget. The cut value says it was
    /// cut, so the cell is not read as the whole string.
    #[tokio::test]
    async fn one_huge_value_is_cut_to_the_cell_cap() {
        let Some(url) = test_url() else { return };
        let limits = Limits {
            max_bytes: MAX_BYTES,
            max_cell_bytes: 4096,
        };
        let conn = PostgresConnection::connect(&url)
            .await
            .unwrap()
            .with_limits(limits);

        let result = conn
            .execute("SELECT repeat('x', 2000000) AS wide")
            .await
            .unwrap();
        let cell = &result.rows[0][0];
        let Value::Text(text) = cell else {
            panic!("not text: {cell:?}")
        };
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
        let conn = PostgresConnection::connect(&url)
            .await
            .unwrap()
            .with_statement_timeout("100ms");

        let error = conn
            .execute("SELECT pg_sleep(5)")
            .await
            .unwrap_err()
            .to_string();
        assert!(error.contains("statement timeout"), "{error}");
        // The connection is still good: a timeout ends the statement, not
        // the backend.
        assert_eq!(
            conn.execute("SELECT 1 AS x").await.unwrap().rows[0][0],
            Value::Int(1)
        );
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
        let conn = PostgresConnection::connect(&url)
            .await
            .unwrap()
            .with_statement_timeout("100ms");

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

        session
            .execute("SET statement_timeout = '5min'")
            .await
            .unwrap();
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
        session
            .execute("CREATE TEMP TABLE meerkat_session_probe (id int)")
            .await
            .unwrap();
        session
            .execute("INSERT INTO meerkat_session_probe VALUES (1), (2)")
            .await
            .unwrap();
        let result = session
            .execute("SELECT count(*) FROM meerkat_session_probe")
            .await
            .unwrap();
        assert_eq!(result.rows[0][0], Value::Int(2));

        session.execute("ROLLBACK").await.unwrap();
        // The table went with the transaction, and the session is still
        // good enough to say so.
        let error = session
            .execute("SELECT * FROM meerkat_session_probe")
            .await
            .unwrap_err()
            .to_string();
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
        let pid = session
            .backend()
            .expect("a Postgres session knows its backend")
            .0;

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

    /// What manual mode is made of. The app opens the transaction, the
    /// statements of several runs land inside it, and the user's word ends
    /// it — so the boundaries have to work without going through `execute`,
    /// which is the path with the cap and the timing on it.
    #[tokio::test]
    async fn a_session_opens_and_ends_a_transaction_on_request() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect_url(&url, false).await.unwrap();
        let session = conn.open_session().await.unwrap();

        session.begin().await.unwrap();
        assert!(session.in_transaction().await.unwrap());
        session
            .execute("CREATE TEMP TABLE meerkat_manual_probe (id int)")
            .await
            .unwrap();
        session
            .execute("INSERT INTO meerkat_manual_probe VALUES (1)")
            .await
            .unwrap();
        // Still open across the runs, which is the whole of manual mode.
        assert!(session.in_transaction().await.unwrap());

        session.end_transaction(TxEnd::Rollback).await.unwrap();
        assert!(!session.in_transaction().await.unwrap());
        let error = session
            .execute("SELECT * FROM meerkat_manual_probe")
            .await
            .unwrap_err()
            .to_string();
        assert!(error.contains("meerkat_manual_probe"), "{error}");

        // And a commit keeps what the transaction did. A temp table lives
        // as long as the session, so it is proof enough without writing to
        // anything the server keeps.
        session.begin().await.unwrap();
        session
            .execute("CREATE TEMP TABLE meerkat_manual_kept (id int)")
            .await
            .unwrap();
        session.end_transaction(TxEnd::Commit).await.unwrap();
        assert!(!session.in_transaction().await.unwrap());
        session
            .execute("SELECT * FROM meerkat_manual_kept")
            .await
            .unwrap();
    }

    /// A table created inside an open transaction exists for the connection
    /// that made it and for nobody else. The session's own catalog read is
    /// what lets the app say what a `CREATE` did before it is committed;
    /// the pool's read is what keeps that table out of the sidebar until
    /// it is. Its own schema, so it cannot trip over the fixture the other
    /// tests build and drop.
    #[tokio::test]
    async fn a_session_reads_the_catalog_it_can_see() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect_url(&url, false).await.unwrap();
        conn.execute("DROP SCHEMA IF EXISTS meerkat_shape CASCADE")
            .await
            .unwrap();
        conn.execute("CREATE SCHEMA meerkat_shape").await.unwrap();
        let session = conn.open_session().await.unwrap();

        session.begin().await.unwrap();
        session
            .execute("CREATE TABLE meerkat_shape.pending (id int, note text)")
            .await
            .unwrap();

        let has_pending = |catalog: &Catalog| {
            catalog
                .schemas
                .iter()
                .find(|s| s.name == "meerkat_shape")
                .is_some_and(|s| s.tables.iter().any(|t| t.name == "pending"))
        };
        let seen = session
            .introspect()
            .await
            .unwrap()
            .expect("a session reads the catalog");
        assert!(has_pending(&seen), "the session sees its own table");
        let pooled = conn.introspect().await.unwrap();
        assert!(!has_pending(&pooled), "the pool does not, until it commits");

        session.end_transaction(TxEnd::Rollback).await.unwrap();
        let after = session.introspect().await.unwrap().unwrap();
        assert!(
            !has_pending(&after),
            "rolled back, so gone for the session too"
        );

        conn.execute("DROP SCHEMA meerkat_shape CASCADE")
            .await
            .unwrap();
    }

    /// Ending a transaction nobody opened is not an error. The app sends it
    /// rather than asking first, and Postgres answers "there is no
    /// transaction in progress" and carries on.
    #[tokio::test]
    async fn ending_a_transaction_that_is_not_open_is_not_an_error() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();
        let session = conn.open_session().await.unwrap();
        session.end_transaction(TxEnd::Commit).await.unwrap();
        session.end_transaction(TxEnd::Rollback).await.unwrap();
        // The session is unharmed and still answers questions.
        session.execute("SELECT 1").await.unwrap();
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
        conn.open_session()
            .await
            .unwrap()
            .execute("SELECT 1")
            .await
            .unwrap();
    }

    /// A statement that fails must not cost the session. The tab keeps it
    /// across an error, so a typo does not silently end a transaction.
    #[tokio::test]
    async fn a_failed_statement_leaves_the_session_usable() {
        let Some(url) = test_url() else { return };
        let conn = PostgresConnection::connect(&url).await.unwrap();
        let session = conn.open_session().await.unwrap();

        assert!(
            session
                .execute("SELECT * FROM no_such_table_here")
                .await
                .is_err()
        );
        assert_eq!(
            session.execute("SELECT 1 AS x").await.unwrap().rows[0][0],
            Value::Int(1)
        );
    }

    /// What another connection sees this backend doing. It is read from
    /// the *app* pool, so it answers whether or not the session is busy.
    async fn backend_state(conn: &PostgresConnection, pid: i32) -> String {
        let result = conn
            .execute(&format!(
                "SELECT state FROM pg_stat_activity WHERE pid = {pid}"
            ))
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
        assert!(
            error.to_string().contains("not a valid PostgreSQL URL"),
            "{error}"
        );
    }

    /// The bytes `tsvectorsend` writes, so the reader can be argued with
    /// without a server: a lexeme count, then each lexeme NUL-terminated
    /// with its positions.
    fn tsvector_bytes(lexemes: &[(&str, &[(u16, u16)])]) -> Vec<u8> {
        let mut bytes = (lexemes.len() as u32).to_be_bytes().to_vec();
        for (lexeme, positions) in lexemes {
            bytes.extend(lexeme.as_bytes());
            bytes.push(0);
            bytes.extend((positions.len() as u16).to_be_bytes());
            for (position, weight) in *positions {
                bytes.extend((position | (weight << 14)).to_be_bytes());
            }
        }
        bytes
    }

    #[test]
    fn a_tsvector_reads_as_postgres_prints_it() {
        let bytes = tsvector_bytes(&[("fox", &[(3, 0)]), ("quick", &[(2, 3), (7, 0)])]);
        assert_eq!(tsvector(&bytes).unwrap(), "'fox':3 'quick':2A,7");

        // No positions at all — `to_tsvector` always gives them, but
        // `'a'::tsvector` does not.
        assert_eq!(tsvector(&tsvector_bytes(&[("a", &[])])).unwrap(), "'a'");
        assert_eq!(tsvector(&tsvector_bytes(&[])).unwrap(), "");

        // Every weight has its letter, and D — weight 0 — has none.
        let bytes = tsvector_bytes(&[("w", &[(1, 3), (2, 2), (3, 1), (4, 0)])]);
        assert_eq!(tsvector(&bytes).unwrap(), "'w':1A,2B,3C,4");

        // A quote and a backslash are doubled, or the rendering could not
        // be read back as a tsvector.
        let bytes = tsvector_bytes(&[("it's", &[]), ("a\\b", &[])]);
        assert_eq!(tsvector(&bytes).unwrap(), "'it''s' 'a\\\\b'");
    }

    #[test]
    fn a_short_tsvector_is_an_error_not_a_panic() {
        // A count that promises more than the bytes hold must not index
        // past the end: a viewer reports the cell, it does not abort.
        assert!(tsvector(&[0, 0, 0, 1]).is_err());
        assert!(tsvector(&[0, 0]).is_err());
        // A lexeme with no terminator.
        assert!(tsvector(&[0, 0, 0, 1, b'a']).is_err());
        // A position count with no positions behind it.
        assert!(tsvector(&[0, 0, 0, 1, b'a', 0, 0, 1]).is_err());
    }

    /// The bytes `array_send` writes for a one-dimensional `int4[]`, so
    /// the reader can be argued with without a server.
    fn int4_array_bytes(lower: i32, items: &[Option<i32>]) -> Vec<u8> {
        let mut bytes = Vec::new();
        bytes.extend(1i32.to_be_bytes());
        bytes.extend(0i32.to_be_bytes());
        bytes.extend(23u32.to_be_bytes());
        bytes.extend((items.len() as i32).to_be_bytes());
        bytes.extend(lower.to_be_bytes());
        for item in items {
            match item {
                Some(n) => {
                    bytes.extend(4i32.to_be_bytes());
                    bytes.extend(n.to_be_bytes());
                }
                None => bytes.extend((-1i32).to_be_bytes()),
            }
        }
        bytes
    }

    fn int4_array(bytes: &[u8]) -> Result<String> {
        let int4 = <i32 as sqlx::Type<sqlx::Postgres>>::type_info();
        array_wire(bytes).map(|items| array(&items, &int4))
    }

    #[test]
    fn an_array_reads_as_postgres_prints_it() {
        let bytes = int4_array_bytes(1, &[Some(1), None, Some(-3)]);
        assert_eq!(int4_array(&bytes).unwrap(), "{1,NULL,-3}");
        // A dimension that does not start at 1 says where it starts.
        let bytes = int4_array_bytes(-1, &[Some(5)]);
        assert_eq!(int4_array(&bytes).unwrap(), "[-1:-1]={5}");
        // No dimensions at all is the empty array.
        let empty = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 23];
        assert_eq!(int4_array(&empty).unwrap(), "{}");
    }

    #[test]
    fn a_malformed_array_is_an_error_not_a_panic() {
        let bytes = int4_array_bytes(1, &[Some(1), Some(2)]);
        // Cut short anywhere, in the header or in an element.
        for end in 0..bytes.len() {
            assert!(int4_array(&bytes[..end]).is_err(), "cut at {end}");
        }
        // Bytes past the last element.
        let mut long = bytes.clone();
        long.push(0);
        assert!(int4_array(&long).is_err());
        // More dimensions than Postgres allows, and a negative length.
        let mut deep = bytes.clone();
        deep[..4].copy_from_slice(&7i32.to_be_bytes());
        assert!(int4_array(&deep).is_err());
        let mut negative = bytes.clone();
        negative[12..16].copy_from_slice(&(-2i32).to_be_bytes());
        assert!(int4_array(&negative).is_err());
    }

    /// A header is a promise about the bytes behind it, and one that
    /// promises billions of elements must be refused before anything is
    /// built for them.
    #[test]
    fn an_array_header_cannot_promise_more_than_its_bytes() {
        let header = |lengths: &[i32]| {
            let mut bytes = Vec::new();
            bytes.extend((lengths.len() as i32).to_be_bytes());
            bytes.extend(0i32.to_be_bytes());
            bytes.extend(23u32.to_be_bytes());
            for length in lengths {
                bytes.extend(length.to_be_bytes());
                bytes.extend(1i32.to_be_bytes());
            }
            bytes
        };
        // A zero length inside a dimension would be braces around nothing,
        // two billion times over; the server sends no dimensions instead.
        assert!(int4_array(&header(&[i32::MAX, 0])).is_err());
        assert!(int4_array(&header(&[i32::MAX; 6])).is_err());
        let mut short = header(&[3]);
        short.extend(4i32.to_be_bytes());
        short.extend(1i32.to_be_bytes());
        assert!(int4_array(&short).is_err());
    }

    #[test]
    fn a_catalog_vector_reads_space_separated() {
        let int2 = <i16 as sqlx::Type<sqlx::Postgres>>::type_info();
        let mut bytes = Vec::new();
        for word in [1i32, 0, 21, 2, 0] {
            bytes.extend(word.to_be_bytes());
        }
        for n in [1i16, 3] {
            bytes.extend(2i32.to_be_bytes());
            bytes.extend(n.to_be_bytes());
        }
        assert_eq!(vector(&array_wire(&bytes).unwrap(), &int2), "1 3");
        assert!(is_vector("int2vector"));
        assert!(is_vector("oidvector"));
        assert!(!is_vector("INT2[]"));
    }

    #[test]
    fn a_date_reads_across_the_whole_postgres_range() {
        assert_eq!(calendar_date(0), ("2000-01-01".to_string(), ""));
        assert_eq!(calendar_date(-1), ("1999-12-31".to_string(), ""));
        assert_eq!(calendar_date(59), ("2000-02-29".to_string(), ""));
        // Past the year 9999, where `time` stops.
        assert_eq!(calendar_date(2_914_635), ("9980-01-01".to_string(), ""));
        assert_eq!(calendar_date(2_921_940), ("10000-01-01".to_string(), ""));
        // There is no year zero: the day before 0001-01-01 is 1 BC.
        assert_eq!(calendar_date(-730_119), ("0001-01-01".to_string(), ""));
        assert_eq!(calendar_date(-730_120), ("0001-12-31".to_string(), " BC"));
    }

    #[test]
    fn a_float_reads_as_postgres_prints_it() {
        let float8 = |value: f64| float(value, format!("{value:e}"), value.to_string(), 15);
        let float4 = |value: f32| float(value.into(), format!("{value:e}"), value.to_string(), 6);
        assert_eq!(float4(0.1), "0.1");
        assert_eq!(float4(123456.0), "123456");
        assert_eq!(float4(1234567.0), "1.234567e+06");
        assert_eq!(float8(1e14), "100000000000000");
        assert_eq!(float8(1e15), "1e+15");
        assert_eq!(float8(-2.5e300), "-2.5e+300");
        assert_eq!(float8(0.0001), "0.0001");
        assert_eq!(float8(0.00001), "1e-05");
        assert_eq!(float8(-0.0), "-0");
        assert_eq!(float8(f64::NEG_INFINITY), "-Infinity");
    }

    #[test]
    fn an_array_element_is_quoted_when_it_would_misread() {
        let quoted = |item: &str| {
            let mut out = String::new();
            array_item(&mut out, item);
            out
        };
        assert_eq!(quoted("plain"), "plain");
        assert_eq!(quoted(""), r#""""#);
        assert_eq!(quoted("null"), r#""null""#);
        assert_eq!(quoted("a b"), r#""a b""#);
        assert_eq!(quoted("{x}"), r#""{x}""#);
        assert_eq!(quoted(r#"say "hi""#), r#""say \"hi\"""#);
        assert_eq!(quoted(r"a\b"), r#""a\\b""#);
    }

    /// The bytes `numeric_send` writes: a digit count, a weight, a sign, a
    /// display scale, then the base-10000 digits.
    fn numeric_bytes(weight: i16, sign: u16, scale: u16, digits: &[u16]) -> Vec<u8> {
        let mut bytes = Vec::new();
        bytes.extend((digits.len() as u16).to_be_bytes());
        bytes.extend(weight.to_be_bytes());
        bytes.extend(sign.to_be_bytes());
        bytes.extend(scale.to_be_bytes());
        for digit in digits {
            bytes.extend(digit.to_be_bytes());
        }
        bytes
    }

    #[test]
    fn a_numeric_keeps_its_scale() {
        assert_eq!(
            numeric(&numeric_bytes(0, 0, 2, &[1280])).unwrap(),
            "1280.00"
        );
        assert_eq!(
            numeric(&numeric_bytes(1, 0, 1, &[1234, 5678, 9000])).unwrap(),
            "12345678.9"
        );
        assert_eq!(
            numeric(&numeric_bytes(-1, 0x4000, 3, &[10])).unwrap(),
            "-0.001"
        );
        // Digits past the ones sent are zero, on either side of the point.
        assert_eq!(numeric(&numeric_bytes(2, 0, 0, &[7])).unwrap(), "700000000");
        assert_eq!(numeric(&numeric_bytes(0, 0, 0, &[])).unwrap(), "0");
        assert_eq!(numeric(&numeric_bytes(0, 0xC000, 0, &[])).unwrap(), "NaN");
        assert!(numeric(&numeric_bytes(0, 0x1234, 0, &[])).is_err());
        assert!(numeric(&[0, 1, 0, 0, 0, 0, 0, 0]).is_err());
    }

    #[test]
    fn a_transaction_id_stays_unsigned() {
        assert_eq!(transaction_id(7), Value::Int(7));
        assert_eq!(transaction_id(u32::MAX as u64), Value::Int(4294967295));
        // Past `i64::MAX` there is no `Int` that says the truth.
        assert_eq!(transaction_id(u64::MAX), Value::Text(u64::MAX.to_string()));
    }

    #[test]
    fn the_server_version_reads_short() {
        assert_eq!(short_version("16.2 (Debian 16.2-1.pgdg120+2)"), "16.2");
        assert_eq!(short_version("15.6"), "15.6");
    }
}
