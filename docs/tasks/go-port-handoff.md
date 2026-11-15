# Handoff: port Meerkat from Rust/GPUI to Go/mygo

This file is a complete brief for a model that picks the work up cold.
Nothing has been ported yet. The survey, the decisions and the module
setup are done; the port itself starts at "Work plan".

## 1. The task, in the user's words

> Re-write this project in Go, using https://github.com/egoist/mygo
> framework. You can checkout the mygo framework if needed and check the
> examples folder. Try to make this port as mechanical as possible to
> avoid any behavior drifts. Make sure test parity is the same.
> Proceed autonomously until the port is done and app fully functional.
> Proceed in a worktree. At the end build the dev build.

Standing user rules that bind the work (from the user's global
instructions):

- Messages to the user: ASD-STE100 Simplified Technical English, short
  sentences, active voice. (A "caveman" terse-output hook is also active
  in the session; keep replies short.) Files, code and comments are normal prose.
- Use `rg`, `fd`, `ast-grep`; not grep/find.
- The machine has 4 performance cores. Every test run uses at most 4:
  `go test -p 4 -parallel 4 …`. With several agents running at once,
  give each a smaller share (`-p 1 -parallel 2`, build `-p 2`).
- Postgres: local instance only without asking
  (`PGPASSWORD=grespost psql -h localhost -U postgres -d crespect`).
  Any other instance needs explicit consent first.
- Do not commit or push unless asked. The repo's own CLAUDE.md says
  "commit straight to main", but this work is in a worktree branch on the
  user's instruction; leave everything uncommitted unless told otherwise.

## 2. Where things are

| Thing | Path |
|---|---|
| Worktree (work only here) | `/Users/nick318/IdeaProjects/personal/meerkat/.claude/worktrees/port-go-mygo-56db29` |
| Branch | `claude/port-go-mygo-56db29` (base `9f9382a`, version 0.1.6) |
| Rust reference | `crates/` — keep it, do not edit or delete it |
| Behaviour spec | `CLAUDE.md` at the repo root — read it whole; it states every behaviour and why |
| Design comp | `docs/design/Meerkat.dc.html` |
| Fonts | `crates/meerkat/assets/fonts/` (IBM Plex Sans, JetBrains Mono: Regular/Medium/SemiBold) |
| Bundle resources | `crates/meerkat/resources/` (`Info.plist.template`, `meerkat.icns`) |
| Scripts | `scripts/run-signed.sh`, `scripts/bundle-mac.sh` |
| mygo checkout (docs, examples) | session scratchpad, may be gone — re-clone: `git clone https://github.com/egoist/mygo && git checkout 31bf65e6ec29915bec129041d4d1b409dfe60c5c` |
| mygo source as pinned | `GOTOOLCHAIN=go1.27.3 go list -m -f '{{.Dir}}' github.com/egoist/mygo` (module cache, always there) |

### State of the tree right now

Only two new, uncommitted files: `go.mod` and `go.sum` at the repo root
(plus this handoff file).

```
module github.com/nick318/meerkat
go 1.27.3
require: github.com/egoist/mygo v0.9.2-0.20261115180743-31bf65e6ec29
         github.com/jackc/pgx/v5 v5.13.0
         modernc.org/sqlite v1.62.0
         golang.org/x/mod v0.43.0   (semver)
```

All are still marked `// indirect` because no Go file imports them yet.
Run `go mod tidy` once at the very end, not while agents are writing.

### Toolchain

- The default `go` is 1.26.8 and `GOTOOLCHAIN=local`. mygo needs 1.27.3.
  **Prefix every go command with `GOTOOLCHAIN=go1.27.3`** (already
  downloaded and verified).
- mygo forbids cgo. Everything must build with `CGO_ENABLED=0`. That is
  why SQLite is `modernc.org/sqlite` (pure Go) and Postgres is `pgx`.

## 3. What the Rust workspace is

38,953 lines of Rust, 398 tests. No test uses a GPUI test context
(`gpui::test` count is zero): every test is a pure function test, so
all 398 can and must exist in Go.

