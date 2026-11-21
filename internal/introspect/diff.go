// What changed between two readings of a catalog.
//
// A `CREATE`, `ALTER` or `DROP` answers with no rows and no count, so the
// only honest report of what it did is the catalog before it against the
// catalog after it. This module is that comparison: tables that appeared
// or went, and for a table that stayed, the columns that appeared, went,
// or changed type, nullability or default.
//
// It is pure — two catalogs in, a delta out — so the rules are argued with
// in a test rather than in a running window. Order is the *after*
// catalog's for what exists, and the *before* catalog's for what does not,
// so a delta reads in the order the sidebar does.
//
// **A table with a different kind is a drop and a create**, not an
// alteration: nothing in SQL turns a table into a view in place, and a
// view that replaced a table of the same name is two events.
//
// **Everything else the catalog carries is an object**: an index, a key,
// a constraint or a trigger of a relation, and a sequence, a routine or a
// type of a schema. They are compared by name and by `detail()` — the
// line the sidebar paints beside them — and listed apart from the
// relations in `Delta::objects`. The objects of a relation that came or
// went are *not* listed: a `CREATE TABLE` with a key makes an index, and
// reporting it beside the table would say one event twice.

package introspect

import "fmt"

// Delta is everything that differs between two catalogs, relation by
// relation, then object by object. Empty means the two describe the same
// shape.
type Delta struct {
	Relations []RelationDelta
	Objects   []ObjectDelta
}

func (d *Delta) IsEmpty() bool {
	return len(d.Relations) == 0 && len(d.Objects) == 0
}

// DropsAnything says whether anything in the delta is a removal — a table
// or a column that is not there any more. It is what decides whether
// "cannot be recovered" is worth saying, so an object does not count: a
// dropped index or function held no rows, and a renamed one reads as a drop
// and an add.
func (d *Delta) DropsAnything() bool {
	for _, relation := range d.Relations {
		switch change := relation.Change.(type) {
		case RelationDropped:
			return true
		case RelationAdded:
		case RelationAltered:
			for _, column := range change.Columns {
				if _, ok := column.(ColumnDropped); ok {
					return true
				}
			}
		}
	}
	return false
}

// DroppedNames is the names of every column and table the delta removes,
// qualified the way the sidebar shows them. Empty when nothing was dropped.
// Objects are left out, for the reason `drops_anything` leaves them.
func (d *Delta) DroppedNames() []string {
	var names []string
	for _, relation := range d.Relations {
		switch change := relation.Change.(type) {
		case RelationDropped:
			names = append(names, fmt.Sprintf("%s.%s", relation.Schema, relation.Name))
		case RelationAdded:
		case RelationAltered:
			for _, column := range change.Columns {
				if column, ok := column.(ColumnDropped); ok {
					names = append(names, fmt.Sprintf("%s.%s", relation.Name, column.Column.Name))
				}
			}
		}
	}
	return names
}

// RelationDelta is one relation that differs.
type RelationDelta struct {
	Schema string
	Name   string
	Kind   TableKind
	Change RelationChange
}

// RelationChange is one of RelationAdded, RelationDropped or
// RelationAltered.
type RelationChange interface {
	isRelationChange()
}

// RelationAdded: the relation is new. Its columns come along, so the report
// can say how many it opened with.
type RelationAdded struct {
	Columns []Column
}

// RelationDropped: the relation is gone. Its columns come along too — the
// report says what went with it.
type RelationDropped struct {
	Columns []Column
}

// RelationAltered: the relation stayed and its columns did not.
type RelationAltered struct {
	Columns []ColumnDelta
}

func (RelationAdded) isRelationChange()   {}
func (RelationDropped) isRelationChange() {}
func (RelationAltered) isRelationChange() {}

// ColumnDelta is one column of a relation that stayed, and what happened to
// it: one of ColumnAdded, ColumnDropped or ColumnChanged.
type ColumnDelta interface {
	isColumnDelta()
}

type ColumnAdded struct {
	Column Column
}

type ColumnDropped struct {
	Column Column
}

// ColumnChanged: the same name, a different type, nullability or default.
// Both readings come along so the report can say `text → integer`.
type ColumnChanged struct {
	Before Column
	After  Column
}

func (ColumnAdded) isColumnDelta()   {}
func (ColumnDropped) isColumnDelta() {}
func (ColumnChanged) isColumnDelta() {}

// ObjectKind is what sort of thing an object is.
type ObjectKind int

const (
	ObjectKindIndex ObjectKind = iota
	ObjectKindForeignKey
	ObjectKindConstraint
	ObjectKindTrigger
	ObjectKindSequence
	ObjectKindRoutine
	ObjectKindType
	// ObjectKindPartition is a partition of a partitioned table: a table
	// of its own, but listed under its parent, so a partition that came
	// or went is news about the parent.
	ObjectKindPartition
)

