// Root view: breadcrumb bar | sidebar + tab pane | status strip.
//
// The shell owns the session: the connection, the catalog it introspected,
// and the open tabs. Every database call goes to the tokio runtime through
// `gpui_tokio` and comes back as an update on this entity, so the window
// keeps painting while a query runs.
//
// Stale replies are dropped by generation: each tab counts its requests,
// and a reply that does not carry the tab's current generation is thrown
// away. Without that, a slow first page would overwrite a fast second one.
//
// Port notes: `crates/meerkat/src/shell.rs` (the Rust, at tag v0.1.8) is
// split by topic over
// `shell*.go` (see docs/tasks/shell-split.md). This file holds every type,
// constant and key table; shell.go the constructor and the tab restore;
// shell_policy.go the pure functions; the rest belong to K2 (runs,
// sessions, transactions, checks, the schema report, the close guard) and
// K3 (interaction and rendering).
//
// # Go shape decisions
//
// Everything the other shell files code against follows these, so they
// are written down once:
//
//   - The window and `cx` parameters are gone. GPUI handed every method a
//     `&mut Window` and a `&mut Context<Self>`; the Go shell keeps its
//     `*mygo.Window` in Shell.window and its methods take neither. Rust
//     `fn new_query(&mut self, window, cx)` is `func (s *Shell)
//     newQuery()`. `cx.notify()` after a change made from input is
//     nothing (a frame follows the input); after a change made from a
//     goroutine or a timer it is the `s.update(fn)` that carried it.
//   - The async bridge: `gpui_tokio::Tokio::spawn` + `cx.spawn` is a
//     goroutine that hands its result back with `s.update(func(){…})`
//     (shell.go), which is `win.Update` and is dropped once the shell has
//     been let go (Shell.released, set by `release()` — GPUI's
//     `WeakEntity::update` failing). A goroutine that must survive a
//     panic recovers it and reports it as `flatten`'s "interrupted"
//     (shell_run.go), the JoinError of the Rust.
//   - Generation counters are kept exactly: `uint64` fields, bumped on the
//     main thread before the request leaves, captured in a local, and
//     compared inside the `s.update` closure. `connectGeneration` counts
//     connects, `TableTab.generation`/`QueryTab.generation` count runs and
//     pages, `QueryTab.checked` counts syntax checks.
//   - `Tab` is an interface with three pointer variants, `*TableTab`,
//     `*QueryTab` and `*HistoryTab`; `match tab { Tab::Query(tab) => … }`
//     is `switch tab := tab.(type) { case *QueryTab: … }`, and
//     `matches!(tab, Tab::History(_))` is `_, ok := tab.(*HistoryTab)`.
//     The variants are pointers because each holds a `ui.ListState` or a
//     `GridState`, which must not be copied, and because a tab is changed
//     in place. `impl Tab` is the `ID`, `Title`, `Selection` and `Marked`
//     methods (exported only because the variants have fields of those
//     names).
//   - `Run` is a tagged struct: `Run{kind: RunRunning, flight: live}`,
//     `Run{kind: RunCancelled, elapsed: ms}`, the zero value is
//     `Run::Idle`. `matches!(tab.run, Run::Idle)` is `tab.run.kind ==
//     RunIdle`.
//   - `Close` is a comparable struct, `Close{Kind: CloseKindTab, Tab: 7}`;
//     the other three carry only their kind. `==` is the derived
//     `PartialEq`.
//   - `Target` and `Status` are interfaces with one struct per variant:
//     `TargetUrl{Url}`, `TargetProfile{Profile}`; `StatusConnecting{To}`,
//     `StatusConnected{}`, `StatusFailed{Error}`.
//   - `ShellEvent` is an int enum; `impl EventEmitter<ShellEvent>` is
//     `Shell.Subscribe(fn)` + `s.emit(ev)`, listeners called synchronously
//     on the main thread (the mapping sheet §14).
//   - `Option<T>` is `*T` for a field or a struct, `(T, bool)` for a small
//     value a function returns. `Rc<T>` and `Arc<T>` are plain pointers,
//     `Arc<dyn Connection>`/`Arc<dyn Session>` the interfaces themselves
//     (nil is `None`). `Entity<T>` is `*T`. `HashSet<SharedString>` is
//     `map[string]struct{}`. `Instant` is `time.Time`, `Duration` is
//     `time.Duration`, a `u128` of milliseconds is `uint64`.
//   - `FocusHandle` is a `ui.Handle` held in place (take its address).
//     `UniformListScrollHandle` is `uniformListScroll`: the list's
//     `ui.ListState` and the `uikit.ScrollHandle` its overlay bar reads.
//   - Key tables: one `[]uikit.KeyBinding[ShellAction]` per key context
//     (a Rust table that mixed contexts is split, as palette.go does), and
//     `actions!(meerkat, [RunQuery, …])` is the `ShellAction` enum, named
//     `ShellRunQuery` … with the module's prefix.
//   - A `Subscription` held to keep a listener alive has no Go value:
//     `Subscribe` lasts as long as the emitter, so `_subscriptions` and
//     `QueryTab::_editor` are gone (the editor and its listener go with
//     the tab).
package main

import (
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/dbpostgres"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/query"
	"github.com/nick318/meerkat/internal/resultsgrid"
	"github.com/nick318/meerkat/internal/sqleditor"
	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// ShellAction is the shell's actions: `actions!(meerkat, [RunQuery, …])`.
type ShellAction int

const (
	ShellRunQuery ShellAction = iota
	ShellStopQuery
	ShellNewQuery
	ShellCloseTab
	ShellRefresh
	ShellPrevPage
	ShellNextPage
	ShellShowHistory
	ShellNextTab
	ShellPrevTab
	ShellConfirmClose
	ShellCancelClose
	ShellSelectUp
	ShellSelectDown
	ShellSelectLeft
	ShellSelectRight
	ShellExtendUp
	ShellExtendDown
	ShellExtendLeft
	ShellExtendRight
	ShellSelectRowStart
	ShellSelectRowEnd
	ShellSelectFirstRow
	ShellSelectLastRow
	ShellSelectAll
	ShellCopySelection
	ShellTogglePick
	ShellClearSelection
	ShellFindColumn
	ShellColumnPrev
	ShellColumnNext
	ShellFindInTab
	ShellFindInResult
	ShellResultFindNext
	ShellResultFindPrev
	ShellFilterCatalog
	ShellCatalogPrev
	ShellCatalogNext
	ShellPeekValue
	ShellClosePeek
	ShellCopyPeek
	ShellSelectAllPeek
	ShellCommitTransaction
	ShellRollbackTransaction
	// ShellGoToTab1 … ShellGoToTab8 are ⌘1 … ⌘8, and ShellGoToLastTab is
	// ⌘9: a browser's keys for a tab by its place in the strip. The eight
	// are consecutive, so the place is the action less ShellGoToTab1.
	ShellGoToTab1
	ShellGoToTab2
	ShellGoToTab3
	ShellGoToTab4
	ShellGoToTab5
	ShellGoToTab6
	ShellGoToTab7
	ShellGoToTab8
	ShellGoToLastTab
)

// ShellKeyContext is the shell's own key context. The Rust wrote it as the
// literal `"Shell"` in every binding; it is named here so the Go tables
// can say which element they are dispatched on.
const ShellKeyContext = "Shell"

// GridKeyContext is the results grid's own key context. The arrows, ⇧
// with them, ⌘A, ⌘C, space, ⌘I and ⎋ are bound here, never to the shell:
// the shell's context is an ancestor of the SQL editor's, so a key bound
// there is taken from the editor before it can be typed.
const GridKeyContext = "ResultGrid"

// ConfirmKeyContext is the confirmation's own key context. Scoped, like
// the palette's: while the dialog is up it is the deepest match, so ⌘⏎
// answers it rather than running a query behind it.
const ConfirmKeyContext = "Confirm"

// ColumnFindKeyContext is the column-find popover's own key context.
// Scoped, like the palette's and the confirmation's: while the popover is
// up it is the deepest match, so ↑↓ walk what the search found rather than
// the result behind it.
const ColumnFindKeyContext = "ColumnFind"

// ColumnFindShellKeyBindings and ColumnFindKeyBindings are the key
// bindings for finding a column of the result on screen.
//
// ⌘J is the **shell's**, not the popover's: it has to open the thing as
// well as close it, and it has to work while the SQL editor holds the
// focus. ↑↓ belong to the popover, or they would be taken from the grid
// while nothing is open.
//
// ⏎ and ⎋ are bound nowhere here. The search line is a `TextField`, which
// reports both as events of its own, so the popover answers them the way
// the palette does.
//
// The Rust `column_find_key_bindings()` was one table carrying a context
// per binding; this is its ShellKeyContext half.
func ColumnFindShellKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("cmd-j", ShellFindColumn),
	}
}

// ColumnFindKeyBindings is the ColumnFindKeyContext half of the Rust
// `column_find_key_bindings()`; see ColumnFindShellKeyBindings.
func ColumnFindKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("up", ShellColumnPrev),
		uikit.Bind("down", ShellColumnNext),
	}
}

// ResultFindKeyContext is the result find strip's own key context. It
// sits beside the grid's rather than inside it: the grid binds a bare
// `space` to ticking a row, and an ancestor binding would take the spaces
// out of the search line.
const ResultFindKeyContext = "ResultFind"

