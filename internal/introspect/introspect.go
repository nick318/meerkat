// Package introspect is the database schema model, produced by driver
// introspection.
//
// Drivers fill this model from `information_schema` or native catalogs
// (`pg_catalog`, `PRAGMA table_info`, ...). The UI renders it in the
// schema tree and uses it to decide whether a result grid is editable.
//
// **Everything added after the first cut is `#[serde(default)]`.** The
// catalog is cached in the local store as JSON and painted before the
// connection lands, so a row an older build wrote must still parse: it
// comes back with no indexes and no functions, and the introspection
// behind it fills them in a moment later. A row that failed to parse
// would cost the first paint instead.
//
// Each object carries a `detail()` — the one line the sidebar paints
// beside its name — and the schema diff compares objects by that same
// line, so "what the tree shows" and "what counts as changed" cannot
// drift into two answers.
//
// Go port: `#[serde(default)]` is a plain field — a missing key leaves the
// zero value. The JSON shape is the Rust build's, byte for byte: field
// names in snake_case, enums as their variant name in a string, `None` as
// `null`, and an empty list as `[]` rather than Go's `null`, so a row this
// build writes is still one the Rust build reads.
package introspect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Catalog is everything we know about one connected database.
type Catalog struct {
	Schemas []Schema `json:"schemas"`
}

type Schema struct {
	Name   string  `json:"name"`
	Tables []Table `json:"tables"`
	// serde(default)
	Sequences []Sequence `json:"sequences"`
	// Functions, procedures and aggregates, overloads apart: two
	// routines of one name differ in their `arguments`.
	// serde(default)
	Routines []Routine `json:"routines"`
	// Types the user made — enums, domains, composites, ranges. The
	// row type every table carries is not one of them.
	// serde(default)
	Types []UserType `json:"types"`
}

type Table struct {
	Name    string    `json:"name"`
	Kind    TableKind `json:"kind"`
	Columns []Column  `json:"columns"`
	// Names of the primary-key columns, in key order. Empty when the
	// table has no primary key; such tables are not editable in place.
	PrimaryKey []string `json:"primary_key"`
	// Estimated row count for the sidebar, from the engine's statistics
	// (`pg_class.reltuples`). `None` when the engine has no estimate or
	// the table was never analyzed — the UI then shows nothing rather
	// than a misleading zero. Never use this for paging arithmetic that
	// must be exact.
	ApproxRows *uint64 `json:"approx_rows"`
	// Every index, the primary key's own among them.
	// serde(default)
	Indexes []Index `json:"indexes"`
	// serde(default)
	ForeignKeys []ForeignKey `json:"foreign_keys"`
	// Unique, check and exclusion constraints. The primary key is
	// `primary_key` and a foreign key is in `foreign_keys`, so neither
	// is listed here a second time.
	// serde(default)
	Constraints []Constraint `json:"constraints"`
	// serde(default)
	Triggers []Trigger `json:"triggers"`
}

type TableKind int

const (
	// The default.
	TableKindTable TableKind = iota
	TableKindView
)

var tableKindTags = []string{"Table", "View"}

func (k TableKind) String() string { return tagName(tableKindTags, int(k)) }

func (k TableKind) MarshalJSON() ([]byte, error) {
	return marshalTag(tableKindTags, int(k), "TableKind")
}

func (k *TableKind) UnmarshalJSON(data []byte) error {
	i, err := unmarshalTag(tableKindTags, data, "TableKind")
	if err == nil {
		*k = TableKind(i)
	}
	return err
}

type Column struct {
	Name string `json:"name"`
	// Type name as the database reports it (e.g. `INTEGER`, `varchar(255)`).
	DataType string  `json:"data_type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default"`
}

func (t *Table) HasPrimaryKey() bool {
	return len(t.PrimaryKey) != 0
}

// IsKey says whether a column is part of the primary key.
func (t *Table) IsKey(column string) bool {
	for _, key := range t.PrimaryKey {
		if key == column {
			return true
		}
	}
	return false
}

type Index struct {
	Name string `json:"name"`
	// The access method and the key, as the engine writes them after
	// `USING`: `btree (email)`, `gin (tags) WHERE archived = false`.
	Definition string `json:"definition"`
	Unique     bool   `json:"unique"`
	// The index that backs the primary key.
	Primary bool `json:"primary"`
}

