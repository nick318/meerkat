# Meerkat

A native, cross-platform database viewer: browse schemas, run queries,
read results. Written in Go on [mygo](https://github.com/egoist/mygo),
whose native UI is drawn on the GPU. One Go binary per platform — no
webview, no server.

The first edition was Rust on [GPUI](https://www.gpui.rs); its source is
still in `crates/` as the reference the Go port was made from.

`CLAUDE.md` is the architecture document: it explains why each part is
shaped the way it is. The visual language is the "warm paper" comp at
`docs/design/Meerkat.dc.html`.

## Status

PostgreSQL. A statement you type is sent verbatim, so writing is done in
SQL rather than in the grid.

Launched with no argument it opens the connections screen: saved
connections, each probed live for its relation counts and server
version, plus a form that takes a `postgres://` URL. Passwords go to the
OS keychain, never to the profiles file. A connection is **read-only by
default** and the form's switch is what turns that off; the server
enforces it, through `default_transaction_read_only=on` in the startup
packet, rather than the app reading the user's SQL.

Open a connection and you get:

- **A sidebar** of the real schemas, tables and views — and their
  sequences, routines and types — painted from a cached catalog before
  the connection has even landed. A table folds out into its columns,
  indexes, foreign keys, constraints and triggers. ⌘E puts the keys
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

Requires Go 1.27.3 (mygo needs it; with `GOTOOLCHAIN=local` and an older
default `go`, prefix commands with `GOTOOLCHAIN=go1.27.3`). Everything
builds with `CGO_ENABLED=0`: mygo forbids cgo, which is why SQLite is
`modernc.org/sqlite` and PostgreSQL is `pgx`.

```sh
scripts/dev.sh                                     # connections screen
scripts/dev.sh postgres://user@host:5432/database  # straight to a database
go build ./...
go test -p 4 -parallel 4 ./...
```

`scripts/dev.sh` is `go run ./cmd/meerkat` through
`scripts/run-signed.sh`; plain `go run ./cmd/meerkat` does the same thing,
unsigned. `MEERKAT_DATABASE_URL` works instead of the argument.

On macOS `scripts/run-signed.sh` code-signs the binary before launching
it. Without that, every rebuild is a different app to the keychain and
macOS asks for the login password again. Make a self-signed "Code
Signing" certificate named `Meerkat Dev` in Keychain Access to stop the
prompts; with no such certificate the script says so and runs the binary
anyway.

The PostgreSQL driver tests need a server and skip without one:

```sh
MEERKAT_TEST_PG_URL=postgres://postgres:pg@localhost:5432/postgres \
  go test ./internal/dbpostgres/...
```

## Layout

UI packages may depend on data packages; the reverse is forbidden.
`dbpostgres`, `dbsqlite`, `dbclient`, `introspect`, `storage`, `secrets`,
`query`, `fuzzy` and `settings` must not import mygo.

| Package | Role |
|---|---|
| `cmd/meerkat` | Binary: boot, windows, the connections and shell screens, history, the ⌘K palette |
| `internal/dbclient` | Engine-agnostic `Connection` and `Session` interfaces, `Profile`, `Value`, `QueryResult`, result caps |
| `internal/dbpostgres` | PostgreSQL driver (pgx): introspection, runs, cancels, server timing |
| `internal/dbsqlite` | SQLite driver (modernc). Written and tested; the app does not offer it yet |
| `internal/introspect` | Schema model: schemas, tables, columns, keys, indexes, constraints, triggers, sequences, routines, types |
| `internal/query` | Splitting a buffer into statements, and reading a statement's verb |
| `internal/fuzzy` | One name matcher for the sidebar, the palette, the column search and completion |
| `internal/sqleditor` | Multi-line SQL buffer: motion, undo, colouring, completion, gutter marks |
| `internal/resultsgrid` | Virtualized grid, selection policy, column search |
| `internal/uikit`, `internal/theme` | Component kit (incl. the text field and the overlay scrollbar) and color tokens |
| `internal/storage` | Local SQLite: profiles, probe counts, layout, history, open tabs, cached catalogs |
| `internal/secrets` | OS keychain wrapper for passwords |
| `internal/releasechannel`, `internal/autoupdate` | Which channel a build is, and the self-updater |
| `internal/settings` | User-editable JSON settings. Not wired up yet |

## License

GPL-3.0-or-later.
