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
    Connection, Profile, QueryResult, ReportRun, Result, RowChange, RunId, Stop, Value,
};
use introspect::{Catalog, Column, Schema, Table, TableKind};
use sqlx::postgres::{PgConnectOptions, PgPool, PgPoolOptions, PgRow};
use sqlx::{Column as _, Executor as _, Row as _, TypeInfo as _, ValueRef as _};
use std::str::FromStr;
use std::time::Duration;

/// Pages of 500 rows plus one query tab at a time: a small pool is enough,
/// and it keeps the connection count polite on shared servers. One slot
/// over what the tabs need, because stopping a run needs a connection of
/// its own — waiting for a free one would mean waiting for the very
/// statement the user asked to stop.
const MAX_CONNECTIONS: u32 = 5;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

pub struct PostgresConnection {
    pool: PgPool,
    label: Label,
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
            .max_connections(MAX_CONNECTIONS)
            .acquire_timeout(CONNECT_TIMEOUT)
            .connect_with(options)
            .await
            .with_context(|| format!("failed to connect to {}", label.host))?;
        Ok(Self { pool, label })
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

    async fn execute(&self, sql: &str) -> Result<QueryResult> {
        let rows: Vec<PgRow> = sqlx::query(sql).fetch_all(&self.pool).await?;
        self.collect(sql, rows).await
    }

    /// One connection is taken out of the pool and kept for the whole
    /// statement, because the backend id only means anything while that
    /// connection is the one running it. Asking the server for the id costs
    /// one round trip before the statement starts; that is the price of
    /// being able to stop it, and it is paid once per run.
    async fn execute_reporting(&self, sql: &str, report: ReportRun) -> Result<QueryResult> {
        let mut conn = self.pool.acquire().await.context("no connection to run the statement")?;
        let pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
            .fetch_one(&mut *conn)
            .await
            .context("failed to read the backend id")?;
        report(RunId(pid));
        let rows: Vec<PgRow> = sqlx::query(sql).fetch_all(&mut *conn).await?;
        // The connection goes back to the pool here, before the rows are
        // decoded: decoding is this process's work, not the server's.
        drop(conn);
        self.collect(sql, rows).await
    }

    /// `pg_cancel_backend` asks the backend to give up its statement, which
    /// comes back to the runner as SQLSTATE 57014. `pg_terminate_backend`
    /// closes the backend outright, and sqlx then throws that connection
    /// away — which is why it is the second press.
    ///
    /// Both go out on a *different* connection: the one being stopped is
    /// busy, and waiting for it would be waiting for the thing the user
    /// just asked to stop.
    async fn stop(&self, run: RunId, how: Stop) -> Result<bool> {
        let sql = match how {
            Stop::Cancel => "SELECT pg_cancel_backend($1)",
            Stop::Terminate => "SELECT pg_terminate_backend($1)",
        };
        let accepted: Option<bool> = sqlx::query_scalar(sql)
            .bind(run.0)
            .fetch_one(&self.pool)
            .await
            .context("failed to ask the server to stop the statement")?;
        Ok(accepted.unwrap_or(false))
    }

    async fn apply(&self, _changes: &[RowChange]) -> Result<u64> {
        anyhow::bail!("in-place editing is not implemented yet (Phase 2)")
    }
}

impl PostgresConnection {
    /// Turn the rows a statement returned into a `QueryResult`, naming the
    /// columns even when no row came back to name them.
    async fn collect(&self, sql: &str, rows: Vec<PgRow>) -> Result<QueryResult> {
        let Some(first) = rows.first() else {
            // No rows came back, so the row metadata cannot name the
            // columns. Ask the server to describe the statement instead,
            // so an empty result still renders its headers. DDL and other
            // statements without a result set simply describe to nothing.
            let columns = match self.pool.describe(sql).await {
                Ok(described) => described
                    .columns()
                    .iter()
                    .map(|c| c.name().to_string())
                    .collect(),
                Err(_) => Vec::new(),
            };
            return Ok(QueryResult { columns, rows: Vec::new(), rows_affected: 0 });
        };

        let columns = first.columns().iter().map(|c| c.name().to_string()).collect();
        let mut out = Vec::with_capacity(rows.len());
        for row in &rows {
            let mut values = Vec::with_capacity(row.columns().len());
            for i in 0..row.columns().len() {
                values.push(decode(row, i)?);
            }
            out.push(values);
        }

        Ok(QueryResult { columns, rows: out, rows_affected: 0 })
    }
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

    /// The whole point of `execute_reporting`: a statement that would run
    /// for half a minute is stopped in the middle, from another connection,
    /// with only the id it reported.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_running_statement_can_be_cancelled_from_outside() {
        let Some(url) = test_url() else { return };
        let conn = std::sync::Arc::new(PostgresConnection::connect(&url).await.unwrap());

        let seen = std::sync::Arc::new(std::sync::atomic::AtomicI32::new(0));
        let report: ReportRun = {
            let seen = seen.clone();
            std::sync::Arc::new(move |id: RunId| {
                seen.store(id.0, std::sync::atomic::Ordering::Relaxed)
            })
        };
        let runner = {
            let conn = conn.clone();
            tokio::spawn(async move { conn.execute_reporting("SELECT pg_sleep(30)", report).await })
        };

        let started = std::time::Instant::now();
        let pid = loop {
            let pid = seen.load(std::sync::atomic::Ordering::Relaxed);
            if pid != 0 {
                break pid;
            }
            assert!(started.elapsed() < Duration::from_secs(5), "no backend id was reported");
            tokio::time::sleep(Duration::from_millis(10)).await;
        };

        assert!(conn.stop(RunId(pid), Stop::Cancel).await.unwrap(), "the server refused the cancel");
        let error = runner.await.unwrap().unwrap_err().to_string();
        assert!(error.contains("canceling statement"), "{error}");
        // It slept for 30 seconds and this test did not.
        assert!(started.elapsed() < Duration::from_secs(10), "the cancel did not land");
    }

    /// The second press. A terminated backend takes its connection with
    /// it, so the runner hears about it as a broken connection rather than
    /// as a cancelled statement — which is why the app only offers this
    /// after a cancel has already been asked for.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_running_statement_can_be_terminated() {
        let Some(url) = test_url() else { return };
        let conn = std::sync::Arc::new(PostgresConnection::connect(&url).await.unwrap());

        let seen = std::sync::Arc::new(std::sync::atomic::AtomicI32::new(0));
        let report: ReportRun = {
            let seen = seen.clone();
            std::sync::Arc::new(move |id: RunId| {
                seen.store(id.0, std::sync::atomic::Ordering::Relaxed)
            })
        };
        let runner = {
            let conn = conn.clone();
            tokio::spawn(async move { conn.execute_reporting("SELECT pg_sleep(30)", report).await })
        };

        let started = std::time::Instant::now();
        let pid = loop {
            let pid = seen.load(std::sync::atomic::Ordering::Relaxed);
            if pid != 0 {
                break pid;
            }
            assert!(started.elapsed() < Duration::from_secs(5), "no backend id was reported");
            tokio::time::sleep(Duration::from_millis(10)).await;
        };

        assert!(conn.stop(RunId(pid), Stop::Terminate).await.unwrap());
        assert!(runner.await.unwrap().is_err(), "the statement outlived its backend");
        assert!(started.elapsed() < Duration::from_secs(10), "the terminate did not land");

        // The pool is still good afterwards: it throws the dead connection
        // away and opens another.
        assert_eq!(conn.execute("SELECT 1 AS x").await.unwrap().rows[0][0], Value::Int(1));
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
