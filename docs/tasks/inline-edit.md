# Editing a result in place

> Status: built for Postgres (steps 1–7). SQLite refuses an apply with
> `dbclient.ErrEditUnsupported`. Where the build differs from this plan,
> "As built" below says so.

pgAdmin's Query Tool lets the user edit the result of a `SELECT` — change
a cell, add a row, mark a row for deletion — and write it all back with
one press (F6). It is the first thing a pgAdmin user reaches for that a
viewer does not have, and `CLAUDE.md` already names in-place editing as
Phase 2. This file is the plan for doing it the Meerkat way.

## What pgAdmin does, and what we keep

pgAdmin (9.19 docs, `query_tool`, `editgrid`, `query_tool_toolbar`):

- A result is editable when every column is either a column of **one**
  table or not a table column at all, and every primary-key column (or the
  OID) is in it. Without a key the grid is read-only.
- Only columns selected directly can be edited. Computed columns,
  **renamed** columns and columns selected twice are read-only. A pencil
  or a lock on each header says which.
- Simple auto-updatable views take updates and deletes, not inserts.
- An empty cell means NULL (the column default on a new row); `''` is the
  empty string.
- Edits are staged; the status bar says "Changes staged" with counts. F6
  saves inserts, updates and deletes together.
- With auto-commit off the save runs inside the open transaction, and a
  failed save rolls back to an automatic savepoint so the transaction
  survives.

We keep the rule (one table, its whole key) and the staged-then-saved
flow. We do better in three places, because we have information pgAdmin
works around:

1. **We know where every column came from.** The server's
   `RowDescription` names each column's source table OID and attribute
   number (`pgconn.FieldDescription.TableOID`, `TableAttributeNumber`).
   So a renamed column (`name AS customer`) is still `customer.name`, and
   stays editable.
2. **NULL and the empty string are two gestures**, not one field read two
   ways. An untouched cell of a new row is left out of the `INSERT`, so
   the server's default applies; NULL is a key of its own.
3. **The row comes back from the server.** Every `UPDATE` and `INSERT`
   ends in `RETURNING`, so a trigger's rewrite, a serial id and a default
   land on screen without a re-run.

## Where the code stands

- `dbclient.RowChange` with `RowChangeUpdate` (key, and old and new value
  per column), `RowChangeInsert` and `RowChangeDelete`
  (`internal/dbclient/dbclient.go:688`).
- `Connection.Apply(ctx, []RowChange)` — "one transaction, roll back on an
  update that matched 0 rows" — is a stub in both drivers and has no
  caller.
- `introspect.Table.PrimaryKey` holds key column **names**; the catalog
  holds no OIDs.
- `collectStatement` (`internal/dbpostgres/dbpostgres.go`) already has
  the prepare's `described.Fields`, OIDs and attribute numbers included,
  and throws them away after reading `Name` and `DataTypeOID`.
- `QueryTab` holds `data *resultsgrid.GridData`, `selection`, `scroll`,
  `session`, `txMode`, `inTransaction`. `Tally.last` is the result the
  grid paints: the last statement of the buffer that had columns.
- Keys in the grid's context today: arrows, ⇧/⌘ arrows, ⌘A, ⌘C, space,
  ⎋, ⏎ (the value card), ⌘F, ⌘G, ⌘J. In the shell: ⌘S commits, ⇧⌘R rolls
  back.

## Design

### 1. Where a column came from

`dbclient.QueryResult` gains

```go
// Origins says, per column, which table column the server says it
// came from. Nil for an engine that does not say (SQLite).
Origins []ColumnOrigin

type ColumnOrigin struct {
	Table  uint32 // pg_class oid; 0 for an expression
	Column int16  // attnum; 0 for an expression
}
```

filled from `described.Fields` in `collectStatement`, at no cost: the
prepare already happened. Postgres resolves the origin through a
subquery in `FROM` and through a CTE, so `SELECT * FROM (SELECT … FROM t)
s` keeps its origins; an expression, an aggregate or a function call has
none. A view's columns name the view, not its base table.

### 2. What the origin table is

One table OID is not enough to edit: we need its qualified name, its
kind, its key columns and, per attnum, the column's name, whether it is
generated and whether it is `GENERATED ALWAYS AS IDENTITY`. A new driver
call:

```go
// EditTarget reads what editing a result of table `oid` needs. Nil, nil
// when there is no such relation (dropped since, or not visible).
Connection.EditTarget(ctx, oid uint32) (*EditTarget, error)
```

