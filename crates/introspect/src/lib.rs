//! Database schema model, produced by driver introspection.
//!
//! Drivers fill this model from `information_schema` or native catalogs
//! (`pg_catalog`, `PRAGMA table_info`, ...). The UI renders it in the
//! schema tree and uses it to decide whether a result grid is editable.

use serde::{Deserialize, Serialize};

pub mod diff;

/// Everything we know about one connected database.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Catalog {
    pub schemas: Vec<Schema>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Schema {
    pub name: String,
    pub tables: Vec<Table>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Table {
    pub name: String,
    pub kind: TableKind,
    pub columns: Vec<Column>,
    /// Names of the primary-key columns, in key order. Empty when the
    /// table has no primary key; such tables are not editable in place.
    pub primary_key: Vec<String>,
    /// Estimated row count for the sidebar, from the engine's statistics
    /// (`pg_class.reltuples`). `None` when the engine has no estimate or
    /// the table was never analyzed — the UI then shows nothing rather
    /// than a misleading zero. Never use this for paging arithmetic that
    /// must be exact.
    pub approx_rows: Option<u64>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum TableKind {
    Table,
    View,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Column {
    pub name: String,
    /// Type name as the database reports it (e.g. `INTEGER`, `varchar(255)`).
    pub data_type: String,
    pub nullable: bool,
    pub default: Option<String>,
}

impl Table {
    pub fn has_primary_key(&self) -> bool {
        !self.primary_key.is_empty()
    }
}
