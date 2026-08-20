# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Meerkat is a native cross-platform database viewer built on [GPUI](https://www.gpui.rs),
the UI framework behind Zed. One Rust binary, no webview, no server. The repo
is a Cargo workspace of small crates in the Zed style. Current milestone:
read-only PostgreSQL. In-place editing and MySQL are Phase 2.

## Commands

```sh
cargo dev                                                   # connections screen
cargo dev postgres://user@host:5432/database                # straight to a database
cargo check --workspace
cargo test --workspace
cargo test -p sql_editor completion::tests                  # one crate / one module
cargo test -p meerkat pages_order_by_the_primary_key        # one test by name
```

`MEERKAT_DATABASE_URL` works instead of the CLI argument.

`cargo dev` is an alias for `cargo run -p meerkat --`, in
`.cargo/config.toml`. Plain `cargo run -p meerkat` does the same thing.

### Signing, and why the keychain keeps asking

On macOS both go through `scripts/run-signed.sh`, cargo's `runner` for the
platform, which signs the binary before launching it.

A keychain item's ACL identifies an app by its code signature, and cargo's
output is ad-hoc signed by the linker — the identity *is* the hash of the
binary, so it changes with every build. macOS then reads each rebuild as a
different app and asks for the login keychain password again; "Always
Allow" trusts one binary and the next `cargo build` throws that away.

Make the certificate once, in Keychain Access: Certificate Assistant →
Create a Certificate, named `Meerkat Dev`, type "Code Signing",
self-signed. `MEERKAT_SIGN_IDENTITY` overrides the name. With no such
certificate the script says so and runs the binary anyway, so a fresh
clone still works — the prompts simply carry on.

The script only signs the app. Test binaries pass through it untouched.

The PostgreSQL driver tests skip themselves when no server is configured, so
`cargo test` stays green without one. To actually run them:

```sh
MEERKAT_TEST_PG_URL=postgres://postgres:pg@localhost:5432/postgres cargo test -p db_postgres
```

The first build compiles GPUI from the pinned Zed git revision and takes a
long time. macOS needs Xcode and its command line tools, because GPUI renders
with Metal.

## Architecture

### Dependency direction

UI crates may depend on data crates; the reverse is forbidden. `db_postgres`,
`db_sqlite`, `db_client`, `introspect`, `storage`, `secrets`, `query` and
`settings` must not know about `gpui`.

| Crate | Role |
|---|---|
| `meerkat` | Binary: `main.rs` boots, `root.rs` switches screens, `connections.rs` and `shell.rs` are the two screens, `history.rs` is the query-history tab, `palette.rs` is the ⌘K palette, `sql.rs` builds the app's own SQL |
| `db_client` | Engine-agnostic `Connection` trait, `Profile`, `Value`, `QueryResult`, `RowChange` |
| `db_postgres`, `db_sqlite` | sqlx drivers behind that trait |
| `introspect` | Schema model (`Catalog` → `Schema` → `Table` → `Column`) that drivers fill |
| `query` | `statements()`: split a buffer into statements, quote- and comment-aware |
| `sql_editor` | Multi-line SQL buffer: motion, undo, colouring, completion |
| `results_grid` | Virtualized grid |
| `ui`, `theme` | Component kit (incl. `TextField` and the overlay `scrollbar`) and color tokens |
| `storage` | Local SQLite: profiles, cached probe counts, layout, history, cached catalogs |
| `secrets` | OS keychain wrapper for passwords |
| `workspace`, `schema_tree` | Empty placeholders |

### Screens

`Root` holds exactly one of `Connections` or `Shell` at a time, and swaps
between them on an emitted event. Leaving a `Shell` drops it, and with it the
connection pool, so a closed database keeps no sockets open.

`Shell` owns the session: the `Arc<dyn Connection>`, the introspected
`Catalog`, the flattened sidebar rows, the completion vocabulary, and the
open tabs (`Tab::Table`, `Tab::Query` or `Tab::History`).

### Opening a connection

A session opens on an empty query tab. That needs neither a catalog nor a
round trip, so the user can type while the connect is still in flight. The
shell never picks a table to open on the user's behalf.

The connect and the introspection are two requests, not one. `connect`
opens the pool and nothing else, so the session reports "connected" and
runs queries as soon as there is a pool; `introspect` then reads the
catalog behind it and `catalog_loading` says so in the sidebar. Reading the
catalog of a large database takes far longer than the connect, and nothing
but the sidebar needs it.

The catalog is cached in the local store, in `catalog_cache`, keyed by the
same *scope* the history uses. `Shell::new` reads that row synchronously —
it is local SQLite, like the history — and paints the sidebar and the
completion vocabulary from it at once. The connect and the introspection
then run in the background as before, and `apply_catalog` replaces the
cached shape with the real one and writes the cache back. So the cache is
always revalidated; it is never trusted past the first paint.

