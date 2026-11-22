package dbclient

import (
	"slices"
	"testing"
)

const users uint32 = 16400

func usersTarget() *EditTarget {
	return &EditTarget{
		OID: users, Schema: "public", Name: "users", Kind: 'r',
		Key: []int16{1},
		Columns: map[int16]EditColumn{
			1: {Name: "id", Type: "bigint"},
			2: {Name: "email", Type: "text"},
			3: {Name: "name", Type: "text"},
			4: {Name: "slug", Type: "text", Generated: true},
			5: {Name: "serial_no", Type: "bigint", IdentityAlways: true},
		},
	}
}

func col(attnum int16) ColumnOrigin { return ColumnOrigin{Table: users, Column: attnum} }

var expr = ColumnOrigin{}

func TestAResultOfOneTableWithItsKeyIsEditable(t *testing.T) {
	got := Editable([]ColumnOrigin{col(1), col(2), col(3)}, usersTarget(), false, false)
	if !got.OK() {
		t.Fatalf("blocker = %v", got.Blocker)
	}
	if !slices.Equal(got.Key, []int{0}) {
		t.Fatalf("key = %v", got.Key)
	}
	for i, role := range got.Columns {
		if !role.Editable {
			t.Fatalf("column %d locked: %q", i, role.Locked)
		}
	}
	if !got.Columns[0].Key || got.Columns[1].Key {
		t.Fatal("key flags wrong")
	}
}

func TestTheShapeOfTheResultDecidesBeforeTheSession(t *testing.T) {
	cases := []struct {
		name    string
		origins []ColumnOrigin
		target  *EditTarget
		want    Blocker
	}{
		{"nothing has a source", []ColumnOrigin{expr, expr}, nil, BlockedNoTable},
		{"two tables", []ColumnOrigin{col(1), {Table: 99, Column: 1}}, usersTarget(), BlockedTables},
		{"not read yet", []ColumnOrigin{col(1)}, nil, BlockedPending},
		{"a view", []ColumnOrigin{col(1)}, func() *EditTarget { t := usersTarget(); t.Kind = 'v'; return t }(), BlockedView},
		{"no key", []ColumnOrigin{col(1)}, func() *EditTarget { t := usersTarget(); t.Key = nil; return t }(), BlockedNoKey},
		{"key left out", []ColumnOrigin{col(2), col(3)}, usersTarget(), BlockedKeyMissing},
		{"a different relation answered", []ColumnOrigin{col(1)}, func() *EditTarget { t := usersTarget(); t.OID = 1; return t }(), BlockedGone},
	}
	for _, c := range cases {
		// Read-only and a failed transaction are both set: the shape
		// must still be what is reported.
		got := Editable(c.origins, c.target, true, true)
		if got.Blocker != c.want {
			t.Errorf("%s: blocker = %v, want %v", c.name, got.Blocker, c.want)
		}
	}
}

func TestTheSessionStateIsReportedOnAnEditableShape(t *testing.T) {
	origins := []ColumnOrigin{col(1), col(2)}
	if got := Editable(origins, usersTarget(), true, false); got.Blocker != BlockedReadOnly {
		t.Fatalf("read-only: %v", got.Blocker)
	}
	if got := Editable(origins, usersTarget(), false, true); got.Blocker != BlockedTxFailed {
		t.Fatalf("failed transaction: %v", got.Blocker)
	}
	// The roles are still worked out, so the headers can paint.
	if got := Editable(origins, usersTarget(), true, false); len(got.Columns) != 2 {
		t.Fatal("no roles on a read-only result")
	}
}

func TestTheMissingKeyColumnsAreNamed(t *testing.T) {
	target := usersTarget()
	target.Key = []int16{1, 3}
	got := Editable([]ColumnOrigin{col(2)}, target, false, false)
	if !slices.Equal(got.Missing, []string{"id", "name"}) {
		t.Fatalf("missing = %v", got.Missing)
	}
}

func TestSomeColumnsOfAnEditableResultAreLocked(t *testing.T) {
	got := Editable([]ColumnOrigin{col(1), col(2), col(2), expr, col(4), col(5)}, usersTarget(), false, false)
	if !got.OK() {
		t.Fatalf("blocker = %v", got.Blocker)
	}
	want := []string{"", "selected twice", "selected twice", "expression", "generated", "identity"}
	for i, role := range got.Columns {
		if role.Locked != want[i] {
			t.Errorf("column %d: locked %q, want %q", i, role.Locked, want[i])
		}
		if role.Editable != (want[i] == "") {
			t.Errorf("column %d: editable %v", i, role.Editable)
		}
	}
}

func TestARenamedColumnIsStillItsColumn(t *testing.T) {
	// `SELECT id, name AS customer` names the column differently, and
	// the attribute number still says which one it is.
	got := Editable([]ColumnOrigin{col(1), col(3)}, usersTarget(), false, false)
	if !got.Columns[1].Editable || got.Columns[1].Attnum != 3 {
		t.Fatalf("role = %+v", got.Columns[1])
	}
}

func TestACutCellIsRecorded(t *testing.T) {
	sink := NewRowSink(Limits{MaxBytes: 1 << 20, MaxCellBytes: 4})
	sink.Push([]Value{ValueText("ok"), ValueText("too long")})
	sink.Push([]Value{ValueBytes{1, 2, 3, 4, 5}, ValueInt(7)})
	result := sink.Finish(0)
	if len(result.Cut) != 2 || !result.Cut[CellAt{0, 1}] || !result.Cut[CellAt{1, 0}] {
		t.Fatalf("cut = %v", result.Cut)
	}
}

func TestNothingCutRecordsNothing(t *testing.T) {
	sink := NewRowSink(Limits{MaxBytes: 1 << 20, MaxCellBytes: 4})
	sink.Push([]Value{ValueText("ok")})
	if result := sink.Finish(0); result.Cut != nil {
		t.Fatalf("cut = %v", result.Cut)
	}
}