func (k ObjectKind) Noun() string {
	switch k {
	case ObjectKindIndex:
		return "index"
	case ObjectKindForeignKey:
		return "foreign key"
	case ObjectKindConstraint:
		return "constraint"
	case ObjectKindTrigger:
		return "trigger"
	case ObjectKindSequence:
		return "sequence"
	case ObjectKindRoutine:
		return "routine"
	case ObjectKindType:
		return "type"
	case ObjectKindPartition:
		return "partition"
	}
	panic(fmt.Sprintf("introspect: unknown ObjectKind %d", int(k)))
}

func (k ObjectKind) Plural() string {
	switch k {
	case ObjectKindIndex:
		return "indexes"
	case ObjectKindForeignKey:
		return "foreign keys"
	case ObjectKindConstraint:
		return "constraints"
	case ObjectKindTrigger:
		return "triggers"
	case ObjectKindSequence:
		return "sequences"
	case ObjectKindRoutine:
		return "routines"
	case ObjectKindType:
		return "types"
	case ObjectKindPartition:
		return "partitions"
	}
	panic(fmt.Sprintf("introspect: unknown ObjectKind %d", int(k)))
}

// ObjectDelta is one object that came, went, or reads differently.
type ObjectDelta struct {
	Schema string
	// The relation the object belongs to; `None` for one that sits in
	// the schema itself.
	Relation *string
	Kind     ObjectKind
	// Its name — a routine's signature, because overloads share a name
	// and are two objects.
	Name   string
	Change ObjectChange
}

// Qualified is the name qualified by what it sits in, the way
// `dropped_names` writes a column: `orders.orders_user_id_fkey`,
// `public.order_seq`.
func (o *ObjectDelta) Qualified() string {
	if o.Relation != nil {
		return fmt.Sprintf("%s.%s", *o.Relation, o.Name)
	}
	return fmt.Sprintf("%s.%s", o.Schema, o.Name)
}

// ObjectChange is one of ObjectAdded, ObjectDropped or ObjectChanged.
type ObjectChange interface {
	isObjectChange()
}

type ObjectAdded struct {
	Detail string
}

type ObjectDropped struct {
	Detail string
}

// ObjectChanged: the same name, another definition: `btree (a)` became
// `btree (a, b)`.
type ObjectChanged struct {
	Before string
	After  string
}

func (ObjectAdded) isObjectChange()   {}
func (ObjectDropped) isObjectChange() {}
func (ObjectChanged) isObjectChange() {}

// Diff compares two readings of one database.
func Diff(before, after *Catalog) Delta {
	var relations []RelationDelta
	var objects []ObjectDelta

	// What exists now: new, or altered.
	for si := range after.Schemas {
		schema := &after.Schemas[si]
		previous := findSchema(before, schema.Name)
		for ti := range schema.Tables {
			table := &schema.Tables[ti]
			var was *Table
			if previous != nil {
				for wi := range previous.Tables {
					t := &previous.Tables[wi]
					if t.Name == table.Name && t.Kind == table.Kind {
						was = t
						break
					}
				}
			}
			if was == nil {
				relations = append(relations, RelationDelta{
					Schema: schema.Name,
					Name:   table.Name,
					Kind:   table.Kind,
					Change: RelationAdded{
						Columns: cloneColumns(table.Columns),
					},
				})
			} else {
				tableName := table.Name
				diffObjects(
					schema.Name,
					&tableName,
					tableObjects(was),
					tableObjects(table),
					&objects,
				)
				columns := diffColumns(was, table)
				if len(columns) != 0 {
					relations = append(relations, RelationDelta{
						Schema: schema.Name,
						Name:   table.Name,
						Kind:   table.Kind,
						Change: RelationAltered{Columns: columns},
					})
				}
			}
		}
	}

	// What sits in the schemas themselves, for every schema that is there
	// now, and then for every one that went.
	for si := range after.Schemas {
		schema := &after.Schemas[si]
		var was []object
		if previous := findSchema(before, schema.Name); previous != nil {
			was = schemaObjects(previous)
		}
		diffObjects(
			schema.Name,
			nil,
			was,
			schemaObjects(schema),
			&objects,
		)
	}
	for si := range before.Schemas {
		schema := &before.Schemas[si]
		if findSchema(after, schema.Name) == nil {
			diffObjects(
				schema.Name,
				nil,
				schemaObjects(schema),
				nil,
				&objects,
			)
		}
	}

	// What existed and does not any more.
	for si := range before.Schemas {
		schema := &before.Schemas[si]
		current := findSchema(after, schema.Name)
		for ti := range schema.Tables {
			table := &schema.Tables[ti]
			still := false
			if current != nil {
				for ci := range current.Tables {
					t := &current.Tables[ci]
					if t.Name == table.Name && t.Kind == table.Kind {
						still = true
						break
					}
				}
			}
			if !still {
				relations = append(relations, RelationDelta{
					Schema: schema.Name,
					Name:   table.Name,
					Kind:   table.Kind,
					Change: RelationDropped{
						Columns: cloneColumns(table.Columns),
					},
				})
			}
		}
	}

	return Delta{Relations: relations, Objects: objects}
}

