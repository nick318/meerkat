# Meerkat

A native, cross-platform database viewer: browse schemas, run queries,
read results. Built on [GPUI](https://www.gpui.rs), the UI framework
behind [Zed](https://github.com/zed-industries/zed). One Rust binary per
platform — no webview, no server.

`CLAUDE.md` is the architecture document: it explains why each part is
shaped the way it is. The visual language is the "warm paper" comp at
`docs/design/Meerkat.dc.html`.

## Status

Read-only PostgreSQL. In-place editing and MySQL are Phase 2.

Launched with no argument it opens the connections screen: saved
connections, each probed live for its relation counts and server
version, plus a form that takes a `postgres://` URL. Passwords go to the
OS keychain, never to the profiles file. Sessions are read-only by
default, enforced by the server through
`default_transaction_read_only=on` in the startup packet rather than by
reading the user's SQL.

Open a connection and you get:

- **A sidebar** of the real schemas, tables and views, painted from a
  cached catalog before the connection has even landed. ⌘E puts the keys
  on its filter line; ↑↓ walk the matches and ⏎ opens one.
- **Query tabs.** ⌘⏎ runs the buffer, ⌘. stops it. Each tab pins its own
  connection from its first run, so `BEGIN`, `SET` and temp tables mean
  what they read as. Each statement of a buffer is marked in the gutter
  as it runs.
- **Transactions the user ends.** A tab is `Auto` or `Manual`; a typed
  `BEGIN` is honoured either way. ⌘S commits, ⇧⌘R rolls back, and a bar
  over the result says what is open.
- **A results grid** with cell and row selection, ⌘C to CSV, ⌘J to
  search the column names, and ⏎ to read a long value whole.
- **⌘K**, a palette over the catalog and the run history.
- **Query history**, per connection, in a tab of its own.
- **The tabs you left**, reopened per connection, waiting rather than
  re-run.
- **⌘N** for another window, with its own session and tab strip.

Runs report where the time went — `queried in 340 ms · server 12.4 ms ·
lag 328 ms` — reading `pg_stat_statements` where the server offers it.
Results are capped by **memory**, not by row count, and nothing rewrites
the user's SQL to do it.

## Build

Requires the latest stable Rust. On macOS you also need Xcode and its
command line tools (GPUI renders with Metal).

```sh
cargo dev                                          # connections screen
cargo dev postgres://user@host:5432/database       # straight to a database
cargo check --workspace
cargo test --workspace
```

`cargo dev` is an alias for `cargo run -p meerkat --`, in
`.cargo/config.toml`; plain `cargo run -p meerkat` does the same thing.
`MEERKAT_DATABASE_URL` works instead of the argument.

On macOS cargo's `runner` is `scripts/run-signed.sh`, which code-signs
the binary before launching it. Without that, every rebuild is a
different app to the keychain and macOS asks for the login password
again. Make a self-signed "Code Signing" certificate named `Meerkat Dev`
in Keychain Access to stop the prompts; with no such certificate the
script says so and runs the binary anyway. Test binaries pass through
untouched.

The PostgreSQL driver tests need a server and skip without one:

```sh
MEERKAT_TEST_PG_URL=postgres://postgres:pg@localhost:5432/postgres \
  cargo test -p db_postgres
```

The first build compiles GPUI from the pinned Zed revision and takes a
while. `gpui`, `gpui_platform` and `gpui_tokio` are pinned by git rev in
the workspace `Cargo.toml`; bump all three together and deliberately —
GPUI is pre-1.0 and breaks APIs.

## Workspace layout

UI crates may depend on data crates; the reverse is forbidden.
`db_postgres`, `db_sqlite`, `db_client`, `introspect`, `storage`,
`secrets`, `query`, `fuzzy` and `settings` must not know about `gpui`.

| Crate | Role |
|---|---|
| `meerkat` | Binary: boot, windows, the connections and shell screens, history, the ⌘K palette |
| `db_client` | Engine-agnostic `Connection` and `Session` traits, `Profile`, `Value`, `QueryResult`, result caps |
| `db_postgres` | PostgreSQL driver (sqlx): introspection, runs, cancels, server timing |
| `db_sqlite` | SQLite driver (sqlx). Written and tested; the app does not offer it yet |
| `introspect` | Schema model: catalog, schemas, tables, columns, keys |
| `query` | Splitting a buffer into statements, and reading a statement's verb |
| `fuzzy` | One name matcher for the sidebar, the palette, the column search and completion |
| `sql_editor` | Multi-line SQL buffer: motion, undo, colouring, completion, gutter marks |
| `results_grid` | Virtualized grid, selection policy, column search |
| `ui`, `theme` | Component kit (incl. the text field and the overlay scrollbar) and color tokens |
| `storage` | Local SQLite: profiles, probe counts, layout, history, open tabs, cached catalogs |
| `secrets` | OS keychain wrapper for passwords |
| `settings` | User-editable JSON settings. Not wired up yet |
| `workspace`, `schema_tree` | Empty placeholders |

## License

GPL-3.0-or-later. GPUI itself is Apache-2.0.
