package dbclient

// Editing a result in place.
//
// **The server says where every column came from**, and that is the whole
// of what makes a result editable. A `RowDescription` carries, per column,
// the OID of the table it was read from and its attribute number — zero
// for an expression — so the rule reads the wire, never the SQL. Parsing
// the statement to find its table would be a second Postgres to keep in
// step with the real one, and every gap would be a write to the wrong row.
//
// A result is editable when every column that has a source comes from one
// table and that table's whole primary key is in it. [Editable] is that
// rule, a plain function argued with in a test. A join is never editable:
// a row of the result is not a row of either table.
//
// **A row is named by its primary key and nothing else**, as pgAdmin names
// it: the `WHERE` of an `UPDATE` or a `DELETE` is the key. A row somebody
// changed since it was read is overwritten; a row somebody deleted, or
// whose key changed, matches nothing, and the apply rolls back as a
// conflict.
//
// The origins name a table, never a copy of it, so a self-join passes the
// rule: `SELECT e.id, m.name FROM staff e JOIN staff m ON …` reads as one
// table with its key, and an edit to `m.name` lands on row `e.id`. pgAdmin
// has the same hole; closing it needs to know which copy each column came
// from, which the wire does not say.

import (
	"errors"
	"fmt"
	"strings"
)

// ColumnOrigin is where the server says a result column came from.
type ColumnOrigin struct {
	// The table's `pg_class` OID; 0 for an expression.
	Table uint32
	// The column's attribute number; 0 for an expression.
	Column int16
}

// CellAt is one cell of a result, by row and column index.
type CellAt struct {
	Row    int
	Column int
}

// EditTarget is what editing a result of one relation needs, read from
// the catalog by [Connection.EditTarget].
//
// It is read from the server rather than from the introspected catalog,
// because that one is keyed by name and may be stale, and the origins name
// an OID.
type EditTarget struct {
	OID    uint32
	Schema string
	Name   string
	// `pg_class.relkind`: `r` a table, `p` a partitioned table, `v` a
	// view, `m` a materialized view, `f` a foreign table.
	Kind byte
	// The primary key's attribute numbers, in key order. Empty when the
	// relation has none.
	Key []int16
	// Every live column, by attribute number.
	Columns map[int16]EditColumn
}

// EditColumn is one column of an [EditTarget].
type EditColumn struct {
	Name string
	// The type as `format_type` renders it, the way `psql` prints it.
	Type string
	// The type's OID, which says what format its values come back in.
	TypeOID uint32
	// A generated column takes no value the user writes.
	Generated bool
	// `GENERATED ALWAYS AS IDENTITY` refuses a written value without
	// `OVERRIDING SYSTEM VALUE`, which is not the user's to ask for here.
	IdentityAlways bool
	// The labels of an enum type, in sort order; nil for any other type.
	Labels []string
	// Whether the column is a boolean, which edits with three keys.
	Bool bool
}

// Qualified is the relation's name for a person to read: `schema.name`.
func (t *EditTarget) Qualified() string {
	return t.Schema + "." + t.Name
}

// Blocker is why a result cannot be edited, or [EditOK] when it can.
type Blocker int

const (
	// EditOK: the result takes an edit.
	EditOK Blocker = iota
	// BlockedNoTable: no column has a source. Nothing is said about it —
	// `SELECT 1` was never a candidate.
	BlockedNoTable
	// BlockedTables: the columns come from more than one table.
	BlockedTables
	// BlockedPending: the table has not been read yet.
	BlockedPending
	// BlockedGone: the server knows no such relation any more, or this
	// connection cannot see it (one an uncommitted transaction made).
	BlockedGone
	// BlockedView: a view, a materialized view or a foreign table.
	BlockedView
	// BlockedNoKey: the table has no primary key, so no `WHERE` names
	// exactly one row. `ctid` is not offered: it moves under an `UPDATE`.
	BlockedNoKey
	// BlockedKeyMissing: the table has a key, and the result left part of
	// it out.
	BlockedKeyMissing
	// BlockedReadOnly: the session starts read-only; the server would
	// refuse the write.
	BlockedReadOnly
	// BlockedTxFailed: the tab's transaction has failed and refuses every
	// statement until it is rolled back.
	BlockedTxFailed
)

// ColumnRole is what one result column is to an edit.
type ColumnRole struct {
	// The column's attribute number in the target; 0 when it is not one.
	Attnum int16
	// Whether a value typed into it is written back.
	Editable bool
	// Whether it is part of the primary key.
	Key bool
	// Why it is read-only, for the header's lock; "" when it is editable
	// or the whole result is not.
	Locked string
}

// Editability is the rule's answer for one result.
type Editability struct {
	Blocker Blocker
	// How many tables the result reads, for [BlockedTables].
	Tables int
	// The key columns the result left out, for [BlockedKeyMissing].
	Missing []string
	// The table the result edits; nil unless the result names one table.
	Target *EditTarget
	// One role per result column. Nil when no column has a source.
	Columns []ColumnRole
	// The result column of each key column, in key order.
	Key []int
}

// OK says the result takes an edit.
func (e Editability) OK() bool {
	return e.Blocker == EditOK
}

// SourceTables lists the distinct tables the columns came from, in the
// order their first column appears.
func SourceTables(origins []ColumnOrigin) []uint32 {
	var tables []uint32
	seen := map[uint32]bool{}
	for _, origin := range origins {
		if origin.Table == 0 || seen[origin.Table] {
			continue
		}
		seen[origin.Table] = true
		tables = append(tables, origin.Table)
	}
	return tables
}

