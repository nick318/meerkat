//! The connections screen: what the app opens on when it is not pointed
//! at a database.
//!
//! It lists the saved connections, probes each one so the list says what
//! is actually reachable right now, and takes a new `postgres://` URL.
//! Profiles live in the local SQLite store; passwords go to the OS
//! keychain and never touch that file.
//!
//! Probing is the reason a row can say "14 tables · 3 views · PG 16.2":
//! each row connects, introspects and disconnects on the tokio runtime,
//! and the counts are cached in the store so the next launch paints them
//! before the servers answer.

use chrono::Local;
use db_client::{Connection, Engine, Profile};
use db_postgres::PostgresConnection;
use gpui::{
    App, Context, Div, ElementId, Entity, EventEmitter, FocusHandle, Focusable, FontWeight,
    SharedString, Stateful, Subscription, Window, div, prelude::*, px,
};
use introspect::{Catalog, TableKind};
use std::time::{SystemTime, UNIX_EPOCH};
use storage::{SavedConnection, Store};
use theme::{FONT_FAMILY, ThemeColors, theme};
use ui::{TextField, TextFieldEvent, accent_button, meerkat_mark, section_label, status_dot};

/// The comp's reading column: the list never stretches over a wide window.
const COLUMN_WIDTH: f32 = 900.;
const STATS_WIDTH: f32 = 150.;
const MODE_WIDTH: f32 = 130.;
const ACTION_WIDTH: f32 = 76.;

pub enum ConnectionsEvent {
    /// The user picked a connection: open the workspace on it.
    Open(Profile),
}

pub struct Connections {
    focus_handle: FocusHandle,
    /// `None` when the profiles file could not be opened; the screen then
    /// says so instead of pretending there are no connections.
    store: Option<Store>,
    error: Option<String>,
    rows: Vec<Row>,
    form: Option<Form>,
    /// Unix seconds when the screen opened, so "3d ago" stays put while
    /// the user reads it.
    now: i64,
}

struct Row {
    saved: SavedConnection,
    state: Probe,
}

/// What the last probe of one connection found.
enum Probe {
    Probing,
    Ready { tables: u32, views: u32, server: String },
    Failed(String),
}

/// The "+ new connection" form: where the database is, and who connects.
/// The URL may carry credentials too; the two fields win over it, so a
/// pasted URL can be corrected without editing the string.
struct Form {
    name: Entity<TextField>,
    url: Entity<TextField>,
    user: Entity<TextField>,
    password: Entity<TextField>,
    error: Option<String>,
    _subscriptions: Vec<Subscription>,
}

impl Form {
    /// Tab order, and the order the form reads on screen.
    fn fields(&self) -> [&Entity<TextField>; 4] {
        [&self.name, &self.url, &self.user, &self.password]
    }
}

impl EventEmitter<ConnectionsEvent> for Connections {}

impl Connections {
    pub fn new(cx: &mut Context<Self>) -> Self {
        let (store, error) = match Store::open_default() {
            Ok(store) => (Some(store), None),
            Err(error) => (None, Some(error.to_string())),
        };
        let mut screen = Self {
            focus_handle: cx.focus_handle(),
            store,
            error,
            rows: Vec::new(),
            form: None,
            now: unix_now(),
        };
        screen.reload(cx);
        screen.probe_all(cx);
        screen
    }

    /// Read the saved connections back from the store, keeping whatever a
    /// probe has already found for the rows that survive.
    fn reload(&mut self, cx: &mut Context<Self>) {
        let Some(store) = &self.store else { return };
        match store.list_connections() {
            Ok(saved) => {
                // A row that is still on screen keeps what its probe
                // found; only the rows that are new start over.
                let mut known: Vec<(String, Probe)> = std::mem::take(&mut self.rows)
                    .into_iter()
                    .map(|row| (row.saved.profile.id, row.state))
                    .collect();
                self.rows = saved
                    .into_iter()
                    .map(|saved| {
                        let state = known
                            .iter()
                            .position(|(id, _)| *id == saved.profile.id)
                            .map(|ix| known.remove(ix).1)
                            .unwrap_or(Probe::Probing);
                        Row { saved, state }
                    })
                    .collect();
                self.error = None;
            }
            Err(error) => self.error = Some(error.to_string()),
        }
        cx.notify();
    }

