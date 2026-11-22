// Package dbclient is driver-agnostic database access.
//
// Every engine (SQLite, PostgreSQL, ...) implements [Connection].
// The rest of the app talks only to this interface, so the UI never knows
// which engine is behind a tab.
//
// Go port: an `async fn` of a trait is a method that takes a
// `context.Context` and returns `(T, error)`; `Arc<dyn Connection>` and
// `Arc<dyn Session>` are the interfaces themselves. A trait method with a
// default body has its default in [SessionDefaults] or
// [ConnectionDefaults], which a driver embeds so it only writes the
// methods it overrides, as a Rust `impl` did.
package dbclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nick318/meerkat/internal/introspect"
)

// `pub type Result<T> = anyhow::Result<T>;` has no Go counterpart: every
// fallible function returns `(T, error)`.

// Profile is a saved connection profile. The password is NOT stored here —
// it lives in the OS keychain, keyed by `id` (see the `secrets` crate).
type Profile struct {
	Id       string  `json:"id"`
	Name     string  `json:"name"`
	Engine   Engine  `json:"engine"`
	Host     *string `json:"host"`
	Port     *uint16 `json:"port"`
	Database string  `json:"database"`
	User     *string `json:"user"`
	// The connection string's parameters the profile has no field for —
	// `sslmode`, `sslrootcert`, `application_name`, `connect_timeout`,
	// `target_session_attrs`, `options` — query-encoded. A host may be a
	// list, `h1:5433,h2`, with `Port` nil.
	Params string `json:"params,omitempty"`
	// Whether the session this profile opens refuses to write. It is a
	// connection parameter, not a label: the driver asks the server for a
	// read-only session, so the server is what enforces it. A profile
	// this build reads without the flag is read-only, because the safe
	// reading of a missing answer is the careful one.
	//
	// serde(default = "read_only_default")
	ReadOnly bool `json:"read_only"`
}

func readOnlyDefault() bool {
	return true
}

// UnmarshalJSON is serde's `#[serde(default = "read_only_default")]`: a
// profile written without `read_only` reads as read-only.
func (p *Profile) UnmarshalJSON(data []byte) error {
	type plain Profile
	out := plain{ReadOnly: readOnlyDefault()}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*p = Profile(out)
	return nil
}

// TxMode is who ends a transaction: the server after every statement, or
// the user.
//
// `Auto` is what a connection does with nothing asked of it — every
// statement commits as it succeeds, which is Postgres's own default and
// the only thing a viewer needs. `Manual` holds one transaction open
// across the runs of a tab, so a set of statements lands together or not
// at all, and the user says which.
//
// It is a property of a **tab**, and of nothing else, because a
// transaction belongs to one connection and a tab is one connection. A
// connection has no say in it: "manual on prod" is a wish about one set of
// statements, not about every tab that database will ever open, and a
// setting that opened every tab holding a transaction would hold them open
// behind a user who wanted one. A tab opens on `Auto`, the query toolbar
// switches it, and the tab remembers its own mode.
type TxMode int

const (
	// Every statement commits on its own, as soon as it succeeds. The
	// default.
	TxModeAuto TxMode = iota
	// The app opens a transaction on the tab's next run and holds it until
	// the user commits or rolls it back.
	TxModeManual
)

// AsStr is the word the toolbar, the store and the settings note all use,
// so the mode cannot be called one thing on screen and another on disk.
func (m TxMode) AsStr() string {
	switch m {
	case TxModeAuto:
		return "auto"
	case TxModeManual:
		return "manual"
	}
	panic(fmt.Sprintf("dbclient: unknown TxMode %d", int(m)))
}

// ParseTxMode reads a stored mode. Anything unrecognised — and a tab an
// older build saved, which has nothing here at all — is `Auto`: the mode a
// tab has when nobody asked for the other one.
func ParseTxMode(text *string) TxMode {
	if text != nil && *text == "manual" {
		return TxModeManual
	}
	return TxModeAuto
}

var txModeTags = []string{"Auto", "Manual"}

// String is the derived `Debug`: the variant's name.
func (m TxMode) String() string { return tagName(txModeTags, int(m)) }

func (m TxMode) MarshalJSON() ([]byte, error) { return marshalTag(txModeTags, int(m), "TxMode") }

func (m *TxMode) UnmarshalJSON(data []byte) error {
	i, err := unmarshalTag(txModeTags, data, "TxMode")
	if err == nil {
		*m = TxMode(i)
	}
	return err
}

// TxEnd is how a transaction ends. The two are one statement apart and
// opposite in every other way, so the app names which one happened rather
// than saying "the transaction closed".
type TxEnd int

const (
	TxEndCommit TxEnd = iota
	TxEndRollback
)

func (e TxEnd) SQL() string {
	switch e {
	case TxEndCommit:
		return "COMMIT"
	case TxEndRollback:
		return "ROLLBACK"
	}
	panic(fmt.Sprintf("dbclient: unknown TxEnd %d", int(e)))
}

