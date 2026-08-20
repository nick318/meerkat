//! Root view: breadcrumb bar | sidebar + tab pane | status strip.
//!
//! The shell owns the session: the connection, the catalog it introspected,
//! and the open tabs. Every database call goes to the tokio runtime through
//! `gpui_tokio` and comes back as an update on this entity, so the window
//! keeps painting while a query runs.
//!
//! Stale replies are dropped by generation: each tab counts its requests,
//! and a reply that does not carry the tab's current generation is thrown
//! away. Without that, a slow first page would overwrite a fast second one.

use db_client::{
    Connection, Profile, QueryResult, RunId, ServerTiming, Session, Stop, TxEnd, TxMode, Wire,
};
use db_postgres::{Label, PostgresConnection};
use gpui::{
    Animation, AnimationExt, AnyElement, App, BoxShadow, ClipboardItem, Context, Div, ElementId,
    Entity, EventEmitter, FocusHandle, Focusable, FontWeight, Hsla, Pixels, ScrollStrategy,
    SharedString, Stateful, Subscription, UniformListScrollHandle, Window, actions, deferred, div,
    prelude::*, px, uniform_list,
};
use introspect::{Catalog, Table, TableKind};
use query::TxVerb;
use results_grid::{
    Cell, Extent, Grid, GridData, GridState, Hit, Selection, Step, clipboard_text, find_columns,
};
use sql_editor::{Kind, Name, SqlEditor, Vocabulary};
use std::collections::HashSet;
use std::rc::Rc;
use std::sync::Arc;
use std::time::{Duration, Instant};
use storage::{HistoryFilter, NewRun, QueryRun, RunSource, SavedTab, SavedTabs, Store};
use theme::{FONT_FAMILY, ThemeColors, theme};
use ui::scrollbar::{self, DragState, Scrollbar};
use ui::{
    TextField, TextFieldEvent, card, format_count, format_millis, format_millis_frac,
    format_seconds, lock_glyph, meerkat_mark, play_glyph, search_glyph, section_label, status_dot,
    stop_glyph, table_glyph,
};

use crate::connections::unix_now;
use crate::env::Env;
use crate::history::{self, HistoryRow, OnOpenRun};
use crate::palette::{self, OnPick, Pick, Scope};
use crate::sql::{PAGE_SIZE, browse_query, page_query};

actions!(
    meerkat,
    [
        RunQuery,
        StopQuery,
        NewQuery,
        CloseTab,
        Refresh,
        PrevPage,
        NextPage,
        ShowHistory,
        NextTab,
        PrevTab,
        ConfirmClose,
        CancelClose,
        SelectUp,
        SelectDown,
        SelectLeft,
        SelectRight,
        ExtendUp,
        ExtendDown,
        ExtendLeft,
        ExtendRight,
        SelectRowStart,
        SelectRowEnd,
        SelectFirstRow,
        SelectLastRow,
        SelectAll,
        CopySelection,
        TogglePick,
        ClearSelection,
        FindColumn,
        ColumnPrev,
        ColumnNext,
        FilterCatalog,
        CatalogPrev,
        CatalogNext,
        PeekValue,
        ClosePeek,
        CopyPeek,
        CommitTransaction,
        RollbackTransaction
    ]
);

/// The results grid's own key context. The arrows, ⇧ with them, ⌘A, ⌘C,
/// space, ⌘I and ⎋ are bound here, never to the shell: the shell's context
/// is an ancestor of the SQL editor's, so a key bound there is taken from
/// the editor before it can be typed.
pub const GRID_KEY_CONTEXT: &str = "ResultGrid";

/// The confirmation's own key context. Scoped, like the palette's: while
/// the dialog is up it is the deepest match, so ⌘⏎ answers it rather than
/// running a query behind it.
pub const CONFIRM_KEY_CONTEXT: &str = "Confirm";

/// The column-find popover's own key context. Scoped, like the palette's
/// and the confirmation's: while the popover is up it is the deepest match,
/// so ↑↓ walk what the search found rather than the result behind it.
pub const COLUMN_FIND_KEY_CONTEXT: &str = "ColumnFind";

/// Key bindings for finding a column of the result on screen.
///
/// ⌘J is the **shell's**, not the popover's: it has to open the thing as
/// well as close it, and it has to work while the SQL editor holds the
/// focus. ↑↓ belong to the popover, or they would be taken from the grid
/// while nothing is open.
///
/// ⏎ and ⎋ are bound nowhere here. The search line is a `TextField`, which
/// reports both as events of its own, so the popover answers them the way
/// the palette does.
pub fn column_find_key_bindings() -> Vec<gpui::KeyBinding> {
    vec![
        gpui::KeyBinding::new("cmd-j", FindColumn, Some("Shell")),
        gpui::KeyBinding::new("up", ColumnPrev, Some(COLUMN_FIND_KEY_CONTEXT)),
        gpui::KeyBinding::new("down", ColumnNext, Some(COLUMN_FIND_KEY_CONTEXT)),
    ]
}

/// The sidebar filter's own key context. Scoped to that line, like the
/// palette's and the column find's: ↑↓ walk the names the filter left
/// while the line has the focus, and mean what they mean everywhere else
/// as soon as it does not.
pub const CATALOG_FILTER_KEY_CONTEXT: &str = "CatalogFilter";

/// Key bindings for the sidebar's filter.
///
/// ⌘E is the **shell's**, like ⌘J and for the same reason: it has to reach
/// the line from wherever the focus is — the SQL editor, the grid — which is
/// the whole of what it is for. It only puts the focus there and marks what
/// is already typed; it is not a toggle, because the line is on screen
/// either way and a second press would have to guess where to hand the
/// focus back to. ⎋ is the way out, as it was.
///
/// A filter that answers with five tables is a list the user has to reach
/// for the mouse to use, and the name they want is rarely the first one.
/// ↓ steps into that list, ↑ steps back out of it, and ⏎ opens whichever
/// name the cursor is on. Those two are the line's own, or they would be
/// taken from the results grid whenever nothing is typed.
///
/// ⏎ and ⎋ are bound nowhere here. The filter is a `TextField`, which
/// reports both as events of its own, so the sidebar answers them the way
/// the palette and the column find do.
pub fn catalog_filter_key_bindings() -> Vec<gpui::KeyBinding> {
    vec![
        gpui::KeyBinding::new("cmd-e", FilterCatalog, Some("Shell")),
        gpui::KeyBinding::new("up", CatalogPrev, Some(CATALOG_FILTER_KEY_CONTEXT)),
        gpui::KeyBinding::new("down", CatalogNext, Some(CATALOG_FILTER_KEY_CONTEXT)),
    ]
}

/// The value peek's own key context. Scoped, like every other overlay's:
/// while the card is up ⏎ closes it, where ⏎ in the grid opened it.
pub const PEEK_KEY_CONTEXT: &str = "ValuePeek";

/// Key bindings for reading a whole value.
///
/// A lane is capped at [`results_grid`]'s `MAX_COLUMN_WIDTH`, so a long
/// value truncates on screen. ⏎ over the cursor's cell asks for the rest of
/// it; a double click in the grid asks for the same thing with the mouse.
///
/// ⌘C is bound here as well as in the grid, and means something narrower:
/// **this value**, not the selection. The card is about one cell, so the
/// key that copies from it copies one cell.
pub fn peek_key_bindings() -> Vec<gpui::KeyBinding> {
    vec![
        gpui::KeyBinding::new("enter", PeekValue, Some(GRID_KEY_CONTEXT)),
        gpui::KeyBinding::new("enter", ClosePeek, Some(PEEK_KEY_CONTEXT)),
        gpui::KeyBinding::new("escape", ClosePeek, Some(PEEK_KEY_CONTEXT)),
        gpui::KeyBinding::new("cmd-c", CopyPeek, Some(PEEK_KEY_CONTEXT)),
    ]
}

/// Key bindings for the close confirmation.
///
/// ⏎ and ⎋ both keep what is open. The destructive answer takes ⌘⏎ — the
/// deliberate chord, the same one that means "do it" everywhere else in
/// the app. A dialog that ends server work on a stray ⏎ is a dialog the
/// user learns to fear.
pub fn confirm_key_bindings() -> Vec<gpui::KeyBinding> {
    vec![
        gpui::KeyBinding::new("cmd-enter", ConfirmClose, Some(CONFIRM_KEY_CONTEXT)),
        gpui::KeyBinding::new("enter", CancelClose, Some(CONFIRM_KEY_CONTEXT)),
        gpui::KeyBinding::new("escape", CancelClose, Some(CONFIRM_KEY_CONTEXT)),
    ]
}

const SIDEBAR_WIDTH: f32 = 246.;
const EDITOR_HEIGHT: f32 = 250.;
/// Every sidebar row is this tall, headers included: `uniform_list` needs
/// one height to measure, and the design's rows are already within a
/// pixel of each other.
const CATALOG_ROW_HEIGHT: f32 = 24.;
/// The sidebar's filter line, a size under the relation names it filters.
const FILTER_FONT_SIZE: f32 = 11.;
/// How far back the history screen looks, in days. The comp's header says
/// "last 7 days", and that is the window the list actually reads.
const HISTORY_DAYS: i64 = 7;
/// What a tab says when it wanted the server before there was one. The
/// cached catalog lets a session open tabs while the connect is still in
/// flight, so this is reachable on a first keystroke.
const NOT_CONNECTED: &str = "not connected yet";
/// How often the toolbar's run timer repaints. The design prints tenths of
/// a second, so this is the slowest tick that still reads as a clock.
const TIMER_TICK: Duration = Duration::from_millis(100);
/// How long a run has to last before the timer appears. Most statements
/// come back in tens of milliseconds, and a pill that flashed up and out
/// again on every one of them would be noise: the timer is there to say a
/// query is taking a while, so it says nothing until one is.
const TIMER_DELAY: Duration = Duration::from_secs(1);
/// How long a run is given to answer before the button offers to stop it.
///
/// **The verb must not flicker.** Most statements come back in tens of
/// milliseconds, and a button that read "run · stop · run" on every one of
/// them would be a button nobody could aim at. So the first `RUN_ARM` of a
/// run keeps the word "run" — the fill is already saying a run is out — and
/// the escalation to "stop" happens only for a run that has proved slow —
/// which
/// is the comp's own arming window, and its own comment: *"stop" only
/// appears once the query has proven itself slow*.
///
/// The comp cheats there, because a simulation knows how long its query
/// will take and can decide during the window that this one will be quick.
/// Nothing here can know that, so the honest reading of the same rule is
/// the elapsed time alone. It paints the same screen: a statement that
/// answers inside the window never shows a stop button at all.
const RUN_ARM: Duration = Duration::from_millis(420);
/// **The run button moves in one way only: its own fill, breathing.**
///
/// The comp animates a run with the vocabulary of a progress widget — a
/// front sweeping the button, then an indeterminate band crossing it on a
/// loop, then a ring blooming out of the border when the rows land. Those
/// are Material's marks, and three of them are a lot of movement to put on
/// a 27-pixel button that sits beside a paper-coloured toolbar all day. They
/// also each carry a shape, and a shape crossing a button asks to be
/// watched.
///
/// A tone does not. So the whole of the animation here is the fill mixing a
/// little of the button's own ink into itself and back out again: nothing
/// travels, nothing has an edge, and the button says "working" the way a
/// held breath does. `RUN_BREATH` is one full cycle and `RUN_BREATH_DEPTH`
/// how far it goes — far enough to be seen out of the corner of an eye,
/// and no further, because a button that pulses hard is a button that
/// interrupts.
const RUN_BREATH: Duration = Duration::from_millis(2600);
const RUN_BREATH_DEPTH: f32 = 0.12;
/// The last exhale, when the rows land: the same mix, easing out to
/// nothing. It is deliberately the *same* mark as the breath rather than a
/// flash of its own, so a run reads as one gesture that starts and stops —
/// and it is what tells the user a statement too quick to breathe has been
/// answered at all.
const RUN_SETTLE: Duration = Duration::from_millis(320);
const RUN_SETTLE_DEPTH: f32 = 0.1;
/// The press, which is a tone and **not** a movement: the same mix, the
/// other way, toward the ink of the app rather than the button's own. A
/// button that drops a pixel under the pointer is the elevation idiom this
/// one is deliberately not in — and nothing shifting means nothing to
/// track.
const RUN_PRESS_DEPTH: f32 = 0.08;
/// The label's width, held still across the verbs so that the button does
/// not resize under the pointer when a run turns out to be slow. "run" and
/// "stop" share one width; "terminate" is three times the word and gets its
/// own.
///
/// **Each one is sized by the longest word it serves, not by the first.**
/// The font is monospaced at 0.6 em an advance, so an 11px label costs
/// 6.6px a character: "stop" needs 26.4px and "terminate" 59.4px. A width
/// that fits "run" clips the "p" off "stop" — the button's own label is the
/// one thing on it that must never be cut, because the word *is* what the
/// button is offering to do. The couple of pixels over are the rounding.
const RUN_LABEL_WIDTH: f32 = 28.;
const TERMINATE_LABEL_WIDTH: f32 = 62.;
/// How long a tab's session may sit unused before it is handed back, and
/// how often the sweep looks. Ten minutes is long enough that a session is
/// still there when the user comes back from a meeting having left a tab
/// mid-thought, and short enough that a laptop left open overnight is not
/// holding eight backends on a shared server.
const IDLE_SESSION: Duration = Duration::from_secs(600);
const IDLE_TICK: Duration = Duration::from_secs(60);
/// The close confirmation's geometry. Narrower than the palette and
/// higher up the window: it is a sentence and two answers, not a list.
const CONFIRM_WIDTH: f32 = 420.;
const CONFIRM_TOP_MARGIN: f32 = 140.;
/// The value peek's geometry: wider than the close dialog, because what it
/// holds is a value rather than a sentence, and no taller than this before
/// the value scrolls inside it.
const PEEK_WIDTH: f32 = 640.;
const PEEK_TOP_MARGIN: f32 = 120.;
const PEEK_MAX_HEIGHT: f32 = 360.;
/// How much of a value the card paints. `db_client::MAX_CELL_BYTES` lets a
/// megabyte of text into one cell, and laying a megabyte of wrapped text out
/// on the GPUI thread would freeze the window — so the card shows the first
/// of it and says how much there is. ⌘C still copies the whole value: the
/// bound is on what is *painted*, as the row cap is on what is *held*.
const PEEK_CHARS: usize = 4_000;
/// The column-find popover's geometry, from the comp: 290px wide, hanging
/// straight under the button that opens it, with a list that grows to about
/// nine rows and then scrolls.
const COLUMN_FIND_WIDTH: f32 = 290.;
const COLUMN_FIND_TOP: f32 = 32.;
const COLUMN_ROW_HEIGHT: f32 = 24.;
const COLUMN_LIST_MAX_HEIGHT: f32 = 236.;
/// The search line inside it, a size up from the list it filters.
const COLUMN_FIND_FONT_SIZE: f32 = 12.;
/// The tab strip is one row tall, as the comp draws it.
const TAB_STRIP_HEIGHT: f32 = 34.;
/// A tab is never squeezed below this, so the strip reads as a row of
/// tabs rather than a row of words of different lengths.
const TAB_MIN_WIDTH: f32 = 116.;
/// ...and never wider than this, so one long table name cannot take the
/// strip. The title truncates at that point.
const TAB_MAX_WIDTH: f32 = 220.;

pub struct Shell {
    focus_handle: FocusHandle,
    /// The focus the results grid takes when the user clicks into it, and
    /// the reason the grid's keys are scoped rather than global.
    ///
    /// A query tab's editor lives *inside* the shell's element, so a key
    /// bound to the shell's context is matched before the keystroke can
    /// reach the editor's text input — a bare `space` bound to "tick this
    /// row" would eat the spaces out of the user's SQL. The grid keys are
    /// bound to `GRID_KEY_CONTEXT` instead, which is only in the context
    /// stack while this handle holds the focus.
    grid_focus: FocusHandle,
    status: Status,
    connection: Option<Arc<dyn Connection>>,
    catalog: Option<Catalog>,
    /// True while the introspection runs. It is a second round trip after
    /// the connect, so a session is connected — and can run a query —
    /// before there is a catalog to paint.
    catalog_loading: bool,
    /// Why the last introspection failed, when one did. The connection is
    /// still usable then: a query tab works without a catalog.
    catalog_error: Option<String>,
    /// The sidebar's groups, built once when the catalog lands: a schema's
    /// tables, then its views.
    catalog_groups: Rc<Vec<Group>>,
    /// Those groups flattened for `uniform_list`, as the open set and the
    /// filter leave them. Rebuilt when either changes, never per frame.
    catalog_rows: Rc<Vec<CatalogRow>>,
    /// Which schemas are open. Empty to start: a catalog of thousands of
    /// relations is a wall of names when every schema is expanded, so the
    /// sidebar opens closed and the user opens what they want.
    open_schemas: HashSet<SharedString>,
    /// Which `TABLES` / `VIEWS` sections are closed. This one is the other
    /// way round because a schema the user just opened was opened to see
    /// what is in it: sections start open, and closing one is the choice
    /// worth remembering.
    closed_sections: HashSet<SharedString>,
    /// The sidebar's filter line. It narrows the rows already in hand — no
    /// query goes out for it.
    catalog_filter: Entity<TextField>,
    /// Which row of the list the arrow keys are on, as an index into
    /// `catalog_rows`. It only ever names a relation, and `None` says the
    /// cursor is still on the line itself — where ⏎ and ⇥ read the *first*
    /// match instead, as they did before there was a cursor at all.
    catalog_selected: Option<usize>,
    /// Where the sidebar is scrolled, kept across re-renders.
    catalog_scroll: UniformListScrollHandle,
    /// Which of the sidebar's scrollbars is being dragged, if any.
    catalog_drag: DragState,
    /// How many relations the catalog holds, for the sidebar footer.
    relation_total: usize,
    /// Every name in the catalog, for the editor's colouring.
    vocabulary: Arc<Vocabulary>,
    label: Option<Label>,
    /// What the user called this connection, from the profile. It names the
    /// session everywhere the shell names it, because the name is what the
    /// user chose to recognise the connection by — two profiles often point
    /// at the same database name on different hosts. A command-line URL has
    /// no profile and so no name, and falls back to the database.
    name: Option<SharedString>,
    /// Whether this session refuses to write. The connection asked the
    /// server for it, so this field only *reports* it — the top bar's mark
    /// and the two lines that name the mode read it. A command-line URL
    /// carries no setting and is read-only.
    read_only: bool,
    /// The transaction mode a new query tab on this connection opens on,
    /// from the profile. A tab may then be switched on its own; nothing a
    /// tab does writes back here. A command-line URL carries no setting,
    /// and auto is what a connection does with nothing asked of it.
    tx_default: TxMode,
    tabs: Vec<Tab>,
    active: usize,
    next_id: u64,
    /// The local file that remembers what this session ran. `None` when
    /// it could not be opened; the history screen then says so instead of
    /// showing an empty list.
    store: Option<Store>,
    store_error: Option<String>,
    /// Which connection the history belongs to: a profile id, or a key
    /// built from the URL the app was started with. A password never
    /// reaches it.
    scope: String,
    /// The environment tag the profile wears, read once on the way in:
    /// it colors the window frame, so a prod session can never be
    /// mistaken for a dev one. A command-line URL has no tag.
    env: Option<Env>,
    /// The ⌘K palette, while it is open. It lives on the shell rather than
    /// in a window of its own, so closing it cannot leave the workspace
    /// without focus.
    palette: Option<Palette>,
    /// The column-find popover, while it is open. It searches the result the
    /// active tab holds, so it lives beside the palette rather than on a
    /// tab: one is open at a time, over whichever result is on screen.
    column_find: Option<ColumnFind>,
    /// The cell whose whole value is being read, while the card is up.
    peek: Option<Peek>,
    /// The close the user is being asked about, while the dialog is up.
    confirm: Option<Confirm>,
    /// True while a repaint loop is running for the query timers. Runs come
    /// and go in several tabs at once; the loop belongs to the window.
    timing: bool,
    /// Whether the run button is being held down. It is the mouse's own
    /// state between one frame and the next rather than anything about the
    /// run, which is why it lives here beside the palette and not on a tab —
    /// the same reason the grid keeps its hovered row on `GridState`. One
    /// button is on screen at a time, so one flag covers it.
    run_pressed: bool,
    /// True while the idle-session sweep is running. One loop for the
    /// window, and only while there is a session to sweep.
    sweeping: bool,
    /// True while the session reopens the tabs it was left with. The
    /// restore builds the strip one tab at a time, and every one of those
    /// steps would otherwise write a half-built strip back over the saved
    /// one — a restore that stopped halfway would lose the rest.
    restoring: bool,
    /// Held for as long as the shell lives, so the filter line keeps
    /// reporting what was typed into it.
    _subscriptions: Vec<Subscription>,
}

/// The open palette: what was typed, what it found, and where the
/// selection is.
struct Palette {
    query: Entity<TextField>,
    /// The scope chip that is lit. A `t:` typed into the line overrides it
    /// for as long as it is there.
    chip: Scope,
    /// This connection's runs, read from the local file once when the
    /// palette opened. Searching them again per keystroke is a filter over
    /// this vector, not another read.
    runs: Rc<Vec<QueryRun>>,
    rows: Rc<Vec<palette::Row>>,
    /// How many results matched before the section caps cut the list.
    matches: usize,
    /// Index into `rows` of the selected result. It always points at a
    /// result, never at a heading.
    selected: usize,
    scroll: UniformListScrollHandle,
    _subscriptions: Vec<Subscription>,
}

/// The open column-find popover: one line searched against the names of
/// the columns the result on screen came back with.
///
/// **What it found is not kept here.** A run or a page turn replaces the
/// result under the popover, and a list of lane indices remembered from the
/// result before would point at lanes that no longer exist — so the matches
/// are worked out from the line and the result in hand, every time they are
/// wanted. What is kept is only where the user has walked to.
struct ColumnFind {
    /// The tab whose result is being searched. The popover is a control on
    /// one result, so switching tabs closes it rather than carrying it over
    /// to a result it was never opened on.
    tab: u64,
    query: Entity<TextField>,
    /// Index into the **matches**, not into the result's columns: the list
    /// is what ↑↓ walk. It is clamped where it is read, because the list can
    /// grow shorter under it.
    selected: usize,
    scroll: UniformListScrollHandle,
    _subscriptions: Vec<Subscription>,
}

/// The cell whose whole value is on screen.
///
/// A lane is capped, so a value longer than about forty characters truncates
/// in the grid — and the question that raises is "what is in this cell",
/// which is not the same question as "how wide should this column be". So
/// the answer is a card over one cell rather than a wider lane: widening a
/// lane to two thousand pixels only turns reading into panning.
///
/// It holds **where** the value is, never the value itself. A run or a page
/// turn replaces the rows under it, and a string copied when the card opened
/// would go on saying what used to be there.
struct Peek {
    tab: u64,
    cell: Cell,
    /// The card holds the focus while it is up, so its key context is the
    /// deepest one and ⏎ closes rather than opening another.
    focus: FocusHandle,
}

/// One "stop this run" request on its way to the server.
type StopTask = gpui::Task<Result<anyhow::Result<bool>, gpui_tokio::JoinError>>;

/// A close the user has to agree to, because it would end work the server
/// is still doing.
///
/// Closing the socket is not a cancel: Postgres notices a client is gone
/// when the backend next writes, which a long `SELECT` may not do for
/// minutes. So a close that abandons a run leaves the server working for
/// a window that is not there any more — and the user is never told.
struct Confirm {
    what: Close,
    /// The tabs whose runs this close would stop, by name.
    running: Vec<SharedString>,
    /// The tabs whose open transactions this close would roll back. One of
    /// the two lists is always filled: with nothing to lose, no question.
    open: Vec<SharedString>,
    /// The dialog holds the focus while it is up, so its key context is
    /// the deepest one and answers the keystroke.
    focus: FocusHandle,
}

/// Which way out is being confirmed.
///
/// There are four ways out of a session and only one of them is ⌘W, so a
/// guard wired to the tab alone would be a lie in the other three: the
/// two here that drop every tab at once, and the × on the tab strip.
#[derive(Clone, Copy)]
pub enum Close {
    /// ⌘W, or the × on a tab.
    Tab(u64),
    /// "‹ connections". The shell is dropped, and every tab with it.
    Shell,
    /// ⌘Q, or the window's close button.
    Window,
}

/// What the workspace was opened on: a URL from the command line, or a
/// connection saved on the connections screen.
#[derive(Clone)]
pub enum Target {
    Url(String),
    Profile(Profile),
}

/// What the workspace tells the window around it.
pub enum ShellEvent {
    /// The user asked for the connections screen back.
    Close,
}

enum Status {
    Connecting(String),
    Connected,
    Failed(String),
}

enum Tab {
    Table(TableTab),
    Query(QueryTab),
    History(HistoryTab),
}

struct TableTab {
    id: u64,
    schema: String,
    table: String,
    kind: TableKind,
    data: Rc<GridData>,
    page: usize,
    approx_rows: Option<u64>,
    timing: Option<Timing>,
    loading: bool,
    error: Option<String>,
    /// What the user has marked in the result: the focused cell and the
    /// range around it, and the rows ticked in the gutter. A page is a
    /// different set of rows, so turning the page clears it.
    selection: Selection,
    /// Scroll position, kept across the re-render after every page.
    scroll: GridState,
    generation: u64,
}

struct QueryTab {
    id: u64,
    title: SharedString,
    /// The relation the tab was opened on, when it came from the sidebar.
    /// The statement is the user's to edit from there on, so this says
    /// where the tab started, not what it now runs — the sidebar marks the
    /// row it came from with it.
    relation: Option<(String, String)>,
    editor: Entity<SqlEditor>,
    data: Rc<GridData>,
    has_result: bool,
    /// Whether the memory cap ended the read before the server ran out of
    /// rows. The result line says so, because a grid that stops at an
    /// arbitrary row must not read as the whole answer.
    truncated: bool,
    /// How many statements the last run sent, so the result strip can say
    /// which set is on screen.
    statements_run: usize,
    timing: Option<Timing>,
    error: Option<String>,
    /// Where the tab's last run got to. It drives the run button, the
    /// timer beside it and the result line, which is why all three agree.
    run: Run,
    /// How many runs have landed with a result in this tab.
    ///
    /// It is a **counter, not a time**, because that is all the run
    /// button's settle needs: GPUI restarts an animation when the element's
    /// id changes, so the id carries this number and the exhale needs
    /// neither a timer of its own nor an `Instant` to be checked against. A
    /// run that failed or was cancelled does not count — the error strip is
    /// the answer there, and a warm confirming tone would be the wrong
    /// word.
    landed: u64,
    /// Whether this tab's session is sitting inside a transaction, as of
    /// its last run — one the user typed a `BEGIN` for, or one the app
    /// opened because the tab is in [`TxMode::Manual`].
    ///
    /// Read after every run rather than at the moment it is wanted,
    /// because the moment it is wanted is a close — and a close cannot
    /// wait on a round trip to find out what to ask. The cache is exact,
    /// not a guess: the connection is pinned to this tab, so nothing but
    /// this tab's own statements can change what it is in.
    in_transaction: bool,
    /// Who ends this tab's transactions. It is a property of the **tab**,
    /// because a transaction lives on one connection and a tab is one
    /// connection; the profile carries the mode a new tab opens on.
    tx_mode: TxMode,
    /// How many statements have run inside the transaction that is open.
    ///
    /// It is what the bar has to say instead of "rows touched": the driver
    /// reports no affected count today, so a row figure would be invented.
    /// A statement count is a number the app actually has.
    tx_statements: usize,
    /// How the last transaction ended, until the next run or the next
    /// change of mode. The bar goes on saying so for a moment, because
    /// "committed" is the answer to the question the user just asked and a
    /// bar that vanished would leave it unanswered.
    tx_done: Option<TxEnd>,
    /// True while a commit or a rollback is out. One at a time: the
    /// session is one connection, and a second press must not send a
    /// second boundary down it.
    tx_ending: bool,
    /// When this tab last used its session. Only a run counts: reading a
    /// result on screen costs the server nothing, and a connection held
    /// open for a tab nobody is asking anything is the thing the sweep
    /// exists to give back.
    last_used: Instant,
    /// Whether the last session this tab had was taken back rather than
    /// closed with the tab. The toolbar says so until the next run, which
    /// opens another — a `search_path` that reset must be explainable.
    session_ended: bool,
    /// This tab's own connection, from its first run onwards.
    ///
    /// It is what makes a `SET`, a `BEGIN` and a temp table mean anything
    /// from one statement to the next: the tab is a session in the user's
    /// head, so it is one on the wire. A run that fails hands the session
    /// back all the same — a syntax error does not end a transaction, and
    /// dropping the session here would both lose the user's state and
    /// return an uncommitted connection to the pool.
    session: Option<Arc<dyn Session>>,
    /// What the user has marked in the result. A run replaces the rows, so
    /// it clears with them.
    selection: Selection,
    scroll: GridState,
    generation: u64,
}

/// The comp's four states for a query tab's run, and the whole of what the
/// run button offers: run, stop, terminate.
///
/// `Idle` covers both "never run" and "finished", because the button says
/// the same thing in each — the design's own `qIdle` groups them too.
enum Run {
    Idle,
    /// The statement is out — or, with no backend yet, the tab's session
    /// is still opening and the statement has not left. ⌘. covers both:
    /// it asks the server to give the statement up, or it calls the run
    /// off before it starts.
    Running(Live),
    /// A cancel has gone to the server and the statement has not come back
    /// yet. A second ⌘. terminates the backend instead of asking it
    /// nicely, which is the only reason this is a state of its own.
    Cancelling(Live),
    /// Stopped on the user's word, after this many milliseconds.
    Cancelled { elapsed: u128 },
}

/// A run in flight.
#[derive(Clone, Copy)]
struct Live {
    /// When it started, for the timer in the toolbar. Wall clock is not
    /// wanted here: the timer measures a wait, not a time of day.
    started: Instant,
    /// The backend the statement is on, known before the run leaves —
    /// because it belongs to the tab's session, not to the request.
    ///
    /// `None` says the session is still opening, and that is not a gap in
    /// what is known: **no statement is out yet**, so there is nothing on
    /// the server to cancel. A stop pressed here calls the run off instead,
    /// which is why it needs no waiting and no atomic. Before sessions the
    /// id arrived one round trip *into* the run, and the difference is the
    /// whole reason a stop used to have to wait for a race.
    backend: Option<RunId>,
}

/// Where a finished run's time went.
///
/// **One number cannot answer the question the user is asking.** A wall
/// clock around the call says "you waited 340 ms" and says nothing about
/// whether the database was slow or the link was — and over a VPN to a
/// remote server the link is usually the larger half. So a run reports two:
/// what the **server** spent on the statement, and the **lag** around it.
///
/// The two always add up to `total_ms`, because the lag is the total less
/// the server's share rather than a clock of its own. Time the app cannot
/// account for has to show up somewhere, and the honest place for it is the
/// half that means "not the database".
#[derive(Clone, Copy)]
struct Timing {
    /// Wall clock around the whole run, which is what the user waited.
    total_ms: u128,
    /// What the client could see for itself: the round trip, and how long
    /// the first row took to appear. See `db_client::Wire`.
    wire: Wire,
    /// What the server says it spent, from `pg_stat_statements`.
    ///
    /// It is **filled in after the result is already on screen**, so it is
    /// `None` for a moment on every run — and for ever against a server
    /// without the extension, or for a buffer of several statements, where
    /// the server's answer names only the last one.
    server: Option<ServerTiming>,
}