func (i *Index) Detail() string {
	switch {
	case i.Primary:
		return fmt.Sprintf("primary · %s", i.Definition)
	case i.Unique:
		return fmt.Sprintf("unique · %s", i.Definition)
	default:
		return i.Definition
	}
}

type ForeignKey struct {
	Name string `json:"name"`
	// The referencing columns, in key order.
	Columns      []string `json:"columns"`
	TargetSchema string   `json:"target_schema"`
	TargetTable  string   `json:"target_table"`
	// The referenced columns, lined up with `columns`.
	TargetColumns []string `json:"target_columns"`
}

func (k *ForeignKey) Detail() string {
	return fmt.Sprintf(
		"(%s) → %s.%s(%s)",
		strings.Join(k.Columns, ", "),
		k.TargetSchema,
		k.TargetTable,
		strings.Join(k.TargetColumns, ", "),
	)
}

type ConstraintKind int

const (
	ConstraintKindUnique ConstraintKind = iota
	ConstraintKindCheck
	ConstraintKindExclusion
)

var constraintKindTags = []string{"Unique", "Check", "Exclusion"}

func (k ConstraintKind) String() string { return tagName(constraintKindTags, int(k)) }

func (k ConstraintKind) MarshalJSON() ([]byte, error) {
	return marshalTag(constraintKindTags, int(k), "ConstraintKind")
}

func (k *ConstraintKind) UnmarshalJSON(data []byte) error {
	i, err := unmarshalTag(constraintKindTags, data, "ConstraintKind")
	if err == nil {
		*k = ConstraintKind(i)
	}
	return err
}

type Constraint struct {
	Name string         `json:"name"`
	Kind ConstraintKind `json:"kind"`
	// The constraint as the engine writes it: `CHECK (price > 0)`,
	// `UNIQUE (email)`.
	Definition string `json:"definition"`
}

func (c *Constraint) Detail() string {
	return c.Definition
}

type Trigger struct {
	Name string `json:"name"`
	// When it fires, in words: `before insert or update`, `after delete
	// · statement`. Empty when the engine does not say.
	Timing string `json:"timing"`
	// What it runs: `audit_row()`. Empty when the body is inline, as
	// SQLite's always is.
	Function string `json:"function"`
}

func (t *Trigger) Detail() string {
	switch {
	case t.Timing != "" && t.Function != "":
		return fmt.Sprintf("%s · %s", t.Timing, t.Function)
	case t.Timing != "":
		return t.Timing
	default:
		return t.Function
	}
}

type Sequence struct {
	Name string `json:"name"`
	// `bigint`, `integer` or `smallint`.
	DataType string `json:"data_type"`
}

func (s *Sequence) Detail() string {
	return s.DataType
}

type RoutineKind int

const (
	RoutineKindFunction RoutineKind = iota
	RoutineKindProcedure
	RoutineKindAggregate
	RoutineKindWindow
)

var routineKindTags = []string{"Function", "Procedure", "Aggregate", "Window"}

func (k RoutineKind) String() string { return tagName(routineKindTags, int(k)) }

func (k RoutineKind) MarshalJSON() ([]byte, error) {
	return marshalTag(routineKindTags, int(k), "RoutineKind")
}

func (k *RoutineKind) UnmarshalJSON(data []byte) error {
	i, err := unmarshalTag(routineKindTags, data, "RoutineKind")
	if err == nil {
		*k = RoutineKind(i)
	}
	return err
}

func (k RoutineKind) Noun() string {
	switch k {
	case RoutineKindFunction:
		return "function"
	case RoutineKindProcedure:
		return "procedure"
	case RoutineKindAggregate:
		return "aggregate"
	case RoutineKindWindow:
		return "window function"
	}
	panic(fmt.Sprintf("introspect: unknown RoutineKind %d", int(k)))
}

type Routine struct {
	Name string      `json:"name"`
	Kind RoutineKind `json:"kind"`
	// The arguments that tell one overload from another, without their
	// defaults: `a integer, b text`.
	Arguments string `json:"arguments"`
	// What it returns: `integer`, `SETOF record`. `None` for a procedure.
	Returns *string `json:"returns"`
}

