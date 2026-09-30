//! SQLite driver, backed by sqlx.

use anyhow::Context as _;
use async_trait::async_trait;
use db_client::{
    Connection, Limits, QueryResult, Result, RowChange, RowSink, RunId, Session, TxEnd, Value,
};
use futures::TryStreamExt as _;
use introspect::{Catalog, Column, ForeignKey, Index, Schema, Table, TableKind, Trigger};
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
        Ok(Self {
            pool,
            limits: Limits::default(),
        })
    }

    pub async fn open_in_memory() -> Result<Self> {
        let pool = SqlitePool::connect("sqlite::memory:").await?;
        Ok(Self {
            pool,
            limits: Limits::default(),
        })
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
        let Some(mut conn) = self.conn.lock().await.take() else {
            return;
        };
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

/// The per-table reads `introspect` makes beside `pragma_table_info`.
impl SqliteConnection {
    /// A table's indexes, the implicit ones a `UNIQUE` or a `PRIMARY KEY`
    /// makes among them. `origin` says which: `pk`, `u`, or `c` for one
    /// made with `CREATE INDEX`.
    async fn indexes(&self, table: &str) -> Result<Vec<Index>> {
        let list: Vec<(String, i64, String, i64)> = sqlx::query_as(
            "SELECT name, \"unique\", origin, partial FROM pragma_index_list(?1) ORDER BY name",
        )
        .bind(table)
        .fetch_all(&self.pool)
        .await?;
        let mut indexes = Vec::with_capacity(list.len());
        for (name, unique, origin, partial) in list {
            // A key over an expression names no column, and reads `NULL`.
            let columns: Vec<(Option<String>,)> =
                sqlx::query_as("SELECT name FROM pragma_index_info(?1) ORDER BY seqno")
                    .bind(&name)
                    .fetch_all(&self.pool)
                    .await?;
            let key: Vec<String> = columns
                .into_iter()
                .map(|(column,)| column.unwrap_or_else(|| "<expression>".to_string()))
                .collect();
            let mut definition = format!("({})", key.join(", "));
            if partial != 0 {
                definition.push_str(" WHERE …");
            }
            indexes.push(Index {
                name,
                definition,
                unique: unique != 0,
                primary: origin == "pk",
            });
        }
        Ok(indexes)
    }

    /// A table's foreign keys. SQLite names none of them, so each is
    /// named the way Postgres would have named it: `orders_user_id_fkey`,
    /// and `orders_user_id_fkey1` for a second key over the same columns,
    /// which Postgres would have named that way too.
    async fn foreign_keys(&self, table: &str) -> Result<Vec<ForeignKey>> {
        let rows: Vec<(i64, String, String, Option<String>)> = sqlx::query_as(
            "SELECT id, \"table\", \"from\", \"to\" FROM pragma_foreign_key_list(?1) \
             ORDER BY id, seq",
        )
        .bind(table)
        .fetch_all(&self.pool)
        .await?;
        let mut keys: Vec<(i64, ForeignKey)> = Vec::new();
        for (id, target, from, to) in rows {
            if keys.last().is_none_or(|(last, _)| *last != id) {
                keys.push((
                    id,
                    ForeignKey {
                        name: String::new(),
                        columns: Vec::new(),
                        target_schema: "main".to_string(),
                        target_table: target,
                        target_columns: Vec::new(),
                    },
                ));
            }
            let (_, key) = keys.last_mut().expect("pushed above");
            key.columns.push(from);
            if let Some(to) = to {
                key.target_columns.push(to);
            }
        }
        let mut named: Vec<ForeignKey> = Vec::with_capacity(keys.len());
        for (_, mut key) in keys {
            // No `to` means the key names the target's primary key, which
            // is where the columns have to be read from.
            if key.target_columns.len() != key.columns.len() {
                let primary: Vec<(String,)> = sqlx::query_as(
                    "SELECT name FROM pragma_table_info(?1) WHERE pk > 0 ORDER BY pk",
                )
                .bind(&key.target_table)
                .fetch_all(&self.pool)
                .await?;
                key.target_columns = primary.into_iter().map(|(name,)| name).collect();
            }
            let base = format!("{table}_{}_fkey", key.columns.join("_"));
            let mut name = base.clone();
            let mut n = 0;
            while named.iter().any(|other| other.name == name) {
                n += 1;
                name = format!("{base}{n}");
            }
            key.name = name;
            named.push(key);
        }
        Ok(named)
    }

    async fn triggers(&self, table: &str) -> Result<Vec<Trigger>> {
        let rows: Vec<(String, Option<String>)> = sqlx::query_as(
            "SELECT name, sql FROM sqlite_master \
             WHERE type = 'trigger' AND tbl_name = ?1 ORDER BY name",
        )
        .bind(table)
        .fetch_all(&self.pool)
        .await?;
        Ok(rows
            .into_iter()
            .map(|(name, sql)| Trigger {
                name,
                timing: sql.as_deref().map(trigger_timing).unwrap_or_default(),
                // The body is inline in the trigger, not a function.
                function: String::new(),
            })
            .collect())
    }
}

/// When a trigger fires, read off its own `CREATE TRIGGER`: the words
/// between its name and `ON`, lowercased — `before insert`, `after update
/// of email`. SQLite keeps nothing else to read it from, and a statement
/// this walk cannot follow says nothing rather than something wrong.
fn trigger_timing(sql: &str) -> String {
    let words: Vec<&str> = sql.split_whitespace().collect();
    let Some(at) = words
        .iter()
        .position(|word| word.eq_ignore_ascii_case("trigger"))
    else {
        return String::new();
    };
    let mut rest = &words[at + 1..];
    if rest.len() >= 3
        && rest[0].eq_ignore_ascii_case("if")
        && rest[1].eq_ignore_ascii_case("not")
        && rest[2].eq_ignore_ascii_case("exists")
    {
        rest = &rest[3..];
    }
    // The name, which a quoted name with a space in it would break.
    let Some(rest) = rest.get(1..) else {
        return String::new();
    };
    let Some(on) = rest.iter().position(|word| word.eq_ignore_ascii_case("on")) else {
        return String::new();
    };
    let timing = rest[..on].join(" ").to_lowercase();
    // A trigger with no timing word fires before the statement.
    if timing.starts_with("before") || timing.starts_with("after") || timing.starts_with("instead")
    {
        timing
    } else {
        format!("before {timing}")
    }
}

#[async_trait]
impl Connection for SqliteConnection {
    async fn open_session(&self) -> Result<Arc<dyn Session>> {
        let conn = self
            .pool
            .acquire()
            .await
            .context("no connection for the session")?;
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
                name: name.clone(),
                kind: if kind == "view" {
                    TableKind::View
                } else {
                    TableKind::Table
                },
                columns,
                primary_key: pk.into_iter().map(|(_, col)| col).collect(),
                // SQLite keeps no row estimate; a per-table COUNT(*) would
                // scan every table on connect, so report nothing.
                approx_rows: None,
                indexes: self.indexes(&name).await?,
                foreign_keys: self.foreign_keys(&name).await?,
                // SQLite keeps a check only in the `CREATE TABLE` text, and
                // reading it back out would mean parsing SQL. A unique
                // constraint is an index here, and is listed there.
                constraints: Vec::new(),
                triggers: self.triggers(&name).await?,
            });
        }

        // SQLite has no sequences, stored routines or user types, so the
        // one schema holds relations and nothing else.
        Ok(Catalog {
            schemas: vec![Schema {
                name: "main".to_string(),
                tables,
                ..Default::default()
            }],
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

        let result = conn
            .execute("SELECT id, name FROM users ORDER BY id")
            .await
            .unwrap();
        assert_eq!(result.columns, vec!["id", "name"]);
        assert_eq!(result.rows[0][1], Value::Text("ada".to_string()));
        assert_eq!(result.rows[1][0], Value::Int(2));
    }

    #[tokio::test]
    async fn introspect_reads_indexes_keys_and_triggers() {
        let conn = SqliteConnection::open_in_memory().await.unwrap();
        for statement in [
            "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE)",
            "CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users (id),
                 CONSTRAINT again FOREIGN KEY (user_id) REFERENCES users)",
            "CREATE INDEX orders_user ON orders (user_id)",
            "CREATE TRIGGER stamp AFTER UPDATE OF user_id ON orders BEGIN SELECT 1; END",
        ] {
            conn.execute(statement).await.unwrap();
        }
        let catalog = conn.introspect().await.unwrap();
        let tables = &catalog.schemas[0].tables;
        let orders = tables.iter().find(|t| t.name == "orders").unwrap();

        assert_eq!(orders.indexes.len(), 1);
        assert_eq!(orders.indexes[0].detail(), "(user_id)");
        assert_eq!(
            orders.foreign_keys[0].detail(),
            "(user_id) → main.users(id)"
        );
        // A key that names no target column reads the target's primary
        // key, and a second key over the same column gets a name of its own.
        let keys: Vec<(String, String)> = (orders.foreign_keys.iter())
            .map(|k| (k.name.clone(), k.detail()))
            .collect();
        assert_eq!(
            keys,
            [
                (
                    "orders_user_id_fkey".to_string(),
                    "(user_id) → main.users(id)".to_string()
                ),
                (
                    "orders_user_id_fkey1".to_string(),
                    "(user_id) → main.users(id)".to_string()
                ),
            ]
        );
        assert_eq!(orders.triggers[0].timing, "after update of user_id");

        // The unique constraint is the index SQLite made for it.
        let users = tables.iter().find(|t| t.name == "users").unwrap();
        assert_eq!(users.indexes.len(), 1);
        assert!(users.indexes[0].unique);
        assert_eq!(users.indexes[0].definition, "(email)");
    }

    #[test]
    fn a_trigger_reads_its_timing_off_its_own_statement() {
        assert_eq!(
            trigger_timing("CREATE TRIGGER IF NOT EXISTS t BEFORE DELETE ON x BEGIN END"),
            "before delete"
        );
        // No timing word means before.
        assert_eq!(
            trigger_timing("CREATE TRIGGER t INSERT ON x BEGIN END"),
            "before insert"
        );
        assert_eq!(trigger_timing("not a trigger"), "");
    }

    /// The count off the completion tag, on the engine that needs no
    /// server. `fetch` dropped that tag, so an `UPDATE` used to answer with
    /// nothing whatever.
    #[tokio::test]
    async fn a_statement_that_changes_rows_reports_how_many() {
        let conn = SqliteConnection::open_in_memory().await.unwrap();
        let session = conn.open_session().await.unwrap();

        session
            .execute("CREATE TABLE t (id INTEGER)")
            .await
            .unwrap();
        let inserted = session
            .execute("INSERT INTO t VALUES (1), (2), (3)")
            .await
            .unwrap();
        assert_eq!(inserted.rows_affected, 3);
        assert!(inserted.columns.is_empty());

        let updated = session
            .execute("UPDATE t SET id = id + 1 WHERE id > 1")
            .await
            .unwrap();
        assert_eq!(updated.rows_affected, 2);

        // Matched nothing, and says so with a count rather than an empty
        // table: the statement worked.
        let nothing = session
            .execute("DELETE FROM t WHERE id = 999")
            .await
            .unwrap();
        assert_eq!(nothing.rows_affected, 0);
        assert!(nothing.columns.is_empty());
    }

    /// A session is one connection, so a temp table made by one statement
    /// is there for the next — and gone once the session closes.
    #[tokio::test]
    async fn a_session_keeps_what_its_statements_made() {
        let conn = SqliteConnection::open_in_memory().await.unwrap();
        let session = conn.open_session().await.unwrap();

        session
            .execute("CREATE TEMP TABLE probe (id INTEGER)")
            .await
            .unwrap();
        session
            .execute("INSERT INTO probe VALUES (1), (2)")
            .await
            .unwrap();
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

        conn.open_session()
            .await
            .unwrap()
            .execute("SELECT 1")
            .await
            .unwrap();
    }
}