type Engine int

const (
	EngineSqlite Engine = iota
	EnginePostgres
	EngineMysql
)

var engineTags = []string{"Sqlite", "Postgres", "Mysql"}

// String is the derived `Debug`: the variant's name.
func (e Engine) String() string { return tagName(engineTags, int(e)) }

func (e Engine) MarshalJSON() ([]byte, error) { return marshalTag(engineTags, int(e), "Engine") }

func (e *Engine) UnmarshalJSON(data []byte) error {
	i, err := unmarshalTag(engineTags, data, "Engine")
	if err == nil {
		*e = Engine(i)
	}
	return err
}

// Value is one cell value, decoded into a small closed set the grid can
// render: one of [ValueNull], [ValueBool], [ValueInt], [ValueFloat],
// [ValueText] or [ValueBytes]. A Value is never a nil interface; SQL's
// NULL is `ValueNull{}`.
//
// Go port: Rust's `#[derive(PartialEq)]` is [Value.Equal] — `==` on two
// `Value`s holding `ValueBytes` would panic, a slice not being comparable.
type Value interface {
	// SizeBytes is roughly what this value costs the process, for
	// [Limits.MaxBytes]. The discriminant plus whatever the value put on
	// the heap; the `Vec` holding the row is not counted. It is a budget,
	// not an audit.
	SizeBytes() int
	// Cap cuts a value down to `max` bytes, for [Limits.MaxCellBytes].
	//
	// A cut `Text` ends in `…`, so a truncated cell says so rather than
	// quietly reading as the whole value. `Bytes` needs no marker:
	// [Value.Display] already renders a prefix and an ellipsis. The cut
	// lands on a character boundary, or the `String` would not be UTF-8.
	Cap(max int) Value
	// Display is the display form for the results grid.
	Display() string
	// Equal is the derived `PartialEq`: the same variant holding an equal
	// value. A float is compared with `==`, so NaN equals nothing.
	Equal(other Value) bool
	isValue()
}

type (
	ValueNull  struct{}
	ValueBool  bool
	ValueInt   int64
	ValueFloat float64
	ValueText  string
	ValueBytes []byte
)

// sizeOfValue is `std::mem::size_of::<Value>()` in the Rust build: the
// largest variant (`Vec<u8>`, 24 bytes) and the discriminant. The budget is
// kept in the Rust build's own reckoning so both builds cap a result at
// the same row.
const sizeOfValue = 32

// sizeOfVec is `std::mem::size_of::<Vec<Value>>()`: pointer, capacity,
// length.
const sizeOfVec = 24

func (ValueNull) isValue()  {}
func (ValueBool) isValue()  {}
func (ValueInt) isValue()   {}
func (ValueFloat) isValue() {}
func (ValueText) isValue()  {}
func (ValueBytes) isValue() {}

func (v ValueNull) SizeBytes() int  { return sizeBytes(v) }
func (v ValueBool) SizeBytes() int  { return sizeBytes(v) }
func (v ValueInt) SizeBytes() int   { return sizeBytes(v) }
func (v ValueFloat) SizeBytes() int { return sizeBytes(v) }
func (v ValueText) SizeBytes() int  { return sizeBytes(v) }
func (v ValueBytes) SizeBytes() int { return sizeBytes(v) }

func (v ValueNull) Cap(max int) Value  { return capValue(v, max) }
func (v ValueBool) Cap(max int) Value  { return capValue(v, max) }
func (v ValueInt) Cap(max int) Value   { return capValue(v, max) }
func (v ValueFloat) Cap(max int) Value { return capValue(v, max) }
func (v ValueText) Cap(max int) Value  { return capValue(v, max) }
func (v ValueBytes) Cap(max int) Value { return capValue(v, max) }

func (v ValueNull) Display() string  { return display(v) }
func (v ValueBool) Display() string  { return display(v) }
func (v ValueInt) Display() string   { return display(v) }
func (v ValueFloat) Display() string { return display(v) }
func (v ValueText) Display() string  { return display(v) }
func (v ValueBytes) Display() string { return display(v) }

func (v ValueNull) Equal(other Value) bool  { return equal(v, other) }
func (v ValueBool) Equal(other Value) bool  { return equal(v, other) }
func (v ValueInt) Equal(other Value) bool   { return equal(v, other) }
func (v ValueFloat) Equal(other Value) bool { return equal(v, other) }
func (v ValueText) Equal(other Value) bool  { return equal(v, other) }
func (v ValueBytes) Equal(other Value) bool { return equal(v, other) }

// sizeBytes is `Value::size_bytes`. A Go string has no capacity apart from
// its length, so `Text` counts its length; `Bytes` counts its capacity, as
// the Rust build does.
func sizeBytes(v Value) int {
	extra := 0
	switch v := v.(type) {
	case ValueText:
		extra = len(v)
	case ValueBytes:
		extra = cap(v)
	}
	return sizeOfValue + extra
}

