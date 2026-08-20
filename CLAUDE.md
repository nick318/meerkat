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
`db_sqlite`, `db_client`, `introspect`, `storage`, `secrets`, `query`,
`fuzzy` and `settings` must not know about `gpui`.

| Crate | Role |
|---|---|
| `meerkat` | Binary: `main.rs` boots, `root.rs` switches screens, `connections.rs` and `shell.rs` are the two screens, `history.rs` is the query-history tab, `palette.rs` is the ⌘K palette, `sql.rs` builds the app's own SQL |
| `db_client` | Engine-agnostic `Connection` trait, `Profile`, `Value`, `QueryResult`, `RowChange` |
| `db_postgres`, `db_sqlite` | sqlx drivers behind that trait |
| `introspect` | Schema model (`Catalog` → `Schema` → `Table` → `Column`) that drivers fill |
| `query` | `statements()`: split a buffer into statements, quote- and comment-aware |
| `fuzzy` | `Pattern::score`: one matcher for every search line over names |
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
`palette::path_matches`: the `fuzzy` crate's word-start rule, and **a dot
names a path**. So `address` finds every `address`, `addr_ln` finds
`address_line`, `dev.addr` finds the one in `sample_dev_sample`, and a
bare schema name answers with everything under it. Whatever is left with nothing under it is dropped, header and all,
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
click, which asks to read the value whole), a cell the pointer has
dragged onto, the gutter beside a row, the gutter's head, or a column
header — and `Shell::hit_handler` turns it into a call on `Selection`. So
the mouse and the keys move the same selection through the same code.

**A hover says what a click there would do, so it is drawn on the cell.**
A click on a cell marks that cell, so the cell alone lights up; washing
the whole row would promise a record the click does not mark. The
**gutter is the exception**, because it is the one target that does speak
for the whole record: hovering it washes the row it belongs to, which is
how the user is told the row can be ticked there.

That wash is painted by the row, and a hover style paints only the
element it is set on — no style reaches up to a parent. So `Pointer`
holds the row the gutter is over, the gutter's `on_hover` writes it, and
every row reads it back. It lives beside the scroll position in
`GridState`, because it is the mouse's own state between one frame and
the next rather than anything the user has marked.

**A press and a drag draw a range.** The press lands on `Hit::Cell` and
sets the anchor; every cell the pointer then reaches with the button
still down lands on `Hit::Drag`, which is `extend_to` — the same call ⇧
with an arrow key makes. It goes out on the mouse *down*, not the click,
because a drag has to start from the cell the button went down on and the
release may be three cells away, or off the pane entirely. `Hit::Drag` is
the one hit that is not a fresh click, so it neither takes the focus nor
closes the column-find popover: the press before it did both.

**Everything after the press lives on the window, in `DragSurface`** —
an element that paints nothing and takes no room, there to hang
listeners from. That is the scrollbar's pattern, and here the reason is
sharper than "a drag outlives its element": the whole point of dragging
past the edge is that **the pointer has left the cells**, so handlers
bound to a hitbox would go deaf exactly when they are needed. It also
means no cell carries a move handler — one listener a grid, not one for
every cell on screen.

So the cell under the pointer is **worked out from the geometry**, not
asked of the elements: `row_at` and `column_at` read it off the scroll
offsets and the lane widths, and outside the pane they answer with the
nearest cell, which is the one the drag is reaching for. Both are plain
functions over numbers, so they are argued with in a test.

**Past an edge, the grid scrolls itself.** A pointer held out there
sends no further events, so the scroll cannot be driven by the mouse: it
is one step per *frame*, `AUTOSCROLL_MIN`..`AUTOSCROLL_MAX` pixels, and
each frame asks for the next by moving the selection, which repaints.
The speed is the overshoot itself, floored so a pointer a hair over the
line still moves readably and capped so a pointer flung to the far side
of the screen does not cross a 500-row page in three frames. Inside the
pane it scrolls **nothing**: a drag that crept while the pointer sat in
the middle of the result could never be made to stop. When neither axis
can travel any further the loop ends, or a drag held past the last row
would repaint for ever.

The drag ends on the button coming up anywhere in the window, and a move
that arrives with no button held ends it too — so a release nothing here
heard about cannot leave the grid dragging for ever.

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

### Matching a name against what is typed

