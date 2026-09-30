//! Database schema model, produced by driver introspection.
//!
//! Drivers fill this model from `information_schema` or native catalogs
//! (`pg_catalog`, `PRAGMA table_info`, ...). The UI renders it in the
//! schema tree and uses it to decide whether a result grid is editable.
//!
//! **Everything added after the first cut is `#[serde(default)]`.** The
//! catalog is cached in the local store as JSON and painted before the
//! connection lands, so a row an older build wrote must still parse: it
//! comes back with no indexes and no functions, and the introspection
//! behind it fills them in a moment later. A row that failed to parse
//! would cost the first paint instead.
//!
//! Each object carries a `detail()` — the one line the sidebar paints
//! beside its name — and the schema diff compares objects by that same
//! line, so "what the tree shows" and "what counts as changed" cannot
//! drift into two answers.

use serde::{Deserialize, Serialize};

pub mod diff;

/// Everything we know about one connected database.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Catalog {
    pub schemas: Vec<Schema>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Schema {
    pub name: String,
    pub tables: Vec<Table>,
    #[serde(default)]
    pub sequences: Vec<Sequence>,
    /// Functions, procedures and aggregates, overloads apart: two
    /// routines of one name differ in their `arguments`.
    #[serde(default)]
    pub routines: Vec<Routine>,
    /// Types the user made — enums, domains, composites, ranges. The
    /// row type every table carries is not one of them.
    #[serde(default)]
    pub types: Vec<UserType>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
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
    /// Every index, the primary key's own among them.
    #[serde(default)]
    pub indexes: Vec<Index>,
    #[serde(default)]
    pub foreign_keys: Vec<ForeignKey>,
    /// Unique, check and exclusion constraints. The primary key is
    /// `primary_key` and a foreign key is in `foreign_keys`, so neither
    /// is listed here a second time.
    #[serde(default)]
    pub constraints: Vec<Constraint>,
    #[serde(default)]
    pub triggers: Vec<Trigger>,
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
pub enum TableKind {
    #[default]
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

    /// Whether a column is part of the primary key.
    pub fn is_key(&self, column: &str) -> bool {
        self.primary_key.iter().any(|key| key == column)
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Index {
    pub name: String,
    /// The access method and the key, as the engine writes them after
    /// `USING`: `btree (email)`, `gin (tags) WHERE archived = false`.
    pub definition: String,
    pub unique: bool,
    /// The index that backs the primary key.
    pub primary: bool,
}

impl Index {
    pub fn detail(&self) -> String {
        match (self.primary, self.unique) {
            (true, _) => format!("primary · {}", self.definition),
            (false, true) => format!("unique · {}", self.definition),
            (false, false) => self.definition.clone(),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ForeignKey {
    pub name: String,
    /// The referencing columns, in key order.
    pub columns: Vec<String>,
    pub target_schema: String,
    pub target_table: String,
    /// The referenced columns, lined up with `columns`.
    pub target_columns: Vec<String>,
}

impl ForeignKey {
    pub fn detail(&self) -> String {
        format!(
            "({}) → {}.{}({})",
            self.columns.join(", "),
            self.target_schema,
            self.target_table,
            self.target_columns.join(", ")
        )
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum ConstraintKind {
    Unique,
    Check,
    Exclusion,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Constraint {
    pub name: String,
    pub kind: ConstraintKind,
    /// The constraint as the engine writes it: `CHECK (price > 0)`,
    /// `UNIQUE (email)`.
    pub definition: String,
}

impl Constraint {
    pub fn detail(&self) -> String {
        self.definition.clone()
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Trigger {
    pub name: String,
    /// When it fires, in words: `before insert or update`, `after delete
    /// · statement`. Empty when the engine does not say.
    pub timing: String,
    /// What it runs: `audit_row()`. Empty when the body is inline, as
    /// SQLite's always is.
    pub function: String,
}

impl Trigger {
    pub fn detail(&self) -> String {
        match (self.timing.is_empty(), self.function.is_empty()) {
            (false, false) => format!("{} · {}", self.timing, self.function),
            (false, true) => self.timing.clone(),
            (true, _) => self.function.clone(),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Sequence {
    pub name: String,
    /// `bigint`, `integer` or `smallint`.
    pub data_type: String,
}

impl Sequence {
    pub fn detail(&self) -> String {
        self.data_type.clone()
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum RoutineKind {
    Function,
    Procedure,
    Aggregate,
    Window,
}

impl RoutineKind {
    pub fn noun(self) -> &'static str {
        match self {
            RoutineKind::Function => "function",
            RoutineKind::Procedure => "procedure",
            RoutineKind::Aggregate => "aggregate",
            RoutineKind::Window => "window function",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Routine {
    pub name: String,
    pub kind: RoutineKind,
    /// The arguments that tell one overload from another, without their
    /// defaults: `a integer, b text`.
    pub arguments: String,
    /// What it returns: `integer`, `SETOF record`. `None` for a procedure.
    pub returns: Option<String>,
}

impl Routine {
    /// The routine's name with its arguments, which is what names an
    /// overload: `total(integer)`.
    pub fn signature(&self) -> String {
        format!("{}({})", self.name, self.arguments)
    }

    pub fn detail(&self) -> String {
        let shape = match &self.returns {
            Some(returns) => format!("({}) → {returns}", self.arguments),
            None => format!("({})", self.arguments),
        };
        match self.kind {
            RoutineKind::Function => shape,
            kind => format!("{} · {shape}", kind.noun()),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum TypeKind {
    Enum,
    Domain,
    Composite,
    Range,
}

impl TypeKind {
    pub fn noun(self) -> &'static str {
        match self {
            TypeKind::Enum => "enum",
            TypeKind::Domain => "domain",
            TypeKind::Composite => "composite",
            TypeKind::Range => "range",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct UserType {
    pub name: String,
    pub kind: TypeKind,
    /// What the type is made of: an enum's labels, a domain's base type,
    /// a composite's attributes, a range's subtype.
    pub definition: String,
}

impl UserType {
    pub fn detail(&self) -> String {
        if self.definition.is_empty() {
            self.kind.noun().to_string()
        } else {
            format!("{} · {}", self.kind.noun(), self.definition)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A catalog an older build cached, before any of the objects were
    /// read, must still come back — with nothing in the new lists.
    #[test]
    fn a_catalog_cached_before_the_objects_still_parses() {
        let json = r#"{"schemas":[{"name":"public","tables":[{"name":"t","kind":"Table",
            "columns":[],"primary_key":[],"approx_rows":null}]}]}"#;
        let catalog: Catalog = serde_json::from_str(json).unwrap();
        let schema = &catalog.schemas[0];
        assert!(schema.routines.is_empty());
        assert!(schema.tables[0].indexes.is_empty());
    }

    #[test]
    fn details_read_the_way_the_sidebar_paints_them() {
        let index = Index {
            name: "users_pkey".into(),
            definition: "btree (id)".into(),
            unique: true,
            primary: true,
        };
        assert_eq!(index.detail(), "primary · btree (id)");

        let key = ForeignKey {
            name: "orders_user_id_fkey".into(),
            columns: vec!["user_id".into()],
            target_schema: "public".into(),
            target_table: "users".into(),
            target_columns: vec!["id".into()],
        };
        assert_eq!(key.detail(), "(user_id) → public.users(id)");

        let mut routine = Routine {
            name: "total".into(),
            kind: RoutineKind::Function,
            arguments: "a integer".into(),
            returns: Some("bigint".into()),
        };
        assert_eq!(routine.signature(), "total(a integer)");
        assert_eq!(routine.detail(), "(a integer) → bigint");
        // Anything but a plain function says what it is.
        routine.kind = RoutineKind::Procedure;
        routine.returns = None;
        assert_eq!(routine.detail(), "procedure · (a integer)");

        let trigger = Trigger {
            name: "audit".into(),
            timing: "after update".into(),
            function: String::new(),
        };
        assert_eq!(trigger.detail(), "after update");
    }
}