    fn probe_all(&mut self, cx: &mut Context<Self>) {
        for id in self.rows.iter().map(|row| row.saved.profile.id.clone()).collect::<Vec<_>>() {
            self.probe(&id, cx);
        }
    }

    /// Connect, count the relations, read the server version, drop the
    /// connection. The row paints whatever comes back.
    fn probe(&mut self, id: &str, cx: &mut Context<Self>) {
        let Some(row) = self.rows.iter_mut().find(|row| row.saved.profile.id == id) else {
            return;
        };
        let profile = row.saved.profile.clone();
        row.state = Probe::Probing;

        if profile.engine != Engine::Postgres {
            row.state = Probe::Failed("only PostgreSQL is supported yet".to_string());
            cx.notify();
            return;
        }

        let task = gpui_tokio::Tokio::spawn(cx, async move {
            let connection = PostgresConnection::connect_profile(&profile).await?;
            let catalog = connection.introspect().await?;
            // A server that will not say its version is still usable.
            let server = connection.server_version().await.unwrap_or_default();
            // A probe is a look, not a session: give the sockets back
            // before the row paints.
            connection.close().await;
            let (tables, views) = count_relations(&catalog);
            anyhow::Ok((tables, views, server))
        });

        let id = id.to_string();
        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                let state = match flatten(outcome) {
                    Ok((tables, views, server)) => {
                        if let Some(store) = &this.store {
                            // Cache what this probe saw; a failure to
                            // write it is not worth interrupting anyone.
                            store.record_probe(&id, tables, views, &server).ok();
                        }
                        Probe::Ready { tables, views, server }
                    }
                    Err(error) => Probe::Failed(error),
                };
                if let Some(row) = this.rows.iter_mut().find(|row| row.saved.profile.id == id) {
                    if let Probe::Ready { tables, views, server } = &state {
                        row.saved.tables = Some(*tables);
                        row.saved.views = Some(*views);
                        row.saved.server = Some(server.clone());
                    }
                    row.state = state;
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
    }

    /// Drop a saved connection, and its password with it. Nothing on the
    /// server is touched.
    fn forget(&mut self, id: &str, cx: &mut Context<Self>) {
        if let Some(store) = &self.store
            && let Err(error) = store.delete_profile(id)
        {
            self.error = Some(error.to_string());
        }
        secrets::delete_password(id).ok();
        self.reload(cx);
    }

    fn open(&mut self, id: &str, cx: &mut Context<Self>) {
        let Some(row) = self.rows.iter().find(|row| row.saved.profile.id == id) else {
            return;
        };
        let profile = row.saved.profile.clone();
        if let Some(store) = &self.store {
            store.mark_opened(&profile.id, unix_now()).ok();
        }
        cx.emit(ConnectionsEvent::Open(profile));
    }

    // --- the new connection form -----------------------------------------

    fn open_form(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.form.is_some() {
            return;
        }
        let name = cx.new(|cx| TextField::new("prod", cx));
        let url = cx.new(|cx| TextField::new("postgres://host:5432/database", cx));
        let user = cx.new(|cx| TextField::new("postgres", cx));
        let password = cx.new(|cx| TextField::new("", cx).masked());
        let subscriptions = vec![
            cx.subscribe_in(&name, window, Self::on_field_event),
            cx.subscribe_in(&url, window, Self::on_field_event),
            cx.subscribe_in(&user, window, Self::on_field_event),
            cx.subscribe_in(&password, window, Self::on_field_event),
        ];
        window.focus(&name.focus_handle(cx), cx);
        self.form =
            Some(Form { name, url, user, password, error: None, _subscriptions: subscriptions });
        cx.notify();
    }

    fn close_form(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.form = None;
        window.focus(&self.focus_handle, cx);
        cx.notify();
    }

    fn on_field_event(
        &mut self,
        field: &Entity<TextField>,
        event: &TextFieldEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            TextFieldEvent::Submit => self.save_form(window, cx),
            TextFieldEvent::Cancel => self.close_form(window, cx),
            TextFieldEvent::NextField => {
                // Tab walks the fields in order and wraps at the end.
                let Some(form) = &self.form else { return };
                let fields = form.fields();
                let here = fields.iter().position(|entry| *entry == field).unwrap_or(0);
                let next = fields[(here + 1) % fields.len()].clone();
                window.focus(&next.focus_handle(cx), cx);
            }
            TextFieldEvent::Changed => {
                if let Some(form) = &mut self.form {
                    // The message was about the URL as it was typed then.
                    form.error = None;
                    cx.notify();
                }
            }
        }
    }