// capValue is `Value::cap`.
func capValue(v Value, max int) Value {
	switch v := v.(type) {
	case ValueText:
		if len(v) > max {
			end := max
			for !isCharBoundary(string(v), end) {
				end--
			}
			return ValueText(string(v[:end]) + "…")
		}
	case ValueBytes:
		if len(v) > max {
			return v[:max]
		}
	}
	return v
}

// isCharBoundary is `str::is_char_boundary`.
func isCharBoundary(s string, index int) bool {
	if index == 0 || index == len(s) {
		return true
	}
	if index > len(s) {
		return false
	}
	return utf8.RuneStart(s[index])
}

// display is `Value::display`.
func display(v Value) string {
	switch v := v.(type) {
	case ValueNull:
		return "NULL"
	case ValueBool:
		return strconv.FormatBool(bool(v))
	case ValueInt:
		return strconv.FormatInt(int64(v), 10)
	case ValueFloat:
		return formatFloat(float64(v))
	case ValueText:
		return string(v)
	case ValueBytes:
		return `\x` + hexPrefix(v, 16)
	}
	panic(fmt.Sprintf("dbclient: unknown Value %T", v))
}

// FullText is a value written out whole, for what is read or copied
// rather than painted in a lane: ⌘C, the value card, the ⌘F search.
//
// It differs from [Value.Display] only for bytes. A cell shows the first
// sixteen of them, because a lane holds no more and hex-encoding a
// megabyte for every frame would freeze the window; a copy that stopped
// at sixteen bytes would silently hand over a different value. Bytes are
// written the way Postgres writes `bytea`, `\x` and the hex.
func FullText(v Value) string {
	if bytes, ok := v.(ValueBytes); ok {
		return `\x` + hex.EncodeToString(bytes)
	}
	return v.Display()
}

// formatFloat is the shortest digits that read back as the same number,
// never in exponent form, and `Infinity`, `-Infinity` and `NaN` — the
// spellings SQL itself reads back — for the three that are not numbers.
func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// equal is the derived `PartialEq`.
func equal(a, b Value) bool {
	switch a := a.(type) {
	case ValueNull:
		_, ok := b.(ValueNull)
		return ok
	case ValueBool:
		b, ok := b.(ValueBool)
		return ok && a == b
	case ValueInt:
		b, ok := b.(ValueInt)
		return ok && a == b
	case ValueFloat:
		b, ok := b.(ValueFloat)
		return ok && a == b
	case ValueText:
		b, ok := b.(ValueText)
		return ok && a == b
	case ValueBytes:
		b, ok := b.(ValueBytes)
		return ok && string(a) == string(b)
	}
	return false
}

func hexPrefix(bytes []byte, max int) string {
	var out strings.Builder
	for i, b := range bytes {
		if i >= max {
			break
		}
		fmt.Fprintf(&out, "%02x", b)
	}
	if len(bytes) > max {
		out.WriteString("…")
	}
	return out.String()
}

// QueryResult is one statement's answer.
//
// **`columns` is what says whether there was a result set at all.** A
// statement that changes rows describes no columns — Postgres answers the
// describe with `NoData` — so an empty `columns` means the statement came
// back as a count, and `rows_affected` is the whole of what it said. A
// `SELECT` that matched nothing still names its columns, so an empty
// result set and a command are never confused: the one paints a grid with
// headers and no rows, the other paints no grid.
//
// The count cannot stand in for that test on its own, because Postgres
// counts a `SELECT` too — the tag for one is `SELECT 5`. So
// `rows_affected` is only ever read where `columns` is empty.
type QueryResult struct {
	Columns []string
	Rows    [][]Value
	// What the server's own completion tag counted. Meaningful only where
	// `columns` is empty — see the note above.
	RowsAffected uint64
	// The statement's command tag, exactly as the server sent it: "SELECT
	// 5", "UPDATE 3", "COMMIT", "ROLLBACK". "" when the engine sends none
	// (SQLite), or when the statement never reached its end (a cancel, an
	// error).
	//
	// It is here for the one answer the count cannot give: a `COMMIT` the
	// user typed inside a transaction that had already failed comes back
	// with **no error** and the tag `ROLLBACK`, which is how Postgres says
	// nothing was kept. [Session.EndTransaction] reads the same tag for the
	// app's own commit. Nothing else should parse it — the count is in
	// `RowsAffected` and the verb is the app's to read off the SQL.
	Tag string
	// Set when the memory cap stopped the read before the server ran out
	// of rows. `false` means the result is the whole result.
	Truncated bool
	// Where the run's time went, as far as the client could see it.
	Wire Wire
	// What the server said beside the result, in the order it said it:
	// a `RAISE NOTICE`, a warning, a notice that `DROP … IF EXISTS` found
	// nothing, a notification on a channel the session listens on. A
	// `DO` block that only raises notices has nothing else to say, so
	// dropping these would leave its run with no answer at all.
	Messages []string
	// Origins says, per column, which table column the server says it
	// came from. Nil for an engine that does not say (SQLite).
	Origins []ColumnOrigin
	// Cut is the cells [Limits.MaxCellBytes] shortened. Such a cell is
	// not the value in the table, so it can be neither edited nor
	// compared against. Nil when nothing was cut.
	Cut map[CellAt]bool
}