impl Timing {
    fn new(total_ms: u128, wire: Wire) -> Self {
        Self { total_ms, wire, server: None }
    }

    /// The server's share, and whether that is a measurement or an
    /// estimate.
    ///
    /// The server's own figure when there is one — planning and execution
    /// together, because both are the server working on this statement and
    /// the user is asking what the database cost. Otherwise time-to-first-
    /// row less the round trip, which is an estimate: for a plan that
    /// streams, the server is still working while the rows cross.
    ///
    /// `None` means the engine offered neither, which is every SQLite
    /// connection — a local file has no link to separate out.
    fn server_ms(&self) -> Option<(f64, bool)> {
        if let Some(server) = self.server {
            return Some((server.exec_ms + server.plan_ms.unwrap_or(0.), server.exact));
        }
        let first_row = self.wire.first_row_ms? as f64;
        let link = self.wire.link_ms.unwrap_or(0) as f64;
        Some(((first_row - link).max(0.), false))
    }

    /// Everything that was not the server: the statement going out, the
    /// rows coming back, and this process decoding them.
    fn lag_ms(&self) -> Option<f64> {
        let (server, _) = self.server_ms()?;
        Some((self.total_ms as f64 - server).max(0.))
    }

    /// The strip's line. `~` marks a server figure that is an estimate or a
    /// mean rather than this run's own measured time — an unmarked number
    /// claims more than the app knows.
    fn summary(&self) -> String {
        let total = format_millis(self.total_ms);
        let Some(((server, exact), lag)) = self.server_ms().zip(self.lag_ms()) else {
            return format!("queried in {total}");
        };
        let mark = if exact { "" } else { "~" };
        format!(
            "queried in {total} · server {mark}{} · lag {}",
            format_millis_frac(server),
            format_millis_frac(lag)
        )
    }
}

/// What the run button is at this instant, which is not quite what the run
/// is.
///
/// `Run` is the state of the statement; this is the state of the *button*,
/// and the two part company for the first `RUN_ARM` of a run: the statement
/// is out, and the button still says "run". Keeping that difference in a
/// type of its own is what stops the verb, the fill, the glyph and the
/// keycap from each working it out slightly differently.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum RunPhase {
    /// Nothing is out, or something is out and has not been out long
    /// enough to be worth offering to stop. The comp's `arm` phase is this
    /// one with a run breathing under it.
    Ready,
    /// A run has proved slow. The button offers to end it.
    Stop,
    /// A cancel is with the server. The button offers to close the backend.
    Terminate,
}

impl Run {
    /// The button's state, from the run's state and the clock.
    fn phase(&self) -> RunPhase {
        match self {
            Run::Idle | Run::Cancelled { .. } => RunPhase::Ready,
            Run::Running(live) if live.started.elapsed() < RUN_ARM => RunPhase::Ready,
            Run::Running(_) => RunPhase::Stop,
            Run::Cancelling(_) => RunPhase::Terminate,
        }
    }

    /// Is the button in its arming window — a run out, and the button not
    /// yet offering to stop it? A click does nothing here: the word under
    /// the pointer says "run", and a run is what this tab already has.
    fn arming(&self) -> bool {
        self.in_flight() && self.phase() == RunPhase::Ready
    }

    fn live(&self) -> Option<&Live> {
        match self {
            Run::Running(live) | Run::Cancelling(live) => Some(live),
            Run::Idle | Run::Cancelled { .. } => None,
        }
    }

    /// Is the server still working on this run? The timer ticks while any
    /// tab says yes.
    fn in_flight(&self) -> bool {
        self.live().is_some()
    }
}

/// The history screen, as a tab. It is a view of the local file rather
/// than of the database, so it holds no generation counter: reading it is
/// a synchronous SQLite call, and there is no slow reply to outrun.
struct HistoryTab {
    id: u64,
    /// Day headings and runs, flattened for `uniform_list`.
    rows: Rc<Vec<HistoryRow>>,
    /// How many of those rows are runs, for the status strip.
    runs: usize,
    /// The comp's two chips.
    user_only: bool,
    errors_only: bool,
    error: Option<String>,
    scroll: UniformListScrollHandle,
}

impl Tab {
    fn id(&self) -> u64 {
        match self {
            Tab::Table(tab) => tab.id,
            Tab::Query(tab) => tab.id,
            Tab::History(tab) => tab.id,
        }
    }

    fn title(&self) -> SharedString {
        match self {
            Tab::Table(tab) => format!("{}.{}", tab.schema, tab.table).into(),
            Tab::Query(tab) => tab.title.clone(),
            Tab::History(_) => "history".into(),
        }
    }

    fn selection(&self) -> Option<&Selection> {
        match self {
            Tab::Table(tab) => Some(&tab.selection),
            Tab::Query(tab) => Some(&tab.selection),
            Tab::History(_) => None,
        }
    }

    /// The four things every key that moves a selection needs at once. A
    /// history tab shows a list of runs rather than a result, so it has
    /// none of them and every one of those keys passes it by.
    fn marked(&mut self) -> Option<Marked<'_>> {
        match self {
            Tab::Table(tab) => Some(Marked {
                data: &tab.data,
                selection: &mut tab.selection,
                scroll: &tab.scroll,
            }),
            Tab::Query(tab) => Some(Marked {
                data: &tab.data,
                selection: &mut tab.selection,
                scroll: &tab.scroll,
            }),
            Tab::History(_) => None,
        }
    }
}

/// A tab's result, borrowed field by field, so one key press can read the
/// rows, move the selection and scroll to where it went.
struct Marked<'a> {
    data: &'a Rc<GridData>,
    selection: &'a mut Selection,
    scroll: &'a GridState,
}

fn empty_grid() -> Rc<GridData> {
    Rc::new(GridData::empty())
}

impl Shell {
    /// Open the shell and start connecting. The window paints the
    /// connecting state immediately; the connection lands later.
    pub fn new(target: Target, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let (store, store_error) = match Store::open_default() {
            Ok(store) => (Some(store), None),
            Err(error) => (None, Some(error.to_string())),
        };
        let scope = scope_of(&target);
        let env = match &target {
            Target::Profile(profile) => Env::parse(
                store
                    .as_ref()
                    .and_then(|store| store.env_of(&profile.id).ok().flatten())
                    .as_deref(),
            ),
            Target::Url(_) => None,
        };
        // The catalog this connection had when it was last open. It paints
        // the sidebar and feeds completion straight away; the introspection
        // running behind it replaces it when it lands. A store that cannot
        // be read is a miss, not an error: the session works without it.
        let cached = store.as_ref().and_then(|store| store.cached_catalog(&scope).ok().flatten());
        // What this connection was left with. It is read here, beside the
        // catalog, for the same reason: a local SQLite row, so the strip
        // is painted on the first frame rather than after a round trip.
        let saved = store
            .as_ref()
            .and_then(|store| store.saved_tabs(&scope).ok())
            .unwrap_or_default();
        let catalog_filter =
            cx.new(|cx| TextField::new("filter schemas and tables…", cx).bare(FILTER_FONT_SIZE));
        let subscriptions = vec![cx.subscribe_in(&catalog_filter, window, Self::on_filter_event)];
        let mut shell = Self {
            focus_handle: cx.focus_handle(),
            grid_focus: cx.focus_handle(),
            status: Status::Connecting(describe(&target)),
            connection: None,
            catalog: None,
            catalog_loading: false,
            catalog_error: None,
            catalog_groups: Rc::new(Vec::new()),
            catalog_rows: Rc::new(Vec::new()),
            open_schemas: HashSet::new(),
            closed_sections: HashSet::new(),
            catalog_filter,
            catalog_selected: None,
            catalog_scroll: UniformListScrollHandle::new(),
            catalog_drag: DragState::default(),
            relation_total: 0,
            vocabulary: Arc::new(Vocabulary::default()),
            label: None,
            name: connection_name(&target),
            read_only: match &target {
                Target::Profile(profile) => profile.read_only,
                Target::Url(_) => true,
            },
            tx_default: match &target {
                Target::Profile(profile) => profile.tx_mode,
                Target::Url(_) => TxMode::Auto,
            },
            tabs: Vec::new(),
            active: 0,
            next_id: 1,
            store,
            store_error,
            scope,
            env,
            palette: None,
            column_find: None,
            peek: None,
            confirm: None,
            sweeping: false,
            timing: false,
            run_pressed: false,
            restoring: false,
            _subscriptions: subscriptions,
        };
        if let Some(catalog) = cached {
            shell.apply_catalog(catalog, cx);
        }
        shell.connect(target, cx);
        if saved.tabs.is_empty() {
            // A session opens on an empty query tab rather than on the
            // first table: it needs no catalog and no round trip, so the
            // user can type while the connection is still being made.
            shell.new_query(window, cx);
        } else {
            shell.restore_tabs(saved, window, cx);
        }
        shell
    }

    // --- the tabs a connection was left with ------------------------------

    /// Open the tabs this connection had last time, in the order the strip
    /// had them.
    ///
    /// A query tab comes back on its statement and waits: a restored
    /// session must not run a hundred statements at the user, for the same
    /// reason the history screen never re-runs one. A table tab is the
    /// paged view, so it asks for its page — and asks for it through the
    /// usual path, which leaves the tab loading until the connection lands.
    fn restore_tabs(&mut self, saved: SavedTabs, window: &mut Window, cx: &mut Context<Self>) {
        self.restoring = true;
        for tab in saved.tabs {
            match tab {
                SavedTab::Query { title, statement, relation, tx_mode } => {
                    self.new_query_with(&statement, window, cx);
                    if let Some(Tab::Query(tab)) = self.tabs.last_mut() {
                        if !title.is_empty() {
                            tab.title = title.into();
                        }
                        tab.relation = relation;
                        // The mode the tab was left in, not the
                        // connection's default: a tab switched to manual
                        // was switched for a reason.
                        tab.tx_mode = tx_mode;
                    }
                }
                SavedTab::Table { schema, table, page } => {
                    self.restore_table(schema, table, page, cx)
                }
                SavedTab::History => self.open_history(cx),
            }
        }
        self.restoring = false;
        // An empty strip is not a session: a file that held nothing this
        // shell could open still opens on a query tab.
        if self.tabs.is_empty() {
            self.new_query(window, cx);
            return;
        }
        self.activate(saved.active.min(self.tabs.len() - 1), cx);
        self.focus_active_tab(window, cx);
    }

    /// Put back a paged table tab. This is the only way one is opened now:
    /// nothing in the session starts a paged view, since the sidebar and
    /// the palette both browse a relation in a query tab, so a table tab
    /// comes back only where an earlier session left one. It is built here
    /// by hand because the catalog names the table's kind and the catalog
    /// may still be on its way — a tab the user had open is opened whether
    /// or not the shell can describe it yet.
    fn restore_table(&mut self, schema: String, table: String, page: usize, cx: &mut Context<Self>) {
        let model = self.table_model(&schema, &table);
        let kind = model.map_or(TableKind::Table, |model| model.kind);
        let approx_rows = model.and_then(|model| model.approx_rows);
        let id = self.take_id();
        self.tabs.push(Tab::Table(TableTab {
            id,
            schema,
            table,
            kind,
            data: empty_grid(),
            page,
            approx_rows,
            timing: None,
            loading: false,
            error: None,
            selection: Selection::default(),
            scroll: GridState::new(),
            generation: 0,
        }));
        self.activate(self.tabs.len() - 1, cx);
        self.load_page(id, page, cx);
    }

    /// Write the strip back to the local file, so the next session on this
    /// connection opens on it. Every path that changes what is open, what
    /// a query tab holds, or which tab is in front comes through here.
    ///
    /// A strip that cannot be written is not worth interrupting a session
    /// over, so the error is dropped — as the history's is.
    pub fn remember_tabs(&self, cx: &App) {
        if self.restoring {
            return;
        }
        let Some(store) = &self.store else { return };
        let tabs: Vec<SavedTab> = self
            .tabs
            .iter()
            .map(|tab| match tab {
                Tab::Table(tab) => SavedTab::Table {
                    schema: tab.schema.clone(),
                    table: tab.table.clone(),
                    page: tab.page,
                },
                Tab::Query(tab) => SavedTab::Query {
                    title: tab.title.to_string(),
                    statement: tab.editor.read(cx).text().to_string(),
                    relation: tab.relation.clone(),
                    tx_mode: tab.tx_mode,
                },
                Tab::History(_) => SavedTab::History,
            })
            .collect();
        store.save_tabs(&self.scope, &tabs, self.active).ok();
    }

    // --- the sidebar's catalog list --------------------------------------

    /// Flatten the groups again for the list. Every path that changes what
    /// the sidebar shows — a catalog landing, a group opening, a keystroke
    /// in the filter — goes through here, so the rows and the state that
    /// produced them cannot drift apart.
    fn rebuild_catalog_rows(&mut self, cx: &mut Context<Self>) {
        // The matching lowercases as it goes, so the line is passed on as
        // it was typed.
        let needle = self.catalog_filter.read(cx).trimmed().to_string();
        self.catalog_rows = catalog_rows(
            &self.catalog_groups,
            &self.open_schemas,
            &self.closed_sections,
            &needle,
        );
        // The cursor is an index into the rows that have just been
        // replaced, so it cannot survive them: a keystroke in the filter
        // puts other names at those indices, and a cursor left where it
        // was would point at one the user never walked to.
        self.catalog_selected = None;
        // What ⇥ would take is read off the rows, so the hint is set here
        // rather than by each caller.
        self.update_filter_ghost(cx);
        cx.notify();
    }

    /// The relation the keys are aimed at: the one the cursor is on, or
    /// the first the filter left while the cursor is still on the line. ⏎
    /// opens it and ⇥ finishes the line from it, so the hint and the key
    /// cannot say two different things.
    fn filter_target(&self) -> Option<(SharedString, SharedString)> {
        let row = match self.catalog_selected {
            Some(ix) => self.catalog_rows.get(ix)?,
            None => self
                .catalog_rows
                .iter()
                .find(|row| matches!(row, CatalogRow::Relation { .. }))?,
        };
        match row {
            CatalogRow::Relation { schema, name, .. } => Some((schema.clone(), name.clone())),
            CatalogRow::Schema { .. } | CatalogRow::Section { .. } => None,
        }
    }

    /// The rows ↑↓ walk: the relations the filter left, by their index
    /// into the flattened list. A header is not among them — it opens and
    /// closes, and ⏎ on this line means "open this relation".
    fn catalog_stops(&self) -> Vec<usize> {
        self.catalog_rows
            .iter()
            .enumerate()
            .filter(|(_, row)| matches!(row, CatalogRow::Relation { .. }))
            .map(|(ix, _)| ix)
            .collect()
    }

    /// Move the cursor one relation on, and scroll it into view.
    fn step_catalog(&mut self, forward: bool, cx: &mut Context<Self>) {
        let stops = self.catalog_stops();
        self.catalog_selected = step_stop(&stops, self.catalog_selected, forward);
        if let Some(ix) = self.catalog_selected {
            // `Nearest` and not `Center`: the cursor walks one row at a
            // time, and a list that re-centred on every press would move
            // further than the cursor did.
            self.catalog_scroll.scroll_to_item(ix, ScrollStrategy::Nearest);
        }
        // ⇥ finishes the line from the cursor, so moving it changes what
        // the hint offers.
        self.update_filter_ghost(cx);
        cx.notify();
    }

    /// What ⇥ would finish the filter line with: the next part of the
    /// relation the keys are aimed at, as the palette finishes a name.
    fn filter_completion(&self, cx: &App) -> Option<String> {
        let needle = self.catalog_filter.read(cx).trimmed();
        let (schema, name) = self.filter_target()?;
        palette::complete_path(&[schema.to_string(), name.to_string()], needle)
    }

    /// Show what ⇥ would take, faint and after the value, in the shape
    /// `palette::ghost` gives it — the rest of the word when it carries on
    /// from the line, and `⇥ <name>` when taking it rewrites the line.
    fn update_filter_ghost(&mut self, cx: &mut Context<Self>) {
        let needle = self.catalog_filter.read(cx).trimmed().to_string();
        let ghost = self
            .filter_completion(cx)
            .map(|completed| palette::ghost(&needle, &completed))
            .unwrap_or_default();
        self.catalog_filter.update(cx, |field, cx| field.set_ghost(ghost, cx));
    }

    /// Take the first match's name into the filter line, one part at a
    /// time, so ⇥⇥ walks schema then relation. It **replaces** the line
    /// rather than appending to it, because a hit sits anywhere inside a
    /// name: `dev` completes to `sample_dev_sample.`.
    fn complete_filter(&mut self, cx: &mut Context<Self>) {
        let Some(completed) = self.filter_completion(cx) else { return };
        // Setting the text emits `Changed`, which filters again, so the
        // list already follows the completed line when this returns.
        self.catalog_filter.update(cx, |field, cx| field.set_text(completed, cx));
    }

    /// Put the keys on the filter line, wherever they were, and mark what
    /// is on it. ⌘E is how the sidebar is reached without the mouse.
    ///
    /// The value is **marked rather than emptied**: a line the user
    /// narrowed to `dev.` is worth carrying on from, and ⎋ already empties
    /// it. Marking it means the next character replaces it either way, so
    /// nothing is lost by keeping it.
    ///
    /// The column find is closed for the reason the palette closes it: one
    /// search line takes the keys at a time. The palette and the close
    /// dialog are not reached past, because each is already the thing
    /// being answered.
    fn focus_catalog_filter(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.palette.is_some() || self.confirm.is_some() {
            return;
        }
        self.close_column_find(window, cx);
        self.catalog_filter.update(cx, |field, cx| field.select_everything(cx));
        window.focus(&self.catalog_filter.focus_handle(cx), cx);
        cx.notify();
    }

    /// Open a closed schema, or close an open one.
    fn toggle_schema(&mut self, key: SharedString, cx: &mut Context<Self>) {
        if !self.open_schemas.remove(&key) {
            self.open_schemas.insert(key);
        }
        self.rebuild_catalog_rows(cx);
    }

    /// Close an open section, or open a closed one. A section is open
    /// until it is closed, so the set holds the opposite of the schemas'.
    fn toggle_section(&mut self, key: SharedString, cx: &mut Context<Self>) {
        if !self.closed_sections.remove(&key) {
            self.closed_sections.insert(key);
        }
        self.rebuild_catalog_rows(cx);
    }