Four search lines ask the same question — the sidebar's filter, the ⌘K
palette, the ⌘J column find, and the SQL editor's completion panel — and
`fuzzy` is the one answer. Before it there were three: a substring, a
substring per path part, and a prefix, so `mast_cl` found
`master_client_reference` in none of them and the same name was found three
different ways depending on which line the user was typing into.

**A query names the starts of a name's words**, which is the idea
IntelliJ's `MinusculeMatcher` is built on. The query is cut into
*fragments* at everything that is not a letter or a digit; the first may
sit anywhere in the name, and every one after it must begin a word — where
a word starts at the name's front, after a separator, at a case change, or
at the step between letters and digits. Inside a fragment a character
either follows the one before it or begins a word, which is what lets
`mcr` find `master_client_reference` by its initials. So `mast_cl` finds
`master_client_reference` and `masterClientReference` both, and `client`
finds it as well.

That last rule is the gate, and it is what keeps this from being a fuzzy
finder. An unrestricted subsequence — fzf's rule — answers a three-letter
query with half of a hundred-column result, and a jump list that long is
not a jump. Nothing lands loose in the middle of a word.

The score is fzf's shape: a match, more for a word start, more again for a
character straight after the last one, and a gap costs. Two constants are
rules rather than taste. **`GAP_START` costs more than the word start it
buys**, so a name matched in one piece always beats a name matched in two
— or `users` would rank `user_settings` above `users`. And the name's own
first character is worth a shade more than an inner word start, which is
what sorts `master_state_type_code` above `invoice_master_name` for
`mast`. Callers break the remaining ties on the length of the name.

The alignment is a dynamic program, not a greedy walk, because the greedy
answer is wrong often enough to see: `cl` in `include_client` has to land
on whichever of the two scores better. A subsequence scan runs first and
throws out most of a large vocabulary without allocating.

Case is ignored until the query shows it means it: a query written in
**both** cases reads as strict, so `Cl` then asks for a capital or for a
word starting with one, while `EMAIL` still finds `email`.

**Two places do not use it.** A run's statement in the palette is matched
by substring, because it is prose rather than a path of words — see the
palette section. And SQL keywords in the completion panel are matched by
prefix: a keyword is one word with nothing to walk, and matching inside
one would answer `em` with `temp`.

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

`results_grid::find_columns` is the whole of the matching: the `fuzzy`
crate's rule over the column names, answering with indices into the
result's own lanes, **ranked rather than in result order** — a search
that offers the right column fourth is one the user reads before they can
use it, and the score is what says which is closest. Ties go to the
shorter name, then to the earlier lane. A column's **type** is not
searched: a result carries
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

Matching a **name** is the `fuzzy` crate's rule, so `mast_cl` finds
`master_client_reference`; rows are ranked by how far out the query
aligned, then by the score, then by name length. Matching a **run's
statement** is still a plain case-insensitive substring, and the
difference is the point: a name is a path of words, which is what the
word-start rule reads, while a statement is prose full of word starts, so
the same rule over it would answer a short query with everything the
connection has ever run. IntelliJ draws the line in the same place — a
hump matcher for symbols, a substring for find-in-text.

The statement comparison lowercases ASCII only, which keeps the hit's byte
range into the lowered copy valid in the original so the row can underline
it. `fuzzy` needs no such rule: it walks `char_indices` and hands back
ranges on character boundaries of the string it was given.

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

**The button also moves, and `RunPhase` is why the states line up.** `Run`
is the state of the *statement*; `RunPhase` is the state of the *button*,
and the two part company for the first `RUN_ARM` — 420 ms — of every run:
the statement is out, and the button still says **run**. Most statements
answer inside that window, and a verb that read "run · stop · run" on every
one of them would be a button nobody could aim at. The comp says the same
thing in its own comment — *"stop" only appears once the query has proven
itself slow* — and gets there by knowing how long its simulated query will
take; the elapsed time alone is the honest reading of the same rule, and it
paints the same screen. A **click** in that window therefore does nothing:
the word under the pointer says "run", and this tab already has one out. ⌘.
is untouched — a key press is aimed at the run, not at a word on a button.