One catalog query (`pg_class`, `pg_namespace`, `pg_attribute`,
`pg_index WHERE indisprimary`) on the **app pool**, off the run's path,
asked when a result with origins lands — the way
`refreshServerTiming` fills in after the grid is already painted. The
headers paint locks until it answers. It is not the introspected catalog
because that one is keyed by name, may be stale, and does not see a table
the user's own transaction made; a table created inside an uncommitted
transaction answers nil on the pool and the result stays read-only,
which errs the right way.

### 3. The rule — one pure function

```go
// editability decides, per column, whether a result can be edited, and
// says why not when it cannot. A plain function over origins and the
// target, argued with in a test.
func editability(origins []ColumnOrigin, target *EditTarget, readOnly bool) Editability
```

A result is editable when all of these hold, and `Editability.Reason`
names the first that fails, worded for the status line:

| Rule | Reason when it fails |
|---|---|
| the connection is not read-only | `read-only connection` |
| at least one column has an origin, and every origin is the same table | `more than one table` / `no table column` |
| the table is an ordinary or partitioned table (`relkind` `r`, `p`) | `a view` (views: later step) |
| the table has a primary key | `no primary key` |
| every key column is in the result | `key column id is not in the result` |

Per column, inside an editable result: an expression is read-only; a
generated column and an identity `ALWAYS` column are read-only; a table
column that appears **twice** is read-only in both places (editing one
copy would leave the other saying something else). A renamed column is
editable — the attnum says what it is.

The result being `truncated` by the memory cap does not stop editing:
every row on screen is still a whole row. A cell the cap **cut**
(`MAX_CELL_BYTES`) is read-only, and a row whose key was cut cannot be
named at all, so
`RowSink` records which cells it cut (`QueryResult.Cut`, a set of row and
column) instead of only appending `…`.

### 4. Staged edits — a model with no window in it

`resultsgrid.Edits` holds what the user has changed and nothing else,
beside `selection` on the tab:

- `cells map[Cell]Pending` — the new value of an edited cell of an
  existing row. `Pending` is `Text(string)` or `Null`. An edit back to the
  original value removes the entry.
- `added [][]*Pending` — new rows, appended after the result; a nil cell
  is "untouched", which the `INSERT` leaves out.
- `deleted set[int]` — existing rows marked for deletion. A deleted row
  loses its cell edits; deleting an added row drops it.
- `undo []step` — every staged action, so ⌘Z in the grid walks them back.

`Edits` is plain functions (`set`, `revert`, `addRow`, `toggleDelete`,
`undo`, `counts`, `changes`) argued with in tests, like `Selection`. It
holds indices into the rows on screen, so **anything that replaces the
rows must ask first** (see "Losing staged edits") and then clears it.

`Edits.changes(data, origins, target)` turns the staged edits into
`[]dbclient.RowChange` — key values from the row's own key cells, old
values from the rows on screen.

### 5. Writing it back — on the tab's session

`Apply` moves from `Connection` to `Session`. The pool is the wrong place
for it: an edit belongs to the tab, and the tab may be inside a
transaction the user opened, which only its own connection can see.

```go
// Apply writes a changeset down this session and answers with the rows
// the server returned, one per change that returns a row.
Session.Apply(ctx, changes []RowChange) ([]AppliedRow, error)
```

- **Outside a transaction** (status byte idle): `BEGIN`, the statements,
  `COMMIT`. Any statement failing, or an `UPDATE`/`DELETE` matching 0
  rows, is a `ROLLBACK` and an error naming the row. All or nothing.
- **Inside a transaction** (auto mode with a typed `BEGIN`, or manual
  mode): `SAVEPOINT meerkat_apply_N`, the statements, `RELEASE`. On
  failure `ROLLBACK TO SAVEPOINT` and `RELEASE`, so the user's
  transaction comes out as it went in — the fence `checkInsideTransaction`
  already uses. Nothing commits; the transaction bar counts the rows, and
  ⌘S commits as it does now.
- **In a failed transaction**: refused before anything is sent — the
  server would refuse every statement anyway.

The statements, built by `dbpostgres` from introspected names through
`quoteIdent`, values always as parameters:

```sql
UPDATE "s"."t" SET "name" = $1, "email" = $2
 WHERE "id" = $3
RETURNING "id", "name", "email"

INSERT INTO "s"."t" ("name") VALUES ($1) RETURNING "id", "name", "email"

DELETE FROM "s"."t" WHERE "id" = $1
```

Every parameter goes out **in text format with type OID 0**, so the
server types it from the column it lands in. That is what makes every
type work — an `interval`, an enum, an array, a range, `jsonb` — with no
type table on our side: the user edits the text the server sent, and the
server reads it back with the same input function. A `bytea` is edited as
its `\x` hex, which is what `bytea` input reads.