| Crate | Lines | Tests | Depends on (internal) | GPUI? |
|---|---:|---:|---|---|
| `fuzzy` | 823 | 21 | — | no |
| `query` | 1116 | 28 | — | no |
| `release_channel` | 130 | 5 | — | no |
| `settings` | 52 | 0 | — | no |
| `introspect` | 973 | 13 | — | no |
| `db_client` | 1208 | 20 | introspect | no |
| `secrets` | 28 | 0 | — (keyring) | no |
| `storage` | 1178 | 16 | db_client, introspect (rusqlite) | no |
| `db_sqlite` | 623 | 6 | db_client, introspect (sqlx) | no |
| `db_postgres` | 3886 | 64 | db_client, introspect, secrets (sqlx) | no |
| `auto_update` | 700 | 9 | release_channel | yes (global entity + gpui_tokio) |
| `theme` | 328 | 0 | — | yes (colour types) |
| `ui` | 2825 | 19 | theme | yes |
| `sql_editor` | 5449 | 52 | fuzzy, query, theme, ui | yes |
| `results_grid` | 2254 | 45 | db_client, fuzzy, theme, ui | yes |
| `meerkat` (binary) | 17376 | 100 | everything | yes |
| `workspace`, `schema_tree` | 2 each | 0 | — | empty placeholders, skip |

Largest files: `meerkat/src/shell.rs` 12,169 lines (its `#[cfg(test)]`
module starts at line 10,693; ~428 fn/impl/type items),
`db_postgres/src/lib.rs` 3,886, `sql_editor/src/lib.rs` 3,820,
`meerkat/src/connections.rs` 2,280, `meerkat/src/palette.rs` 1,455,
`results_grid/src/lib.rs` 1,299, `ui/src/text_field.rs` 1,266,
`storage/src/lib.rs` 1,178, `query/src/lib.rs` 1,116.

Recount tests per file before and after with:

```sh
rg -c '#\[(tokio::)?test' crates -g '*.rs'
rg -c '^func Test' internal cmd -g '*_test.go'
```

## 4. What mygo is (verified from its README, AGENTS.md and docs)

- Go desktop framework. A window shows a web page **or native UI drawn
  by mygo on the GPU** (Metal on macOS). Use native UI only: package
  `github.com/egoist/mygo/ui`. No webview, no JS, no Bun.
- **Immediate mode.** The view is a Go function run every frame over
  the app's state:

  ```go
  func (s *shell) view(c *ui.Context) {
      ui.Column(c).Fill().Gap(12).Children(func() {
          ui.Text(c, "…").FontSize(40).Bold()
          if ui.PrimaryButton(c, "Run").Clicked() { s.run() }
      })
  }
  mygo.App.WhenReady(func() {
      mygo.NewWindow(mygo.WindowOptions{Title: "…", Content: ui.View(s.view)})
  })
  mygo.App.Run()
  ```

  GPUI's `render` also rebuilds its element tree each frame, so
  `div().flex().child(..)` maps onto `ui.Row/Column/Box(c)….Children(func(){…})`
  fairly directly.
- **Threading.** The view and all state live on the main thread. From a
  goroutine, apply a result with `win.Update(func(){ … })` (runs on main,
  then draws a frame); `mygo.RunOnMain` runs without a redraw;
  `win.Invalidate()` only redraws; in the view `c.Invalidate()` and
  `c.After(d)` ask for frames. This replaces the `gpui_tokio` bridge:

  ```go
  gen := tab.generation            // bump before the request, as in Rust
  go func() {
      res, err := conn.Execute(ctx, sql)
      win.Update(func() { if tab.generation != gen { return }; /* apply */ })
  }()
  ```

  Keep every generation counter exactly as the Rust has it.
- **Identity / state.** `c.Key(k)` keys the next control; `ui.Local(el,
  "name", init)` is per-element state (closed via `io.Closer` when the
  element leaves); `ui.Handle` is persistent control identity. Never
  store a `ui.Element` in app state (`mygo vet` checks this).
- **Custom widgets.** Bases without a look (`ButtonBase`,
  `TextInputBase`, `PopoverBase`, `DialogBase`, …); custom painting with
  `Draw` and `ui.Painter` (`docs/ui/drawing.md`); a text control with its
  own buffer uses `HandleTextInput` and retained text geometry
  (`docs/ui/text-input-client.md`); every key as it comes via
  `HandleInput`; scoped keys via `Shortcut` on an element
  (`docs/ui/input.md`); overlays via `AttachTo`, `PressedOutside`,
  `OverlayShortcut`, `Modal` (`docs/ui/overlays.md`).
