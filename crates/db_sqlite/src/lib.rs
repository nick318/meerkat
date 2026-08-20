//! SQLite driver, backed by sqlx.

use anyhow::Context as _;
use async_trait::async_trait;
use db_client::{
    Connection, Limits, QueryResult, Result, RowChange, RowSink, RunId, Session, TxEnd, Value,
};
use futures::TryStreamExt as _;
use introspect::{Catalog, Column, Schema, Table, TableKind};
use sqlx::sqlite::{SqliteConnectOptions, SqlitePool, SqliteRow};
use sqlx::{Column as _, Either, Executor as _, Row as _, TypeInfo as _, ValueRef as _};
use std::path::Path;
use std::str::FromStr;
use std::sync::Arc;

pub struct SqliteConnection {
    pool: SqlitePool,
    limits: Limits,
}

impl SqliteConnection {
    pub async fn open(path: &Path) -> Result<Self> {
        let options = SqliteConnectOptions::from_str(&format!("sqlite://{}", path.display()))?
            .create_if_missing(false);
        let pool = SqlitePool::connect_with(options)
            .await
            .with_context(|| format!("failed to open {}", path.display()))?;
        Ok(Self { pool, limits: Limits::default() })
    }

    pub async fn open_in_memory() -> Result<Self> {
        let pool = SqlitePool::connect("sqlite::memory:").await?;
        Ok(Self { pool, limits: Limits::default() })
    }

    /// Hold a smaller result than the app's own budget. See the Postgres
    /// driver's version: it is for the tests.
    pub fn with_limits(mut self, limits: Limits) -> Self {
        self.limits = limits;
        self
    }
}

/// One tab's pinned connection. The same shape as the Postgres one and
/// for the same reason — a `PRAGMA` and a temp table belong to the
/// connection that made them — minus everything about cancelling: the rows
/// come from a file on this machine, so there is no backend to reach into.
struct SqliteSession {
    conn: futures::lock::Mutex<Option<sqlx::pool::PoolConnection<sqlx::Sqlite>>>,
    limits: Limits,
}

#[async_trait]
impl Session for SqliteSession {
    fn backend(&self) -> Option<RunId> {
        None
    }

    async fn execute(&self, sql: &str) -> Result<QueryResult> {
        let mut held = self.conn.lock().await;
        let conn = held.as_mut().context("this tab's session is closed")?;
        let mut sink = RowSink::new(self.limits);
        // `fetch_many` rather than `fetch`, because `fetch` drops the
        // completion tag — and the tag is the only place SQLite ever states
        // how many rows an `UPDATE` changed. See `collect_capped` in
        // `db_postgres` for the whole of that reasoning; it holds here too.
        let mut affected = 0;
        let mut stream = (&mut **conn).fetch_many(sqlx::query(sql));
        while let Some(step) = stream.try_next().await? {
            let row = match step {
                Either::Left(done) => {
                    affected += done.rows_affected();
                    continue;
                }
                Either::Right(row) => row,
            };
            if !sink.has_columns() {
                sink.columns(row.columns().iter().map(|c| c.name().to_string()).collect());
            }
            let mut values = Vec::with_capacity(row.columns().len());
            for i in 0..row.columns().len() {
                values.push(decode(&row, i)?);
            }
            if !sink.push(values) {
                break;
            }
        }
        drop(stream);
        Ok(sink.finish(affected))
    }

    /// The same two statements the Postgres session sends, and the same
    /// reason they do not go through `execute`: a boundary is not a run.
    ///
    /// SQLite leaves [`Session::in_transaction`] unanswered, so the app has
    /// only what it sent to go on here — see that method. It is why a
    /// second `BEGIN`, which SQLite refuses outright, must never be asked
    /// for.
    async fn begin(&self) -> Result<()> {
        self.boundary("BEGIN").await
    }

    async fn end_transaction(&self, how: TxEnd) -> Result<()> {
        self.boundary(how.sql()).await
    }

    async fn close(&self) {
        let Some(mut conn) = self.conn.lock().await.take() else { return };
        match sqlx::query("ROLLBACK").execute(&mut *conn).await {
            Ok(_) => drop(conn),
            // SQLite answers "cannot rollback - no transaction is active",
            // which is the ordinary case and not a reason to throw the
            // connection away. Only a connection that failed to *answer*
            // leaves the pool.
            Err(sqlx::Error::Database(_)) => drop(conn),
            Err(_) => drop(conn.detach()),
        }
    }
}

