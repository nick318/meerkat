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
//!
//! **Everything else the catalog carries is an object**: an index, a key,
//! a constraint or a trigger of a relation, and a sequence, a routine or a
//! type of a schema. They are compared by name and by `detail()` — the
//! line the sidebar paints beside them — and listed apart from the
//! relations in `Delta::objects`. The objects of a relation that came or
//! went are *not* listed: a `CREATE TABLE` with a key makes an index, and
//! reporting it beside the table would say one event twice.

use crate::{Catalog, Column, Schema, Table, TableKind};

/// Everything that differs between two catalogs, relation by relation,
/// then object by object. Empty means the two describe the same shape.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Delta {
    pub relations: Vec<RelationDelta>,
    pub objects: Vec<ObjectDelta>,
}

impl Delta {
    pub fn is_empty(&self) -> bool {
        self.relations.is_empty() && self.objects.is_empty()
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
            || self
                .objects
                .iter()
                .any(|object| matches!(object.change, ObjectChange::Dropped { .. }))
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
        for object in &self.objects {
            if matches!(object.change, ObjectChange::Dropped { .. }) {
                names.push(object.qualified());
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

/// What sort of thing an object is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ObjectKind {
    Index,
    ForeignKey,
    Constraint,
    Trigger,
    Sequence,
    Routine,
    Type,
}

impl ObjectKind {
    pub fn noun(self) -> &'static str {
        match self {
            ObjectKind::Index => "index",
            ObjectKind::ForeignKey => "foreign key",
            ObjectKind::Constraint => "constraint",
            ObjectKind::Trigger => "trigger",
            ObjectKind::Sequence => "sequence",
            ObjectKind::Routine => "routine",
            ObjectKind::Type => "type",
        }
    }

    pub fn plural(self) -> &'static str {
        match self {
            ObjectKind::Index => "indexes",
            ObjectKind::ForeignKey => "foreign keys",
            ObjectKind::Constraint => "constraints",
            ObjectKind::Trigger => "triggers",
            ObjectKind::Sequence => "sequences",
            ObjectKind::Routine => "routines",
            ObjectKind::Type => "types",
        }
    }
}

/// One object that came, went, or reads differently.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ObjectDelta {
    pub schema: String,
    /// The relation the object belongs to; `None` for one that sits in
    /// the schema itself.
    pub relation: Option<String>,
    pub kind: ObjectKind,
    /// Its name — a routine's signature, because overloads share a name
    /// and are two objects.
    pub name: String,
    pub change: ObjectChange,
}