- **Windows.** `TitleBarStyle: mygo.TitleBarHidden` + `c.TitleBar()`
  (room the traffic lights take) + `.DragWindow()` on elements —
  this is the replacement for `appears_transparent`,
  `app_owns_titlebar_drag` and `TitleDrag::claim` (`docs/ui/windows.md`).
- **Lists.** Virtualized lists, tables, scroll views with scroll bars
  (`docs/ui/list.md`, `scroll.md`, `table.md`).
- **Testing.** `ui.NewTester(view, w, h)` runs a view headless: click,
  type, key presses, a controllable clock (`tt.Advance`), `tt.Image()`
  for a PNG of the frame. Use it to check the UI without a window, and
  save `tt.Image()` to a file and look at it to verify layout.
- **Desktop APIs** present in package `mygo`: secrets
  (`mygo` secrets module: `Service(name)`, `Set/Get/Delete` — see
  `secrets.go`), clipboard, menus, dialogs, updater, single instance,
  window state.
- **CLI.** `go run github.com/egoist/mygo/cmd/mygo dev|build <dir>`;
  `mygo build` makes a `.app` + `.dmg`, signs and notarizes
  (`docs/cli.md`, `docs/distribution.md`). `examples/counter-native` is
  the minimal native app with a view test; `examples/gallery`,
  `code-editor`, `text-buffer`, `text-input`, `frameless`, `native` are
  the ones worth reading.

**Not yet verified — check these in the mygo docs/source before the UI
work is designed around them:**

1. Loading bundled TTF fonts and using two families (`docs/ui/text.md` § Fonts).
2. Fixed-advance text measurement for the mono font (the SQL editor and
   the grid measure by a fixed advance).
3. How keystroke scoping resolves between nested elements ("deepest
   context wins" in GPUI: ⌘⏎ runs in the shell, opens a row in the
   palette; ⌘F is bound three times; ⌘W bound to root and shell).
4. Several windows from one process, each with its own state (⌘N), and
   the close-request hook that can answer "no" (`on_window_should_close`),
   plus quit interception (⌘Q walk) and restart.
5. Window-level pointer capture during a drag (GPUI `DragSurface`
   pattern: listeners on the window, not the hitbox).
6. A virtualized list that also scrolls sideways, with a programmatic
   scroll offset (`GridState::reveal`, autoscroll per frame).
7. Wavy underline and per-range highlight in painted text.
8. `reduce_motion`, and an animation phase-locked to an app clock
   (`repeat_synced`) — may be a hand-rolled `c.Now()` + `c.Invalidate()`.

## 5. Decisions already made

