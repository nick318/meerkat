# Meerkat

A native, cross-platform database viewer: browse schemas, run queries,
edit rows in place. Built on [GPUI](https://www.gpui.rs), the UI framework
behind [Zed](https://github.com/zed-industries/zed). One Rust binary per
platform — no webview, no server.

Architecture blueprint: see the pinned artifact / `docs/` (Zed-style crate
workspace, GPUI frontend, sqlx drivers behind a `Connection` trait).

## Status

Bootstrap skeleton. The app opens the main window with placeholder panes.
The SQLite driver can introspect and query; in-place editing is Phase 2.

## Build

Requires the latest stable Rust. On macOS you also need Xcode and its
command line tools (GPUI renders with Metal).

```sh
cargo run -p meerkat     # open the app window
cargo test               # driver + storage tests
```

The first build compiles GPUI from the pinned Zed revision and takes a
while. `gpui`/`gpui_platform` are pinned by git rev in the workspace
`Cargo.toml`; bump the rev deliberately — GPUI is pre-1.0 and breaks APIs.

## Workspace layout

| Crate | Role |
|---|---|
| `meerkat` | Binary: app entry, window bootstrap |
| `workspace` | Panes, docks, tabs, keymap (placeholder) |
| `ui`, `theme` | Component kit and color tokens on GPUI |
| `settings` | User-editable JSON settings |
| `db_client` | Engine-agnostic `Connection` trait, values, changesets |
| `db_sqlite` | SQLite driver (sqlx) — introspect + query work |
| `db_postgres` | PostgreSQL driver (placeholder) |
| `introspect` | Schema model: schemas, tables, columns, keys |
| `query` | Query execution service (placeholder) |
| `sql_editor` | Editor pane (placeholder) |
| `results_grid` | Virtualized grid, cell editing (placeholder) |
| `schema_tree` | Sidebar tree (placeholder) |
| `storage` | Local SQLite: profiles, layout, history |
| `secrets` | OS keychain wrapper for passwords |

## License

GPL-3.0-or-later. GPUI itself is Apache-2.0.