impl ObjectDelta {
    /// The name qualified by what it sits in, the way `dropped_names`
    /// writes a column: `orders.orders_user_id_fkey`, `public.order_seq`.
    pub fn qualified(&self) -> String {
        match &self.relation {
            Some(relation) => format!("{relation}.{}", self.name),
            None => format!("{}.{}", self.schema, self.name),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ObjectChange {
    Added {
        detail: String,
    },
    Dropped {
        detail: String,
    },
    /// The same name, another definition: `btree (a)` became `btree (a, b)`.
    Changed {
        before: String,
        after: String,
    },
}

/// Compare two readings of one database.
pub fn diff(before: &Catalog, after: &Catalog) -> Delta {
    let mut relations = Vec::new();
    let mut objects = Vec::new();

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
                    diff_objects(
                        &schema.name,
                        Some(&table.name),
                        &table_objects(was),
                        &table_objects(table),
                        &mut objects,
                    );
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

    // What sits in the schemas themselves, for every schema that is there
    // now, and then for every one that went.
    for schema in &after.schemas {
        let was = before
            .schemas
            .iter()
            .find(|s| s.name == schema.name)
            .map(schema_objects)
            .unwrap_or_default();
        diff_objects(
            &schema.name,
            None,
            &was,
            &schema_objects(schema),
            &mut objects,
        );
    }
    for schema in &before.schemas {
        if !after.schemas.iter().any(|s| s.name == schema.name) {
            diff_objects(
                &schema.name,
                None,
                &schema_objects(schema),
                &[],
                &mut objects,
            );
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

    Delta { relations, objects }
}

/// An object as the diff compares it: what it is, its name, its detail.
type Object = (ObjectKind, String, String);

/// Everything a relation carries beside its columns.
fn table_objects(table: &Table) -> Vec<Object> {
    let mut objects = Vec::new();
    objects.extend((table.indexes.iter()).map(|i| (ObjectKind::Index, i.name.clone(), i.detail())));
    objects.extend(
        (table.foreign_keys.iter()).map(|k| (ObjectKind::ForeignKey, k.name.clone(), k.detail())),
    );
    objects.extend(
        (table.constraints.iter()).map(|c| (ObjectKind::Constraint, c.name.clone(), c.detail())),
    );
    objects
        .extend((table.triggers.iter()).map(|t| (ObjectKind::Trigger, t.name.clone(), t.detail())));
    objects
}

/// Everything a schema holds beside its relations.
fn schema_objects(schema: &Schema) -> Vec<Object> {
    let mut objects = Vec::new();
    objects.extend(
        (schema.sequences.iter()).map(|s| (ObjectKind::Sequence, s.name.clone(), s.detail())),
    );
    objects
        .extend((schema.routines.iter()).map(|r| (ObjectKind::Routine, r.signature(), r.detail())));
    objects.extend((schema.types.iter()).map(|t| (ObjectKind::Type, t.name.clone(), t.detail())));
    objects
}

/// The objects of one relation or one schema that differ, in the order
/// they have now — dropped ones last, in the order they had.
fn diff_objects(
    schema: &str,
    relation: Option<&str>,
    before: &[Object],
    after: &[Object],
    out: &mut Vec<ObjectDelta>,
) {
    let delta = |kind: ObjectKind, name: &str, change: ObjectChange| ObjectDelta {
        schema: schema.to_string(),
        relation: relation.map(str::to_string),
        kind,
        name: name.to_string(),
        change,
    };
    for (kind, name, detail) in after {
        match before.iter().find(|(k, n, _)| k == kind && n == name) {
            None => out.push(delta(
                *kind,
                name,
                ObjectChange::Added {
                    detail: detail.clone(),
                },
            )),
            Some((_, _, was)) if was != detail => out.push(delta(
                *kind,
                name,
                ObjectChange::Changed {
                    before: was.clone(),
                    after: detail.clone(),
                },
            )),
            Some(_) => {}
        }
    }
    for (kind, name, detail) in before {
        if !after.iter().any(|(k, n, _)| k == kind && n == name) {
            out.push(delta(
                *kind,
                name,
                ObjectChange::Dropped {
                    detail: detail.clone(),
                },
            ));
        }
    }
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
    use crate::{Index, Routine, RoutineKind, Sequence};

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
            columns,
            ..Default::default()
        }
    }

    fn catalog(tables: Vec<Table>) -> Catalog {
        Catalog {
            schemas: vec![Schema {
                name: "public".into(),
                tables,
                ..Default::default()
            }],
        }
    }

    fn index(name: &str, definition: &str) -> Index {
        Index {
            name: name.into(),
            definition: definition.into(),
            unique: false,
            primary: false,
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
                ..Default::default()
            }],
        };
        let after = Catalog { schemas: vec![] };
        let delta = diff(&before, &after);
        assert_eq!(delta.dropped_names(), vec!["scratch.t"]);
    }

    #[test]
    fn an_index_on_a_table_that_stayed_is_an_object() {
        let before = catalog(vec![table("t", vec![])]);
        let mut with_index = table("t", vec![]);
        with_index.indexes.push(index("t_a_idx", "btree (a)"));
        let after = catalog(vec![with_index.clone()]);

        let delta = diff(&before, &after);
        // The table itself did not change, so it is not a relation delta.
        assert!(delta.relations.is_empty());
        assert_eq!(delta.objects.len(), 1);
        let object = &delta.objects[0];
        assert_eq!(object.kind, ObjectKind::Index);
        assert_eq!(object.relation.as_deref(), Some("t"));
        assert!(matches!(&object.change, ObjectChange::Added { detail } if detail == "btree (a)"));
        assert!(!delta.drops_anything());

        // The same name over another key reads as a change.
        let mut widened = with_index.clone();
        widened.indexes[0].definition = "btree (a, b)".into();
        let delta = diff(&after, &catalog(vec![widened]));
        assert!(matches!(
            &delta.objects[0].change,
            ObjectChange::Changed { before, after }
                if before == "btree (a)" && after == "btree (a, b)"
        ));

        // And going away is a drop, named under its table.
        let delta = diff(&after, &before);
        assert!(delta.drops_anything());
        assert_eq!(delta.dropped_names(), vec!["t.t_a_idx"]);
    }

    #[test]
    fn a_new_table_does_not_list_its_own_indexes() {
        let mut with_key = table("t", vec![column("id", "integer")]);
        with_key.indexes.push(index("t_pkey", "btree (id)"));
        let delta = diff(&catalog(vec![]), &catalog(vec![with_key]));
        assert_eq!(delta.relations.len(), 1);
        assert!(delta.objects.is_empty());
    }

    #[test]
    fn overloads_of_a_routine_are_two_objects() {
        let routine = |arguments: &str| Routine {
            name: "total".into(),
            kind: RoutineKind::Function,
            arguments: arguments.into(),
            returns: Some("bigint".into()),
        };
        let mut before = catalog(vec![]);
        before.schemas[0].routines.push(routine("integer"));
        let mut after = before.clone();
        after.schemas[0].routines.push(routine("integer, integer"));

        let delta = diff(&before, &after);
        assert_eq!(delta.objects.len(), 1);
        assert_eq!(delta.objects[0].name, "total(integer, integer)");
        assert_eq!(delta.objects[0].relation, None);
        assert!(!delta.is_empty());
    }

    #[test]
    fn a_schema_that_went_takes_its_sequences_with_it() {
        let mut before = catalog(vec![]);
        before.schemas[0].sequences.push(Sequence {
            name: "order_seq".into(),
            data_type: "bigint".into(),
        });
        let delta = diff(&before, &Catalog { schemas: vec![] });
        assert_eq!(delta.dropped_names(), vec!["public.order_seq"]);
    }
}