A row that no longer parses (the `introspect` model changed under it) is
dropped and reported as a miss, never as an error. An introspection that
fails leaves the cached catalog on screen and sets `catalog_error`: a stale
sidebar beats no sidebar, and the connection is still good for queries.

Because the sidebar is live before the connection is, a table can be opened
with no connection to page it. `load_page` then leaves the tab marked
loading and sends nothing; `resume_pending_pages` asks again when the
connection lands, and a failed connect marks those tabs `NOT_CONNECTED`
instead of leaving them waiting for ever.

### The sidebar

The tree is schema → `TABLES` and `VIEWS` → the relations. The catalog is
read into `Group`s (a schema) holding `Section`s (its tables, its views),
and flattened into `CatalogRow`s again whenever what the sidebar shows
changes.

**The two levels remember themselves the other way round.** A schema is
closed until `open_schemas` holds it, because a database of 3,000
relations is a wall of names when every schema is expanded; a section is
open until `closed_sections` holds it, because a schema the user just
opened was opened to see what is in it — one click, not three. Both sets
are keyed by name, so they survive an introspection replacing the cached
catalog.

The filter line over the list narrows the names the session already
holds — no query goes out — by the palette's own rule, through
`palette::path_matches`: a case-insensitive substring, and **a dot names
a path**. So `address` finds every `address`, `dev.addr` finds the one in
`sample_dev_sample`, and a bare schema name answers with everything under
it. Whatever is left with nothing under it is dropped, header and all,
and what survives is drawn open whatever the two sets say — a search that
needs a second click to show its hits is not a search. ⏎ opens the first
relation left, ⎋ empties the line.

⇥ completes the line from the first match, through
`palette::complete_path`, so a name is walked in the same steps here and
in the palette: one part at a time, **replacing** what was typed rather
than appending to it, and the faint hint is painted only when the
completion happens to carry on from what is there.

**A click on a relation opens a query tab, not a table tab.** It writes
`sql::browse_query` — `SELECT * FROM "schema"."relation" LIMIT 500;` —
into a new tab, names the tab after the relation, and runs it at once: a
click on a table name asks for its rows, not for a line of SQL to look
at. From there the statement is the user's, editable and re-run with ⌘⏎.
The tab keeps a `relation` so the sidebar can mark the row it came from.
Every click opens another tab; the paged `Tab::Table` view is what the
palette still opens.

The list carries an overlay scrollbar, `ui::scrollbar`, the one the
results grid uses. It reads the plain `ScrollHandle` that
`UniformListScrollHandle` keeps inside itself, and is painted outside the
scrolling list, or it would scroll away with it.

### The tabs a connection was left with

A session reopens the tabs it was closed with. `open_tabs` in the local
store holds one row per tab, keyed by the same *scope* the history and
the catalog cache use, so tabs come back per connection: leaving a shell
and choosing that connection again paints the strip it had. `Shell::new`
reads the rows synchronously, beside the cached catalog, and opens on
them; with no rows it opens on the one empty query tab as before.

**Only what is needed to open a tab again is kept — never a result
set.** A query tab is remembered by its statement, its name and the
relation it was opened on, and comes back *waiting*: restoring a session
must not fire a hundred statements at the user, for the reason the
history screen does not re-run one either. A table tab is the paged
view, so it is remembered by its page and asks for it through
`load_page` — which leaves it loading until the connection lands, the
path a table opened from the cached catalog already takes.

`storage::TAB_LIMIT` is 100. Above that the **front** of the strip goes:
the newest tabs are the ones worth reopening, and `active` moves with
what is left.

`Shell::remember_tabs` writes the whole strip back at once, and
`Shell::activate` — which every path that changes the active tab already
goes through — is the main caller. The editor's buffer is the one thing
that changes without touching the strip, so a run, a tab switch and
every way out of the shell save as well: "‹ connections", ⌘Q, and the
window's close button through `on_window_should_close`. `restoring`
guards the rebuild, or each restored tab would write a half-built strip
over the saved one.

### Picking cells and rows

`results_grid::Selection` holds everything the user has marked in a
result, and the tab holds it beside the scroll position, so it survives
the re-render after every keystroke. **Two things get marked, and they
are not the same thing.** A *range* is a rectangle grown from the cell
the user pressed on: it is what the arrow keys move and what ⌘C copies. A
*pick* is a whole row ticked in the gutter — discontiguous by nature,
which is why it is a `BTreeSet` and not a second rectangle. A tick says
"this record"; a range says "these cells", and neither can stand for the
other.

The policy is plain functions over indices — `focus`, `extend_to`,
`step`, `toggle_pick`, `pick_through`, `toggle_all_picks` — with no
window and no theme in sight, so it can be argued with in a test rather
than in a running window. The view paints what it reads there and
decides nothing.

**One callback, not five.** The grid reports where the mouse landed as
one `Hit` — a cell (with ⇧ held, and with the second click of a double
click, which asks to read the value whole), the gutter beside a row, the
gutter's head, or a column header — and `Shell::hit_handler` turns it
into a call on `Selection`. So the mouse and the keys move the same
selection through the same code.

