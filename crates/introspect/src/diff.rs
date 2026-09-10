//! What changed between two readings of a catalog.
//!
//! A `CREATE`, `ALTER` or `DROP` answers with no rows and no count, so the
//! only honest report of what it did is the catalog before it against the
//! catalog after it. This module is that comparison: tables that appeared
//! or went, and for a table that stayed, the columns that appeared, went,
//! or changed type, nullability or default.
//!
//! It is pure — two catalogs in, a delta out — so the rules are argued with
//! in a test rather than in a running window. Order is the *after*
//! catalog's for what exists, and the *before* catalog's for what does not,
//! so a delta reads in the order the sidebar does.
//!
//! **A table with a different kind is a drop and a create**, not an
//! alteration: nothing in SQL turns a table into a view in place, and a
//! view that replaced a table of the same name is two events.

use crate::{Catalog, Column, Table, TableKind};

/// Everything that differs between two catalogs, relation by relation.
/// Empty means the two describe the same tables and columns.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Delta {
    pub relations: Vec<RelationDelta>,
}

impl Delta {
    pub fn is_empty(&self) -> bool {
        self.relations.is_empty()
    }

    /// Whether anything in the delta is a removal — a table or a column
    /// that is not there any more. It is what decides whether "cannot be
    /// recovered" is worth saying.
    pub fn drops_anything(&self) -> bool {
        self.relations
            .iter()
            .any(|relation| match &relation.change {
                RelationChange::Dropped { .. } => true,
                RelationChange::Added { .. } => false,
                RelationChange::Altered { columns } => columns
                    .iter()
                    .any(|column| matches!(column, ColumnDelta::Dropped(_))),
            })
    }

    /// The names of every column and table the delta removes, qualified
    /// the way the sidebar shows them. Empty when nothing was dropped.
    pub fn dropped_names(&self) -> Vec<String> {
        let mut names = Vec::new();
        for relation in &self.relations {
            match &relation.change {
                RelationChange::Dropped { .. } => {
                    names.push(format!("{}.{}", relation.schema, relation.name));
                }
                RelationChange::Added { .. } => {}
                RelationChange::Altered { columns } => {
                    for column in columns {
                        if let ColumnDelta::Dropped(column) = column {
                            names.push(format!("{}.{}", relation.name, column.name));
                        }
                    }
                }
            }
        }
        names
    }
}

/// One relation that differs.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RelationDelta {
    pub schema: String,
    pub name: String,
    pub kind: TableKind,
    pub change: RelationChange,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RelationChange {
    /// The relation is new. Its columns come along, so the report can say
    /// how many it opened with.
    Added { columns: Vec<Column> },
    /// The relation is gone. Its columns come along too — the report says
    /// what went with it.
    Dropped { columns: Vec<Column> },
    /// The relation stayed and its columns did not.
    Altered { columns: Vec<ColumnDelta> },
}

/// One column of a relation that stayed, and what happened to it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ColumnDelta {
    Added(Column),
    Dropped(Column),
    /// The same name, a different type, nullability or default. Both
    /// readings come along so the report can say `text → integer`.
    Changed {
        before: Column,
        after: Column,
    },
}

/// Compare two readings of one database.
pub fn diff(before: &Catalog, after: &Catalog) -> Delta {
    let mut relations = Vec::new();

    // What exists now: new, or altered.
    for schema in &after.schemas {
        let previous = before.schemas.iter().find(|s| s.name == schema.name);
        for table in &schema.tables {
            let was = previous.and_then(|schema| {
                schema
                    .tables
                    .iter()
                    .find(|t| t.name == table.name && t.kind == table.kind)
            });
            match was {
                None => relations.push(RelationDelta {
                    schema: schema.name.clone(),
                    name: table.name.clone(),
                    kind: table.kind,
                    change: RelationChange::Added {
                        columns: table.columns.clone(),
                    },
                }),
                Some(was) => {
                    let columns = diff_columns(was, table);
                    if !columns.is_empty() {
                        relations.push(RelationDelta {
                            schema: schema.name.clone(),
                            name: table.name.clone(),
                            kind: table.kind,
                            change: RelationChange::Altered { columns },
                        });
                    }
                }
            }
        }
    }

    // What existed and does not any more.
    for schema in &before.schemas {
        let current = after.schemas.iter().find(|s| s.name == schema.name);
        for table in &schema.tables {
            let still = current.is_some_and(|schema| {
                schema
                    .tables
                    .iter()
                    .any(|t| t.name == table.name && t.kind == table.kind)
            });
            if !still {
                relations.push(RelationDelta {
                    schema: schema.name.clone(),
                    name: table.name.clone(),
                    kind: table.kind,
                    change: RelationChange::Dropped {
                        columns: table.columns.clone(),
                    },
                });
            }
        }
    }

    Delta { relations }
}

