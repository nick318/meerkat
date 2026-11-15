package introspect

import (
	"reflect"
	"testing"
)

func column(name, dataType string) Column {
	return Column{
		Name:     name,
		DataType: dataType,
		Nullable: true,
		Default:  nil,
	}
}

func table(name string, columns []Column) Table {
	return Table{
		Name:    name,
		Columns: columns,
	}
}

func catalog(tables []Table) Catalog {
	return Catalog{
		Schemas: []Schema{{
			Name:   "public",
			Tables: tables,
		}},
	}
}

func index(name, definition string) Index {
	return Index{
		Name:       name,
		Definition: definition,
		Unique:     false,
		Primary:    false,
	}
}

func TestIdenticalCatalogsDifferInNothing(t *testing.T) {
	a := catalog([]Table{table("t", []Column{column("a", "integer")})})
	b := a.Clone()
	delta := Diff(&a, &b)
	if !delta.IsEmpty() {
		t.Fatalf("delta = %+v", delta)
	}
}

func TestANewTableIsAddedWithItsColumns(t *testing.T) {
	before := catalog(nil)
	after := catalog([]Table{table("t", []Column{column("a", "integer")})})
	delta := Diff(&before, &after)
	if len(delta.Relations) != 1 {
		t.Fatalf("relations = %+v", delta.Relations)
	}
	relation := &delta.Relations[0]
	if relation.Schema != "public" || relation.Name != "t" {
		t.Fatalf("relation = (%q, %q)", relation.Schema, relation.Name)
	}
	if added, ok := relation.Change.(RelationAdded); !ok || len(added.Columns) != 1 {
		t.Fatalf("change = %+v", relation.Change)
	}
	if delta.DropsAnything() {
		t.Fatal("an addition drops something")
	}
}

func TestAMissingTableIsDroppedAndNamed(t *testing.T) {
	before := catalog([]Table{table("t", []Column{column("a", "integer")})})
	after := catalog(nil)
	delta := Diff(&before, &after)
	if _, ok := delta.Relations[0].Change.(RelationDropped); !ok {
		t.Fatalf("change = %+v", delta.Relations[0].Change)
	}
	if !delta.DropsAnything() {
		t.Fatal("a drop drops nothing")
	}
	if got := delta.DroppedNames(); !reflect.DeepEqual(got, []string{"public.t"}) {
		t.Fatalf("dropped names = %v", got)
	}
}

func TestColumnsAreAddedDroppedAndChangedInPlace(t *testing.T) {
	before := catalog([]Table{table(
		"t",
		[]Column{
			column("keep", "integer"),
			column("widen", "integer"),
			column("gone", "text"),
		},
	)})
	after := catalog([]Table{table(
		"t",
		[]Column{
			column("keep", "integer"),
			column("widen", "bigint"),
			column("new", "boolean"),
		},
	)})
	delta := Diff(&before, &after)
	altered, ok := delta.Relations[0].Change.(RelationAltered)
	if !ok {
		t.Fatal("expected an alteration")
	}
	columns := altered.Columns
	// Present columns first, in the order they have now; the dropped
	// one last.
	if c, ok := columns[0].(ColumnChanged); !ok || c.Before.DataType != "integer" || c.After.DataType != "bigint" {
		t.Fatalf("columns[0] = %+v", columns[0])
	}
	if c, ok := columns[1].(ColumnAdded); !ok || c.Column.Name != "new" {
		t.Fatalf("columns[1] = %+v", columns[1])
	}
	if c, ok := columns[2].(ColumnDropped); !ok || c.Column.Name != "gone" {
		t.Fatalf("columns[2] = %+v", columns[2])
	}
	if got := delta.DroppedNames(); !reflect.DeepEqual(got, []string{"t.gone"}) {
		t.Fatalf("dropped names = %v", got)
	}
}

func TestAChangeOfNullabilityOrDefaultCounts(t *testing.T) {
	afterColumn := column("a", "integer")
	afterColumn.Nullable = false
	before := catalog([]Table{table("t", []Column{column("a", "integer")})})
	after := catalog([]Table{table("t", []Column{afterColumn})})
	if got := len(Diff(&before, &after).Relations); got != 1 {
		t.Fatalf("relations = %d", got)
	}
}

func TestATableThatBecameAViewIsADropAndACreate(t *testing.T) {
	before := catalog([]Table{table("t", nil)})
	view := table("t", nil)
	view.Kind = TableKindView
	after := catalog([]Table{view})
	delta := Diff(&before, &after)
	if len(delta.Relations) != 2 {
		t.Fatalf("relations = %+v", delta.Relations)
	}
	if _, ok := delta.Relations[0].Change.(RelationAdded); !ok {
		t.Fatalf("relations[0] = %+v", delta.Relations[0].Change)
	}
	if _, ok := delta.Relations[1].Change.(RelationDropped); !ok {
		t.Fatalf("relations[1] = %+v", delta.Relations[1].Change)
	}
}

