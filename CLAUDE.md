# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Meerkat is a native cross-platform database viewer built on [GPUI](https://www.gpui.rs),
the UI framework behind Zed. One Rust binary, no webview, no server. The repo
is a Cargo workspace of small crates in the Zed style. Current milestone:
read-only PostgreSQL. In-place editing and MySQL are Phase 2.

## Commands

```sh
cargo run -p meerkat                                        # connections screen
cargo run -p meerkat -- postgres://user@host:5432/database  # straight to a database
cargo check --workspace
cargo test --workspace
cargo test -p sql_editor completion::tests                  # one crate / one module
cargo test -p meerkat pages_order_by_the_primary_key        # one test by name
```

`MEERKAT_DATABASE_URL` works instead of the CLI argument.

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
| `meerkat` | Binary: `main.rs` boots, `root.rs` switches screens, `connections.rs` and `shell.rs` are the two screens, `history.rs` is the query-history tab, `sql.rs` builds the app's own SQL |
| `db_client` | Engine-agnostic `Connection` trait, `Profile`, `Value`, `QueryResult`, `RowChange` |
| `db_postgres`, `db_sqlite` | sqlx drivers behind that trait |
| `introspect` | Schema model (`Catalog` → `Schema` → `Table` → `Column`) that drivers fill |
| `query` | `statements()`: split a buffer into statements, quote- and comment-aware |
| `sql_editor` | Multi-line SQL buffer: motion, undo, colouring, completion |
| `results_grid` | Virtualized grid plus its overlay scrollbar |
| `ui`, `theme` | Component kit (incl. `TextField`) and color tokens |
| `storage` | Local SQLite: profiles, cached probe counts, layout, history |
| `secrets` | OS keychain wrapper for passwords |
| `workspace`, `schema_tree` | Empty placeholders |

### Screens

`Root` holds exactly one of `Connections` or `Shell` at a time, and swaps
between them on an emitted event. Leaving a `Shell` drops it, and with it the
connection pool, so a closed database keeps no sockets open.

`Shell` owns the session: the `Arc<dyn Connection>`, the introspected
`Catalog`, the flattened sidebar rows, the completion vocabulary, and the
open tabs (`Tab::Table`, `Tab::Query` or `Tab::History`).

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