Three marks in the same warm family, and they have to stay apart at a
glance: the cursor's own cell wears `match_strong`, the range around it
`range_surface`, and a ticked row `selection`, whole. **None of them
carries a border** — a border would take a pixel out of the cell's
content box and shift the value inside it every time the cursor moved.
The gutter shows the row's number, and `✓` in place of it when the row is
ticked.

The **gutter scrolls sideways with the content** rather than pinning
itself to the left edge. Pinning would need a second vertical scroller
kept in step with the row list, and `uniform_list` gives nothing to keep
it in step with. A table page numbers from where the page starts, so the
gutter counts `page * PAGE_SIZE + 1` upwards rather than from 1 again.

⌘C writes **CSV**, one line per row, by RFC 4180's rules. **Ticked rows win over the range and are copied whole**,
because that is what ticking them said. No header line either way: a copy
pastes back exactly what was marked. A NULL copies as an empty field,
since pasting the word `NULL` would make it data, and a value holding a
comma, a quote or a line break is quoted with its own quotes doubled — or
one cell would read as two fields, and one with a newline in it would
shear every row below it out of line. Nothing marked copies nothing
rather than the whole result.

A selection is a set of indices into the rows on screen, so **anything
that replaces the rows clears it**: a page turn, a refresh, a run.

The keys live in `GRID_KEY_CONTEXT`, never the shell's: the SQL editor's
element sits *inside* the shell's, so a bare `space` bound there would eat
the spaces out of the user's SQL. The grid holds a focus of its own —
`Shell::grid_focus` — which a table tab takes when it opens and a query
tab takes on the first click in its result. Arrows move, ⇧ with them
extends, ⌘ with them goes as far as it goes, ⌘A takes everything, space
ticks the cursor's row — ↓ then space walks a result and picks out of it
without the mouse — and ⎋ drops what is marked.

### Finding a column of a result

A result wider than the pane is the ordinary case for a real table, and
the column's **name** is usually all the user knows about the one they
want. So ⌘J opens a search line over the names the result came back
with, and ⏎ jumps to the column: `Shell::jump_to_column` puts the cursor
there and `GridState::reveal` scrolls it in, the same call the arrow keys
already go through.

The jump **keeps the row the cursor is on**. The user asked for a
column, not for a cell somewhere else in the result.

The **column header marks the cursor's lane** — an accent rule under the
name, and the name in `accent_deep`. That is the mark which survives
scrolling: the cursor's own cell can be a hundred rows down the page, so
after a jump it is the only thing on screen that says the jump landed.
It is a child of the header cell rather than a bottom border, because a
border carries one colour for all four sides and the left hairline has
already claimed it.

`results_grid::find_columns` is the whole of the matching: the palette's
rule — a case-insensitive substring, not a fuzzy score — over the column
names, answering with indices into the result's own lanes. It lowercases
the whole of Unicode where the palette lowercases ASCII alone, because
nothing underlines the hit here, so no byte range has to stay valid in
the original name. A column's **type** is not searched: a result carries
names and values, the driver reports no type per column, and a search
that answered differently on a tab opened from the catalog would be
worse than one that does not offer it.

**What the search found is never kept.** A run or a page turn replaces
the result under the popover, and remembered lane indices would point at
lanes that are not there any more — so `Shell::column_matches` works
them out from the line and the result in hand every time, and
`jump_to_column` checks the lane against the result before it moves
anything. `ColumnFind` holds only where the user has walked to.

The popover is `deferred`, which paints it after the elements around it:
GPUI paints siblings in order, and the toolbar it hangs from is painted
before the grid it hangs over. Its keys live in
`COLUMN_FIND_KEY_CONTEXT` — ↑↓ only, or they would be taken from the
grid while nothing is open — and ⏎ and ⎋ arrive as the `TextField`'s own
`Submit` and `Cancel`, the way the palette answers them. ⇥ finishes
nothing: a column name is one part, and the whole list is on screen.

It is a control on **one** result, so `Shell::activate` drops it — which
is why every path that changes the active tab now hands the focus on
through `focus_active_tab`, or the focus would be left on a control the
switch took off the screen. `guard_close` and `open_palette` drop it for
the reason they close the palette: one thing takes keys at a time.

### Reading a whole value

A lane is capped at `MAX_COLUMN_WIDTH`, so a long value truncates on
screen. **The question that raises is "what is in this cell", which is
not the question "how wide should this column be"** — widening a lane to
two thousand pixels only turns reading into panning. So ⏎ over the
cursor's cell, or a double click on any cell, opens a card holding the
value whole: wrapped, scrolling if it is long, ⌘C to copy it, ⏎ or ⎋ to
close. The lane cap stays where it is; it was only ever wrong as the
*one* way to read a value.