func TestASchemaThatWentTakesItsTablesWithIt(t *testing.T) {
	before := Catalog{
		Schemas: []Schema{{
			Name:   "scratch",
			Tables: []Table{table("t", nil)},
		}},
	}
	after := Catalog{Schemas: []Schema{}}
	delta := Diff(&before, &after)
	if got := delta.DroppedNames(); !reflect.DeepEqual(got, []string{"scratch.t"}) {
		t.Fatalf("dropped names = %v", got)
	}
}

func TestAnIndexOnATableThatStayedIsAnObject(t *testing.T) {
	before := catalog([]Table{table("t", nil)})
	withIndex := table("t", nil)
	withIndex.Indexes = append(withIndex.Indexes, index("t_a_idx", "btree (a)"))
	after := catalog([]Table{withIndex.Clone()})

	delta := Diff(&before, &after)
	// The table itself did not change, so it is not a relation delta.
	if len(delta.Relations) != 0 {
		t.Fatalf("relations = %+v", delta.Relations)
	}
	if len(delta.Objects) != 1 {
		t.Fatalf("objects = %+v", delta.Objects)
	}
	object := &delta.Objects[0]
	if object.Kind != ObjectKindIndex {
		t.Fatalf("kind = %v", object.Kind)
	}
	if object.Relation == nil || *object.Relation != "t" {
		t.Fatalf("relation = %v", object.Relation)
	}
	if added, ok := object.Change.(ObjectAdded); !ok || added.Detail != "btree (a)" {
		t.Fatalf("change = %+v", object.Change)
	}
	if delta.DropsAnything() {
		t.Fatal("an index drops something")
	}

	// The same name over another key reads as a change.
	widened := withIndex.Clone()
	widened.Indexes[0].Definition = "btree (a, b)"
	widenedCatalog := catalog([]Table{widened})
	delta = Diff(&after, &widenedCatalog)
	if changed, ok := delta.Objects[0].Change.(ObjectChanged); !ok ||
		changed.Before != "btree (a)" || changed.After != "btree (a, b)" {
		t.Fatalf("change = %+v", delta.Objects[0].Change)
	}

	// And going away is a drop, named under its table — but not data
	// lost: an index held no rows.
	delta = Diff(&after, &before)
	if _, ok := delta.Objects[0].Change.(ObjectDropped); !ok {
		t.Fatalf("change = %+v", delta.Objects[0].Change)
	}
	if got := delta.Objects[0].Qualified(); got != "t.t_a_idx" {
		t.Fatalf("qualified = %q", got)
	}
	if delta.DropsAnything() {
		t.Fatal("a dropped index drops something")
	}
	if got := delta.DroppedNames(); len(got) != 0 {
		t.Fatalf("dropped names = %v", got)
	}
}

func TestANewTableDoesNotListItsOwnIndexes(t *testing.T) {
	withKey := table("t", []Column{column("id", "integer")})
	withKey.Indexes = append(withKey.Indexes, index("t_pkey", "btree (id)"))
	before := catalog(nil)
	after := catalog([]Table{withKey})
	delta := Diff(&before, &after)
	if len(delta.Relations) != 1 {
		t.Fatalf("relations = %+v", delta.Relations)
	}
	if len(delta.Objects) != 0 {
		t.Fatalf("objects = %+v", delta.Objects)
	}
}

func TestOverloadsOfARoutineAreTwoObjects(t *testing.T) {
	routine := func(arguments string) Routine {
		bigint := "bigint"
		return Routine{
			Name:      "total",
			Kind:      RoutineKindFunction,
			Arguments: arguments,
			Returns:   &bigint,
		}
	}
	before := catalog(nil)
	before.Schemas[0].Routines = append(before.Schemas[0].Routines, routine("integer"))
	after := before.Clone()
	after.Schemas[0].Routines = append(after.Schemas[0].Routines, routine("integer, integer"))

	delta := Diff(&before, &after)
	if len(delta.Objects) != 1 {
		t.Fatalf("objects = %+v", delta.Objects)
	}
	if delta.Objects[0].Name != "total(integer, integer)" {
		t.Fatalf("name = %q", delta.Objects[0].Name)
	}
	if delta.Objects[0].Relation != nil {
		t.Fatalf("relation = %v", *delta.Objects[0].Relation)
	}
	if delta.IsEmpty() {
		t.Fatal("the delta is empty")
	}
}

func TestASchemaThatWentTakesItsSequencesWithIt(t *testing.T) {
	before := catalog(nil)
	before.Schemas[0].Sequences = append(before.Schemas[0].Sequences, Sequence{
		Name:     "order_seq",
		DataType: "bigint",
	})
	after := Catalog{Schemas: []Schema{}}
	delta := Diff(&before, &after)
	if len(delta.Objects) != 1 {
		t.Fatalf("objects = %+v", delta.Objects)
	}
	if got := delta.Objects[0].Qualified(); got != "public.order_seq" {
		t.Fatalf("qualified = %q", got)
	}
}