// Refusal is why the server refused a statement it was asked to prepare.
//
// **The two are told apart because one of them can be wrong.** A syntax
// error is context-free: the scanner and the grammar refuse the same text
// on any connection, whoever is looking, so it is true of the statement
// itself. A name error is true of **this** connection — the one the check
// went down — and a caller that knows the buffer may know better. See
// [CheckError].
type Refusal int

const (
	// The scanner or the grammar would not read it.
	RefusalSyntax Refusal = iota
	// A relation, column, function or type the server could not resolve.
	RefusalName
)

var refusalTags = []string{"Syntax", "Name"}

// String is the derived `Debug`: the variant's name.
func (r Refusal) String() string { return tagName(refusalTags, int(r)) }

// CheckError is a statement the server refused to *parse*.
//
// **A name error is only as good as the connection it was asked on**, and
// that is what [Refusal] is for. The check resolves names against
// whatever connection carried it, so a temp table, a `SET search_path`
// and a table created by an earlier statement of the same buffer are all
// things it can be wrong about. Two rules keep it honest, and neither of
// them belongs to a driver: the check goes down the **tab's own session**
// when the tab has one, which is where a temp table and a `search_path`
// actually live; and the app drops name errors for statements that follow
// one which changes what names resolve to. What is left is a name the
// server would refuse the moment the statement ran.
type CheckError struct {
	// Which of the two this is, and so how much it can be trusted.
	Refusal Refusal
	// The server's own words, unabridged.
	Message string
	// Byte offset into the statement that was checked, where the server
	// put its cursor. `None` when it named no position — the message is
	// then all there is, and the caller marks the statement whole.
	//
	// Bytes, not characters: Postgres counts the position in characters
	// and the driver converts, because every offset above this layer is a
	// byte offset into the text the app holds.
	Offset *int
}

// Wire is what a run cost, timed by the **client**, around the wire.
//
// A wall clock around `execute` answers "how long until I could look at
// this", which is the right number to show and the wrong number to reason
// about: on a VPN most of it is the link, and the user cannot tell that
// from a slow server. These three split it as far as the client can see,
// with no help from the engine and no extra round trip.
//
// **The split is honest about what it cannot separate.** Postgres emits no
// row until it has one, so `first_row_ms` bounds the server's work — but
// for a plan that streams (a plain sequential scan) the server keeps
// working while the wire moves, so server time and fetch time genuinely
// overlap and no client-side clock can tell them apart. For a plan that
// blocks (a sort, a hash aggregate, `count(*)`) the first row comes at the
// end, and `first_row_ms` *is* the whole of the server's work.
//
// [ServerTiming] is the exact answer, when the server can be asked for
// one. This is the answer that is always there.
//
// Go port: Rust's `u128` milliseconds (`Duration::as_millis`) are `uint64`
// here — 584 million years of milliseconds.
type Wire struct {
	// One round trip to this server and back, measured on a statement
	// whose own cost is nothing — the driver's `pg_backend_pid()` trip,
	// which it makes anyway. So it is the link's latency alone, and it is
	// free: no round trip is added to measure it.
	LinkMs *uint64
	// Statement sent → the first row off the wire. Subtract `link_ms` and
	// what is left is the server's work, up to the caveat above.
	FirstRowMs *uint64
	// First row → last row: the result crossing the wire, and this process
	// decoding it. `None` when no row ever came back.
	FetchMs *uint64
}

// ServerTiming is what the **server** says one execution of a statement
// cost.
//
// The client cannot work this out, and the server will not volunteer it:
// the wire protocol carries no timing, `EXPLAIN ANALYZE` would mean
// rewriting the user's statement and running it twice, and the log has it
// but no client can read the log. `pg_stat_statements` is the one place a
// client can ask, so this is `None` wherever that extension is not
// installed.
//
// It is read **after the result is already on screen**, on a connection
// that is not the one that ran the statement, so asking for it costs the
// run nothing.
type ServerTiming struct {
	// Executor time, in milliseconds.
	ExecMs float64
	// Planner time, when the server tracks it. `pg_stat_statements`
	// reports `0` both for "planning was free" and for "planning was not
	// tracked", so a zero is dropped rather than shown as a measurement.
	PlanMs *float64
	// Whether this is **this** execution's own time.
	//
	// `pg_stat_statements` counts, it does not log: one row holds the
	// running totals for every execution of a statement, cluster-wide. The
	// driver subtracts the totals it saw last time, so one run between two
	// reads gives that run's own time exactly. It is `false` when there was
	// nothing to subtract from (a mean over every execution ever) or when
	// somebody else ran the same statement in the same window (a mean over
	// the few that landed there). Either way the number is worth showing
	// and must not be shown as a measurement of this run.
	Exact bool
}

