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

use db_client::{Connection, Profile, QueryResult, ReportRun, RunId, Stop};
use db_postgres::{Label, PostgresConnection};
use gpui::{
    AnyElement, App, BoxShadow, Context, Div, ElementId, Entity, EventEmitter, FocusHandle,
    Focusable, FontWeight, Hsla, Pixels, ScrollStrategy, SharedString, Stateful, Subscription,
    UniformListScrollHandle, Window, actions, div, prelude::*, px, uniform_list,
};
use introspect::{Catalog, Table, TableKind};
use results_grid::{GridData, GridState, grid};
use sql_editor::{Kind, Name, SqlEditor, Vocabulary};
use std::collections::HashSet;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicI32, Ordering};
use std::time::{Duration, Instant};
use storage::{HistoryFilter, NewRun, QueryRun, RunSource, SavedTab, SavedTabs, Store};
use theme::{FONT_FAMILY, ThemeColors, theme};
use ui::scrollbar::{self, DragState, Scrollbar};
use ui::{
    TextField, TextFieldEvent, card, format_count, format_millis, format_seconds, lock_glyph,
    meerkat_mark, play_glyph, section_label, status_dot, stop_glyph, table_glyph,
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
        PrevTab
    ]
);

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
/// How long a stop waits for the driver to say which backend the run is
/// on, and how often it looks. The id lands one round trip after the run
/// starts, so this is a wait for a race, not for a server.
const STOP_WAIT: Duration = Duration::from_secs(5);
const STOP_POLL: Duration = Duration::from_millis(20);
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
    /// True while a repaint loop is running for the query timers. Runs come
    /// and go in several tabs at once; the loop belongs to the window.
    timing: bool,
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
    elapsed: Option<u128>,
    loading: bool,
    error: Option<String>,
    selected: Option<usize>,
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
    /// How many statements the last run sent, so the result strip can say
    /// which set is on screen.
    statements_run: usize,
    elapsed: Option<u128>,
    error: Option<String>,
    /// Where the tab's last run got to. It drives the run button, the
    /// timer beside it and the result line, which is why all three agree.
    run: Run,
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
    /// The statement is out. ⌘. asks the server to give it up.
    Running(Live),
    /// A cancel has gone to the server and the statement has not come back
    /// yet. A second ⌘. terminates the backend instead of asking it
    /// nicely, which is the only reason this is a state of its own.
    Cancelling(Live),
    /// Stopped on the user's word, after this many milliseconds.
    Cancelled { elapsed: u128 },
}

/// A run in flight.
struct Live {
    /// When it started, for the timer in the toolbar. Wall clock is not
    /// wanted here: the timer measures a wait, not a time of day.
    started: Instant,
    /// The backend the statement is on, written by the tokio task as each
    /// statement starts and read by the UI when the user asks to stop.
    /// Zero means the driver has not said yet — one round trip's worth of
    /// window, which `stop_active_query` waits out rather than ignoring.
    ///
    /// An atomic rather than a channel because there is nothing to wake:
    /// whoever wants the id wants the latest one, and only then.
    backend: Arc<AtomicI32>,
}