    fn on_filter_event(
        &mut self,
        _field: &Entity<TextField>,
        event: &TextFieldEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            TextFieldEvent::Changed => {
                // A filtered list is scrolled to wherever the last one was,
                // which is nowhere in particular once the rows change under
                // it. Send it back to the top.
                self.catalog_scroll.scroll_to_item(0, ScrollStrategy::Top);
                self.rebuild_catalog_rows(cx);
            }
            // Enter opens the relation the cursor is on, and the first the
            // filter left while the cursor is still on the line — which is
            // what a one-hit filter is for. Escape empties the line and
            // hands the keys back to the workspace.
            TextFieldEvent::Submit => {
                if let Some((schema, table)) = self.filter_target() {
                    self.browse_table(&schema.to_string(), &table.to_string(), window, cx);
                }
            }
            TextFieldEvent::Cancel => {
                self.catalog_filter.update(cx, |field, cx| field.clear(cx));
                window.focus(&self.focus_handle, cx);
            }
            // ⇥ finishes the line from the first match, one part at a time.
            TextFieldEvent::NextField => self.complete_filter(cx),
        }
    }

    /// Take a catalog as the session's own. The sidebar rows, the
    /// completion vocabulary and the relation count all derive from it,
    /// and every open query tab is handed the new words — a tab built
    /// before the catalog landed carries an empty vocabulary otherwise.
    fn apply_catalog(&mut self, catalog: Catalog, cx: &mut Context<Self>) {
        self.vocabulary = Arc::new(vocabulary_of(&catalog));
        self.catalog_groups = catalog_groups(&catalog);
        // A group the user opened keeps its name across an introspection,
        // so the sidebar does not close under them when the real catalog
        // replaces the cached one.
        self.rebuild_catalog_rows(cx);
        self.relation_total = catalog.schemas.iter().map(|schema| schema.tables.len()).sum();
        self.catalog = Some(catalog);
        let vocabulary = self.vocabulary.clone();
        for tab in &self.tabs {
            if let Tab::Query(tab) = tab {
                tab.editor.update(cx, |editor, cx| editor.set_vocabulary(vocabulary.clone(), cx));
            }
        }
    }

    /// Open the pool, and nothing more. Reading the catalog of a large
    /// database takes far longer than the connect itself, so it is a
    /// second request: the session says "connected" and runs queries as
    /// soon as there is a pool, and the sidebar fills in behind it.
    fn connect(&mut self, target: Target, cx: &mut Context<Self>) {
        let task = gpui_tokio::Tokio::spawn(cx, async move {
            let connection = match &target {
                Target::Url(url) => PostgresConnection::connect(url).await?,
                // A saved connection takes its password from the OS
                // keychain, so nothing here carries one.
                Target::Profile(profile) => PostgresConnection::connect_profile(profile).await?,
            };
            let label = connection.label().clone();
            anyhow::Ok((Arc::new(connection) as Arc<dyn Connection>, label))
        });

        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                match flatten(outcome) {
                    Ok((connection, label)) => {
                        this.connection = Some(connection);
                        this.label = Some(label);
                        this.status = Status::Connected;
                        this.resume_pending_pages(cx);
                        this.introspect(cx);
                    }
                    Err(error) => {
                        this.status = Status::Failed(error);
                        // Tabs opened from the cached catalog were waiting
                        // for this connection. Nothing is coming.
                        for tab in &mut this.tabs {
                            if let Tab::Table(tab) = tab
                                && tab.loading
                            {
                                tab.loading = false;
                                tab.error = Some(NOT_CONNECTED.to_string());
                            }
                        }
                    }
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
    }

    /// Read the catalog behind the connection. A session that opened on a
    /// cached catalog keeps painting it until this lands, and keeps it if
    /// this fails — a stale sidebar beats no sidebar.
    fn introspect(&mut self, cx: &mut Context<Self>) {
        let Some(connection) = self.connection.clone() else { return };
        self.catalog_loading = true;
        self.catalog_error = None;
        let task = gpui_tokio::Tokio::spawn(cx, async move { connection.introspect().await });
        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                this.catalog_loading = false;
                match flatten(outcome) {
                    Ok(catalog) => {
                        // Keep this shape for the next session. A cache
                        // that cannot be written costs nothing here.
                        if let Some(store) = &this.store {
                            store.cache_catalog(&this.scope, &catalog, unix_now()).ok();
                        }
                        this.apply_catalog(catalog, cx);
                    }
                    Err(error) => this.catalog_error = Some(error),
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
    }

    /// A table opened from the cached catalog before the connection landed
    /// is still waiting for its first page: `load_page` left it marked
    /// loading and sent nothing. Ask for those pages now.
    fn resume_pending_pages(&mut self, cx: &mut Context<Self>) {
        let pending: Vec<(u64, usize)> = self
            .tabs
            .iter()
            .filter_map(|tab| match tab {
                Tab::Table(tab) if tab.loading => Some((tab.id, tab.page)),
                _ => None,
            })
            .collect();
        for (id, page) in pending {
            self.load_page(id, page, cx);
        }
    }

    /// Open a relation the way a click in the sidebar asks for one: a new
    /// query tab on `SELECT * FROM ... LIMIT 500;`, run at once.
    ///
    /// The statement is the user's from that moment — editable, and re-run
    /// with ⌘⏎ — which is the point of opening a relation this way rather
    /// than in the paged table view. It is run rather than left waiting
    /// because a click on a table name asks for its rows, not for a line of
    /// SQL to look at.
    fn browse_table(
        &mut self,
        schema: &str,
        table: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.new_query_with(&browse_query(schema, table), window, cx);
        // The tab is named for what it opened, not `query 7`: the strip is
        // then a list of what the user is looking at.
        if let Some(Tab::Query(tab)) = self.tabs.last_mut() {
            tab.title = format!("{schema}.{table}").into();
            tab.relation = Some((schema.to_string(), table.to_string()));
        }
        // The name and the relation land after the tab was opened, so the
        // strip is written back once more with them on it.
        self.remember_tabs(cx);
        self.run_active_query(cx);
    }

    /// Browse a relation from the palette, or focus the tab that already
    /// browses it. `new_tab` is the palette's ⌘⏎: give me another tab on
    /// this one, so two statements over the same table can sit side by
    /// side.
    ///
    /// A tab is the one that already browses this relation when it was
    /// opened on it — `QueryTab::relation` is what says so. The statement
    /// in it is the user's by then and may say anything, which is exactly
    /// why the tab is focused rather than rewritten.
    fn browse_table_in(
        &mut self,
        schema: &str,
        table: &str,
        new_tab: bool,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if !new_tab
            && let Some(ix) = self.tabs.iter().position(|tab| match tab {
                Tab::Query(t) => t
                    .relation
                    .as_ref()
                    .is_some_and(|(s, r)| s == schema && r == table),
                Tab::Table(_) | Tab::History(_) => false,
            })
        {
            self.activate(ix, cx);
            self.focus_active_tab(window, cx);
            cx.notify();
            return;
        }
        self.browse_table(schema, table, window, cx);
    }

    fn table_model(&self, schema: &str, table: &str) -> Option<&Table> {
        self.catalog
            .as_ref()?
            .schemas
            .iter()
            .find(|s| s.name == schema)?
            .tables
            .iter()
            .find(|t| t.name == table)
    }

    fn load_page(&mut self, tab_id: u64, page: usize, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        let failed = matches!(self.status, Status::Failed(_));
        let Some(Tab::Table(tab)) = self.tab_mut(tab_id) else { return };
        // The cached catalog can open a table before the connection is
        // there. Leave the tab loading; `resume_pending_pages` asks again
        // as soon as the connection lands. Once the connect has failed
        // there is nothing left to wait for, so say so instead.
        let Some(connection) = connection else {
            tab.page = page;
            tab.selection.clear();
            tab.loading = !failed;
            tab.error = failed.then(|| NOT_CONNECTED.to_string());
            self.remember_tabs(cx);
            cx.notify();
            return;
        };

        let (schema, table) = (tab.schema.clone(), tab.table.clone());
        let Some(model) = self.table_model(&schema, &table).cloned() else { return };
        let sql = page_query(&schema, &model, page);

        let Some(Tab::Table(tab)) = self.tab_mut(tab_id) else { return };
        tab.page = page;
        tab.loading = true;
        tab.error = None;
        // A page is a different set of rows, and a selection points at rows
        // by index: what was marked cannot be carried over to them.
        tab.selection.clear();
        tab.generation += 1;
        let generation = tab.generation;

        // The page a table tab sits on is part of what the next session
        // reopens, so it is written back as it turns.
        self.remember_tabs(cx);

        let recorded = sql.clone();
        let task = run_sql(connection, sql, cx);
        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                let Some(Tab::Table(tab)) = this.tab_mut(tab_id) else { return };
                if tab.generation != generation {
                    return;
                }
                tab.loading = false;
                let run = match flatten(outcome) {
                    Ok((result, elapsed)) => {
                        let rows = result.rows.len() as u64;
                        // A table page runs on the *pool*, so there is no
                        // session to ask what the server spent: the backend
                        // that ran it has gone back to the pool and may be
                        // running somebody else's statement by now. The
                        // client's own split is all this path reports, and
                        // it is enough — a page is the app's own
                        // `LIMIT 500`, not a statement anyone is tuning.
                        tab.timing = Some(Timing::new(elapsed, result.wire));
                        tab.data = Rc::new(GridData::new(result.columns, result.rows));
                        Outcome { elapsed: Some(elapsed), rows: Some(rows), error: None }
                    }
                    Err(error) => {
                        tab.error = Some(error.clone());
                        tab.data = empty_grid();
                        Outcome { elapsed: None, rows: None, error: Some(error) }
                    }
                };
                this.record_run(&recorded, RunSource::App, &run);
                this.reload_open_history(cx);
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
    }

    fn new_query(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.new_query_with("select * from ", window, cx);
    }

    /// Open a query tab on a statement. The history screen opens a run
    /// this way, so a query comes back editable rather than re-run behind
    /// the user's back.
    fn new_query_with(&mut self, sql: &str, window: &mut Window, cx: &mut Context<Self>) {
        let id = self.take_id();
        let vocabulary = self.vocabulary.clone();
        let editor = cx.new(|cx| SqlEditor::new(sql, vocabulary, cx));
        window.focus(&editor.focus_handle(cx), cx);
        self.tabs.push(Tab::Query(QueryTab {
            id,
            title: format!("query {id}").into(),
            relation: None,
            editor,
            data: empty_grid(),
            has_result: false,
            truncated: false,
            statements_run: 0,
            timing: None,
            error: None,
            run: Run::Idle,
            landed: 0,
            in_transaction: false,
            tx_mode: self.tx_default,
            tx_statements: 0,
            tx_done: None,
            tx_ending: false,
            last_used: Instant::now(),
            session_ended: false,
            // Opened on the tab's first run, never here: a strip of a
            // hundred restored tabs must cost the server nothing until the
            // user asks one of them a question.
            session: None,
            selection: Selection::default(),
            scroll: GridState::new(),
            generation: 0,
        }));
        self.activate(self.tabs.len() - 1, cx);
        cx.notify();
    }

    fn run_active_query(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        let Some(Tab::Query(tab)) = self.tabs.get_mut(self.active) else { return };
        // ⌘⏎ while the last run is still out does nothing: the key that
        // starts a run is not the key that stops one, and a second run
        // over the top of the first would leave the first unstoppable.
        // A commit on its way out holds the same connection, so it counts
        // as a run in flight here.
        if tab.run.in_flight() || tab.tx_ending {
            return;
        }
        // The tab opens before the connection does, so ⌘⏎ can arrive
        // first. Say so rather than doing nothing.
        let Some(connection) = connection else {
            tab.error = Some(NOT_CONNECTED.to_string());
            cx.notify();
            return;
        };

        // The selection when there is one, the whole buffer otherwise.
        let editor = tab.editor.read(cx);
        let sql = editor.selected_text().unwrap_or_else(|| editor.text().to_string());
        // A driver runs one command at a time, so send the statements in
        // turn rather than handing the server a whole scratchpad.
        let statements = query::statements(&sql);
        if statements.is_empty() {
            return;
        }
        let tab_id = tab.id;
        let session = tab.session.clone();
        // **Manual mode holds one transaction across the runs of a tab**,
        // so a run opens one when none is open — and never when the buffer
        // opens its own, because a second `BEGIN` is a warning from the
        // server and a lie in the bar.
        let begin = needs_begin(
            tab.tx_mode,
            tab.in_transaction,
            query::transaction_verb(&statements[0]),
        );
        // What the buffer itself does to the transaction, which the bar has
        // to report whichever mode the tab is in: a `COMMIT` typed by hand
        // is the same event as the button. The **last** verb wins, because
        // `BEGIN; …; COMMIT` in one buffer leaves nothing open.
        let buffer_verb = statements.iter().rev().find_map(|sql| query::transaction_verb(sql));
        // What the bar counts as having landed *inside* the transaction:
        // the statements that are not boundaries. A bare `BEGIN` therefore
        // opens a transaction with nothing in it, which is what it did.
        let plain_statements =
            statements.iter().filter(|sql| query::transaction_verb(sql).is_none()).count();
        // A tab about to open its first session may be the one too many.
        // Give back the session that has gone longest without a question;
        // with nothing spare to give, say so here rather than let the pool
        // wait out its connect timeout and answer with something about
        // connections.
        if session.is_none() && !self.make_room_for_session(cx) {
            let Some(Tab::Query(tab)) = self.tabs.get_mut(self.active) else { return };
            tab.error = Some(NO_SESSION_LEFT.to_string());
            cx.notify();
            return;
        }
        let Some(Tab::Query(tab)) = self.tabs.get_mut(self.active) else { return };
        // The backend is the *session's*, so it is known before the run
        // leaves — there is no id to wait for any more. A tab running for
        // the first time has no session yet, and `None` says so: the run
        // is alive, and nothing is out on the server to stop.
        let backend = session.as_ref().and_then(|session| session.backend());
        tab.run = Run::Running(Live { started: Instant::now(), backend });
        tab.error = None;
        // "committed" was the answer to the last question. This is a new
        // one, so the bar stops saying it: news about a transaction that
        // has been over since before this run is not news.
        tab.tx_done = None;
        tab.last_used = Instant::now();
        tab.session_ended = false;
        tab.generation += 1;
        let generation = tab.generation;

        let recorded = sql;
        // The buffer has been edited since the tab was opened, and this is
        // the moment the user says it is worth something.
        self.remember_tabs(cx);
        let task = run_statements(connection, session, begin, statements, tab_id, generation, cx);
        self.start_timer(cx);
        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                let Some(Tab::Query(tab)) = this.tab_mut(tab_id) else { return };
                if tab.generation != generation {
                    return;
                }
                // A run the user stopped comes back as the server's own
                // refusal. The tab says CANCELLED rather than painting that
                // as a failure, because the user is the one who asked —
                // but the history still keeps what the server said.
                let stopped = matches!(tab.run, Run::Cancelling(_));
                let waited = tab.run.live().map(|live| live.started.elapsed().as_millis());
                let RunOutcome { session, result, began } = outcome;
                // The `BEGIN` went out, so a transaction is open whether or
                // not the statements after it worked: a statement that fails
                // inside a transaction leaves it open and aborted, which is
                // the state most worth having a bar for.
                if began {
                    tab.in_transaction = true;
                }
                // Kept whether the run worked or not. A failed statement
                // does not end a transaction, and throwing the session away
                // here would lose whatever the tab had set — and hand the
                // pool a connection with an open transaction on it.
                if session.is_some() {
                    tab.session = session;
                }
                // The sweep counts from the end of a run, not the start:
                // a statement that took nine minutes has not left its
                // session idle for nine minutes.
                tab.last_used = Instant::now();
                // The server counts one statement at a time, and names only
                // the one a backend ran last. A buffer of several is
                // therefore unanswerable — reporting the last statement's
                // time as the run's would be a wrong number, not a partial
                // one — so the ask goes out only for a buffer of one.
                let one_statement = matches!(result, Ok((_, _, 1)));
                let run = match result {
                    Ok((result, elapsed, ran)) => {
                        tab.run = Run::Idle;
                        // The run button settles on this, so it counts a
                        // result rather than a reply: a failure is not
                        // something to congratulate the user on.
                        tab.landed += 1;
                        let rows = result.rows.len() as u64;
                        tab.timing = Some(Timing::new(elapsed, result.wire));
                        tab.has_result = true;
                        tab.truncated = result.truncated;
                        tab.statements_run = ran;
                        tab.data = Rc::new(GridData::new(result.columns, result.rows));
                        // A run answers with a different set of rows, and a
                        // selection points at rows by index. Marks from the
                        // last answer can only point at these ones wrongly.
                        tab.selection.clear();
                        // What the buffer did to the transaction itself. The
                        // server's own answer follows in `refresh_transaction`
                        // a moment later; this is what the bar says at once,
                        // so a commit the user typed reads as a commit.
                        match buffer_verb {
                            Some(TxVerb::Begin) => tab.in_transaction = true,
                            Some(TxVerb::Commit) => {
                                tab.in_transaction = false;
                                tab.tx_done = Some(TxEnd::Commit);
                            }
                            Some(TxVerb::Rollback) => {
                                tab.in_transaction = false;
                                tab.tx_done = Some(TxEnd::Rollback);
                            }
                            None => {}
                        }
                        if tab.in_transaction {
                            tab.tx_statements += plain_statements;
                        } else {
                            tab.tx_statements = 0;
                        }
                        Outcome { elapsed: Some(elapsed), rows: Some(rows), error: None }
                    }
                    Err(error) => {
                        if stopped {
                            tab.run = Run::Cancelled { elapsed: waited.unwrap_or_default() };
                        } else {
                            tab.run = Run::Idle;
                            tab.error = Some(error.clone());
                        }
                        tab.has_result = false;
                        tab.truncated = false;
                        tab.data = empty_grid();
                        tab.selection.clear();
                        Outcome { elapsed: None, rows: None, error: Some(error) }
                    }
                };
                this.record_run(&recorded, RunSource::User, &run);
                this.reload_open_history(cx);
                this.refresh_transaction(tab_id, cx);
                if one_statement {
                    this.refresh_server_timing(tab_id, generation, cx);
                }
                // There is a session now, so there is something to sweep.
                this.start_session_timer(cx);
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
    }

    /// ⌘. and the run button while a statement is out. The first press
    /// asks the server to give the statement up; the second closes the
    /// backend it is on. Nothing here touches the tokio task: the task is
    /// waiting on the server, and the server is what has to let go.
    fn stop_active_query(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        let Some(Tab::Query(tab)) = self.tabs.get_mut(self.active) else { return };
        let (how, live) = match &tab.run {
            Run::Running(live) => (Stop::Cancel, live),
            Run::Cancelling(live) => (Stop::Terminate, live),
            // Nothing is out, so there is nothing to stop. ⌘. on an idle
            // tab does nothing rather than something surprising.
            Run::Idle | Run::Cancelled { .. } => return,
        };
        let Some(connection) = connection else { return };
        // `Live` is the run, not the request, so both presses keep the same
        // start time and the same backend: the timer must not restart
        // because the user asked twice. A terminate therefore leaves the
        // state where the cancel put it, and the server's reply is still
        // what ends the run.
        let live = *live;
        let (tab_id, generation) = (tab.id, tab.generation);
        tab.run = Run::Cancelling(live);
        cx.notify();

        // No backend means the tab's session is still opening and nothing
        // has been sent. Marking the run `Cancelling` **is** the stop: the
        // open lands into a run that then does not start. There is no id
        // to wait for, because there is nothing to wait for it to reach.
        let Some(id) = live.backend else { return };

        cx.spawn(async move |this, cx| {
            let Ok(task) = this.update(cx, |_, cx| {
                gpui_tokio::Tokio::spawn(cx, async move { connection.stop(id, how).await })
            }) else {
                return;
            };
            // `Ok(false)` means the statement had already finished, which
            // the reply will report on its own. Only a failure to *ask*
            // is worth a word.
            if let Err(error) = flatten(task.await) {
                this.update(cx, |this, cx| {
                    // The tab this stop was for, not whichever tab is in
                    // front when the answer lands, and only while it is
                    // still the same run.
                    let Some(Tab::Query(tab)) = this.tab_mut(tab_id) else { return };
                    if tab.generation == generation {
                        tab.error = Some(error);
                        cx.notify();
                    }
                })
                .ok();
            }
        })
        .detach();
    }

    // --- sessions ---------------------------------------------------------

    /// Every open session, as the sweep and the cap need to see it.
    fn session_states(&self) -> Vec<SessionState> {
        self.tabs
            .iter()
            .filter_map(|tab| match tab {
                Tab::Query(tab) if tab.session.is_some() => Some(SessionState {
                    tab_id: tab.id,
                    running: tab.run.in_flight(),
                    in_transaction: tab.in_transaction,
                    idle: tab.last_used.elapsed(),
                }),
                _ => None,
            })
            .collect()
    }

    /// Hand back sessions nobody has used for `IDLE_SESSION`.
    fn sweep_idle_sessions(&mut self, cx: &mut Context<Self>) {
        for tab_id in idle_sessions(&self.session_states(), IDLE_SESSION) {
            self.end_session(tab_id, cx);
        }
    }

    /// Make room for one more session by giving back the one that has gone
    /// longest without a question. `false` means every session is busy or
    /// holding a transaction, and the caller must say so rather than ask
    /// for one too many: the pool would make it wait out the connect
    /// timeout and then answer with a message about connections, for
    /// something the app could see coming.
    fn make_room_for_session(&mut self, cx: &mut Context<Self>) -> bool {
        let states = self.session_states();
        if states.len() < db_client::MAX_SESSIONS {
            return true;
        }
        match evictable(&states) {
            Some(tab_id) => {
                self.end_session(tab_id, cx);
                true
            }
            None => false,
        }
    }

    /// Take one tab's session back: rolled back, returned to the pool, and
    /// the tab told so it can say why its next run starts fresh.
    fn end_session(&mut self, tab_id: u64, cx: &mut Context<Self>) {
        let Some(Tab::Query(tab)) = self.tab_mut(tab_id) else { return };
        let Some(session) = tab.session.take() else { return };
        tab.session_ended = true;
        gpui_tokio::Tokio::spawn(cx, async move { session.close().await }).detach();
        cx.notify();
    }

    /// Sweep for idle sessions while there are any. One loop for the
    /// window, as the run timer is, and it ends itself when the last
    /// session has gone — a workspace sitting on no connections must not
    /// keep waking up to notice that.
    fn start_session_timer(&mut self, cx: &mut Context<Self>) {
        if self.sweeping {
            return;
        }
        self.sweeping = true;
        cx.spawn(async move |this, cx| {
            loop {
                cx.background_executor().timer(IDLE_TICK).await;
                let more = this.update(cx, |this, cx| {
                    this.sweep_idle_sessions(cx);
                    let more = this.tabs.iter().any(
                        |tab| matches!(tab, Tab::Query(tab) if tab.session.is_some()),
                    );
                    if !more {
                        this.sweeping = false;
                    }
                    more
                });
                if !matches!(more, Ok(true)) {
                    break;
                }
            }
        })
        .detach();
    }

    /// Ask what the tab's session is now in, after a run has changed it.
    ///
    /// It runs **after** the result is on screen and on a connection of
    /// the app's own, so it costs the run nothing. The answer is worth
    /// having early because the moment it is wanted — a close — cannot
    /// wait for a round trip to decide what to ask the user.
    ///
    /// A session that cannot answer is left as it was rather than reported
    /// as clean: the safe reading of a missing answer is the careful one.
    fn refresh_transaction(&mut self, tab_id: u64, cx: &mut Context<Self>) {
        let Some(Tab::Query(tab)) = self.tab_mut(tab_id) else { return };
        let Some(session) = tab.session.clone() else { return };
        cx.spawn(async move |this, cx| {
            let Ok(task) = this.update(cx, |_, cx| {
                gpui_tokio::Tokio::spawn(cx, async move { session.in_transaction().await })
            }) else {
                return;
            };
            let Ok(open) = flatten(task.await) else { return };
            this.update(cx, |this, cx| {
                let Some(Tab::Query(tab)) = this.tab_mut(tab_id) else { return };
                if tab.in_transaction != open {
                    tab.in_transaction = open;
                    // Nothing is open, so there is nothing for the count to
                    // be a count of. It is reset here as well as on the
                    // buttons, because the server is what has the last word:
                    // a statement the app did not recognise may have ended
                    // the transaction, and a stale count would outlive it.
                    if !open {
                        tab.tx_statements = 0;
                    }
                    cx.notify();
                }
            })
            .ok();
        })
        .detach();
    }

    // --- transactions -----------------------------------------------------

    /// Switch which way this tab commits.
    ///
    /// **Auto is refused while a transaction is open.** The switch would
    /// otherwise leave the transaction standing with nothing on screen
    /// offering to end it, and the statements after it would land inside a
    /// transaction the tab says it is not in. Commit or roll back first;
    /// the bar is right there.
    fn set_tx_mode(&mut self, mode: TxMode, cx: &mut Context<Self>) {
        let Some(Tab::Query(tab)) = self.tabs.get_mut(self.active) else { return };
        if tab.tx_mode == mode {
            return;
        }
        if mode == TxMode::Auto && tab.in_transaction {
            return;
        }
        tab.tx_mode = mode;
        // The mode is not news about the last transaction, and the bar's
        // "committed" line is: switching clears it rather than leaving an
        // answer to an older question under a control that just moved.
        tab.tx_done = None;
        // A tab remembers its mode across sessions, so a switch is worth
        // writing back at once.
        self.remember_tabs(cx);
        cx.notify();
    }

    /// Commit or roll back the transaction the active tab is in.
    ///
    /// It goes down the tab's **own** session, because that is where the
    /// transaction is: a `COMMIT` on a pooled connection would commit
    /// nothing and report success. So it waits for the connection the same
    /// way a run does — and a run in flight is holding it, which is why a
    /// boundary is refused rather than queued behind one. The user has ⌘.
    /// for that, and the bar says so.
    fn end_transaction(&mut self, how: TxEnd, cx: &mut Context<Self>) {
        let Some(Tab::Query(tab)) = self.tabs.get_mut(self.active) else { return };
        if !tab.in_transaction || tab.tx_ending {
            return;
        }
        if tab.run.in_flight() {
            tab.error = Some(RUN_HOLDS_THE_SESSION.to_string());
            cx.notify();
            return;
        }
        let Some(session) = tab.session.clone() else {
            // No session and yet in a transaction is a state that cannot
            // happen: the flag is only ever set from a session's answer.
            return;
        };
        let (tab_id, statement) = (tab.id, how.sql());
        tab.tx_ending = true;
        tab.error = None;
        tab.last_used = Instant::now();
        cx.notify();

        let started = Instant::now();
        cx.spawn(async move |this, cx| {
            let Ok(task) = this.update(cx, |_, cx| {
                gpui_tokio::Tokio::spawn(cx, async move { session.end_transaction(how).await })
            }) else {
                return;
            };
            let outcome = flatten(task.await);
            this.update(cx, |this, cx| {
                let Some(Tab::Query(tab)) = this.tab_mut(tab_id) else { return };
                tab.tx_ending = false;
                tab.last_used = Instant::now();
                match &outcome {
                    Ok(()) => {
                        tab.in_transaction = false;
                        tab.tx_statements = 0;
                        tab.tx_done = Some(how);
                    }
                    // The transaction is still whatever the server says it
                    // is, so the bar stays up and `refresh_transaction`
                    // below is what corrects it.
                    Err(error) => tab.error = Some(error.clone()),
                }
                // A boundary is a statement the user ran on this
                // connection, so the history keeps it: "why did my work
                // disappear" is answered by a `ROLLBACK` in the list.
                this.record_run(
                    statement,
                    RunSource::User,
                    &Outcome {
                        elapsed: Some(started.elapsed().as_millis()),
                        rows: outcome.is_ok().then_some(0),
                        error: outcome.err(),
                    },
                );
                this.reload_open_history(cx);
                this.refresh_transaction(tab_id, cx);
                cx.notify();
            })
            .ok();
        })
        .detach();
    }

    /// Ask the server what it actually spent on the statement this tab just
    /// ran, and fill it into the timing already on screen.
    ///
    /// **It is asked after the result, never before it.** The number is
    /// worth having and not worth waiting for: a round trip in front of the
    /// grid would add lag to the very measurement the user opened this to
    /// understand. So the result paints on the client's own reckoning, and
    /// the server's figure replaces the estimate a moment later — the strip
    /// drops its `~` and nothing else moves.
    ///
    /// The generation is checked twice over, as every reply to this tab is:
    /// a slow answer must not land on the run after the one it describes.
    fn refresh_server_timing(&mut self, tab_id: u64, generation: u64, cx: &mut Context<Self>) {
        let Some(Tab::Query(tab)) = self.tab_mut(tab_id) else { return };
        let Some(session) = tab.session.clone() else { return };
        cx.spawn(async move |this, cx| {
            let Ok(task) = this.update(cx, |_, cx| {
                gpui_tokio::Tokio::spawn(cx, async move { session.server_timing().await })
            }) else {
                return;
            };
            // Nothing to say is the ordinary answer here — no extension, or
            // a statement the server has not counted — and it is not worth
            // a word on screen. The client's own split stands.
            let Ok(Some(server)) = flatten(task.await) else { return };
            this.update(cx, |this, cx| {
                let Some(Tab::Query(tab)) = this.tab_mut(tab_id) else { return };
                if tab.generation != generation {
                    return;
                }
                if let Some(timing) = &mut tab.timing {
                    timing.server = Some(server);
                    cx.notify();
                }
            })
            .ok();
        })
        .detach();
    }

    /// Repaint while any run is out, so the toolbar's timer moves. One
    /// timer for the window, not one per run: `timing` is what stops a
    /// second run from starting a second loop, and the loop ends itself
    /// when the last run comes back.
    fn start_timer(&mut self, cx: &mut Context<Self>) {
        if self.timing {
            return;
        }
        self.timing = true;
        cx.spawn(async move |this, cx| {
            loop {
                cx.background_executor().timer(TIMER_TICK).await;
                let more = this.update(cx, |this, cx| {
                    let more = this.any_run_in_flight();
                    if !more {
                        this.timing = false;
                    }
                    cx.notify();
                    more
                });
                if !matches!(more, Ok(true)) {
                    break;
                }
            }
        })
        .detach();
    }

    fn any_run_in_flight(&self) -> bool {
        self.tabs.iter().any(|tab| match tab {
            Tab::Query(tab) => tab.run.in_flight(),
            _ => false,
        })
    }

    // --- query history ---------------------------------------------------

    /// Remember a run in the local file. A history that cannot be written
    /// is not worth interrupting a session over, so the error is dropped.
    fn record_run(&self, statement: &str, source: RunSource, outcome: &Outcome) {
        let Some(store) = &self.store else { return };
        // A blank buffer is not a run worth keeping.
        if statement.trim().is_empty() {
            return;
        }
        store
            .record_query(NewRun {
                scope: &self.scope,
                statement: statement.trim(),
                ran_at: unix_now(),
                source,
                elapsed_ms: outcome.elapsed.map(|millis| millis as u64),
                row_count: outcome.rows,
                error: outcome.error.as_deref(),
            })
            .ok();
    }

    /// Focus the history tab, opening it if this session has none. One
    /// history tab is enough: it is a view of one file, not of a query.
    fn open_history(&mut self, cx: &mut Context<Self>) {
        if let Some(ix) = self.tabs.iter().position(|tab| matches!(tab, Tab::History(_))) {
            self.activate(ix, cx);
            let id = self.tabs[ix].id();
            self.load_history(id, cx);
            return;
        }
        let id = self.take_id();
        self.tabs.push(Tab::History(HistoryTab {
            id,
            rows: Rc::new(Vec::new()),
            runs: 0,
            user_only: false,
            errors_only: false,
            error: None,
            scroll: UniformListScrollHandle::new(),
        }));
        self.activate(self.tabs.len() - 1, cx);
        self.load_history(id, cx);
    }

    /// Read the runs back and flatten them. Reading is a local SQLite
    /// call, so it stays on this thread; the tokio bridge is for the
    /// database, not for a file next to the profiles.
    fn load_history(&mut self, tab_id: u64, cx: &mut Context<Self>) {
        let scope = self.scope.clone();
        let Some(Tab::History(tab)) = self.tab_mut(tab_id) else { return };
        let filter = HistoryFilter {
            user_only: tab.user_only,
            errors_only: tab.errors_only,
            since: Some(unix_now() - HISTORY_DAYS * 24 * 60 * 60),
            ..Default::default()
        };

        if self.store.is_none() {
            let reason = self.store_error.clone().unwrap_or_else(|| "no history file".into());
            if let Some(Tab::History(tab)) = self.tab_mut(tab_id) {
                tab.error = Some(reason);
            }
            cx.notify();
            return;
        }
        let read = self.store.as_ref().expect("checked above").list_history(&scope, filter);
        let Some(Tab::History(tab)) = self.tab_mut(tab_id) else { return };
        match read {
            Ok(runs) => {
                let rows = history::flatten(&runs, history::today());
                tab.runs = runs.len();
                tab.rows = Rc::new(rows);
                tab.error = None;
            }
            Err(error) => {
                tab.rows = Rc::new(Vec::new());
                tab.runs = 0;
                tab.error = Some(error.to_string());
            }
        }
        cx.notify();
    }

    /// Keep an open history tab current when a run finishes behind it.
    fn reload_open_history(&mut self, cx: &mut Context<Self>) {
        let Some(id) = self
            .tabs
            .iter()
            .find(|tab| matches!(tab, Tab::History(_)))
            .map(|tab| tab.id())
        else {
            return;
        };
        self.load_history(id, cx);
    }

    fn toggle_history_filter(&mut self, tab_id: u64, errors: bool, cx: &mut Context<Self>) {
        let Some(Tab::History(tab)) = self.tab_mut(tab_id) else { return };
        if errors {
            tab.errors_only = !tab.errors_only;
        } else {
            tab.user_only = !tab.user_only;
        }
        self.load_history(tab_id, cx);
    }

    // --- the ⌘K palette --------------------------------------------------

    fn toggle_palette(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.palette.is_some() {
            self.close_palette(window, cx);
        } else {
            self.open_palette(window, cx);
        }
    }

    /// Open the palette on everything the session already holds: the
    /// catalog it introspected, and the runs it reads back here. Reading
    /// the runs is a local SQLite call, so it stays on this thread — the
    /// tokio bridge is for the database, not for the file beside it.
    fn open_palette(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        // One overlay at a time: the palette takes the focus, so a
        // column-find popover or a value peek left open would sit under it
        // holding keys nobody can see them taking.
        self.column_find = None;
        self.peek = None;
        let query = cx.new(|cx| {
            TextField::new("Search tables and history…", cx)
                .bare(palette::INPUT_FONT_SIZE)
        });
        let subscriptions = vec![cx.subscribe_in(&query, window, Self::on_palette_event)];
        window.focus(&query.focus_handle(cx), cx);

        let runs = self
            .store
            .as_ref()
            .and_then(|store| store.list_history(&self.scope, HistoryFilter::default()).ok())
            .unwrap_or_default();

        self.palette = Some(Palette {
            query,
            chip: Scope::All,
            runs: Rc::new(runs),
            rows: Rc::new(Vec::new()),
            matches: 0,
            selected: 0,
            scroll: UniformListScrollHandle::new(),
            _subscriptions: subscriptions,
        });
        self.rebuild_palette(cx);
    }

    fn close_palette(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.palette = None;
        // The workspace must take the focus back, or the key bindings have
        // nowhere to dispatch once the palette is gone.
        window.focus(&self.focus_handle, cx);
        cx.notify();
    }

    /// Search again and keep the selection on a result. The rows change
    /// under the selection with every keystroke, so it goes back to the
    /// first result rather than trying to follow what was there before.
    fn rebuild_palette(&mut self, cx: &mut Context<Self>) {
        let Some(palette) = &self.palette else { return };
        let typed = palette.query.read(cx).text().to_string();
        let (scope, needle) = palette::parse(&typed, palette.chip);
        let found = palette::build(
            self.catalog.as_ref(),
            &palette.runs,
            scope,
            needle,
            history::today(),
        );

        let Some(palette) = &mut self.palette else { return };
        palette.selected = found.rows.iter().position(|row| row.pick().is_some()).unwrap_or(0);
        palette.rows = Rc::new(found.rows);
        palette.matches = found.matches;
        palette.scroll.scroll_to_item(palette.selected, ScrollStrategy::Top);
        self.update_ghost(cx);
        cx.notify();
    }

    /// Show what ⇥ would finish the line with, faint and after the value.
    ///
    /// `palette::ghost` decides the shape: the rest of the word when the
    /// completion carries on from what was typed, and `⇥ <name>` when
    /// taking it would rewrite the line instead.
    fn update_ghost(&mut self, cx: &mut Context<Self>) {
        let Some(palette) = &self.palette else { return };
        let typed = palette.query.read(cx).text().to_string();
        let (_, needle) = palette::parse(&typed, palette.chip);
        let ghost = palette::completion(&palette.rows, palette.selected, needle)
            .map(|completed| palette::ghost(needle, &completed))
            .unwrap_or_default();
        palette.query.update(cx, |field, cx| field.set_ghost(ghost, cx));
    }

    /// Take the selected row's real name into the search line, so the next
    /// thing typed narrows inside it. Reports whether there was anything
    /// to take.
    fn complete_palette(&mut self, cx: &mut Context<Self>) -> bool {
        let Some(palette) = &self.palette else { return false };
        let typed = palette.query.read(cx).text().to_string();
        let (_, needle) = palette::parse(&typed, palette.chip);
        let Some(completed) = palette::completion(&palette.rows, palette.selected, needle) else {
            return false;
        };
        // Whatever came before the needle — a `t:` and any space after it
        // — is the user's and stays.
        let kept = &typed[..typed.len() - needle.len()];
        let line = format!("{kept}{completed}");
        // Setting the text emits `Changed`, which searches again, so the
        // list is already following the completed line by the time this
        // returns.
        palette.query.update(cx, |field, cx| field.set_text(line, cx));
        true
    }

    /// Move the selection to the next result in that direction, stepping
    /// over the headings. It stops at the ends rather than wrapping: a
    /// list that jumps back to the top loses the reader's place.
    fn step_palette(&mut self, forward: bool, cx: &mut Context<Self>) {
        let Some(palette) = &mut self.palette else { return };
        let mut ix = palette.selected;
        loop {
            let next = if forward { ix + 1 } else { ix.checked_sub(1).unwrap_or(usize::MAX) };
            let Some(row) = palette.rows.get(next) else { return };
            ix = next;
            if row.pick().is_some() {
                break;
            }
        }
        palette.selected = ix;
        palette.scroll.scroll_to_item(ix, ScrollStrategy::Center);
        // ⇥ finishes the line from the selection, so moving it changes
        // what the hint offers.
        self.update_ghost(cx);
        cx.notify();
    }

    fn set_palette_scope(&mut self, chip: Scope, window: &mut Window, cx: &mut Context<Self>) {
        let Some(palette) = &mut self.palette else { return };
        palette.chip = chip;
        // A chip is only meaningful while the line has the focus.
        let query = palette.query.clone();
        window.focus(&query.focus_handle(cx), cx);
        self.rebuild_palette(cx);
    }

    /// Open what the selection points at.
    fn open_palette_selection(&mut self, new_tab: bool, window: &mut Window, cx: &mut Context<Self>) {
        let Some(palette) = &self.palette else { return };
        let Some(pick) = palette.rows.get(palette.selected).and_then(|row| row.pick()) else {
            return;
        };
        self.open_pick(pick, new_tab, window, cx);
    }

    fn open_pick(&mut self, pick: Pick, new_tab: bool, window: &mut Window, cx: &mut Context<Self>) {
        // The palette closes first, so the tab it opens is the thing that
        // holds the focus afterwards.
        self.close_palette(window, cx);
        match pick {
            // A relation opens the way a click in the sidebar opens one: a
            // query tab on `SELECT * FROM ... LIMIT 500;`, run at once. The
            // palette answers "show me this table", and the answer is its
            // rows with an editable statement over them.
            Pick::Table { schema, table } => {
                self.browse_table_in(&schema, &table, new_tab, window, cx)
            }
            // A run comes back editable, never re-run behind the user, for
            // the same reason the history screen opens one that way.
            Pick::Query(statement) => self.new_query_with(&statement, window, cx),
        }
    }

    fn on_palette_event(
        &mut self,
        _field: &Entity<TextField>,
        event: &TextFieldEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            TextFieldEvent::Changed => self.rebuild_palette(cx),
            TextFieldEvent::Submit => self.open_palette_selection(false, window, cx),
            TextFieldEvent::Cancel => self.close_palette(window, cx),
            // Tab finishes the line from the selected row. With nothing
            // left to finish it walks the scope chips instead, the way it
            // walks the fields of the connection form.
            TextFieldEvent::NextField => {
                if self.complete_palette(cx) {
                    return;
                }
                let Some(palette) = &self.palette else { return };
                let here = Scope::ALL.iter().position(|scope| *scope == palette.chip).unwrap_or(0);
                let next = Scope::ALL[(here + 1) % Scope::ALL.len()];
                self.set_palette_scope(next, window, cx);
            }
        }
    }

    // --- finding a column -------------------------------------------------

    /// ⌘J. A result wider than the pane is the ordinary case for a real
    /// table, and the name of the column is usually all the user knows about
    /// the one they are looking for — so the way to it is a search over the
    /// names, not a scroll along the header.
    fn toggle_column_find(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        // The palette and the close dialog are each already the thing taking
        // keys while they are up.
        if self.palette.is_some() || self.confirm.is_some() {
            return;
        }
        if self.column_find.is_some() {
            self.close_column_find(window, cx);
        } else {
            self.open_column_find(window, cx);
        }
    }

    fn open_column_find(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(tab) = self.tabs.get(self.active).map(Tab::id) else { return };
        // A tab with no result has no column to jump to. Nothing is said
        // about it: the button is not there either.
        if self.result_columns().is_empty() {
            return;
        }
        let query = cx.new(|cx| TextField::new("find column", cx).bare(COLUMN_FIND_FONT_SIZE));
        let subscriptions = vec![cx.subscribe_in(&query, window, Self::on_column_find_event)];
        window.focus(&query.focus_handle(cx), cx);
        self.column_find = Some(ColumnFind {
            tab,
            query,
            selected: 0,
            scroll: UniformListScrollHandle::new(),
            _subscriptions: subscriptions,
        });
        cx.notify();
    }

    /// Close it and give the keys back to the tab, so ⎋ leaves the user
    /// where they were rather than nowhere.
    fn close_column_find(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.column_find = None;
        self.focus_active_tab(window, cx);
        cx.notify();
    }

    /// The column names of the result on screen. Both kinds of tab that show
    /// one answer here; a history tab shows a list of runs, so it has none.
    fn result_columns(&self) -> &[String] {
        match self.tabs.get(self.active) {
            Some(Tab::Table(tab)) => &tab.data.columns,
            Some(Tab::Query(tab)) => &tab.data.columns,
            Some(Tab::History(_)) | None => &[],
        }
    }

    /// Which lanes the typed line names, as indices into the result. Worked
    /// out rather than remembered — see [`ColumnFind`].
    fn column_matches(&self, cx: &App) -> Vec<usize> {
        let Some(find) = &self.column_find else { return Vec::new() };
        find_columns(self.result_columns(), find.query.read(cx).text())
    }

    /// ↑↓ walk what the search found. They stop at the ends rather than
    /// wrapping, as the palette's list does: a list that jumps back to the
    /// top loses the reader's place.
    fn step_column_find(&mut self, forward: bool, cx: &mut Context<Self>) {
        let count = self.column_matches(cx).len();
        let Some(find) = &mut self.column_find else { return };
        if count == 0 {
            return;
        }
        let here = find.selected.min(count - 1);
        let next = if forward { (here + 1).min(count - 1) } else { here.saturating_sub(1) };
        find.selected = next;
        find.scroll.scroll_to_item(next, ScrollStrategy::Nearest);
        cx.notify();
    }

    /// ⏎: jump to the row the list has selected.
    fn jump_to_selected_column(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let matches = self.column_matches(cx);
        let Some(find) = &self.column_find else { return };
        let Some(column) = matches.get(find.selected.min(matches.len().saturating_sub(1))).copied()
        else {
            return;
        };
        self.jump_to_column(column, window, cx);
    }

    /// Put the cursor in one column of the result and bring it into view,
    /// **keeping the row it is already on**: the user asked for a column,
    /// not for a cell somewhere else in the result.
    fn jump_to_column(&mut self, column: usize, window: &mut Window, cx: &mut Context<Self>) {
        self.column_find = None;
        let Some(marked) = self.tabs.get_mut(self.active).and_then(Tab::marked) else { return };
        let extent = Extent::of(marked.data);
        // The result can have been replaced since the list was painted, so
        // the lane is checked against the result in hand rather than trusted.
        if column >= extent.columns || extent.rows == 0 {
            return;
        }
        let row = marked.selection.cursor().map_or(0, |cursor| cursor.row);
        let cell = Cell::new(row.min(extent.rows - 1), column);
        marked.selection.focus(cell);
        marked.scroll.reveal(cell, marked.data);
        // A jump hands the keys to the result it jumped in, so ↑↓ walk the
        // grid from the cell it landed on.
        window.focus(&self.grid_focus, cx);
        cx.notify();
    }

    fn on_column_find_event(
        &mut self,
        _field: &Entity<TextField>,
        event: &TextFieldEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            // The list is read straight off the line, so a keystroke only
            // has to put the selection back on the first match — the rows
            // change under it with every character.
            TextFieldEvent::Changed => {
                if let Some(find) = &mut self.column_find {
                    find.selected = 0;
                    find.scroll.scroll_to_item(0, ScrollStrategy::Top);
                }
                cx.notify();
            }
            TextFieldEvent::Submit => self.jump_to_selected_column(window, cx),
            TextFieldEvent::Cancel => self.close_column_find(window, cx),
            // ⇥ has nothing to finish here. The palette completes a path one
            // part at a time because a name is deep; a column name is one
            // part, and the whole list of them is already on screen.
            TextFieldEvent::NextField => {}
        }
    }

    // --- reading a whole value ---------------------------------------------

    /// ⏎ over the cursor's cell, and a double click on any cell: show the
    /// value whole.
    fn open_peek(&mut self, cell: Cell, window: &mut Window, cx: &mut Context<Self>) {
        // Each of those is already the thing taking keys while it is up.
        if self.palette.is_some() || self.confirm.is_some() {
            return;
        }
        let Some(tab) = self.tabs.get(self.active).map(Tab::id) else { return };
        self.column_find = None;
        let focus = cx.focus_handle();
        window.focus(&focus, cx);
        // A focusable element focuses itself on mouse down, and the bubble
        // phase runs a child's handlers before its parents' — so on a double
        // click the cell opens this card and the grid pane around it, which
        // tracks `grid_focus`, would then take the focus straight back and
        // leave ⎋ meaning "clear the selection". `prevent_default` is how an
        // element says the focus is already placed; the flag is reset on the
        // next input, so it costs nothing on the ⏎ path.
        window.prevent_default();
        self.peek = Some(Peek { tab, cell, focus });
        // Nothing worth showing means nothing shown: an out-of-range cell
        // is a cell the result no longer has.
        if self.peek_value().is_none() {
            self.peek = None;
            window.focus(&self.grid_focus, cx);
            return;
        }
        cx.notify();
    }

    /// ⏎ from the grid: the cursor's own cell.
    fn peek_cursor(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(cell) = self.tabs.get(self.active).and_then(Tab::selection).and_then(Selection::cursor)
        else {
            return;
        };
        self.open_peek(cell, window, cx);
    }

    /// What the card is showing: the column's name, the row's number as the
    /// gutter counts it, and the value.
    ///
    /// Read from the result in hand every time, never remembered — see
    /// [`Peek`]. `None` says there is nothing there to show, which is what a
    /// page turn under an open card leaves behind.
    fn peek_value(&self) -> Option<(SharedString, usize, String)> {
        let peek = self.peek.as_ref()?;
        let tab = self.tabs.get(self.active).filter(|tab| tab.id() == peek.tab)?;
        let (data, first_row) = match tab {
            // A table tab is a window on the table, so the row wears the
            // number its gutter gives it rather than its index in the page.
            Tab::Table(tab) => (&tab.data, tab.page * PAGE_SIZE + 1),
            Tab::Query(tab) => (&tab.data, 1),
            Tab::History(_) => return None,
        };
        let value = data.rows.get(peek.cell.row)?.get(peek.cell.column)?;
        let name = data.columns.get(peek.cell.column)?;
        Some((name.clone().into(), first_row + peek.cell.row, value.display()))
    }

    fn close_peek(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.peek = None;
        // Back to the result the card was opened over, not to the tab's
        // usual focus: the user was walking the grid a keystroke ago.
        window.focus(&self.grid_focus, cx);
        cx.notify();
    }

    /// ⌘C in the card copies **this value**, whole — not the slice the card
    /// painted, and not the selection the grid's own ⌘C would take.
    ///
    /// It is written bare, with none of CSV's quoting: a value read on its
    /// own is not a row, so a URL with a comma in it must not come back
    /// wrapped in quotes it never had.
    fn copy_peek(&mut self, cx: &mut Context<Self>) {
        let Some((_, _, value)) = self.peek_value() else { return };
        cx.write_to_clipboard(ClipboardItem::new_string(value));
    }

    // --- walking the tabs -------------------------------------------------

    /// ⌃⇥ moves to the next tab, ⌃⇧⇥ to the one before it, in the order
    /// the strip paints them. There is no popup: the switch happens on
    /// the keystroke, and the strip is the list.
    ///
    /// It stays out of the way while the palette is up: that dialog is
    /// already the one taking keys.
    fn step_tab(&mut self, forward: bool, window: &mut Window, cx: &mut Context<Self>) {
        if self.palette.is_some() || self.tabs.len() < 2 {
            return;
        }
        self.activate(step_wrapping(self.tabs.len(), self.active, forward), cx);
        self.focus_active_tab(window, cx);
        cx.notify();
    }

    // --- closing a session -----------------------------------------------

    /// Ask before a close that would end work the server is still doing.
    ///
    /// `true` means go ahead now. `false` means the dialog is up, and the
    /// close happens — or does not — when the user answers. **Every way out
    /// calls this**, which is what makes the guard true rather than a
    /// warning the ⌘W path happens to show.
    ///
    /// A close that loses nothing never asks. Only a run in flight counts:
    /// an editor's buffer is already written back, and a tab with a result
    /// on screen loses a result the statement above it will fetch again.
    pub fn guard_close(
        &mut self,
        what: Close,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if self.confirm.is_some() {
            // One question at a time. A second ⌘W while the dialog is up is
            // the user repeating themselves, not asking something new.
            return false;
        }
        let (running, open) = self.work_ended_by(what);
        if running.is_empty() && open.is_empty() {
            return true;
        }
        // The palette must not be left underneath: two overlays both taking
        // keys, and only one of them the one being answered. The column-find
        // popover and the value peek go for the same reason.
        if self.palette.is_some() {
            self.close_palette(window, cx);
        }
        self.column_find = None;
        self.peek = None;
        let focus = cx.focus_handle();
        window.focus(&focus, cx);
        self.confirm = Some(Confirm { what, running, open, focus });
        cx.notify();
        false
    }

    /// What a close would end, by tab name: the runs still out, and the
    /// transactions still open. A tab can be in both lists.
    fn work_ended_by(&self, what: Close) -> (Vec<SharedString>, Vec<SharedString>) {
        let mut running = Vec::new();
        let mut open = Vec::new();
        for tab in self.tabs.iter().filter(|tab| match what {
            Close::Tab(id) => tab.id() == id,
            Close::Shell | Close::Window => true,
        }) {
            let Tab::Query(tab) = tab else { continue };
            if tab.run.in_flight() {
                running.push(tab.title.clone());
            }
            if tab.in_transaction {
                open.push(tab.title.clone());
            }
        }
        (running, open)
    }

    /// Ask the server to give up every run this close would end, and hand
    /// the requests back rather than detaching them here: quitting has to
    /// wait for them, and closing a tab must not.
    ///
    /// The backend id lands one round trip after a run starts, so a close
    /// inside that window finds no id and cannot ask. It is a race the
    /// stop button waits out and this cannot — the tab is going away — and
    /// the session work closes it for good, because a session knows its
    /// backend from the moment it opens.
    fn cancel_runs(&mut self, what: Close, cx: &mut Context<Self>) -> Vec<StopTask> {
        let Some(connection) = self.connection.clone() else { return Vec::new() };
        let mut stops = Vec::new();
        for tab in self.tabs.iter_mut().filter(|tab| match what {
            Close::Tab(id) => tab.id() == id,
            Close::Shell | Close::Window => true,
        }) {
            let Tab::Query(tab) = tab else { continue };
            let Some(live) = tab.run.live() else { continue };
            let Some(id) = live.backend else {
                // The session is still opening, so nothing is out on the
                // server. Dropping the tab is all the stopping there is.
                continue;
            };
            // The tab says CANCELLING while the request is out. It matters
            // on the way to a quit, where the tabs stay on screen until the
            // cancels land — closing a tab takes the tab with it, and this
            // says nothing to nobody.
            tab.run = Run::Cancelling(*live);
            let connection = connection.clone();
            stops.push(gpui_tokio::Tokio::spawn(cx, async move {
                connection.stop(id, Stop::Cancel).await
            }));
        }
        stops
    }

    /// Do the close the user agreed to.
    fn proceed_close(&mut self, what: Close, window: &mut Window, cx: &mut Context<Self>) {
        let stops = self.cancel_runs(what, cx);
        match what {
            Close::Tab(id) => {
                // Detached, because a tab closing must not wait on the
                // network. Dropping the task instead would abort it —
                // `Tokio::spawn` cancels its future when the handle goes.
                for stop in stops {
                    stop.detach();
                }
                self.close_tab(id, window, cx);
            }
            Close::Shell => {
                for stop in stops {
                    stop.detach();
                }
                // The shell is dropped from here, and with it every editor
                // buffer. Write the strip back while there is still
                // something to read it from.
                self.remember_tabs(cx);
                cx.emit(ShellEvent::Close);
            }
            Close::Window => {
                self.remember_tabs(cx);
                // This one *is* waited for. Quitting drops the tokio
                // runtime, so a cancel that has not left yet never leaves,
                // and the statement outlives the app that started it.
                cx.spawn(async move |_, cx| {
                    for stop in stops {
                        stop.await.ok();
                    }
                    cx.update(|cx| cx.quit());
                })
                .detach();
            }
        }
    }

    fn close_confirm(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.confirm = None;
        // The workspace takes the focus back, or the shell's keys have
        // nowhere to dispatch once the dialog is gone.
        self.focus_active_tab(window, cx);
        cx.notify();
    }

    /// Close one tab and hand the focus to whatever takes its place. The
    /// active index counts tabs, not ids, so closing a tab to the left of
    /// the active one has to walk it back or the selection jumps.
    fn close_tab(&mut self, tab_id: u64, window: &mut Window, cx: &mut Context<Self>) {
        let Some(ix) = self.tabs.iter().position(|tab| tab.id() == tab_id) else { return };
        let closed = self.tabs.remove(ix);
        // The tab's connection goes back to the pool, so it is rolled back
        // first: whatever acquires it next must not inherit a transaction
        // the user left open here. Leaving the shell needs none of this —
        // the pool dies with it, and a closing socket is itself a rollback.
        if let Tab::Query(closed) = &closed {
            if let Some(session) = closed.session.clone() {
                gpui_tokio::Tokio::spawn(cx, async move { session.close().await }).detach();
            }
        }
        if ix < self.active {
            self.active -= 1;
        }
        self.activate(self.active.min(self.tabs.len().saturating_sub(1)), cx);
        // Closing the last tab leaves nothing to activate, so the strip is
        // written back here rather than only from `activate`.
        self.remember_tabs(cx);
        self.focus_active_tab(window, cx);
        cx.notify();
    }

    /// Make one tab the active one. Every path that changes the active tab
    /// goes through here, which is also what makes this the place the
    /// strip is written back from.
    fn activate(&mut self, ix: usize, cx: &App) {
        if self.tabs.get(ix).is_none() {
            return;
        }
        // The column-find popover and the value peek are both controls on
        // one result. Every caller here hands the focus on to the tab it
        // activated, so they go rather than being carried to a result they
        // were never opened on.
        self.column_find = None;
        self.peek = None;
        self.active = ix;
        self.remember_tabs(cx);
    }

    /// A query tab types into its editor, so it wants the focus itself; a
    /// table tab is a result and nothing else, so it opens with the grid
    /// focused and its keys live at once. Everything else leaves the focus
    /// on the shell, where the shell's own keys are bound.
    fn focus_active_tab(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        match self.tabs.get(self.active) {
            Some(Tab::Query(tab)) => window.focus(&tab.editor.focus_handle(cx), cx),
            Some(Tab::Table(_)) => window.focus(&self.grid_focus, cx),
            _ => window.focus(&self.focus_handle, cx),
        }
    }

    fn step_page(&mut self, forward: bool, cx: &mut Context<Self>) {
        let Some(Tab::Table(tab)) = self.tabs.get(self.active) else { return };
        if tab.loading {
            return;
        }
        let (id, page) = (tab.id, tab.page);
        let next = if forward {
            // Only offer the next page when this one came back full.
            if tab.data.rows.len() < PAGE_SIZE {
                return;
            }
            page + 1
        } else {
            if page == 0 {
                return;
            }
            page - 1
        };
        self.load_page(id, next, cx);
    }

    fn refresh_active(&mut self, cx: &mut Context<Self>) {
        match self.tabs.get(self.active) {
            Some(Tab::Table(tab)) => {
                let (id, page) = (tab.id, tab.page);
                self.load_page(id, page, cx);
            }
            Some(Tab::Query(_)) => self.run_active_query(cx),
            // Refreshing the history re-reads the local file, which is
            // what ⌘R means on every other tab: show me this again.
            Some(Tab::History(tab)) => {
                let id = tab.id;
                self.load_history(id, cx);
            }
            None => {}
        }
    }

    fn tab_mut(&mut self, tab_id: u64) -> Option<&mut Tab> {
        self.tabs.iter_mut().find(|tab| tab.id() == tab_id)
    }

    fn take_id(&mut self) -> u64 {
        let id = self.next_id;
        self.next_id += 1;
        id
    }

    // --- the selection ---------------------------------------------------

    /// Move the cursor, and take the scroll position with it.
    ///
    /// One function for every way the cursor moves — the arrow keys and ⇧
    /// with them — so a cursor that has walked off the edge of the pane is
    /// always brought back into view, whichever key moved it.
    fn step_cursor(&mut self, step: Step, extend: bool, cx: &mut Context<Self>) {
        let Some(marked) = self.tabs.get_mut(self.active).and_then(Tab::marked) else { return };
        let extent = Extent::of(marked.data);
        let Some(cell) = marked.selection.step(step, extend, extent) else { return };
        marked.scroll.reveal(cell, marked.data);
        cx.notify();
    }

    // --- actions ---------------------------------------------------------

    fn on_run_query(&mut self, _: &RunQuery, _: &mut Window, cx: &mut Context<Self>) {
        self.run_active_query(cx);
    }

    fn on_select_up(&mut self, _: &SelectUp, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::Up, false, cx);
    }

    fn on_select_down(&mut self, _: &SelectDown, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::Down, false, cx);
    }

    fn on_select_left(&mut self, _: &SelectLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::Left, false, cx);
    }

    fn on_select_right(&mut self, _: &SelectRight, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::Right, false, cx);
    }

    fn on_extend_up(&mut self, _: &ExtendUp, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::Up, true, cx);
    }

    fn on_extend_down(&mut self, _: &ExtendDown, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::Down, true, cx);
    }

    fn on_extend_left(&mut self, _: &ExtendLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::Left, true, cx);
    }

    fn on_extend_right(&mut self, _: &ExtendRight, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::Right, true, cx);
    }

    fn on_select_row_start(&mut self, _: &SelectRowStart, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::RowStart, false, cx);
    }

    fn on_select_row_end(&mut self, _: &SelectRowEnd, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::RowEnd, false, cx);
    }

    fn on_select_first_row(&mut self, _: &SelectFirstRow, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::First, false, cx);
    }

    fn on_select_last_row(&mut self, _: &SelectLastRow, _: &mut Window, cx: &mut Context<Self>) {
        self.step_cursor(Step::Last, false, cx);
    }

    fn on_select_all(&mut self, _: &SelectAll, _: &mut Window, cx: &mut Context<Self>) {
        let Some(marked) = self.tabs.get_mut(self.active).and_then(Tab::marked) else { return };
        let extent = Extent::of(marked.data);
        marked.selection.select_all(extent);
        cx.notify();
    }

    /// ⌘C. Nothing marked copies nothing rather than the whole result: a
    /// copy that took 500 rows because the user pressed the key with an
    /// empty selection is a copy nobody asked for.
    fn on_copy_selection(&mut self, _: &CopySelection, _: &mut Window, cx: &mut Context<Self>) {
        let Some(marked) = self.tabs.get_mut(self.active).and_then(Tab::marked) else { return };
        let Some(text) = clipboard_text(marked.data, marked.selection) else { return };
        cx.write_to_clipboard(ClipboardItem::new_string(text));
    }

    /// Space ticks the row the cursor is on, so a run of rows can be picked
    /// without the mouse: ↓ then space, down the result.
    fn on_toggle_pick(&mut self, _: &TogglePick, _: &mut Window, cx: &mut Context<Self>) {
        let Some(marked) = self.tabs.get_mut(self.active).and_then(Tab::marked) else { return };
        let Some(cursor) = marked.selection.cursor() else { return };
        marked.selection.toggle_pick(cursor.row);
        cx.notify();
    }

    /// ⎋ drops what is marked.
    fn on_clear_selection(&mut self, _: &ClearSelection, _: &mut Window, cx: &mut Context<Self>) {
        let Some(marked) = self.tabs.get_mut(self.active).and_then(Tab::marked) else { return };
        marked.selection.clear();
        cx.notify();
    }

    fn on_stop_query(&mut self, _: &StopQuery, _: &mut Window, cx: &mut Context<Self>) {
        self.stop_active_query(cx);
    }

    fn on_commit_transaction(
        &mut self,
        _: &CommitTransaction,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.end_transaction(TxEnd::Commit, cx);
    }

    fn on_rollback_transaction(
        &mut self,
        _: &RollbackTransaction,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.end_transaction(TxEnd::Rollback, cx);
    }

    fn on_new_query(&mut self, _: &NewQuery, window: &mut Window, cx: &mut Context<Self>) {
        self.new_query(window, cx);
    }

    fn on_close_tab(&mut self, _: &CloseTab, window: &mut Window, cx: &mut Context<Self>) {
        let Some(tab) = self.tabs.get(self.active) else { return };
        let id = tab.id();
        if self.guard_close(Close::Tab(id), window, cx) {
            self.close_tab(id, window, cx);
        }
    }

    fn on_confirm_close(&mut self, _: &ConfirmClose, window: &mut Window, cx: &mut Context<Self>) {
        let Some(confirm) = self.confirm.take() else { return };
        cx.notify();
        self.proceed_close(confirm.what, window, cx);
    }

    fn on_cancel_close(&mut self, _: &CancelClose, window: &mut Window, cx: &mut Context<Self>) {
        self.close_confirm(window, cx);
    }

    fn on_refresh(&mut self, _: &Refresh, _: &mut Window, cx: &mut Context<Self>) {
        self.refresh_active(cx);
    }

    fn on_prev_page(&mut self, _: &PrevPage, _: &mut Window, cx: &mut Context<Self>) {
        self.step_page(false, cx);
    }

    fn on_next_page(&mut self, _: &NextPage, _: &mut Window, cx: &mut Context<Self>) {
        self.step_page(true, cx);
    }

    fn on_show_history(&mut self, _: &ShowHistory, window: &mut Window, cx: &mut Context<Self>) {
        self.open_history(cx);
        // The history tab leaves the focus on the shell, where the shell's
        // own keys are bound. Saying so here matters because the tab this
        // one replaces may have been holding the focus in a popover.
        self.focus_active_tab(window, cx);
    }

    fn on_find_column(&mut self, _: &FindColumn, window: &mut Window, cx: &mut Context<Self>) {
        self.toggle_column_find(window, cx);
    }

    fn on_filter_catalog(
        &mut self,
        _: &FilterCatalog,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.focus_catalog_filter(window, cx);
    }

    fn on_column_prev(&mut self, _: &ColumnPrev, _: &mut Window, cx: &mut Context<Self>) {
        self.step_column_find(false, cx);
    }

    fn on_column_next(&mut self, _: &ColumnNext, _: &mut Window, cx: &mut Context<Self>) {
        self.step_column_find(true, cx);
    }

    fn on_peek_value(&mut self, _: &PeekValue, window: &mut Window, cx: &mut Context<Self>) {
        self.peek_cursor(window, cx);
    }

    fn on_close_peek(&mut self, _: &ClosePeek, window: &mut Window, cx: &mut Context<Self>) {
        self.close_peek(window, cx);
    }

    fn on_copy_peek(&mut self, _: &CopyPeek, _: &mut Window, cx: &mut Context<Self>) {
        self.copy_peek(cx);
    }

    fn on_toggle_palette(
        &mut self,
        _: &palette::Toggle,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.toggle_palette(window, cx);
    }

    fn on_palette_prev(&mut self, _: &palette::SelectPrev, _: &mut Window, cx: &mut Context<Self>) {
        self.step_palette(false, cx);
    }

    fn on_palette_next(&mut self, _: &palette::SelectNext, _: &mut Window, cx: &mut Context<Self>) {
        self.step_palette(true, cx);
    }

    fn on_palette_new_tab(
        &mut self,
        _: &palette::OpenInNewTab,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.open_palette_selection(true, window, cx);
    }

    fn on_catalog_prev(&mut self, _: &CatalogPrev, _: &mut Window, cx: &mut Context<Self>) {
        self.step_catalog(false, cx);
    }

    fn on_catalog_next(&mut self, _: &CatalogNext, _: &mut Window, cx: &mut Context<Self>) {
        self.step_catalog(true, cx);
    }

    fn on_next_tab(&mut self, _: &NextTab, window: &mut Window, cx: &mut Context<Self>) {
        self.step_tab(true, window, cx);
    }

    fn on_prev_tab(&mut self, _: &PrevTab, window: &mut Window, cx: &mut Context<Self>) {
        self.step_tab(false, window, cx);
    }
}