// Limits is how much of a result the app will hold.
//
// The cap is on **memory, not rows**. A row count is only a proxy for what
// actually runs the process out of room, and a poor one: ten thousand
// two-column rows cost about a megabyte, and ten thousand rows of wide
// `jsonb` cost gigabytes. The same number cannot bound both, so the bound
// is the resource itself.
//
// A typed statement goes to the server verbatim — nothing rewrites it to
// add a `LIMIT`, because a rewrite breaks on CTEs, `UNION` and statements
// that are not `SELECT`, and it would put SQL in the history that the user
// never wrote. So the bound is on what comes *back*.
//
// Go port: the zero `Limits` caps everything at nothing; `impl Default` is
// [DefaultLimits].
type Limits struct {
	// Decoded bytes kept in one result.
	//
	// Sized so that a real working set fits without ever meeting the cap:
	// a row of a dozen ordinary columns costs roughly 500 bytes here (a
	// `Value` is 32 bytes whatever it holds, plus the row's own `Vec` and
	// whatever the strings put on the heap), so 120,000 such rows come to
	// about 60 MB — a quarter of the budget. What the cap is for is the
	// other shape: a `SELECT *` over a table of wide `jsonb` or `bytea`,
	// where a few thousand rows would take the process down.
	MaxBytes int
	// Bytes kept of any one value. One wide `bytea` or `jsonb` column must
	// not eat the whole budget, and a cell larger than this cannot be read
	// in a grid anyway.
	MaxCellBytes int
}

const MaxBytes int = 256 * 1024 * 1024
const MaxCellBytes int = 1024 * 1024

// DefaultLimits is `impl Default for Limits`.
func DefaultLimits() Limits {
	return Limits{
		MaxBytes:     MaxBytes,
		MaxCellBytes: MaxCellBytes,
	}
}

// RowSink collects a result as the driver streams it, and says when to
// stop.
//
// The caps live here rather than in each driver so that both engines
// answer to one policy, and so the rule can be tested without a server.
//
// The rule: a row that would carry the result *past* the cap is dropped,
// and the result is marked truncated. A result that ends exactly on the
// cap is therefore whole, not truncated. The first row is always kept, or
// a single huge row would come back as an empty grid.
type RowSink struct {
	limits    Limits
	columns   []string
	rows      [][]Value
	bytes     int
	truncated bool
	cut       map[CellAt]bool
}

func NewRowSink(limits Limits) *RowSink {
	return &RowSink{
		limits:    limits,
		columns:   nil,
		rows:      nil,
		bytes:     0,
		truncated: false,
	}
}

// Columns names the columns, which the driver reads off the first row it
// gets.
func (s *RowSink) Columns(columns []string) {
	s.columns = columns
}

func (s *RowSink) HasColumns() bool {
	return len(s.columns) != 0
}

// Push takes one decoded row. `false` means the budget is spent and the
// driver must stop reading — the row was not kept.
func (s *RowSink) Push(values []Value) bool {
	capped := make([]Value, len(values))
	var cut []int
	for i, value := range values {
		capped[i] = value.Cap(s.limits.MaxCellBytes)
		if WasCut(value, s.limits.MaxCellBytes) {
			cut = append(cut, i)
		}
	}
	values = capped
	// The row's own `Vec` counts as well. It is 24 bytes against a
	// narrow row's 32-per-value, so leaving it out would undercount a
	// two-column result by a third — and a narrow result is exactly the
	// shape that reaches a large row count.
	size := sizeOfVec
	for _, value := range values {
		size += value.SizeBytes()
	}

	if len(s.rows) != 0 && s.bytes+size > s.limits.MaxBytes {
		s.truncated = true
		return false
	}

	for _, column := range cut {
		if s.cut == nil {
			s.cut = map[CellAt]bool{}
		}
		s.cut[CellAt{Row: len(s.rows), Column: column}] = true
	}
	s.bytes += size
	s.rows = append(s.rows, values)
	return true
}

// WasCut says whether [Value.Cap] shortens this value.
func WasCut(v Value, max int) bool {
	switch v := v.(type) {
	case ValueText:
		return len(v) > max
	case ValueBytes:
		return len(v) > max
	}
	return false
}

// Bytes is what the result holds so far, by the same reckoning as the cap.
func (s *RowSink) Bytes() int {
	return s.bytes
}

func (s *RowSink) Truncated() bool {
	return s.truncated
}