**One thing moves, and it is the fill.** The comp animates a run with the
vocabulary of a progress widget: a front sweeping the button, an
indeterminate band crossing it on a loop, a ring blooming out of the border
when the rows land. Those are Material's marks, and they are a lot of
movement for a 27-pixel button that sits beside a paper toolbar all day —
each of them carries a shape, and a shape crossing a button asks to be
watched. A tone does not. So the animation here is the fill mixing a little
colour in and back out again:

| Mix | When | What it says |
|---|---|---|
| toward the button's **own ink**, `RUN_BREATH_DEPTH`, breathing over `RUN_BREATH` | a run is out | working |
| the same mix, easing out over `RUN_SETTLE` | a result lands | that came back |
| toward the **app's ink**, `RUN_PRESS_DEPTH`, at once | held down | the press landed |

`Hsla::blend` is the whole of it, and one formula for all three moods is what
keeps them from drifting into three unrelated effects. **The direction is the
message**: the button's own ink is paper on a filled button and clay on the
outlined one, so mixing that way is lighter or warmer but always "working",
on every state, with no tone per state; the app's ink is darker everywhere,
and that is "held down". A press holds the button still at its pressed tone —
what is under the pointer must not also be breathing. Nothing travels,
nothing blooms, and nothing moves by a pixel: not the button, and not the
keycap, which is the one part of it that would still read as a mechanism.

The breath and the settle are GPUI animations, never ticks of the shell's
timer: `with_animation` asks for its own frames, restarts when the element's
id changes, and holds still under `reduce_motion`. The breath is
`repeat_synced`, phase-locked to the app's clock, so it does not start over
every time something else on screen rebuilds the element. The settle's id
carries `QueryTab::landed`, a **count of results**, which is the whole of why
the exhale needs no `Instant` and no timer of its own. It counts results and
not replies — a failure is not something to congratulate the user on — and
`landed == 0` is what keeps a freshly opened tab from exhaling at a user who
has asked it nothing.

The press is also the *click*: `on_click` keeps its half-finished state under
the element's id, that id path carries whichever animation is running, and a
button held down across the moment a result lands would come up under a
different id and lose the press. `Shell::run_pressed` is where the press
lives instead — mouse state between one frame and the next, beside the
palette rather than on a tab, for the reason the grid's hovered row lives on
`GridState`.

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
than a mystery. It does not say "a session is open": that is the ordinary
case and needs no badge. An open transaction is marked by the transaction
bar below, which is a strip and not a badge — see the next section.

### Who ends a transaction

A statement on a Postgres connection commits as it succeeds, and for a
viewer that is the whole story. It stops being the whole story the moment
the connection is the user's to write on: a set of statements that has to
land together, or a `DELETE` worth looking at before anyone else can see
it, needs a transaction the user ends rather than one the server ends for
them. So `db_client::TxMode` is a property of a **tab** — `Auto` or
`Manual` — because a transaction lives on one connection and a tab is one
connection.

`Profile::tx_mode` is the mode a new tab opens on, set in the connection
form beside the read-only switch. It is **not** a connect parameter —
nothing in the startup packet says it, and the app is what holds the
transaction open — but it belongs to the connection rather than to a
window: "manual on prod, auto on the laptop copy" is a decision about the
database. A command-line URL carries no setting and opens on `Auto`, and a
profiles row an older build wrote reads as `Auto` — the mirror image of
`read_only`, whose missing answer is the *careful* one. Here the careful
answer is to hold nothing open. A tab remembers its own mode in
`open_tabs`, so a tab switched to manual comes back manual; the
transaction is not remembered, because a restored tab has no session and
so has nothing open.

**In manual mode a run opens the transaction and nothing closes it but the
user.** `needs_begin` is the whole of the decision, and it answers no
three ways: auto mode holds nothing open; one is open already, so a second
`BEGIN` would be a warning from the server and a lie in the bar; and the
buffer opens its own, which is the user saying where the transaction
starts. The `BEGIN` goes out through `Session::begin` rather than
`Session::execute`, because **a transaction boundary is not a run**: it has
no result to cap, no timing worth reporting and no columns to describe, and
`execute` would pay a round trip on the app pool asking the server what
columns `COMMIT` returns. It is not counted in the run's statements either
— but the clock does start before it, because the round trip is part of
what the user waited.