/// Where one press of ↑ or ↓ lands in the sidebar's filtered list.
/// `stops` are the rows a cursor may sit on, and `from` is where it sits
/// now.
///
/// **The line the user is typing on is a position in this ring**, and it
/// is `None`. ↑ off the first name goes back to it and ↓ off the last name
/// comes round to it, so the way out of the list is the key that walked
/// into it. A cursor that could only be escaped by deleting the line would
/// be a trap, and the line is where the next keystroke has to land: the
/// user is still typing a name.
fn step_stop(stops: &[usize], from: Option<usize>, forward: bool) -> Option<usize> {
    if stops.is_empty() {
        return None;
    }
    let here = from.and_then(|ix| stops.iter().position(|stop| *stop == ix));
    match (here, forward) {
        (None, true) => stops.first().copied(),
        (None, false) => stops.last().copied(),
        (Some(ix), true) => stops.get(ix + 1).copied(),
        (Some(0), false) => None,
        (Some(ix), false) => stops.get(ix - 1).copied(),
    }
}

/// Where one step lands. It wraps at both ends, because the tabs are a
/// ring: one press past the last tab is how the user comes back to the
/// first.
fn step_wrapping(len: usize, from: usize, forward: bool) -> usize {
    if len == 0 {
        return 0;
    }
    if forward { (from + 1) % len } else { (from + len - 1) % len }
}

/// What a finished run is worth remembering by: the timing and the row
/// count on success, the message on failure.
struct Outcome {
    elapsed: Option<u128>,
    rows: Option<u64>,
    error: Option<String>,
}

/// Which connection a session's history belongs to. A saved connection
/// has an id already; a URL from the command line is keyed by the URL
/// with its password taken out, so the history file never holds one.
fn scope_of(target: &Target) -> String {
    match target {
        Target::Profile(profile) => profile.id.clone(),
        Target::Url(url) => format!("url:{}", redact(url)),
    }
}

/// Say when a run would send only part of the buffer.
fn run_scope(tab: &QueryTab, cx: &App) -> Option<SharedString> {
    tab.editor
        .read(cx)
        .selected_text()
        .map(|_| "runs the selection".into())
}

/// Every schema, relation and column in the catalog, so the query editor
/// can colour the names the database really has and complete them as the
/// user types.
///
/// Ownership is what makes `users.` offer that table's columns: a column
/// is owned by its relation, a relation by its schema.
fn vocabulary_of(catalog: &Catalog) -> Vocabulary {
    Vocabulary::new(catalog.schemas.iter().flat_map(|schema| {
        std::iter::once(Name {
            name: schema.name.clone(),
            detail: "schema".to_string(),
            kind: Kind::Schema,
            owner: None,
        })
        .chain(schema.tables.iter().flat_map(|table| {
            std::iter::once(Name {
                name: table.name.clone(),
                detail: match table.kind {
                    TableKind::Table => "table".to_string(),
                    TableKind::View => "view".to_string(),
                },
                kind: Kind::Relation,
                owner: Some(schema.name.clone()),
            })
            .chain(table.columns.iter().map(|column| Name {
                name: column.name.clone(),
                // The declared type, as the design's panel shows it.
                detail: column.data_type.clone(),
                kind: Kind::Column,
                owner: Some(table.name.clone()),
            }))
        }))
    }))
}

/// Run one statement on tokio and time it. Timing wraps the call itself,
/// so it measures the database, not the paint that follows.
fn run_sql(
    connection: Arc<dyn Connection>,
    sql: String,
    cx: &mut Context<Shell>,
) -> gpui::Task<Result<anyhow::Result<(QueryResult, u128)>, gpui_tokio::JoinError>> {
    gpui_tokio::Tokio::spawn(cx, async move {
        let started = Instant::now();
        let result = connection.execute(&sql).await?;
        anyhow::Ok((result, started.elapsed().as_millis()))
    })
}

/// What a run hands back: the tab's session, and how the statements went.
///
/// The two are separate because they fail separately. A statement that
/// errors leaves the session perfectly good — and still holding whatever
/// the user set on it — so the session comes back either way, and is
/// `None` only when opening it is what failed.
struct RunOutcome {
    session: Option<Arc<dyn Session>>,
    result: Result<(QueryResult, u128, usize), String>,
    /// Whether the run opened a transaction of its own, for a tab in
    /// [`TxMode::Manual`]. It is reported apart from the result because it
    /// is true on both paths: a statement that fails inside a transaction
    /// leaves it open, and that is the state most worth reporting.
    began: bool,
}

impl RunOutcome {
    /// The shell went away mid-run. Nothing will read this, and the pool
    /// it belonged to is going with it.
    fn abandoned() -> Self {
        Self { session: None, result: Err("the workspace closed".to_string()), began: false }
    }
}

/// What a run says for itself when ⌘. reached it before it had left. The
/// tab paints CANCELLED, as it does for a run the server gave up, because
/// it is the same answer to the same question.
const CALLED_OFF: &str = "the run was called off before it started";

/// Said when every session is busy or holding a transaction and this tab
/// wanted one too. It names the two ways out, because the app cannot pick
/// either of them on the user's behalf: both lose something.
const NO_SESSION_LEFT: &str =
    "every connection is in use — close a tab, or end a transaction, and run again";

/// Said when a commit or a rollback is asked for while a run is still out.
/// A session is one connection and the run has it, so the boundary would
/// have to queue behind the very statement the user may want to give up.
/// It names the way out rather than waiting.
const RUN_HOLDS_THE_SESSION: &str =
    "this tab is still running a statement — ⌘. stops it, then commit or roll back";

/// Run a buffer's statements in order and keep the last result that has
/// columns, so a trailing `create table` does not blank a grid the SELECT
/// before it filled. The first statement to fail stops the run and its
/// error is what the user sees.
///
/// **All of them go down one connection, in order**, so `BEGIN; SELECT …;`
/// in one buffer means what it reads as. Before sessions each statement
/// took whichever pooled connection was free, and the second one could not
/// see what the first had done.
fn run_statements(
    connection: Arc<dyn Connection>,
    session: Option<Arc<dyn Session>>,
    begin: bool,
    statements: Vec<String>,
    tab_id: u64,
    generation: u64,
    cx: &mut Context<Shell>,
) -> gpui::Task<RunOutcome> {
    cx.spawn(async move |this, cx| {
        // The tab's session, opening one if this is its first run. The
        // open is its own step rather than the first thing inside the run,
        // so the tab knows its backend before a statement is ever sent.
        let session = match session {
            Some(session) => session,
            None => {
                let opening = this.update(cx, |_, cx| {
                    gpui_tokio::Tokio::spawn(cx, async move { connection.open_session().await })
                });
                let Ok(opening) = opening else { return RunOutcome::abandoned() };
                match flatten(opening.await) {
                    Ok(session) => session,
                    Err(error) => {
                        return RunOutcome { session: None, result: Err(error), began: false };
                    }
                }
            }
        };

        // The tab learns where its session is the moment there is one, so
        // every stop and every close from here on has a backend to name.
        //
        // It is also where a run called off while the session was opening
        // ends. Nothing was sent, so calling it off costs the server
        // nothing — which is why a stop in that window needs no waiting.
        let go = this.update(cx, |this, cx| {
            let Some(Tab::Query(tab)) = this.tab_mut(tab_id) else { return false };
            if tab.generation != generation {
                return false;
            }
            tab.session = Some(session.clone());
            if let Run::Running(live) = &mut tab.run {
                live.backend = session.backend();
            }
            cx.notify();
            !matches!(tab.run, Run::Cancelling(_))
        });
        if !matches!(go, Ok(true)) {
            // Nothing was sent, so nothing was begun: a run called off in
            // this window costs the server nothing at all.
            return RunOutcome {
                session: Some(session),
                result: Err(CALLED_OFF.to_string()),
                began: false,
            };
        }

        let running = this.update(cx, |_, cx| {
            gpui_tokio::Tokio::spawn(cx, async move {
                let (began, outcome) = execute_all(&session, begin, &statements).await;
                (session, began, outcome)
            })
        });
        let Ok(running) = running else { return RunOutcome::abandoned() };
        match running.await {
            Ok((session, began, result)) => RunOutcome { session: Some(session), result, began },
            // The task itself died, so what reached the server is unknown.
            // `refresh_transaction` asks rather than guessing, and it is the
            // one thing here that can answer.
            Err(join) => RunOutcome {
                session: None,
                result: Err(format!("the query was interrupted: {join}")),
                began: false,
            },
        }
    })
}

/// Send a buffer's statements in turn, keeping the last result that has
/// columns. The first failure ends the run.
///
/// `begin` opens a transaction in front of them, for a tab in
/// [`TxMode::Manual`]. It is **not one of the statements**: it does not
/// count towards what the result line reports, and it is not what the
/// server is asked to time. The clock does start before it, because the
/// round trip is part of what the user waited.
///
/// The `bool` that comes back says the transaction was opened, which the
/// caller needs on the failure path too.
async fn execute_all(
    session: &Arc<dyn Session>,
    begin: bool,
    statements: &[String],
) -> (bool, Result<(QueryResult, u128, usize), String>) {
    let started = Instant::now();
    if begin {
        if let Err(error) = session.begin().await {
            return (false, Err(error.to_string()));
        }
    }
    (begin, run_each(session, statements, started).await)
}