    /// Turn the form into a saved profile, then probe it. The screen stays
    /// where it is, so a connection that does not answer says so in its
    /// own row rather than in a dead workspace.
    fn save_form(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(form) = &self.form else { return };
        let (name, url) = (
            form.name.read(cx).trimmed().to_string(),
            form.url.read(cx).trimmed().to_string(),
        );
        // A password may hold spaces at either end, so it is the one field
        // that is taken exactly as typed.
        let (user, typed_password) = (
            form.user.read(cx).trimmed().to_string(),
            form.password.read(cx).text().to_string(),
        );
        if url.is_empty() {
            self.set_form_error("a connection needs a URL", cx);
            return;
        }
        let Some(store) = &self.store else {
            self.set_form_error("there is nowhere to save connections", cx);
            return;
        };

        let (mut profile, url_password) =
            match db_postgres::profile_from_url(&new_id(), &name, &url) {
                Ok(parsed) => parsed,
                Err(error) => {
                    self.set_form_error(error.to_string(), cx);
                    return;
                }
            };
        let password = merge_credentials(&mut profile, &user, &typed_password, url_password);
        if let Err(error) = store.save_profile(&profile) {
            self.set_form_error(error.to_string(), cx);
            return;
        }
        // The password leaves the URL here and goes to the keychain, so
        // the profiles file never carries it.
        if let Some(password) = password
            && let Err(error) = secrets::set_password(&profile.id, &password)
        {
            self.set_form_error(format!("saved, but the password did not: {error}"), cx);
        }

        let id = profile.id.clone();
        self.close_form(window, cx);
        self.reload(cx);
        self.probe(&id, cx);
    }

    fn set_form_error(&mut self, message: impl Into<String>, cx: &mut Context<Self>) {
        if let Some(form) = &mut self.form {
            form.error = Some(message.into());
        }
        cx.notify();
    }
}

impl Focusable for Connections {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}

impl Render for Connections {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();

        div()
            .track_focus(&self.focus_handle(cx))
            .size_full()
            .bg(colors.window)
            .font_family(FONT_FAMILY)
            .text_color(colors.text_body)
            .child(
                div()
                    .id("connections")
                    .size_full()
                    .overflow_y_scroll()
                    .child(
                        div()
                            .max_w(px(COLUMN_WIDTH))
                            .mx_auto()
                            .px(px(32.))
                            .pt(px(56.))
                            .pb(px(40.))
                            .flex()
                            .flex_col()
                            .gap(px(26.))
                            .child(self.header(&colors, cx))
                            .child(self.list(&colors, cx))
                            .children(self.error.clone().map(|error| {
                                div().text_size(px(11.)).text_color(colors.error).child(error)
                            }))
                            .child(shortcuts(&colors, cx)),
                    ),
            )
    }
}