The card is the overlay pattern the palette and the close dialog already
use — an absolutely positioned child of the shell, its own focus and its
own `PEEK_KEY_CONTEXT` — with two differences. Its scrim carries **no
wash**: the palette is a place the user went to, while this is a second
look at something already on screen, and dimming the result would hide
what is being looked at. And a click on the scrim **does** close it,
unlike the confirmation's, because nothing here is lost by dismissing.

`Peek` holds **where** the value is, never the value: a run or a page
turn replaces the rows under the card, and a string copied when it opened
would go on saying what used to be there. `Shell::peek_value` reads the
cell out of the result in hand and answers `None` when the result no
longer has it.

**What is painted is bounded; what ⌘C copies is not.** `MAX_CELL_BYTES`
lets a megabyte of text into one cell, and laying a megabyte of wrapped
text out on the GPUI thread would freeze the window, so the card paints
the first `PEEK_CHARS` and says how much there is. ⌘C is bound in the
card's own context and means something narrower than the grid's: **this
value**, whole, written bare — a value read on its own is not a row, so
none of CSV's quoting applies to it.

The status strip lists the keys a result answers to, as the comp's footer
does. It is the only place ⏎ and ⌘J are written down, and a gesture
nothing on screen names is a gesture nobody finds.

### Query history

Every run — a table page the app built as well as a statement the user
typed — goes into the `query_history` table of the local store, keyed by
a *scope*: the profile id, or the command-line URL with its password
taken out. `Store` keeps the newest 500 runs per scope and drops the
rest, so the file cannot grow without end.

The history tab reads that file synchronously on the GPUI thread: it is
local SQLite, not the database, so it needs no tokio bridge and no
generation counter. `history.rs` flattens the runs into day headings plus
runs of one height, because `uniform_list` needs one row height. Clicking
a run opens it in a new query tab; it never re-runs behind the user.

### The ⌘K palette

`palette.rs` searches only what the session already holds: the `Catalog`
the shell introspected, and the runs it read back from the local file when
the palette opened. Nothing there hits the database, so a keystroke costs a
substring scan, not a query. `build()` is pure — catalog and runs in, flat
rows out — which is what makes the matching testable without a window.

Matching is a case-insensitive **substring**, not a fuzzy score, and the
comparison lowercases ASCII only. That is deliberate: `to_ascii_lowercase`
never changes a character's byte length, so the hit's byte range into the
lowered copy is still valid in the original, and the row can underline the
matched characters. Rows are ranked by where the hit sits, then by name
length.

Rows are flattened into headings plus results of one height, as the
sidebar and the history list are, because `uniform_list` needs one row
height. `Shell::palette` owns the state; the palette is an absolutely
positioned child of the shell, never a window of its own, so closing it
hands the focus straight back to the workspace.

A **dot in the query names a path**. Each result carries its name in
parts — the schema and the relation — and the typed parts line up with the
*end* of that path first, sliding one part further out when nothing hit
there. So `task` finds every `task`, `dev.ta` finds
`sample_dev_sample.task`, and a bare schema name answers with the tables
it holds.

**It does not search columns**, and the `c:` scope is gone with them.
Every row of the palette ends in "open this", and a column has nothing of
its own to open — a column row could only offer its table, which the
table row already offers. It also made a bare word answer with a page of
near-identical names. The way to a column is ⌘J over the result that
holds it, which is a search over the columns actually on screen rather
than over every column in the database.

⇥ finishes the line from the selected row (`completion()`), one part at a
time, so ⇥⇥ walks schema then relation. It **replaces** what was typed
rather than appending, because a hit sits anywhere inside a name — `dev`
completes to `sample_dev_sample.`. The faint hint in the field is only
painted when the completion happens to carry on from what was typed;
otherwise it would lie about what ⇥ does. With nothing left to finish, ⇥
walks the scope chips instead.

Everything the palette offers ends in a `Pick`: open a relation, or open a
statement in a query tab. A statement is never re-run behind the user, for
the same reason the history screen does not re-run one. The comp's "saved"
scope is not implemented — the app has no saved queries yet.

**Key bindings are scoped, not global.** The shell's own keys are bound to
the `Shell` context and the palette's to `Palette`. GPUI gives a keystroke
to the binding that matched deepest in the context stack, so ⌘⏎ runs a
query while the shell has focus and opens a palette row in a new tab while
the palette is open. A binding with no context is treated as the *deepest*
match, so binding either one globally would take the key from the other.

### Walking the tabs

⌃⇥ moves to the next tab and ⌃⇧⇥ to the one before it, on the keystroke.
There is no popup and no most-recently-used order: the walk follows the
strip, which paints the tabs in the order they were opened, and the strip
is the list the user is reading. `step_wrapping` **wraps** at both ends,
because the tabs are a ring — one press past the last is how the user
comes back to the first.

Both keys are bound to the `Shell` context. The walk does nothing while
the palette is open: that dialog is already the one taking keys. Every
path that changes the active tab still goes through `Shell::activate`.

### Async bridge

All sqlx work runs on tokio; all UI state lives on the GPUI main thread.
`gpui_tokio::init(cx)` is the first call in `main`. Every database call
follows this shape:

```rust
let task = gpui_tokio::Tokio::spawn(cx, async move { connection.execute(&sql).await });
cx.spawn(async move |this, cx| {
    let outcome = task.await;                       // JoinError, then inner Result
    this.update(cx, |this, cx| { /* apply */ cx.notify(); }).ok();
})
.detach();
```

`flatten()` in `shell.rs` collapses the two error layers into a `String` for
display. Never block the GPUI thread on a query — a slow query must not
freeze the window.

**Generation counters guard against stale replies.** Each tab counts its
requests; a reply that does not carry the tab's current `generation` is
dropped. Without this a slow first page overwrites a fast second one. Bump
the generation whenever you start a request, and check it in the callback.

### SQL safety

Identifiers the app splices into its own SQL come only from introspection and
always go through `sql::quote_ident` (doubles embedded quotes). Nothing the
user types reaches those builders — a typed query goes to the driver verbatim.
Table pages are `SELECT * ... ORDER BY <pk, else first column> LIMIT 500
OFFSET n`; the ORDER BY is what keeps paging stable.

### Stopping a run

A query tab's run walks the comp's four states — `Run::Idle`, `Running`,
`Cancelling`, `Cancelled` — and one button says all of them: **run** filled
in the accent, **stop** outlined in clay over paper, **terminate** filled in
clay. ⌘⏎ runs, ⌘. stops. The escalation is the point: the button that ends
a backend must not look like the button that starts a query.

**The server does the stopping, not the app.** Dropping the future would
leave the statement running on the server, so a run has to name the backend
it is on. A query tab's session knows that from the moment it opens; the
pooled path — table pages — asks for `pg_backend_pid()` before it sends the
statement, because a pooled connection is a different backend each time.
`Connection::stop` then sends `pg_cancel_backend` — the statement comes
back as SQLSTATE 57014 — or `pg_terminate_backend`, which closes the
backend and the connection with it. That is why terminate is the *second*
press.

Both go out on the **app pool**, never a session's: waiting for a free
connection would mean waiting for the statement the user just asked to
stop. See "A tab is a session" for why that reserve is a pool of its own.

`Live::backend` is an `Option<RunId>`, filled in before the run leaves.
**`None` is not a gap in what is known**: it says the tab's session is
still opening, and *no statement is out*, so there is nothing on the server
to cancel. A stop pressed there marks the run `Cancelling`, and the open
lands into a run that then does not start — the run is called off rather
than chased. That is why the stop needs no waiting: before sessions the id
arrived one round trip *into* the run, and the wait loop existed to cover
exactly that window. `Live` belongs to the run, not to the request, so both
presses keep the same start time: the timer must not restart because the
user asked twice.

A stopped run comes back as the server's own refusal. The tab paints
CANCELLED instead of an error strip, because the user is who asked — but
`query_history` still keeps what the server said. ⌘⏎ while a run is out does
nothing: a second run over the first would leave the first unstoppable.

`Shell::start_timer` repaints every 100 ms while any run is in flight, one
loop for the window (`timing`), and it ends itself when the last run lands.
The timer pill itself waits out `TIMER_DELAY` — one second — before it
appears, so it opens at `1.0 s` and a statement that answers at once never
raises it. It is there to say a query is taking a while.
Table pages are not cancellable — they are `LIMIT 500` and the design puts
the button in the query toolbar.

The RUNNING line says "the server has the statement", not the comp's
"streaming · N rows buffered". The driver does stream the rows now (see
below), but it hands the result over in one piece at the end, so there is
still no buffered count to report without lying. A count needs the driver to
report *progress*, not merely to stream; that line is where it shows up.

### A tab is a session

**The tab is a session in the user's head, so it has to be one on the
wire.** Statements sent on whichever pooled connection happened to be free
cannot see each other's `SET`, cannot share a `BEGIN`, and cannot read a
temp table the statement before them made. Nothing short of one connection
per tab fixes that: replaying the settings would mean parsing the user's
SQL, and no amount of parsing replays a temp table or an open transaction.

`db_client::Session` is that connection. `Connection::open_session` pins one
and hands it back; a query tab holds it in `QueryTab::session` from its
**first run** onwards — never before, so a strip of a hundred restored tabs
costs the server nothing until the user asks one of them a question. Every
statement of a buffer then goes down that one connection, in order, so
`BEGIN; SELECT …;` in one buffer means what it reads as.

**Two pools, and the split is the point.** `APP_CONNECTIONS` (3) serves
what the app does for itself — introspection, table pages, and every
`pg_cancel_backend`. `SESSION_CONNECTIONS` (8) is the ceiling on live tabs.
Stopping a run must never wait for a free connection, because the
connection it would wait for is the statement the user asked to stop; a
reserve written as a comment over one shared pool would be a reserve until
the day it was not. Both pools are built from the same `PgConnectOptions`,
so a session's startup packet is the app's — one read-only flag, no second
story about what a connection may do. The session pool connects lazily.