async fn run_each(
    session: &Arc<dyn Session>,
    statements: &[String],
    started: Instant,
) -> Result<(QueryResult, u128, usize), String> {
    let mut last = QueryResult::default();
    let mut ran = 0;
    for statement in statements {
        match session.execute(statement).await {
            Ok(result) => {
                ran += 1;
                if !result.columns.is_empty() {
                    last = result;
                }
            }
            Err(error) => return Err(error.to_string()),
        }
    }
    Ok((last, started.elapsed().as_millis(), ran))
}

/// Collapse "the tokio task died" and "the query failed" into one message.
/// Either way the user needs a sentence, not a nested Result.
fn flatten<T>(outcome: Result<anyhow::Result<T>, gpui_tokio::JoinError>) -> Result<T, String> {
    match outcome {
        Ok(Ok(value)) => Ok(value),
        Ok(Err(error)) => Err(error.to_string()),
        Err(join) => Err(format!("the query was interrupted: {join}")),
    }
}

impl Focusable for Shell {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}

impl EventEmitter<ShellEvent> for Shell {}

/// The radius macOS masks the window's bottom corners to. The
/// environment frame curves to the same radius, or the mask would cut
/// its corners off. Tahoe (Darwin 25, macOS 26) rounds windows to
/// 16pt; the versions before it used 10pt. There is no public API for
/// the value, so it is read off the OS version once.
fn window_corner_radius() -> f32 {
    #[cfg(target_os = "macos")]
    {
        static RADIUS: std::sync::LazyLock<f32> = std::sync::LazyLock::new(|| {
            let release = std::process::Command::new("uname")
                .arg("-r")
                .output()
                .ok()
                .and_then(|out| String::from_utf8(out.stdout).ok())
                .unwrap_or_default();
            let major: u32 = release
                .split('.')
                .next()
                .and_then(|s| s.trim().parse().ok())
                .unwrap_or(0);
            if major >= 25 { 16. } else { 10. }
        });
        *RADIUS
    }
    #[cfg(not(target_os = "macos"))]
    {
        0.
    }
}

/// The environment ring's width. The frame's `border_3` must agree.
const RING_WIDTH: f32 = 3.;

/// The environment frame, painted a second time over the content. The
/// panels at the bottom corners are square and paint after the shell's
/// borders, so without this pass they would cover the curved ring and
/// hairline. No background and no listeners: it colors pixels only.
///
/// Both layers are positioned absolutely in the same frame, never laid
/// out inside a border: border layout rounds to layout pixels, which
/// slid the hairline's curve off the ring's and let the panel show
/// through as a light arc at each corner. The hairline is also drawn a
/// point wide of itself, tucked under the ring, so the two curves
/// overlap instead of meeting edge to edge — an abutting seam leaks
/// background on fractional display scales, an overlapped one cannot.
/// The ring paints last and keeps the visible hairline to one point.
fn frame_overlay(env: Env, radius: Pixels, colors: &ThemeColors) -> Div {
    let tuck = px(RING_WIDTH - 1.);
    div()
        .absolute()
        .inset_0()
        .child(
            div()
                .absolute()
                .inset(tuck)
                .border_2()
                .border_color(env.inner(colors))
                .rounded_b((radius - tuck).max(px(0.))),
        )
        .child(
            div()
                .absolute()
                .inset_0()
                .border_3()
                .border_color(env.ring(colors))
                .rounded_b(radius),
        )
}

/// What the connecting line says before there is a connection to describe.
fn describe(target: &Target) -> String {
    match target {
        Target::Url(url) => redact(url),
        Target::Profile(profile) => profile.name.clone(),
    }
}

/// The name the session goes by. A profile's own name, empty names aside; a
/// command-line URL has none, and the database name stands in for it.
fn connection_name(target: &Target) -> Option<SharedString> {
    match target {
        Target::Profile(profile) if !profile.name.trim().is_empty() => {
            Some(profile.name.clone().into())
        }
        _ => None,
    }
}

impl Render for Shell {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();

        // The comp's environment frame: a tagged session wears its ring
        // around the whole window, with a hairline of the same family
        // just inside it. An untagged session wears nothing.
        //
        // macOS masks the window to rounded bottom corners, so a square
        // ring loses its corners to that mask. The ring and the hairline
        // curve to the same radius instead — except in fullscreen, where
        // the mask is square. The panels at the bottom corners stay
        // square and paint after the borders, so `frame_overlay` repaints
        // both curves on top of them.
        //
        // The ring's border lives on `framed`, never on `root`: an
        // absolutely positioned child is placed in its parent's padding
        // box, so a border on `root` would shove `frame_overlay` inward
        // by the ring's own width. `root` still rounds its background,
        // which keeps the corners outside the ring dark under the mask.
        let radius = if window.is_fullscreen() { px(0.) } else { px(window_corner_radius()) };
        let inner_radius = (radius - px(RING_WIDTH)).max(px(0.));
        let mut content = div()
            .size_full()
            .flex()
            .flex_col()
            .child(self.breadcrumb_bar(&colors, cx))
            .child(
                div()
                    .flex()
                    .flex_1()
                    .min_h(px(0.))
                    .child(self.sidebar(&colors, cx))
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .flex_1()
                            .min_w(px(0.))
                            .child(self.tab_strip(&colors, cx))
                            .child(self.pane(&colors, window, cx))
                            .child(self.status_strip(&colors, cx)),
                    ),
            );
        if let Some(env) = self.env {
            content = content
                .border_1()
                .border_color(env.inner(&colors))
                .rounded_b(inner_radius);
        }

        let mut framed = div().size_full().flex().flex_col().child(content);
        if let Some(env) = self.env {
            framed = framed.border_3().border_color(env.ring(&colors)).rounded_b(radius);
        }

        let mut root = div()
            .key_context("Shell")
            .track_focus(&self.focus_handle(cx))
            .on_action(cx.listener(Self::on_run_query))
            .on_action(cx.listener(Self::on_stop_query))
            .on_action(cx.listener(Self::on_commit_transaction))
            .on_action(cx.listener(Self::on_rollback_transaction))
            .on_action(cx.listener(Self::on_new_query))
            .on_action(cx.listener(Self::on_close_tab))
            .on_action(cx.listener(Self::on_refresh))
            .on_action(cx.listener(Self::on_prev_page))
            .on_action(cx.listener(Self::on_next_page))
            .on_action(cx.listener(Self::on_show_history))
            .on_action(cx.listener(Self::on_find_column))
            .on_action(cx.listener(Self::on_filter_catalog))
            .on_action(cx.listener(Self::on_peek_value))
            .on_action(cx.listener(Self::on_toggle_palette))
            .on_action(cx.listener(Self::on_next_tab))
            .on_action(cx.listener(Self::on_prev_tab))
            .on_action(cx.listener(Self::on_select_up))
            .on_action(cx.listener(Self::on_select_down))
            .on_action(cx.listener(Self::on_select_left))
            .on_action(cx.listener(Self::on_select_right))
            .on_action(cx.listener(Self::on_extend_up))
            .on_action(cx.listener(Self::on_extend_down))
            .on_action(cx.listener(Self::on_extend_left))
            .on_action(cx.listener(Self::on_extend_right))
            .on_action(cx.listener(Self::on_select_row_start))
            .on_action(cx.listener(Self::on_select_row_end))
            .on_action(cx.listener(Self::on_select_first_row))
            .on_action(cx.listener(Self::on_select_last_row))
            .on_action(cx.listener(Self::on_select_all))
            .on_action(cx.listener(Self::on_copy_selection))
            .on_action(cx.listener(Self::on_toggle_pick))
            .on_action(cx.listener(Self::on_clear_selection))
            .relative()
            .flex()
            .flex_col()
            .size_full()
            .bg(colors.window)
            .font_family(FONT_FAMILY)
            .text_color(colors.text_body);
        if self.env.is_some() {
            root = root.rounded_b(radius);
        }
        root.child(framed)
            .children(self.env.map(|env| frame_overlay(env, radius, &colors)))
            // Under the palette and the dialog, both of which clear it on
            // the way up, so the order only settles the one frame where two
            // could be painted.
            .children(self.peek_overlay(&colors, cx))
            .children(self.palette_overlay(&colors, cx))
            // Last, so it paints over the palette on the one frame where
            // both could be up.
            .children(self.confirm_overlay(&colors, cx))
    }
}

impl Shell {
    fn breadcrumb_bar(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let separator = || div().text_color(colors.text_faint).child("/");
        let mut trail = div()
            .flex_1()
            .flex()
            .justify_center()
            .items_center()
            .gap(px(7.))
            .text_color(colors.text_muted)
            .child(
                div()
                    .text_color(colors.text_secondary)
                    .child(self.session_name()),
            );
        match self.tabs.get(self.active) {
            Some(Tab::Table(tab)) => {
                trail = trail
                    .child(separator())
                    .child(tab.schema.clone())
                    .child(separator())
                    .child(
                        div()
                            .text_color(colors.text)
                            .font_weight(FontWeight::MEDIUM)
                            .child(tab.table.clone()),
                    );
            }
            Some(Tab::History(_)) => {
                trail = trail.child(separator()).child(
                    div()
                        .text_color(colors.text)
                        .font_weight(FontWeight::MEDIUM)
                        .child("history"),
                );
            }
            _ => {}
        }

        // A tagged session tints the whole bar with the environment's
        // wash and rules it with the same family, the way the comp does.
        let (bar_bg, bar_rule) = match self.env {
            Some(env) => (env.surface(colors), env.inner(colors)),
            None => (colors.panel, colors.border),
        };
        div()
            .h(px(38.))
            .flex_none()
            .flex()
            .items_center()
            .px(px(12.))
            .gap(px(14.))
            .border_b_1()
            .border_color(bar_rule)
            .bg(bar_bg)
            .text_size(px(11.))
            // The way back to the connections screen, where the comp puts
            // the window controls.
            .child(
                div()
                    .id("connections")
                    .flex_none()
                    .text_color(colors.text_muted)
                    .cursor_pointer()
                    .hover(|s| s.text_color(colors.accent))
                    .on_click(cx.listener(|this, _event, window, cx| {
                        // Leaving drops every tab at once, so it asks about
                        // all of them — `proceed_close` writes the strip
                        // back before the shell goes.
                        if this.guard_close(Close::Shell, window, cx) {
                            this.proceed_close(Close::Shell, window, cx);
                        }
                    }))
                    .child("‹ connections"),
            )
            .children(self.env.map(|env| {
                // The badge names the frame: PROD in the ring's own
                // color, so the tint never has to be decoded from memory.
                div()
                    .flex_none()
                    .px(px(9.))
                    .py(px(4.))
                    .rounded(px(5.))
                    .bg(env.ring(colors))
                    .text_size(px(10.))
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_color(colors.window)
                    .child(env.as_str().to_ascii_uppercase())
            }))
            .child(self.mode_mark(colors))
            .child(trail)
    }

    /// The comp's mode mark: a padlock and one word, in the top bar beside
    /// the environment badge. It is the answer to "can this window change
    /// the database", and it is on screen at all times because that
    /// question must never need a click.
    ///
    /// Read-only wears the dev family's green and read-write the prod
    /// family's clay — the same warning the frame gives, in the same tones,
    /// so the two marks agree with each other. A session that never
    /// connected has nothing to be read-only against and goes grey.
    fn mode_mark(&self, colors: &ThemeColors) -> Div {
        let (label, surface, border, ink) = self.mode_tones(colors);
        div()
            .flex_none()
            .flex()
            .items_center()
            .gap(px(6.))
            .px(px(8.))
            .py(px(4.))
            .border_1()
            .border_color(border)
            .rounded(px(5.))
            .bg(surface)
            .child(lock_glyph(ink))
            .child(
                div()
                    .text_size(px(10.))
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_color(ink)
                    .child(label),
            )
    }

    /// The mark's label and its three tones. A session that never
    /// connected is grey whichever mode it asked for.
    fn mode_tones(&self, colors: &ThemeColors) -> (&'static str, Hsla, Hsla, Hsla) {
        match (&self.status, self.read_only) {
            (Status::Failed(_), _) => (
                "OFFLINE",
                colors.mode_off_surface,
                colors.mode_off_border,
                colors.mode_off_text,
            ),
            (_, true) => (
                "READ-ONLY",
                colors.env_dev_surface,
                colors.env_dev_inner,
                colors.env_dev_text,
            ),
            (_, false) => (
                "READ-WRITE",
                colors.env_prod_surface,
                colors.env_prod_inner,
                colors.env_prod_text,
            ),
        }
    }

    /// The mark's ink on its own, for the lines that say the mode in text.
    fn mode_ink(&self, colors: &ThemeColors) -> Hsla {
        self.mode_tones(colors).3
    }