impl Connections {
    fn header(&self, colors: &ThemeColors, cx: &App) -> Div {
        let count = self.rows.len();
        let subtitle = match count {
            0 => "No databases yet. Add the first one below.".to_string(),
            1 => "Meerkat is standing watch over 1 database.".to_string(),
            n => format!("Meerkat is standing watch over {n} databases."),
        };

        div()
            .flex()
            .items_center()
            .gap(px(14.))
            .child(meerkat_mark(42., cx))
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(5.))
                    .child(
                        div()
                            .text_size(px(19.))
                            .font_weight(FontWeight::MEDIUM)
                            .text_color(colors.text)
                            .child(greeting()),
                    )
                    .child(div().text_size(px(12.)).text_color(colors.text_muted).child(subtitle)),
            )
    }

    fn list(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let mut list = div()
            .flex()
            .flex_col()
            .gap(px(9.))
            .child(section_label("CONNECTIONS", cx));

        for row in &self.rows {
            list = list.child(self.row(row, colors, cx));
        }

        match &self.form {
            Some(form) => list.child(self.form_card(form, colors, cx)),
            None => list.child(
                div()
                    .id("new-connection")
                    .flex()
                    .items_center()
                    .justify_center()
                    .gap(px(8.))
                    .p(px(13.))
                    .border_1()
                    .border_dashed()
                    .border_color(colors.border_strong)
                    .rounded(px(8.))
                    .text_size(px(11.))
                    .text_color(colors.text_muted)
                    .cursor_pointer()
                    .hover(|s| s.border_color(colors.accent).text_color(colors.accent))
                    .on_click(cx.listener(|this, _event, window, cx| this.open_form(window, cx)))
                    .child("+ new connection"),
            ),
        }
    }

    fn row(&self, row: &Row, colors: &ThemeColors, cx: &mut Context<Self>) -> Stateful<Div> {
        let failed = matches!(row.state, Probe::Failed(_));
        let id = row.saved.profile.id.clone();

        // Failure recolours the whole row, the way the comp does: warm
        // surface, warm border, warm text.
        let (name_color, url_color, meta_color, dot) = match &row.state {
            Probe::Ready { .. } => {
                (colors.text, colors.text_muted, colors.text_muted, colors.ok)
            }
            Probe::Probing => (colors.text, colors.text_muted, colors.text_muted, colors.idle),
            Probe::Failed(_) => (
                colors.error,
                colors.error_secondary,
                colors.error_secondary,
                colors.error_mark,
            ),
        };

        let stats = match &row.state {
            Probe::Ready { tables, views, .. } => relation_count(*tables, *views),
            Probe::Probing => match (row.saved.tables, row.saved.views) {
                // Last launch's counts, until this probe answers.
                (Some(tables), Some(views)) => relation_count(tables, views),
                _ => "connecting…".to_string(),
            },
            Probe::Failed(error) => first_line(error),
        };

        let mode = match &row.state {
            Probe::Ready { server, .. } if !server.is_empty() => {
                format!("read-only · {server}")
            }
            Probe::Ready { .. } => "read-only".to_string(),
            Probe::Probing => row
                .saved
                .server
                .clone()
                .map(|server| format!("read-only · {server}"))
                .unwrap_or_default(),
            Probe::Failed(_) => match row.saved.last_opened {
                Some(at) => format!("last seen {}", ago(self.now - at)),
                None => "never opened".to_string(),
            },
        };

        let (action, action_color) = match &row.state {
            Probe::Ready { .. } => ("open →", colors.accent),
            Probe::Probing => ("…", colors.text_faint),
            Probe::Failed(_) => ("retry", colors.error_secondary),
        };

        let retry = failed;
        let forget_id = id.clone();
        let card = div()
            .id(ElementId::Name(format!("connection-{id}").into()))
            .flex()
            .items_center()
            .gap(px(16.))
            .px(px(16.))
            .py(px(14.))
            .border_1()
            .border_color(if failed { colors.error_border } else { colors.border })
            .rounded(px(8.))
            .bg(if failed { colors.error_surface } else { colors.elevated })
            .cursor_pointer()
            .on_click(cx.listener(move |this, _event, _window, cx| {
                // A row that is down retries rather than opening a
                // workspace that cannot paint anything.
                if retry {
                    this.probe(&id, cx);
                } else {
                    this.open(&id, cx);
                }
            }))
            .child(div().w(px(9.)).flex_none().child(status_dot_of(dot)))
            .child(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .flex()
                    .flex_col()
                    .gap(px(4.))
                    .child(
                        div()
                            .text_size(px(13.))
                            .font_weight(FontWeight::MEDIUM)
                            .text_color(name_color)
                            .truncate()
                            .child(row.saved.profile.name.clone()),
                    )
                    .child(
                        div()
                            .text_size(px(11.))
                            .text_color(url_color)
                            .truncate()
                            .child(profile_url(&row.saved.profile)),
                    ),
            )
            .child(
                div()
                    .w(px(STATS_WIDTH))
                    .flex_none()
                    .text_size(px(11.))
                    .text_color(meta_color)
                    .truncate()
                    .child(stats),
            )
            .child(
                div()
                    .w(px(MODE_WIDTH))
                    .flex_none()
                    .text_size(px(11.))
                    .text_color(if failed { colors.error_faint } else { colors.text_muted })
                    .truncate()
                    .child(mode),
            )
            .child(
                div()
                    .w(px(ACTION_WIDTH))
                    .flex_none()
                    .text_size(px(11.))
                    .text_color(action_color)
                    .text_right()
                    .child(action),
            )
            .child(
                // Forgetting a connection drops the profile and its
                // keychain password. The database itself is untouched.
                div()
                    .id(ElementId::Name(format!("forget-{forget_id}").into()))
                    .flex_none()
                    .text_size(px(12.))
                    .text_color(colors.text_faint)
                    .cursor_pointer()
                    .hover(|s| s.text_color(colors.error))
                    .on_click(cx.listener(move |this, _event, _window, cx| {
                        this.forget(&forget_id, cx);
                    }))
                    .child("×"),
            );

        if failed {
            card.hover(|s| s.border_color(colors.error_mark))
        } else {
            card.hover(|s| s.border_color(colors.accent))
        }
    }

    fn form_card(&self, form: &Form, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let label = |text: &'static str| {
            div().text_size(px(10.)).text_color(colors.text_muted).child(text)
        };

        div()
            .flex()
            .flex_col()
            .gap(px(10.))
            .p(px(16.))
            .border_1()
            .border_color(colors.border_strong)
            .rounded(px(8.))
            .bg(colors.panel)
            .child(section_label("NEW CONNECTION", cx))
            .child(
                div()
                    .flex()
                    .gap(px(12.))
                    .child(
                        div()
                            .w(px(200.))
                            .flex_none()
                            .flex()
                            .flex_col()
                            .gap(px(5.))
                            .child(label("name"))
                            .child(form.name.clone()),
                    )
                    .child(
                        div()
                            .flex_1()
                            .min_w(px(0.))
                            .flex()
                            .flex_col()
                            .gap(px(5.))
                            .child(label("url"))
                            .child(form.url.clone()),
                    ),
            )
            .child(
                div()
                    .flex()
                    .gap(px(12.))
                    .child(
                        div()
                            .w(px(200.))
                            .flex_none()
                            .flex()
                            .flex_col()
                            .gap(px(5.))
                            .child(label("user"))
                            .child(form.user.clone()),
                    )
                    .child(
                        div()
                            .flex_1()
                            .min_w(px(0.))
                            .flex()
                            .flex_col()
                            .gap(px(5.))
                            .child(label("password"))
                            .child(form.password.clone()),
                    ),
            )
            .children(form.error.clone().map(|error| {
                div().text_size(px(11.)).text_color(colors.error).child(error)
            }))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(10.))
                    .child(
                        div()
                            .flex_1()
                            .text_size(px(10.))
                            .text_color(colors.text_faint)
                            // Say where the password goes before it is typed.
                            .child("the password goes to the OS keychain, not to the profiles file"),
                    )
                    .child(
                        div()
                            .id("cancel-connection")
                            .px(px(10.))
                            .py(px(5.))
                            .text_size(px(11.))
                            .text_color(colors.text_muted)
                            .cursor_pointer()
                            .hover(|s| s.text_color(colors.text))
                            .on_click(
                                cx.listener(|this, _event, window, cx| this.close_form(window, cx)),
                            )
                            .child("cancel"),
                    )
                    .child(
                        accent_button("save ⏎", cx)
                            .id("save-connection")
                            .on_click(
                                cx.listener(|this, _event, window, cx| this.save_form(window, cx)),
                            ),
                    ),
            )
    }
}

