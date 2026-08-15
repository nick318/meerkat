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

use db_client::{Connection, Profile, QueryResult};
use db_postgres::{Label, PostgresConnection};
use gpui::{
    AnyElement, App, Context, Div, ElementId, Entity, EventEmitter, FocusHandle, Focusable,
    FontWeight, SharedString, Stateful, UniformListScrollHandle, Window, actions, div, prelude::*,
    px, uniform_list,
};
use introspect::{Catalog, Table, TableKind};
use results_grid::{GridData, GridState, grid};
use sql_editor::{Kind, Name, SqlEditor, Vocabulary};
use std::rc::Rc;
use std::sync::Arc;
use std::time::Instant;
use storage::{HistoryFilter, NewRun, RunSource, Store};
use theme::{FONT_FAMILY, ThemeColors, theme};
use ui::{accent_button, card, format_count, format_millis, section_label, status_dot, table_glyph};

use crate::connections::unix_now;
use crate::history::{self, HistoryRow, OnOpenRun};
use crate::sql::{PAGE_SIZE, page_query};

actions!(meerkat, [RunQuery, NewQuery, Refresh, PrevPage, NextPage, ShowHistory]);

const SIDEBAR_WIDTH: f32 = 246.;
const EDITOR_HEIGHT: f32 = 250.;
/// Every sidebar row is this tall, headers included: `uniform_list` needs
/// one height to measure, and the design's rows are already within a
/// pixel of each other.
const CATALOG_ROW_HEIGHT: f32 = 24.;
/// How far back the history screen looks, in days. The comp's header says
/// "last 7 days", and that is the window the list actually reads.
const HISTORY_DAYS: i64 = 7;

pub struct Shell {
    focus_handle: FocusHandle,
    status: Status,
    connection: Option<Arc<dyn Connection>>,
    catalog: Option<Catalog>,
    /// The sidebar's rows, flattened once when the catalog lands.
    catalog_rows: Rc<Vec<CatalogRow>>,
    /// Where the sidebar is scrolled, kept across re-renders.
    catalog_scroll: UniformListScrollHandle,
    /// How many relations the catalog holds, for the sidebar footer.
    relation_total: usize,
    /// Every name in the catalog, for the editor's colouring.
    vocabulary: Arc<Vocabulary>,
    label: Option<Label>,
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
    editor: Entity<SqlEditor>,
    data: Rc<GridData>,
    has_result: bool,
    /// How many statements the last run sent, so the result strip can say
    /// which set is on screen.
    statements_run: usize,
    elapsed: Option<u128>,
    error: Option<String>,
    running: bool,
    scroll: GridState,
    generation: u64,
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
    pub fn new(target: Target, cx: &mut Context<Self>) -> Self {
        let (store, store_error) = match Store::open_default() {
            Ok(store) => (Some(store), None),
            Err(error) => (None, Some(error.to_string())),
        };
        let mut shell = Self {
            focus_handle: cx.focus_handle(),
            status: Status::Connecting(describe(&target)),
            connection: None,
            catalog: None,
            catalog_rows: Rc::new(Vec::new()),
            catalog_scroll: UniformListScrollHandle::new(),
            relation_total: 0,
            vocabulary: Arc::new(Vocabulary::default()),
            label: None,
            tabs: Vec::new(),
            active: 0,
            next_id: 1,
            store,
            store_error,
            scope: scope_of(&target),
        };
        shell.connect(target, cx);
        shell
    }