/// The columns of one relation that differ between two readings, in the
/// order they have now — dropped ones last, in the order they had.
fn diff_columns(before: &Table, after: &Table) -> Vec<ColumnDelta> {
    let mut deltas = Vec::new();
    for column in &after.columns {
        match before.columns.iter().find(|c| c.name == column.name) {
            None => deltas.push(ColumnDelta::Added(column.clone())),
            Some(was) if !same_shape(was, column) => deltas.push(ColumnDelta::Changed {
                before: was.clone(),
                after: column.clone(),
            }),
            Some(_) => {}
        }
    }
    for column in &before.columns {
        if !after.columns.iter().any(|c| c.name == column.name) {
            deltas.push(ColumnDelta::Dropped(column.clone()));
        }
    }
    deltas
}

/// Whether two readings of one column describe the same column. The name
/// is what matched them; this is everything else.
fn same_shape(a: &Column, b: &Column) -> bool {
    a.data_type == b.data_type && a.nullable == b.nullable && a.default == b.default
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Schema;

    fn column(name: &str, data_type: &str) -> Column {
        Column {
            name: name.into(),
            data_type: data_type.into(),
            nullable: true,
            default: None,
        }
    }

    fn table(name: &str, columns: Vec<Column>) -> Table {
        Table {
            name: name.into(),
            kind: TableKind::Table,
            columns,
            primary_key: Vec::new(),
            approx_rows: None,
        }
    }

    fn catalog(tables: Vec<Table>) -> Catalog {
        Catalog {
            schemas: vec![Schema {
                name: "public".into(),
                tables,
            }],
        }
    }

    #[test]
    fn identical_catalogs_differ_in_nothing() {
        let a = catalog(vec![table("t", vec![column("a", "integer")])]);
        assert!(diff(&a, &a.clone()).is_empty());
    }

    #[test]
    fn a_new_table_is_added_with_its_columns() {
        let before = catalog(vec![]);
        let after = catalog(vec![table("t", vec![column("a", "integer")])]);
        let delta = diff(&before, &after);
        assert_eq!(delta.relations.len(), 1);
        let relation = &delta.relations[0];
        assert_eq!(
            (relation.schema.as_str(), relation.name.as_str()),
            ("public", "t")
        );
        assert!(matches!(
            &relation.change,
            RelationChange::Added { columns } if columns.len() == 1
        ));
        assert!(!delta.drops_anything());
    }

    #[test]
    fn a_missing_table_is_dropped_and_named() {
        let before = catalog(vec![table("t", vec![column("a", "integer")])]);
        let after = catalog(vec![]);
        let delta = diff(&before, &after);
        assert!(matches!(
            &delta.relations[0].change,
            RelationChange::Dropped { .. }
        ));
        assert!(delta.drops_anything());
        assert_eq!(delta.dropped_names(), vec!["public.t"]);
    }

    #[test]
    fn columns_are_added_dropped_and_changed_in_place() {
        let before = catalog(vec![table(
            "t",
            vec![
                column("keep", "integer"),
                column("widen", "integer"),
                column("gone", "text"),
            ],
        )]);
        let after = catalog(vec![table(
            "t",
            vec![
                column("keep", "integer"),
                column("widen", "bigint"),
                column("new", "boolean"),
            ],
        )]);
        let delta = diff(&before, &after);
        let RelationChange::Altered { columns } = &delta.relations[0].change else {
            panic!("expected an alteration");
        };
        // Present columns first, in the order they have now; the dropped
        // one last.
        assert!(matches!(&columns[0], ColumnDelta::Changed { before, after }
            if before.data_type == "integer" && after.data_type == "bigint"));
        assert!(matches!(&columns[1], ColumnDelta::Added(c) if c.name == "new"));
        assert!(matches!(&columns[2], ColumnDelta::Dropped(c) if c.name == "gone"));
        assert_eq!(delta.dropped_names(), vec!["t.gone"]);
    }

    #[test]
    fn a_change_of_nullability_or_default_counts() {
        let mut after_column = column("a", "integer");
        after_column.nullable = false;
        let before = catalog(vec![table("t", vec![column("a", "integer")])]);
        let after = catalog(vec![table("t", vec![after_column])]);
        assert_eq!(diff(&before, &after).relations.len(), 1);
    }

    #[test]
    fn a_table_that_became_a_view_is_a_drop_and_a_create() {
        let before = catalog(vec![table("t", vec![])]);
        let mut view = table("t", vec![]);
        view.kind = TableKind::View;
        let after = catalog(vec![view]);
        let delta = diff(&before, &after);
        assert_eq!(delta.relations.len(), 2);
        assert!(matches!(
            delta.relations[0].change,
            RelationChange::Added { .. }
        ));
        assert!(matches!(
            delta.relations[1].change,
            RelationChange::Dropped { .. }
        ));
    }

    #[test]
    fn a_schema_that_went_takes_its_tables_with_it() {
        let before = Catalog {
            schemas: vec![Schema {
                name: "scratch".into(),
                tables: vec![table("t", vec![])],
            }],
        };
        let after = Catalog { schemas: vec![] };
        let delta = diff(&before, &after);
        assert_eq!(delta.dropped_names(), vec!["scratch.t"]);
    }
}