    /// The same fact in a word, for the lines that carry it as text: the
    /// sidebar's foot and the query toolbar.
    fn mode_word(&self) -> &'static str {
        if self.read_only { "read-only" } else { "read-write" }
    }

    /// What to call this session on screen. The profile's name first, then
    /// the database the connection reports, then the app's own name.
    fn session_name(&self) -> SharedString {
        if let Some(name) = &self.name {
            return name.clone();
        }
        match &self.label {
            Some(label) if !label.database.is_empty() => label.database.clone().into(),
            _ => "meerkat".into(),
        }
    }

    fn sidebar(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        // The card's second line carries whatever the first one does not.
        // A named profile takes the title, so the database moves down here
        // rather than leaving the screen: the user still has to know which
        // database of that host is open.
        let (host, dot) = match (&self.label, &self.status) {
            (Some(label), Status::Connected) => {
                let where_it_is = format!("{} · {}", label.host, label.port);
                let line = match (&self.name, label.database.is_empty()) {
                    (Some(_), false) => format!("{} · {where_it_is}", label.database),
                    _ => where_it_is,
                };
                (line, colors.ok)
            }
            (_, Status::Failed(_)) => ("not connected".to_string(), colors.error),
            _ => ("connecting…".to_string(), colors.text_faint),
        };

        div()
            .w(px(SIDEBAR_WIDTH))
            .flex_none()
            .flex()
            .flex_col()
            .border_r_1()
            .border_color(colors.border)
            .bg(colors.panel)
            .child(
                div().p(px(12.)).border_b_1().border_color(colors.hairline).child(
                    card(cx)
                        .flex()
                        .items_center()
                        .gap(px(9.))
                        .px(px(8.))
                        .py(px(7.))
                        .child(
                            div()
                                .flex_1()
                                .min_w(px(0.))
                                .flex()
                                .flex_col()
                                .gap(px(2.))
                                .child(
                                    div()
                                        .text_size(px(12.))
                                        .font_weight(FontWeight::MEDIUM)
                                        .text_color(colors.text)
                                        .truncate()
                                        .child(self.session_name()),
                                )
                                .child(
                                    div()
                                        .text_size(px(10.))
                                        .text_color(colors.text_muted)
                                        .truncate()
                                        .child(host),
                                ),
                        )
                        .child(status_dot(dot)),
                ),
            )
            .child(self.catalog_filter_row(colors, cx))
            .child(self.catalog_list(colors, cx))
            .child(
                div()
                    .flex_none()
                    .px(px(12.))
                    .py(px(9.))
                    .border_t_1()
                    .border_color(colors.hairline)
                    .flex()
                    .flex_col()
                    .gap(px(6.))
                    .text_size(px(10.))
                    .text_color(colors.text_muted)
                    // The comp's way into the history, kept where it puts
                    // it: the foot of the sidebar.
                    .child(
                        div()
                            .flex()
                            .justify_between()
                            .child(
                                div()
                                    .id("query-history")
                                    .cursor_pointer()
                                    .hover(|s| s.text_color(colors.accent))
                                    .on_click(cx.listener(|this, _event, _window, cx| {
                                        this.open_history(cx)
                                    }))
                                    .child("query history"),
                            )
                            // The foot says the mode in words, where the
                            // comp puts it, and in the mark's own ink: the
                            // sidebar is where a session is read, and it
                            // must not have to be read against the top bar.
                            .child(div().text_color(self.mode_ink(colors)).child(self.mode_word())),
                    )
                    .child(div().text_color(colors.text_faint).child(self.table_total())),
            )
    }

    fn table_total(&self) -> SharedString {
        match self.relation_total {
            0 => "no tables".into(),
            1 => "1 relation".into(),
            n => format!("{n} relations").into(),
        }
    }

    /// The filter line over the catalog list. It searches the names the
    /// session already holds, so a keystroke costs a substring scan and
    /// never a query.
    fn catalog_filter_row(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let filtering = !self.catalog_filter.read(cx).is_empty();
        div()
            // ↑↓ walk the list below while this line has the focus. The
            // context sits here rather than on the shell, or the two keys
            // would be taken from the grid whenever nothing is typed.
            .key_context(CATALOG_FILTER_KEY_CONTEXT)
            .on_action(cx.listener(Self::on_catalog_prev))
            .on_action(cx.listener(Self::on_catalog_next))
            .flex_none()
            .px(px(12.))
            .py(px(8.))
            .border_b_1()
            .border_color(colors.hairline)
            .child(
                card(cx)
                    .flex()
                    .items_center()
                    .gap(px(7.))
                    .px(px(8.))
                    .py(px(5.))
                    .child(
                        div()
                            .flex_none()
                            .text_size(px(10.))
                            .text_color(colors.text_faint)
                            .child("⌕"),
                    )
                    .child(div().flex_1().min_w(px(0.)).child(self.catalog_filter.clone()))
                    // ⌘E reaches this line from anywhere, and a gesture
                    // nothing on screen names is a gesture nobody finds.
                    // It gives way to the clear mark, which is about the
                    // line the user is already on.
                    .children((!filtering).then(|| {
                        div()
                            .flex_none()
                            .text_size(px(10.))
                            .text_color(colors.text_faint)
                            .child("⌘E")
                    }))
                    // The way out of a filter for the mouse. It appears only
                    // when there is something to clear.
                    .children(filtering.then(|| {
                        div()
                            .id("clear-filter")
                            .flex_none()
                            .text_size(px(10.))
                            .text_color(colors.text_faint)
                            .cursor_pointer()
                            .hover(|s| s.text_color(colors.accent))
                            .on_click(cx.listener(|this, _event, _window, cx| {
                                this.catalog_filter.update(cx, |field, cx| field.clear(cx));
                                this.rebuild_catalog_rows(cx);
                            }))
                            .child("✕")
                    })),
            )
    }

    /// The sidebar list. A catalog can run to thousands of relations, so
    /// the rows are virtualized: the flattened list is built once, when
    /// the catalog lands, and only the rows on screen are laid out.
    fn catalog_list(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> AnyElement {
        if self.catalog_rows.is_empty() {
            return div()
                .id("catalog")
                .flex_1()
                .min_h(px(0.))
                .px(px(14.))
                .pt(px(10.))
                .text_size(px(11.))
                .text_color(colors.text_faint)
                // The catalog is a request of its own, so a connected
                // session can still be waiting for one. A filter that
                // matched nothing empties the list as well, and says so
                // rather than reporting an empty catalog.
                .child(match (&self.status, self.catalog_loading) {
                    _ if !self.catalog_groups.is_empty() => "nothing goes by that name",
                    (Status::Failed(_), _) => "no catalog",
                    (_, true) => "reading the catalog…",
                    (Status::Connecting(_), _) => "connecting…",
                    (Status::Connected, _) if self.catalog_error.is_some() => "no catalog",
                    (Status::Connected, _) => "no relations",
                })
                .into_any_element();
        }

        // The row the active tab came from, whichever kind of tab that is:
        // a query tab opened from the sidebar marks its row too.
        let active = match self.tabs.get(self.active) {
            Some(Tab::Table(tab)) => {
                Some((SharedString::from(tab.schema.clone()), SharedString::from(tab.table.clone())))
            }
            Some(Tab::Query(tab)) => tab
                .relation
                .as_ref()
                .map(|(schema, table)| (schema.clone().into(), table.clone().into())),
            _ => None,
        };
        let rows = self.catalog_rows.clone();
        let cursor = self.catalog_selected;
        let shell = cx.entity().downgrade();

        let mut list = uniform_list(
            "catalog",
            rows.len(),
            move |range, _window, cx| {
                let colors = theme(cx).colors.clone();
                range
                    .map(|ix| {
                        catalog_row(
                            ix,
                            &rows[ix],
                            active.as_ref(),
                            cursor == Some(ix),
                            &shell,
                            &colors,
                            cx,
                        )
                    })
                    .collect::<Vec<_>>()
            },
        );
        // `UniformList` carries an `Interactivity` but not
        // `StatefulInteractiveElement`, so the flag that
        // `restrict_scroll_to_axis()` would set is set by hand, as the
        // results grid does.
        list.style().restrict_scroll_to_axis = Some(true);
        let list = list.track_scroll(&self.catalog_scroll).size_full().px(px(8.));

        // The bar sits outside the scrolling list, or it would scroll away
        // with it. `uniform_list` keeps a plain handle inside its own, and
        // that is the one the scrollbar reads.
        let handle = self.catalog_scroll.0.borrow().base_handle.clone();
        div()
            .relative()
            .flex_1()
            .min_h(px(0.))
            .child(list)
            .children(
                Scrollbar::new(
                    true,
                    handle,
                    self.catalog_drag.clone(),
                    colors.text_faint,
                    colors.text_muted,
                )
                .map(|bar| {
                    div()
                        .absolute()
                        .top(px(0.))
                        .right(px(0.))
                        .bottom(px(0.))
                        .w(px(scrollbar::THICKNESS))
                        .child(bar)
                }),
            )
            .into_any_element()
    }

    fn tab_strip(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        // The tabs scroll inside the strip rather than pushing "+ new
        // query" off the end, and each one keeps its height and a width
        // between the two bounds: a long table name is truncated, and a
        // short one is not squeezed to its text.
        let mut tabs = div()
            .id("tabs")
            .flex()
            .flex_1()
            .min_w(px(0.))
            .items_stretch()
            .overflow_x_scroll();

        for (ix, tab) in self.tabs.iter().enumerate() {
            let is_active = ix == self.active;
            let id = tab.id();
            let mut item = div()
                .id(ElementId::Name(format!("tab-{id}").into()))
                .h_full()
                .flex_none()
                .min_w(px(TAB_MIN_WIDTH))
                .max_w(px(TAB_MAX_WIDTH))
                .flex()
                .items_center()
                .gap(px(8.))
                .px(px(14.))
                .border_r_1()
                .border_color(colors.border)
                .cursor_pointer()
                .on_click(cx.listener(move |this, _event, window, cx| {
                    this.activate(ix, cx);
                    // The tab that was clicked takes the focus, each kind the
                    // way it takes it when it opens: a query tab into its
                    // editor, a table tab into its grid. Leaving the focus
                    // where it was would leave it on a control the click has
                    // just taken off the screen.
                    this.focus_active_tab(window, cx);
                    cx.notify();
                }))
                .child(match tab {
                    Tab::Table(tab) if tab.kind == TableKind::Table => {
                        table_glyph(is_active, cx).flex_none()
                    }
                    // Views and query results are both "not a table": the
                    // sidebar marks them with a ring, so tabs match.
                    _ => div().size(px(5.)).flex_none().rounded_full().border_1().border_color(
                        if is_active { colors.accent } else { colors.text_faint },
                    ),
                })
                .child(
                    // The title is what gives way when the tab is at its
                    // widest, so it takes the room the glyph and the ×
                    // leave and truncates inside it.
                    div()
                        .flex_1()
                        .min_w(px(0.))
                        .text_size(px(11.))
                        .font_weight(if is_active { FontWeight::MEDIUM } else { FontWeight::NORMAL })
                        .text_color(if is_active { colors.text } else { colors.text_muted })
                        .truncate()
                        .child(tab.title()),
                )
                .child(
                    div()
                        .id(ElementId::Name(format!("close-{id}").into()))
                        .flex_none()
                        .text_size(px(12.))
                        .text_color(colors.text_faint)
                        .cursor_pointer()
                        .hover(|s| s.text_color(colors.error))
                        .on_click(cx.listener(move |this, _event, window, cx| {
                            if this.guard_close(Close::Tab(id), window, cx) {
                                this.close_tab(id, window, cx);
                            }
                        }))
                        .child("×"),
                );
            item = if is_active {
                item.bg(colors.window)
            } else {
                let hover = colors.hairline;
                item.hover(move |s| s.bg(hover))
            };
            tabs = tabs.child(item);
        }

        div()
            .h(px(TAB_STRIP_HEIGHT))
            .flex_none()
            .flex()
            .items_stretch()
            .border_b_1()
            .border_color(colors.border)
            .bg(colors.panel)
            .child(tabs)
            .child(
                div()
                    .id("new-query")
                    .flex_none()
                    .flex()
                    .items_center()
                    .px(px(12.))
                    .text_size(px(11.))
                    .text_color(colors.accent)
                    .cursor_pointer()
                    .hover(|s| s.text_color(colors.accent_deep))
                    .on_click(cx.listener(|this, _event, window, cx| this.new_query(window, cx)))
                    .child("+ new query"),
            )
    }

    fn pane(&self, colors: &ThemeColors, window: &mut Window, cx: &mut Context<Self>) -> Div {
        let pane = div().flex().flex_col().flex_1().min_h(px(0.)).min_w(px(0.));
        match self.tabs.get(self.active) {
            Some(Tab::Table(tab)) => pane
                .child(self.table_toolbar(tab, colors, cx))
                .children(tab.error.clone().map(|error| error_strip(error, colors)))
                .child(self.result_body(tab.id, cx)),
            Some(Tab::Query(tab)) => self.query_pane(pane, tab, colors, cx),
            Some(Tab::History(tab)) => self.history_pane(pane, tab, colors, cx),
            None => pane.child(self.placeholder(colors, window)),
        }
    }

    /// The grid, in a focus and a key context of its own. Both kinds of tab
    /// that show a result render through here, so the keys reach the grid the
    /// same way in each.
    fn result_body(&self, tab_id: u64, cx: &Context<Self>) -> Div {
        let Some(tab) = self.tabs.iter().find(|tab| tab.id() == tab_id) else { return div() };
        let (data, selection, scroll, first_row) = match tab {
            Tab::Table(tab) => (
                &tab.data,
                &tab.selection,
                &tab.scroll,
                // A page is a window on the table, so the gutter counts
                // from where the page starts rather than from 1 again.
                tab.page * PAGE_SIZE + 1,
            ),
            Tab::Query(tab) => (&tab.data, &tab.selection, &tab.scroll, 1),
            Tab::History(_) => return div(),
        };

        div()
            .key_context(GRID_KEY_CONTEXT)
            .track_focus(&self.grid_focus)
            .flex()
            .flex_1()
            .min_h(px(0.))
            .min_w(px(0.))
            .child(
                Grid::new(format!("tab-{tab_id}"), data.clone(), scroll, selection)
                    .first_row(first_row)
                    .on_hit(self.hit_handler(tab_id, cx))
                    .render(cx),
            )
    }

    /// Everything a click in the grid can mean, in one place.
    ///
    /// The grid reports where the mouse landed and what the modifiers said;
    /// what that does to the selection is decided here, and carried out by
    /// [`Selection`]'s own methods — so the mouse and the keys move the same
    /// selection through the same code.
    fn hit_handler(&self, tab_id: u64, cx: &Context<Self>) -> results_grid::OnHit {
        let this = cx.entity().downgrade();
        Rc::new(move |hit, window, cx| {
            this.update(cx, |this: &mut Shell, cx| {
                // A click in the result is a click outside the column-find
                // popover, and a popover a click has landed behind is one
                // the user is done with. A drag is not a fresh click: the
                // press that started it closed the popover already, and
                // took the focus with it.
                let dragging = matches!(hit, Hit::Drag { .. });
                if !dragging {
                    this.column_find = None;
                }
                let Some(tab) = this.tab_mut(tab_id) else { return };
                let Some(marked) = tab.marked() else { return };
                let extent = Extent::of(marked.data);
                let mut asked_to_peek = None;
                match hit {
                    Hit::Cell { cell, extend, peek } => {
                        if extend {
                            marked.selection.extend_to(cell);
                        } else {
                            marked.selection.focus(cell);
                        }
                        // The second click of a double click asks for the
                        // whole value. The first one has already moved the
                        // cursor there, so the card opens on the cell the
                        // user is looking at either way.
                        if peek {
                            asked_to_peek = Some(cell);
                        }
                    }
                    // The pointer dragging over the cells is the range
                    // growing, which is what ⇧ with an arrow key does — so
                    // it is the same call.
                    Hit::Drag { cell } => marked.selection.extend_to(cell),
                    Hit::Pick { row, through } => {
                        if through {
                            marked.selection.pick_through(row);
                        } else {
                            marked.selection.toggle_pick(row);
                        }
                    }
                    Hit::PickAll => marked.selection.toggle_all_picks(extent),
                    Hit::Column { column } => marked.selection.select_column(column, extent),
                }
                // A query tab types into its editor, which holds the focus
                // while the user is typing. A click in the result says the
                // grid is what the keys are for now, so the grid takes the
                // focus — and with it its own key context, which is what
                // makes a bare `space` mean "tick this row" here and a
                // space in the SQL there.
                if !dragging {
                    window.focus(&this.grid_focus, cx);
                }
                if let Some(cell) = asked_to_peek {
                    // After the focus, not before: the card takes the focus
                    // for itself, and ⎋ hands it back to the grid.
                    this.open_peek(cell, window, cx);
                }
                cx.notify();
            })
            .ok();
        })
    }

    fn table_toolbar(&self, tab: &TableTab, colors: &ThemeColors, cx: &Context<Self>) -> Div {
        let columns = self
            .table_model(&tab.schema, &tab.table)
            .map(|model| model.columns.len())
            .unwrap_or(tab.data.columns.len());
        let rows = match tab.approx_rows {
            Some(count) => format!("~{} rows", format_count(count)),
            None => "row count unknown".to_string(),
        };

        div()
            .flex_none()
            .flex()
            .items_center()
            .gap(px(10.))
            .px(px(14.))
            .py(px(9.))
            .border_b_1()
            .border_color(colors.hairline)
            .child(
                div()
                    .text_size(px(12.))
                    .font_weight(FontWeight::MEDIUM)
                    .text_color(colors.text)
                    .child(tab.table.clone()),
            )
            .child(
                div()
                    .text_size(px(11.))
                    .text_color(colors.text_muted)
                    .child(format!("{rows} · {columns} columns")),
            )
            .children(tab.loading.then(|| {
                div().text_size(px(11.)).text_color(colors.accent).child("loading…")
            }))
            .child(div().flex_1())
            .children(self.column_find_control(colors, cx))
            .child(
                div()
                    .id("refresh")
                    .px(px(9.))
                    .py(px(5.))
                    .border_1()
                    .border_color(colors.border_strong)
                    .rounded(px(6.))
                    .bg(colors.elevated)
                    .text_size(px(11.))
                    .text_color(colors.text_secondary)
                    .cursor_pointer()
                    .hover(|s| s.border_color(colors.text_faint))
                    .on_click(cx.listener(|this, _event, _window, cx| this.refresh_active(cx)))
                    .child("refresh"),
            )
    }

    /// The comp's "column ⌘J" button, and the popover it opens under itself.
    /// Both kinds of tab that show a result carry it: a result is as wide
    /// wherever it came from.
    ///
    /// Nothing is drawn while the result has no columns. A button that opened
    /// an empty list would be a control that says it can do something it
    /// cannot.
    ///
    /// The popover is `deferred`, which paints it after everything around it
    /// rather than under: GPUI paints siblings in order, and the toolbar is
    /// painted before the grid it hangs over.
    fn column_find_control(&self, colors: &ThemeColors, cx: &Context<Self>) -> Option<Div> {
        let tab = self.tabs.get(self.active).map(Tab::id)?;
        if self.result_columns().is_empty() {
            return None;
        }
        // Only ever over the result it was opened on, and never under one of
        // the two dialogs — both of them are already the thing taking keys.
        let open = self
            .column_find
            .as_ref()
            .filter(|find| find.tab == tab && self.palette.is_none() && self.confirm.is_none());
        // Open, the button wears the run timer's warm pill: the same "this is
        // live" reading, in the accent's family rather than a warning's.
        let (border, fill) = match open {
            Some(_) => (colors.running_border, colors.running_surface),
            None => (colors.border_strong, colors.elevated),
        };

        Some(
            div()
                .relative()
                .flex_none()
                .child(
                    div()
                        .id("find-column")
                        .flex()
                        .items_center()
                        .gap(px(7.))
                        .pl(px(9.))
                        .pr(px(5.))
                        .py(px(4.))
                        .border_1()
                        .border_color(border)
                        .rounded(px(6.))
                        .bg(fill)
                        .text_size(px(11.))
                        .text_color(colors.text_secondary)
                        .cursor_pointer()
                        .hover(|s| s.border_color(colors.text_faint))
                        .on_click(cx.listener(|this, _event, window, cx| {
                            this.toggle_column_find(window, cx)
                        }))
                        .child(search_glyph(colors.accent))
                        .child("column")
                        .child(key_badge("⌘J", colors)),
                )
                .children(open.map(|find| {
                    deferred(self.column_find_popover(find, colors, cx)).with_priority(1)
                })),
        )
    }

    /// The popover: the search line, what it found, and what the keys do.
    fn column_find_popover(
        &self,
        find: &ColumnFind,
        colors: &ThemeColors,
        cx: &Context<Self>,
    ) -> Stateful<Div> {
        let matches = self.column_matches(cx);
        let columns = self.result_columns();
        let count = format!("{}/{}", matches.len(), columns.len());

        div()
            .id("column-find")
            .key_context(COLUMN_FIND_KEY_CONTEXT)
            .on_action(cx.listener(Self::on_column_prev))
            .on_action(cx.listener(Self::on_column_next))
            // A click in the popover is not a click in the grid under it.
            .occlude()
            .absolute()
            .top(px(COLUMN_FIND_TOP))
            // Hung from the button's right edge, not its left as the comp
            // draws it: the button sits at the right end of the toolbar
            // here, and 290px to the right of it is off the window.
            .right(px(0.))
            .w(px(COLUMN_FIND_WIDTH))
            .flex()
            .flex_col()
            .overflow_hidden()
            .border_1()
            .border_color(colors.border_strong)
            .rounded(px(9.))
            .bg(colors.elevated)
            .shadow(vec![
                BoxShadow::new(px(0.), px(18.), colors.shadow)
                    .blur_radius(px(40.))
                    .spread_radius(px(-14.)),
            ])
            .child(
                div()
                    .flex_none()
                    .flex()
                    .items_center()
                    .gap(px(8.))
                    .px(px(11.))
                    .py(px(9.))
                    .border_b_1()
                    .border_color(colors.border)
                    .child(search_glyph(colors.text_faint))
                    .child(div().flex_1().min_w(px(0.)).child(find.query.clone()))
                    // How much of the result is left, so a search that has
                    // narrowed to one column says so without counting rows.
                    .child(div().text_size(px(10.)).text_color(colors.text_faint).child(count)),
            )
            .child(self.column_find_list(find, &matches, colors, cx))
            .child(
                div()
                    .flex_none()
                    .flex()
                    .items_center()
                    .gap(px(10.))
                    .px(px(11.))
                    .py(px(7.))
                    .border_t_1()
                    .border_color(colors.border)
                    .bg(colors.panel)
                    .text_size(px(9.))
                    .text_color(colors.text_muted)
                    .child("↑↓ browse")
                    .child("⏎ jump")
                    .child("esc close"),
            )
    }

    /// What the search found, one column per row. The list grows to the
    /// matches and stops at nine rows: a popover is a way to one column, not
    /// a second view of the header.
    fn column_find_list(
        &self,
        find: &ColumnFind,
        matches: &[usize],
        colors: &ThemeColors,
        cx: &Context<Self>,
    ) -> Div {
        if matches.is_empty() {
            return div()
                .flex_none()
                .px(px(13.))
                .py(px(10.))
                .text_size(px(11.))
                .text_color(colors.text_faint)
                .child("no column matches");
        }

        let columns = self.result_columns();
        let rows: Rc<Vec<(usize, SharedString)>> = Rc::new(
            matches.iter().map(|&ix| (ix, columns[ix].clone().into())).collect(),
        );
        // Which lane the cursor is in, so the list can say so: the user is
        // being shown where they are as well as where they could go.
        let cursor = self
            .tabs
            .get(self.active)
            .and_then(Tab::selection)
            .and_then(Selection::cursor)
            .map(|cursor| cursor.column);
        let selected = find.selected.min(rows.len() - 1);
        let jump: Rc<dyn Fn(usize, &mut Window, &mut App)> = {
            let shell = cx.entity().downgrade();
            Rc::new(move |column, window, cx| {
                shell
                    .update(cx, |shell: &mut Shell, cx| {
                        shell.jump_to_column(column, window, cx)
                    })
                    .ok();
            })
        };

        // The 5px of padding the list sits in counts towards the height the
        // comp gives the box, so it comes off the rows rather than being
        // added to them: a scrolling list must not be a half-row tall.
        let padding = 5.;
        let height =
            (rows.len() as f32 * COLUMN_ROW_HEIGHT).min(COLUMN_LIST_MAX_HEIGHT - 2. * padding);
        let mut list = uniform_list("column-matches", rows.len(), move |range, _window, cx| {
            let colors = theme(cx).colors.clone();
            range
                .map(|ix| column_row(ix, &rows[ix], ix == selected, cursor, &jump, &colors))
                .collect::<Vec<_>>()
        });
        list.style().restrict_scroll_to_axis = Some(true);

        div()
            .h(px(height + 2. * padding))
            .flex_none()
            .flex()
            .flex_col()
            .p(px(padding))
            .child(list.track_scroll(&find.scroll).flex_1().min_h(px(0.)))
    }

    /// The timer beside the run button. It is the only thing on screen that
    /// says a slow query is still alive, so it is there for every state
    /// where the server has the statement — cancelling included — but not
    /// for the first second of one: it appears at `1.0 s` and counts from
    /// there, so a query that answers at once never raises it at all.
    fn run_timer(&self, tab: &QueryTab, colors: &ThemeColors) -> Option<Div> {
        let live = tab.run.live()?;
        let waited = live.started.elapsed();
        if waited < TIMER_DELAY {
            return None;
        }
        Some(
            div()
                .flex_none()
                .flex()
                .items_center()
                .gap(px(7.))
                .px(px(9.))
                .py(px(5.))
                .border_1()
                .border_color(colors.running_border)
                .rounded(px(6.))
                .bg(colors.running_surface)
                .text_size(px(11.))
                .text_color(colors.accent_deep)
                .child(status_dot(colors.running_mark))
                .child(format_seconds(waited.as_millis())),
        )
    }

    /// The comp's `tx auto | manual` switch, in the query toolbar.
    ///
    /// Two chips and one of them is always lit: the mode is a choice
    /// between two states rather than a switch with an off position. The
    /// dot on `manual` is the quiet mark that a transaction is open — the
    /// bar under the editor is the loud one.
    ///
    /// **Auto does nothing while a transaction is open**, and paints faint
    /// to say so. Switching would leave the transaction standing with
    /// nothing offering to end it; the bar's two buttons are the way out.
    fn tx_switch(&self, tab: &QueryTab, colors: &ThemeColors, cx: &Context<Self>) -> Div {
        let manual = tab.tx_mode == TxMode::Manual;
        let locked = tab.in_transaction;
        let chip = |mode: TxMode, lit: bool, faint: bool, cx: &Context<Self>| {
            let mut chip = div()
                .id(SharedString::from(format!("tx-{}", mode.as_str())))
                .flex()
                .items_center()
                .gap(px(5.))
                .px(px(7.))
                .py(px(4.))
                .rounded(px(4.))
                .text_size(px(10.))
                .font_weight(FontWeight::MEDIUM)
                .text_color(if lit {
                    colors.accent_deep
                } else if faint {
                    colors.text_faint
                } else {
                    colors.text_muted
                })
                .child(mode.as_str());
            if lit {
                chip = chip.bg(colors.selection);
            }
            if !faint {
                chip = chip.cursor_pointer().on_click(
                    cx.listener(move |this, _event, _window, cx| this.set_tx_mode(mode, cx)),
                );
            }
            chip
        };
        div()
            .flex_none()
            .flex()
            .items_center()
            .gap(px(3.))
            .p(px(3.))
            .border_1()
            .border_color(if manual { colors.running_border } else { colors.border_strong })
            .rounded(px(6.))
            .bg(if manual { colors.running_surface } else { colors.panel })
            .child(
                div()
                    .px(px(4.))
                    .text_size(px(9.))
                    .text_color(colors.mode_off_text)
                    .child("tx"),
            )
            .child(chip(TxMode::Auto, !manual, locked, cx))
            .child(
                chip(TxMode::Manual, manual, false, cx).when(manual, |chip| {
                    chip.child(status_dot(if tab.in_transaction {
                        colors.running_mark
                    } else {
                        colors.running_border
                    }))
                }),
            )
    }

    /// The strip over the result: what the open transaction holds, and the
    /// two ways to end it.
    ///
    /// It is painted **whichever mode the tab is in**. A `BEGIN` the user
    /// typed into an auto-mode buffer opens a transaction exactly as manual
    /// mode does, and leaving that unmarked was the old behaviour's real
    /// gap: the transaction was on screen only as a badge, and the only way
    /// out of it was to close the tab, which rolled it back.
    ///
    /// It also stays up for a moment after the transaction ends, saying
    /// which way it went. "committed" is the answer to the question the
    /// user just asked, and a strip that vanished would leave it
    /// unanswered.
    fn tx_bar(&self, tab: &QueryTab, colors: &ThemeColors, cx: &Context<Self>) -> Option<Div> {
        let state = TxState {
            mode: tab.tx_mode,
            open: tab.in_transaction,
            done: tab.tx_done,
            statements: tab.tx_statements,
            running: tab.run.in_flight(),
            ending: tab.tx_ending,
        };
        let (title, sub) = tx_copy(state)?;
        // Open warms to the accent's family, a commit rests in the dev
        // green the read-only mark already uses, and a rollback goes back
        // to paper: nothing was written, so nothing is worth a colour.
        let (surface, border, ink, sub_ink, dot) = match (state.open, state.done) {
            (true, _) => (
                colors.running_surface,
                colors.running_border,
                colors.accent_deep,
                colors.accent_muted,
                colors.running_mark,
            ),
            (false, Some(TxEnd::Commit)) => (
                colors.env_dev_surface,
                colors.env_dev_inner,
                colors.env_dev_text,
                colors.text_muted,
                colors.env_dev,
            ),
            (false, _) => (
                colors.panel,
                colors.border_strong,
                colors.text_secondary,
                colors.text_muted,
                colors.text_faint,
            ),
        };
        // A boundary needs the session, and a run is holding it. The
        // buttons go faint rather than away: the transaction is still
        // there, and so is the answer to it once ⌘. has landed.
        let ready = state.open && !state.running && !state.ending;

        Some(
            div()
                .flex_none()
                .flex()
                .items_center()
                .gap(px(10.))
                .px(px(14.))
                .py(px(9.))
                .border_b_1()
                .border_color(border)
                .bg(surface)
                .child(status_dot(dot))
                .child(
                    div()
                        .text_size(px(11.))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(ink)
                        .child(title),
                )
                .child(div().text_size(px(10.)).text_color(sub_ink).child(sub))
                .child(div().flex_1())
                .when(state.open, |bar| {
                    bar.child(self.tx_button(TxEnd::Rollback, ready, colors, cx))
                        .child(self.tx_button(TxEnd::Commit, ready, colors, cx))
                }),
        )
    }

    /// One of the bar's two buttons. Commit is filled in the accent, as the
    /// deliberate answer; rollback is outlined over paper, because
    /// discarding work the user has not seen the point of yet must not be
    /// the button the eye lands on first.
    ///
    /// The key sits in a **keycap**, the same one the run button carries.
    /// These three are the app's only buttons with a key written on them,
    /// and a key that reads as a key in one of them and as trailing text in
    /// the others would say the three were different kinds of thing. The
    /// tones follow the fill, as they do there: paper over the outlined
    /// button, and the white-on-fill pair over the filled one.
    fn tx_button(
        &self,
        how: TxEnd,
        ready: bool,
        colors: &ThemeColors,
        cx: &Context<Self>,
    ) -> Stateful<Div> {
        let (label, keys) = match how {
            TxEnd::Commit => ("commit", "⌘S"),
            TxEnd::Rollback => ("rollback", "⇧⌘R"),
        };
        let (border, fill, ink, cap_surface, cap_border) = match how {
            TxEnd::Commit => (
                colors.accent,
                colors.accent,
                colors.window,
                colors.key_on_fill_surface,
                colors.key_on_fill_border,
            ),
            TxEnd::Rollback => (
                colors.border_strong,
                colors.elevated,
                colors.text_secondary,
                colors.window,
                colors.mode_off_border,
            ),
        };
        let button = div()
            .id(SharedString::from(format!("tx-{label}")))
            .flex_none()
            .flex()
            .items_center()
            .gap(px(6.))
            // Less room on the right than on the left: the keycap carries a
            // box of its own, and even padding either side of it reads as
            // too much. The run button is spaced the same way.
            //
            // **Everything here is a size under the run button's**, and the
            // difference is the point: that one is the verb of the whole
            // pane and these two are controls on a strip inside it. A
            // rollback drawn as large as a run would read as the bigger
            // decision, which it is not.
            .pl(px(8.))
            .pr(px(4.))
            .py(px(3.))
            .border_1()
            .border_color(border)
            .rounded(px(5.))
            .bg(fill)
            .child(
                div()
                    .text_size(px(10.))
                    .when(how == TxEnd::Commit, |label| {
                        label.font_weight(FontWeight::MEDIUM)
                    })
                    .text_color(ink)
                    .child(label),
            )
            .child(keycap(keys, 9., cap_surface, cap_border, ink));
        if !ready {
            return button.opacity(0.5);
        }
        match how {
            TxEnd::Commit => button.hover(|s| s.bg(colors.accent_deep)),
            TxEnd::Rollback => button.hover(|s| s.border_color(colors.text_faint)),
        }
        .cursor_pointer()
        .on_click(cx.listener(move |this, _event, _window, cx| this.end_transaction(how, cx)))
    }

    /// One button for all three verbs, as the comp draws it: **run** filled
    /// in the accent, **stop** outlined in clay over paper, **terminate**
    /// filled in clay. The escalation is the point — the button that ends a
    /// backend must not look like the button that starts a query.
    ///
    /// **It also moves, and it moves in exactly one way: its own fill.**
    /// While a run is out the fill breathes — a little of the button's ink
    /// mixed in and back out again, `RUN_BREATH` for the cycle. When the
    /// rows land the same mix eases out to nothing, so a run reads as one
    /// gesture that starts and finishes rather than as an effect that plays.
    /// And a press mixes the other way, toward the app's ink.
    ///
    /// Nothing travels across the button, nothing blooms out of it, and
    /// nothing moves by a pixel. See `RUN_BREATH` for why the comp's
    /// sweeping front and looping band are not here.
    ///
    /// The breath and the settle are GPUI animations rather than ticks of
    /// the shell's timer. `with_animation` asks for its own frames, restarts
    /// when the element's id changes — which is the whole of how a landed
    /// result gets its exhale — and holds still when the platform says the
    /// user wants less motion.
    fn run_button(&self, tab: &QueryTab, colors: &ThemeColors, cx: &Context<Self>) -> AnyElement {
        let phase = tab.run.phase();
        let (verb, keys) = match phase {
            RunPhase::Ready => ("run", "⌘⏎"),
            RunPhase::Stop => ("stop", "⌘."),
            RunPhase::Terminate => ("terminate", "⌘."),
        };
        let pressed = self.run_pressed;
        // Paper under clay for "stop": the one state where the button is
        // outlined rather than filled, so a run in flight reads as a
        // question rather than as a command already given.
        let (border, resting, ink, cap_surface, cap_border) = match phase {
            RunPhase::Ready => (
                colors.accent,
                colors.accent,
                colors.window,
                colors.key_on_fill_surface,
                colors.key_on_fill_border,
            ),
            RunPhase::Stop => (
                colors.env_prod,
                colors.window,
                colors.env_prod_text,
                colors.env_prod_surface,
                colors.env_prod_inner,
            ),
            RunPhase::Terminate => (
                colors.env_prod,
                colors.env_prod,
                colors.window,
                colors.key_on_fill_surface,
                colors.key_on_fill_border,
            ),
        };
        // One formula for all three of the fill's moods, so that the
        // breath, the settle and the press cannot drift into three
        // unrelated effects: mix something into the resting fill.
        //
        // **Which way it mixes is the whole of what it says.** Toward the
        // button's own ink is lighter on a filled button and warmer on the
        // outlined one — that is "working", and it reads on every state
        // without a tone per state. Toward the app's ink is darker
        // everywhere, and that is "held down".
        let fill = if pressed {
            resting.blend(colors.text.opacity(RUN_PRESS_DEPTH))
        } else {
            resting
        };

        let button = div()
            .id("run-query")
            .flex_none()
            .flex()
            .items_center()
            .gap(px(8.))
            .pl(px(10.))
            .pr(px(5.))
            .py(px(4.))
            .border_1()
            .border_color(border)
            .rounded(px(6.))
            .bg(fill)
            .cursor_pointer()
            // **The press is the click here, and `on_click` is not used.**
            // GPUI keeps a click's half-finished state under the element's
            // id, and this element's id path carries whichever animation is
            // running — so a button held down across the moment a result
            // lands would come up under a different id and lose the click.
            // `run_pressed` is on the shell, where no animation can move it.
            .on_mouse_down(
                gpui::MouseButton::Left,
                cx.listener(|this, _event, _window, cx| {
                    this.run_pressed = true;
                    cx.notify();
                }),
            )
            .on_mouse_up(
                gpui::MouseButton::Left,
                cx.listener(|this, _event, _window, cx| {
                    if this.run_pressed {
                        this.release_run_button(cx);
                        this.toggle_run(cx);
                    }
                }),
            )
            // A press that comes up somewhere else is not a click, but it
            // is still the end of the press — and nothing else would say
            // so, which would leave the button held down for good.
            .on_mouse_up_out(
                gpui::MouseButton::Left,
                cx.listener(|this, _event, _window, cx| this.release_run_button(cx)),
            )
            .child(if phase == RunPhase::Ready {
                play_glyph(ink).into_any_element()
            } else {
                stop_glyph(ink).into_any_element()
            })
            .child(
                // Held at one width across the verbs, so that a run turning
                // out to be slow does not resize the button under the
                // pointer that is reaching for it.
                div()
                    // Fixed *and* `flex_none`: a width alone still leaves a
                    // flex item shrinkable, so a toolbar tight for room
                    // would take the pixels back out of the word.
                    .flex_none()
                    .w(px(if phase == RunPhase::Terminate {
                        TERMINATE_LABEL_WIDTH
                    } else {
                        RUN_LABEL_WIDTH
                    }))
                    .text_size(px(11.))
                    .font_weight(FontWeight::MEDIUM)
                    .text_color(ink)
                    .child(verb),
            )
            // The keycap does not move on a press: the tone says the press
            // landed, and a keycap sinking a pixel is the one thing on this
            // button that would still read as a mechanism.
            .child(keycap(keys, 11., cap_surface, cap_border, ink));

        // A press holds the button still at its pressed tone: what is under
        // the pointer must not also be breathing.
        if pressed {
            return button.into_any_element();
        }
        if tab.run.in_flight() {
            return button
                .with_animation(
                    "run-breath",
                    // Phase-locked to the app's own clock, so a breath does
                    // not restart from nothing every time something else on
                    // screen rebuilds this element.
                    Animation::new(RUN_BREATH)
                        .repeat_synced()
                        .with_easing(gpui::pulsating_between(0., 1.)),
                    move |button, delta| {
                        button.bg(resting.blend(ink.opacity(RUN_BREATH_DEPTH * delta)))
                    },
                )
                .into_any_element();
        }
        // `landed == 0` is a tab that has never answered, and the first
        // paint of every window is exactly that: nothing to exhale.
        if tab.landed == 0 {
            return button.into_any_element();
        }
        button
            .with_animation(
                ("run-settle", tab.landed),
                Animation::new(RUN_SETTLE).with_easing(gpui::ease_out_quint()),
                move |button, delta| {
                    button.bg(resting.blend(ink.opacity(RUN_SETTLE_DEPTH * (1. - delta))))
                },
            )
            .into_any_element()
    }

    /// The press ends, and nothing else happens: a release away from the
    /// button is not a click on it.
    fn release_run_button(&mut self, cx: &mut Context<Self>) {
        if self.run_pressed {
            self.run_pressed = false;
            cx.notify();
        }
    }

    /// The button does whichever of the three things its label says — and
    /// nothing at all while it says "run" over a run that is already out.
    /// The word under the pointer is what the click has to mean: a click
    /// during the arming window would either start a second run over the
    /// first, which is refused anyway, or stop a run the button never
    /// offered to stop. ⌘. is unaffected: a key press is aimed at the run,
    /// not at a word on a button.
    fn toggle_run(&mut self, cx: &mut Context<Self>) {
        match self.tabs.get(self.active) {
            Some(Tab::Query(tab)) if tab.run.arming() => {}
            Some(Tab::Query(tab)) if tab.run.in_flight() => self.stop_active_query(cx),
            _ => self.run_active_query(cx),
        }
    }

    fn query_pane(
        &self,
        pane: Div,
        tab: &QueryTab,
        colors: &ThemeColors,
        cx: &Context<Self>,
    ) -> Div {
        // The design's four result lines. Each one says what is true of the
        // run *now*, so the line and the button never disagree.
        let (label, summary, ink) = match &tab.run {
            // The driver streams the rows, but it hands the result over in
            // one piece at the end, so there is still no row count to report
            // while a run is out. The line says what it can: the run is
            // alive, and how to end it. A count would need the driver to
            // report progress, not merely to stream.
            // With no backend the tab's session is still opening, and the
            // line must not claim the server has anything: it does not.
            Run::Running(live) if live.backend.is_none() => (
                "RUNNING",
                "opening this tab's session · ⌘. calls it off".to_string(),
                colors.accent_deep,
            ),
            Run::Running(_) => (
                "RUNNING",
                "the server has the statement · ⌘. stops it".to_string(),
                colors.accent_deep,
            ),
            Run::Cancelling(live) => (
                "CANCELLING",
                match live.backend {
                    None => "the run was called off before it left".to_string(),
                    Some(RunId(pid)) => format!(
                        "cancel sent to backend pid {pid} · \
                         waiting for the server to acknowledge"
                    ),
                },
                colors.accent_deep,
            ),
            Run::Cancelled { elapsed } => (
                "CANCELLED",
                format!("stopped after {} · no rows kept", format_seconds(*elapsed)),
                colors.error,
            ),
            Run::Idle if tab.has_result => {
                let statements = match tab.statements_run {
                    0 | 1 => String::new(),
                    n => format!("{n} statements · last result · "),
                };
                (
                    "RESULT",
                    format!(
                        "{statements}{} rows · {} columns · {}",
                        tab.data.rows.len(),
                        tab.data.columns.len(),
                        // The whole wait, and only that. The split into
                        // server and lag lives on the status strip, which
                        // is already the line about what is on screen —
                        // saying it twice would crowd both.
                        tab.timing.map(|t| format_millis(t.total_ms)).unwrap_or_default()
                    ),
                    colors.text_muted,
                )
            }
            Run::Idle => ("RESULT", "not run yet".to_string(), colors.text_muted),
        };

        pane.child(
            // Query toolbar
            div()
                .flex_none()
                .flex()
                .items_center()
                .gap(px(10.))
                .px(px(14.))
                .py(px(9.))
                .border_b_1()
                .border_color(colors.hairline)
                .child(
                    // **Everything on the left gives up room before any
                    // control on the right does.** The run button must not
                    // move because a tab has a long name, and it must not
                    // be the thing that falls off the edge: it is
                    // `flex_none`, so a row that overflows pushes it past
                    // the pane and clips it.
                    //
                    // `min_w(0)` on each child is what makes that possible.
                    // A text element's automatic minimum is its own text,
                    // so without it these four cannot shrink at all,
                    // whatever the flex factors say — and the row overflows
                    // rather than truncating. It is the same pairing every
                    // cell in the grid needs.
                    //
                    // This group also *is* the spacer: it takes the slack
                    // when there is any, so the controls sit right however
                    // little there is to say on the left.
                    div()
                        .flex()
                        .flex_1()
                        .min_w(px(0.))
                        .items_center()
                        .gap(px(10.))
                        .child(
                            div()
                                .min_w(px(0.))
                                .text_size(px(12.))
                                .font_weight(FontWeight::MEDIUM)
                                .text_color(colors.text)
                                .truncate()
                                .child(tab.title.clone()),
                        )
                        .child(
                            div()
                                .min_w(px(0.))
                                .text_size(px(11.))
                                .text_color(colors.text_muted)
                                .truncate()
                                .child(format!(
                                    "{} · {}",
                                    self.session_name(),
                                    self.mode_word()
                                )),
                        )
                        .children(session_mark(tab, colors))
                        // With several statements in the buffer, say which
                        // one a run would send, so ⌘⏎ never comes as a
                        // surprise.
                        .children(run_scope(tab, cx).map(|scope| {
                            div()
                                .min_w(px(0.))
                                .text_size(px(11.))
                                .text_color(colors.text_faint)
                                .truncate()
                                .child(scope)
                        })),
                )
                .child(self.tx_switch(tab, colors, cx))
                .children(self.run_timer(tab, colors))
                .child(self.run_button(tab, colors, cx)),
        )
        .child(
            div()
                .h(px(EDITOR_HEIGHT))
                .flex_none()
                .border_b_1()
                .border_color(colors.border_strong)
                .child(tab.editor.clone()),
        )
        .children(tab.error.clone().map(|error| error_strip(error, colors)))
        // Directly over the result, because it is about what the runs have
        // done rather than about the statement above them.
        .children(self.tx_bar(tab, colors, cx))
        .child(
            // Result header
            div()
                .flex_none()
                .flex()
                .items_center()
                .gap(px(12.))
                .px(px(14.))
                .py(px(8.))
                .border_b_1()
                .border_color(colors.hairline)
                .bg(colors.panel)
                .text_size(px(10.))
                .text_color(ink)
                .child(div().font_weight(FontWeight::SEMIBOLD).text_color(ink).child(label))
                .child(summary)
                .children(cap_note(tab, colors))
                .child(div().flex_1())
                // The way to a column of the result, on the line that
                // describes the result — not on the toolbar above the
                // editor, which is about the statement.
                .children(self.column_find_control(colors, cx)),
        )
        .child(self.result_body(tab.id, cx))
    }

    /// The history screen: a heading, the comp's two chips, and the list
    /// of runs under their day.
    fn history_pane(
        &self,
        pane: Div,
        tab: &HistoryTab,
        colors: &ThemeColors,
        cx: &mut Context<Self>,
    ) -> Div {
        let tab_id = tab.id;

        pane.child(
            div()
                .flex_none()
                .flex()
                .items_center()
                .gap(px(10.))
                .px(px(14.))
                .py(px(9.))
                .border_b_1()
                .border_color(colors.hairline)
                .child(
                    div()
                        .text_size(px(12.))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.text)
                        .child("Query history"),
                )
                .child(
                    div()
                        .text_size(px(11.))
                        .text_color(colors.text_muted)
                        .child(format!("{} · last {HISTORY_DAYS} days", self.session_name())),
                )
                .child(div().flex_1())
                .child(
                    history::filter_chip("mine-only", "mine only", tab.user_only, colors).on_click(
                        cx.listener(move |this, _event, _window, cx| {
                            this.toggle_history_filter(tab_id, false, cx)
                        }),
                    ),
                )
                .child(
                    history::filter_chip("errors-only", "errors", tab.errors_only, colors)
                        .on_click(cx.listener(move |this, _event, _window, cx| {
                            this.toggle_history_filter(tab_id, true, cx)
                        })),
                ),
        )
        .children(tab.error.clone().map(|error| error_strip(error, colors)))
        .child(self.history_list(tab, colors, cx))
    }

    fn history_list(
        &self,
        tab: &HistoryTab,
        colors: &ThemeColors,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        if tab.rows.is_empty() {
            return div()
                .flex_1()
                .min_h(px(0.))
                .px(px(16.))
                .pt(px(14.))
                .text_size(px(11.))
                .text_color(colors.text_faint)
                .child(if tab.user_only || tab.errors_only {
                    "no runs match these filters"
                } else {
                    "nothing run yet on this connection"
                })
                .into_any_element();
        }

        let rows = tab.rows.clone();
        let open: OnOpenRun = {
            let shell = cx.entity().downgrade();
            Rc::new(move |statement, window, cx| {
                shell
                    .update(cx, |shell: &mut Shell, cx| {
                        shell.new_query_with(&statement, window, cx)
                    })
                    .ok();
            })
        };

        let mut list = uniform_list("history", rows.len(), move |range, _window, cx| {
            let colors = theme(cx).colors.clone();
            range
                .map(|ix| history::history_row(ix, &rows[ix], &open, &colors, cx))
                .collect::<Vec<_>>()
        });
        list.style().restrict_scroll_to_axis = Some(true);
        list.track_scroll(&tab.scroll)
            .flex_1()
            .min_h(px(0.))
            .px(px(16.))
            .pt(px(6.))
            .into_any_element()
    }

    fn placeholder(&self, colors: &ThemeColors, _window: &mut Window) -> Div {
        let (heading, detail) = match &self.status {
            Status::Connecting(target) => ("connecting".to_string(), target.clone()),
            Status::Failed(error) => ("could not connect".to_string(), error.clone()),
            Status::Connected => (
                "nothing open".to_string(),
                "pick a table on the left, or start a query".to_string(),
            ),
        };
        let failed = matches!(self.status, Status::Failed(_));

        div()
            .flex_1()
            .flex()
            .flex_col()
            .items_center()
            .justify_center()
            .gap(px(8.))
            .child(
                div()
                    .text_size(px(12.))
                    .font_weight(FontWeight::MEDIUM)
                    .text_color(if failed { colors.error } else { colors.text })
                    .child(heading),
            )
            .child(
                div()
                    .max_w(px(560.))
                    .text_size(px(11.))
                    .text_color(if failed { colors.error } else { colors.text_muted })
                    .child(detail),
            )
    }

    fn status_strip(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let divider = || div().text_color(colors.text_faint).child("|");
        let (range, timing) = match self.tabs.get(self.active) {
            Some(Tab::Table(tab)) => {
                let first = tab.page * PAGE_SIZE + 1;
                let last = tab.page * PAGE_SIZE + tab.data.rows.len();
                let range = if tab.data.rows.is_empty() {
                    "no rows".to_string()
                } else {
                    match tab.approx_rows {
                        Some(total) => {
                            format!("rows {first}–{last} of ~{}", format_count(total))
                        }
                        None => format!("rows {first}–{last}"),
                    }
                };
                (range, tab.timing)
            }
            Some(Tab::Query(tab)) => (
                if tab.has_result {
                    format!("{} rows", tab.data.rows.len())
                } else {
                    String::new()
                },
                tab.timing,
            ),
            Some(Tab::History(tab)) => (
                match tab.runs {
                    0 => "no runs".to_string(),
                    1 => "1 run".to_string(),
                    n => format!("{n} runs"),
                },
                None,
            ),
            None => (String::new(), None),
        };

        let paging = matches!(self.tabs.get(self.active), Some(Tab::Table(_)));
        // What is marked, and the key that takes it. The strip is where the
        // count of a selection belongs: it is already the line that says how
        // much is on screen.
        let marks = self
            .tabs
            .get(self.active)
            .and_then(|tab| tab.selection())
            .and_then(|selection| selection.summary());
        // Only over a result there is something to do to. A tab still
        // waiting on its first run has no columns and no keys worth listing.
        let keys = (!self.result_columns().is_empty())
            .then_some("↑↓←→ move · ⌘J column · ⏎ value · space picks · ⌘C copies");

        div()
            .h(px(30.))
            .flex_none()
            .flex()
            .items_center()
            .gap(px(12.))
            .px(px(14.))
            .border_t_1()
            .border_color(colors.border_strong)
            .bg(colors.panel)
            .text_size(px(10.))
            .text_color(colors.text_muted)
            .child(range)
            .children(paging.then(|| divider()))
            .children(paging.then(|| {
                self.page_link("prev", false, colors, cx)
            }))
            .children(paging.then(|| {
                self.page_link("next", true, colors, cx)
            }))
            .children(marks.as_ref().map(|_| divider()))
            .children(marks.map(|marks| div().text_color(colors.accent_deep).child(marks)))
            // The keys the result answers to, as the comp's own footer lists
            // them. It is the only place ⏎ and ⌘J are written down, and a
            // gesture nothing on screen names is a gesture nobody finds.
            .children(keys.map(|_| divider()))
            .children(keys.map(|keys| div().text_color(colors.text_faint).truncate().child(keys)))
            .child(div().flex_1())
            .children(timing.map(|timing| div().child(timing.summary())))
            .child(divider())
            .child(match self.status {
                Status::Connected => "on lookout",
                Status::Connecting(_) => "connecting",
                Status::Failed(_) => "off duty",
            })
            .child(status_dot(match self.status {
                Status::Connected => colors.ok,
                Status::Connecting(_) => colors.text_faint,
                Status::Failed(_) => colors.error,
            }))
    }

    /// The palette, over everything. It is an absolutely positioned child
    /// of the shell rather than a window of its own, so it cannot outlive
    /// the workspace it searches, and closing it hands the focus straight
    /// back.
    /// The close confirmation. The palette's pattern — an absolutely
    /// positioned child of the shell, never a window of its own — so
    /// answering it hands the focus straight back to the workspace.
    ///
    /// The scrim does **not** dismiss on a click, unlike the palette's. A
    /// stray click must not be an answer to a question about ending server
    /// work; ⎋ is the way out, and it says so.
    fn confirm_overlay(
        &self,
        colors: &ThemeColors,
        cx: &mut Context<Self>,
    ) -> Option<Stateful<Div>> {
        let confirm = self.confirm.as_ref()?;
        let (title, body, keep, act) =
            confirm_copy(confirm.what, &confirm.running, &confirm.open);
        // Every tab the close would touch, named once even when it is in
        // both lists.
        let mut named: Vec<String> = Vec::new();
        for name in confirm.running.iter().chain(confirm.open.iter()) {
            let name = name.to_string();
            if !named.contains(&name) {
                named.push(name);
            }
        }

        Some(
            div()
                .id("confirm-scrim")
                .absolute()
                .top(px(0.))
                .left(px(0.))
                .size_full()
                .flex()
                .justify_center()
                .items_start()
                .pt(px(CONFIRM_TOP_MARGIN))
                .bg(colors.overlay)
                .occlude()
                .child(
                    div()
                        .id("confirm")
                        .key_context(CONFIRM_KEY_CONTEXT)
                        .track_focus(&confirm.focus)
                        .on_action(cx.listener(Self::on_confirm_close))
                        .on_action(cx.listener(Self::on_cancel_close))
                        .occlude()
                        .w(px(CONFIRM_WIDTH))
                        .flex()
                        .flex_col()
                        .gap(px(10.))
                        .p(px(18.))
                        .border_1()
                        .border_color(colors.border_strong)
                        .rounded(px(10.))
                        .bg(colors.elevated)
                        .shadow(vec![
                            BoxShadow::new(px(0.), px(24.), colors.shadow)
                                .blur_radius(px(60.))
                                .spread_radius(px(-20.)),
                        ])
                        .child(
                            div()
                                .text_size(px(13.))
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_color(colors.text)
                                .child(title),
                        )
                        .children(body.into_iter().map(|line| {
                            div().text_size(px(11.)).text_color(colors.text_secondary).child(line)
                        }))
                        // With more than one tab at stake, name them: the
                        // user is about to end work they cannot see from
                        // here.
                        .children((named.len() > 1).then(|| {
                            div()
                                .text_size(px(11.))
                                .text_color(colors.text_muted)
                                .truncate()
                                .child(named.join(", "))
                        }))
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap(px(8.))
                                .pt(px(4.))
                                .child(
                                    div()
                                        .flex_1()
                                        .text_size(px(10.))
                                        .text_color(colors.text_faint)
                                        .child("⌘⏎ confirms · ⎋ or ⏎ keeps"),
                                )
                                .child(
                                    div()
                                        .id("confirm-keep")
                                        .px(px(12.))
                                        .py(px(5.))
                                        .border_1()
                                        .border_color(colors.border_strong)
                                        .rounded(px(6.))
                                        .text_size(px(11.))
                                        .text_color(colors.text)
                                        .cursor_pointer()
                                        .hover(|s| s.bg(colors.panel))
                                        .on_click(cx.listener(|this, _event, window, cx| {
                                            this.close_confirm(window, cx)
                                        }))
                                        .child(keep),
                                )
                                .child(
                                    // Clay and filled, the tone the
                                    // terminate button wears: the control
                                    // that ends server work must not look
                                    // like the one that keeps it.
                                    div()
                                        .id("confirm-act")
                                        .px(px(12.))
                                        .py(px(5.))
                                        .border_1()
                                        .border_color(colors.env_prod)
                                        .rounded(px(6.))
                                        .bg(colors.env_prod)
                                        .text_size(px(11.))
                                        .font_weight(FontWeight::MEDIUM)
                                        .text_color(colors.window)
                                        .cursor_pointer()
                                        .on_click(cx.listener(|this, _event, window, cx| {
                                            let Some(confirm) = this.confirm.take() else { return };
                                            cx.notify();
                                            this.proceed_close(confirm.what, window, cx);
                                        }))
                                        .child(act),
                                ),
                        ),
                ),
        )
    }

    /// The value peek: one cell's whole value, over the grid it came from.
    ///
    /// The scrim carries **no wash**. The palette's dims the workspace
    /// because the palette is a place the user has gone to; this card is a
    /// second look at something already on screen, and dimming the result
    /// behind it would hide what is being looked at. It still occludes, so a
    /// click cannot reach the grid underneath — and a click on it closes the
    /// card, which is what a click outside a peek means.
    fn peek_overlay(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Option<Stateful<Div>> {
        let peek = self.peek.as_ref()?;
        let (column, row, value) = self.peek_value()?;
        let characters = value.chars().count();
        // What is painted is bounded; what ⌘C copies is not.
        let shown: String = value.chars().take(PEEK_CHARS).collect();
        let cut = characters > PEEK_CHARS;
        let meta = match (characters, cut) {
            (1, _) => "1 character".to_string(),
            (n, false) => format!("{} characters", format_count(n as u64)),
            (n, true) => format!(
                "first {} of {} characters",
                format_count(PEEK_CHARS as u64),
                format_count(n as u64)
            ),
        };

        Some(
            div()
                .id("peek-scrim")
                .absolute()
                .top(px(0.))
                .left(px(0.))
                .size_full()
                .flex()
                .justify_center()
                .items_start()
                .pt(px(PEEK_TOP_MARGIN))
                .occlude()
                .on_click(cx.listener(|this, _event, window, cx| this.close_peek(window, cx)))
                .child(
                    div()
                        .id("peek")
                        .key_context(PEEK_KEY_CONTEXT)
                        .track_focus(&peek.focus)
                        .on_action(cx.listener(Self::on_close_peek))
                        .on_action(cx.listener(Self::on_copy_peek))
                        // A click in the card is not a click on the scrim,
                        // so it must not close it.
                        .occlude()
                        .w(px(PEEK_WIDTH))
                        .flex()
                        .flex_col()
                        .overflow_hidden()
                        .border_1()
                        .border_color(colors.border_strong)
                        .rounded(px(10.))
                        .bg(colors.elevated)
                        .shadow(vec![
                            BoxShadow::new(px(0.), px(24.), colors.shadow)
                                .blur_radius(px(60.))
                                .spread_radius(px(-20.)),
                        ])
                        .child(
                            div()
                                .flex_none()
                                .flex()
                                .items_center()
                                .gap(px(10.))
                                .px(px(15.))
                                .py(px(11.))
                                .border_b_1()
                                .border_color(colors.border)
                                .child(
                                    div()
                                        .text_size(px(12.))
                                        .font_weight(FontWeight::MEDIUM)
                                        .text_color(colors.text)
                                        .truncate()
                                        .child(column),
                                )
                                .child(
                                    div()
                                        .flex_none()
                                        .text_size(px(11.))
                                        .text_color(colors.text_muted)
                                        .child(format!("row {row} · {meta}")),
                                )
                                .child(div().flex_1())
                                .child(
                                    key_badge("esc", colors)
                                        .id("close-peek")
                                        .cursor_pointer()
                                        .hover(|s| s.text_color(colors.accent))
                                        .on_click(cx.listener(|this, _event, window, cx| {
                                            this.close_peek(window, cx)
                                        })),
                                ),
                        )
                        .child(
                            div()
                                .id("peek-value")
                                .max_h(px(PEEK_MAX_HEIGHT))
                                .overflow_y_scroll()
                                .px(px(15.))
                                .py(px(13.))
                                .text_size(px(12.))
                                .text_color(if value == "NULL" {
                                    // The word is what the grid paints for an
                                    // absence, and it reads as an absence
                                    // there; it has to read the same here.
                                    colors.text_faint
                                } else {
                                    colors.text_body
                                })
                                .child(shown),
                        )
                        .child(
                            div()
                                .flex_none()
                                .flex()
                                .items_center()
                                .gap(px(14.))
                                .px(px(15.))
                                .py(px(9.))
                                .border_t_1()
                                .border_color(colors.border)
                                .bg(colors.panel)
                                .text_size(px(10.))
                                .text_color(colors.text_muted)
                                .child(if cut {
                                    "⌘C copies the whole value"
                                } else {
                                    "⌘C copies this value"
                                })
                                .child(div().flex_1())
                                .child(
                                    div()
                                        .text_color(colors.text_faint)
                                        .child("⏎ or ⎋ closes"),
                                ),
                        ),
                ),
        )
    }

    fn palette_overlay(
        &self,
        colors: &ThemeColors,
        cx: &mut Context<Self>,
    ) -> Option<Stateful<Div>> {
        let palette = self.palette.as_ref()?;

        Some(
            div()
                .id("palette-scrim")
                .absolute()
                .top(px(0.))
                .left(px(0.))
                .size_full()
                .flex()
                .justify_center()
                .items_start()
                .pt(px(palette::TOP_MARGIN))
                .bg(colors.overlay)
                // Nothing behind the scrim may be clicked or hovered
                // through it, or the grid reacts to a click meant to
                // dismiss the palette.
                .occlude()
                .on_click(cx.listener(|this, _event, window, cx| this.close_palette(window, cx)))
                .child(
                    div()
                        .id("palette")
                        .key_context(palette::KEY_CONTEXT)
                        .on_action(cx.listener(Self::on_palette_prev))
                        .on_action(cx.listener(Self::on_palette_next))
                        .on_action(cx.listener(Self::on_palette_new_tab))
                        // A click inside the dialog is not a click on the
                        // scrim, so it must not close it.
                        .occlude()
                        .w(px(palette::WIDTH))
                        .flex()
                        .flex_col()
                        .overflow_hidden()
                        .border_1()
                        .border_color(colors.border_strong)
                        .rounded(px(10.))
                        .bg(colors.elevated)
                        .shadow(vec![
                            BoxShadow::new(px(0.), px(24.), colors.shadow)
                                .blur_radius(px(60.))
                                .spread_radius(px(-20.)),
                        ])
                        .child(self.palette_header(palette, colors, cx))
                        .child(self.palette_chips(palette, colors, cx))
                        .child(self.palette_list(palette, colors, cx))
                        .child(self.palette_footer(palette, colors, cx)),
                ),
        )
    }

    fn palette_header(&self, palette: &Palette, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        div()
            .flex_none()
            .flex()
            .items_center()
            .gap(px(10.))
            .px(px(15.))
            .py(px(13.))
            .border_b_1()
            .border_color(colors.border)
            .child(meerkat_mark(20., cx))
            .child(div().flex_1().min_w(px(0.)).child(palette.query.clone()))
            .child(
                key_badge("esc", colors)
                    .id("close-palette")
                    .cursor_pointer()
                    .hover(|s| s.text_color(colors.accent))
                    .on_click(cx.listener(|this, _event, window, cx| {
                        this.close_palette(window, cx)
                    })),
            )
    }

    fn palette_chips(&self, palette: &Palette, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        // The chip that is lit is the one the search is actually running
        // in, so a typed `t:` lights the tables chip too.
        let (scope, _) = palette::parse(palette.query.read(cx).text(), palette.chip);
        let mut row = div()
            .flex_none()
            .flex()
            .gap(px(3.))
            .px(px(12.))
            .py(px(8.))
            .border_b_1()
            .border_color(colors.hairline)
            .bg(colors.panel);
        for chip in Scope::ALL {
            row = row.child(palette::scope_chip(chip, chip == scope, colors).on_click(
                cx.listener(move |this, _event, window, cx| {
                    this.set_palette_scope(chip, window, cx)
                }),
            ));
        }
        row
    }

    fn palette_list(&self, palette: &Palette, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        if palette.rows.is_empty() {
            return div()
                .h(px(palette::LIST_HEIGHT))
                .flex_none()
                .px(px(17.))
                .pt(px(14.))
                .text_size(px(11.))
                .text_color(colors.text_faint)
                .child(if self.catalog.is_some() {
                    "nothing here goes by that name"
                } else {
                    "reading the catalog…"
                });
        }

        let rows = palette.rows.clone();
        let selected = palette.selected;
        let on_pick: OnPick = {
            let shell = cx.entity().downgrade();
            Rc::new(move |pick, new_tab, window, cx| {
                shell
                    .update(cx, |shell: &mut Shell, cx| {
                        shell.open_pick(pick, new_tab, window, cx)
                    })
                    .ok();
            })
        };

        let mut list = uniform_list("palette-rows", rows.len(), move |range, _window, cx| {
            let colors = theme(cx).colors.clone();
            range
                .map(|ix| {
                    palette::palette_row(ix, &rows[ix], ix == selected, &on_pick, &colors, cx)
                })
                .collect::<Vec<_>>()
        });
        list.style().restrict_scroll_to_axis = Some(true);

        div()
            .h(px(palette::LIST_HEIGHT))
            .flex_none()
            .flex()
            .flex_col()
            .px(px(8.))
            .py(px(8.))
            .child(list.track_scroll(&palette.scroll).flex_1().min_h(px(0.)))
    }

    fn palette_footer(&self, palette: &Palette, colors: &ThemeColors, cx: &App) -> Div {
        let hint = |keys: &'static str, what: &'static str| {
            div()
                .flex()
                .gap(px(5.))
                .child(div().text_color(colors.text_secondary).child(keys))
                .child(what)
        };
        let (_, needle) = palette::parse(palette.query.read(cx).text(), palette.chip);
        // What the search found, in the comp's own words. With nothing
        // typed there is no count worth saying, only what to do next.
        let found = match (needle.is_empty(), palette.matches) {
            (true, _) => "type to search tables and history".to_string(),
            (false, 0) => format!("meerkat found nothing for “{needle}”"),
            (false, 1) => format!("meerkat found 1 match for “{needle}”"),
            (false, n) => format!("meerkat found {n} matches for “{needle}”"),
        };

        div()
            .flex_none()
            .flex()
            .items_center()
            .gap(px(14.))
            .px(px(14.))
            .py(px(9.))
            .border_t_1()
            .border_color(colors.border)
            .bg(colors.panel)
            .text_size(px(10.))
            .text_color(colors.text_muted)
            .child(hint("↑↓", "move"))
            .child(hint("⇥", "complete"))
            .child(hint("⏎", "open"))
            .child(hint("⌘⏎", "new tab"))
            .child(div().flex_1())
            .child(div().text_color(colors.text_faint).truncate().child(found))
    }

    fn page_link(
        &self,
        label: &'static str,
        forward: bool,
        colors: &ThemeColors,
        cx: &Context<Self>,
    ) -> Stateful<Div> {
        let accent = colors.accent;
        div()
            .id(ElementId::Name(label.into()))
            .cursor_pointer()
            .hover(move |s| s.text_color(accent))
            .on_click(cx.listener(move |this, _event, _window, cx| {
                this.step_page(forward, cx);
            }))
            .child(label)
    }
}