impl SqliteSession {
    /// A transaction boundary, on this session's own connection: the
    /// transaction belongs to the connection that opened it.
    async fn boundary(&self, sql: &str) -> Result<()> {
        let mut held = self.conn.lock().await;
        let conn = held.as_mut().context("this tab's session is closed")?;
        sqlx::query(sql)
            .execute(&mut **conn)
            .await
            .with_context(|| format!("SQLite refused {sql}"))?;
        Ok(())
    }
}

/// The same fallback the Postgres session needs, and for the same reason:
/// sqlx returns a pooled connection by spawning onto tokio, and the last
/// handle to a session is usually dropped on the UI thread, which has no
/// runtime. See `impl Drop for PostgresSession`.
impl Drop for SqliteSession {
    fn drop(&mut self) {
        if let Some(conn) = self.conn.get_mut().take() {
            drop(conn.detach());
        }
    }
}

#[async_trait]
impl Connection for SqliteConnection {
    async fn open_session(&self) -> Result<Arc<dyn Session>> {
        let conn = self.pool.acquire().await.context("no connection for the session")?;
        Ok(Arc::new(SqliteSession {
            conn: futures::lock::Mutex::new(Some(conn)),
            limits: self.limits,
        }))
    }

    async fn introspect(&self) -> Result<Catalog> {
        let names: Vec<(String, String)> = sqlx::query_as(
            "SELECT name, type FROM sqlite_master \
             WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' \
             ORDER BY name",
        )
        .fetch_all(&self.pool)
        .await?;

        let mut tables = Vec::with_capacity(names.len());
        for (name, kind) in names {
            let info: Vec<(String, String, i64, Option<String>, i64)> = sqlx::query_as(
                "SELECT name, type, \"notnull\", dflt_value, pk FROM pragma_table_info(?1)",
            )
            .bind(&name)
            .fetch_all(&self.pool)
            .await?;

            let mut pk: Vec<(i64, String)> = Vec::new();
            let columns = info
                .into_iter()
                .map(|(col, data_type, notnull, default, pk_ord)| {
                    if pk_ord > 0 {
                        pk.push((pk_ord, col.clone()));
                    }
                    Column {
                        name: col,
                        data_type,
                        nullable: notnull == 0,
                        default,
                    }
                })
                .collect();
            pk.sort_by_key(|(ord, _)| *ord);

            tables.push(Table {
                name,
                kind: if kind == "view" { TableKind::View } else { TableKind::Table },
                columns,
                primary_key: pk.into_iter().map(|(_, col)| col).collect(),
                // SQLite keeps no row estimate; a per-table COUNT(*) would
                // scan every table on connect, so report nothing.
                approx_rows: None,
            });
        }

        Ok(Catalog {
            schemas: vec![Schema { name: "main".to_string(), tables }],
        })
    }

    async fn server_version(&self) -> Result<String> {
        let (version,): (String,) = sqlx::query_as("SELECT sqlite_version()")
            .fetch_one(&self.pool)
            .await
            .context("failed to read the SQLite version")?;
        Ok(format!("SQLite {version}"))
    }

    /// Streamed and capped by memory, as the Postgres driver is. There is
    /// no cancel to send: the rows come from a file on this machine, so
    /// dropping the stream ends the work rather than leaving a server to
    /// finish a result nobody will read.
    ///
    /// [`db_client::Wire`] is left empty on purpose, here and on the
    /// session. There is no link to measure and no server to blame: the
    /// rows come off a local file, so the wall clock around the call is the
    /// whole story and splitting it would invent two numbers out of one.
    async fn execute(&self, sql: &str) -> Result<QueryResult> {
        let mut sink = RowSink::new(self.limits);
        // The tag carries the count, and only `fetch_many` hands it over.
        let mut affected = 0;
        let mut stream = self.pool.fetch_many(sqlx::query(sql));
        while let Some(step) = stream.try_next().await? {
            let row = match step {
                Either::Left(done) => {
                    affected += done.rows_affected();
                    continue;
                }
                Either::Right(row) => row,
            };
            if !sink.has_columns() {
                sink.columns(row.columns().iter().map(|c| c.name().to_string()).collect());
            }
            let mut values = Vec::with_capacity(row.columns().len());
            for i in 0..row.columns().len() {
                values.push(decode(&row, i)?);
            }
            if !sink.push(values) {
                break;
            }
        }
        drop(stream);
        Ok(sink.finish(affected))
    }