// Finish hands the result over. `rows_affected` is the server's own count,
// off the statement's completion tag. The sink counts rows it kept, which
// is a different number and not the one to report: a statement that
// changed a thousand rows streams none of them back.
func (s *RowSink) Finish(rowsAffected uint64) QueryResult {
	return QueryResult{
		Columns:      s.columns,
		Rows:         s.rows,
		RowsAffected: rowsAffected,
		Truncated:    s.truncated,
		Cut:          s.cut,
		// The sink counts bytes, not seconds. The driver is what holds
		// the clock, because only the driver knows when the statement
		// went out, and it fills this in on the way back.
		Wire: Wire{},
	}
}

// RunId is a statement the server is running, as the server itself names
// it. PostgreSQL uses the backend process id; an engine with no way to
// reach into a running statement never hands one out.
type RunId int32

// Stop is how firmly to stop a run.
type Stop int

const (
	// Ask the server to abandon the statement. The connection lives, and
	// the statement comes back as an error.
	StopCancel Stop = iota
	// Close the whole backend. The connection dies with it, which is why
	// it is the second press and never the first.
	StopTerminate
)

var stopTags = []string{"Cancel", "Terminate"}

// String is the derived `Debug`: the variant's name.
func (s Stop) String() string { return tagName(stopTags, int(s)) }

// Session is one tab's connection to the server, held for as long as the
// tab is.
//
// **The tab is a session in the user's head, so it has to be one on the
// wire.** Statements sent on whichever pooled connection happened to be
// free cannot see each other's `SET`, cannot share a `BEGIN`, and cannot
// read a temp table the statement before it made. Nothing short of one
// connection per tab fixes that: replaying the settings would mean
// parsing the user's SQL, and no amount of parsing replays a temp table
// or an open transaction.
//
// A session is one connection, so it runs one statement at a time. That
// is already what a tab does — ⌘⏎ while a run is out does nothing.
//
// Go port: an implementation must be safe to share between goroutines
// (Rust's `Send + Sync`). The methods with a default body in Rust are in
// [SessionDefaults].
type Session interface {
	// Backend is the backend this session is on, known from the moment it
	// opens rather than one round trip into each run. An engine with
	// nothing to cancel reports `None` (false).
	Backend() (RunId, bool)

	// Execute runs one statement on this session's own connection.
	Execute(ctx context.Context, sql string) (QueryResult, error)

	// InTransaction says whether this connection is sitting inside a
	// transaction — one the user typed a `BEGIN` for, or one the app
	// opened because the tab is in [TxModeManual]. It is the one thing a
	// close cannot take back for them.
	//
	// The answer is worth **caching against the last run**, and the cache
	// is exact rather than approximate: the connection is pinned to one
	// tab, so nothing but that tab's own statements can change what it is
	// in. Ask once after each run and the answer holds until the next.
	//
	// **What the app sends down the session itself must not move the
	// answer.** After a DDL the app reads the catalog on this connection
	// ([Session.Introspect]), and it checks the buffer here as the user
	// types; neither opens or ends a transaction, so the answer has to be
	// the state between statements, never "a statement is running".
	//
	// An engine that cannot answer says `false` for ever, and the app then
	// has only what it sent to go on: no transaction bar, and no close
	// warning. Every engine that lets a tab hold a transaction open owes
	// this an answer.
	InTransaction(ctx context.Context) (bool, error)

	// Introspect reads the catalog **as this connection sees it**.
	//
	// It is the same model [Connection.Introspect] reads, asked on the
	// tab's own connection rather than the pool — and the difference is
	// the point: a `CREATE TABLE` inside an open transaction exists for
	// this connection alone until it commits, and a pooled read would
	// report the table as not there. The shell reads the catalog this way
	// after a shape-changing statement so it can say what changed, and
	// only takes the answer for the sidebar once nothing is uncommitted.
	//
	// `Ok(None)` (nil, nil) is an engine that cannot answer on a session;
	// the caller then falls back to the pool.
	Introspect(ctx context.Context) (*introspect.Catalog, error)

	// Check says whether one statement parses **on this connection**.
	//
	// The same question [Connection.Check] answers, asked where the
	// answer is right: a temp table this tab made, a `SET search_path` it
	// ran and a schema it created are all on this connection and on no
	// other. It costs the session one round trip and runs nothing.
	//
	// It waits for the connection like any other statement, so a caller
	// must not send one while a run is out — the check would queue behind
	// the very statement the user is waiting for.
	Check(ctx context.Context, sql string) (*CheckError, error)

	// RelationExists is the same question as [Connection.RelationExists],
	// down the tab's own connection — so a temp table it made and a
	// `search_path` it set are part of the answer.
	RelationExists(ctx context.Context, name string) (exists bool, known bool, err error)

	// Begin opens a transaction on this session.
	//
	// It is **not** `execute`. A transaction boundary is not a run: it has
	// no result to cap, no timing worth reporting to the user and no
	// columns to describe — and `execute` would pay a prepare round trip
	// asking the server what columns `COMMIT` returns.
	//
	// The caller must not send this on a session already inside a
	// transaction. Postgres answers a second `BEGIN` with a warning and
	// SQLite with an error, and neither is worth finding out: the app
	// knows what it opened, and [Session.InTransaction] is what
	// corrects it.
	Begin(ctx context.Context) error

	// EndTransaction ends the transaction this session is in, either way,
	// and answers with what the server **actually** did.
	//
	// The two can differ, and only one way round: a `COMMIT` of a
	// transaction that has already failed is not refused — Postgres
	// answers it with no error and the tag `ROLLBACK`, having thrown the
	// work away. So asking for [TxEndCommit] can come back as
	// [TxEndRollback], and the app must say "rolled back", not
	// "committed". An engine that cannot tell answers with `how`.
	//
	// Sent whether or not one is open, for the reason [Session.Close]
	// sends its `ROLLBACK` that way: outside a transaction the server says
	// so and carries on, which is cheaper than asking first.
	EndTransaction(ctx context.Context, how TxEnd) (TxEnd, error)

	// IsClosed says the session's connection is gone for good: its backend
	// was terminated, the server restarted, the network dropped, or
	// [Session.Close] already ran. Every statement on it would fail, so
	// the app drops the session and the next run opens a new one.
	//
	// It sends nothing. A connection whose peer vanished while it sat idle
	// is not known to be gone until something is sent down it, so this
	// turns true after the run that found out, not before it. While a run
	// is out it answers `false`: the connection is busy, and a dead one
	// is reported by the run itself.
	IsClosed() bool

	// ServerTiming is what the server says the statement this session ran
	// **last** cost.
	//
	// Asked after the result is on screen, never before it — the whole
	// point is that the number is worth having and not worth waiting for.
	// It goes out on a connection that is not this session's, so it
	// cannot queue behind the run it is about.
	//
	// `Ok(None)` (nil, nil) is the ordinary answer, not a failure: the
	// extension that keeps these numbers is not installed everywhere, and
	// a statement the server has not counted yet has nothing to report.
	//
	// It describes **one statement**, so a caller that sent a buffer of
	// several must not report it as the run's: only the last one is here.
	ServerTiming(ctx context.Context) (*ServerTiming, error)

	// Apply writes a changeset to `table` down this session, and answers
	// with what the server returned for each change. See [Apply] for the
	// transaction it runs in and what a refusal leaves behind.
	Apply(ctx context.Context, table *EditTarget, changes []RowChange, returning []int16) (ApplyOutcome, error)

	// Reload reads one row of `table` again, by its key, as the columns
	// `columns` name: the conflict's "reload row". Nil, nil when the row
	// is gone.
	Reload(ctx context.Context, table *EditTarget, key []AttrValue, columns []int16) ([]Value, error)

	// Close rolls back and hands the connection back. `execute` fails
	// afterwards.
	//
	// The rollback is not about the *server*: closing a socket already
	// rolls a transaction back, at once, with no timeout involved. It is
	// about the **pool** — the connection is about to be reused, and it
	// must not carry the user's half-finished transaction into whatever
	// acquires it next.
	Close(ctx context.Context)
}

