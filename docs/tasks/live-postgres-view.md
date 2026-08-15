# Task: make Meerkat live — PostgreSQL, read-only data view, query runner

Instructions for a Claude session (or a human) picking up the next milestone.
Read this whole file before writing code.

## Context

Meerkat is a native cross-platform DB viewer built on GPUI (Zed's UI
framework). The repo is a Cargo workspace of small crates, Zed-style.
Current state (see `git log`):

- The app opens one window and renders the **table screen as a static mock**
  (`crates/meerkat/src/shell.rs`) with hardcoded sample data.
- `crates/db_sqlite` is a **working** driver: introspection + queries via
  sqlx, with a passing test. Use it as the reference implementation.
- `crates/db_client` defines the engine-agnostic `Connection` trait
  (`introspect` / `execute` / `apply`), `Profile`, `Value`, `QueryResult`.
- `crates/db_postgres` is an empty stub.
- `crates/storage` (profiles in local SQLite), `crates/secrets` (OS
  keychain), `crates/theme` (warm-paper tokens), `crates/ui` (small
  component kit) all compile and have tests where meaningful.
- `gpui`, `gpui_platform`, `gpui_tokio` are pinned by git rev in the root
  `Cargo.toml` to Zed rev `9bde578ef5af`. Do not bump the rev in this task.

## Scope of this task

1. **PostgreSQL driver** (`db_postgres`): implement `Connection` for
   Postgres via sqlx. Read-only — `apply()` returns an error, same as
   db_sqlite does today.
2. **Live data view**: sidebar lists real schemas/tables from the connected
   database; clicking a table shows real rows in the grid, paginated.
3. **Query runner**: a query tab with an editable SQL buffer; run on ⌘⏎ or
   the "run" button; results in the same grid component; show timing and
   row count; show errors in the design's error tones.

Explicitly OUT of scope: in-place editing, MySQL/MSSQL, connection-form
polish (a minimal path to connect is enough — see below), auto-update,
packaging.

## Design constraints (non-negotiable)

- The visual language is the "warm paper" design comp checked in at
  `docs/design/Meerkat.dc.html` (open it in a browser). Every color must
  come from `theme::ThemeColors` tokens — never a literal in view code.
  If a token is missing, add it to `crates/theme` with a comment.
- Font is bundled JetBrains Mono (`theme::FONT_FAMILY`); already loaded in
  `main.rs`.
- Keep the crate boundaries: UI crates may depend on data crates, never the
  reverse. `db_postgres` must not know about gpui.
- License is GPL-3.0-or-later, so copying code from any Zed crate is
  allowed. Still prefer reading Zed for patterns over wholesale copying.

## Implementation plan

### 1. db_postgres driver

Mirror `crates/db_sqlite/src/lib.rs`. Specifics:

- Enable the feature in `crates/db_postgres/Cargo.toml`:
  `sqlx = { workspace = true, features = ["postgres"] }`.
- Connect with `PgPoolOptions` from a `db_client::Profile` plus a password
  fetched via `secrets::get_password(&profile.id)`. Also accept a raw URL
  (`postgres://...`) for the bootstrap path below.
- Introspection — one query for tables + columns, one for primary keys:
  - Tables/columns: `information_schema.columns` joined with
    `information_schema.tables`, filtered to
    `table_schema NOT IN ('pg_catalog', 'information_schema')`;
    map `is_nullable`, `data_type`, `column_default`.
  - Primary keys: `pg_index` joined to `pg_class`/`pg_attribute`
    (`indisprimary = true`), or `information_schema.key_column_usage` +
    `table_constraints` — either is fine; keep key column order.
  - Approximate row counts for the sidebar: `pg_class.reltuples` (cast to
    i64, clamp negatives to 0). Add `approx_rows: Option<u64>` to
    `introspect::Table` — update db_sqlite too (it can return `None` or a
    `SELECT count(*)` per table; `None` is fine).
- `execute()`: `sqlx::query(sql).fetch_all(&pool)`, decode per column by
  `type_info().name()`. Handle at least: BOOL, INT2/INT4/INT8, FLOAT4/
  FLOAT8, NUMERIC (decode as string via `sqlx::types::BigDecimal` feature
  `bigdecimal`, or `try_get::<String>` will NOT work — pick BigDecimal and
  render `to_string`), TEXT/VARCHAR/BPCHAR/NAME, UUID (feature `uuid`),
  TIMESTAMP/TIMESTAMPTZ/DATE/TIME (feature `time`, render ISO strings),
  JSON/JSONB (`serde_json::Value` → compact string), BYTEA (`Vec<u8>`).
  Unknown type: return `Value::Text(format!("<{}>", type_name))` rather
  than erroring — a viewer must not fall over on exotic columns.
- Tests: gate behind env var. `#[tokio::test]` that reads
  `MEERKAT_TEST_PG_URL` and returns early (with eprintln) when unset, so
  `cargo test` stays green without a server. Test introspection and a
  `SELECT` with mixed types.

### 2. Async bridge (tokio ↔ GPUI)

All sqlx work runs on tokio; all UI state lives on the GPUI main thread.

- Add `gpui_tokio.workspace = true` to `crates/meerkat/Cargo.toml` and call
  `gpui_tokio::init(cx)` first thing in `application().run`.
- Pattern for a DB call from a view (check the pinned rev's source at
  `crates/gpui_tokio/src/gpui_tokio.rs` in the Zed checkout for exact
  signatures):

  ```rust
  let task = gpui_tokio::Tokio::spawn(cx, async move {
      connection.execute(&sql).await
  });
  cx.spawn(async move |this, cx| {
      let result = task.await; // JoinError wrapper, then inner Result
      this.update(cx, |this, cx| { this.apply_result(result); cx.notify(); })
  })
  .detach();
  ```