**A typed `BEGIN` is handled in either mode, and that is the point of
`query::transaction_verb`.** In auto mode a `BEGIN` the user wrote opens a
transaction exactly as manual mode does, and before this the only way out
of it was to close the tab — which rolled it back. Now the bar appears
either way. The verb reader takes the first word or two past any leading
comments and does not attempt to parse SQL: `ROLLBACK TO SAVEPOINT` is not
an end, `START` is only a verb with `TRANSACTION` after it, and a `begin`
inside a dollar-quoted body never reaches it because `statements()` hands
such a body over whole. **Being wrong is cheap on purpose** — the server's
own answer, `Session::in_transaction`, is what the app believes about the
state afterwards, so a misread costs at most one spare `BEGIN` or a bar
that says nothing rather than something wrong. The buffer's **last** verb
is what the bar reports, because `BEGIN; …; COMMIT` leaves nothing open.

The bar sits directly over the result, because it is about what the runs
have done rather than about the statement above them. `tx_copy` is pure, so
the wording is testable without a window, and `None` means no strip at all:
a strip that said "no transaction" would be one that is always there saying
nothing. Open warms to the accent's family, a commit rests in the dev
family's green that the read-only mark already wears, and a rollback goes
back to paper — nothing was written, so nothing is worth a colour. It stays
up for a moment after the transaction ends, saying which way it went,
because "committed" is the answer to the question the user just asked; the
next run clears it, since news about a transaction that was over before the
run is not news.

The second line counts **statements, not rows**. The comp says "N rows
touched", and the driver reports no affected count today — `rows_affected`
is filled in nowhere — so a row figure would be invented. A statement count
is a number the app has. What it counts is the statements that are *not*
boundaries, so a bare `BEGIN` opens a transaction with nothing in it, which
is what it did.

⌘S commits and ⇧⌘R rolls back, and the bar's two buttons say so on their
faces: this is the only place those keys are written down. **⇧⌘R is not the
comp's ⇧⌘Z**, which is Redo in the SQL editor — the editor's context sits
*inside* the shell's, so a binding here would never fire while the user is
typing, and taking redo off a text editor would be the wrong trade even if
it did.

Both go down the tab's **own** session, because that is where the
transaction is: a `COMMIT` on a pooled connection would commit nothing and
report success. So a boundary waits for that connection the way a run does
— and a run in flight is holding it, which is why a boundary is **refused
rather than queued** behind one, naming ⌘. as the way out. The buttons go
faint rather than away: the transaction is still there, and so is the
answer to it once the run has stopped. `tx_ending` is what stops a second
press from sending a second boundary, and it blocks ⌘⏎ for the same reason
a run in flight does.

A boundary is kept in `query_history`, unlike the `BEGIN`: "why did my work
disappear" is answered by a `ROLLBACK` in the list.

**Switching to auto is refused while a transaction is open.** It would
leave the transaction standing with nothing on screen offering to end it,
and the statements after it would land inside a transaction the tab says it
is not in. The auto chip paints faint and does nothing; the bar's two
buttons are the way out. Switching the other way is allowed at any time —
a user who typed `BEGIN` may well want the app to stop committing behind it.

Everything else about an open transaction is unchanged and still holds: the
sweep never takes a session that has one (`spare`), the cap gives way
rather than evicting one, closing a tab rolls it back for the *pool's* sake,
and every way out of the shell asks about it first.

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

### Where a run's time went

**A wall clock around the call cannot answer the question the user is
asking.** "Queried in 340 ms" says nothing about whether the database was
slow or the link was — and over a VPN to a remote server the link is
usually the larger half. So a run reports two numbers: what the **server**
spent on the statement, and the **lag** around it. `Timing` in `shell.rs`
holds both, and the status strip reads
`queried in 340 ms · server 12.4 ms · lag 328 ms`.

The two always add up to the total, because the lag is the total *less* the
server's share rather than a clock of its own. Time the app cannot account
for has to show up somewhere, and the honest place for it is the half that
means "not the database".

**There are two ways to know the server's share, and they are not equally
good.** The strip prefers the exact one and falls back to the estimate.

`db_client::Wire` is the **estimate**, and it is always there:

- `link_ms` — one round trip, timed around `prepare_run`. That statement
  reads two catalog values and sets a string, so the server's share of it
  rounds to nothing and what is left is the link. It costs no round trip of
  its own: the driver makes that trip anyway, for the backend id. Measuring
  the lag must not add to it.