// SessionDefaults holds the default bodies of [Session]'s methods. A
// driver embeds it and writes only the methods it overrides.
type SessionDefaults struct{}

func (SessionDefaults) InTransaction(context.Context) (bool, error) {
	return false, nil
}

func (SessionDefaults) Introspect(context.Context) (*introspect.Catalog, error) {
	return nil, nil
}

func (SessionDefaults) Check(context.Context, string) (*CheckError, error) {
	return nil, nil
}

func (SessionDefaults) RelationExists(context.Context, string) (bool, bool, error) {
	return false, false, nil
}

func (SessionDefaults) ServerTiming(context.Context) (*ServerTiming, error) {
	return nil, nil
}

// Apply is refused: an engine that does not edit says so rather than
// pretending to have written anything.
func (SessionDefaults) Apply(context.Context, *EditTarget, []RowChange, []int16) (ApplyOutcome, error) {
	return ApplyOutcome{}, ErrEditUnsupported
}

func (SessionDefaults) Reload(context.Context, *EditTarget, []AttrValue, []int16) ([]Value, error) {
	return nil, ErrEditUnsupported
}

// IsClosed is `false`: an engine that cannot see its connection die has
// only [Session.Close] to go on, and a driver that has a closed state of
// its own overrides this.
func (SessionDefaults) IsClosed() bool {
	return false
}

// MaxSessions is how many sessions may be open at once — one pinned
// connection each.
//
// It is here rather than in a driver because both ends need the same
// number: the driver sizes its session pool by it, and the app has to
// know when it is about to ask for one too many. Waiting for the pool to
// time out instead would be a ten-second pause and then a message about
// connections, for something the app could see coming.
const MaxSessions int = 8