// ResultFindShellKeyBindings, ResultFindGridKeyBindings and
// ResultFindKeyBindings are the key bindings for finding a value in the
// result on screen.
//
// ⌘F means "find in what I am looking at", so it is bound three times and
// the deepest context answers: in the SQL editor it is the editor's own
// find, in the grid it is this one, and anywhere else in the shell it
// picks by the tab — a query tab's editor, a table tab's result. ⌘G and
// ⇧⌘G step, in the grid as in the strip, over what the strip last
// searched for. ⏎ and ⎋ are the search line's own `Submit` and `Cancel`.
//
// The Rust `result_find_key_bindings()` was one table over three
// contexts; this is its ShellKeyContext part.
func ResultFindShellKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("cmd-f", ShellFindInTab),
	}
}

// ResultFindGridKeyBindings is the GridKeyContext part of the Rust
// `result_find_key_bindings()`; see ResultFindShellKeyBindings.
func ResultFindGridKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("cmd-f", ShellFindInResult),
		uikit.Bind("cmd-g", ShellResultFindNext),
		uikit.Bind("cmd-shift-g", ShellResultFindPrev),
	}
}

// ResultFindKeyBindings is the ResultFindKeyContext part of the Rust
// `result_find_key_bindings()`; see ResultFindShellKeyBindings.
func ResultFindKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("cmd-f", ShellFindInResult),
		uikit.Bind("cmd-g", ShellResultFindNext),
		uikit.Bind("cmd-shift-g", ShellResultFindPrev),
		uikit.Bind("shift-enter", ShellResultFindPrev),
	}
}

// CatalogFilterKeyContext is the sidebar filter's own key context. Scoped
// to that line, like the palette's and the column find's: ↑↓ walk the
// names the filter left while the line has the focus, and mean what they
// mean everywhere else as soon as it does not.
const CatalogFilterKeyContext = "CatalogFilter"

// CatalogFilterShellKeyBindings and CatalogFilterKeyBindings are the key
// bindings for the sidebar's filter.
//
// ⌘E is the **shell's**, like ⌘J and for the same reason: it has to reach
// the line from wherever the focus is — the SQL editor, the grid — which is
// the whole of what it is for. It only puts the focus there and marks what
// is already typed; it is not a toggle, because the line is on screen
// either way and a second press would have to guess where to hand the
// focus back to. ⎋ is the way out, as it was.
//
// A filter that answers with five tables is a list the user has to reach
// for the mouse to use, and the name they want is rarely the first one.
// ↓ steps into that list, ↑ steps back out of it, and ⏎ opens whichever
// name the cursor is on. Those two are the line's own, or they would be
// taken from the results grid whenever nothing is typed.
//
// ⏎ and ⎋ are bound nowhere here. The filter is a `TextField`, which
// reports both as events of its own, so the sidebar answers them the way
// the palette and the column find do.
//
// The Rust `catalog_filter_key_bindings()` was one table over two
// contexts; this is its ShellKeyContext half.
func CatalogFilterShellKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("cmd-e", ShellFilterCatalog),
	}
}

// CatalogFilterKeyBindings is the CatalogFilterKeyContext half of the
// Rust `catalog_filter_key_bindings()`; see CatalogFilterShellKeyBindings.
func CatalogFilterKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("up", ShellCatalogPrev),
		uikit.Bind("down", ShellCatalogNext),
	}
}

// PeekKeyContext is the value peek's own key context. Scoped, like every
// other overlay's: while the card is up ⏎ closes it, where ⏎ in the grid
// opened it.
const PeekKeyContext = "ValuePeek"

// PeekGridKeyBindings and PeekKeyBindings are the key bindings for reading
// a whole value.
//
// A lane is capped at [`results_grid`]'s `MAX_COLUMN_WIDTH`, so a long
// value truncates on screen. ⏎ over the cursor's cell asks for the rest of
// it; a double click in the grid asks for the same thing with the mouse.
//
// ⌘C is bound here as well as in the grid, and means something narrower:
// **this value**, not the selection. The card is about one cell, so the
// key that copies from it copies one cell.
//
// The Rust `peek_key_bindings()` was one table over two contexts; this is
// its GridKeyContext half.
func PeekGridKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("enter", ShellPeekValue),
	}
}

// PeekKeyBindings is the PeekKeyContext half of the Rust
// `peek_key_bindings()`; see PeekGridKeyBindings.
func PeekKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("enter", ShellClosePeek),
		uikit.Bind("escape", ShellClosePeek),
		uikit.Bind("cmd-c", ShellCopyPeek),
		uikit.Bind("cmd-a", ShellSelectAllPeek),
	}
}

// ConfirmKeyBindings is the key bindings for the close confirmation.
//
// ⏎ and ⎋ both keep what is open. The destructive answer takes ⌘⏎ — the
// deliberate chord, the same one that means "do it" everywhere else in
// the app. A dialog that ends server work on a stray ⏎ is a dialog the
// user learns to fear.
func ConfirmKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("cmd-enter", ShellConfirmClose),
		uikit.Bind("enter", ShellCancelClose),
		uikit.Bind("escape", ShellCancelClose),
	}
}

// The two pane sizes a hand can set: the sidebar's width and the SQL
// editor's height. Each is a default the comp drew, a rail at either end,
// and a key in the store's `ui_state` bag — one value for the whole app,
// not per connection, because a pane size is hand ergonomics rather than
// a property of any database.
const sidebarDefault float32 = 246.

// Narrow enough to be out of the way, wide enough that the filter line
// and a relation name still read.
const sidebarMin float32 = 180.

// Past this a sidebar is a second window, and it also never takes more
// than half the window — see [`Divider::range`].
const sidebarMax float32 = 520.

const editorDefault float32 = 250.

// About three lines of SQL: an editor shorter than its own statement is
// no editor, and the result below always keeps the rest of the pane.
const editorMin float32 = 72.

// The editor never takes more than this share of the window, so a result
// can always be seen under it — the ceiling is a share rather than a
// pixel count because it is the *result's* remainder being protected.
const editorMaxShare float32 = 0.7

// The divider's hitbox straddles the 1px border it lives on: wide enough
// to hit without aiming, and taking no layout room — it is an absolute
// overlay, so nothing moves by a pixel when it appears.
const handleHitbox float32 = 7.

// The line painted down its middle on hover and while dragging. The rest
// of the time the divider paints nothing: the border under it is the mark,
// and the resize cursor is what names the gesture.
const handleLine float32 = 2.

// Every sidebar row is this tall, headers included: `uniform_list` needs
// one height to measure, and the design's rows are already within a
// pixel of each other.
const catalogRowHeight float32 = 26.

// The sidebar's filter line, a size under the relation names it filters.
const filterFontSize float32 = 12.

// How far back the history screen looks, in days. The comp's header says
// "last 7 days", and that is the window the list actually reads.
const historyDays int64 = 7

// What a tab says when it wanted the server before there was one. The
// cached catalog lets a session open tabs while the connect is still in
// flight, so this is reachable on a first keystroke.
const notConnected = "not connected yet"

// The id the connect strip's `copy` link goes by in `copied_error`. Tab
// ids start at 1, so no tab's error strip can claim it.
const connectCopyID uint64 = 0

// How long a connect is out before the strip says so.
//
// A connect to a server next door lands in a few milliseconds, and a
// strip that opened and shut inside them would shove the pane down and
// back on every session — a flicker that reports nothing. It is the run
// button's rule, `RUN_ARM`: the notice appears once the wait has proven
// itself one. A failure is never held back; it paints when it lands.
const connectArm = 400 * time.Millisecond

// How long the typing has to stop before the buffer is checked.
//
// A check is a round trip, and a round trip per keystroke would be a
// question a second about text the user is halfway through writing. It is
// also long enough that a squiggle never appears under the character
// being typed, which is the mark's worst reading: a red line that chases
// the caret says the user is wrong about a word they have not finished.
const checkDebounce = 500 * time.Millisecond

// How many statements of a buffer are checked.
//
// One round trip each, and a scratchpad of two hundred statements is a
// file rather than a query. What is over the cap is left unmarked rather
// than marked wrongly — the buffer is checked from the top, which is
// where the user is reading.
const checkStatements int = 20

// How often the toolbar's run timer repaints. The design prints tenths of
// a second, so this is the slowest tick that still reads as a clock.
const timerTick = 100 * time.Millisecond

// How long a run has to last before the timer appears. Most statements
// come back in tens of milliseconds, and a pill that flashed up and out
// again on every one of them would be noise: the timer is there to say a
// query is taking a while, so it says nothing until one is.
const timerDelay = 1 * time.Second

// How long a run is given to answer before the button offers to stop it.
//
// **The verb must not flicker.** Most statements come back in tens of
// milliseconds, and a button that read "run · stop · run" on every one of
// them would be a button nobody could aim at. So the first `RUN_ARM` of a
// run keeps the word "run" — the fill is already saying a run is out — and
// the escalation to "stop" happens only for a run that has proved slow —
// which
// is the comp's own arming window, and its own comment: *"stop" only
// appears once the query has proven itself slow*.
//
// The comp cheats there, because a simulation knows how long its query
// will take and can decide during the window that this one will be quick.
// Nothing here can know that, so the honest reading of the same rule is
// the elapsed time alone. It paints the same screen: a statement that
// answers inside the window never shows a stop button at all.
const runArm = 420 * time.Millisecond

// **The run button moves in one way only: its own fill, breathing.**
//
// The comp animates a run with the vocabulary of a progress widget — a
// front sweeping the button, then an indeterminate band crossing it on a
// loop, then a ring blooming out of the border when the rows land. Those
// are Material's marks, and three of them are a lot of movement to put on
// a 27-pixel button that sits beside a paper-coloured toolbar all day. They
// also each carry a shape, and a shape crossing a button asks to be
// watched.
//
// A tone does not. So the whole of the animation here is the fill mixing a
// little of the button's own ink into itself and back out again: nothing
// travels, nothing has an edge, and the button says "working" the way a
// held breath does. `RUN_BREATH` is one full cycle and `RUN_BREATH_DEPTH`
// how far it goes — far enough to be seen out of the corner of an eye,
// and no further, because a button that pulses hard is a button that
// interrupts.
const runBreath = 2600 * time.Millisecond

