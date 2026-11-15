# Package-main API already ported (cmd/meerkat)

Naming rule in package main: a generic name takes its Rust module as a
prefix (`palette::build` → `PaletteBuild`); names that already say their
module keep the Rust name. connections.go uses lowercase names for its
module-level functions.

## palette.go
- `PaletteAction` (`PaletteToggle`, `PaletteSelectPrev`, `PaletteSelectNext`,
  `PaletteOpenInNewTab`), `PaletteKeyContext = "Palette"`.
- `PaletteShellKeyBindings()` (cmd-k, dispatch in the Shell context) and
  `PaletteKeyBindings()` (up/down/ctrl-p/ctrl-n/cmd-enter, on the palette element).
- Geometry: `PaletteWidth`, `PaletteListHeight`, `PaletteTopMargin`,
  `PaletteInputFontSize`, `PaletteRowHeight`.
- `PaletteScope` (`PaletteScopeAll/Tables/History`), `PaletteScopes`,
  `.Label()`, `.Prefix() (string, bool)`; `PaletteParse(query, chip) (PaletteScope, string)`.
- `PalettePick{Kind PalettePickKind; Schema, Table, Statement string}` with
  `PalettePickTable` / `PalettePickQuery`; `OnPick func(pick PalettePick, newTab bool)`.
- `PaletteRow` interface (`PaletteHeader{Label, Count}`, `*PaletteItem`) with
  `Pick() (PalettePick, bool)`; `PaletteResults{Rows; Matches}`.
- `PaletteBuild(catalog *introspect.Catalog, runs []storage.QueryRun, scope, needle string, today time.Time) PaletteResults`.
- `PaletteCompletion(rows, selected, needle) (string, bool)`, `PaletteGhost(needle, completed) string`,
  `PaletteCompletePath(parts []string, needle) (string, bool)`.
- `PalettePathRank(parts, needle) (PaletteRank, bool)`; `PaletteRank{Out; Cost int32; Len}.Less()` —
  the sidebar sort must use `Less`.
- Views: `PaletteRowView(c, ix, row, selected, onPick, colors) ui.Element`,
  `PaletteScopeChip(c, scope, active, colors) ui.Element` (caller reads `.Clicked()`).

## history.go
- `HistoryRowHeight`, `OnOpenRun func(statement string)`.
- `HistoryRow` interface (`HistoryDay{Label}`, `*RunRow`), `HistoryFlatten(runs, today) []HistoryRow`,
  `HistoryToday()`.
- `HistoryRowView(c, ix, row, open, colors)`, `HistoryFilterChip(c, id, label, active, colors)`.
- Date helpers: `naiveDateOf`, `naiveNumDays`, `localTimestamp`.

## sql.go
`PageSize = 500`, `QuoteIdent`, `PageQuery(schema, table *introspect.Table, page)`,
`BrowseQuery(schema, table)`.

## env.go
`Env` (`EnvProd/Staging/Dev`, `EnvAll`), `.AsStr()`, `.Ring/.Inner/.Surface/.Text(colors)`,
`.FormNote()`, `parseEnvOpt(*string) (Env, bool)`.

## update.go
`toast(c, colors, bottom float32) (ui.Element, bool)`, `footSummary(c, colors)`.

## Wiring main.go MUST do at start-up
- `updateRestart = restartToUpdate` (the toast's "restart now").
- `dbpostgres.KeychainPassword = secrets.GetPassword` (or saved profiles cannot connect).
- `uikit.RegisterFonts()`, `theme.SetGlobal(theme.WarmPaper())`, `autoupdate.Init(onChange)`
  with onChange invalidating every window.
- Delete `zz_temp_main.go` when main.go lands.

## connections.go
- `NewConnections(window *mygo.Window) *Connections` (opens `storage.OpenDefault()`; replies go back through `window.Update`).
- Events: `ConnectionsEvent` interface, one variant `ConnectionsEventOpen{Profile dbclient.Profile}`;
  `(*Connections).Subscribe(func(*Connections, ConnectionsEvent))`, delivered synchronously (maybe mid-build:
  Root should defer a screen swap to after the frame, e.g. via `win.Update`/`c.After`).
- `(*Connections).View(c) ui.Element` (root is a filled `ui.Column`), `(*Connections).FocusHandle() *ui.Handle`.
- Keys: the screen dispatches its own table (`connectionsKeyBindings`); main binds nothing for it.
- `unixNow() int64` (Rust `unix_now`) is shared with the shell.
