# Exported API: `internal/introspect` and `internal/dbclient`

Wave 1, agent B. Ported from `crates/introspect` and `crates/db_client`.
Wave 2 (drivers, storage) and Wave 3 (the app) code against this. The
source is the truth; this is the map. `go doc -all ./internal/dbclient`
prints the full doc comments.

Import paths:

```go
import (
	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"
)
```

## Conventions used in both packages

- Rust enum without data → `type X int` with `XVariant` constants, zero
  value = the Rust `#[default]` (or the first variant when there is none).
- Rust enum with data → a sealed interface plus one struct (or named type)
  per variant. Match with a type switch.
- `Option<T>` as a struct field → `*T`. `Option<T>` as a small return value
  → `(T, bool)`; `Result<Option<small>>` → `(T, bool, error)`;
  `Result<Option<struct>>` → `(*T, error)`.
- `async fn` → method with `ctx context.Context` first, returning `error`
  last.
- Rust methods with a default body in a trait → an embeddable
  `XDefaults` struct.

## `introspect`

### Catalog tree (JSON shape = the Rust build's `catalog_cache` rows)

```go
type Catalog struct {
	Schemas []Schema `json:"schemas"`
}

type Schema struct {
	Name      string     `json:"name"`
	Tables    []Table    `json:"tables"`
	Sequences []Sequence `json:"sequences"` // serde(default)
	Routines  []Routine  `json:"routines"`  // serde(default)
	Types     []UserType `json:"types"`     // serde(default)
}

type Table struct {
	Name        string       `json:"name"`
	Kind        TableKind    `json:"kind"`
	Columns     []Column     `json:"columns"`
	PrimaryKey  []string     `json:"primary_key"`
	ApproxRows  *uint64      `json:"approx_rows"` // estimate; never for paging
	Indexes     []Index      `json:"indexes"`      // serde(default)
	ForeignKeys []ForeignKey `json:"foreign_keys"` // serde(default)
	Constraints []Constraint `json:"constraints"`  // serde(default)
	Triggers    []Trigger    `json:"triggers"`     // serde(default)
}
func (t *Table) HasPrimaryKey() bool
func (t *Table) IsKey(column string) bool

type TableKind int // JSON "Table" | "View"
const ( TableKindTable TableKind = iota; TableKindView )

type Column struct {
	Name     string  `json:"name"`
	DataType string  `json:"data_type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default"`
}

type Index struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Unique     bool   `json:"unique"`
	Primary    bool   `json:"primary"`
}
func (i *Index) Detail() string

type ForeignKey struct {
	Name          string   `json:"name"`
	Columns       []string `json:"columns"`
	TargetSchema  string   `json:"target_schema"`
	TargetTable   string   `json:"target_table"`
	TargetColumns []string `json:"target_columns"`
}
func (k *ForeignKey) Detail() string

type ConstraintKind int // JSON "Unique" | "Check" | "Exclusion"
const ( ConstraintKindUnique ConstraintKind = iota; ConstraintKindCheck; ConstraintKindExclusion )

type Constraint struct {
	Name       string         `json:"name"`
	Kind       ConstraintKind `json:"kind"`
	Definition string         `json:"definition"`
}
func (c *Constraint) Detail() string

type Trigger struct {
	Name     string `json:"name"`
	Timing   string `json:"timing"`
	Function string `json:"function"`
}
func (t *Trigger) Detail() string

type Sequence struct {
	Name     string `json:"name"`
	DataType string `json:"data_type"`
}
func (s *Sequence) Detail() string

type RoutineKind int // JSON "Function" | "Procedure" | "Aggregate" | "Window"
const ( RoutineKindFunction RoutineKind = iota; RoutineKindProcedure; RoutineKindAggregate; RoutineKindWindow )
func (k RoutineKind) Noun() string

type Routine struct {
	Name      string      `json:"name"`
	Kind      RoutineKind `json:"kind"`
	Arguments string      `json:"arguments"`
	Returns   *string     `json:"returns"` // nil for a procedure
}
func (r *Routine) Signature() string
func (r *Routine) Detail() string

type TypeKind int // JSON "Enum" | "Domain" | "Composite" | "Range"
const ( TypeKindEnum TypeKind = iota; TypeKindDomain; TypeKindComposite; TypeKindRange )
func (k TypeKind) Noun() string

type UserType struct {
	Name       string   `json:"name"`
	Kind       TypeKind `json:"kind"`
	Definition string   `json:"definition"`
}
func (t *UserType) Detail() string
```

Every kind type also has `String()` (the variant name), `MarshalJSON` and
`UnmarshalJSON` (an unknown variant name is an error, as in serde).

Go-port additions (Rust got them from `#[derive]`):

```go
func (c *Catalog) Clone() Catalog      // deep copy
func (s *Schema) Clone() Schema
func (t *Table) Clone() Table
func (c *Column) Clone() Column
func (k *ForeignKey) Clone() ForeignKey
func (r *Routine) Clone() Routine
func (c *Column) Equal(other *Column) bool // derived PartialEq
```