const runBreathDepth float32 = 0.12

// The last exhale, when the rows land: the same mix, easing out to
// nothing. It is deliberately the *same* mark as the breath rather than a
// flash of its own, so a run reads as one gesture that starts and stops —
// and it is what tells the user a statement too quick to breathe has been
// answered at all.
const runSettle = 320 * time.Millisecond

const runSettleDepth float32 = 0.1

// The press, which is a tone and **not** a movement: the same mix, the
// other way, toward the ink of the app rather than the button's own. A
// button that drops a pixel under the pointer is the elevation idiom this
// one is deliberately not in — and nothing shifting means nothing to
// track.
const runPressDepth float32 = 0.08

// The label's width, held still across the verbs so that the button does
// not resize under the pointer when a run turns out to be slow. "run" and
// "stop" share one width; "terminate" is three times the word and gets its
// own.
//
// **Each one is sized by the longest word it serves, not by the first.**
// The font is monospaced at 0.6 em an advance, so an 11px label costs
// 6.6px a character: "stop" needs 26.4px and "terminate" 59.4px. A width
// that fits "run" clips the "p" off "stop" — the button's own label is the
// one thing on it that must never be cut, because the word *is* what the
// button is offering to do. The couple of pixels over are the rounding.
const runLabelWidth float32 = 30.

const terminateLabelWidth float32 = 64.

// How long a tab's session may sit unused before it is handed back, and
// how often the sweep looks. Ten minutes is long enough that a session is
// still there when the user comes back from a meeting having left a tab
// mid-thought, and short enough that a laptop left open overnight is not
// holding eight backends on a shared server.
const idleSession = 600 * time.Second

const idleTick = 60 * time.Second

// The close confirmation's geometry. Narrower than the palette and
// higher up the window: it is a sentence and two answers, not a list.
const confirmWidth float32 = 420.

const confirmTopMargin float32 = 140.

// The value peek's geometry: wider than the close dialog, because what it
// holds is a value rather than a sentence, and no taller than this before
// the value scrolls inside it.
const peekWidth float32 = 640.

const peekTopMargin float32 = 120.

const peekMaxHeight float32 = 360.

// How much of a value the card paints. `db_client::MAX_CELL_BYTES` lets a
// megabyte of text into one cell, and laying a megabyte of wrapped text out
// on the GPUI thread would freeze the window — so the card shows the first
// of it and says how much there is. ⌘C still copies the whole value: the
// bound is on what is *painted*, as the row cap is on what is *held*.
const peekChars int = 4_000

// The column-find popover's geometry. It sits in the middle of the shell,
// not under its button (see columnFindOverlay), so it is a dialog and
// takes the palette's proportions rather than the comp's 290px dropdown:
// the palette's row height and search line, and room for a long snake_case
// name. The list grows to nine rows and then scrolls; the box holds those
// nine and the 6px it is padded by.
const columnFindWidth float32 = 440.

const columnRowHeight float32 = PaletteRowHeight

const columnListMaxHeight float32 = 9.*columnRowHeight + 2.*6.

// The popover at its tallest: the list at its cap, with the search line
// over it and the key hints under it. The overlay centres a frame of this
// height rather than the card, so the search line never moves as the
// list shrinks; the two rows' share only has to be close, since being
// off by a few pixels moves the card's centre, not the line.
const columnFindMaxHeight float32 = columnListMaxHeight + columnFindChrome

// columnFindChrome is the search line and the key hints together, borders
// included, as a headless frame measures them.
const columnFindChrome float32 = 83.

// The search line inside it, a size up from the list it filters.
const columnFindFontSize = theme.SizeInput

// A tab's height inside the title bar: a pill, with the bar's own
// padding above and below it.
const tabHeight float32 = 30.

// A tab is never squeezed below this, so the strip reads as a row of
// tabs rather than a row of words of different lengths.
const tabMinWidth float32 = 116.

// ...and never wider than this, so one long table name cannot take the
// strip. The title truncates at that point.
const tabMaxWidth float32 = 220.

// Divider is one of the two dividers a hand can drag. Everything either
// divider needs — its rails, its key in the store, its cursor — hangs off
// this, so the two cannot drift into two unrelated behaviours.
type Divider int

const (
	// DividerSidebar is sidebar | main: drags the sidebar's width.
	DividerSidebar Divider = iota
	// DividerEditor is editor | result, inside a query tab: drags the
	// editor's height.
	DividerEditor
)

func (d Divider) key() string {
	switch d {
	case DividerSidebar:
		return "sidebar_width"
	default:
		return "editor_height"
	}
}

func (d Divider) defaultSize() float32 {
	switch d {
	case DividerSidebar:
		return sidebarDefault
	default:
		return editorDefault
	}
}

// rangeIn is the rails, against the window in hand. A saved size is
// clamped by the same rule as a drag, so a width dragged out on a big
// monitor cannot open a sidebar past half of a small one. The ceiling
// never falls below the floor, whatever the window has shrunk to, or the
// clamp itself would panic.
//
// Go port: Rust's `range(viewport: Size<Pixels>)`; `range` is a Go
// keyword, and the viewport is its width and height.
func (d Divider) rangeIn(width, height float32) (float32, float32) {
	var min, max float32
	switch d {
	case DividerSidebar:
		min, max = sidebarMin, minF32(sidebarMax, width/2.)
	default:
		min, max = editorMin, height*editorMaxShare
	}
	return min, maxF32(max, min)
}

// cursor is what the window wears for the whole drag: the pointer leaves
// the 7px handle on the first frame, and a cursor that flickered back to
// an arrow mid-drag would say the drag had ended.
func (d Divider) cursor() ui.Cursor {
	switch d {
	case DividerSidebar:
		return ui.CursorResizeColumn
	default:
		return ui.CursorResizeRow
	}
}

// PaneDrag is a divider drag in flight. It is the mouse's own state
// between one frame and the next, which is why it lives on the shell
// beside `run_pressed` rather than on any tab. `anchor` is where the
// pointer went down and `start` the size at that moment, so the size
// tracks the pointer's *travel* — a drag that jumped the pane edge under
// the pointer on the first frame would read as the handle slipping out of
// the hand.
type PaneDrag struct {
	divider Divider
	anchor  float32
	start   float32
}

// draggedSize is the whole of the drag policy: the size the pointer's
// travel asks for, held between the rails. A plain function over numbers,
// so it is argued with in a test rather than in a running window.
func draggedSize(start, anchor, at, min, max float32) float32 {
	return clampF32(start+at-anchor, min, maxF32(max, min))
}

// clampF32 is Rust's `f32::clamp` (min ≤ max is the caller's promise): a
// NaN goes through as it came.
func clampF32(x, min, max float32) float32 {
	if x < min {
		x = min
	}
	if x > max {
		x = max
	}
	return x
}