/// The keyboard the workspace answers to, as the comp lists it.
fn shortcuts(colors: &ThemeColors, cx: &App) -> Div {
    let line = |what: &'static str, keys: &'static str| {
        div()
            .flex()
            .justify_between()
            .child(div().child(what))
            .child(div().text_color(colors.text_muted).child(keys))
    };

    div()
        .flex()
        .flex_col()
        .gap(px(9.))
        .child(section_label("SHORTCUTS", cx))
        .child(
            div()
                .flex()
                .flex_col()
                .gap(px(8.))
                .px(px(13.))
                .py(px(11.))
                .border_1()
                .border_color(colors.border)
                .rounded(px(7.))
                .bg(colors.panel)
                .text_size(px(11.))
                .text_color(colors.text_secondary)
                .child(line("new query", "⌘T"))
                .child(line("run query", "⌘⏎"))
                .child(line("refresh", "⌘R"))
                .child(line("page back / forward", "⌘[ ⌘]")),
        )
}

/// The comp's dot, one size up: on its own row it needs the extra pixel.
fn status_dot_of(color: gpui::Hsla) -> Div {
    status_dot(color).size(px(9.))
}

/// `postgres://host:port/database`, with no credentials in it.
fn profile_url(profile: &Profile) -> SharedString {
    let scheme = match profile.engine {
        Engine::Postgres => "postgres",
        Engine::Mysql => "mysql",
        Engine::Sqlite => "sqlite",
    };
    let host = profile.host.clone().unwrap_or_else(|| "localhost".to_string());
    match profile.port {
        Some(port) => format!("{scheme}://{host}:{port}/{}", profile.database),
        None => format!("{scheme}://{host}/{}", profile.database),
    }
    .into()
}