    async fn apply(&self, _changes: &[RowChange]) -> Result<u64> {
        anyhow::bail!("in-place editing is not implemented yet (Phase 2)")
    }
}

fn decode(row: &SqliteRow, i: usize) -> Result<Value> {
    let raw = row.try_get_raw(i)?;
    if raw.is_null() {
        return Ok(Value::Null);
    }
    let type_name = raw.type_info().name().to_string();
    drop(raw);
    Ok(match type_name.as_str() {
        "BOOLEAN" => Value::Bool(row.try_get::<bool, _>(i)?),
        "INTEGER" => Value::Int(row.try_get::<i64, _>(i)?),
        "REAL" => Value::Float(row.try_get::<f64, _>(i)?),
        "BLOB" => Value::Bytes(row.try_get::<Vec<u8>, _>(i)?),
        _ => Value::Text(row.try_get::<String, _>(i)?),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn introspect_and_query() {
        let conn = SqliteConnection::open_in_memory().await.unwrap();
        conn.execute("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL)")
            .await
            .unwrap();
        conn.execute("INSERT INTO users (id, name) VALUES (1, 'ada'), (2, 'grace')")
            .await
            .unwrap();

        let catalog = conn.introspect().await.unwrap();
        let table = &catalog.schemas[0].tables[0];
        assert_eq!(table.name, "users");
        assert_eq!(table.primary_key, vec!["id".to_string()]);
        assert_eq!(table.columns.len(), 2);

        let result = conn.execute("SELECT id, name FROM users ORDER BY id").await.unwrap();
        assert_eq!(result.columns, vec!["id", "name"]);
        assert_eq!(result.rows[0][1], Value::Text("ada".to_string()));
        assert_eq!(result.rows[1][0], Value::Int(2));
    }

    /// The count off the completion tag, on the engine that needs no
    /// server. `fetch` dropped that tag, so an `UPDATE` used to answer with
    /// nothing whatever.
    #[tokio::test]
    async fn a_statement_that_changes_rows_reports_how_many() {
        let conn = SqliteConnection::open_in_memory().await.unwrap();
        let session = conn.open_session().await.unwrap();

        session.execute("CREATE TABLE t (id INTEGER)").await.unwrap();
        let inserted = session.execute("INSERT INTO t VALUES (1), (2), (3)").await.unwrap();
        assert_eq!(inserted.rows_affected, 3);
        assert!(inserted.columns.is_empty());

        let updated = session.execute("UPDATE t SET id = id + 1 WHERE id > 1").await.unwrap();
        assert_eq!(updated.rows_affected, 2);

        // Matched nothing, and says so with a count rather than an empty
        // table: the statement worked.
        let nothing = session.execute("DELETE FROM t WHERE id = 999").await.unwrap();
        assert_eq!(nothing.rows_affected, 0);
        assert!(nothing.columns.is_empty());
    }

    /// A session is one connection, so a temp table made by one statement
    /// is there for the next — and gone once the session closes.
    #[tokio::test]
    async fn a_session_keeps_what_its_statements_made() {
        let conn = SqliteConnection::open_in_memory().await.unwrap();
        let session = conn.open_session().await.unwrap();

        session.execute("CREATE TEMP TABLE probe (id INTEGER)").await.unwrap();
        session.execute("INSERT INTO probe VALUES (1), (2)").await.unwrap();
        let result = session.execute("SELECT count(*) FROM probe").await.unwrap();
        assert_eq!(result.rows[0][0], Value::Int(2));

        session.close().await;
        let error = session.execute("SELECT 1").await.unwrap_err().to_string();
        assert!(error.contains("closed"), "{error}");
    }

    /// See `impl Drop for SqliteSession`: the UI thread has no tokio
    /// runtime, and sqlx returns a pooled connection by spawning onto one.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_session_may_be_dropped_off_the_runtime() {
        let conn = SqliteConnection::open_in_memory().await.unwrap();
        let session = conn.open_session().await.unwrap();
        session.execute("SELECT 1").await.unwrap();

        std::thread::spawn(move || drop(session)).join().unwrap();

        conn.open_session().await.unwrap().execute("SELECT 1").await.unwrap();
    }
}