// findSchema is `catalog.schemas.iter().find(|s| s.name == name)`.
func findSchema(catalog *Catalog, name string) *Schema {
	for i := range catalog.Schemas {
		if catalog.Schemas[i].Name == name {
			return &catalog.Schemas[i]
		}
	}
	return nil
}

func cloneColumns(columns []Column) []Column {
	if columns == nil {
		return nil
	}
	out := make([]Column, len(columns))
	for i := range columns {
		out[i] = columns[i].Clone()
	}
	return out
}

// object is an object as the diff compares it: what it is, its name, its
// detail.
type object struct {
	kind   ObjectKind
	name   string
	detail string
}

// tableObjects is everything a relation carries beside its columns.
func tableObjects(table *Table) []object {
	var objects []object
	for i := range table.Indexes {
		objects = append(objects, object{ObjectKindIndex, table.Indexes[i].Name, table.Indexes[i].Detail()})
	}
	for i := range table.ForeignKeys {
		objects = append(objects, object{ObjectKindForeignKey, table.ForeignKeys[i].Name, table.ForeignKeys[i].Detail()})
	}
	for i := range table.Constraints {
		objects = append(objects, object{ObjectKindConstraint, table.Constraints[i].Name, table.Constraints[i].Detail()})
	}
	for i := range table.Triggers {
		objects = append(objects, object{ObjectKindTrigger, table.Triggers[i].Name, table.Triggers[i].Detail()})
	}
	for _, p := range table.Partitions {
		objects = append(objects, object{ObjectKindPartition, p.Name, p.Bound})
	}
	return objects
}

// schemaObjects is everything a schema holds beside its relations.
func schemaObjects(schema *Schema) []object {
	var objects []object
	for i := range schema.Sequences {
		objects = append(objects, object{ObjectKindSequence, schema.Sequences[i].Name, schema.Sequences[i].Detail()})
	}
	for i := range schema.Routines {
		objects = append(objects, object{ObjectKindRoutine, schema.Routines[i].Signature(), schema.Routines[i].Detail()})
	}
	for i := range schema.Types {
		objects = append(objects, object{ObjectKindType, schema.Types[i].Name, schema.Types[i].Detail()})
	}
	return objects
}

// diffObjects appends the objects of one relation or one schema that
// differ, in the order they have now — dropped ones last, in the order they
// had.
func diffObjects(
	schema string,
	relation *string,
	before []object,
	after []object,
	out *[]ObjectDelta,
) {
	delta := func(kind ObjectKind, name string, change ObjectChange) ObjectDelta {
		return ObjectDelta{
			Schema:   schema,
			Relation: cloneString(relation),
			Kind:     kind,
			Name:     name,
			Change:   change,
		}
	}
	for _, now := range after {
		var was *object
		for i := range before {
			if before[i].kind == now.kind && before[i].name == now.name {
				was = &before[i]
				break
			}
		}
		switch {
		case was == nil:
			*out = append(*out, delta(
				now.kind,
				now.name,
				ObjectAdded{Detail: now.detail},
			))
		case was.detail != now.detail:
			*out = append(*out, delta(
				now.kind,
				now.name,
				ObjectChanged{Before: was.detail, After: now.detail},
			))
		}
	}
	for _, was := range before {
		still := false
		for _, now := range after {
			if now.kind == was.kind && now.name == was.name {
				still = true
				break
			}
		}
		if !still {
			*out = append(*out, delta(
				was.kind,
				was.name,
				ObjectDropped{Detail: was.detail},
			))
		}
	}
}

// diffColumns is the columns of one relation that differ between two
// readings, in the order they have now — dropped ones last, in the order
// they had.
func diffColumns(before, after *Table) []ColumnDelta {
	var deltas []ColumnDelta
	for ai := range after.Columns {
		column := &after.Columns[ai]
		var was *Column
		for bi := range before.Columns {
			if before.Columns[bi].Name == column.Name {
				was = &before.Columns[bi]
				break
			}
		}
		switch {
		case was == nil:
			deltas = append(deltas, ColumnAdded{Column: column.Clone()})
		case !sameShape(was, column):
			deltas = append(deltas, ColumnChanged{
				Before: was.Clone(),
				After:  column.Clone(),
			})
		}
	}
	for bi := range before.Columns {
		column := &before.Columns[bi]
		still := false
		for ai := range after.Columns {
			if after.Columns[ai].Name == column.Name {
				still = true
				break
			}
		}
		if !still {
			deltas = append(deltas, ColumnDropped{Column: column.Clone()})
		}
	}
	return deltas
}

// sameShape says whether two readings of one column describe the same
// column. The name is what matched them; this is everything else.
func sameShape(a, b *Column) bool {
	return a.DataType == b.DataType && a.Nullable == b.Nullable && eqString(a.Default, b.Default) &&
		eqString(a.Generated, b.Generated) && eqString(a.Identity, b.Identity)
}