`Catalog`, `Schema`, `Table` and `ForeignKey` have a `MarshalJSON` that
writes a nil slice as `[]` (serde writes an empty `Vec` as `[]`, and the
Rust build refuses `null`) and does not HTML-escape. **Storage must write
the catalog with an `Encoder` that has `SetEscapeHTML(false)`** (and trim
the trailing newline), or `CHECK (price > 0)` comes out as `>` — still
parseable by both builds, but not byte-identical to the Rust row.

### Diff

```go
func Diff(before, after *Catalog) Delta

type Delta struct {
	Relations []RelationDelta
	Objects   []ObjectDelta
}
func (d *Delta) IsEmpty() bool
func (d *Delta) DropsAnything() bool
func (d *Delta) DroppedNames() []string

type RelationDelta struct {
	Schema string
	Name   string
	Kind   TableKind
	Change RelationChange
}

type RelationChange interface{ isRelationChange() }
type RelationAdded   struct{ Columns []Column }
type RelationDropped struct{ Columns []Column }
type RelationAltered struct{ Columns []ColumnDelta }

type ColumnDelta interface{ isColumnDelta() }
type ColumnAdded   struct{ Column Column }
type ColumnDropped struct{ Column Column }
type ColumnChanged struct{ Before, After Column }

type ObjectKind int
const (
	ObjectKindIndex ObjectKind = iota
	ObjectKindForeignKey
	ObjectKindConstraint
	ObjectKindTrigger
	ObjectKindSequence
	ObjectKindRoutine
	ObjectKindType
)
func (k ObjectKind) Noun() string
func (k ObjectKind) Plural() string

type ObjectDelta struct {
	Schema   string
	Relation *string // nil: the object sits in the schema itself
	Kind     ObjectKind
	Name     string  // a routine's signature
	Change   ObjectChange
}
func (o *ObjectDelta) Qualified() string

type ObjectChange interface{ isObjectChange() }
type ObjectAdded   struct{ Detail string }
type ObjectDropped struct{ Detail string }
type ObjectChanged struct{ Before, After string }
```

## `dbclient`

### Profile, engine, transaction mode

```go
type Profile struct {
	Id       string  `json:"id"`
	Name     string  `json:"name"`
	Engine   Engine  `json:"engine"`
	Host     *string `json:"host"`
	Port     *uint16 `json:"port"`
	Database string  `json:"database"`
	User     *string `json:"user"`
	ReadOnly bool    `json:"read_only"` // missing key reads as true
}
func (p *Profile) UnmarshalJSON(data []byte) error

type Engine int // JSON "Sqlite" | "Postgres" | "Mysql"; String() = variant name
const ( EngineSqlite Engine = iota; EnginePostgres; EngineMysql )

type TxMode int // zero value = Auto; JSON "Auto" | "Manual"; String() = variant name
const ( TxModeAuto TxMode = iota; TxModeManual )
func (m TxMode) AsStr() string           // "auto" | "manual" — the stored word
func ParseTxMode(text *string) TxMode    // nil / unknown → TxModeAuto

type TxEnd int
const ( TxEndCommit TxEnd = iota; TxEndRollback )
func (e TxEnd) SQL() string // "COMMIT" | "ROLLBACK"
```

### Values and results

```go
type Value interface {
	SizeBytes() int          // budget in the Rust build's reckoning (Value = 32 bytes)
	Cap(max int) Value       // Text ends in "…" when cut, on a char boundary; Bytes cut bare
	Display() string         // grid text: "NULL", "true", f64 like Rust's to_string, "0x…" hex prefix
	Equal(other Value) bool  // derived PartialEq; never use == (ValueBytes panics)
	isValue()
}
type ValueNull  struct{}   // SQL NULL; a Value is never a nil interface
type ValueBool  bool
type ValueInt   int64
type ValueFloat float64
type ValueText  string
type ValueBytes []byte

type QueryResult struct {
	Columns      []string   // empty = no result set (a command); see RowsAffected
	Rows         [][]Value
	RowsAffected uint64     // read only where Columns is empty
	Truncated    bool
	Wire         Wire
}

type Wire struct {          // client-side timing, milliseconds
	LinkMs     *uint64
	FirstRowMs *uint64
	FetchMs    *uint64
}

type ServerTiming struct {  // from pg_stat_statements
	ExecMs float64
	PlanMs *float64
	Exact  bool
}

type Refusal int            // String() = variant name
const ( RefusalSyntax Refusal = iota; RefusalName )

type CheckError struct {
	Refusal Refusal
	Message string
	Offset  *int            // byte offset into the checked statement
}
```

### Limits and the row sink

```go
const MaxBytes int = 256 * 1024 * 1024
const MaxCellBytes int = 1024 * 1024

type Limits struct {
	MaxBytes     int
	MaxCellBytes int
}
func DefaultLimits() Limits // impl Default — the zero Limits caps at nothing

type RowSink struct{ /* unexported */ }
func NewRowSink(limits Limits) *RowSink
func (s *RowSink) Columns(columns []string)
func (s *RowSink) HasColumns() bool
func (s *RowSink) Push(values []Value) bool // false: budget spent, row not kept, stop reading
func (s *RowSink) Bytes() int
func (s *RowSink) Truncated() bool
func (s *RowSink) Finish(rowsAffected uint64) QueryResult // Wire left zero for the driver
```

