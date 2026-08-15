//! PostgreSQL driver, backed by sqlx.
//!
//! Read-only for now: [`Connection::apply`] returns an error, exactly as
//! the SQLite driver does. Introspection reads `pg_catalog` rather than
//! `information_schema` for tables and columns, because `pg_catalog` also
//! carries the `reltuples` row estimate and `format_type` renders the type
//! names the way `psql` shows them. Primary keys come from
//! `information_schema`, which already reports the key column order.

use anyhow::Context as _;
use async_trait::async_trait;
use db_client::{Connection, Profile, QueryResult, Result, RowChange, Value};
use introspect::{Catalog, Column, Schema, Table, TableKind};
use sqlx::postgres::{PgConnectOptions, PgPool, PgPoolOptions, PgRow};
use sqlx::{Column as _, Executor as _, Row as _, TypeInfo as _, ValueRef as _};
use std::str::FromStr;
use std::time::Duration;

/// Pages of 500 rows plus one query tab at a time: a small pool is enough,
/// and it keeps the connection count polite on shared servers.
const MAX_CONNECTIONS: u32 = 4;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

pub struct PostgresConnection {
    pool: PgPool,
}

impl PostgresConnection {
    /// Connect from a `postgres://user:password@host:port/database` URL.
    pub async fn connect(url: &str) -> Result<Self> {
        let options = PgConnectOptions::from_str(url)
            .with_context(|| format!("not a valid PostgreSQL URL: {url}"))?;
        Self::connect_with(options).await
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
        Self::connect_with(options).await
    }

    async fn connect_with(options: PgConnectOptions) -> Result<Self> {
        let host = options.get_host().to_string();
        let pool = PgPoolOptions::new()
            .max_connections(MAX_CONNECTIONS)
            .acquire_timeout(CONNECT_TIMEOUT)
            .connect_with(options)
            .await
            .with_context(|| format!("failed to connect to {host}"))?;
        Ok(Self { pool })
    }
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

    async fn execute(&self, sql: &str) -> Result<QueryResult> {
        let rows: Vec<PgRow> = sqlx::query(sql).fetch_all(&self.pool).await?;

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

    async fn apply(&self, _changes: &[RowChange]) -> Result<u64> {
        anyhow::bail!("in-place editing is not implemented yet (Phase 2)")
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
        let conn = PostgresConnection::connect(&url).await.unwrap();

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
}