impl Run {
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
            tabs: Vec::new(),
            active: 0,
            next_id: 1,
            store,
            store_error,
            scope,
            env,
            palette: None,
            timing: false,
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
                SavedTab::Query { title, statement, relation } => {
                    self.new_query_with(&statement, window, cx);
                    if let Some(Tab::Query(tab)) = self.tabs.last_mut() {
                        if !title.is_empty() {
                            tab.title = title.into();
                        }
                        tab.relation = relation;
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

    /// Put back a paged table tab. It is built here rather than through
    /// `open_table_in` because that one needs the catalog to name the
    /// table's kind, and the catalog may still be on its way — a tab the
    /// user had open is opened whether or not the shell can describe it
    /// yet.
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
            elapsed: None,
            loading: false,
            error: None,
            selected: None,
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
        // What ⇥ would take is read off the rows, so the hint is set here
        // rather than by each caller.
        self.update_filter_ghost(cx);
        cx.notify();
    }

    /// What ⇥ would finish the filter line with: the next part of the
    /// first relation the filter left, as the palette finishes a name.
    fn filter_completion(&self, cx: &App) -> Option<String> {
        let needle = self.catalog_filter.read(cx).trimmed();
        let first = self.catalog_rows.iter().find_map(|row| match row {
            CatalogRow::Relation { schema, name, .. } => {
                Some(vec![schema.to_string(), name.to_string()])
            }
            CatalogRow::Schema { .. } | CatalogRow::Section { .. } => None,
        })?;
        palette::complete_path(&first, needle)
    }

    /// Show what ⇥ would take, faint and after the caret — but only when
    /// it carries on from what was typed. A hit sits anywhere inside a
    /// name, so ⇥ often rewrites the line instead of extending it, and a
    /// hint that says otherwise lies.
    fn update_filter_ghost(&mut self, cx: &mut Context<Self>) {
        let needle = self.catalog_filter.read(cx).trimmed().to_string();
        let ghost = self
            .filter_completion(cx)
            .and_then(|completed| completed.strip_prefix(needle.as_str()).map(str::to_string))
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
            // Enter opens the first relation the filter left, which is what
            // a one-hit filter is for. Escape empties the line and hands the
            // keys back to the workspace.
            TextFieldEvent::Submit => {
                let first = self.catalog_rows.iter().find_map(|row| match row {
                    CatalogRow::Relation { schema, name, .. } => {
                        Some((schema.to_string(), name.to_string()))
                    }
                    CatalogRow::Schema { .. } | CatalogRow::Section { .. } => None,
                });
                if let Some((schema, table)) = first {
                    self.browse_table(&schema, &table, window, cx);
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

    /// Open a table, or focus the tab that already holds it. `new_tab` is
    /// the palette's ⌘⏎: give me another view of this one, so two pages of
    /// the same table can sit side by side.
    fn open_table_in(
        &mut self,
        schema: String,
        table: String,
        new_tab: bool,
        cx: &mut Context<Self>,
    ) {
        if !new_tab
            && let Some(ix) = self.tabs.iter().position(|tab| match tab {
                Tab::Table(t) => t.schema == schema && t.table == table,
                Tab::Query(_) | Tab::History(_) => false,
            })
        {
            self.activate(ix, cx);
            cx.notify();
            return;
        }

        let Some((kind, approx_rows)) = self
            .table_model(&schema, &table)
            .map(|model| (model.kind, model.approx_rows))
        else {
            return;
        };
        let id = self.take_id();
        self.tabs.push(Tab::Table(TableTab {
            id,
            schema,
            table,
            kind,
            data: empty_grid(),
            page: 0,
            approx_rows,
            elapsed: None,
            loading: false,
            error: None,
            selected: None,
            scroll: GridState::new(),
            generation: 0,
        }));
        self.activate(self.tabs.len() - 1, cx);
        self.load_page(id, 0, cx);
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
            tab.selected = None;
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
        tab.selected = None;
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
                        tab.elapsed = Some(elapsed);
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
            statements_run: 0,
            elapsed: None,
            error: None,
            run: Run::Idle,
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
        if tab.run.in_flight() {
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
        let backend = Arc::new(AtomicI32::new(0));
        tab.run = Run::Running(Live { started: Instant::now(), backend: backend.clone() });
        tab.error = None;
        tab.generation += 1;
        let generation = tab.generation;

        let recorded = sql;
        // The buffer has been edited since the tab was opened, and this is
        // the moment the user says it is worth something.
        self.remember_tabs(cx);
        let task = run_statements(connection, statements, backend, cx);
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
                let run = match flatten(outcome) {
                    Ok((result, elapsed, ran)) => {
                        tab.run = Run::Idle;
                        let rows = result.rows.len() as u64;
                        tab.elapsed = Some(elapsed);
                        tab.has_result = true;
                        tab.statements_run = ran;
                        tab.data = Rc::new(GridData::new(result.columns, result.rows));
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
                        tab.data = empty_grid();
                        Outcome { elapsed: None, rows: None, error: Some(error) }
                    }
                };
                this.record_run(&recorded, RunSource::User, &run);
                this.reload_open_history(cx);
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
        let (backend, started) = (live.backend.clone(), live.started);
        let (tab_id, generation) = (tab.id, tab.generation);
        tab.run = Run::Cancelling(Live { started, backend: backend.clone() });
        cx.notify();

        cx.spawn(async move |this, cx| {
            // The backend id lands one round trip after a run starts, so a
            // stop pressed the instant a run began can arrive first. Wait
            // it out on the background executor rather than dropping the
            // request — a stop that silently did nothing is the worst of
            // the three things this button can do.
            let mut waited = Duration::ZERO;
            let id = loop {
                let id = backend.load(Ordering::Relaxed);
                if id != 0 {
                    break Some(RunId(id));
                }
                if waited >= STOP_WAIT {
                    break None;
                }
                cx.background_executor().timer(STOP_POLL).await;
                waited += STOP_POLL;
            };
            let Some(id) = id else { return };
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
        let query = cx.new(|cx| {
            TextField::new("Search tables, columns and history…", cx)
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

    /// Show what ⇥ would finish the line with, faint and after the caret.
    ///
    /// Only when the completion carries on from what was typed. A hit
    /// sits anywhere in a name, so accepting one often rewrites the line
    /// instead of extending it — and a hint that says otherwise lies.
    fn update_ghost(&mut self, cx: &mut Context<Self>) {
        let Some(palette) = &self.palette else { return };
        let typed = palette.query.read(cx).text().to_string();
        let (_, needle) = palette::parse(&typed, palette.chip);
        let ghost = palette::completion(&palette.rows, palette.selected, needle)
            .and_then(|completed| completed.strip_prefix(needle).map(str::to_string))
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
            Pick::Table { schema, table } => self.open_table_in(schema, table, new_tab, cx),
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

    /// Close one tab and hand the focus to whatever takes its place. The
    /// active index counts tabs, not ids, so closing a tab to the left of
    /// the active one has to walk it back or the selection jumps.
    fn close_tab(&mut self, tab_id: u64, window: &mut Window, cx: &mut Context<Self>) {
        let Some(ix) = self.tabs.iter().position(|tab| tab.id() == tab_id) else { return };
        self.tabs.remove(ix);
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
        self.active = ix;
        self.remember_tabs(cx);
    }

    /// A query tab types into its editor, so it wants the focus itself;
    /// everything else leaves it on the shell, where the shell's own keys
    /// are bound.
    fn focus_active_tab(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        match self.tabs.get(self.active) {
            Some(Tab::Query(tab)) => window.focus(&tab.editor.focus_handle(cx), cx),
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

    // --- actions ---------------------------------------------------------

    fn on_run_query(&mut self, _: &RunQuery, _: &mut Window, cx: &mut Context<Self>) {
        self.run_active_query(cx);
    }

    fn on_stop_query(&mut self, _: &StopQuery, _: &mut Window, cx: &mut Context<Self>) {
        self.stop_active_query(cx);
    }

    fn on_new_query(&mut self, _: &NewQuery, window: &mut Window, cx: &mut Context<Self>) {
        self.new_query(window, cx);
    }

    fn on_close_tab(&mut self, _: &CloseTab, window: &mut Window, cx: &mut Context<Self>) {
        let Some(tab) = self.tabs.get(self.active) else { return };
        let id = tab.id();
        self.close_tab(id, window, cx);
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

    fn on_show_history(&mut self, _: &ShowHistory, _: &mut Window, cx: &mut Context<Self>) {
        self.open_history(cx);
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

    fn on_next_tab(&mut self, _: &NextTab, window: &mut Window, cx: &mut Context<Self>) {
        self.step_tab(true, window, cx);
    }

    fn on_prev_tab(&mut self, _: &PrevTab, window: &mut Window, cx: &mut Context<Self>) {
        self.step_tab(false, window, cx);
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

/// Run a buffer's statements in order and keep the last result that has
/// columns, so a trailing `create table` does not blank a grid the SELECT
/// before it filled. The first statement to fail stops the run and its
/// error is what the user sees.
#[allow(clippy::type_complexity)]
fn run_statements(
    connection: Arc<dyn Connection>,
    statements: Vec<String>,
    backend: Arc<AtomicI32>,
    cx: &mut Context<Shell>,
) -> gpui::Task<Result<anyhow::Result<(QueryResult, u128, usize)>, gpui_tokio::JoinError>> {
    // Each statement gets its own connection out of the pool, so each one
    // runs on its own backend. The slot holds whichever is current, which
    // is the one a stop has to reach; the one before it has already
    // finished, and stopping it would be stopping nothing.
    let report: ReportRun = Arc::new(move |id: RunId| {
        backend.store(id.0, Ordering::Relaxed);
    });
    gpui_tokio::Tokio::spawn(cx, async move {
        let started = Instant::now();
        let mut last = QueryResult::default();
        let mut ran = 0;
        for statement in &statements {
            let result = connection.execute_reporting(statement, report.clone()).await?;
            ran += 1;
            if !result.columns.is_empty() {
                last = result;
            }
        }
        anyhow::Ok((last, started.elapsed().as_millis(), ran))
    })
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
            .on_action(cx.listener(Self::on_new_query))
            .on_action(cx.listener(Self::on_close_tab))
            .on_action(cx.listener(Self::on_refresh))
            .on_action(cx.listener(Self::on_prev_page))
            .on_action(cx.listener(Self::on_next_page))
            .on_action(cx.listener(Self::on_show_history))
            .on_action(cx.listener(Self::on_toggle_palette))
            .on_action(cx.listener(Self::on_next_tab))
            .on_action(cx.listener(Self::on_prev_tab))
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
            .children(self.palette_overlay(&colors, cx))
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
                    .on_click(cx.listener(|this, _event, _window, cx| {
                        // The shell is dropped from here, and with it every
                        // editor buffer. Write the strip back while there
                        // is still something to read it from.
                        this.remember_tabs(cx);
                        cx.emit(ShellEvent::Close);
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
            // The way into the palette, where the comp puts it: beside the
            // ⌘⏎ badge, and clickable, because a badge that only tells you
            // about a shortcut is a badge that teaches nothing.
            .child(
                key_badge("⌘K", colors)
                    .id("open-palette")
                    .cursor_pointer()
                    .hover(|s| s.text_color(colors.accent).border_color(colors.text_faint))
                    .on_click(cx.listener(|this, _event, window, cx| {
                        this.toggle_palette(window, cx)
                    })),
            )
            .child(key_badge("⌘⏎", colors))
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
        let shell = cx.entity().downgrade();

        let mut list = uniform_list(
            "catalog",
            rows.len(),
            move |range, _window, cx| {
                let colors = theme(cx).colors.clone();
                range
                    .map(|ix| {
                        catalog_row(ix, &rows[ix], active.as_ref(), &shell, &colors, cx)
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
                    if let Some(Tab::Query(tab)) = this.tabs.get(ix) {
                        window.focus(&tab.editor.focus_handle(cx), cx);
                    }
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
                            this.close_tab(id, window, cx);
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
                .child(grid(
                    format!("tab-{}", tab.id),
                    tab.data.clone(),
                    &tab.scroll,
                    tab.selected,
                    Some(self.row_click_handler(tab.id, cx)),
                    cx,
                )),
            Some(Tab::Query(tab)) => self.query_pane(pane, tab, colors, cx),
            Some(Tab::History(tab)) => self.history_pane(pane, tab, colors, cx),
            None => pane.child(self.placeholder(colors, window)),
        }
    }

    /// Selecting a row is view state, so update it straight on the entity.
    fn row_click_handler(&self, tab_id: u64, cx: &Context<Self>) -> results_grid::OnClickRow {
        let this = cx.entity().downgrade();
        Rc::new(move |ix, _window, cx| {
            this.update(cx, |this: &mut Shell, cx| {
                if let Some(Tab::Table(tab)) = this.tab_mut(tab_id) {
                    tab.selected = Some(ix);
                    cx.notify();
                }
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

    /// One button for all three verbs, as the comp draws it: **run** filled
    /// in the accent, **stop** outlined in clay over paper, **terminate**
    /// filled in clay. The escalation is the point — the button that ends a
    /// backend must not look like the button that starts a query.
    fn run_button(
        &self,
        tab: &QueryTab,
        colors: &ThemeColors,
        cx: &Context<Self>,
    ) -> Stateful<Div> {
        let (verb, keys) = match tab.run {
            Run::Idle | Run::Cancelled { .. } => ("run", "⌘⏎"),
            Run::Running(_) => ("stop", "⌘."),
            Run::Cancelling(_) => ("terminate", "⌘."),
        };
        // Paper under clay for "stop": the one state where the button is
        // outlined rather than filled, so a run in flight reads as a
        // question rather than as a command already given.
        let (border, fill, ink, cap_surface, cap_border) = match tab.run {
            Run::Idle | Run::Cancelled { .. } => (
                colors.accent,
                colors.accent,
                colors.window,
                colors.key_on_fill_surface,
                colors.key_on_fill_border,
            ),
            Run::Running(_) => (
                colors.env_prod,
                colors.window,
                colors.env_prod_text,
                colors.env_prod_surface,
                colors.env_prod_inner,
            ),
            Run::Cancelling(_) => (
                colors.env_prod,
                colors.env_prod,
                colors.window,
                colors.key_on_fill_surface,
                colors.key_on_fill_border,
            ),
        };

        let mut button = div()
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
            .on_click(cx.listener(|this, _event, _window, cx| this.toggle_run(cx)));
        button = if tab.run.in_flight() {
            button.child(stop_glyph(ink))
        } else {
            button.child(play_glyph(ink))
        };
        button
            .child(
                div()
                    .text_size(px(11.))
                    .font_weight(FontWeight::MEDIUM)
                    .text_color(ink)
                    .child(verb),
            )
            .child(
                // The keycap the comp puts inside the button, with the
                // 2px bottom edge that makes it read as a key.
                div()
                    .px(px(6.))
                    .py(px(4.))
                    .border_1()
                    .border_b_2()
                    .border_color(cap_border)
                    .rounded(px(5.))
                    .bg(cap_surface)
                    .text_size(px(11.))
                    .font_weight(FontWeight::MEDIUM)
                    .text_color(ink)
                    .child(keys),
            )
    }

    /// The button does whichever of the two things its label says.
    fn toggle_run(&mut self, cx: &mut Context<Self>) {
        match self.tabs.get(self.active) {
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
            // No streaming yet — `execute` collects the whole result — so
            // there is no row count to report while a run is out. The line
            // says what it can: the run is alive, and how to end it.
            Run::Running(_) => (
                "RUNNING",
                "the server has the statement · ⌘. stops it".to_string(),
                colors.accent_deep,
            ),
            Run::Cancelling(live) => (
                "CANCELLING",
                match live.backend.load(Ordering::Relaxed) {
                    0 => "cancel sent · waiting for the server to acknowledge".to_string(),
                    pid => format!(
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
                        tab.elapsed.map(format_millis).unwrap_or_default()
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
                    div()
                        .text_size(px(12.))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.text)
                        .child(tab.title.clone()),
                )
                .child(
                    div()
                        .text_size(px(11.))
                        .text_color(colors.text_muted)
                        .child(format!("{} · {}", self.session_name(), self.mode_word())),
                )
                // With several statements in the buffer, say which one a
                // run would send, so ⌘⏎ never comes as a surprise.
                .children(run_scope(tab, cx).map(|scope| {
                    div().text_size(px(11.)).text_color(colors.text_faint).child(scope)
                }))
                .child(div().flex_1())
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
                .child(summary),
        )
        .child(grid(
            format!("tab-{}", tab.id),
            tab.data.clone(),
            &tab.scroll,
            None,
            None,
            cx,
        ))
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
        let (range, elapsed) = match self.tabs.get(self.active) {
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
                (range, tab.elapsed)
            }
            Some(Tab::Query(tab)) => (
                if tab.has_result {
                    format!("{} rows", tab.data.rows.len())
                } else {
                    String::new()
                },
                tab.elapsed,
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
            .child(div().flex_1())
            .children(
                elapsed.map(|ms| div().child(format!("queried in {}", format_millis(ms)))),
            )
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
            (true, _) => "type to search tables, columns and history".to_string(),
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
/// rule the palette uses: a case-insensitive substring, and **a dot names
/// a path**. So `address` finds every `address`, `dev.addr` finds the one
/// in `sample_dev_sample`, and a bare schema name answers with everything
/// under it. Whatever is left with nothing under it is dropped, header and
/// all. While the filter is on, everything that survived is drawn open
/// whatever the two sets say — a search that needs a second click to show
/// its hits is not a search.
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
                    section
                        .relations
                        .iter()
                        .filter(|name| {
                            let path = [group.schema.to_string(), name.to_string()];
                            palette::path_matches(&path, needle)
                        })
                        .collect()
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
                    TableKind::Table => table_glyph(is_active, cx),
                    TableKind::View => div()
                        .size(px(5.))
                        .rounded_full()
                        .border_1()
                        .border_color(if is_active { colors.accent } else { colors.text_faint }),
                })
                .child(
                    div()
                        .flex_1()
                        .min_w(px(0.))
                        .text_size(px(12.))
                        .font_weight(if is_active {
                            FontWeight::MEDIUM
                        } else {
                            FontWeight::NORMAL
                        })
                        .text_color(if is_active { colors.text } else { colors.text_secondary })
                        .truncate()
                        .child(name.clone()),
                );

            if is_active {
                item.bg(colors.selection).into_any_element()
            } else {
                let hover = colors.hairline;
                item.hover(move |s| s.bg(hover)).into_any_element()
            }
        }
    }
}

/// A keystroke, in the bordered pill the comp puts one in.
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

    fn relation(name: &str, kind: TableKind) -> Table {
        Table { name: name.to_string(), kind, columns: Vec::new(), primary_key: Vec::new(), approx_rows: None }
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