// Signature is the routine's name with its arguments, which is what names
// an overload: `total(integer)`.
func (r *Routine) Signature() string {
	return fmt.Sprintf("%s(%s)", r.Name, r.Arguments)
}

func (r *Routine) Detail() string {
	var shape string
	if r.Returns != nil {
		shape = fmt.Sprintf("(%s) → %s", r.Arguments, *r.Returns)
	} else {
		shape = fmt.Sprintf("(%s)", r.Arguments)
	}
	switch r.Kind {
	case RoutineKindFunction:
		return shape
	default:
		return fmt.Sprintf("%s · %s", r.Kind.Noun(), shape)
	}
}

type TypeKind int

const (
	TypeKindEnum TypeKind = iota
	TypeKindDomain
	TypeKindComposite
	TypeKindRange
)

var typeKindTags = []string{"Enum", "Domain", "Composite", "Range"}

func (k TypeKind) String() string { return tagName(typeKindTags, int(k)) }

func (k TypeKind) MarshalJSON() ([]byte, error) {
	return marshalTag(typeKindTags, int(k), "TypeKind")
}

func (k *TypeKind) UnmarshalJSON(data []byte) error {
	i, err := unmarshalTag(typeKindTags, data, "TypeKind")
	if err == nil {
		*k = TypeKind(i)
	}
	return err
}

func (k TypeKind) Noun() string {
	switch k {
	case TypeKindEnum:
		return "enum"
	case TypeKindDomain:
		return "domain"
	case TypeKindComposite:
		return "composite"
	case TypeKindRange:
		return "range"
	}
	panic(fmt.Sprintf("introspect: unknown TypeKind %d", int(k)))
}

type UserType struct {
	Name string   `json:"name"`
	Kind TypeKind `json:"kind"`
	// What the type is made of: an enum's labels, a domain's base type,
	// a composite's attributes, a range's subtype.
	Definition string `json:"definition"`
}

func (t *UserType) Detail() string {
	if t.Definition == "" {
		return t.Kind.Noun()
	}
	return fmt.Sprintf("%s · %s", t.Kind.Noun(), t.Definition)
}

// ---------------------------------------------------------------------------
// Go port: what Rust's derives gave for free.
//
// `#[derive(Clone)]` is a deep copy, and a Go struct copy shares its
// slices' backing arrays — so a catalog cloned and then edited would edit
// the original as well. These are the derives, written out.

// Clone is `#[derive(Clone)]`: a deep copy.
func (c *Catalog) Clone() Catalog {
	out := Catalog{}
	if c.Schemas != nil {
		out.Schemas = make([]Schema, len(c.Schemas))
		for i := range c.Schemas {
			out.Schemas[i] = c.Schemas[i].Clone()
		}
	}
	return out
}

// Clone is `#[derive(Clone)]`: a deep copy.
func (s *Schema) Clone() Schema {
	out := Schema{
		Name:      s.Name,
		Sequences: cloneSlice(s.Sequences),
		Types:     cloneSlice(s.Types),
	}
	if s.Tables != nil {
		out.Tables = make([]Table, len(s.Tables))
		for i := range s.Tables {
			out.Tables[i] = s.Tables[i].Clone()
		}
	}
	if s.Routines != nil {
		out.Routines = make([]Routine, len(s.Routines))
		for i := range s.Routines {
			out.Routines[i] = s.Routines[i].Clone()
		}
	}
	return out
}

// Clone is `#[derive(Clone)]`: a deep copy.
func (t *Table) Clone() Table {
	out := Table{
		Name:        t.Name,
		Kind:        t.Kind,
		PrimaryKey:  cloneSlice(t.PrimaryKey),
		Indexes:     cloneSlice(t.Indexes),
		Constraints: cloneSlice(t.Constraints),
		Triggers:    cloneSlice(t.Triggers),
	}
	if t.ApproxRows != nil {
		rows := *t.ApproxRows
		out.ApproxRows = &rows
	}
	if t.Columns != nil {
		out.Columns = make([]Column, len(t.Columns))
		for i := range t.Columns {
			out.Columns[i] = t.Columns[i].Clone()
		}
	}
	if t.ForeignKeys != nil {
		out.ForeignKeys = make([]ForeignKey, len(t.ForeignKeys))
		for i := range t.ForeignKeys {
			out.ForeignKeys[i] = t.ForeignKeys[i].Clone()
		}
	}
	return out
}