/// The sidebar tree, flattened into rows of one height. Headers and
/// relations share the list so `uniform_list` can measure once and lay
/// out only what is on screen.
#[derive(Clone)]
enum CatalogRow {
    /// A schema, and how many relations it holds.
    Schema { key: SharedString, label: SharedString, count: usize, open: bool },
    /// `TABLES` or `VIEWS` inside the schema above it.
    Section { key: SharedString, label: SharedString, count: usize, open: bool },
    Relation { schema: SharedString, name: SharedString, kind: TableKind },
}

/// One schema of the sidebar tree, with its relations split into the two
/// sections under it. Built once per catalog, and flattened into rows
/// again whenever something opens or the filter changes.
struct Group {
    /// Identity of the schema in the open set: its own name.
    key: SharedString,
    label: SharedString,
    schema: SharedString,
    sections: Vec<Section>,
}

/// A schema's tables, or the same schema's views.
struct Section {
    /// Identity of the section in the closed set. A schema name cannot
    /// hold a tab, so the two parts cannot run together into another
    /// section's key.
    key: SharedString,
    label: SharedString,
    kind: TableKind,
    relations: Vec<SharedString>,
}

/// Read the catalog the way the sidebar reads it: a schema, then `TABLES`
/// and `VIEWS` under it, then the relations under those. A section with
/// nothing in it is left out rather than drawn empty.
fn catalog_groups(catalog: &Catalog) -> Rc<Vec<Group>> {
    let mut groups = Vec::new();
    for schema in &catalog.schemas {
        let sections: Vec<Section> = [(TableKind::Table, "TABLES"), (TableKind::View, "VIEWS")]
            .into_iter()
            .filter_map(|(kind, label)| {
                let relations: Vec<SharedString> = schema
                    .tables
                    .iter()
                    .filter(|table| table.kind == kind)
                    .map(|table| table.name.clone().into())
                    .collect();
                (!relations.is_empty()).then(|| Section {
                    key: format!("{}\t{label}", schema.name).into(),
                    label: label.into(),
                    kind,
                    relations,
                })
            })
            .collect();
        if sections.is_empty() {
            continue;
        }
        groups.push(Group {
            key: schema.name.clone().into(),
            label: schema.name.to_ascii_uppercase().into(),
            schema: schema.name.clone().into(),
            sections,
        });
    }
    Rc::new(groups)
}

/// Flatten the tree for `uniform_list`: a schema row, its section rows
/// while it is open, and their relations while they are.
///
/// The two levels remember themselves the other way round. A schema is
/// closed until `open_schemas` holds it, because a catalog of thousands of
/// relations is a wall of names otherwise; a section is open until
/// `closed_sections` holds it, because a schema the user just opened was
/// opened to see what is in it.
///
/// A `needle` narrows the list to the relations it matches, by the same
/// rule the palette uses: the word-start rule, and **a dot names a path**.
/// So `address` finds every `address`, `dev.addr` finds the one in
/// `sample_dev_sample`, and a bare schema name answers with everything
/// under it. Whatever is left with nothing under it is dropped, header and
/// all. While the filter is on, everything that survived is drawn open
/// whatever the two sets say — a search that needs a second click to show
/// its hits is not a search.
///
/// **The hits are ranked, and only while the filter is on.** The catalog's
/// own order is alphabetical, which puts `master` in the middle of the
/// thirteen names that hold the word, and a list whose best answer is
/// eighth is one the user reads before they can use it. So a filtered
/// section is sorted by `palette::path_rank` — the palette's own order, so
/// the same query offers the same name first in both places — and ↑↓ walk
/// it best-first. Unfiltered, nothing is ranked: with no query there is
/// nothing to be closest to, and shuffling a schema's tables on every
/// keystroke of an emptying line would be worse than alphabetical.
fn catalog_rows(
    groups: &[Group],
    open_schemas: &HashSet<SharedString>,
    closed_sections: &HashSet<SharedString>,
    needle: &str,
) -> Rc<Vec<CatalogRow>> {
    let filtering = !needle.is_empty();
    let mut rows = Vec::new();
    for group in groups {
        let found: Vec<(&Section, Vec<&SharedString>)> = group
            .sections
            .iter()
            .filter_map(|section| {
                let matches: Vec<&SharedString> = if !filtering {
                    section.relations.iter().collect()
                } else {
                    let mut ranked: Vec<((usize, i32, usize), &SharedString)> = section
                        .relations
                        .iter()
                        .filter_map(|name| {
                            let path = [group.schema.to_string(), name.to_string()];
                            Some((palette::path_rank(&path, needle)?, name))
                        })
                        .collect();
                    // A stable sort, so two names the query cannot tell
                    // apart stay in the catalog's own order.
                    ranked.sort_by(|(a, _), (b, _)| a.cmp(b));
                    ranked.into_iter().map(|(_, name)| name).collect()
                };
                (!matches.is_empty()).then_some((section, matches))
            })
            .collect();
        let count: usize = found.iter().map(|(_, matches)| matches.len()).sum();
        if count == 0 {
            continue;
        }

        let open = filtering || open_schemas.contains(&group.key);
        rows.push(CatalogRow::Schema {
            key: group.key.clone(),
            label: group.label.clone(),
            count,
            open,
        });
        if !open {
            continue;
        }
        for (section, matches) in found {
            let open = filtering || !closed_sections.contains(&section.key);
            rows.push(CatalogRow::Section {
                key: section.key.clone(),
                label: section.label.clone(),
                count: matches.len(),
                open,
            });
            if open {
                rows.extend(matches.into_iter().map(|name| CatalogRow::Relation {
                    schema: group.schema.clone(),
                    name: name.clone(),
                    kind: section.kind,
                }));
            }
        }
    }
    Rc::new(rows)
}

/// The shell of a header row, up to its chevron: the caller adds the
/// label and the count. `indent` is what puts the level on the tree.
fn header_row(
    ix: usize,
    id: &'static str,
    indent: f32,
    open: bool,
    colors: &ThemeColors,
) -> Stateful<Div> {
    let hover = colors.hairline;
    div()
        .id(ElementId::NamedInteger(id.into(), ix as u64))
        .h(px(CATALOG_ROW_HEIGHT))
        .flex()
        .items_center()
        .gap(px(5.))
        .pl(px(indent))
        .pr(px(8.))
        .rounded(px(5.))
        .cursor_pointer()
        .hover(move |s| s.bg(hover))
        // The chevron is the whole affordance: a closed row shows nothing
        // else that says it can be opened.
        .child(
            div()
                .flex_none()
                .w(px(8.))
                .text_size(px(8.))
                .text_color(colors.text_faint)
                .child(if open { "▾" } else { "▸" }),
        )
}

/// How many relations sit under a header, at its right edge.
fn count_label(count: usize, colors: &ThemeColors) -> Div {
    div()
        .flex_none()
        .text_size(px(9.))
        .text_color(colors.text_faint)
        .child(format_count(count as u64))
}

/// One row of the flattened list. Free-standing because the list's render
/// closure outlives the borrow of the shell it was built from; it reaches
/// the shell again through a weak handle when a row is clicked.
fn catalog_row(
    ix: usize,
    row: &CatalogRow,
    active: Option<&(SharedString, SharedString)>,
    cursor: bool,
    shell: &gpui::WeakEntity<Shell>,
    colors: &ThemeColors,
    cx: &App,
) -> AnyElement {
    match row {
        // The two header levels differ in where they sit and how loud
        // they read; the chevron, the count and the click are the same.
        CatalogRow::Schema { key, label, count, open } => {
            let (key, shell) = (key.clone(), shell.clone());
            header_row(ix, "schema", 6., *open, colors)
                .on_click(move |_event, _window, cx| {
                    shell.update(cx, |shell, cx| shell.toggle_schema(key.clone(), cx)).ok();
                })
                .child(
                    div()
                        .flex_1()
                        .min_w(px(0.))
                        .text_size(px(11.))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.text_secondary)
                        .truncate()
                        .child(label.clone()),
                )
                .child(count_label(*count, colors))
                .into_any_element()
        }
        CatalogRow::Section { key, label, count, open } => {
            let (key, shell) = (key.clone(), shell.clone());
            header_row(ix, "section", 19., *open, colors)
                .on_click(move |_event, _window, cx| {
                    shell.update(cx, |shell, cx| shell.toggle_section(key.clone(), cx)).ok();
                })
                .child(div().flex_1().min_w(px(0.)).child(section_label(label.clone(), cx)))
                .child(count_label(*count, colors))
                .into_any_element()
        }
        CatalogRow::Relation { schema, name, kind } => {
            let is_active = active.is_some_and(|(s, t)| s == schema && t == name);
            // Two rows read loud, and for the same reason: this is the one
            // being pointed at. The tab's row says where the result on
            // screen came from, the cursor's says what ⏎ would open.
            let lit = is_active || cursor;
            let shell = shell.clone();
            let (schema_name, table_name) = (schema.to_string(), name.to_string());

            let item = div()
                .id(ElementId::NamedInteger("relation".into(), ix as u64))
                .h(px(CATALOG_ROW_HEIGHT))
                .flex()
                .items_center()
                .gap(px(8.))
                // Indented under its section's chevron, so the schema and
                // the section a relation sits in read off the left edge.
                .pl(px(32.))
                .pr(px(8.))
                .rounded(px(5.))
                .cursor_pointer()
                .on_click(move |_event, window, cx| {
                    shell
                        .update(cx, |shell, cx| {
                            shell.browse_table(&schema_name, &table_name, window, cx)
                        })
                        .ok();
                })
                .child(match kind {
                    TableKind::Table => table_glyph(lit, cx),
                    TableKind::View => div()
                        .size(px(5.))
                        .rounded_full()
                        .border_1()
                        .border_color(if lit { colors.accent } else { colors.text_faint }),
                })
                .child(
                    div()
                        .flex_1()
                        .min_w(px(0.))
                        .text_size(px(12.))
                        .font_weight(if lit { FontWeight::MEDIUM } else { FontWeight::NORMAL })
                        .text_color(if lit { colors.text } else { colors.text_secondary })
                        .truncate()
                        .child(name.clone()),
                );

            match (cursor, is_active) {
                // The cursor's mark is the deeper of the two, and it has
                // to be: the row it is on is often the row the active tab
                // came from as well, and one mark cannot say both. It is
                // the mark the grid's own cursor wears, for the same
                // reason — it sits over `selection` and stays visible.
                (true, _) => item.bg(colors.match_strong).into_any_element(),
                (false, true) => item.bg(colors.selection).into_any_element(),
                (false, false) => {
                    let hover = colors.hairline;
                    item.hover(move |s| s.bg(hover)).into_any_element()
                }
            }
        }
    }
}

/// A keystroke, in the bordered pill the comp puts one in.
/// One row of the column-find list: where the column sits in the result,
/// its name, and the one thing worth saying about it.
///
/// The trailing mark says **`cursor`** for the column the cursor is already
/// in and **⏎** for the one the key would jump to. They are never both:
/// "you are here" outranks "you could go here", and a row that claimed both
/// would be saying the jump goes nowhere.
fn column_row(
    ix: usize,
    (column, name): &(usize, SharedString),
    selected: bool,
    cursor: Option<usize>,
    jump: &Rc<dyn Fn(usize, &mut Window, &mut App)>,
    colors: &ThemeColors,
) -> Stateful<Div> {
    let (mark, mark_ink) = if cursor == Some(*column) {
        ("cursor", colors.accent_deep)
    } else if selected {
        ("⏎", colors.accent)
    } else {
        ("", colors.accent)
    };
    let column = *column;
    let jump = jump.clone();

    let mut row = div()
        .id(ix)
        .h(px(COLUMN_ROW_HEIGHT))
        .flex()
        .items_center()
        .gap(px(9.))
        .px(px(8.))
        .rounded(px(5.))
        .cursor_pointer()
        .on_click(move |_event, window, cx| jump(column, window, cx))
        .child(
            // The lane's own number, counted from one as the header reads,
            // so the popover and the result agree about where a column is.
            div()
                .w(px(15.))
                .flex_none()
                .text_size(px(9.))
                .text_color(colors.line_number)
                .child(format!("{}", column + 1)),
        )
        .child(
            div()
                .flex_1()
                .min_w(px(0.))
                .text_size(px(11.))
                .text_color(if selected { colors.text } else { colors.text_secondary })
                .truncate()
                .child(name.clone()),
        )
        .child(div().flex_none().text_size(px(10.)).text_color(mark_ink).child(mark));
    row = if selected {
        row.bg(colors.selection)
    } else {
        let hover = colors.panel;
        row.hover(move |s| s.bg(hover))
    };
    row
}

/// The comp's keycap, as it appears **inside a button**: a small box with a
/// 2px bottom edge, which is the whole of what makes it read as a key
/// rather than as a chip.
///
/// Every button in the app that carries a key uses this one — the run
/// button's three verbs, and the transaction bar's commit and rollback.
/// They are the only buttons that name a key, and a key drawn as a key on
/// one and as trailing text on another would say they were different kinds
/// of thing.
///
/// The tones are the caller's, because they follow the **fill** rather than
/// the key: `key_on_fill_*` over a filled button, paper over an outlined
/// one. Both of the alpha pair carry alpha for exactly this reason — one
/// cap has to work over ochre and over clay without a tone per state.
///
/// **The padding is in proportion to the type**, the comp's 6 and 4 at its
/// own 11px. A smaller cap has to be smaller in every direction: a 9px key
/// in a box built for an 11px one is a small word in a big box, which reads
/// as a mistake rather than as a smaller key. The bottom edge stays at 2px
/// through all of it — it is what says "key", and 1.6px of it would only
/// blur.
///
/// [`key_badge`] is the flat cousin, for a key named in a hint or a footer
/// rather than written on a button.
fn keycap(keys: &'static str, size: f32, surface: Hsla, border: Hsla, ink: Hsla) -> Div {
    div()
        .flex_none()
        .px(px(size * 6. / 11.))
        .py(px(size * 4. / 11.))
        .border_1()
        .border_b_2()
        .border_color(border)
        .rounded(px(5.))
        .bg(surface)
        .text_size(px(size))
        .font_weight(FontWeight::MEDIUM)
        .text_color(ink)
        .child(keys)
}