- Wrap the connection in `Arc<dyn Connection>` so tokio tasks can own a
  clone. Add `dyn`-compatibility check: the trait already uses
  `async_trait`, so `Arc<dyn Connection>` works.

### 3. App state

New crate wiring inside `crates/meerkat` (or promote to `crates/workspace`
if it grows): a `SessionState` GPUI entity holding:

- `connection: Option<Arc<dyn Connection>>` + profile name/host for the
  header and sidebar card,
- `catalog: Option<introspect::Catalog>`,
- `active_tab: Tab` where `Tab::Table { name, rows, columns, page, total }`
  or `Tab::Query { buffer, result, error, elapsed }`,
- loading flags so the UI can render a subtle busy state.

Views observe the entity (`cx.observe`) and re-render on `cx.notify()`.

### 4. Connecting (bootstrap path)

Minimal, in this order of preference:

1. CLI arg / env var: `meerkat postgres://user@host/db` or
   `MEERKAT_DATABASE_URL`. Password may be inline in the URL or in the
   keychain. This unblocks everything else and is fine for this milestone.
2. If time remains: "+ new connection" screen from the design comp
   (`isConnections` section) with profile persistence via `storage` and
   password via `secrets`. Text inputs in GPUI need an input handler —
   see `crates/gpui/examples/input.rs` in the Zed checkout for a minimal
   single-line implementation you can copy (GPL is fine).

On successful connect: run `introspect()` on tokio, populate the sidebar,
open the first table.

### 5. Table view (live)

- Sidebar: replace the `TABLES`/`VIEWS` consts in `shell.rs` with catalog
  data. Show `approx_rows` formatted compactly (18.4k, 1.2m — write a
  small helper, it's in the design).
- Clicking a table issues
  `SELECT * FROM "schema"."table" ORDER BY <pk or first column> LIMIT 500 OFFSET <page*500>`
  (quote identifiers with `"`, escape embedded quotes by doubling; never
  interpolate anything user-typed other than identifiers that came from
  introspection).
- Grid: replace the static row loop with `uniform_list` (see
  `crates/gpui/examples/uniform_list.rs` and `data_table.rs` in the Zed
  checkout) so 500-row pages render smoothly. Keep the existing cell
  styling: 28px rows, hairline bottoms, `.truncate()` on every cell,
  NULL in `text_faint`, `Value::display()` for formatting.
- Column widths: fixed default (e.g. 140px, ID-ish first column 64px) for
  now; measuring/resizing is out of scope.
- Status strip: real `rows X–Y of ~N`, real elapsed ms, prev/next wired to
  the page.

### 6. Query runner

- The editor: a multi-line text buffer. Recommended minimal approach:
  extend the pattern from `crates/gpui/examples/input.rs` to multi-line
  (store a `String` + cursor offset; handle keystrokes, Enter, Backspace,
  arrows; render lines with a line-number gutter per the design comp's
  query screen). Syntax highlighting is OPTIONAL in this milestone — plain
  `text_body`-colored SQL is acceptable; do not pull in tree-sitter yet
  unless everything else is done.
- Run: ⌘⏎ (`cx.on_action` / key context binding — see how the examples
  register key bindings) and the ochre "run" accent button
  (`ui::accent_button`).
- Time the query with `std::time::Instant` around the tokio call.
- Errors: render the sqlx error message in a strip under the editor using
  `error`, `error_surface`, `error_border` tokens (the design's history
  screen shows the error styling).
- Results reuse the same grid component as the table view. Add "RESULT ·
  N rows · M columns · K ms" header per the comp.

## Verification (do all of these)

1. `cargo check --workspace` and `cargo test --workspace` stay green with
   no Postgres server available.
2. With a local Postgres (user may have one; otherwise
   `docker run --rm -e POSTGRES_PASSWORD=pg -p 5432:5432 postgres:16` and
   seed a couple of tables), run
   `MEERKAT_TEST_PG_URL=... cargo test -p db_postgres`.
3. `cargo run -p meerkat -- postgres://...` — verify: sidebar shows real
   tables; clicking a table shows real rows; paging works; a query tab
   runs `select 1 as x, now() as t` and renders both values; a broken
   query shows the error strip and does not crash or hang the UI.
4. The window must never freeze during a slow query (that means the query
   ran on the GPUI thread — it must not).

## Gotchas learned in this repo (read before debugging)

- **Metal Toolchain**: on this machine Xcode 26 needed
  `xcodebuild -downloadComponent MetalToolchain` once; already installed.
  If `gpui_apple` fails with "cannot execute tool 'metal'", that's it.
- **GPUI trait imports**: `.hover()`, `.cursor_pointer()`, `.truncate()`
  come from `InteractiveElement` / `Styled`; `use gpui::prelude::*` in
  view files or import the traits explicitly.
- **Cell wrapping**: every grid/table cell needs `.truncate()` or long
  values wrap and break row heights (already fixed in shell.rs — keep it).
- **First build is slow**: gpui compiles from the pinned Zed git checkout;
  `[profile.dev.package."*"] opt-level = 2` is already set — leave it.
- **sqlx decode**: `try_get::<String>` fails on non-text Postgres types;
  decode by `type_info().name()` match, as db_sqlite does.
- **Screenshots**: `screencapture` fails without the Screen Recording
  permission for the terminal; ask the user for a screenshot instead of
  burning time on it.
- Commit messages end with
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`; commit in
  logical steps (driver, bridge+state, table view, query runner).

## Definition of done

A user with a Postgres URL can browse every table read-only and run ad-hoc
SQL, in the warm-paper UI, without the app ever blocking or crashing on
weird column types. Tests green with and without a database available.
