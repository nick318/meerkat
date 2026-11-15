package introspect

import (
	"encoding/json"
	"testing"
)

// A catalog an older build cached, before any of the objects were
// read, must still come back — with nothing in the new lists.
func TestACatalogCachedBeforeTheObjectsStillParses(t *testing.T) {
	data := `{"schemas":[{"name":"public","tables":[{"name":"t","kind":"Table",
            "columns":[],"primary_key":[],"approx_rows":null}]}]}`
	var catalog Catalog
	if err := json.Unmarshal([]byte(data), &catalog); err != nil {
		t.Fatal(err)
	}
	schema := &catalog.Schemas[0]
	if len(schema.Routines) != 0 {
		t.Fatalf("routines = %v", schema.Routines)
	}
	if len(schema.Tables[0].Indexes) != 0 {
		t.Fatalf("indexes = %v", schema.Tables[0].Indexes)
	}
}

func TestDetailsReadTheWayTheSidebarPaintsThem(t *testing.T) {
	index := Index{
		Name:       "users_pkey",
		Definition: "btree (id)",
		Unique:     true,
		Primary:    true,
	}
	if got := index.Detail(); got != "primary · btree (id)" {
		t.Fatalf("index detail = %q", got)
	}

	key := ForeignKey{
		Name:          "orders_user_id_fkey",
		Columns:       []string{"user_id"},
		TargetSchema:  "public",
		TargetTable:   "users",
		TargetColumns: []string{"id"},
	}
	if got := key.Detail(); got != "(user_id) → public.users(id)" {
		t.Fatalf("key detail = %q", got)
	}

	bigint := "bigint"
	routine := Routine{
		Name:      "total",
		Kind:      RoutineKindFunction,
		Arguments: "a integer",
		Returns:   &bigint,
	}
	if got := routine.Signature(); got != "total(a integer)" {
		t.Fatalf("signature = %q", got)
	}
	if got := routine.Detail(); got != "(a integer) → bigint" {
		t.Fatalf("routine detail = %q", got)
	}
	// Anything but a plain function says what it is.
	routine.Kind = RoutineKindProcedure
	routine.Returns = nil
	if got := routine.Detail(); got != "procedure · (a integer)" {
		t.Fatalf("procedure detail = %q", got)
	}

	trigger := Trigger{
		Name:     "audit",
		Timing:   "after update",
		Function: "",
	}
	if got := trigger.Detail(); got != "after update" {
		t.Fatalf("trigger detail = %q", got)
	}
}