- `first_row_ms` — statement sent to the first row off the wire. Postgres
  emits no row until it has one, so this bounds the server's work. Take the
  round trip off and what is left is the estimate.
- `fetch_ms` — first row to last row.

**The estimate is honest about what it cannot separate.** For a plan that
streams — a plain sequential scan — the server keeps working while the
rows cross, so server time and fetch time genuinely overlap and no
client-side clock can tell them apart. For a plan that blocks — a sort, a
hash aggregate, `count(*)` — the first row comes at the end, and
`first_row_ms` *is* the whole of the server's work.

`db_client::ServerTiming` is the **exact** answer, from
`pg_stat_statements`. It is the only place a client can ask: the wire
protocol carries no timing, `EXPLAIN ANALYZE` would mean rewriting the
user's statement and running it twice, and `log_min_duration_statement`
writes somewhere no client can read.

**It is asked for after the result is already on screen.** A round trip in
front of the grid would add lag to the very measurement the user opened
this to understand. So the result paints on the client's own reckoning, and
`Shell::refresh_server_timing` fills the server's figure in a moment later
— the strip drops its `~` and nothing else moves. It goes out on the **app
pool**, like `in_transaction` and for the same reasons.

**The view counts, it does not log.** One row holds the running totals for
every execution of a statement, cluster-wide, so a single reading can only
give a mean. `between` in `db_postgres` subtracts the totals the session
saw last time, and `calls` is what says how many executions landed in the
window:

| Δ`calls` | What is reported | `exact` |
|---|---|---|
| 1 | that run's own time | yes |
| more than 1 | the mean over the window — somebody else ran the same statement | no |
| no earlier reading | the mean over every execution counted so far | no |
| 0, or `calls` went down | nothing; the reading becomes the baseline | — |

So the **first** run of a statement in a tab gets a mean and the second
gets a measurement. `~` in the strip is the whole of what says which is
which, and an unmarked number claims more than the app knows.

`pg_stat_activity.query_id` is what names the statement — it needs
`compute_query_id`, which `pg_stat_statements` turns on by itself, and it
is retained on an idle backend, being that backend's *most recent* query
rather than only a running one. So the two views join on it and the driver
never matches SQL text, which would not match anyway: the view normalizes
constants out. `userid` and `dbid` are part of the join because `queryid`
is not a key on its own.

Three things are deliberately not asked:

- **A buffer of several statements.** The server names only the one a
  backend ran last, so reporting it as the run's time would be a wrong
  number rather than a partial one. `one_statement` gates the ask.
- **A table page.** It runs on the pool, so the backend that ran it has
  gone back and may be running somebody else's statement. A page is the
  app's own `LIMIT 500`, not a statement anyone is tuning.
- **A second time on a server that cannot answer.** The first error sets
  `no_statement_stats`, or a server without the extension would pay one
  wasted round trip per run for ever.

SQLite leaves `Wire` empty on purpose. There is no link to measure and no
server to blame — the rows come off a local file, so the wall clock is the
whole story and splitting it would invent two numbers out of one.

`query_history` keeps the **total** alone. The exact figure lands after the
row is written, and a history row that disagreed with the strip would be
worse than one that says what the user waited.

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
fails on non-text Postgres types. An unknown type must never error: a viewer
must not fall over on exotic columns.

**What sqlx cannot decode is read off the wire.** sqlx knows the built-in
types by OID, under upper-case names, and calls everything else by the name
it reads back from the catalog — `xid8`, `tsvector` — with no `Decode` for
it, so `try_get` fails on every Rust type. `off_the_wire` takes the bytes
instead. A statement sent **unprepared** answers in text format, which is
Postgres's own rendering and is right for every type there will ever be. A
run is **prepared**, so it answers in binary, where each type is its own
layout and nothing generic can be said: `xid` and `xid8` are big-endian
unsigned integers, `tsvector` is read by `tsvector` — a lexeme count, then
each lexeme NUL-terminated with its positions, the weight in the top two
bits of each — and everything else keeps the `<type_name>` placeholder it
had. A short or malformed value is an error on that cell, never a panic.

A transaction id is unsigned 64 bits, so one past `i64::MAX` goes through
as its digits rather than as a negative `Value::Int`.

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