### Edits, runs, stopping

```go
type RowChange interface{ isRowChange() }
type RowChangeUpdate struct {
	Table string
	Key   []ColumnValue // pk columns and CURRENT values
	Set   []ColumnEdit  // (column, old, new)
}
type RowChangeInsert struct {
	Table  string
	Values []ColumnValue
}
type RowChangeDelete struct {
	Table string
	Key   []ColumnValue
}
type ColumnValue struct { Column string; Value Value }           // Rust (String, Value)
type ColumnEdit  struct { Column string; Old, New Value }        // Rust (String, Value, Value)

type RunId int32 // Postgres backend pid

type Stop int    // String() = variant name
const ( StopCancel Stop = iota; StopTerminate )

const MaxSessions int = 8
```

### `Session` and `Connection`

```go
type Session interface {
	Backend() (RunId, bool)                                     // false: nothing to cancel
	Execute(ctx context.Context, sql string) (QueryResult, error)
	InTransaction(ctx context.Context) (bool, error)            // default: false, nil
	Introspect(ctx context.Context) (*introspect.Catalog, error) // default: nil, nil (fall back to the pool)
	Check(ctx context.Context, sql string) (*CheckError, error)  // default: nil, nil
	RelationExists(ctx context.Context, name string) (exists bool, known bool, err error) // default: false, false, nil
	Begin(ctx context.Context) error
	EndTransaction(ctx context.Context, how TxEnd) error
	ServerTiming(ctx context.Context) (*ServerTiming, error)    // default: nil, nil
	Close(ctx context.Context)
}

// Embed in a driver's session to get the five defaults above.
type SessionDefaults struct{}

type Connection interface {
	OpenSession(ctx context.Context) (Session, error)
	Introspect(ctx context.Context) (introspect.Catalog, error)
	ServerVersion(ctx context.Context) (string, error)
	Execute(ctx context.Context, sql string) (QueryResult, error)
	Check(ctx context.Context, sql string) (*CheckError, error)  // default: nil, nil
	RelationExists(ctx context.Context, name string) (exists bool, known bool, err error) // default: false, false, nil
	Stop(ctx context.Context, run RunId, how Stop) (bool, error) // default: false, nil
	Apply(ctx context.Context, changes []RowChange) (uint64, error)
}

// Embed in a driver's connection to get the three defaults above.
type ConnectionDefaults struct{}
```

Implementations must be safe for concurrent use (Rust `Send + Sync`).

### Why a connect failed (`connect.go`)

```go
type ConnectFailure int // String() = variant name
const (
	ConnectFailureUnreachable ConnectFailure = iota
	ConnectFailureRefused
	ConnectFailureTimeout
	ConnectFailureDns
	ConnectFailureAuth
	ConnectFailureNoDatabase
	ConnectFailureTls
	ConnectFailureStartupOption
	ConnectFailureOther
)
func (k ConnectFailure) Headline() string
func (k ConnectFailure) Hint() string

// Rust std::io::ErrorKind, the part the rules read (String() = variant name).
type ErrorKind int
const (
	ErrorKindOther ErrorKind = iota
	ErrorKindConnectionRefused
	ErrorKindConnectionReset
	ErrorKindConnectionAborted
	ErrorKindTimedOut
	ErrorKindHostUnreachable
	ErrorKindNetworkUnreachable
	ErrorKindInvalidData
	ErrorKindUnexpectedEof
)
// io::Error::kind() for a Go error: syscall.Errno / os.ErrDeadlineExceeded / net.Error.
// false = not a socket error (Io: nil in the evidence).
func IoErrorKind(err error) (ErrorKind, bool)

type ConnectEvidence struct {   // Rust db_client::ConnectEvidence (connect::Evidence)
	Sqlstate *string
	Io       *ErrorKind
	TimedOut bool
	Tls      bool
	Message  string
}
func ClassifyConnect(evidence ConnectEvidence) ConnectFailure // Rust classify_connect

type ConnectError struct {
	Kind   ConnectFailure
	Detail string
}
func NewConnectError(kind ConnectFailure, detail string) ConnectError
func ConnectErrorFromEvidence(evidence ConnectEvidence) ConnectError
func ConnectErrorOf(err error) ConnectError // errors.As; a stranger → Other with err.Error() as detail
func (e ConnectError) Error() string        // "<headline> · <detail>"
```

A driver returns a `ConnectError` **by value** (wrapped with `%w` as much
as it likes); `ConnectErrorOf` also reads back a `*ConnectError`.

Note for the Postgres driver: Go's resolver words a failed lookup as
`lookup <host>: no such host`, which none of the Rust needles in
`isLookupFailure` match (they are getaddrinfo's and Windows's wording). The
driver should spot a `*net.DNSError` and classify it as `Dns` itself (or
hand in evidence whose message carries resolver wording); the needles were
ported as they are.