/// Put the form's user and password over whatever the URL carried, and
/// hand back the password that goes to the keychain. A field left empty
/// keeps what the URL said, so a URL with credentials in it still works
/// on its own.
fn merge_credentials(
    profile: &mut Profile,
    user: &str,
    typed_password: &str,
    url_password: Option<String>,
) -> Option<String> {
    if !user.is_empty() {
        profile.user = Some(user.to_string());
    }
    if typed_password.is_empty() {
        url_password
    } else {
        Some(typed_password.to_string())
    }
}

fn count_relations(catalog: &Catalog) -> (u32, u32) {
    let mut tables = 0;
    let mut views = 0;
    for schema in &catalog.schemas {
        for table in &schema.tables {
            match table.kind {
                TableKind::Table => tables += 1,
                TableKind::View => views += 1,
            }
        }
    }
    (tables, views)
}

fn relation_count(tables: u32, views: u32) -> String {
    let plural = |count: u32, one: &str, many: &str| {
        format!("{count} {}", if count == 1 { one } else { many })
    };
    match views {
        0 => plural(tables, "table", "tables"),
        _ => format!("{} · {}", plural(tables, "table", "tables"), plural(views, "view", "views")),
    }
}

/// A driver error can run to several lines; a row has space for one.
fn first_line(error: &str) -> String {
    error.lines().next().unwrap_or_default().to_string()
}

