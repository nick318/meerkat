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

use db_client::{Connection, QueryResult};
use db_postgres::{Label, PostgresConnection};
use gpui::{
    App, Context, Div, ElementId, Entity, FocusHandle, Focusable, FontWeight, SharedString,
    Stateful, Window, actions, div, prelude::*, px,
};
use introspect::{Catalog, Table, TableKind};
use results_grid::{GridData, grid};
use sql_editor::SqlEditor;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Instant;
use theme::{FONT_FAMILY, ThemeColors, theme};
use ui::{accent_button, card, format_count, format_millis, section_label, status_dot, table_glyph};

use crate::sql::{PAGE_SIZE, page_query};

actions!(meerkat, [RunQuery, NewQuery, Refresh, PrevPage, NextPage]);

const SIDEBAR_WIDTH: f32 = 246.;
const EDITOR_HEIGHT: f32 = 250.;

pub struct Shell {
    focus_handle: FocusHandle,
    status: Status,
    connection: Option<Arc<dyn Connection>>,
    catalog: Option<Catalog>,
    label: Option<Label>,
    tabs: Vec<Tab>,
    active: usize,
    next_id: u64,
}

enum Status {
    Connecting(String),
    Connected,
    Failed(String),
}

enum Tab {
    Table(TableTab),
    Query(QueryTab),
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
    generation: u64,
}

struct QueryTab {
    id: u64,
    title: SharedString,
    editor: Entity<SqlEditor>,
    data: Rc<GridData>,
    has_result: bool,
    elapsed: Option<u128>,
    error: Option<String>,
    running: bool,
    generation: u64,
}

impl Tab {
    fn id(&self) -> u64 {
        match self {
            Tab::Table(tab) => tab.id,
            Tab::Query(tab) => tab.id,
        }
    }

    fn title(&self) -> SharedString {
        match self {
            Tab::Table(tab) => format!("{}.{}", tab.schema, tab.table).into(),
            Tab::Query(tab) => tab.title.clone(),
        }
    }
}

fn empty_grid() -> Rc<GridData> {
    Rc::new(GridData { columns: Vec::new(), rows: Vec::new() })
}

impl Shell {
    /// Open the shell and start connecting. The window paints the
    /// connecting state immediately; the connection lands later.
    pub fn new(url: String, cx: &mut Context<Self>) -> Self {
        let mut shell = Self {
            focus_handle: cx.focus_handle(),
            status: Status::Connecting(url.clone()),
            connection: None,
            catalog: None,
            label: None,
            tabs: Vec::new(),
            active: 0,
            next_id: 1,
        };
        shell.connect(url, cx);
        shell
    }