// minF32 and maxF32 are Rust's `f32::min` / `f32::max`: a NaN on one side
// gives the other.
func minF32(a, b float32) float32 {
	if a != a {
		return b
	}
	if b != b {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func maxF32(a, b float32) float32 {
	if a != a {
		return b
	}
	if b != b {
		return a
	}
	if a > b {
		return a
	}
	return b
}

// uniformListScroll is GPUI's `UniformListScrollHandle`: the list's own
// state (what `scroll_to_item` goes through — `List.ScrollTo`,
// `ScrollIntoView`) and the plain scroll handle inside it that an overlay
// scrollbar reads (`.0.borrow().base_handle`). Give the list
// `TrackScroll(&h.Handle.State)` and write `h.Handle.Bounds` after
// building it. Hold it in place: a `ui.ListState` must not be copied.
type uniformListScroll struct {
	List   ui.ListState
	Handle uikit.ScrollHandle
}

// copiedError is the error strip whose message was last put on the
// clipboard: Rust's `(u64, String)`.
type copiedError struct {
	tab     uint64
	message string
}

// Shell is the workspace of one window: the session and everything it
// shows.
type Shell struct {
	focusHandle ui.Handle
	// The focus the results grid takes when the user clicks into it, and
	// the reason the grid's keys are scoped rather than global.
	//
	// A query tab's editor lives *inside* the shell's element, so a key
	// bound to the shell's context is matched before the keystroke can
	// reach the editor's text input — a bare `space` bound to "tick this
	// row" would eat the spaces out of the user's SQL. The grid keys are
	// bound to `GRID_KEY_CONTEXT` instead, which is only in the context
	// stack while this handle holds the focus.
	gridFocus  ui.Handle
	status     Status
	connection dbclient.Connection
	// What this shell was opened on, kept so a failed connect can be
	// asked again in place: the retry goes down the path the first
	// attempt took, with nothing rebuilt.
	target Target
	// Counts the connects, as a tab counts its runs. A retry bumps it,
	// and a reply that carries an older number is dropped.
	connectGeneration uint64
	// When the connect in flight left, for `CONNECT_ARM`.
	connectStarted time.Time
	catalog        *introspect.Catalog
	// True while the introspection runs. It is a second round trip after
	// the connect, so a session is connected — and can run a query —
	// before there is a catalog to paint.
	catalogLoading bool
	// Why the last introspection failed, when one did. The connection is
	// still usable then: a query tab works without a catalog.
	catalogError *string
	// The sidebar's groups, built once when the catalog lands: a schema's
	// tables, then its views.
	catalogGroups []Group
	// Those groups flattened for `uniform_list`, as the open set and the
	// filter leave them. Rebuilt when either changes, never per frame.
	catalogRows []CatalogRow
	// Which schemas are open. Empty to start: a catalog of thousands of
	// relations is a wall of names when every schema is expanded, so the
	// sidebar opens closed and the user opens what they want.
	openSchemas map[string]struct{}
	// Which `Tables` / `Views` sections are closed. This one is the other
	// way round because a schema the user just opened was opened to see
	// what is in it: sections start open, and closing one is the choice
	// worth remembering.
	closedSections map[string]struct{}
	// Relations folded out to show their columns and objects, keyed by
	// `schema\trelation`. Closed until opened, as a schema is.
	openRelations map[string]struct{}
	// The sidebar's filter line. It narrows the rows already in hand — no
	// query goes out for it.
	catalogFilter *uikit.TextField
	// Which row of the list the arrow keys are on, as an index into
	// `catalog_rows`. It only ever names a relation, and `None` says the
	// cursor is still on the line itself — where ⏎ and ⇥ read the *first*
	// match instead, as they did before there was a cursor at all.
	catalogSelected *int
	// Where the sidebar is scrolled, kept across re-renders.
	catalogScroll uniformListScroll
	// Which of the sidebar's scrollbars is being dragged, if any.
	catalogDrag uikit.DragState
	// How many relations the catalog holds, for the sidebar footer.
	relationTotal int
	// Every name in the catalog, for the editor's colouring.
	vocabulary *sqleditor.Vocabulary
	label      *dbpostgres.Label
	// What the user called this connection, from the profile. It names the
	// session everywhere the shell names it, because the name is what the
	// user chose to recognise the connection by — two profiles often point
	// at the same database name on different hosts. A command-line URL has
	// no profile and so no name, and falls back to the database.
	name *string
	// Whether this session refuses to write. The connection asked the
	// server for it, so this field only *reports* it — the top bar's mark
	// and the two lines that name the mode read it. A command-line URL
	// carries no setting and is read-only.
	readOnly bool
	tabs     []Tab
	active   int
	nextID   uint64
	// The local file that remembers what this session ran. `None` when
	// it could not be opened; the history screen then says so instead of
	// showing an empty list.
	store      *storage.Store
	storeError *string
	// Which connection the history belongs to: a profile id, or a key
	// built from the URL the app was started with. A password never
	// reaches it.
	scope string
	// The environment tag the profile wears, read once on the way in:
	// it colors the window frame, so a prod session can never be
	// mistaken for a dev one. A command-line URL has no tag.
	env *Env
	// The ⌘K palette, while it is open. It lives on the shell rather than
	// in a window of its own, so closing it cannot leave the workspace
	// without focus.
	palette *Palette
	// The column-find popover, while it is open. It searches the result the
	// active tab holds, so it lives beside the palette rather than on a
	// tab: one is open at a time, over whichever result is on screen.
	columnFind *ColumnFind
	// The ⌘F strip over the result, while it is open. Like the column
	// find it is a control on one result, so it lives here and names its
	// tab rather than living on the tab.
	resultFind *ResultFind
	// What the result strip last searched for, kept when it closes so ⌘G
	// in the grid goes on stepping and ⌘F opens on it again.
	resultNeedle string
	// The cell whose whole value is being read, while the card is up.
	peek *Peek
	// The close the user is being asked about, while the dialog is up.
	confirm *Confirm
	// True while a repaint loop is running for the query timers. Runs come
	// and go in several tabs at once; the loop belongs to the window.
	timing bool
	// Whether the run button is being held down. It is the mouse's own
	// state between one frame and the next rather than anything about the
	// run, which is why it lives here beside the palette and not on a tab —
	// the same reason the grid keeps its hovered row on `GridState`. One
	// button is on screen at a time, so one flag covers it.
	runPressed bool
	// title_drag — the title bar's own mouse state, whether a press on it
	// may still become a window drag — has no Go field: mygo's
	// `DragWindow` on `uikit.TitleBar` drags only when the innermost
	// interactive element under the press is the bar itself, so a tab or
	// a button in it claims its own press (mapping sheet §17).
	//
	// The error strip whose message was last put on the clipboard: its
	// tab and the message itself. The strip's link says `copied` while
	// both still match, so the press has an answer on screen; a new
	// error in that tab is a different message and reads `copy` again.
	copiedError *copiedError
	// The divider drag in flight, if any. Mouse state between frames,
	// like `run_pressed`, and one at a time: there is one pointer.
	paneDrag *PaneDrag
	// The two sizes a hand can set, read from the store on the way in
	// and written back when a drag ends. They live here rather than on a
	// tab because every tab shares them: an editor height that jumped on
	// every tab switch would read as the strip reshuffling the pane.
	sidebarWidth float32
	editorHeight float32
	// True while the idle-session sweep is running. One loop for the
	// window, and only while there is a session to sweep.
	sweeping bool
	// True while the session reopens the tabs it was left with. The
	// restore builds the strip one tab at a time, and every one of those
	// steps would otherwise write a half-built strip back over the saved
	// one — a restore that stopped halfway would lose the rest.
	restoring bool
	// True once the user has agreed to a quit this window asked about.
	// The quit then walks every window again — this one included — so
	// without the flag the same dialog would come back for ever: a run
	// asked to stop is still in flight, and a transaction is still open,
	// until the cancels land.
	quitting bool

	// Go port: what GPUI's `Context<Self>` carried.

	// The window this shell paints in: where replies from goroutines are
	// handed back (`win.Update`) and where the viewport is read. Nil only
	// in a test.
	window *mygo.Window
	// The listeners of `impl EventEmitter<ShellEvent>`.
	listeners []func(*Shell, ShellEvent)
	// Set once the window has let this shell go (GPUI dropped the
	// entity). A reply that lands afterwards is dropped, as
	// `WeakEntity::update` failed in the Rust.
	released bool
}

// Palette is the open palette: what was typed, what it found, and where
// the selection is.
type Palette struct {
	query *uikit.TextField
	// The scope chip that is lit. A `t:` typed into the line overrides it
	// for as long as it is there.
	chip PaletteScope
	// This connection's runs, read from the local file once when the
	// palette opened. Searching them again per keystroke is a filter over
	// this vector, not another read.
	runs []storage.QueryRun
	rows []PaletteRow
	// How many results matched before the section caps cut the list.
	matches int
	// Index into `rows` of the selected result. It always points at a
	// result, never at a heading.
	selected int
	scroll   uniformListScroll
}

// ColumnFind is the open column-find popover: one line searched against
// the names of the columns the result on screen came back with.
//
// **What it found is not kept here.** A run or a page turn replaces the
// result under the popover, and a list of lane indices remembered from the
// result before would point at lanes that no longer exist — so the matches
// are worked out from the line and the result in hand, every time they are
// wanted. What is kept is only where the user has walked to.
type ColumnFind struct {
	// The tab whose result is being searched. The popover is a control on
	// one result, so switching tabs closes it rather than carrying it over
	// to a result it was never opened on.
	tab   uint64
	query *uikit.TextField
	// Index into the **matches**, not into the result's columns: the list
	// is what ↑↓ walk. It is clamped where it is read, because the list can
	// grow shorter under it.
	selected int
	scroll   uniformListScroll
}

// ResultFind is the open ⌘F strip over a result: one line searched
// against the values the result holds.
//
// **What it found is remembered, not kept.** A run or a page turn replaces
// the result under the strip, so a list of cells taken from the result
// before would point at cells that are not there. But the search reads
// every value in the result, which is too much to pay per frame, so the
// answer is memoised on the result it was worked out from — held by a
// `Weak`, which also keeps the allocation from being reused by the next
// result — and on the line's text. A new result or a keystroke is a miss.
type ResultFind struct {
	// The tab whose result is being searched. Switching tabs closes the
	// strip rather than carrying it over to a result it was never opened
	// on.
	tab   uint64
	query *uikit.TextField
	// Go port: Rust's `RefCell<Option<FoundCells>>`, written from a
	// `&self` method; nil is `None`.
	found *FoundCells
}

// FoundCells is one search over one result, and the two things it was
// worked out from.
type FoundCells struct {
	// Go port: Rust's `Weak<GridData>`, compared by address. A Go pointer
	// keeps the old result alive for as long as the memo does, which is
	// also what stops its address being reused by the next one.
	data   *resultsgrid.GridData
	needle string
	cells  []resultsgrid.Cell
	capped bool
}

// Peek is the cell whose whole value is on screen.
//
// A lane is capped, so a value longer than about forty characters truncates
// in the grid — and the question that raises is "what is in this cell",
// which is not the same question as "how wide should this column be". So
// the answer is a card over one cell rather than a wider lane: widening a
// lane to two thousand pixels only turns reading into panning.
//
// It holds **where** the value is, never the value itself. A run or a page
// turn replaces the rows under it, and a string copied when the card opened
// would go on saying what used to be there.
type Peek struct {
	tab  uint64
	cell resultsgrid.Cell
	// The card holds the focus while it is up, so its key context is the
	// deepest one and ⏎ closes rather than opening another.
	focus ui.Handle
	// What the user has marked, as byte offsets into the text the card
	// **painted** — not into the value, which may be longer than what
	// `PEEK_CHARS` let on screen. Empty means nothing is marked, and ⌘C
	// then means the whole value, as it always did.
	selection query.Range
	// Where the pointer went down, while the button is still down. A
	// drag is anchored there and every move extends to it, so dragging
	// backwards marks the same run of text as dragging forwards.
	anchor *int
}

// StopTask is one "stop this run" request on its way to the server.
//
// Go port: GPUI's `Task<Result<anyhow::Result<bool>, JoinError>>`. The
// request runs on its own goroutine from the moment it is made (Rust
// detached or awaited the task; a goroutine is never aborted by dropping
// a handle), and Wait blocks until the server has answered — which is
// what ⌘Q does before it lets the process go.
type StopTask struct {
	done    chan struct{}
	stopped bool
	err     error
}

// newStopTask sends one stop on a goroutine of its own. A panic in it is
// the task dying, reported the way `flatten` reports a JoinError.
func newStopTask(stop func() (bool, error)) *StopTask {
	task := &StopTask{done: make(chan struct{})}
	go func() {
		defer close(task.done)
		task.stopped, task.err = flatten(joinTask(stop))
	}()
	return task
}

// Wait blocks until the stop has been answered: whether the server had
// something to stop, or why the request failed.
func (t *StopTask) Wait() (bool, error) {
	<-t.done
	return t.stopped, t.err
}

// Confirm is a close the user has to agree to, because it would end work
// the server is still doing.
//
// Closing the socket is not a cancel: Postgres notices a client is gone
// when the backend next writes, which a long `SELECT` may not do for
// minutes. So a close that abandons a run leaves the server working for
// a window that is not there any more — and the user is never told.
type Confirm struct {
	what Close
	// The tabs whose runs this close would stop, by name.
	running []string
	// The tabs whose open transactions this close would roll back. One of
	// the two lists is always filled: with nothing to lose, no question.
	open []string
	// The dialog holds the focus while it is up, so its key context is
	// the deepest one and answers the keystroke.
	focus ui.Handle
}

// CloseKind is which way out a Close is.
type CloseKind int

const (
	// CloseKindTab is ⌘W, or the × on a tab — except on the last tab,
	// which closes the window instead. See `close_intent`.
	CloseKindTab CloseKind = iota
	// CloseKindShell is "‹ connections". The shell is dropped, and every
	// tab with it.
	CloseKindShell
	// CloseKindWindow is the window's close button. This window's tabs
	// go; the app stays if another window is open.
	CloseKindWindow
	// CloseKindQuit is ⌘Q. Every window's tabs go, and the app with them.
	CloseKindQuit
)

// Close is which way out is being confirmed.
//
// There are five ways out of a session and only one of them is ⌘W, so a
// guard wired to the tab alone would be a lie in the other four: the
// three here that drop every tab at once, and the × on the tab strip.
//
// **The window's close button and ⌘Q are not the same way out**, now that
// there can be more than one window. Closing a window ends that window's
// work; quitting ends every window's, and the app goes with it. One
// wording could not say both, and one guard could not: ⌘Q has to ask each
// window in turn.
//
// Go port: Rust's `enum Close { Tab(u64), Shell, Window, Quit }`. Tab is
// the tab's id, set only for CloseKindTab; `==` is the derived equality.
type Close struct {
	Kind CloseKind
	Tab  uint64
}

// closeIntent is what a ⌘W — or a click on a tab's × — actually ends. The
// last tab is the exception: a workspace with an empty strip has no tab to
// open the next query in, so the gesture that took the last tab away
// takes the **session** with it and puts the window back on the
// connections screen. The window stays: closing it, and with the last
// window the app, is the next ⌘W there, or the close button.
//
// The Rust app closed the window here, the browser's rule. That made a
// ⌘W on the last tab quit an app of one window outright, which is not
// what a key that closes tabs should do.
//
// It is a `Close::Shell` carrying the tab, not a tab close followed by a
// leave, because the guard has to ask the leave's question: the dialog
// says what is ending, and what is ending is the session. The tab rides
// along so the leave forgets it — the user closed it, so the connection
// must not open on it again next time.
func closeIntent(tabs int, tabID uint64) Close {
	if tabs <= 1 {
		if tabs == 0 {
			tabID = 0
		}
		return Close{Kind: CloseKindShell, Tab: tabID}
	}
	return Close{Kind: CloseKindTab, Tab: tabID}
}

// Target is what the workspace was opened on: a URL from the command
// line, or a connection saved on the connections screen. One of TargetUrl
// or TargetProfile.
type Target interface {
	isTarget()
}

// TargetUrl is `Target::Url`.
type TargetUrl struct {
	Url string
}

// TargetProfile is `Target::Profile`.
type TargetProfile struct {
	Profile dbclient.Profile
}

func (TargetUrl) isTarget()     {}
func (TargetProfile) isTarget() {}

// ShellEvent is what the workspace tells the window around it.
type ShellEvent int

const (
	// ShellEventClose: the user asked for the connections screen back.
	ShellEventClose ShellEvent = iota
	// ShellEventQuit: the user agreed to a quit this window had asked
	// about. The window cannot quit on its own — the other windows have
	// not been asked yet — so it says so and the app's own quit walks the
	// list again.
	ShellEventQuit
	// ShellEventQuitCancelled: the user refused a quit at this window's
	// dialog. Every window that agreed to it before this one was asked is
	// asked again next time.
	ShellEventQuitCancelled
)

// Status is where the connect is. One of StatusConnecting,
// StatusConnected or StatusFailed.
type Status interface {
	isStatus()
}

// StatusConnecting: the connect is out. The string is where it is going
// — `host:port` — which is what the strip over the pane says while it
// waits.
type StatusConnecting struct {
	To string
}

// StatusConnected is `Status::Connected`.
type StatusConnected struct{}

// StatusFailed: the connect came back refused: its class, and the
// server's or the system's own words. The strip over the pane paints it,
// and a run asked for meanwhile repeats it.
type StatusFailed struct {
	Error dbclient.ConnectError
}

func (StatusConnecting) isStatus() {}
func (StatusConnected) isStatus()  {}
func (StatusFailed) isStatus()     {}

// Tab is one tab of the strip: `*TableTab`, `*QueryTab` or `*HistoryTab`.
// See the shape decisions at the top of this file.
type Tab interface {
	ID() uint64
	Title() string
	Selection() *resultsgrid.Selection
	Marked() *Marked
	isTab()
}

// TableTab is the paged view of one table.
type TableTab struct {
	id         uint64
	schema     string
	table      string
	kind       introspect.TableKind
	data       *resultsgrid.GridData
	page       int
	approxRows *uint64
	timing     *Timing
	loading    bool
	error      *string
	// What the user has marked in the result: the focused cell and the
	// range around it, and the rows ticked in the gutter. A page is a
	// different set of rows, so turning the page clears it.
	selection resultsgrid.Selection
	// Scroll position, kept across the re-render after every page.
	scroll     resultsgrid.GridState
	generation uint64
}

// QueryTab is a statement the user writes and runs, and what it answered.
type QueryTab struct {
	id    uint64
	title string
	// The relation the tab was opened on, when it came from the sidebar.
	// The statement is the user's to edit from there on, so this says
	// where the tab started, not what it now runs — the sidebar marks the
	// row it came from with it.
	//
	// Go port: Rust's `Option<(String, String)>`, schema then relation.
	relation *[2]string
	editor   *sqleditor.SqlEditor
	data     *resultsgrid.GridData
	// Whether the last run answered with a **result set** — columns to
	// paint a grid from, however few rows came back with them.
	//
	// It is not "the run worked". A statement that changes rows works and
	// answers with no result set at all, and painting a grid of no columns
	// for it drew a bare gutter and a row of nothing: a table where there
	// was never a table. So this is what decides whether there is a grid,
	// and [`QueryTab::changed`] is what the line says instead.
	hasResult bool
	// What the run's counted statements changed, when any of them did.
	//
	// `None` says no statement of the buffer answered with a count, so
	// there is nothing to report — which is different from a count of
	// zero, and the difference is the whole point: zero means the `WHERE`
	// matched nothing, and that is news.
	changed *Changed
	// Whether the memory cap ended the read before the server ran out of
	// rows. The result line says so, because a grid that stops at an
	// arbitrary row must not read as the whole answer.
	truncated bool
	// What the server said beside the last run's results — notices,
	// warnings, notifications — in the order it said them. Nil on a tab
	// whose last run failed or said nothing.
	messages []string
	// How many statements the last run sent, so the result strip can say
	// which set is on screen.
	statementsRun int
	timing        *Timing
	error         *string
	// Where the tab's last run got to. It drives the run button, the
	// timer beside it and the result line, which is why all three agree.
	run Run
	// How many runs have landed with a result in this tab.
	//
	// It is a **counter, not a time**, because that is all the run
	// button's settle needs: GPUI restarts an animation when the element's
	// id changes, so the id carries this number and the exhale needs
	// neither a timer of its own nor an `Instant` to be checked against. A
	// run that failed or was cancelled does not count — the error strip is
	// the answer there, and a warm confirming tone would be the wrong
	// word.
	landed uint64
	// Whether this tab's session is sitting inside a transaction, as of
	// its last run — one the user typed a `BEGIN` for, or one the app
	// opened because the tab is in [`TxMode::Manual`].
	//
	// Read after every run rather than at the moment it is wanted,
	// because the moment it is wanted is a close — and a close cannot
	// wait on a round trip to find out what to ask. The cache is exact,
	// not a guess: the connection is pinned to this tab, so nothing but
	// this tab's own statements can change what it is in.
	inTransaction bool
	// Who ends this tab's transactions. It is a property of the **tab**
	// and of nothing else, because a transaction lives on one connection
	// and a tab is one connection. A tab opens on auto and the toolbar's
	// two chips switch it; the mode is remembered per tab in `open_tabs`.
	txMode dbclient.TxMode
	// What has landed inside the transaction that is open: the
	// statements, and the rows they changed. One value rather than two
	// fields, so every way a transaction ends forgets both at once.
	txTally TxTally
	// How the last transaction ended, until the next run or the next
	// change of mode. The bar goes on saying so for a moment, because
	// "committed" is the answer to the question the user just asked and a
	// bar that vanished would leave it unanswered.
	txDone *dbclient.TxEnd
	// True while a commit or a rollback is out. One at a time: the
	// session is one connection, and a second press must not send a
	// second boundary down it.
	txEnding bool
	// When this tab last used its session. Only a run counts: reading a
	// result on screen costs the server nothing, and a connection held
	// open for a tab nobody is asking anything is the thing the sweep
	// exists to give back.
	lastUsed time.Time
	// Whether the last session this tab had was taken back rather than
	// closed with the tab. The toolbar says so until the next run, which
	// opens another — a `search_path` that reset must be explainable.
	sessionEnded bool
	// This tab's own connection, from its first run onwards.
	//
	// It is what makes a `SET`, a `BEGIN` and a temp table mean anything
	// from one statement to the next: the tab is a session in the user's
	// head, so it is one on the wire. A run that fails hands the session
	// back all the same — a syntax error does not end a transaction, and
	// dropping the session here would both lose the user's state and
	// return an uncommitted connection to the pool.
	session dbclient.Session
	// What the last run did to the shape of the database, when it held a
	// `CREATE`, `ALTER`, `DROP` or `TRUNCATE`. `None` is the ordinary
	// case and paints nothing. See [`SchemaReport`].
	schemaReport *SchemaReport
	// What the user has marked in the result. A run replaces the rows, so
	// it clears with them.
	selection  resultsgrid.Selection
	scroll     resultsgrid.GridState
	generation uint64
	// How many syntax checks this tab has asked for. A check is
	// debounced, so the answer arrives after several more keystrokes have
	// gone by; a reply that does not carry this number is about text that
	// is no longer in the buffer. It counts separately from
	// `generation`, which counts runs: a check must not be called off by
	// a run, and a run must not be called off by a keystroke.
	checked uint64
	// `_editor: Subscription` — held so the editor's `Changed` event
	// reached the shell for as long as the tab was open — has no Go
	// field: the listener lives on the editor, and goes with it.
}

// RunKind is which of the comp's four states a Run is in.
type RunKind int

const (
	// RunIdle covers both "never run" and "finished". The zero value.
	RunIdle RunKind = iota
	// RunRunning: the statement is out — or, with no backend yet, the
	// tab's session is still opening and the statement has not left. ⌘.
	// covers both: it asks the server to give the statement up, or it
	// calls the run off before it starts.
	RunRunning
	// RunCancelling: a cancel has gone to the server and the statement has
	// not come back yet. A second ⌘. terminates the backend instead of
	// asking it nicely, which is the only reason this is a state of its
	// own.
	RunCancelling
	// RunCancelled: stopped on the user's word, after `elapsed`
	// milliseconds.
	RunCancelled
)

// Run is the comp's four states for a query tab's run, and the whole of
// what the run button offers: run, stop, terminate.
//
// `Idle` covers both "never run" and "finished", because the button says
// the same thing in each — the design's own `qIdle` groups them too.
//
// Go port: Rust's `enum Run { Idle, Running(Live), Cancelling(Live),
// Cancelled { elapsed } }` as a tagged struct; the zero value is `Idle`.
type Run struct {
	kind RunKind
	// The run in flight, for RunRunning and RunCancelling.
	flight Live
	// Milliseconds, for RunCancelled.
	elapsed uint64
}

// Live is a run in flight.
type Live struct {
	// When it started, for the timer in the toolbar. Wall clock is not
	// wanted here: the timer measures a wait, not a time of day.
	started time.Time
	// The backend the statement is on, known before the run leaves —
	// because it belongs to the tab's session, not to the request.
	//
	// `None` says the session is still opening, and that is not a gap in
	// what is known: **no statement is out yet**, so there is nothing on
	// the server to cancel. A stop pressed here calls the run off instead,
	// which is why it needs no waiting and no atomic. Before sessions the
	// id arrived one round trip *into* the run, and the difference is the
	// whole reason a stop used to have to wait for a race.
	backend *dbclient.RunId
}

// Timing is where a finished run's time went.
//
// **One number cannot answer the question the user is asking.** A wall
// clock around the call says "you waited 340 ms" and says nothing about
// whether the database was slow or the link was — and over a VPN to a
// remote server the link is usually the larger half. So a run reports two:
// what the **server** spent on the statement, and the **lag** around it.
//
// The two always add up to `total_ms`, because the lag is the total less
// the server's share rather than a clock of its own. Time the app cannot
// account for has to show up somewhere, and the honest place for it is the
// half that means "not the database".
type Timing struct {
	// Wall clock around the whole run, which is what the user waited.
	totalMs uint64
	// What the client could see for itself: the round trip, and how long
	// the first row took to appear. See `db_client::Wire`.
	wire dbclient.Wire
	// What the server says it spent, from `pg_stat_statements`.
	//
	// It is **filled in after the result is already on screen**, so it is
	// `None` for a moment on every run — and for ever against a server
	// without the extension, or for a buffer of several statements, where
	// the server's answer names only the last one.
	server *dbclient.ServerTiming
}

func newTiming(totalMs uint64, wire dbclient.Wire) Timing {
	return Timing{
		totalMs: totalMs,
		wire:    wire,
		server:  nil,
	}
}

// serverMs is the server's share, and whether that is a measurement or an
// estimate.
//
// The server's own figure when there is one — planning and execution
// together, because both are the server working on this statement and
// the user is asking what the database cost. Otherwise time-to-first-
// row less the round trip, which is an estimate: for a plan that
// streams, the server is still working while the rows cross.
//
// `None` means the engine offered neither, which is every SQLite
// connection — a local file has no link to separate out.
func (t *Timing) serverMs() (ms float64, exact bool, ok bool) {
	if t.server != nil {
		plan := 0.
		if t.server.PlanMs != nil {
			plan = *t.server.PlanMs
		}
		return t.server.ExecMs + plan, t.server.Exact, true
	}
	if t.wire.FirstRowMs == nil {
		return 0, false, false
	}
	firstRow := float64(*t.wire.FirstRowMs)
	var link float64
	if t.wire.LinkMs != nil {
		link = float64(*t.wire.LinkMs)
	}
	return max(firstRow-link, 0.), false, true
}

// lagMs is everything that was not the server: the statement going out,
// the rows coming back, and this process decoding them.
func (t *Timing) lagMs() (float64, bool) {
	server, _, ok := t.serverMs()
	if !ok {
		return 0, false
	}
	return max(float64(t.totalMs)-server, 0.), true
}

// The strip's line. `~` marks a server figure that is an estimate or a
// mean rather than this run's own measured time — an unmarked number
// claims more than the app knows.
//
// serverShare is how much of the wait was the server's, from 0 to 1 — the
// width of the server's part of the bar beside the summary. `None` when
// the summary has no split to draw.
func (t *Timing) serverShare() (float32, bool) {
	server, _, ok := t.serverMs()
	if !ok {
		return 0, false
	}
	if _, ok := t.lagMs(); !ok {
		return 0, false
	}
	if t.totalMs > 0 {
		return clampF32(float32(server)/float32(t.totalMs), 0., 1.), true
	}
	return 0, false
}

func (t *Timing) summary() string {
	total := uikit.FormatMillis(t.totalMs)
	server, exact, ok := t.serverMs()
	lag, lagOk := t.lagMs()
	if !ok || !lagOk {
		return "queried in " + total
	}
	mark := "~"
	if exact {
		mark = ""
	}
	return "queried in " + total + " · server " + mark + uikit.FormatMillisFrac(server) +
		" · lag " + uikit.FormatMillisFrac(lag)
}

// RunPhase is what the run button is at this instant, which is not quite
// what the run is.
//
// `Run` is the state of the statement; this is the state of the *button*,
// and the two part company for the first `RUN_ARM` of a run: the statement
// is out, and the button still says "run". Keeping that difference in a
// type of its own is what stops the verb, the fill, the glyph and the
// keycap from each working it out slightly differently.
type RunPhase int

const (
	// RunPhaseReady: nothing is out, or something is out and has not been
	// out long enough to be worth offering to stop. The comp's `arm`
	// phase is this one with a run breathing under it.
	RunPhaseReady RunPhase = iota
	// RunPhaseStop: a run has proved slow. The button offers to end it.
	RunPhaseStop
	// RunPhaseTerminate: a cancel is with the server. The button offers
	// to close the backend.
	RunPhaseTerminate
)

// String is the derived `Debug`: the variant's name.
func (p RunPhase) String() string {
	switch p {
	case RunPhaseReady:
		return "Ready"
	case RunPhaseStop:
		return "Stop"
	case RunPhaseTerminate:
		return "Terminate"
	}
	return "RunPhase(?)"
}

// phase is the button's state, from the run's state and the clock.
func (r *Run) phase() RunPhase {
	switch r.kind {
	case RunRunning:
		if time.Since(r.flight.started) < runArm {
			return RunPhaseReady
		}
		return RunPhaseStop
	case RunCancelling:
		return RunPhaseTerminate
	default: // RunIdle, RunCancelled
		return RunPhaseReady
	}
}

// arming: is the button in its arming window — a run out, and the button
// not yet offering to stop it? A click does nothing here: the word under
// the pointer says "run", and a run is what this tab already has.
func (r *Run) arming() bool {
	return r.inFlight() && r.phase() == RunPhaseReady
}

func (r *Run) live() *Live {
	switch r.kind {
	case RunRunning, RunCancelling:
		return &r.flight
	default: // RunIdle, RunCancelled
		return nil
	}
}

// inFlight: is the server still working on this run? The timer ticks
// while any tab says yes.
func (r *Run) inFlight() bool {
	return r.live() != nil
}

// HistoryTab is the history screen, as a tab. It is a view of the local
// file rather than of the database, so it holds no generation counter:
// reading it is a synchronous SQLite call, and there is no slow reply to
// outrun.
type HistoryTab struct {
	id uint64
	// Day headings and runs, flattened for `uniform_list`.
	rows []HistoryRow
	// How many of those rows are runs, for the status strip.
	runs int
	// The comp's two chips.
	userOnly   bool
	errorsOnly bool
	// search narrows the runs to the statements that hold its text. Made
	// on first use by `Shell.historySearch`, so a tab built without one
	// still works.
	search *uikit.TextField
	error  *string
	scroll uniformListScroll
}

func (*TableTab) isTab()   {}
func (*QueryTab) isTab()   {}
func (*HistoryTab) isTab() {}

// ID is `Tab::id`.
func (t *TableTab) ID() uint64 { return t.id }

// ID is `Tab::id`.
func (t *QueryTab) ID() uint64 { return t.id }

// ID is `Tab::id`.
func (t *HistoryTab) ID() uint64 { return t.id }

// Title is `Tab::title`.
func (t *TableTab) Title() string { return t.schema + "." + t.table }

// Title is `Tab::title`.
func (t *QueryTab) Title() string { return t.title }

// Title is `Tab::title`.
func (t *HistoryTab) Title() string { return "history" }

// Selection is `Tab::selection`.
func (t *TableTab) Selection() *resultsgrid.Selection { return &t.selection }

// Selection is `Tab::selection`.
func (t *QueryTab) Selection() *resultsgrid.Selection { return &t.selection }

// Selection is `Tab::selection`: a history tab has none.
func (t *HistoryTab) Selection() *resultsgrid.Selection { return nil }

// Marked is the four things every key that moves a selection needs at
// once. A history tab shows a list of runs rather than a result, so it has
// none of them and every one of those keys passes it by.
func (t *TableTab) Marked() *Marked {
	return &Marked{
		data:      t.data,
		selection: &t.selection,
		scroll:    &t.scroll,
	}
}

// Marked is `Tab::marked`; see TableTab.Marked.
func (t *QueryTab) Marked() *Marked {
	return &Marked{
		data:      t.data,
		selection: &t.selection,
		scroll:    &t.scroll,
	}
}

// Marked is `Tab::marked`: a history tab has none.
func (t *HistoryTab) Marked() *Marked { return nil }

// Marked is a tab's result, borrowed field by field, so one key press can
// read the rows, move the selection and scroll to where it went.
type Marked struct {
	data      *resultsgrid.GridData
	selection *resultsgrid.Selection
	scroll    *resultsgrid.GridState
}

func emptyGrid() *resultsgrid.GridData {
	return resultsgrid.EmptyGridData()
}

// Outcome is what a finished run is worth remembering by: the timing and
// the row count on success, the message on failure.
type Outcome struct {
	elapsed *uint64
	rows    *uint64
	// Rows changed, for a statement that answered with a count instead of
	// a result set. `None` where nothing counted — which is every `SELECT`,
	// every table page and every failed run — so the history says nothing
	// there rather than `0 rows changed`.
	affected *uint64
	error    *string
}

// Changed is what a run changed, for a run that answered with a count.
//
// The verb rides along because the count alone cannot be worded: `0` is
// "your `WHERE` matched nothing" for an `UPDATE` and "there was nothing to
// count" for a `CREATE INDEX`, and those are not the same news.
type Changed struct {
	rows uint64
	verb *query.CommandVerb
}

// ddlAt is one shape-changing statement of a run: Rust's `(usize,
// DdlVerb)` — its index in the run, and its verb.
type ddlAt struct {
	ix   int
	verb query.DdlVerb
}

// SchemaReport is what a run did to the **shape** of the database, read
// back off the catalog rather than guessed from the SQL.
//
// A `CREATE`, `ALTER` or `DROP` answers with no rows and no count, so the
// result pane has nothing to say about it — and the thing it changed is
// on screen somewhere else, in the sidebar or in the next query's columns.
// So once such a statement lands the shell reads the catalog again, on the
// tab's own session, and compares it with the one it had: the report is
// that comparison, in the panel under the editor.
//
// **It is read on the session because that is the only place the change
// exists yet.** Inside an open transaction a new table is visible to the
// connection that made it and to nobody else, so a pooled read would
// report nothing changed. `held` says the change is in that state, and
// it is what keeps the new catalog *out of the sidebar*: every other tab
// would see a table that is not there for them. A commit lets it through
// and a rollback throws the report away.
type SchemaReport struct {
	// What differs, once the catalog has been read back. Empty until
	// then, and empty afterwards for a change the model does not carry —
	// a grant, a comment, a function's body.
	delta introspect.Delta
	// The shape-changing statements of the run: their index in the run,
	// for the statistic beside them, and their verb, for the wording.
	statements []ddlAt
	// What those statements took together, the app's own clock.
	ms uint64
	// The change sits inside a transaction the user has not ended.
	held bool
	// The catalog is still being read back.
	pending bool
	// The catalog could not be read back. The panel says so rather than
	// claiming nothing changed.
	error *string
}

// Ran is what a buffer's statements came back with, taken together.
//
// A buffer is several statements and one result line, so the run has to
// answer for all of them at once. **The two halves are separate because
// they are different answers**: a result set is rows to look at, and a
// count is rows that changed and are not on screen. One buffer can do
// both — `UPDATE …; SELECT …;` — and the line says both.
type Ran struct {
	// The last result that had columns: the one the grid paints. With no
	// columns there is no result set in the buffer at all, and nothing to
	// paint a grid for.
	result dbclient.QueryResult
	// Rows changed, summed over the statements that answered with a count
	// rather than a result set.
	//
	// A `SELECT`'s own tag count is never added: the server counts one for
	// a query too, and adding it would report the rows already on screen a
	// second time as though they had been written.
	affected uint64
	// How many statements answered with a count rather than a result set.
	// It is what says whether `affected == 0` is worth a sentence: no
	// counted statement means nothing changed nothing, while one that
	// counted zero means a `WHERE` matched nothing.
	counted int
	// The verb those statements share, for the wording alone. `None` where
	// they disagree, or where the verb could not be read — a mixed buffer
	// takes the generic word rather than the last statement's.
	verb *query.CommandVerb
	// The whole wait, the app's own clock around the run.
	elapsedMs uint64
	// How many statements were sent.
	ran int
	// See [Tally]'s field of the same name.
	boundaryTag string
	// What the server said beside every statement of the run, in order.
	messages []string
}

// RunOutcome is what a run hands back: the tab's session, and how the
// statements went.
//
// The two are separate because they fail separately. A statement that
// errors leaves the session perfectly good — and still holding whatever
// the user set on it — so the session comes back either way, and is
// `None` only when opening it is what failed.
type RunOutcome struct {
	session dbclient.Session
	// Go port: Rust's `Result<Ran, String>` — `result` on success, nil
	// with `err` saying why on failure.
	result *Ran
	err    string
	// Whether the run opened a transaction of its own, for a tab in
	// [`TxMode::Manual`]. It is reported apart from the result because it
	// is true on both paths: a statement that fails inside a transaction
	// leaves it open, and that is the state most worth reporting.
	began bool
	// The shape-changing statements that landed, on either path: in auto
	// mode a `CREATE` before the statement that failed has committed all
	// the same, and the sidebar is stale until it is read again.
	shape Shape
}

// Shape is the `CREATE`, `ALTER`, `DROP` and `TRUNCATE` statements a run
// has sent and had answered, with their index in the run and what they
// cost.
type Shape struct {
	ddl []ddlAt
	ms  uint64
}

// abandoned: the shell went away mid-run. Nothing will read this, and the
// pool it belonged to is going with it.
func abandonedRunOutcome() RunOutcome {
	return RunOutcome{
		session: nil,
		result:  nil,
		err:     "the workspace closed",
		began:   false,
		shape:   Shape{},
	}
}

// What a run says for itself when ⌘. reached it before it had left. The
// tab paints CANCELLED, as it does for a run the server gave up, because
// it is the same answer to the same question.
const calledOff = "the run was called off before it started"

// What a run says for itself when ⌘. reached it between two statements.
// The cancel the button sent found the backend idle, so it stopped
// nothing — this is what stops the rest of the buffer instead. The tab
// paints CANCELLED for it, as it does for a statement the server gave
// up: it is the same answer to the same question.
const stoppedBetween = "the run was stopped between statements"

// What the statistic beside a statement says when it never left, by why.
// The comp's `not run · statement 3 failed` is the shape; these are the
// other reasons a statement is left with the dash.
const (
	metaNoSession = "not run · no session"
	metaNoBegin   = "not run · begin failed"
	metaStopped   = "not run · stopped"
	metaRunning   = "running…"
)

// Said when every session is busy or holding a transaction and this tab
// wanted one too. It names the two ways out, because the app cannot pick
// either of them on the user's behalf: both lose something.
const noSessionLeft = "every connection is in use — close a tab, or end a transaction, and run again"

// Said when a commit or a rollback is asked for while a run is still out.
// A session is one connection and the run has it, so the boundary would
// have to queue behind the very statement the user may want to give up.
// It names the way out rather than waiting.
const runHoldsTheSession = "this tab is still running a statement — ⌘. stops it, then commit or roll back"

// tallyVerb is the inner half of Rust's `Option<Option<CommandVerb>>`:
// a verb the counted statements agree on, or nil where it could not be
// read.
type tallyVerb struct {
	verb *query.CommandVerb
}

// Tally is what a run has added up over the statements it has sent so
// far.
//
// The loop that feeds it is on the GPUI thread now, one statement at a
// time, so the running totals cannot live in a `for` loop inside one
// tokio task any more. Every rule about what is counted is here, in one
// place, rather than spread over that loop.
type Tally struct {
	// The last result that had columns: the one the grid paints, so a
	// trailing `create table` does not blank the grid the `SELECT` before
	// it filled.
	last     dbclient.QueryResult
	ran      int
	affected uint64
	counted  int
	// The verb the counted statements agree on. `Some(None)` is one that
	// could not be read; the outer `None` is "nothing counted yet", which
	// is why this is not simply an `Option<CommandVerb>`.
	//
	// Go port: nil is the outer `None`; a tallyVerb with a nil verb is
	// `Some(None)`.
	verb *tallyVerb
	// The shape-changing statements so far, handed back on every path.
	shape Shape
	// The command tag the last transaction boundary (`COMMIT`, `END`,
	// `ROLLBACK`…) of the buffer answered with. Postgres answers the
	// `COMMIT` of a failed transaction with the tag `ROLLBACK` and no
	// error, and this is the only place that is said.
	boundaryTag string
	// What the server said beside the statements so far.
	messages []string
}

// CatalogRow is one row of the sidebar's tree, flattened so the list can
// measure one row height and lay out only what is on screen.
//
// Every row sits at a `level` of the tree, and one rule places them all:
// the chevron slot of a row at level n starts at `chevron_at(n)`, and
// what follows it at `content_at(n)`. So a relation's glyph sits under
// its section's label, and a column's under the relation's name — the
// tree reads off the left edge however deep it goes.
//
// One of CatalogRowSchema, CatalogRowSection, CatalogRowRelation or
// CatalogRowLeaf.
type CatalogRow interface {
	isCatalogRow()
}

// CatalogRowSchema is a schema, and how many things it holds.
type CatalogRowSchema struct {
	key   string
	label string
	count int
	open  bool
}

// CatalogRowSection is a folder: `Tables`, `Views`, `Sequences`,
// `Routines` or `Types` inside a schema at level 1, or `Indexes`,
// `Foreign keys`, `Constraints` or `Triggers` inside an open relation at
// level 3.
type CatalogRowSection struct {
	key   string
	label string
	count int
	open  bool
	level int
}

// CatalogRowRelation is `CatalogRow::Relation`.
type CatalogRowRelation struct {
	schema string
	name   string
	kind   introspect.TableKind
	// Whether its columns and folders are drawn under it.
	open bool
}

// CatalogRowLeaf is something the tree lists and does not open: a column,
// an index, a routine. It says what it is and nothing happens on a click —
// there is no tab for a column to open into.
type CatalogRowLeaf struct {
	name   string
	detail string
	mark   Mark
	level  int
}

func (CatalogRowSchema) isCatalogRow()   {}
func (CatalogRowSection) isCatalogRow()  {}
func (CatalogRowRelation) isCatalogRow() {}
func (CatalogRowLeaf) isCatalogRow()     {}

// Mark is what a leaf is, which is what its glyph says.
type Mark int

const (
	MarkColumn Mark = iota
	// MarkKey is a column of the primary key.
	MarkKey
	MarkIndex
	MarkForeignKey
	MarkConstraint
	MarkTrigger
	MarkSequence
	MarkRoutine
	MarkType
	// MarkPartition is a partition of a partitioned table.
	MarkPartition
)

// How far one level of the tree steps in: a chevron and the gap after it,
// which is also a glyph and the gap after *it* — `TREE_GLYPH` and
// `TREE_GAP` add up to this, or a column's glyph would not sit under its
// relation's name.
const treeStep float32 = 16.

// A row's glyph (a chevron, a relation's icon, a leaf's mark) is this wide.
const treeGlyph float32 = 11.

// ...and this far from what follows it.
const treeGap float32 = treeStep - treeGlyph

// chevronAt is where the chevron of a row at `level` starts.
func chevronAt(level int) float32 {
	return 6. + treeStep*float32(level)
}

// contentAt is where what follows that chevron starts — a label, or a
// leaf's glyph.
func contentAt(level int) float32 {
	return chevronAt(level) + treeStep
}

// Group is one schema of the sidebar tree, with what it holds split into
// the sections under it. Built once per catalog, and flattened into rows
// again whenever something opens or the filter changes.
type Group struct {
	// Identity of the schema in the open set: its own name.
	key      string
	label    string
	schema   string
	sections []Section
	// The catalog the group was read from, and where its schema sits in
	// it, so a relation's columns can be read when it is first unfolded.
	catalog  *introspect.Catalog
	schemaIx int
}

// Section is one folder of a schema: its tables, its views, its
// sequences, its routines or its types.
type Section struct {
	// Identity of the section in the closed set. A schema name cannot
	// hold a tab, so the two parts cannot run together into another
	// section's key.
	key     string
	label   string
	entries []Entry
}

// Entry is one name a section lists, which is what the filter matches.
// One of *EntryRelation or EntryObject.
type Entry interface {
	// Name is `Entry::name`.
	Name() string
	isEntry()
}

// EntryRelation is a relation: it opens a tab on a click and folds out
// under its chevron.
type EntryRelation struct {
	name string
	kind introspect.TableKind
	// Identity of the relation in the open set: `schema\trelation`.
	key string
	// Where the relation sits in its schema's `tables`.
	tableIx int
	// What it folds out into, built the first time it is unfolded.
	// A catalog of three thousand tables holds some eighty thousand
	// columns and indexes, and building a leaf for each on the GPUI
	// thread every time a catalog lands — after every committed DDL —
	// would pay for rows almost nobody opens.
	//
	// Go port: Rust's `OnceCell<Parts>`; nil until it is built, which is
	// why the entry is held by pointer.
	parts *Parts
}

// EntryObject is `Entry::Object(Leaf)`.
type EntryObject struct {
	leaf Leaf
}

func (*EntryRelation) isEntry() {}
func (EntryObject) isEntry()    {}

func (e *EntryRelation) Name() string { return e.name }

func (e EntryObject) Name() string { return e.leaf.name }

// Leaf is a name the tree lists and does not open, before it has a level.
type Leaf struct {
	name   string
	detail string
	mark   Mark
}

func newLeaf(name, detail string, mark Mark) Leaf {
	return Leaf{
		name:   name,
		detail: detail,
		mark:   mark,
	}
}

func (l Leaf) row(level int) CatalogRow {
	return CatalogRowLeaf{
		name:   l.name,
		detail: l.detail,
		mark:   l.mark,
		level:  level,
	}
}

// Parts is what an open relation shows under it: its columns, straight
// away, and then a folder for each sort of object it carries.
type Parts struct {
	columns []Leaf
	folders []Folder
}

type Folder struct {
	label  string
	leaves []Leaf
}

// ConfirmCopy is what the close confirmation says: title, body, the
// keeping answer, the ending one.
//
// Pure, so the wording is testable without a window — and so the three
// ways out cannot drift into saying three unrelated things. The body
// names what happens on the *server*, because that is what the user
// cannot see and cannot undo.
//
// Go port: Rust's `(String, Vec<String>, &'static str, &'static str)`.
type ConfirmCopy struct {
	Title string
	Body  []string
	Keep  string
	Act   string
}

// SessionState is one open session, as the sweep and the cap need to see
// it. Kept apart from the tab so the policy below is a function of plain
// values, and can be argued with in a test rather than in a running window.
type SessionState struct {
	tabID         uint64
	running       bool
	inTransaction bool
	idle          time.Duration
}

// TxTally is what has landed inside the open transaction.
//
// The two counts live and die together. They were two fields once, and
// the button's commit or rollback reset the statements and forgot the
// rows, so the next transaction started with the last one's rows already
// in it: an `UPDATE` rolled back and run again read as twice the rows.
// A transaction that ends puts back `TxTally::default()`, which cannot
// forget half of it.
type TxTally struct {
	// Statements that are not boundaries.
	statements int
	// Rows those statements changed, as the server counted them.
	//
	// The bar says this beside the statement count, because "what have I
	// touched that nobody else can see yet" is the question an open
	// transaction raises. It counts only the statements that answered with
	// a count: a `SELECT` inside the transaction changed nothing.
	affected uint64
}

// add counts one run's statements into the transaction it landed in.
func (t *TxTally) add(statements int, affected uint64) {
	t.statements += statements
	t.affected += affected
}

// TxState is everything the transaction bar reads, as plain values.
//
// Kept apart from the tab so the wording below is a function of numbers
// and flags, and can be argued with in a test rather than in a running
// window — the reason `confirm_copy` and the session policy are written
// the same way.
type TxState struct {
	mode dbclient.TxMode
	// Whether a transaction is open, as the server last said.
	open bool
	// How the last one ended, when nothing is open.
	done *dbclient.TxEnd
	// Statements that have landed inside the open transaction.
	statements int
	// Rows those statements changed, as the server counted them.
	affected uint64
	// A run is out on this tab's session.
	running bool
	// A commit or a rollback is out.
	ending bool
}

// Sign is which way a row of the report goes.
type Sign int

const (
	SignPlus Sign = iota
	SignMinus
	SignTilde
)
