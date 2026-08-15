//! SQLite driver, backed by sqlx.

use anyhow::Context as _;
use async_trait::async_trait;
use db_client::{Connection, QueryResult, Result, RowChange, Value};
use introspect::{Catalog, Column, Schema, Table, TableKind};
use sqlx::sqlite::{SqliteConnectOptions, SqlitePool, SqliteRow};
use sqlx::{Column as _, Row as _, TypeInfo as _, ValueRef as _};
use std::path::Path;
use std::str::FromStr;

pub struct SqliteConnection {
    pool: SqlitePool,
}

impl SqliteConnection {
    pub async fn open(path: &Path) -> Result<Self> {
        let options = SqliteConnectOptions::from_str(&format!("sqlite://{}", path.display()))?
            .create_if_missing(false);
        let pool = SqlitePool::connect_with(options)
            .await
            .with_context(|| format!("failed to open {}", path.display()))?;
        Ok(Self { pool })
    }

    pub async fn open_in_memory() -> Result<Self> {
        let pool = SqlitePool::connect("sqlite::memory:").await?;
        Ok(Self { pool })
    }
}

#[async_trait]
impl Connection for SqliteConnection {
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
            });
        }

        Ok(Catalog {
            schemas: vec![Schema { name: "main".to_string(), tables }],
        })
    }

    async fn execute(&self, sql: &str) -> Result<QueryResult> {
        let rows: Vec<SqliteRow> = sqlx::query(sql).fetch_all(&self.pool).await?;
        let Some(first) = rows.first() else {
            return Ok(QueryResult::default());
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
}