The session knows its **backend at open**, not one round trip into each
run, so a stop or a close has something to aim at from the moment a run
leaves. The `statement_timeout` probe rides the same trip and is likewise
paid once per session. Only a tab's very first run still races.

**A failed statement must not cost the session.** A syntax error does not
end a transaction, so `RunOutcome` carries the session back beside the
result and the tab keeps it either way — `session` is `None` only when
opening it is what failed. Dropping it on error would lose whatever the
user had set *and* hand the pool a connection with an open transaction.

**Closing a tab rolls back.** Not for the server's sake — a closing socket
already rolls a transaction back, at once, with no timeout involved — but
for the **pool's**: that connection is about to be reused and must not
carry the user's half-finished transaction to whoever acquires it next.
`ROLLBACK` is sent whether or not a transaction is open, because it costs
one round trip to send and one to ask; outside a transaction Postgres
answers "there is no transaction in progress" and carries on. A connection
that fails to *answer* is detached and closed rather than pooled — closing
is itself a rollback, so there is no state left stuck.

Leaving the shell and quitting need none of this: the pool dies with them.

**A session must survive being dropped off the runtime.** sqlx returns a
pooled connection to its pool by *spawning onto tokio* when the last handle
drops, and spawning off the runtime panics — inside an Objective-C callback
that cannot unwind, so the process aborts. The last `Arc<dyn Session>` is
routinely dropped on the UI thread: "‹ connections" drops the shell, every
tab and every session inside a mouse-up. So `impl Drop` on each session
**detaches** the connection first; a bare `PgConnection` drops by closing
its socket, which needs no runtime. It is the fallback, not the ordinary
path — `close` gives the connection back properly from inside a tokio task
and leaves `Drop` nothing to do. Both drivers have a test that drops a
session on a plain `std::thread`, which is what the UI thread looks like to
sqlx.

**Sessions are given back when nobody is using them.** `IDLE_SESSION` is
ten minutes and `Shell::sweep_idle_sessions` runs every `IDLE_TICK` — one
loop for the window, as the run timer is, and it ends itself once the last
session has gone, so a workspace on no connections does not keep waking up
to notice that. A laptop left open overnight must not hold every connection
against a shared server for a window nobody is looking at.

`db_client::MAX_SESSIONS` (8) is the ceiling, known to both ends: the
driver sizes its session pool by it, and `Shell::make_room_for_session`
checks it before asking for one too many. Over the cap, the session that
has gone longest without a question is given back. **Two states are never
taken**: a run in flight is using its connection, and a transaction would
be rolled away with nobody asking — the very thing the close dialog exists
to refuse to do quietly. So a tab sitting mid-transaction keeps its
connection however long it waits, and the cap gives way instead: with
nothing spare the run is refused outright, naming both ways out, rather
than left to wait out the pool's connect timeout and come back with a
message about connections.

The policy is three plain functions over `SessionState` — `spare`,
`idle_sessions`, `evictable` — so it can be argued with in a test rather
than in a running window.

**Every taking-back is visible.** `QueryTab::session_ended` puts
`session ended · a run opens a new one` in the toolbar until the next run,
because a `search_path` that reset an hour later must be explainable rather
than a mystery. Beside it, `IN TRANSACTION` in the accent's family marks
the state where everything the tab does is inside something a close would
roll back. Neither mark says "a session is open": that is the ordinary case
and needs no badge.

### Closing something that is still running

**Closing a socket is not a cancel.** Postgres notices the client is gone
when the backend next writes, which a long `SELECT` may not do for minutes.
So a close that abandons a run leaves the server working for a window that
is not there any more, and nobody is told. That is what the confirmation is
for — not tidiness.

`Shell::guard_close` is the guard, and it answers `true` for "go ahead" or
`false` for "the dialog is up, and the close happens when the user says so".
**Every way out calls it.** There are four, and only one of them is ⌘W:

| Way out | Asks about |
|---|---|
| ⌘W, or the × on a tab | that tab |
| "‹ connections" | every tab |
| ⌘Q | every tab |
| the window's close button | every tab |

The last two reach the shell through `Root::guard_quit`.
`on_window_should_close` wants a yes or no on the spot and the question
takes a person to answer, so it answers **no** and puts the dialog up;
agreeing to it quits from there. A guard wired only to ⌘W would be a lie in
the other three.

A close that loses nothing never asks. **Two things count**: a run still in
flight, and a transaction still open. Each gets its own line, because they
are lost in different ways — a run the server can be asked to give up, a
transaction it throws away — and one line cannot stand for the other. A tab
in both states says both. Nothing else counts: the editor's buffer is
already written back, and a result on screen is one the statement above it
will fetch again. A dialog the user meets every time is one they learn to
dismiss.