// Editable decides whether a result can be edited, and per column what
// each one is. `target` is the relation [SourceTables] named, read by
// [Connection.EditTarget]; nil while that read is out.
//
// The shape of the result is judged first and the session's state after
// it, so a join on a read-only connection says "2 tables" — the reason
// that would still hold after the user turned writes on.
func Editable(origins []ColumnOrigin, target *EditTarget, readOnly, txFailed bool) Editability {
	tables := SourceTables(origins)
	switch {
	case len(tables) == 0:
		return Editability{Blocker: BlockedNoTable}
	case len(tables) > 1:
		return Editability{Blocker: BlockedTables, Tables: len(tables)}
	case target == nil:
		return Editability{Blocker: BlockedPending}
	case target.OID != tables[0]:
		return Editability{Blocker: BlockedGone}
	case target.Kind != 'r' && target.Kind != 'p':
		return Editability{Blocker: BlockedView, Target: target}
	case len(target.Key) == 0:
		return Editability{Blocker: BlockedNoKey, Target: target}
	}

	// A table column that appears twice is read-only in both places:
	// editing one copy would leave the other saying something else.
	count := map[int16]int{}
	for _, origin := range origins {
		if origin.Table == target.OID && origin.Column > 0 {
			count[origin.Column]++
		}
	}
	isKey := map[int16]bool{}
	for _, attnum := range target.Key {
		isKey[attnum] = true
	}
	columns := make([]ColumnRole, len(origins))
	for i, origin := range origins {
		if origin.Table != target.OID || origin.Column <= 0 {
			columns[i] = ColumnRole{Locked: "expression"}
			continue
		}
		column, ok := target.Columns[origin.Column]
		role := ColumnRole{Attnum: origin.Column, Key: isKey[origin.Column]}
		switch {
		case !ok:
			role.Locked = "dropped"
		case count[origin.Column] > 1:
			role.Locked = "selected twice"
		case column.Generated:
			role.Locked = "generated"
		case column.IdentityAlways:
			role.Locked = "identity"
		default:
			role.Editable = true
		}
		columns[i] = role
	}

	key := make([]int, 0, len(target.Key))
	var missing []string
	for _, attnum := range target.Key {
		at := -1
		for i, role := range columns {
			if role.Attnum == attnum {
				at = i
				break
			}
		}
		if at < 0 {
			missing = append(missing, target.Columns[attnum].Name)
			continue
		}
		key = append(key, at)
	}
	if len(missing) > 0 {
		return Editability{Blocker: BlockedKeyMissing, Missing: missing, Target: target}
	}

	result := Editability{Target: target, Columns: columns, Key: key}
	switch {
	case readOnly:
		result.Blocker = BlockedReadOnly
	case txFailed:
		result.Blocker = BlockedTxFailed
	}
	return result
}

// AttrValue is a value for one column of the target, by attribute number.
type AttrValue struct {
	Column int16
	Value  Value
}

// RowChange is one staged edit against the target: a [RowChangeUpdate],
// [RowChangeInsert] or [RowChangeDelete].
type RowChange interface {
	isRowChange()
}

// RowChangeUpdate writes `Set` into the row `Key` names.
type RowChangeUpdate struct {
	// The key columns and their values as the result read them.
	Key []AttrValue
	// The columns written and their new values: a [ValueText] the user
	// typed, or [ValueNull].
	Set []AttrValue
}

// RowChangeInsert adds a row. A column left out takes its default.
type RowChangeInsert struct {
	Values []AttrValue
}

// RowChangeDelete removes the row `Key` names.
type RowChangeDelete struct {
	Key []AttrValue
}

func (RowChangeUpdate) isRowChange() {}
func (RowChangeInsert) isRowChange() {}
func (RowChangeDelete) isRowChange() {}

// ApplyOutcome is what [Session.Apply] wrote.
type ApplyOutcome struct {
	// What the server returned for each change, in order: the row as it
	// now is for an update or an insert, in the order the caller asked
	// for its columns; nil for a delete.
	Rows [][]Value
	// Whether the apply opened its own transaction and committed it. False
	// inside a transaction the user holds, where nothing commits until
	// they say so.
	Committed bool
	// The returned cells the memory cap cut, by change and by position in
	// the columns asked for. Nil when nothing was cut.
	Cut map[CellAt]bool
}

// ApplyError is a refused apply. Nothing it was asked to write was kept.
type ApplyError struct {
	// The change that was refused, as an index into the changeset.
	Change int
	// How many changes the apply was asked to write.
	Of int
	// The change matched no row: somebody deleted it, or changed its key,
	// since it was read.
	Conflict bool
	// What the server said; nil for a conflict.
	Err error
}

func (e *ApplyError) Error() string {
	if e.Conflict {
		return fmt.Sprintf("change %d of %d: the row is no longer there", e.Change+1, e.Of)
	}
	return fmt.Sprintf("change %d of %d: %v", e.Change+1, e.Of, e.Err)
}

func (e *ApplyError) Unwrap() error {
	return e.Err
}

// ErrEditUnsupported is an engine that does not edit.
var ErrEditUnsupported = errors.New("this connection does not edit results")

// ErrTransactionFailed is an apply asked of a session whose transaction
// has already failed: the server would refuse every statement.
var ErrTransactionFailed = errors.New("the transaction has failed · roll back to edit")

// KeyText is a row's key as a person reads it — `id = 1043` — for the
// apply's answer and the conflict mark.
func KeyText(target *EditTarget, key []AttrValue) string {
	parts := make([]string, 0, len(key))
	for _, part := range key {
		parts = append(parts, target.Columns[part.Column].Name+" = "+part.Value.Display())
	}
	return strings.Join(parts, ", ")
}