    fn connect(&mut self, target: Target, cx: &mut Context<Self>) {
        let task = gpui_tokio::Tokio::spawn(cx, async move {
            let connection = match &target {
                Target::Url(url) => PostgresConnection::connect(url).await?,
                // A saved connection takes its password from the OS
                // keychain, so nothing here carries one.
                Target::Profile(profile) => PostgresConnection::connect_profile(profile).await?,
            };
            let label = connection.label().clone();
            let catalog = connection.introspect().await?;
            anyhow::Ok((Arc::new(connection) as Arc<dyn Connection>, label, catalog))
        });

        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                match flatten(outcome) {
                    Ok((connection, label, catalog)) => {
                        this.connection = Some(connection);
                        this.label = Some(label);
                        this.vocabulary = Arc::new(vocabulary_of(&catalog));
                        this.catalog_rows = catalog_rows(&catalog);
                        this.relation_total =
                            catalog.schemas.iter().map(|schema| schema.tables.len()).sum();
                        this.catalog = Some(catalog);
                        this.status = Status::Connected;
                        // A query tab opened while connecting was built
                        // with an empty vocabulary; give it the real one.
                        let vocabulary = this.vocabulary.clone();
                        for tab in &this.tabs {
                            if let Tab::Query(tab) = tab {
                                tab.editor.update(cx, |editor, cx| {
                                    editor.set_vocabulary(vocabulary.clone(), cx)
                                });
                            }
                        }
                        this.open_first_table(cx);
                    }
                    Err(error) => this.status = Status::Failed(error),
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
    }

    fn open_first_table(&mut self, cx: &mut Context<Self>) {
        let Some(catalog) = &self.catalog else { return };
        let Some((schema, table)) = catalog
            .schemas
            .iter()
            .flat_map(|schema| schema.tables.first().map(|t| (schema.name.clone(), t.name.clone())))
            .next()
        else {
            return;
        };
        self.open_table(schema, table, cx);
    }

    fn open_table(&mut self, schema: String, table: String, cx: &mut Context<Self>) {
        if let Some(ix) = self.tabs.iter().position(|tab| match tab {
            Tab::Table(t) => t.schema == schema && t.table == table,
            Tab::Query(_) | Tab::History(_) => false,
        }) {
            self.active = ix;
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
        self.active = self.tabs.len() - 1;
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
        let Some(connection) = self.connection.clone() else { return };
        let Some(Tab::Table(tab)) = self.tab_mut(tab_id) else { return };

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
        self.new_query_with("select 1 as x, now() as t", window, cx);
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
            editor,
            data: empty_grid(),
            has_result: false,
            statements_run: 0,
            elapsed: None,
            error: None,
            running: false,
            scroll: GridState::new(),
            generation: 0,
        }));
        self.active = self.tabs.len() - 1;
        cx.notify();
    }

    fn run_active_query(&mut self, cx: &mut Context<Self>) {
        let Some(connection) = self.connection.clone() else { return };
        let Some(Tab::Query(tab)) = self.tabs.get_mut(self.active) else { return };

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
        tab.running = true;
        tab.error = None;
        tab.generation += 1;
        let generation = tab.generation;

        let recorded = sql;
        let task = run_statements(connection, statements, cx);
        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                let Some(Tab::Query(tab)) = this.tab_mut(tab_id) else { return };
                if tab.generation != generation {
                    return;
                }
                tab.running = false;
                let run = match flatten(outcome) {
                    Ok((result, elapsed, ran)) => {
                        let rows = result.rows.len() as u64;
                        tab.elapsed = Some(elapsed);
                        tab.has_result = true;
                        tab.statements_run = ran;
                        tab.data = Rc::new(GridData::new(result.columns, result.rows));
                        Outcome { elapsed: Some(elapsed), rows: Some(rows), error: None }
                    }
                    Err(error) => {
                        tab.error = Some(error.clone());
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
            self.active = ix;
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
        self.active = self.tabs.len() - 1;
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

    fn close_tab(&mut self, tab_id: u64, cx: &mut Context<Self>) {
        let Some(ix) = self.tabs.iter().position(|tab| tab.id() == tab_id) else { return };
        self.tabs.remove(ix);
        self.active = self.active.min(self.tabs.len().saturating_sub(1));
        cx.notify();
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

    fn on_new_query(&mut self, _: &NewQuery, window: &mut Window, cx: &mut Context<Self>) {
        self.new_query(window, cx);
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
    cx: &mut Context<Shell>,
) -> gpui::Task<Result<anyhow::Result<(QueryResult, u128, usize)>, gpui_tokio::JoinError>> {
    gpui_tokio::Tokio::spawn(cx, async move {
        let started = Instant::now();
        let mut last = QueryResult::default();
        let mut ran = 0;
        for statement in &statements {
            let result = connection.execute(statement).await?;
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

/// What the connecting line says before there is a connection to describe.
fn describe(target: &Target) -> String {
    match target {
        Target::Url(url) => redact(url),
        Target::Profile(profile) => profile.name.clone(),
    }
}

impl Render for Shell {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();

        div()
            .key_context("Shell")
            .track_focus(&self.focus_handle(cx))
            .on_action(cx.listener(Self::on_run_query))
            .on_action(cx.listener(Self::on_new_query))
            .on_action(cx.listener(Self::on_refresh))
            .on_action(cx.listener(Self::on_prev_page))
            .on_action(cx.listener(Self::on_next_page))
            .on_action(cx.listener(Self::on_show_history))
            .flex()
            .flex_col()
            .size_full()
            .bg(colors.window)
            .font_family(FONT_FAMILY)
            .text_color(colors.text_body)
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
            )
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
                    .child(self.database_name()),
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

        div()
            .h(px(38.))
            .flex_none()
            .flex()
            .items_center()
            .px(px(12.))
            .gap(px(14.))
            .border_b_1()
            .border_color(colors.border)
            .bg(colors.panel)
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
                    .on_click(cx.listener(|_this, _event, _window, cx| {
                        cx.emit(ShellEvent::Close);
                    }))
                    .child("‹ connections"),
            )
            .child(trail)
            .child(
                div()
                    .px(px(6.))
                    .py(px(4.))
                    .border_1()
                    .border_color(colors.border_strong)
                    .rounded(px(4.))
                    .bg(colors.window)
                    .text_size(px(10.))
                    .font_weight(FontWeight::MEDIUM)
                    .text_color(colors.text_muted)
                    .child("⌘⏎"),
            )
    }

    fn database_name(&self) -> SharedString {
        match &self.label {
            Some(label) if !label.database.is_empty() => label.database.clone().into(),
            _ => "meerkat".into(),
        }
    }

    fn sidebar(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let (host, dot) = match (&self.label, &self.status) {
            (Some(label), Status::Connected) => (
                format!("{} · {}", label.host, label.port),
                colors.ok,
            ),
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
                                        .child(self.database_name()),
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
                            .child(div().text_color(colors.text_faint).child("read-only")),
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
                .child(match &self.status {
                    Status::Failed(_) => "no catalog",
                    Status::Connected => "no relations",
                    Status::Connecting(_) => "reading the catalog…",
                })
                .into_any_element();
        }

        let active = match self.tabs.get(self.active) {
            Some(Tab::Table(tab)) => {
                Some((SharedString::from(tab.schema.clone()), SharedString::from(tab.table.clone())))
            }
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
        list.track_scroll(&self.catalog_scroll)
            .flex_1()
            .min_h(px(0.))
            .px(px(8.))
            .into_any_element()
    }

    fn tab_strip(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let mut strip = div()
            .h(px(34.))
            .flex_none()
            .flex()
            .items_stretch()
            .border_b_1()
            .border_color(colors.border)
            .bg(colors.panel);

        for (ix, tab) in self.tabs.iter().enumerate() {
            let is_active = ix == self.active;
            let id = tab.id();
            let mut item = div()
                .id(ElementId::Name(format!("tab-{id}").into()))
                .flex()
                .items_center()
                .gap(px(8.))
                .px(px(14.))
                .border_r_1()
                .border_color(colors.border)
                .cursor_pointer()
                .on_click(cx.listener(move |this, _event, window, cx| {
                    this.active = ix;
                    if let Some(Tab::Query(tab)) = this.tabs.get(ix) {
                        window.focus(&tab.editor.focus_handle(cx), cx);
                    }
                    cx.notify();
                }))
                .child(match tab {
                    Tab::Table(tab) if tab.kind == TableKind::Table => table_glyph(is_active, cx),
                    // Views and query results are both "not a table": the
                    // sidebar marks them with a ring, so tabs match.
                    _ => div().size(px(5.)).rounded_full().border_1().border_color(
                        if is_active { colors.accent } else { colors.text_faint },
                    ),
                })
                .child(
                    div()
                        .text_size(px(11.))
                        .font_weight(if is_active { FontWeight::MEDIUM } else { FontWeight::NORMAL })
                        .text_color(if is_active { colors.text } else { colors.text_muted })
                        .child(tab.title()),
                )
                .child(
                    div()
                        .id(ElementId::Name(format!("close-{id}").into()))
                        .text_size(px(12.))
                        .text_color(colors.text_faint)
                        .cursor_pointer()
                        .hover(|s| s.text_color(colors.error))
                        .on_click(cx.listener(move |this, _event, _window, cx| {
                            this.close_tab(id, cx);
                        }))
                        .child("×"),
                );
            item = if is_active {
                item.bg(colors.window)
            } else {
                let hover = colors.hairline;
                item.hover(move |s| s.bg(hover))
            };
            strip = strip.child(item);
        }

        strip.child(div().flex_1()).child(
            div()
                .id("new-query")
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

    fn query_pane(
        &self,
        pane: Div,
        tab: &QueryTab,
        colors: &ThemeColors,
        cx: &Context<Self>,
    ) -> Div {
        let summary = if tab.running {
            "running…".to_string()
        } else if tab.has_result {
            let statements = match tab.statements_run {
                0 | 1 => String::new(),
                n => format!("{n} statements · last result · "),
            };
            format!(
                "{statements}{} rows · {} columns · {}",
                tab.data.rows.len(),
                tab.data.columns.len(),
                tab.elapsed.map(format_millis).unwrap_or_default()
            )
        } else {
            "not run yet".to_string()
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
                        .child(format!("{} · read-only", self.database_name())),
                )
                // With several statements in the buffer, say which one a
                // run would send, so ⌘⏎ never comes as a surprise.
                .children(run_scope(tab, cx).map(|scope| {
                    div().text_size(px(11.)).text_color(colors.text_faint).child(scope)
                }))
                .child(div().flex_1())
                .child(
                    accent_button("run ⌘⏎", cx)
                        .id("run-query")
                        .on_click(cx.listener(|this, _event, _window, cx| {
                            this.run_active_query(cx)
                        })),
                ),
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
                .text_color(colors.text_muted)
                .child(
                    div()
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.text)
                        .child("RESULT"),
                )
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
                        .child(format!("{} · last {HISTORY_DAYS} days", self.database_name())),
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
    Header { label: SharedString },
    Relation { schema: SharedString, name: SharedString, kind: TableKind },
}

/// Flatten the catalog the way the sidebar reads it: each schema's tables
/// under a `SCHEMA ·` header, then its views under a `VIEWS ·` one. Built
/// once per catalog, not once per frame.
fn catalog_rows(catalog: &Catalog) -> Rc<Vec<CatalogRow>> {
    let mut rows = Vec::new();
    for schema in &catalog.schemas {
        for (kind, label) in [(TableKind::Table, "SCHEMA"), (TableKind::View, "VIEWS")] {
            let mut relations = schema.tables.iter().filter(|table| table.kind == kind).peekable();
            if relations.peek().is_none() {
                continue;
            }
            rows.push(CatalogRow::Header {
                label: format!("{label} · {}", schema.name.to_ascii_uppercase()).into(),
            });
            rows.extend(relations.map(|table| CatalogRow::Relation {
                schema: schema.name.clone().into(),
                name: table.name.clone().into(),
                kind,
            }));
        }
    }
    Rc::new(rows)
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
        CatalogRow::Header { label } => div()
            .h(px(CATALOG_ROW_HEIGHT))
            .flex()
            .items_end()
            .px(px(6.))
            .pb(px(5.))
            .child(section_label(label.clone(), cx))
            .into_any_element(),
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
                .px(px(8.))
                .rounded(px(5.))
                .cursor_pointer()
                .on_click(move |_event, _window, cx| {
                    shell
                        .update(cx, |shell, cx| {
                            shell.open_table(schema_name.clone(), table_name.clone(), cx)
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
    fn the_sidebar_flattens_tables_then_views_per_schema() {
        let catalog = Catalog {
            schemas: vec![
                Schema {
                    name: "public".to_string(),
                    tables: vec![
                        relation("users", TableKind::Table),
                        relation("active_users", TableKind::View),
                        relation("orders", TableKind::Table),
                    ],
                },
                // A schema of views only gets one header, not an empty
                // `SCHEMA ·` one above it.
                Schema {
                    name: "reporting".to_string(),
                    tables: vec![relation("daily", TableKind::View)],
                },
            ],
        };

        let rows = catalog_rows(&catalog);
        let read: Vec<String> = rows
            .iter()
            .map(|row| match row {
                CatalogRow::Header { label } => format!("[{label}]"),
                CatalogRow::Relation { name, .. } => name.to_string(),
            })
            .collect();
        assert_eq!(
            read,
            [
                "[SCHEMA · PUBLIC]",
                "users",
                "orders",
                "[VIEWS · PUBLIC]",
                "active_users",
                "[VIEWS · REPORTING]",
                "daily",
            ]
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