**The transaction is known before it is asked about.** `Session::
in_transaction` reads `pg_stat_activity` from the *app* pool — not from the
session, whose connection may be busy with the very run in question, and
which refuses every statement once its transaction has gone wrong: the
session in the state most worth reporting is the one that cannot report it.
`idle in transaction` and `idle in transaction (aborted)` both count.

`Shell::refresh_transaction` reads it **after every run**, off the run's
path, and caches it on the tab. The cache is exact rather than a guess: the
connection is pinned to one tab, so nothing but that tab's own statements
can change what it is in. Reading it at close time instead would mean a
close that waits on a round trip before it can decide what to ask.

⌘⏎ ends the runs, ⏎ and ⎋ both keep what is open, and the scrim does not
dismiss on a click — unlike the palette's, because a stray click must not
answer a question about ending server work. The ending button is clay and
filled, the tone the terminate button wears. `confirm_copy` is pure, so the
three ways out cannot drift into saying three unrelated things, and the
wording is testable without a window.

On confirm the runs are cancelled with `pg_cancel_backend`, the same call
the stop button sends. **⌘Q waits for those requests and the other two do
not**: quitting drops the tokio runtime, so a cancel that has not left yet
never leaves. Closing a tab must not wait on the network, so its requests
are detached — and detaching is required, not tidy: `Tokio::spawn` aborts
its future when the handle is dropped.

A close during a session's *opening* cancels nothing, and needs to cancel
nothing: no statement has been sent. The tab goes, and the run that was
about to leave never does.

### How much a result may weigh

A typed statement is the user's, so `SELECT * FROM big_table` with no
`LIMIT` is a statement the app must survive rather than refuse. The rows are
**streamed and capped**, in `db_client::RowSink`:

- `MAX_BYTES` is 256 MB of decoded values per result.
- `MAX_CELL_BYTES` is 1 MB of any one value. A cut `Text` ends in `…`.

**The cap is on memory, not on rows.** A row count is only a proxy for what
runs the process out of room, and a poor one: 120,000 two-column rows cost
about 60 MB, and 120,000 rows of wide `jsonb` cost gigabytes. One number
cannot bound both, so the bound is the resource itself — and a narrow result
of a few hundred thousand rows comes back whole, which is the point.

**Nothing rewrites the user's SQL.** Injecting a `LIMIT` breaks on CTEs,
`UNION` and statements that are not `SELECT`, and it would put SQL in
`query_history` that the user never wrote. The bound is on what comes back.

A row that would carry the result past the cap is dropped and the result is
marked `truncated`; a result that ends exactly on the cap is whole, not
truncated. The first row is always kept, or one huge row would come back as
an empty grid. `QueryResult::truncated` is `Ok`, never an error: the
statement did not fail, the app declined to hold the rest. The query tab
says so beside the row count, in the accent's family rather than the error's.

**The cap stops the server too.** Dropping the stream would bound this
process and nothing else — sqlx must read every remaining row off the wire
before the connection is usable again, so the server would go on building
and sending a result nobody will read. So the Postgres driver sends
`pg_cancel_backend` at the cap, the same call the stop button sends, and
then drains what is still in flight. That is why every run has to name its
backend: a session knows it from the moment it opens, and the pooled path
pays a round trip for it, so even a run nothing is watching can be stopped.
`collect_capped` is the one place that streams, caps, cancels and drains,
and both paths go through it. SQLite needs none of it — the rows come from
a local file, so dropping the stream ends the work.

`Limits` is a field on the connection, and `with_limits` is for the tests: a
cap of a few kilobytes is reached in a query that takes no time, where the
real one would want gigabytes of fixture.

### How long a run may take

`STATEMENT_TIMEOUT` is 30 seconds, and **only when nothing else has set
one**. A viewer must not leave a statement on a shared server for ever
because a window is open somewhere.

**Postgres says where a setting came from**, which is what makes "only when
nothing else has" answerable: `pg_settings.source` reads `default` when
nothing anywhere named a value, and otherwise names the level that did —
`user` for `ALTER ROLE`, `database`, `configuration file`, `client` for a
startup option in the connection URL, `session` for a `SET` the user typed.
So the driver defers to every one of those, including a deliberate `0`,
which `default` would never report. `current_setting()` cannot tell those
apart: it answers `0` for "nobody asked" and for "somebody asked for none".

The ask rides the round trip that already reads the backend id, so the guard
costs nothing:

```sql
SELECT pg_backend_pid(),
       (SELECT set_config('statement_timeout', $1, false)
          WHERE (SELECT source FROM pg_settings
                  WHERE name = 'statement_timeout') = 'default')
```

`set_config(.., false)` *is* `SET`, and the `WHERE` is what makes it
conditional — a `DO` block could not, because `SET` inside one is scoped to
the block. The subquery answers NULL when somebody else already set the
value.

**It is not a startup option, though `default_transaction_read_only` is.** A
startup option outranks `ALTER ROLE`, so asking for one would quietly
overrule the DBA, and it is fixed before the connect, so it cannot depend on
what the server turns out to say. The read-only flag has the opposite need:
it is a promise, so it must survive `RESET ALL`, and there is nothing on the
server to defer to. A timeout is a guard, not a promise — a `RESET ALL`
washes it off that pooled connection and the next run puts it back, because
every run asks.