fn greeting() -> &'static str {
    match Local::now().format("%H").to_string().parse::<u32>().unwrap_or(9) {
        5..=11 => "Good morning.",
        12..=17 => "Good afternoon.",
        _ => "Good evening.",
    }
}

/// How long ago, in the comp's shorthand: `3d ago`, `5h ago`, `just now`.
fn ago(seconds: i64) -> String {
    match seconds {
        ..=59 => "just now".to_string(),
        60..=3_599 => format!("{}m ago", seconds / 60),
        3_600..=86_399 => format!("{}h ago", seconds / 3_600),
        _ => format!("{}d ago", seconds / 86_400),
    }
}

/// Wall clock in unix seconds: what the store keeps its times in, both
/// for "last opened" and for the query history.
pub(crate) fn unix_now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|since| since.as_secs() as i64)
        .unwrap_or_default()
}

/// A profile id that no other profile will take. The clock is enough:
/// two connections are never saved in the same nanosecond.
fn new_id() -> String {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|since| since.as_nanos())
        .unwrap_or_default();
    format!("{nanos:x}")
}

/// Collapse "the task died" and "the probe failed" into one message.
fn flatten<T>(outcome: Result<anyhow::Result<T>, gpui_tokio::JoinError>) -> Result<T, String> {
    match outcome {
        Ok(Ok(value)) => Ok(value),
        Ok(Err(error)) => Err(error.to_string()),
        Err(join) => Err(format!("the probe was interrupted: {join}")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use db_client::Engine;

    #[test]
    fn a_row_names_its_relations() {
        assert_eq!(relation_count(14, 3), "14 tables · 3 views");
        assert_eq!(relation_count(1, 1), "1 table · 1 view");
        assert_eq!(relation_count(9, 0), "9 tables");
    }

    #[test]
    fn a_row_url_carries_no_credentials() {
        let profile = Profile {
            id: "p1".into(),
            name: "prod".into(),
            engine: Engine::Postgres,
            host: Some("db.internal".into()),
            port: Some(5432),
            database: "meerkat".into(),
            user: Some("ada".into()),
        };
        assert_eq!(profile_url(&profile), "postgres://db.internal:5432/meerkat");
    }

    #[test]
    fn last_seen_reads_in_the_largest_unit_that_fits() {
        assert_eq!(ago(12), "just now");
        assert_eq!(ago(300), "5m ago");
        assert_eq!(ago(7_200), "2h ago");
        assert_eq!(ago(3 * 86_400 + 60), "3d ago");
    }

    #[test]
    fn the_credential_fields_win_over_the_url() {
        let base = Profile {
            id: "p1".into(),
            name: "prod".into(),
            engine: Engine::Postgres,
            host: Some("db.internal".into()),
            port: Some(5432),
            database: "meerkat".into(),
            user: Some("from_url".into()),
        };

        // Both fields filled: the URL's credentials are replaced.
        let mut profile = base.clone();
        let password =
            merge_credentials(&mut profile, "ada", "typed", Some("from_url".to_string()));
        assert_eq!(profile.user.as_deref(), Some("ada"));
        assert_eq!(password.as_deref(), Some("typed"));

        // Both fields empty: a URL that carries its own credentials still
        // works on its own.
        let mut profile = base.clone();
        let password = merge_credentials(&mut profile, "", "", Some("from_url".to_string()));
        assert_eq!(profile.user.as_deref(), Some("from_url"));
        assert_eq!(password.as_deref(), Some("from_url"));

        // A password field left empty means no password at all, which is
        // what trust and peer authentication need.
        let mut profile = base;
        let password = merge_credentials(&mut profile, "ada", "", None);
        assert_eq!(profile.user.as_deref(), Some("ada"));
        assert_eq!(password, None);
    }

    #[test]
    fn a_multi_line_driver_error_fits_one_row() {
        assert_eq!(first_line("connection refused\n  caused by: ..."), "connection refused");
    }
}