// Clone is `#[derive(Clone)]`: a deep copy.
func (c *Column) Clone() Column {
	out := *c
	out.Default = cloneString(c.Default)
	return out
}

// Clone is `#[derive(Clone)]`: a deep copy.
func (k *ForeignKey) Clone() ForeignKey {
	out := *k
	out.Columns = cloneSlice(k.Columns)
	out.TargetColumns = cloneSlice(k.TargetColumns)
	return out
}

// Clone is `#[derive(Clone)]`: a deep copy.
func (r *Routine) Clone() Routine {
	out := *r
	out.Returns = cloneString(r.Returns)
	return out
}

// Equal is `#[derive(PartialEq)]` on `Column`.
func (c *Column) Equal(other *Column) bool {
	return c.Name == other.Name &&
		c.DataType == other.DataType &&
		c.Nullable == other.Nullable &&
		eqString(c.Default, other.Default)
}

func cloneSlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	out := make([]T, len(s))
	copy(out, s)
	return out
}

func cloneString(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

// eqString compares two `Option<String>`s the way Rust's `==` does.
func eqString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ---------------------------------------------------------------------------
// Go port: serde's JSON shape.

// nonNil writes an empty list as `[]`, the way serde writes an empty `Vec`.
// Go writes a nil slice as `null`, which the Rust build would refuse.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (c Catalog) MarshalJSON() ([]byte, error) {
	type plain Catalog
	c.Schemas = nonNil(c.Schemas)
	return marshalPlain(plain(c))
}

func (s Schema) MarshalJSON() ([]byte, error) {
	type plain Schema
	s.Tables = nonNil(s.Tables)
	s.Sequences = nonNil(s.Sequences)
	s.Routines = nonNil(s.Routines)
	s.Types = nonNil(s.Types)
	return marshalPlain(plain(s))
}

func (t Table) MarshalJSON() ([]byte, error) {
	type plain Table
	t.Columns = nonNil(t.Columns)
	t.PrimaryKey = nonNil(t.PrimaryKey)
	t.Indexes = nonNil(t.Indexes)
	t.ForeignKeys = nonNil(t.ForeignKeys)
	t.Constraints = nonNil(t.Constraints)
	t.Triggers = nonNil(t.Triggers)
	return marshalPlain(plain(t))
}

func (k ForeignKey) MarshalJSON() ([]byte, error) {
	type plain ForeignKey
	k.Columns = nonNil(k.Columns)
	k.TargetColumns = nonNil(k.TargetColumns)
	return marshalPlain(plain(k))
}

// marshalPlain is `json.Marshal` without the HTML escaping: serde_json
// writes `<`, `>` and `&` as themselves, and `CHECK (price > 0)` must come
// out as the Rust build wrote it. The caller that writes the whole catalog
// has to turn the escaping off on its own encoder as well
// (`Encoder.SetEscapeHTML(false)`), or it escapes what this returns.
func marshalPlain(v any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func tagName(names []string, i int) string {
	if i >= 0 && i < len(names) {
		return names[i]
	}
	return fmt.Sprintf("%d", i)
}

// marshalTag writes a unit variant the way serde does: its name, in a
// string.
func marshalTag(names []string, i int, typ string) ([]byte, error) {
	if i < 0 || i >= len(names) {
		return nil, fmt.Errorf("introspect: %s has no variant %d", typ, i)
	}
	return json.Marshal(names[i])
}

// unmarshalTag reads a unit variant back. An unknown name is an error, as
// it is for serde: the row no longer parses.
func unmarshalTag(names []string, data []byte, typ string) (int, error) {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return 0, fmt.Errorf("introspect: %s: %w", typ, err)
	}
	for i, n := range names {
		if n == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown variant `%s` for %s, expected one of %s", name, typ, strings.Join(names, ", "))
}