fn key_badge(keys: &'static str, colors: &ThemeColors) -> Div {
    div()
        .flex_none()
        .px(px(6.))
        .py(px(4.))
        .border_1()
        .border_color(colors.border_strong)
        .rounded(px(4.))
        .bg(colors.window)
        .text_size(px(10.))
        .font_weight(FontWeight::MEDIUM)
        .text_color(colors.text_muted)
        .child(keys)
}

/// What the close confirmation says: title, body, the keeping answer, the
/// ending one.
///
/// Pure, so the wording is testable without a window — and so the three
/// ways out cannot drift into saying three unrelated things. The body
/// names what happens on the *server*, because that is what the user
/// cannot see and cannot undo.
type ConfirmCopy = (String, Vec<String>, &'static str, &'static str);

fn confirm_copy(what: Close, running: &[SharedString], open: &[SharedString]) -> ConfirmCopy {
    let (title, verb, keep, act) = match what {
        Close::Tab(_) => {
            let named = running.first().or_else(|| open.first());
            let first = named.map(|name| name.to_string()).unwrap_or_default();
            (format!("Close “{first}”?"), "Closing the tab", "Keep tab", "Close it")
        }
        Close::Shell => {
            ("Leave this connection?".to_string(), "Leaving", "Stay", "Leave anyway")
        }
        Close::Window => ("Quit Meerkat?".to_string(), "Quitting", "Stay", "Quit anyway"),
    };

    // One line per thing at stake, because they are lost in different
    // ways: a run the server can be asked to give up, a transaction it
    // throws away. A close that said only "are you sure" would be telling
    // the user nothing they could not already see.
    let mut body = Vec::new();
    if !running.is_empty() {
        body.push(if running.len() == 1 {
            format!(
                "“{}” is still running a statement. {verb} asks the server to stop it.",
                running[0]
            )
        } else {
            format!(
                "{} tabs are still running statements. {verb} asks the server to stop them.",
                running.len()
            )
        });
    }
    if !open.is_empty() {
        // "rolls it back" and nothing more dramatic: the session is
        // read-only unless the user said otherwise, so a rollback often
        // costs nothing but the transaction itself. Saying the work is
        // lost would be a warning the app cannot always stand behind.
        body.push(if open.len() == 1 {
            format!("“{}” has a transaction open. {verb} rolls it back.", open[0])
        } else {
            format!("{} tabs have transactions open. {verb} rolls them back.", open.len())
        });
    }
    (title, body, keep, act)
}

/// One open session, as the sweep and the cap need to see it. Kept apart
/// from the tab so the policy below is a function of plain values, and can
/// be argued with in a test rather than in a running window.
#[derive(Clone, Copy, Debug)]
struct SessionState {
    tab_id: u64,
    running: bool,
    in_transaction: bool,
    idle: Duration,
}

/// Whether a session may be taken back at all.
///
/// A run in flight is using it. **A transaction is the one that must never
/// be taken**: handing the connection back rolls the user's work away with
/// nobody asking, which is exactly what the close dialog exists to refuse
/// to do quietly. So a tab mid-transaction keeps its connection however
/// long it sits there, and the cap is what gives way instead.
fn spare(state: &SessionState) -> bool {
    !state.running && !state.in_transaction
}

/// The sessions nobody has used for `after`.
///
/// A laptop left open overnight must not hold every connection against a
/// shared server for a window nobody is looking at. The cost of being
/// wrong is small and visible: the tab says the session ended, and its
/// next run opens another.
fn idle_sessions(states: &[SessionState], after: Duration) -> Vec<u64> {
    states
        .iter()
        .filter(|state| spare(state) && state.idle >= after)
        .map(|state| state.tab_id)
        .collect()
}

/// Which session to give back when one more is wanted: the one that has
/// gone longest without a question. `None` means none may be taken, and
/// the caller must refuse rather than pick a session that is holding
/// something.
fn evictable(states: &[SessionState]) -> Option<u64> {
    states.iter().filter(|state| spare(state)).max_by_key(|state| state.idle).map(|s| s.tab_id)
}

/// Whether a run has to open a transaction before it sends anything.
///
/// Three ways to answer no, and each of them matters: auto mode holds no
/// transaction at all; one is open already, so a second `BEGIN` would be a
/// warning from the server and a lie in the bar; and the buffer opens its
/// own, which is the user saying where the transaction starts.
fn needs_begin(mode: TxMode, in_transaction: bool, first: Option<TxVerb>) -> bool {
    mode == TxMode::Manual && !in_transaction && first != Some(TxVerb::Begin)
}

/// Everything the transaction bar reads, as plain values.
///
/// Kept apart from the tab so the wording below is a function of numbers
/// and flags, and can be argued with in a test rather than in a running
/// window — the reason `confirm_copy` and the session policy are written
/// the same way.
#[derive(Clone, Copy, Debug)]
struct TxState {
    mode: TxMode,
    /// Whether a transaction is open, as the server last said.
    open: bool,
    /// How the last one ended, when nothing is open.
    done: Option<TxEnd>,
    /// Statements that have landed inside the open transaction.
    statements: usize,
    /// A run is out on this tab's session.
    running: bool,
    /// A commit or a rollback is out.
    ending: bool,
}

/// What the transaction bar says: the state, and the line under it.
///
/// `None` is the ordinary case — no transaction, and none just ended — and
/// it means the bar is not painted at all. A strip that said "no
/// transaction" would be a strip that is always there saying nothing.
fn tx_copy(state: TxState) -> Option<(&'static str, String)> {
    if state.open {
        let title = "transaction open";
        if state.ending {
            return Some((title, "ending it · waiting for the server".to_string()));
        }
        if state.running {
            // The session is one connection and the run has it, so the
            // buttons cannot be answered yet. Say which key clears the way.
            return Some((title, "a run is out · ⌘. stops it first".to_string()));
        }
        let held = match state.statements {
            // The transaction is open and nothing has landed in it: a run
            // that failed on its first statement leaves exactly this.
            0 => "nothing has landed in it yet".to_string(),
            1 => "1 statement".to_string(),
            n => format!("{n} statements"),
        };
        return Some((title, format!("{held} · nothing visible to anyone else yet")));
    }
    match state.done? {
        // What happens *next* differs by mode, and the line has to be
        // right about it: in auto mode nothing opens another transaction.
        TxEnd::Commit => Some((
            "committed",
            match state.mode {
                TxMode::Manual => {
                    "changes are live · a new transaction opens on your next run".to_string()
                }
                TxMode::Auto => {
                    "changes are live · statements commit on their own again".to_string()
                }
            },
        )),
        TxEnd::Rollback => {
            Some(("rolled back", "all changes discarded · nothing was written".to_string()))
        }
    }
}

/// What the toolbar says about the tab's own connection, when there is
/// anything to say.
///
/// Two states earn a mark, and neither is "a session is open" — that is
/// the ordinary case and needs no badge. **IN TRANSACTION** is a state the
/// user has to be able to see, because everything the tab does is inside
/// it and closing the tab rolls it back. **session ended** explains a
/// reset the user did not ask for: the sweep took the connection back, so
/// a `search_path` set an hour ago is gone, and a mark is the difference
/// between that and a mystery.
///
/// One state earns a mark, and it is not "a session is open" — that is the
/// ordinary case and needs no badge. **session ended** explains a reset the
/// user did not ask for: the sweep took the connection back, so a
/// `search_path` set an hour ago is gone, and a mark is the difference
/// between that and a mystery.
///
/// An open transaction used to be a badge here as well. It is not any more,
/// because the transaction bar says it in a whole strip and offers the two
/// ways out — and the toolbar's mode switch carries the same dot. Three
/// marks for one state is two too many.
fn session_mark(tab: &QueryTab, colors: &ThemeColors) -> Option<Div> {
    if !tab.session_ended {
        return None;
    }
    Some(
        // Shrinkable, not `flex_none`: it is the longest thing on the
        // toolbar's left side, and a note about a connection that reset
        // must not be what pushes the run button off the edge.
        div()
            .min_w(px(0.))
            .text_size(px(11.))
            .text_color(colors.text_faint)
            .truncate()
            .child("session ended · a run opens a new one"),
    )
}

/// The clause the result line adds when the memory cap ended the read.
///
/// It is in the accent's family, not the error's: nothing failed, and the
/// rows on screen are real rows. The app declined to hold the rest. Only an
/// idle run says it — while a run is out, the grid still shows the result
/// before it, and that result's cap is not news about this one.
fn cap_note(tab: &QueryTab, colors: &ThemeColors) -> Option<Div> {
    if !tab.truncated || tab.run.in_flight() {
        return None;
    }
    Some(
        div()
            .font_weight(FontWeight::MEDIUM)
            .text_color(colors.accent_deep)
            .child(format!(
                "capped at {} MB · select fewer columns, or add a LIMIT",
                db_client::MAX_BYTES / 1024 / 1024
            )),
    )
}

/// The design's error tone: warm surface, warm border, warm text.
fn error_strip(message: String, colors: &ThemeColors) -> Div {
    div()
        .flex_none()
        .px(px(14.))
        .py(px(9.))
        .border_b_1()
        .border_color(colors.error_border)
        .bg(colors.error_surface)
        .text_size(px(11.))
        .text_color(colors.error)
        .child(message)
}

/// Never paint a password, not even in the "could not connect" card.
fn redact(url: &str) -> String {
    let Some(scheme_end) = url.find("://") else { return url.to_string() };
    let (scheme, rest) = url.split_at(scheme_end + 3);
    let Some(at) = rest.find('@') else { return url.to_string() };
    let (credentials, host) = rest.split_at(at);
    match credentials.split_once(':') {
        Some((user, _)) => format!("{scheme}{user}:•••{host}"),
        None => url.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use introspect::Schema;

    /// The arming window, which is the whole of why the button's state is a
    /// type of its own: for the first `RUN_ARM` the statement is out and the
    /// button still says "run".
    #[test]
    fn the_button_offers_a_stop_only_once_a_run_has_lasted() {
        let ago = |ago: Duration| Live { started: Instant::now() - ago, backend: None };
        assert_eq!(Run::Idle.phase(), RunPhase::Ready);
        assert_eq!(Run::Cancelled { elapsed: 120 }.phase(), RunPhase::Ready);
        // A statement that answers inside the window never swaps the verb,
        // which is the point: most of them answer inside it.
        let quick = Run::Running(ago(Duration::from_millis(80)));
        assert_eq!(quick.phase(), RunPhase::Ready);
        assert!(quick.arming());
        let slow = Run::Running(ago(RUN_ARM));
        assert_eq!(slow.phase(), RunPhase::Stop);
        assert!(!slow.arming());
        // A cancel is with the server, so the second press is the one that
        // closes the backend — and that state never arms.
        let cancelling = Run::Cancelling(ago(Duration::from_millis(10)));
        assert_eq!(cancelling.phase(), RunPhase::Terminate);
        assert!(!cancelling.arming());
        // Nothing is out, so a click runs a query.
        assert!(!Run::Idle.arming());
    }

    /// The sidebar cursor's ring, with the filter line itself in it: ↓
    /// walks into the names and ↑ walks back out to the line.
    #[test]
    fn the_sidebar_cursor_walks_out_of_the_list_it_walked_into() {
        // Indices into the flattened rows, so the headers between them are
        // the gaps: a schema heading, two tables, a section heading, one
        // more table.
        let stops = [1_usize, 2, 4];
        // The line is where a fresh filter leaves the cursor, and each key
        // enters the list from its own end.
        assert_eq!(step_stop(&stops, None, true), Some(1));
        assert_eq!(step_stop(&stops, None, false), Some(4));
        // One name at a time, over the headings.
        assert_eq!(step_stop(&stops, Some(1), true), Some(2));
        assert_eq!(step_stop(&stops, Some(2), true), Some(4));
        assert_eq!(step_stop(&stops, Some(4), false), Some(2));
        // Both ends come back to the line, which is where the user is
        // still typing.
        assert_eq!(step_stop(&stops, Some(4), true), None);
        assert_eq!(step_stop(&stops, Some(1), false), None);
        // Nothing matched: there is nowhere to walk to.
        assert_eq!(step_stop(&[], None, true), None);
    }

    /// Manual mode's whole mechanism: one `BEGIN`, and never a second.
    #[test]
    fn a_run_opens_a_transaction_only_when_manual_mode_has_none() {
        // Auto mode holds nothing open, whatever the buffer says.
        assert!(!needs_begin(TxMode::Auto, false, None));
        assert!(!needs_begin(TxMode::Auto, false, Some(TxVerb::Begin)));
        // Manual mode with nothing open opens one.
        assert!(needs_begin(TxMode::Manual, false, None));
        // ...and never a second: the transaction spans the runs of the tab.
        assert!(!needs_begin(TxMode::Manual, true, None));
        // The buffer's own `BEGIN` is the user saying where it starts.
        assert!(!needs_begin(TxMode::Manual, false, Some(TxVerb::Begin)));
        // A buffer that ends a transaction still needs one to end.
        assert!(needs_begin(TxMode::Manual, false, Some(TxVerb::Commit)));
    }

    /// The bar is painted for a transaction the *user* opened as well, in
    /// either mode — which is the whole of what "handle a typed BEGIN"
    /// means on screen.
    #[test]
    fn the_transaction_bar_speaks_for_both_modes() {
        let state = |mode, open, done, statements| TxState {
            mode,
            open,
            done,
            statements,
            running: false,
            ending: false,
        };
        // Nothing open and nothing just ended: no bar at all.
        assert!(tx_copy(state(TxMode::Auto, false, None, 0)).is_none());
        assert!(tx_copy(state(TxMode::Manual, false, None, 0)).is_none());

        // A `BEGIN` typed into an auto-mode buffer gets the same bar as a
        // transaction manual mode opened.
        let (title, sub) = tx_copy(state(TxMode::Auto, true, None, 2)).unwrap();
        assert_eq!(title, "transaction open");
        assert!(sub.starts_with("2 statements · "), "{sub}");
        let (manual_title, _) = tx_copy(state(TxMode::Manual, true, None, 1)).unwrap();
        assert_eq!(manual_title, title);
        // One statement is not "1 statements".
        let (_, sub) = tx_copy(state(TxMode::Manual, true, None, 1)).unwrap();
        assert!(sub.starts_with("1 statement · "), "{sub}");
        // A run that failed on its first statement leaves a transaction
        // with nothing in it, and the line says so rather than "0".
        let (_, sub) = tx_copy(state(TxMode::Manual, true, None, 0)).unwrap();
        assert!(sub.starts_with("nothing has landed in it yet"), "{sub}");
    }

    /// What happens *next* differs by mode, so the line after a commit
    /// cannot be one sentence for both: in auto mode nothing opens another
    /// transaction.
    #[test]
    fn the_line_after_a_commit_says_what_the_mode_does_next() {
        let ended = |mode, done| TxState {
            mode,
            open: false,
            done: Some(done),
            statements: 0,
            running: false,
            ending: false,
        };
        let (title, sub) = tx_copy(ended(TxMode::Manual, TxEnd::Commit)).unwrap();
        assert_eq!(title, "committed");
        assert!(sub.ends_with("a new transaction opens on your next run"), "{sub}");
        let (_, sub) = tx_copy(ended(TxMode::Auto, TxEnd::Commit)).unwrap();
        assert!(sub.ends_with("statements commit on their own again"), "{sub}");
        let (title, sub) = tx_copy(ended(TxMode::Auto, TxEnd::Rollback)).unwrap();
        assert_eq!(title, "rolled back");
        assert!(sub.contains("nothing was written"), "{sub}");
    }

    /// A run holds the one connection the transaction is on, so the bar
    /// says which key clears the way rather than offering a button that
    /// would have to queue behind the run.
    #[test]
    fn a_run_in_flight_is_named_in_the_bar() {
        let state = TxState {
            mode: TxMode::Manual,
            open: true,
            done: None,
            statements: 3,
            running: true,
            ending: false,
        };
        let (_, sub) = tx_copy(state).unwrap();
        assert!(sub.contains("⌘."), "{sub}");
        let (_, sub) = tx_copy(TxState { running: false, ending: true, ..state }).unwrap();
        assert!(sub.contains("waiting for the server"), "{sub}");
    }

    /// The shape a VPN puts the app in, and the reason the split exists: a
    /// third of a second of waiting, of which the database did twelve
    /// milliseconds' work. One number could not have said that.
    #[test]
    fn the_server_and_the_lag_are_reported_apart() {
        let mut timing =
            Timing::new(340, Wire { link_ms: Some(33), first_row_ms: Some(46), fetch_ms: Some(294) });
        timing.server = Some(ServerTiming { exec_ms: 12.4, plan_ms: Some(0.6), exact: true });
        assert_eq!(timing.summary(), "queried in 340 ms · server 13 ms · lag 327 ms");
    }

    /// Until the server answers — and for ever without
    /// `pg_stat_statements` — the estimate stands, and `~` says it is one.
    #[test]
    fn an_estimated_server_time_is_marked() {
        let timing =
            Timing::new(340, Wire { link_ms: Some(33), first_row_ms: Some(46), fetch_ms: Some(294) });
        // The same numbers as the measured run above, and only the `~`
        // between them: the mark is the whole of what says which is which.
        assert_eq!(timing.summary(), "queried in 340 ms · server ~13 ms · lag 327 ms");
    }

    /// The estimate is time-to-first-row **less the round trip**. Leaving
    /// the round trip in would charge the database for the link, which is
    /// the mistake the whole split is here to fix.
    #[test]
    fn the_estimate_takes_the_round_trip_off() {
        let far = Timing::new(400, Wire { link_ms: Some(120), first_row_ms: Some(132), fetch_ms: Some(268) });
        let near = Timing::new(280, Wire { link_ms: Some(1), first_row_ms: Some(13), fetch_ms: Some(267) });
        // The same server work, seen down two very different links.
        assert_eq!(far.server_ms().map(|(ms, _)| ms), Some(12.));
        assert_eq!(near.server_ms().map(|(ms, _)| ms), Some(12.));
    }

    /// A server figure larger than the wall clock is possible — the two are
    /// measured by different clocks, and the server's covers work that
    /// overlapped the fetch. The lag floors at zero rather than going
    /// negative, which would read as the link giving time back.
    #[test]
    fn the_lag_never_goes_negative() {
        let mut timing = Timing::new(10, Wire::default());
        timing.server = Some(ServerTiming { exec_ms: 14., plan_ms: None, exact: true });
        assert_eq!(timing.lag_ms(), Some(0.));
    }

    /// A local file has no link to separate out, so the engine offers
    /// neither half and the strip says only what it waited.
    #[test]
    fn an_engine_with_no_split_reports_the_total_alone() {
        let timing = Timing::new(7, Wire::default());
        assert_eq!(timing.server_ms(), None);
        assert_eq!(timing.summary(), "queried in 7 ms");
    }

    fn relation(name: &str, kind: TableKind) -> Table {
        Table { name: name.to_string(), kind, columns: Vec::new(), primary_key: Vec::new(), approx_rows: None }
    }

    fn names(names: &[&str]) -> Vec<SharedString> {
        names.iter().map(|name| SharedString::from(name.to_string())).collect()
    }

    /// Each way out names itself and names what it ends. The three must
    /// not drift into saying three unrelated things, which is the whole
    /// reason the copy is one function.
    #[test]
    fn the_confirmation_names_the_way_out_it_is_guarding() {
        let one = names(&["query 3"]);
        let none: Vec<SharedString> = Vec::new();

        let (title, body, keep, act) = confirm_copy(Close::Tab(7), &one, &none);
        assert_eq!(title, "Close “query 3”?");
        assert!(body[0].contains("stop it"), "{body:?}");
        assert_eq!((keep, act), ("Keep tab", "Close it"));

        let (title, _, keep, act) = confirm_copy(Close::Shell, &one, &none);
        assert_eq!(title, "Leave this connection?");
        assert_eq!((keep, act), ("Stay", "Leave anyway"));

        let (title, _, _, act) = confirm_copy(Close::Window, &one, &none);
        assert_eq!(title, "Quit Meerkat?");
        assert_eq!(act, "Quit anyway");

        // A tab with only a transaction open is still named in the title:
        // the dialog must say which tab it is asking about.
        let (title, body, ..) = confirm_copy(Close::Tab(7), &none, &one);
        assert_eq!(title, "Close “query 3”?");
        assert_eq!(body, vec!["“query 3” has a transaction open. Closing the tab rolls it back."]);
    }

    /// Leaving with one run out names it; leaving with several counts
    /// them, and the dialog lists the names underneath.
    #[test]
    fn the_confirmation_counts_what_it_would_end() {
        let none: Vec<SharedString> = Vec::new();

        let body = confirm_copy(Close::Window, &names(&["query 3"]), &none).1;
        assert_eq!(
            body,
            vec!["“query 3” is still running a statement. Quitting asks the server to stop it."]
        );

        let body = confirm_copy(Close::Shell, &names(&["query 3", "query 7"]), &none).1;
        assert_eq!(
            body,
            vec!["2 tabs are still running statements. Leaving asks the server to stop them."]
        );

        let body = confirm_copy(Close::Shell, &none, &names(&["a", "b", "c"])).1;
        assert_eq!(body, vec!["3 tabs have transactions open. Leaving rolls them back."]);
    }

    /// A tab can be both, and then the dialog says both — they are lost in
    /// different ways, so one line cannot stand for the other.
    #[test]
    fn the_confirmation_says_both_when_both_are_true() {
        let one = names(&["query 3"]);
        let (_, body, _, act) = confirm_copy(Close::Tab(7), &one, &one);
        assert_eq!(body.len(), 2, "{body:?}");
        assert!(body[0].contains("asks the server to stop it"), "{body:?}");
        assert!(body[1].contains("rolls it back"), "{body:?}");
        assert_eq!(act, "Close it");
    }

    fn session(tab_id: u64, idle_secs: u64) -> SessionState {
        SessionState {
            tab_id,
            running: false,
            in_transaction: false,
            idle: Duration::from_secs(idle_secs),
        }
    }

    #[test]
    fn the_sweep_takes_back_what_has_gone_unused() {
        let states = vec![session(1, 900), session(2, 60), session(3, 601)];
        assert_eq!(idle_sessions(&states, IDLE_SESSION), vec![1, 3]);
        // Ten minutes on the nose is not yet ten minutes past.
        assert_eq!(idle_sessions(&[session(1, 599)], IDLE_SESSION), Vec::<u64>::new());
    }

    /// The two states a session is never taken in. A run is using it; a
    /// transaction would be rolled away with nobody asking, which is the
    /// thing the close dialog exists to refuse to do quietly.
    #[test]
    fn the_sweep_never_takes_a_busy_or_transacting_session() {
        let running = SessionState { running: true, ..session(1, 9000) };
        let holding = SessionState { in_transaction: true, ..session(2, 9000) };
        assert_eq!(idle_sessions(&[running, holding], IDLE_SESSION), Vec::<u64>::new());
        assert_eq!(evictable(&[running, holding]), None);
    }

    /// The cap gives back the session that has gone longest without a
    /// question — and refuses rather than pick one that is holding
    /// something, because both ways out of that cost the user something
    /// the app may not choose for them.
    #[test]
    fn the_cap_gives_back_the_longest_idle_session() {
        let states = vec![session(1, 30), session(2, 300), session(3, 120)];
        assert_eq!(evictable(&states), Some(2));

        let held = vec![SessionState { in_transaction: true, ..session(1, 9000) }, {
            SessionState { running: true, ..session(2, 9000) }
        }];
        assert_eq!(evictable(&held), None);
    }

    #[test]
    fn walking_the_tabs_wraps_at_both_ends() {
        assert_eq!(step_wrapping(3, 0, true), 1);
        assert_eq!(step_wrapping(3, 1, true), 2);
        // Past the last tab is the first one again.
        assert_eq!(step_wrapping(3, 2, true), 0);
        // ⌃⇧⇥ walks the other way, and wraps as well.
        assert_eq!(step_wrapping(3, 0, false), 2);
        assert_eq!(step_wrapping(3, 2, false), 1);
    }

    #[test]
    fn one_tab_or_none_stays_put() {
        assert_eq!(step_wrapping(0, 0, true), 0);
        assert_eq!(step_wrapping(1, 0, true), 0);
        assert_eq!(step_wrapping(1, 0, false), 0);
    }

    /// The rows as the sidebar draws them: a schema in `[]`, a section in
    /// `()`, both with their count and whether they are open, and a
    /// relation as its own name.
    fn read(rows: &[CatalogRow]) -> Vec<String> {
        let shown = |count: &usize, open: &bool| {
            format!("{count}{}", if *open { " open" } else { "" })
        };
        rows.iter()
            .map(|row| match row {
                CatalogRow::Schema { label, count, open, .. } => {
                    format!("[{label} {}]", shown(count, open))
                }
                CatalogRow::Section { label, count, open, .. } => {
                    format!("({label} {})", shown(count, open))
                }
                CatalogRow::Relation { name, .. } => name.to_string(),
            })
            .collect()
    }

    /// `catalog_rows` with both sets at their starting state.
    fn rows(groups: &[Group], needle: &str) -> Rc<Vec<CatalogRow>> {
        catalog_rows(groups, &HashSet::new(), &HashSet::new(), needle)
    }

    fn sample() -> Catalog {
        Catalog {
            schemas: vec![
                Schema {
                    name: "public".to_string(),
                    tables: vec![
                        relation("users", TableKind::Table),
                        relation("active_users", TableKind::View),
                        relation("orders", TableKind::Table),
                    ],
                },
                // A schema of views only gets a `VIEWS` section, not an
                // empty `TABLES` one above it.
                Schema {
                    name: "reporting".to_string(),
                    tables: vec![relation("daily", TableKind::View)],
                },
            ],
        }
    }

    #[test]
    fn a_schema_holds_a_tables_section_and_a_views_section() {
        let groups = catalog_groups(&sample());
        let read: Vec<String> = groups.iter().map(|group| group.label.to_string()).collect();
        assert_eq!(read, ["PUBLIC", "REPORTING"]);

        let sections: Vec<String> =
            groups[0].sections.iter().map(|section| section.label.to_string()).collect();
        assert_eq!(sections, ["TABLES", "VIEWS"]);
        assert_eq!(groups[0].sections[0].relations, ["users", "orders"]);
        // A schema of views only carries the one section.
        assert_eq!(groups[1].sections.len(), 1);
    }

    #[test]
    fn every_schema_starts_closed() {
        let groups = catalog_groups(&sample());
        // The schemas and their totals, and nothing under them.
        assert_eq!(read(&rows(&groups, "")), ["[PUBLIC 3]", "[REPORTING 1]"]);
    }

    #[test]
    fn an_open_schema_shows_its_sections_and_their_relations() {
        let groups = catalog_groups(&sample());
        let open = HashSet::from([groups[0].key.clone()]);
        // A section is open until it is closed: opening the schema is one
        // click, not three.
        assert_eq!(
            read(&catalog_rows(&groups, &open, &HashSet::new(), "")),
            [
                "[PUBLIC 3 open]",
                "(TABLES 2 open)",
                "users",
                "orders",
                "(VIEWS 1 open)",
                "active_users",
                "[REPORTING 1]",
            ]
        );

        // Closing a section leaves its header and takes its relations.
        let closed = HashSet::from([groups[0].sections[0].key.clone()]);
        assert_eq!(
            read(&catalog_rows(&groups, &open, &closed, "")),
            ["[PUBLIC 3 open]", "(TABLES 2)", "(VIEWS 1 open)", "active_users", "[REPORTING 1]"]
        );
    }

    #[test]
    fn the_filter_opens_what_it_matched() {
        let groups = catalog_groups(&sample());
        // A relation name: only what holds a hit survives, open however
        // the two sets stand, and the counts are of the hits.
        assert_eq!(
            read(&rows(&groups, "user")),
            ["[PUBLIC 2 open]", "(TABLES 1 open)", "users", "(VIEWS 1 open)", "active_users"]
        );
        // A schema's own name keeps everything under it.
        assert_eq!(
            read(&rows(&groups, "report")),
            ["[REPORTING 1 open]", "(VIEWS 1 open)", "daily"]
        );
        // No hit anywhere is an empty list, not a list of empty headers.
        assert!(rows(&groups, "nothing").is_empty());
    }

    #[test]
    fn the_filter_puts_its_closest_hit_first() {
        let catalog = Catalog {
            schemas: vec![Schema {
                name: "app".to_string(),
                tables: vec![
                    relation("correspondence_master", TableKind::Table),
                    relation("master_assignment", TableKind::Table),
                    relation("master", TableKind::Table),
                    relation("master_rate", TableKind::Table),
                ],
            }],
        };
        let groups = catalog_groups(&catalog);
        // Alphabetical order buries the name the query says outright, so
        // the filtered section is ranked instead: the whole word first,
        // then the names it starts, shortest first, then the name that
        // only holds it further in.
        assert_eq!(
            read(&rows(&groups, "master")),
            [
                "[APP 4 open]",
                "(TABLES 4 open)",
                "master",
                "master_rate",
                "master_assignment",
                "correspondence_master",
            ]
        );
        // With no filter the catalog's own order stands.
        let open = HashSet::from([groups[0].key.clone()]);
        assert_eq!(
            read(&catalog_rows(&groups, &open, &HashSet::new(), ""))[2..],
            ["correspondence_master", "master_assignment", "master", "master_rate"]
        );
    }

    #[test]
    fn a_dot_in_the_filter_names_a_path() {
        let groups = catalog_groups(&sample());
        // Both parts must hit: the schema, then the relation under it.
        assert_eq!(
            read(&rows(&groups, "public.orders")),
            ["[PUBLIC 1 open]", "(TABLES 1 open)", "orders"]
        );
        // Part of each is enough, as in the palette.
        assert_eq!(
            read(&rows(&groups, "rep.dai")),
            ["[REPORTING 1 open]", "(VIEWS 1 open)", "daily"]
        );
        // A trailing dot asks for everything the schema holds.
        assert_eq!(
            read(&rows(&groups, "public.")),
            [
                "[PUBLIC 3 open]",
                "(TABLES 2 open)",
                "users",
                "orders",
                "(VIEWS 1 open)",
                "active_users",
            ]
        );
        // The schema of the hit is part of the query, so a relation of the
        // right name under the wrong schema is not an answer.
        assert!(rows(&groups, "reporting.orders").is_empty());
        // Case never matters, on either side of the dot.
        assert_eq!(
            read(&rows(&groups, "PUBLIC.Orders")),
            ["[PUBLIC 1 open]", "(TABLES 1 open)", "orders"]
        );
    }

    #[test]
    fn passwords_never_reach_the_screen() {
        assert_eq!(
            redact("postgres://ada:hunter2@db.internal:5432/app"),
            "postgres://ada:•••@db.internal:5432/app"
        );
        assert_eq!(
            redact("postgres://ada@db.internal/app"),
            "postgres://ada@db.internal/app"
        );
        assert_eq!(redact("postgres:///app"), "postgres:///app");
    }
}