Two things follow. A timed-out statement comes back as **SQLSTATE 57014**,
the same code as a cancel, because it is the same event from the server's
side; the tab paints it as an error rather than as CANCELLED, since `Run`
is `Running` and not `Cancelling`, and the server's own message says
"statement timeout". And the `SET` is session-level on a pooled connection,
so an `introspect` that lands on a connection a run has used inherits the
timeout, though introspection never asks for one itself.

### The read-only session

A typed statement goes to the driver verbatim, so `DROP TABLE` is only
refused if something refuses it. `Profile::read_only` is that something,
and **the server enforces it, never the app**: `connect_with` asks for
`default_transaction_read_only=on` in the startup packet, and Postgres
answers a write with "cannot execute DROP TABLE in a read-only
transaction". Nothing reads the SQL, so there is no pattern to slip past.

It is a **startup option, not a `SET`**, because `RESET ALL` — which
`DISCARD ALL` runs, and a pool may — restores a parameter to the value the
session *started* with. A `SET` would be washed away by exactly the kind of
reset a pool does between one tab and the next.

The flag defaults to **on** everywhere: a new connection in the form, a row
an older build saved (the column reads NULL as on), and a command-line URL,
which carries no setting to read. It is a connection parameter, so it lives
on `Profile` — unlike the environment tag, which is a label on the store's
row.

Two limits are worth knowing. The setting is the session's *default*, so a
statement may turn it off for itself; only a role without write rights
closes that door. And a connection pooler that refuses the `options`
startup parameter fails the connect — which is the right way round, because
a read-only session that cannot be asked for must not open at all.

The mode is on screen at all times: `Shell::mode_mark` paints the comp's
padlock badge in the top bar beside the environment badge, read-only in the
dev family's green and read-write in the prod family's clay, so the two
marks warn in the same tones. A session that never connected goes grey
(`mode_off_*`). The sidebar's foot and the query toolbar say the same word,
and the connections list carries it in its MODE column.

### Drivers

`db_postgres` reads `pg_catalog` (not `information_schema`) for tables and
columns, because it also carries the `reltuples` estimate and `format_type`
renders type names the way `psql` does. Primary keys come from
`information_schema`, which reports key column order.

Decode result columns by matching on `type_info().name()` — `try_get::<String>`
fails on non-text Postgres types. An unknown type must return
`Value::Text(format!("<{type_name}>"))` rather than erroring: a viewer must
not fall over on exotic columns.

`Table::approx_rows` is an estimate for the sidebar only. Never use it for
paging arithmetic that must be exact.

### GPUI conventions

- Every color comes from `theme::ThemeColors` tokens, never a literal in view
  code. If a token is missing, add it to `crates/theme` with a comment. The
  visual language is the "warm paper" comp at `docs/design/Meerkat.dc.html`.
- Font is bundled JetBrains Mono, loaded in `main.rs`.
- `.hover()`, `.cursor_pointer()` and `.truncate()` come from
  `InteractiveElement` / `Styled` — `use gpui::prelude::*` in view files.
- Every grid or table cell needs `.truncate()`, or a long value wraps and
  breaks the row height.
- Long lists use `uniform_list`, which needs one fixed row height.
- `sql_editor` and `ui::text_field` each own a buffer, implement
  `EntityInputHandler` for platform text and IME input, and paint through a
  custom `Element`. Their key bindings are scoped to a `KEY_CONTEXT` string
  and returned from `key_bindings()` / `text_field_key_bindings()`, which
  `main.rs` binds. Offsets in both are byte offsets on character boundaries.
- Scroll state (`GridState`, `UniformListScrollHandle`) is held by whoever
  owns the tab, so it survives the re-render after every keystroke and page.

### Persistence

Profiles live in a SQLite file under the user data directory. `Store::migrate`
adds columns one at a time so an older profiles file keeps its rows. Passwords
go to the OS keychain keyed by profile id, and never touch that file.

## Pinned GPUI

`gpui`, `gpui_platform` and `gpui_tokio` are pinned by git rev to a single Zed
revision in the workspace `Cargo.toml`. Bump all three together and only
deliberately — GPUI is pre-1.0 and breaks APIs. `[profile.dev.package."*"]
opt-level = 2` is set because GPUI is unusably slow unoptimized; leave it.

## Conventions

- Commit subjects are short imperative sentences in sentence case, no
  conventional-commit prefix: "Add scrollbars to the results grid".
- Commit straight to `main`. This is a single-author repository with no
  review flow, so never open a branch for a change.
- Module-level `//!` docs explain why the module exists and what its
  non-obvious constraints are. Keep that habit when adding a module.
- License is GPL-3.0-or-later, so copying from Zed's crates is allowed.
  Prefer reading Zed for patterns over wholesale copying.