- **Layout.** `go.mod` at the repo root. One Go package per crate under
  `internal/`, names: `fuzzy query introspect dbclient dbpostgres dbsqlite
  storage secrets settings releasechannel autoupdate theme uikit sqleditor
  resultsgrid` (`uikit` = crate `ui`, renamed so it does not collide with
  mygo's `ui`). The binary is `cmd/meerkat`. One Go file per Rust file,
  same base name; `lib.rs` → `<package>.go`. `shell.rs` must be split by
  topic into `shell_*.go` files in package `main`.
- **The Rust stays** in `crates/` as the reference until the user says to
  remove it. Tell the user at the end; removal is one command.
- **Drivers.** sqlx-postgres → `pgx/v5` (`pgxpool`, `pgconn`).
  rusqlite and sqlx-sqlite → `modernc.org/sqlite` through `database/sql`.
  keyring → mygo's secrets module if its service/account naming can read
  the entries the Rust `keyring` crate wrote (service and user strings are
  in `crates/secrets/src/lib.rs`); otherwise a pure-Go keychain call. An
  existing user must not lose saved passwords.
- **Local store compatibility.** The Go build must open the SQLite file
  the Rust build wrote (same path, same schema, same `migrate` steps,
  same JSON shapes in `catalog_cache`, `open_tabs`, `ui_state`).
- **Channel baked at build.** Rust used `option_env!("MEERKAT_CHANNEL")`
  and `MEERKAT_COMMIT_SHA`. Go: `-ldflags "-X …/releasechannel.channel=dev
  -X …/releasechannel.commitSHA=<sha>"`; no value = `local`, and `local`
  cannot be asked for by name. Version constant comes from one place
  (was the workspace `Cargo.toml`, 0.1.6).
- **Async.** tokio task → goroutine + `context.Context`. `Arc<dyn
  Connection>` / `Arc<dyn Session>` → interfaces. The Rust `impl Drop`
  that detaches a pooled connection off-runtime has no Go analogue; port
  the two "drop a session on a plain thread" tests as the closest thing
  (abandon a session without `Close` from another goroutine; nothing
  panics, nothing leaks into the pool) and say so in the report.

## 6. Port rules (give these to every sub-agent verbatim)

**Goal: a MECHANICAL port.** Same types, same functions, same constants,
same algorithms, same strings, same edge cases, in the same order in the
file. No redesign, no improvements. When Go forces a different shape,
take the closest one and keep the Rust name.

Translation:

- `Option<T>` → `*T` for structs, `(T, bool)` for small returned values;
  consistent inside a package. `Result<T,E>` → `(T, error)`.
- Enum without data → `type X int` + `iota` consts (+ `String()` where
  Rust's Display/Debug reaches output). Enum with data → a small
  interface with one struct per variant, or a struct with a `Kind` tag.
- Byte offsets stay byte offsets (Go strings are UTF-8 bytes like `str`).
  `char` → `rune`. Match Rust's character classes exactly:
  `char::is_alphanumeric` is Alphabetic || Numeric, `is_whitespace` is
  Unicode White_Space, `to_ascii_lowercase` touches ASCII only.
- `BTreeSet`/`BTreeMap` iterate sorted: keep sorted order wherever order
  is observed. `HashMap` order was unspecified: do not start depending
  on Go's map order either.
- serde: keep exact JSON field names, enum tags and shapes;
  `#[serde(default)]` → plain field. Cached JSON from the Rust build
  must parse.
- `pub fn foo_bar` → `FooBar`; private → `fooBar`. Constants keep value
  and meaning (`MAX_BYTES` → `MaxBytes`).
- `//!` and `///` docs and inline comments come across with their
  wording; they carry the reasons.
- Integer widths matter: `u32`/`usize`/`i64` arithmetic, saturating and
  wrapping ops, and `as` casts must behave the same.

Tests (parity is mandatory):

- Every `#[test]` / `#[tokio::test]` → one Go test, same inputs, same
  assertions, in `<file>_test.go` of the same package (internal test
  package, so private functions are reachable). Name = CamelCase of the
  Rust name: `pages_order_by_the_primary_key` →
  `TestPagesOrderByThePrimaryKey`.
- Never drop, merge, weaken or skip a test. A failing test means the
  port is wrong; fix the port, never the expected value.
- Tests that need a server skip themselves as in Rust
  (`MEERKAT_TEST_PG_URL`). Run the Postgres ones for real against the
  local instance:
  `MEERKAT_TEST_PG_URL=postgres://postgres:grespost@localhost:5432/postgres`
  (the Rust docs use `postgres:pg@…/postgres`; the local container's
  password is `grespost` — confirm which database the tests may write in
  before running; they create temp objects).
- Each agent ends with a table: Rust tests per file vs Go tests per file.

Mechanics:

- `GOTOOLCHAIN=go1.27.3` on every command; `CGO_ENABLED=0` must build.
- Build/test only your own packages (`./internal/<pkg>/...`), never
  `./...` while others are mid-write. `gofmt -l` prints nothing; `go vet`
  clean.
- Do not run `go mod tidy`. Touch only your directories. Do not commit.

## 7. Work plan

Dependencies decide the order. Agents in one wave touch disjoint
directories and can run in parallel (3–4 at a time on this machine).

### Wave 1 — no internal dependencies

| Agent | Packages | Rust lines | Tests |
|---|---|---:|---:|
| A | `fuzzy`, `query`, `releasechannel`, `settings` | 2,121 | 54 |
| B | `introspect` (incl. `diff.rs`), `dbclient` (incl. `connect.rs`) | 2,181 | 33 |
| U | **UI foundation**: `theme`, `uikit` (`lib.rs` title bar, `text_field.rs`, `scrollbar.rs`, `find_bar.rs`, `icon.rs`, `blink.rs`) **and a GPUI→mygo mapping sheet** | 3,153 | 19 |

Agent U is the critical path. Its extra deliverables:

1. Resolve the eight unverified points in §4, each with a file/line in
   the mygo source or docs, or a working snippet.
2. Write `docs/tasks/gpui-to-mygo.md`: for each GPUI construct the app
   uses (`div` styling chain, `uniform_list`, `deferred`, `canvas`,
   custom `Element`, `EntityInputHandler`, `FocusHandle`, key contexts
   and actions, `on_mouse_down/move/up`, `occlude`, `with_animation`,
   `cx.notify`, `cx.spawn`, `Entity`/`emit`/`subscribe`, clipboard,
   `ScrollHandle`), the mygo construct that replaces it, with a 3–10
   line example. Every later UI agent reads this instead of rediscovering it.
3. A tiny `cmd/meerkat-spike` (delete it later) that opens one window
   with the hidden title bar, both fonts, a `uikit.TextField`, and a
   custom-painted text block; and a `ui.NewTester` test that renders it
   to a PNG so the look can be checked without a desktop.

`B`'s report must list the exported API of `dbclient` and `introspect`
(the `Connection` and `Session` interfaces, `Profile`, `Value`,
`QueryResult`, `RowChange`, `RowSink`, `Limits`, `ConnectFailure`,
`ConnectEvidence`, `ConnectError`, `Refusal`, `TxMode`, `Wire`,
`ServerTiming`, `RunId`, `MAX_SESSIONS`, the `Catalog` tree, `Delta`),
because Wave 2 codes against it.

### Wave 2 — needs Wave 1

| Agent | Packages | Rust lines | Tests | Notes |
|---|---|---:|---:|---|
| C | `storage`, `secrets` | 1,206 | 16 | WAL + `busy_timeout`; `migrate` adds columns one at a time; 500-run history cap; `TAB_LIMIT` 100 |
| D | `dbpostgres` | 3,886 | 64 | the hardest data crate — see below |
| E | `dbsqlite`, `autoupdate` | 1,323 | 15 | `autoupdate`: keep `update_available`, `announces`, manifest, sha256 check, `rsync -a --delete` install; the GPUI global entity becomes a plain struct with a mutex and a change callback |
| F | `sqleditor` | 5,449 | 52 | pure parts first (`edit.rs`, `motion.rs`, `display.rs`, `highlight.rs`, `completion.rs`), then the painted element on `HandleTextInput` + `Draw` |
| G | `resultsgrid` | 2,254 | 45 | `selection.rs`, `columns.rs`, `find.rs` pure; then the grid view, `DragSurface`, autoscroll |

Points the `dbpostgres` agent must not lose (all are in CLAUDE.md):

- Two pools: `APP_CONNECTIONS` 3 and `SESSION_CONNECTIONS` 8 (lazy),
  same connect options. Read-only is the startup option
  `default_transaction_read_only=on`, never a `SET`.
- `reach`: a bare TCP connect under `CONNECT_TIMEOUT` before the pool.
- `classify_connect` evidence filled from the driver error (pgx:
  `*pgconn.PgError` SQLSTATE, `*pgconn.ConnectError`, `net.OpError`,
  DNS errors, TLS errors, timeouts).
- `prepare_run`: one round trip for `pg_backend_pid()` + conditional
  `set_config('statement_timeout', …)` when `pg_settings.source =
  'default'`; `STATEMENT_TIMEOUT` 30 s.
- `collect_capped`: stream rows, cap by bytes (`MAX_BYTES` 256 MB,
  `MAX_CELL_BYTES` 1 MB), on cap send `pg_cancel_backend` on the app
  pool and drain; keep the first row always; rows-affected from the
  command tag (`pgconn.CommandTag.RowsAffected()`); describe columns
  only when the count is zero.
- Runs are **prepared** (binary results). Decode by type name; unknown
  types never error: text placeholder `<type_name>`, with the
  hand-written binary readers for `xid`, `xid8`, `tsvector`. With pgx,
  ask for raw values (`rows.RawValues()` + `FieldDescriptions()`) and
  port the decode table by OID/type name so output strings match what
  the Rust build prints (numeric, timestamps, uuid, json, arrays,
  bytea, intervals). The 64 tests pin most of this.
- `check` = prepare and throw away (`conn.Prepare` then deallocate),
  error position is a one-based character index → `byte_offset`.
- `relation_exists` via `to_regclass`. `in_transaction` via
  `pg_stat_activity` on the app pool. `ServerTiming` from
  `pg_stat_statements` joined on `query_id`, `between` semantics table.
- Introspection: one `read_catalog` used by both `Connection` and
  `Session`; best-effort object reads; version gates on
  `server_version_num`; extension-owned objects left out.
- Session close sends `ROLLBACK`; a connection that fails to answer is
  closed, not pooled.

### Wave 3 — the app (`cmd/meerkat`), needs Waves 1–2 and the mapping sheet

`meerkat` is 17,376 lines and 100 tests. Split it so no agent owns more
than ~4k lines of Rust, and so two agents never write the same file:

| Agent | Rust source | Go files |
|---|---|---|
| H | `main.rs`, `root.rs`, `env.rs`, `sql.rs`, `update.rs`, `history.rs` | `main.go root.go env.go sql.go update.go history.go` |
| I | `connections.rs` | `connections.go` |
| J | `palette.rs` | `palette.go` |
| K1 | `shell.rs` — types, state, pure policy functions, and the whole `#[cfg(test)]` module (lines 10,693–end) | `shell.go`, `shell_types.go`, `shell_policy.go`, `shell_test.go` |
| K2 | `shell.rs` — sessions, runs, stop/terminate, transactions, checks, schema report, timing, close guard | `shell_run.go`, `shell_tx.go`, `shell_check.go`, `shell_schema.go`, `shell_close.go` |
| K3 | `shell.rs` — rendering: title bar, sidebar, tabs, toolbar, strips, grid pane, overlays (peek card, column find, confirm dialog), dividers | `shell_view_*.go` |

Do K1 first and alone: it fixes the `Shell` struct and its field names,
which K2, K3, H, I and J all code against. Before starting K1, map
`shell.rs` with
`rg -n '^(pub )?(async )?fn |^impl |^(pub )?(struct|enum) |^    (pub )?(async )?fn ' crates/meerkat/src/shell.rs`
and assign every item to one of the Go files above, so the split is
explicit in the briefs.

### Wave 4 — integrate

1. `GOTOOLCHAIN=go1.27.3 go mod tidy`, `go build ./...`, `go vet ./...`,
   `gofmt -l .` (empty), `CGO_ENABLED=0 go build ./cmd/meerkat`.
2. `go test -p 4 -parallel 4 ./...`, then again with
   `MEERKAT_TEST_PG_URL` set. Compare test counts per file with the
   Rust table; the totals must be 398 (plus any Tester-based UI tests
   added on top, which are extra, not replacements).
3. Run the app for real: `go run ./cmd/meerkat` (connections screen) and
   `go run ./cmd/meerkat postgres://postgres:grespost@localhost:5432/crespect`
   (`MEERKAT_DATABASE_URL` must work too). Walk the list in §8.
4. Port the scripts: `cargo dev` → a `Makefile`/script target that
   builds, signs via `scripts/run-signed.sh`, and runs;
   `scripts/bundle-mac.sh dev|public` → either `mygo build` with a
   config, or the existing script with the `cargo build` line replaced by
   `go build -trimpath -tags mygo_noinspector -ldflags "-s -w -X
   github.com/egoist/mygo.production=1 -X …releasechannel…"`. Keep the
   outputs identical: `target/dist/Meerkat Dev.app`, `.tar.gz`, `.dmg`,
   `.sha256` beside each archive, bundle id `com.nick318.meerkat-dev`,
   the signing ladder (`MEERKAT_SIGN_IDENTITY` → `Meerkat Dev`
   certificate → ad-hoc). The updater downloads the `.tar.gz` and rsyncs
   the bundle contents, so the bundle's inner layout
   (`Contents/MacOS/<binary name>`, `Info.plist` keys, icon) must stay
   what `Info.plist.template` and the Rust build produced — a dev
   install of the Rust app must be able to update onto the Go app.
5. **Build the dev build** (the user's last instruction):
   `scripts/bundle-mac.sh dev`. Do not notarize, do not publish, do not
   touch the `updates` branch, do not tag — those are release steps the
   user did not ask for.
6. Update `CLAUDE.md` and `README.md` commands and the crate table to the
   Go layout (keep the behaviour sections; they are still the spec).

## 8. Functional checklist for the running app

Each line is a behaviour the Rust app has; the section names are the
CLAUDE.md headings that define it.

- Connections screen: list, form, read-only default on, MODE and env
  columns, version line bottom-right, ⌘N new window (cascade), ⌘W closes
  the window.
- Open a connection: empty query tab at once; cached catalog paints the
  sidebar before the connect lands; connect strip after `CONNECT_ARM`;
  failure shows class headline, detail with copy, hint, Retry, Back.
- Sidebar: schema → sections → relations → columns/folders; lazy parts;
  filter line with ⌘E, ranking, ↑↓ ring, ⏎ open, ⇥ complete; click on a
  relation opens a named query tab with `SELECT * … LIMIT 500;` and runs it.
- Panes: two dividers, rails, double click resets, sizes saved in `ui_state`.
- Tabs: restore per connection (waiting, never re-run), ⌃⇥ / ⌃⇧⇥ wrap,
  ⌘T, ⌘W, last tab closes the window.
- SQL editor: Zed key set, virtualized rows, tabs as columns, completion
  panel, ⌘F strip, statement gutter marks + rail, DDL teal family,
  per-statement statistics column, syntax squiggles + `syntax_strip`.
- Run: ⌘⏎ run, ⌘. stop then terminate, `RUN_ARM` 420 ms, breathing fill,
  timer pill after 1 s, CANCELLED vs error, result line RESULT/DONE,
  rows-affected wording, truncated mark, timing split
  (`queried in … · server … · lag …`, `~` when not exact).
- Transactions: per-tab Auto/Manual, bar with ⌘S commit / ⇧⌘R rollback,
  refused while a run is out, auto chip faint while open.
- Sessions: one per tab from first run, cap 8, idle sweep 10 min,
  `session ended · a run opens a new one`.
- Schema report: SCHEMA CHANGED panel, held inside a transaction,
  `reveal in schema tree`.
- Grid: cell/range/pick selection, drag with autoscroll, gutter ticks,
  ⌘C as CSV, ⌘J column find, ⌘F result find (`3 of 12`, `10,000+`),
  ⏎ / double click peek card with text marking and ⌘C.
- Palette ⌘K: fuzzy names, dotted paths, statement substring, ⇥
  completion with ghost hint, ⏎ focus existing / ⌘⏎ new tab.
- History tab: day headings, click opens in a new tab, never re-runs.
- Close guard: all six ways out, two-line dialog (run / transaction),
  ⌘Q walks windows, "stay" cancels the whole quit.
- Read-only padlock badge, env badge, mode mark colours.
- Update toast (dismissable, in-memory), version line narration;
  `local` builds never poll.
- Theme: every colour from `theme` tokens; both fonts; text ≥ 4.5:1.

## 9. Risks, ranked

1. **The custom SQL editor on mygo.** 3,820 lines of GPUI `Element` +
   `EntityInputHandler`. mygo has a ready code editor (`ui/editor`), but
   swapping it in is a redesign and would drift from the key set, the
   gutter marks, the rail, the wash and the statistics column. Port the
   buffer and policy verbatim and repaint with `Draw` +
   `HandleTextInput`. Fall back to `ui/editor` only if a required
   primitive is truly missing, and then say exactly what drifted.
2. **Key scoping.** GPUI's deepest-context-wins dispatch is relied on in
   many places. If mygo resolves shortcuts differently, build one small
   dispatcher in `uikit` that reproduces "deepest focused context first"
   and route every binding through it, rather than patching call sites.
3. **pgx vs sqlx value rendering.** Output strings for every Postgres
   type must match. The driver tests cover it; run them against a real
   server, not only the skip path.
4. **Size of `shell.rs`.** Do not hand it to one agent. Fix the struct
   first (K1), then fan out.
5. **Sub-agent drift.** Agents "improve" things. Review each report's
   deviation list, and spot-check one function and one test per package
   against the Rust.
6. **Keychain and store compatibility** with an existing install (§5).
7. **Immediate mode vs GPUI animations** (`with_animation`,
   `repeat_synced`, the settle keyed on `landed`): reproduce with
   `c.Now()` and explicit phase math; honour reduce-motion.

## 10. Definition of done

- `CGO_ENABLED=0 GOTOOLCHAIN=go1.27.3 go build ./...` clean; `go vet`
  clean; `gofmt -l .` empty.
- `go test ./...` green without a server and green with
  `MEERKAT_TEST_PG_URL`; 398 Rust tests have 398 Go twins, table in the
  final report, file by file.
- The app runs, and the §8 checklist was walked on the local Postgres;
  anything not verified is named as not verified.
- `scripts/bundle-mac.sh dev` produced `target/dist/Meerkat Dev.app`
  (+ `.tar.gz`, `.dmg`, `.sha256`), and the app from that bundle starts.
- Final report to the user: what was ported, the parity table, every
  deviation from the Rust behaviour with its reason, what is not
  verified, and that `crates/` (Rust) is still in the tree awaiting their
  word to remove it. Nothing committed unless they ask.