// Connection is a live connection to one database.
//
// Go port: an implementation must be safe to share between goroutines
// (Rust's `Send + Sync`). The methods with a default body in Rust are in
// [ConnectionDefaults].
type Connection interface {
	// OpenSession opens a session for one tab. See [Session] for why a tab
	// needs one of its own rather than the pool everything else uses.
	OpenSession(ctx context.Context) (Session, error)

	// Introspect reads the full schema model for the schema tree.
	Introspect(ctx context.Context) (introspect.Catalog, error)

	// ServerVersion is a short description of the server, as the
	// connections screen shows it next to the connection: "PG 16.2",
	// "SQLite 3.45".
	ServerVersion(ctx context.Context) (string, error)

	// Execute runs one SQL statement and collects its result, up to
	// [Limits]. A result the caps cut short still comes back as `Ok`, with
	// [QueryResult.Truncated] set: the statement did not fail, the app
	// declined to hold the rest.
	Execute(ctx context.Context, sql string) (QueryResult, error)

	// Check asks the server whether one statement parses, without running
	// it.
	//
	// **Nothing is executed.** The driver sends the statement to be
	// prepared and throws the prepared statement away: the server scans
	// it, runs it through the grammar, and answers. A `DELETE` checked
	// this way deletes nothing, takes no lock worth the name and costs
	// one round trip.
	//
	// One statement, never a buffer: the extended protocol refuses more
	// than one command in a prepared statement, so the caller splits
	// first — which it has to do anyway, being what the gutter marks are
	// worked out from.
	//
	// `Ok(None)` (nil, nil) is "nothing to say", and an engine that cannot
	// check says it for ever.
	//
	// **This is the pooled answer.** Names resolve against a connection
	// that is nobody's session, so it cannot see a temp table or a
	// `search_path` a tab has set — [Session.Check] is the one to
	// prefer when the tab has a session. See [CheckError].
	Check(ctx context.Context, sql string) (*CheckError, error)

	// RelationExists says whether the server has a relation of this name,
	// as **it** resolves the name — schema-qualified or through
	// `search_path`, and folding case the way SQL does.
	//
	// It exists because [Connection.Check] goes quiet exactly where it
	// would be most useful. Postgres resolves no names for a utility
	// statement until it runs one, so `DROP TABLE nosuch` prepares
	// happily and the check has nothing to report. The app reads the name
	// out of the statement itself — `query::relation_target` — and asks
	// this instead.
	//
	// `Ok(None)` (known == false) is "cannot say", and it is not
	// `Ok(false)`: an engine with no answer must not have its silence read
	// as "it is not there".
	RelationExists(ctx context.Context, name string) (exists bool, known bool, err error)

	// Stop stops a run this connection started. `Ok(false)` means the
	// server had nothing to stop — the statement finished on its own
	// first, which is a race, not an error. An engine with no cancellation
	// always answers `Ok(false)`.
	Stop(ctx context.Context, run RunId, how Stop) (bool, error)

	// EditTarget reads what editing a result of the relation `oid`
	// needs: its qualified name, its kind, its key and its columns by
	// attribute number. Nil, nil when there is no such relation, or the
	// engine cannot say. See [EditTarget].
	EditTarget(ctx context.Context, oid uint32) (*EditTarget, error)

	// PreviewEdit is the statements [Session.Apply] would send, with
	// every value written as a literal, for the user to read. It sends
	// nothing, and needs no session: a tab may have none yet.
	PreviewEdit(table *EditTarget, changes []RowChange, returning []int16) []string
}

// ConnectionDefaults holds the default bodies of [Connection]'s methods. A
// driver embeds it and writes only the methods it overrides.
type ConnectionDefaults struct{}

func (ConnectionDefaults) Check(context.Context, string) (*CheckError, error) {
	return nil, nil
}

func (ConnectionDefaults) RelationExists(context.Context, string) (bool, bool, error) {
	return false, false, nil
}

func (ConnectionDefaults) Stop(context.Context, RunId, Stop) (bool, error) {
	return false, nil
}

// EditTarget is nil: an engine that reports no column origins has no
// result to edit.
func (ConnectionDefaults) EditTarget(context.Context, uint32) (*EditTarget, error) {
	return nil, nil
}

func (ConnectionDefaults) PreviewEdit(*EditTarget, []RowChange, []int16) []string {
	return nil
}

// ---------------------------------------------------------------------------
// Go port: serde's JSON shape for a unit variant — its name, in a string.

func tagName(names []string, i int) string {
	if i >= 0 && i < len(names) {
		return names[i]
	}
	return strconv.Itoa(i)
}

func marshalTag(names []string, i int, typ string) ([]byte, error) {
	if i < 0 || i >= len(names) {
		return nil, fmt.Errorf("dbclient: %s has no variant %d", typ, i)
	}
	return json.Marshal(names[i])
}

func unmarshalTag(names []string, data []byte, typ string) (int, error) {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return 0, fmt.Errorf("dbclient: %s: %w", typ, err)
	}
	for i, n := range names {
		if n == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown variant `%s` for %s, expected one of %s", name, typ, strings.Join(names, ", "))
}
