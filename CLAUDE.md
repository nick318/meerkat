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

A **dot in the query names a path**. Each result carries its name in parts
— schema, relation, and a column's own name — and the typed parts line up
with the *end* of that path first, sliding one part further out when
nothing hit there. So `task` finds every `task`, `dev.ta` finds
`sample_dev_sample.task`, and a bare schema name answers with the tables
it holds. Sliding is off for columns: a bare `task` there would return
every column of every task table.

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