    fn connect(&mut self, url: String, cx: &mut Context<Self>) {
        let task = gpui_tokio::Tokio::spawn(cx, async move {
            let connection = PostgresConnection::connect(&url).await?;
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
                        this.catalog = Some(catalog);
                        this.status = Status::Connected;
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
            Tab::Query(_) => false,
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

        let task = run_sql(connection, sql, cx);
        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                let Some(Tab::Table(tab)) = this.tab_mut(tab_id) else { return };
                if tab.generation != generation {
                    return;
                }
                tab.loading = false;
                match flatten(outcome) {
                    Ok((result, elapsed)) => {
                        tab.elapsed = Some(elapsed);
                        tab.data = Rc::new(GridData {
                            columns: result.columns,
                            rows: result.rows,
                        });
                    }
                    Err(error) => {
                        tab.error = Some(error);
                        tab.data = empty_grid();
                    }
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
    }

    fn new_query(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let id = self.take_id();
        let editor = cx.new(|cx| SqlEditor::new("select 1 as x, now() as t", cx));
        window.focus(&editor.focus_handle(cx), cx);
        self.tabs.push(Tab::Query(QueryTab {
            id,
            title: format!("query {id}").into(),
            editor,
            data: empty_grid(),
            has_result: false,
            elapsed: None,
            error: None,
            running: false,
            generation: 0,
        }));
        self.active = self.tabs.len() - 1;
        cx.notify();
    }

    fn run_active_query(&mut self, cx: &mut Context<Self>) {
        let Some(connection) = self.connection.clone() else { return };
        let Some(Tab::Query(tab)) = self.tabs.get_mut(self.active) else { return };

        let sql = tab.editor.read(cx).text().trim().to_string();
        if sql.is_empty() {
            return;
        }
        let tab_id = tab.id;
        tab.running = true;
        tab.error = None;
        tab.generation += 1;
        let generation = tab.generation;

        let task = run_sql(connection, sql, cx);
        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                let Some(Tab::Query(tab)) = this.tab_mut(tab_id) else { return };
                if tab.generation != generation {
                    return;
                }
                tab.running = false;
                match flatten(outcome) {
                    Ok((result, elapsed)) => {
                        tab.elapsed = Some(elapsed);
                        tab.has_result = true;
                        tab.data = Rc::new(GridData {
                            columns: result.columns,
                            rows: result.rows,
                        });
                    }
                    Err(error) => {
                        tab.error = Some(error);
                        tab.has_result = false;
                        tab.data = empty_grid();
                    }
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
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
            .flex()
            .flex_col()
            .size_full()
            .bg(colors.window)
            .font_family(FONT_FAMILY)
            .text_color(colors.text_body)
            .child(self.breadcrumb_bar(&colors))
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
    fn breadcrumb_bar(&self, colors: &ThemeColors) -> Div {
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
        if let Some(Tab::Table(tab)) = self.tabs.get(self.active) {
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
                    .justify_between()
                    .text_size(px(10.))
                    .text_color(colors.text_muted)
                    .child(self.table_total())
                    .child(div().text_color(colors.text_faint).child("read-only")),
            )
    }

    fn table_total(&self) -> SharedString {
        let count: usize = self
            .catalog
            .iter()
            .flat_map(|catalog| catalog.schemas.iter())
            .map(|schema| schema.tables.len())
            .sum();
        match count {
            0 => "no tables".into(),
            1 => "1 relation".into(),
            n => format!("{n} relations").into(),
        }
    }

    fn catalog_list(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Stateful<Div> {
        let mut list = div()
            .id("catalog")
            .flex_1()
            .min_h(px(0.))
            .overflow_y_scroll()
            .px(px(8.))
            .pb(px(12.))
            .flex()
            .flex_col();

        let active = match self.tabs.get(self.active) {
            Some(Tab::Table(tab)) => Some((tab.schema.clone(), tab.table.clone())),
            _ => None,
        };

        let Some(catalog) = &self.catalog else {
            return list.child(
                div()
                    .pt(px(10.))
                    .px(px(6.))
                    .text_size(px(11.))
                    .text_color(colors.text_faint)
                    .child(match &self.status {
                        Status::Failed(_) => "no catalog",
                        _ => "reading the catalog…",
                    }),
            );
        };

        for schema in &catalog.schemas {
            let tables: Vec<&Table> =
                schema.tables.iter().filter(|t| t.kind == TableKind::Table).collect();
            let views: Vec<&Table> =
                schema.tables.iter().filter(|t| t.kind == TableKind::View).collect();

            if !tables.is_empty() {
                list = list.child(list_header(
                    format!("SCHEMA · {}", schema.name.to_ascii_uppercase()),
                    tables.len(),
                    colors,
                    cx,
                ));
                for table in tables {
                    list = list.child(self.catalog_item(
                        &schema.name,
                        table,
                        active.as_ref(),
                        colors,
                        cx,
                    ));
                }
            }
            if !views.is_empty() {
                list = list.child(list_header(
                    format!("VIEWS · {}", schema.name.to_ascii_uppercase()),
                    views.len(),
                    colors,
                    cx,
                ));
                for view in views {
                    list = list.child(self.catalog_item(
                        &schema.name,
                        view,
                        active.as_ref(),
                        colors,
                        cx,
                    ));
                }
            }
        }
        list
    }

    fn catalog_item(
        &self,
        schema: &str,
        table: &Table,
        active: Option<&(String, String)>,
        colors: &ThemeColors,
        cx: &mut Context<Self>,
    ) -> Stateful<Div> {
        let is_active =
            active.is_some_and(|(s, t)| s == schema && t.as_str() == table.name.as_str());
        let count = table.approx_rows.map(format_count).unwrap_or_default();
        let (schema_name, table_name) = (schema.to_string(), table.name.clone());

        let item = div()
            .id(ElementId::Name(format!("relation-{schema}-{}", table.name).into()))
            .flex()
            .items_center()
            .gap(px(8.))
            .px(px(8.))
            .py(px(5.))
            .rounded(px(5.))
            .cursor_pointer()
            .on_click(cx.listener(move |this, _event, _window, cx| {
                this.open_table(schema_name.clone(), table_name.clone(), cx);
            }))
            .child(match table.kind {
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
                    .font_weight(if is_active { FontWeight::MEDIUM } else { FontWeight::NORMAL })
                    .text_color(if is_active { colors.text } else { colors.text_secondary })
                    .truncate()
                    .child(table.name.clone()),
            )
            .child(div().text_size(px(10.)).text_color(colors.text_faint).child(count));

        if is_active {
            item.bg(colors.selection)
        } else {
            let hover = colors.hairline;
            item.hover(move |s| s.bg(hover))
        }
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
                    tab.selected,
                    Some(self.row_click_handler(tab.id, cx)),
                    cx,
                )),
            Some(Tab::Query(tab)) => self.query_pane(pane, tab, colors, cx),
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
            format!(
                "{} rows · {} columns · {}",
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
        .child(grid(format!("tab-{}", tab.id), tab.data.clone(), None, None, cx))
    }

    fn placeholder(&self, colors: &ThemeColors, _window: &mut Window) -> Div {
        let (heading, detail) = match &self.status {
            Status::Connecting(url) => ("connecting".to_string(), redact(url)),
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

fn list_header(
    label: String,
    count: usize,
    colors: &ThemeColors,
    cx: &App,
) -> Div {
    div()
        .flex()
        .justify_between()
        .items_center()
        .px(px(6.))
        .pt(px(8.))
        .pb(px(6.))
        .child(section_label(label, cx))
        .child(
            div()
                .text_size(px(10.))
                .text_color(colors.text_faint)
                .child(count.to_string()),
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