**The `WHERE` is the primary key, as pgAdmin writes it.** One
`UPDATE` per row, every edited cell of the row in its `SET`. A row
somebody changed since it was read is overwritten, as it is in pgAdmin. A
row somebody deleted, or whose key changed, matches nothing, and the apply
rolls back as a conflict: `the row is no longer there`.

An earlier cut also checked the old value of every visible column, which
caught concurrent changes and most self-joins. It was dropped for
pgAdmin's behaviour: a trigger or a job touching an `updated_at` made
every edit of the row a conflict. **What that leaves open is the
self-join.** The origins name a table, never a copy of it, so

```sql
SELECT e.id, m.name FROM employees e JOIN employees m ON m.id = e.manager_id
```

reads as one table with its key present, and an edit to `m.name` lands on
employee `e.id`. pgAdmin has the same hole. Closing it means mapping
columns to range-table entries (`EXPLAIN VERBOSE`'s `Output`), the same
work joins would need, and is not planned.

`RETURNING` lists the table columns the result shows, by name from the
target, in the result's order. The reply patches the grid in place:
an updated row takes the returned values, an added row becomes a real row
with its id, a deleted row goes. An expression column cannot be
recomputed and keeps what it showed.

A save is refused, not queued, while a run is out — the boundary rule —
and `QueryTab.applying` blocks ⌘⏎ while it is.

A tab whose session was swept opens a new one for the save. If the
session was swept, any transaction is gone already, so the save goes the
outside-a-transaction way.

### 6. The screen

The design reference is the "Meerkat inline editing" canvas
(https://claude.ai/artifact/CVYFXoEpS1ASm1bPbNuRnL). What follows is what
it settles.

**The toolbar says whether the result takes an edit**, in one chip beside
the auto/manual switch: `editable · public.customers · key id` in a new
slate family (`edit*` tokens), or the reason in plain paper —
`not editable · the result reads 2 tables`, `not editable · the key id is
not in the result`, `… has no primary key`, `… is a view`; `read-only
connection · edits off` in the dev green; `transaction failed · roll back
to edit` in clay. A result with no table column at all shows no chip.

**Headers.** A primary-key column wears a key glyph. A read-only column
inside an editable result wears a lock, and its cells a recessed well
(`edit_locked`), so the rest of the row is plainly the part that edits.

**Cell states**, none of which takes a pixel from the content box:

| State | Mark |
|---|---|
| cursor | as today, `match_strong` |
| editing | a `TextField` over the cell, white, an inset 2px ochre ring |
| staged edit | slate wash `edit_surface`, a 7px slate flag in the top-left corner, `~` in the gutter |
| staged NULL | the NULL chip in slate |
| added row | pinned under the header, green wash, `+` in the gutter, `DEFAULT` chips in the cells left untouched |
| deleted row | clay wash, text struck through, `−` in the gutter |
| refused | clay wash and flag, a wavy clay underline, `~` in clay |
| changed on the server | sand row wash, `!` in the gutter |

**Starting an edit.** ⏎, a double click, or a typed character on an
editable cell opens the editor; a typed character replaces the value.
**⇧⏎ is the value card** now (it used to be ⏎), and on an editable cell
the card is where a long or multi-line value is edited. On a read-only
cell ⏎ and ⇧⏎ both open the card, as before.

In the editor: ⏎ keeps and stays on the cell, ⇥ and ⇧⇥ keep and move
right or left, ⎋ puts the old value back. ⌥⌘N stages NULL, in the grid or
in the editor. An emptied field is the empty string, never NULL.

**Typed editors.** An enum column offers its labels from the cached
catalog, matched by the `fuzzy` rule, ↑↓ to pick — no query goes out. A
boolean offers `true` `false` `NULL` on `t` `f` `n`; space still ticks the
row and never toggles a value.

**Rows.** ⌘I adds a row pinned under the header. ⌘⌫ marks the ticked
rows, or the cursor's row, for deletion, and again unmarks them. ⌘Z and
⇧⌘Z undo and redo one staged change. ⌘V pastes CSV (the format ⌘C writes)
into the range, staged.

**The status strip** while editing: `editing customers.name · text · was
Dana Marsh`, then the editor's keys.

**The change bar** sits where the transaction bar sits, slate, while
anything is staged:

```
5 changes not applied                         preview SQL   discard ⇧⌘R   apply 5 changes ⌘S
3 cells edited in 3 rows · 1 row added · 1 row deleted · nothing has left this window
```

**⌘S applies, ⇧⌘R discards** — the commit and rollback keys, because the
bar is the thing they act on while it is up. One bar at a time: with
nothing staged, the transaction bar is back and ⌘S commits.

**Preview SQL** is a dialog with the statements as they will run, one wash
per kind (update slate, insert green, delete clay), the values written as
literals for reading, `copy into a new query tab`, and apply on ⌘⏎.

**The bar turns into the answer:**

- refused — `Nothing applied · statement 4 of 7 was refused`, the server's
  words, `all 5 changes are still staged`; preview SQL, discard ⇧⌘R,
  apply again ⌘S. The refused cell carries the refusal.
- applied, auto — `5 changes applied · committed · 38 ms`, `3 rows updated
  · 1 row inserted, id 1053 · 1 row deleted · the rows on screen are what
  the server returned`; copy the SQL.
- applied, manual — the transaction bar: `Transaction open · 5 changes
  applied, not committed`; roll back ⇧⌘R, commit ⌘S.
- a conflict — the row is gone or its key changed: the row turns sand,
  `the row is no longer there · reload row`. (The canvas's *write mine*
  went with the old-value check; with the key alone there is nothing to
  write past.)

A tab with staged changes wears a slate dot in the strip.

### 7. Losing staged edits

Staged edits are a third thing at stake, beside a run in flight and an
open transaction, so every way out `guard_close` covers counts them:
the close dialog gains `5 changes not applied. They live only in this tab;
closing it throws them away. Nothing was sent, so the server loses
nothing.`

A run over staged changes asks `Run again over 5 staged changes?` with
stay ⏎, apply, then run ⌘S, and discard and run ⌘⏎. The same question
comes up for a page turn and a refresh.

The tab's buffer is restored on reopen, staged edits are not: a restored
tab has no result for them to sit on.

### 8. History

A save goes into `query_history` as the statements it sent, with the
parameters written as literals for reading (`quote_literal` rules), one
row per save. "Why did this row change" is answered there, as "why did my
work disappear" is answered by a `ROLLBACK`.

## Steps

Each step lands green on its own.

1. **Origins.** `QueryResult.Origins` and `QueryResult.Cut`, filled by
   `collectStatement` and `RowSink`. Driver tests: a renamed column, a
   subquery, a CTE, a join, an expression, a view.
2. **Target and rule.** `Connection.EditTarget`; `editability` with a
   table-driven test over every row of the rule table and the per-column
   cases.
3. **Apply.** `Session.Apply` with the statement builders; remove
   `Connection.Apply`. Tests against `MEERKAT_TEST_PG_URL`: idle and
   inside-a-transaction paths, the savepoint leaving the transaction
   usable, a row that is gone, `RETURNING`, and one row through every awkward
   type (`interval`, enum, `int[]`, `jsonb`, `bytea`, `numeric` `NaN`,
   NULL, empty string). SQLite answers "not supported".
4. **Edits model.** `resultsgrid.Edits` and `changes()`, with tests.
5. **Screen.** Header locks, the cell field, marks, the pending strip,
   keys, ⌘S's two meanings. `ui.NewTester` view tests for the marks and
   the strip.
6. **Save and guards.** The save path on the shell, the grid patch, the
   history row, the three new "at stake" cases.
7. **Docs.** A section in `CLAUDE.md`; drop "read-only PostgreSQL" from
   the milestone line.

## As built

- `Connection.Apply` is gone. `Session.Apply` writes, `Session.Reload`
  reads one row again for the conflict's "reload row", and
  `Connection.PreviewEdit` renders the statements — on the connection, not
  the session, because a tab may have no session yet when the preview
  opens.
- Rows are named by the primary key alone, as pgAdmin names them; see
  "Writing it back". The design canvas's old-value check and *write mine*
  are not built.
- The enum labels and the boolean's three keys are offered in the status
  strip under the grid, not in a popover under the cell: the grid's rows
  are a virtualized list painted in order, and a popover would be covered
  by the rows under it.
- The "transaction failed" chip is not shown ahead of time: a session
  reports only open or not. An apply into a failed transaction is refused
  with the same words.
- History keeps one row per apply, the statements with their values as
  literals.

## Later

- Auto-updatable views: update and delete, no insert — pgAdmin's rule.
  The view's key is its base table's, which `EditTarget` would read
  through `pg_rewrite`.
- The paged table tab, which knows its table outright.
- Paste rows from the clipboard as new rows (CSV, the format ⌘C writes).
- SQLite: modernc reports no origin table, so an editable SQLite result
  would have to be a browse query whose table the app wrote.

## Not planned

- **Joins.** A result with columns of more than one table stays
  read-only, as in pgAdmin. Editing one would need to know which copy of
  a table each column came from (the server's origins name the table, not
  the range-table entry), and an insert or a delete has no single target.
  Nobody has asked for it.

## Open questions

- Should the read-only default stay on for the editing flow, with the
  lock's reason pointing at the connection form, or should a session be
  switchable to read-write from the title bar's mode mark?
- Should a deleted row disappear on save or stay struck through until the
  next run? The plan removes it, which matches the server.
