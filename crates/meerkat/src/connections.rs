//! The connections screen: what the app opens on when it is not pointed
//! at a database.
//!
//! It lists the saved connections and takes a new `postgres://` URL.
//! Profiles live in the local SQLite store; passwords go to the OS
//! keychain and never touch that file.
//!
//! ⌘F narrows the list. The search reads the rows already in memory —
//! name, URL and user — so it costs a substring scan, not a query, and
//! it is a filter, not a jump: the list keeps the store's order.
//!
//! Opening the screen touches no server. A saved connection is a note of
//! where a database is, not a standing session: the app must not wake a
//! sleeping server, spend a connection slot, or ask the keychain for a
//! password nobody asked it to use. A probe — connect, read the server
//! version, disconnect on the tokio runtime — runs only when the user
//! asks for it: saving a connection, the form's "test" button, or the
//! "retry" action. The version it read is cached in the store, so the
//! next launch paints "read-only · PG 17.2" without a socket.
//!
//! **"test" lives in the form, at the end of the URL it tests.** The
//! question it answers is "does what I have typed reach a database", and a
//! saved row has answered that already: the row paints what the last probe
//! found, and ⏎ retries the ones that are down. So the button belongs where
//! the answer is still unknown, and it tests the fields *as typed*, saving
//! nothing — a URL that does not work must not have to be saved first.
//!
//! The rows come grouped by an environment tag the user gives a
//! connection in the form. The set of tags is closed — prod, staging,
//! dev — because the tags exist to be compared across connections, and
//! free text would give every database its own spelling of "prod". The
//! tag is a label on the saved row, never a connection parameter.
//! Groups follow the form's own order with the untagged rows last; with
//! no tag anywhere the headings disappear and the list reads flat. The
//! "group by env" switch beside the search turns the grouping off, and
//! the store remembers the choice across launches.
//!
//! The list selects before it acts, after the "select, then act" comp.
//! A click selects a row and the bar over the list serves the selection:
//! connect, edit, forget. ⏎ or a double click connects, ↑↓ move
//! the selection, ⌘I edits. One bar for the actions keeps the rows
//! dense, and the keyboard never needs the mouse.
//!
//! Editing reuses the new-connection form, prefilled from the profile.
//! The saved password never comes back into the form; a password field
//! left empty keeps the one in the keychain.

use crate::env::Env;
use chrono::Local;
use db_client::{Connection, Engine, Profile};
use db_postgres::{Password, PostgresConnection};
use gpui::{
    Animation, AnimationExt, AnyElement, App, Context, Div, ElementId, Entity, EventEmitter,
    FocusHandle, Focusable, FontWeight, KeyBinding, SharedString, Stateful, Subscription, Window,
    actions, div, prelude::*, px,
};
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use storage::{SavedConnection, Store};
use theme::{FONT_FAMILY, ThemeColors, theme};
use ui::{
    TextField, TextFieldEvent, accent_button, meerkat_mark, section_label, status_dot, switch,
    toolbar_button, toolbar_button_bare,
};

/// The comp's reading column: the list never stretches over a wide window.
const COLUMN_WIDTH: f32 = 900.;
/// Empty on a row that is up, so it costs nothing until a probe fails
/// and needs the room for the error.
const STATUS_WIDTH: f32 = 200.;
const MODE_WIDTH: f32 = 130.;
const HOST_WIDTH: f32 = 230.;
const LAST_WIDTH: f32 = 80.;
/// The search line sits beside the section label, so it takes a fixed
/// share of that row rather than all of it.
const SEARCH_WIDTH: f32 = 300.;
/// The width the form's "test" button holds whatever it says. The label
/// changes while a test is out — "test" is 4 characters and "testing…" is
/// 8 — and a button that resized around its own word would move the URL
/// field beside it on every press, which is a shift the eye reads as a
/// flicker on the very element the pointer is on. The room is taken for
/// the longest word once, and the label is centred in it.
const TEST_WIDTH: f32 = 74.;
/// The room the form's message line holds whether or not it has anything
/// to say, so an answer never moves the blocks under it. Two lines of the
/// 10px face: a test result is one short line, and a driver's refusal —
/// `error returned from database: password authentication failed for user
/// "postgres"` — is the message worth reading whole, so the room is taken
/// for the longer of the two.
const MESSAGE_HEIGHT: f32 = 28.;
/// One cycle of the test button's breath while a test is out, and how much
/// of the button's own ink each breath mixes into the fill. It is the run
/// button's gesture at a slower pace: this button sits in a form the user
/// is reading rather than in the toolbar over a result they are waiting
/// for, and a slow breath asks to be noticed once rather than watched.
/// `pulsating_between` is a sine, so the turn at either end of the cycle is
/// already smooth. Nothing travels across the button and nothing moves by a
/// pixel — see the run button for why.
const TEST_BREATH: Duration = Duration::from_millis(3600);
const TEST_BREATH_DEPTH: f32 = 0.12;
/// How long a word coming up on the button, or a message coming up under
/// it, takes to reach full ink.
///
/// It is a fade and **only** a fade. The width the word sits in and the
/// room the message sits in are both already taken — `TEST_WIDTH` and
/// `MESSAGE_HEIGHT` — so a slide or a grow would be the very shift those
/// two constants exist to prevent. Text that swapped in one frame reads as
/// a flicker even when nothing has moved; over `FADE_IN` it reads as an
/// answer arriving.
///
/// The easing is `ease_in_out`, which leaves and arrives slowly. A quint
/// ease-out — what the run button's settle uses, where the point is to be
/// over quickly — spends most of a long duration almost finished, so the
/// longer it is set the more it reads as a snap followed by a crawl.
const FADE_IN: Duration = Duration::from_millis(480);
/// How much ink the word keeps at the start of its fade. The two words are
/// a **crossfade**, not an arrival: `test` is already on the button when
/// `testing…` replaces it, so a fade from nothing would blink the slot
/// empty for a moment. A message under the URL does start from nothing, and
/// fades from nothing.
const WORD_FADE_FLOOR: f32 = 0.3;

/// The screen's own key context. ⌘F belongs to this screen, not to the
/// app, so it cannot take the key from a workspace that is open.
const KEY_CONTEXT: &str = "Connections";

/// Where the "group by env" switch keeps its state across launches.
const GROUP_BY_ENV_KEY: &str = "connections.group_by_env";

actions!(
    connections,
    [FocusSearch, SelectNext, SelectPrevious, OpenSelected, EditSelected]
);

pub fn key_bindings() -> Vec<KeyBinding> {
    vec![
        KeyBinding::new("cmd-f", FocusSearch, Some(KEY_CONTEXT)),
        // The walk follows the list, and ⏎ connects: the keyboard path
        // the comp promises with "↑↓ move · ⏎ connect · ⌘I edit".
        KeyBinding::new("down", SelectNext, Some(KEY_CONTEXT)),
        KeyBinding::new("up", SelectPrevious, Some(KEY_CONTEXT)),
        KeyBinding::new("enter", OpenSelected, Some(KEY_CONTEXT)),
        KeyBinding::new("cmd-i", EditSelected, Some(KEY_CONTEXT)),
    ]
}

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
    /// The search line over the saved connections. It filters the list
    /// that is already in memory; it never reaches a server.
    search: Entity<TextField>,
    /// The selected row, by profile id so it survives a reload. A click
    /// selects; only ⏎, the connect button or a double click opens.
    selected: Option<String>,
    /// Whether the list stands in environment groups or reads flat, as
    /// the "group by env" switch says. The store remembers it.
    group_by_env: bool,
    form: Option<Form>,
    /// Unix seconds when the screen opened, so "3d ago" stays put while
    /// the user reads it.
    now: i64,
    _subscriptions: Vec<Subscription>,
}

struct Row {
    saved: SavedConnection,
    state: Probe,
}

/// What the last probe of one connection found.
enum Probe {
    /// Nothing has been asked of this server in this session. The row
    /// paints what the store cached at the last probe, if anything.
    Idle,
    Probing,
    Ready { server: String },
    Failed(String),
}

/// The connection form: where the database is, and who connects. It
/// serves both "+ new connection" and "edit ⌘I"; `editing` carries the
/// profile id an edit writes back to. The URL may carry credentials too;
/// the two fields win over it, so a pasted URL can be corrected without
/// editing the string.
struct Form {
    name: Entity<TextField>,
    url: Entity<TextField>,
    user: Entity<TextField>,
    password: Entity<TextField>,
    /// The environment tag, picked from the chips; `None` is untagged.
    env: Option<Env>,
    /// Whether the session this connection opens refuses to write. A new
    /// connection starts on: the careful setting is the one nobody has to
    /// remember to choose.
    read_only: bool,
    /// What the "test" button beside the URL last found. It is state of the
    /// form rather than of a row: nothing tested here is saved, so there is
    /// no row to paint it in.
    test: FormTest,
    /// Counts the tests this form has sent. A reply that does not carry the
    /// current count is dropped: a keystroke has changed the fields under
    /// it, so it answers about a URL that is not on screen any more.
    test_generation: u64,
    /// Counts the messages the form has shown. It is what the message's
    /// fade is keyed by — a GPUI animation replays when its element's id
    /// changes — and `0` is a form that has said nothing yet, which must
    /// not fade an empty line in at a user who has asked nothing.
    messages: u64,
    /// The profile this form edits; `None` saves a new one.
    editing: Option<String>,
    error: Option<String>,
    _subscriptions: Vec<Subscription>,
}

impl Form {
    /// Tab order, and the order the form reads on screen.
    fn fields(&self) -> [&Entity<TextField>; 4] {
        [&self.name, &self.url, &self.user, &self.password]
    }
}

/// What the form's own probe found. `Idle` is where every keystroke puts it
/// back: the answer was about the URL as it was typed then.
enum FormTest {
    Idle,
    Testing,
    /// The server version, as `server_version` shortens it. It is empty
    /// when the server would not say.
    Ok(String),
    Failed(String),
}

impl EventEmitter<ConnectionsEvent> for Connections {}

impl Connections {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        // The footer carries the updater's line, so the screen repaints
        // when the updater moves; a local build has no updater.
        if let Some(updater) = auto_update::AutoUpdater::try_global(cx) {
            cx.observe(&updater, |_, _, cx| cx.notify()).detach();
        }
        let (store, error) = match Store::open_default() {
            Ok(store) => (Some(store), None),
            Err(error) => (None, Some(error.to_string())),
        };
        let search = cx.new(|cx| TextField::new("name, host, port or database", cx).bare(11.));
        let subscription = cx.subscribe_in(&search, window, Self::on_search_event);
        let group_by_env =
            store.as_ref().map(|store| store.flag(GROUP_BY_ENV_KEY, true)).unwrap_or(true);
        let mut screen = Self {
            focus_handle: cx.focus_handle(),
            store,
            error,
            rows: Vec::new(),
            search,
            selected: None,
            group_by_env,
            form: None,
            now: unix_now(),
            _subscriptions: vec![subscription],
        };
        screen.reload(cx);
        screen
    }

    fn focus_search(&mut self, _: &FocusSearch, window: &mut Window, cx: &mut Context<Self>) {
        window.focus(&self.search.focus_handle(cx), cx);
        cx.notify();
    }

    fn on_search_event(
        &mut self,
        _field: &Entity<TextField>,
        event: &TextFieldEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            // esc empties the line. A filter left behind would hide
            // connections with nothing on screen to say why.
            TextFieldEvent::Cancel => {
                self.search.update(cx, |field, cx| field.clear(cx));
                window.focus(&self.focus_handle, cx);
                cx.notify();
            }
            // With the list down to one connection, ⏎ opens it: the
            // search line is then the fastest way into a database.
            TextFieldEvent::Submit => {
                let matched: Vec<String> = self
                    .matching(cx)
                    .into_iter()
                    .map(|row| row.saved.profile.id.clone())
                    .collect();
                if let [id] = matched.as_slice() {
                    self.open(&id.clone(), cx);
                }
            }
            _ => cx.notify(),
        }
    }

    /// The rows the search line leaves on screen, in the store's order.
    fn matching(&self, cx: &App) -> Vec<&Row> {
        let query = self.search.read(cx).trimmed().to_ascii_lowercase();
        self.rows.iter().filter(|row| matches_query(&row.saved, &query)).collect()
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
                            .unwrap_or(Probe::Idle);
                        Row { saved, state }
                    })
                    .collect();
                self.error = None;
            }
            Err(error) => self.error = Some(error.to_string()),
        }
        self.ensure_selection();
        cx.notify();
    }

    /// Keep the selection on the row it was on; a selection that points
    /// at nothing (first load, a forgotten row) falls to the first row,
    /// so the action bar always serves something when there are rows.
    fn ensure_selection(&mut self) {
        let still_there = self
            .selected
            .as_ref()
            .is_some_and(|id| self.rows.iter().any(|row| row.saved.profile.id == *id));
        if !still_there {
            self.selected = self.rows.first().map(|row| row.saved.profile.id.clone());
        }
    }

    fn selected_row(&self) -> Option<&Row> {
        let id = self.selected.as_ref()?;
        self.rows.iter().find(|row| row.saved.profile.id == *id)
    }

    fn select(&mut self, id: &str, cx: &mut Context<Self>) {
        self.selected = Some(id.to_string());
        cx.notify();
    }

    fn toggle_grouping(&mut self, cx: &mut Context<Self>) {
        self.group_by_env = !self.group_by_env;
        if let Some(store) = &self.store {
            // Losing the choice on a crash is not worth interrupting anyone.
            store.set_flag(GROUP_BY_ENV_KEY, self.group_by_env).ok();
        }
        cx.notify();
    }

    /// The rows in the order the screen paints them: grouped when the
    /// switch says so, flat otherwise. ↑↓ and the list must agree on
    /// this order, or the selection would jump.
    fn visible_ids(&self, cx: &App) -> Vec<String> {
        if self.group_by_env {
            group_by_env(self.matching(cx))
                .into_iter()
                .flat_map(|(_, rows)| rows)
                .map(|row| row.saved.profile.id.clone())
                .collect()
        } else {
            self.matching(cx).into_iter().map(|row| row.saved.profile.id.clone()).collect()
        }
    }

    /// ↑↓ walk the rows the search leaves on screen, in the order the
    /// screen paints them, clamped at the ends: a short list is not a
    /// ring, and the edge is where the eye stops.
    fn move_selection(&mut self, delta: isize, cx: &mut Context<Self>) {
        let ids = self.visible_ids(cx);
        if ids.is_empty() {
            return;
        }
        let here = self.selected.as_ref().and_then(|id| ids.iter().position(|x| x == id));
        let next = match here {
            Some(ix) => (ix as isize + delta).clamp(0, ids.len() as isize - 1) as usize,
            None => 0,
        };
        self.selected = Some(ids[next].clone());
        cx.notify();
    }

    fn select_next(&mut self, _: &SelectNext, _: &mut Window, cx: &mut Context<Self>) {
        self.move_selection(1, cx);
    }

    fn select_previous(&mut self, _: &SelectPrevious, _: &mut Window, cx: &mut Context<Self>) {
        self.move_selection(-1, cx);
    }

    fn open_selected(&mut self, _: &OpenSelected, _: &mut Window, cx: &mut Context<Self>) {
        if let Some(id) = self.selected.clone() {
            self.connect(&id, cx);
        }
    }

    fn edit_selected(&mut self, _: &EditSelected, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(id) = self.selected.clone() {
            self.open_edit(&id, window, cx);
        }
    }

    /// ⏎, the connect button and a double click all land here. A row
    /// that is down retries rather than opening a workspace that cannot
    /// paint anything.
    fn connect(&mut self, id: &str, cx: &mut Context<Self>) {
        let failed = self
            .rows
            .iter()
            .find(|row| row.saved.profile.id == id)
            .is_some_and(|row| matches!(row.state, Probe::Failed(_)));
        if failed {
            self.probe(id, cx);
        } else {
            self.open(id, cx);
        }
    }

    /// Connect, read the server version, drop the connection. The row
    /// paints whatever comes back. A probe never introspects: the row
    /// says nothing about a catalog, so reading one would cost a large
    /// database a long query for a line nobody reads.
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
            // A server that will not say its version is still usable.
            let server = connection.server_version().await.unwrap_or_default();
            // A probe is a look, not a session: give the sockets back
            // before the row paints.
            connection.close().await;
            anyhow::Ok(server)
        });

        let id = id.to_string();
        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                let state = match flatten(outcome) {
                    Ok(server) => {
                        if let Some(store) = &this.store {
                            // Cache what this probe saw; a failure to
                            // write it is not worth interrupting anyone.
                            store.record_probe(&id, &server).ok();
                        }
                        Probe::Ready { server }
                    }
                    Err(error) => Probe::Failed(error),
                };
                if let Some(row) = this.rows.iter_mut().find(|row| row.saved.profile.id == id) {
                    if let Probe::Ready { server } = &state {
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

    // --- the connection form (new, and edit) ------------------------------

    fn open_form(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.form.is_some() {
            return;
        }
        self.form = Some(self.build_form(None, window, cx));
        cx.notify();
    }

    /// Edit reuses the same form, prefilled from the saved row. The saved
    /// password stays in the keychain: it never comes back on screen, and
    /// an empty password field on save keeps it as it is.
    fn open_edit(&mut self, id: &str, window: &mut Window, cx: &mut Context<Self>) {
        let Some(row) = self.rows.iter().find(|row| row.saved.profile.id == id) else {
            return;
        };
        let saved = row.saved.clone();
        self.form = Some(self.build_form(Some(&saved), window, cx));
        cx.notify();
    }

    fn build_form(
        &mut self,
        prefill: Option<&SavedConnection>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Form {
        let name = cx.new(|cx| TextField::new("prod", cx));
        let url = cx.new(|cx| TextField::new("postgres://host:5432/database", cx));
        let user = cx.new(|cx| TextField::new("postgres", cx));
        let password = cx.new(|cx| TextField::new("", cx).masked());
        if let Some(saved) = prefill {
            let profile = &saved.profile;
            name.update(cx, |field, cx| field.set_text(profile.name.clone(), cx));
            url.update(cx, |field, cx| field.set_text(profile_url(profile).to_string(), cx));
            if let Some(who) = profile.user.clone() {
                user.update(cx, |field, cx| field.set_text(who, cx));
            }
        }
        let subscriptions = vec![
            cx.subscribe_in(&name, window, Self::on_field_event),
            cx.subscribe_in(&url, window, Self::on_field_event),
            cx.subscribe_in(&user, window, Self::on_field_event),
            cx.subscribe_in(&password, window, Self::on_field_event),
        ];
        window.focus(&name.focus_handle(cx), cx);
        Form {
            name,
            url,
            user,
            password,
            env: Env::parse(prefill.and_then(|saved| saved.env.as_deref())),
            read_only: prefill.map(|saved| saved.profile.read_only).unwrap_or(true),
            test: FormTest::Idle,
            test_generation: 0,
            messages: 0,
            editing: prefill.map(|saved| saved.profile.id.clone()),
            error: None,
            _subscriptions: subscriptions,
        }
    }

    /// A chip toggles: clicking the tag the form already has takes it
    /// away, so untagged needs no fourth chip.
    fn set_form_env(&mut self, env: Env, cx: &mut Context<Self>) {
        if let Some(form) = &mut self.form {
            form.env = if form.env == Some(env) { None } else { Some(env) };
            cx.notify();
        }
    }

    fn toggle_form_read_only(&mut self, cx: &mut Context<Self>) {
        if let Some(form) = &mut self.form {
            form.read_only = !form.read_only;
            cx.notify();
        }
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
                    // The message was about the URL as it was typed then,
                    // and so was whatever "test" found. The count going up
                    // drops the reply of a test still in flight.
                    form.error = None;
                    form.test = FormTest::Idle;
                    form.test_generation += 1;
                    cx.notify();
                }
            }
        }
    }

    /// Turn the form into a saved profile, then probe it. An edit keeps
    /// the profile's id, so the store row, the keychain entry, the
    /// history and the cached catalog all stay its. The screen stays
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
        let env = form.env;
        let read_only = form.read_only;
        let id = form.editing.clone().unwrap_or_else(new_id);
        if url.is_empty() {
            self.set_form_error("a connection needs a URL", cx);
            return;
        }
        let Some(store) = &self.store else {
            self.set_form_error("there is nowhere to save connections", cx);
            return;
        };

        let (mut profile, url_password) = match db_postgres::profile_from_url(&id, &name, &url) {
            Ok(parsed) => parsed,
            Err(error) => {
                self.set_form_error(error.to_string(), cx);
                return;
            }
        };
        // The URL says nothing about how careful to be; the switch does.
        profile.read_only = read_only;
        let password = merge_credentials(&mut profile, &user, &typed_password, url_password);
        if let Err(error) = store
            .save_profile(&profile)
            .and_then(|()| store.set_env(&profile.id, env.map(Env::as_str)))
        {
            self.set_form_error(error.to_string(), cx);
            return;
        }
        // The password leaves the URL here and goes to the keychain, so
        // the profiles file never carries it. `None` writes nothing, which
        // is what lets an edit keep the password it already has.
        if let Some(password) = password
            && let Err(error) = secrets::set_password(&profile.id, &password)
        {
            self.set_form_error(format!("saved, but the password did not: {error}"), cx);
        }

        let id = profile.id.clone();
        self.close_form(window, cx);
        self.selected = Some(id.clone());
        self.reload(cx);
        self.probe(&id, cx);
    }

    /// Test the fields as they are typed, and save nothing.
    ///
    /// The URL goes through the same parse a save makes, so what is tried
    /// is what a save would store. The password is handed to the driver
    /// rather than looked up, because a connection that has not been saved
    /// has nothing in the keychain under its id. An **edit** that left the
    /// field empty is the one case that does look it up — the same reading
    /// of an empty field the save takes.
    ///
    /// It reads the server version and gives the connection straight back,
    /// as `probe` does: a test is a look, not a session.
    fn test_form(&mut self, cx: &mut Context<Self>) {
        let Some(form) = &self.form else { return };
        if matches!(form.test, FormTest::Testing) {
            return;
        }
        let url = form.url.read(cx).trimmed().to_string();
        let user = form.user.read(cx).trimmed().to_string();
        // A password may hold spaces at either end, as it may on save.
        let typed_password = form.password.read(cx).text().to_string();
        let read_only = form.read_only;
        let editing = form.editing.clone();
        if url.is_empty() {
            self.set_form_error("a connection needs a URL", cx);
            return;
        }
        // The id is only what the keychain is keyed by, and a connection
        // that has never been saved asks the keychain nothing.
        let id = editing.clone().unwrap_or_default();
        let (mut profile, url_password) = match db_postgres::profile_from_url(&id, "", &url) {
            Ok(parsed) => parsed,
            Err(error) => {
                self.set_form_error(error.to_string(), cx);
                return;
            }
        };
        // Test the session the switch asks for, so a pooler that refuses a
        // read-only session says so here rather than on the first open.
        profile.read_only = read_only;
        let password = merge_credentials(&mut profile, &user, &typed_password, url_password);
        let from_keychain = password.is_none() && editing.is_some();

        let generation = {
            let Some(form) = &mut self.form else { return };
            form.test_generation += 1;
            form.test = FormTest::Testing;
            form.error = None;
            form.test_generation
        };

        let task = gpui_tokio::Tokio::spawn(cx, async move {
            let credential = if from_keychain {
                Password::Keychain
            } else {
                Password::Given(password.as_deref())
            };
            let connection = PostgresConnection::connect_probe(&profile, credential).await?;
            // A server that will not say its version is still usable.
            let server = connection.server_version().await.unwrap_or_default();
            connection.close().await;
            anyhow::Ok(server)
        });

        cx.spawn(async move |this, cx| {
            let outcome = task.await;
            this.update(cx, |this, cx| {
                let Some(form) = &mut this.form else { return };
                // A keystroke since the request left makes this an answer
                // about a URL that is not on screen any more.
                if form.test_generation != generation {
                    return;
                }
                form.test = match flatten(outcome) {
                    Ok(server) => FormTest::Ok(server),
                    Err(error) => FormTest::Failed(error),
                };
                form.messages += 1;
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
    }

    fn set_form_error(&mut self, message: impl Into<String>, cx: &mut Context<Self>) {
        if let Some(form) = &mut self.form {
            form.error = Some(message.into());
            form.messages += 1;
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
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();

        div()
            .track_focus(&self.focus_handle(cx))
            .key_context(KEY_CONTEXT)
            .on_action(cx.listener(Self::focus_search))
            .on_action(cx.listener(Self::select_next))
            .on_action(cx.listener(Self::select_previous))
            .on_action(cx.listener(Self::open_selected))
            .on_action(cx.listener(Self::edit_selected))
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
                            .child(self.list(&colors, window, cx))
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

    fn list(&self, colors: &ThemeColors, window: &Window, cx: &mut Context<Self>) -> Div {
        let searching = !self.search.read(cx).is_empty();

        // One card holds it all: the action bar over the selection, the
        // column headings, the rows, and the footer with "+ new
        // connection" and the keys. The rows carry no chrome of their
        // own, which is what keeps them dense.
        let mut card = div()
            .flex()
            .flex_col()
            .border_1()
            .border_color(colors.border_strong)
            .rounded(px(8.))
            .bg(colors.elevated)
            .overflow_hidden();
        if let Some(bar) = self.action_bar(colors, cx) {
            card = card.child(bar);
        }
        card = card.child(self.header_row(colors));
        if self.group_by_env {
            // With no tag anywhere the headings say nothing, so they are
            // left out and the list reads flat even with the switch on.
            let groups = group_by_env(self.matching(cx));
            let tagged = groups.iter().any(|(env, _)| env.is_some());
            for (env, rows) in groups {
                if tagged {
                    card = card.child(self.group_heading(env, colors));
                }
                for row in rows {
                    card = card.child(self.row(row, colors, cx));
                }
            }
        } else {
            for row in self.matching(cx) {
                card = card.child(self.row(row, colors, cx));
            }
        }

        // A filter that hides everything must say so, or the screen reads
        // as a store that lost its connections.
        if searching && self.matching(cx).is_empty() {
            card = card.child(
                div()
                    .px(px(13.))
                    .py(px(12.))
                    .text_size(px(11.))
                    .text_color(colors.text_muted)
                    .child("no connection matches the search"),
            );
        }
        card = card.child(self.footer(colors, cx));

        let mut list = div()
            .flex()
            .flex_col()
            .gap(px(9.))
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .gap(px(12.))
                    .child(section_label("CONNECTIONS", cx))
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap(px(10.))
                            .child(self.env_switch(colors, cx))
                            // The line holds its place whatever the list
                            // holds, so it does not appear and disappear
                            // under the pointer.
                            .child(self.search_line(colors, window, cx)),
                    ),
            )
            .child(card);

        if let Some(form) = &self.form {
            list = list.child(self.form_card(form, colors, cx));
        }
        list
    }

    /// The bar over the list. It serves the selected row — connect, edit,
    /// test, forget — so the rows themselves stay free of buttons, and
    /// there is one place to look for what ⏎ will do.
    fn action_bar(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Option<Div> {
        let row = self.selected_row()?;
        let id = row.saved.profile.id.clone();
        let failed = matches!(row.state, Probe::Failed(_));

        let meta = match &row.state {
            Probe::Failed(error) => {
                let seen = match row.saved.last_opened {
                    Some(at) => format!("last seen {}", ago(self.now - at)),
                    None => "never opened".to_string(),
                };
                format!("{} · {seen}", first_line(error))
            }
            Probe::Probing => format!("{} · connecting…", profile_url(&row.saved.profile)),
            _ => {
                let server = row
                    .saved
                    .server
                    .clone()
                    .map(|server| format!(" · {server}"))
                    .unwrap_or_default();
                format!(
                    "{} · {}{server}",
                    profile_url(&row.saved.profile),
                    mode_word(&row.saved.profile)
                )
            }
        };

        let connect_label = match &row.state {
            Probe::Probing => "…",
            Probe::Failed(_) => "retry ⏎",
            _ => "connect ⏎",
        };

        let (connect_id, edit_id, forget_id) = (id.clone(), id.clone(), id);
        Some(
            div()
                .flex()
                .items_center()
                .gap(px(8.))
                .px(px(12.))
                .py(px(8.))
                .bg(colors.panel)
                .border_b_1()
                .border_color(colors.border)
                .child(
                    div()
                        .text_size(px(11.))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(if failed { colors.error } else { colors.text })
                        .flex_none()
                        .child(row.saved.profile.name.clone()),
                )
                .children(Env::parse(row.saved.env.as_deref()).map(|env| {
                    // The selection's tag, as a small chip: the heading
                    // that says it may be off screen.
                    div()
                        .flex_none()
                        .flex()
                        .items_center()
                        .gap(px(4.))
                        .px(px(5.))
                        .py(px(2.))
                        .border_1()
                        .border_color(colors.border_strong)
                        .rounded(px(4.))
                        .text_size(px(8.))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.text_muted)
                        .child(status_dot(env.ring(colors)).size(px(5.)))
                        .child(env.as_str().to_ascii_uppercase())
                }))
                .child(
                    div()
                        .flex_1()
                        .min_w(px(0.))
                        .text_size(px(10.))
                        .text_color(if failed { colors.error_secondary } else { colors.text_muted })
                        .truncate()
                        .child(meta),
                )
                .child(
                    accent_button(connect_label, cx)
                        .id("connect-selected")
                        .text_size(px(10.))
                        .on_click(cx.listener(move |this, _event, _window, cx| {
                            this.connect(&connect_id, cx);
                        })),
                )
                .child(
                    toolbar_button("edit ⌘I", cx)
                        .id("edit-selected")
                        .text_size(px(10.))
                        .on_click(cx.listener(move |this, _event, window, cx| {
                            this.open_edit(&edit_id, window, cx);
                        })),
                )
                .child(
                    // Forgetting a connection drops the profile and its
                    // keychain password. The database itself is untouched.
                    // `toolbar_button` already carries a hover style, and
                    // GPUI panics on a second one.
                    toolbar_button("forget", cx)
                        .id("forget-selected")
                        .text_size(px(10.))
                        .on_click(cx.listener(move |this, _event, _window, cx| {
                            this.forget(&forget_id, cx);
                        })),
                ),
        )
    }

    /// One environment's heading over its rows, in the section-label
    /// voice the sidebar uses for its groups. The dot carries the
    /// environment's own color; the untagged group has none.
    fn group_heading(&self, env: Option<Env>, colors: &ThemeColors) -> Div {
        let text = match env {
            Some(env) => env.as_str().to_ascii_uppercase(),
            None => "UNTAGGED".to_string(),
        };
        div()
            .flex()
            .items_center()
            .gap(px(6.))
            .px(px(13.))
            .pt(px(10.))
            .pb(px(5.))
            .border_b_1()
            .border_color(colors.hairline)
            .text_size(px(9.))
            .font_weight(FontWeight::SEMIBOLD)
            .text_color(colors.text_faint)
            .children(env.map(|env| status_dot(env.ring(colors)).size(px(5.))))
            .child(text)
    }

    /// The "group by env" switch: a small pill whose knob sits at the
    /// end the state is. It flips the list between environment groups
    /// and the flat store order.
    fn env_switch(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Stateful<Div> {
        let on = self.group_by_env;
        let mut knob = div()
            .w(px(20.))
            .h(px(11.))
            .rounded_full()
            .px(px(2.))
            .flex()
            .items_center()
            .flex_none()
            .bg(if on { colors.accent } else { colors.border_strong })
            .child(div().size(px(7.)).rounded_full().bg(colors.window));
        if on {
            knob = knob.justify_end();
        }
        div()
            .id("group-by-env")
            .flex_none()
            .flex()
            .items_center()
            .gap(px(7.))
            .px(px(9.))
            .py(px(5.))
            .border_1()
            .border_color(colors.border)
            .rounded(px(6.))
            .bg(colors.panel)
            .cursor_pointer()
            .hover(|s| s.border_color(colors.border_strong))
            .on_click(cx.listener(|this, _event, _window, cx| this.toggle_grouping(cx)))
            .child(knob)
            .child(
                div()
                    .text_size(px(10.))
                    .text_color(if on { colors.text_secondary } else { colors.text_muted })
                    .child("group by env"),
            )
    }

    /// The column headings, on the rows' own grid so they line up.
    fn header_row(&self, colors: &ThemeColors) -> Div {
        let heading = |text: &'static str| {
            div()
                .text_size(px(9.))
                .font_weight(FontWeight::SEMIBOLD)
                .text_color(colors.text_faint)
                .child(text)
        };
        div()
            .flex()
            .items_center()
            .gap(px(12.))
            .px(px(13.))
            .h(px(26.))
            .border_b_1()
            .border_color(colors.border_strong)
            .child(div().w(px(9.)).flex_none())
            .child(div().flex_1().min_w(px(0.)).child(heading("NAME")))
            .child(div().w(px(HOST_WIDTH)).flex_none().child(heading("HOST")))
            .child(div().w(px(STATUS_WIDTH)).flex_none().child(heading("STATUS")))
            .child(div().w(px(MODE_WIDTH)).flex_none().child(heading("MODE")))
            .child(div().w(px(LAST_WIDTH)).flex_none().text_right().child(heading("LAST USED")))
    }

    /// The card's last line: the way to a new connection, and the keys
    /// the list answers to.
    fn footer(&self, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let key = |text: &'static str| {
            div().text_size(px(10.)).text_color(colors.text_faint).child(text)
        };
        div()
            .flex()
            .items_center()
            .gap(px(12.))
            .px(px(13.))
            .py(px(8.))
            .bg(colors.panel)
            .border_t_1()
            .border_color(colors.border)
            .child(
                div()
                    .id("new-connection")
                    .text_size(px(10.))
                    .text_color(colors.accent)
                    .cursor_pointer()
                    .hover(|s| s.text_color(colors.accent_deep))
                    .on_click(cx.listener(|this, _event, window, cx| this.open_form(window, cx)))
                    .child("+ new connection"),
            )
            .child(div().flex_1())
            .child(crate::update::foot_summary(colors, cx))
            .child(key("↑↓ move"))
            .child(key("⏎ connect"))
            .child(key("⌘I edit"))
    }

    /// The search line: a bare field inside a surface of the screen's own,
    /// the way the palette's line is built.
    fn search_line(
        &self,
        colors: &ThemeColors,
        window: &Window,
        cx: &mut Context<Self>,
    ) -> Stateful<Div> {
        let focused = self.search.focus_handle(cx).is_focused(window);
        div()
            .id("search-connections")
            .w(px(SEARCH_WIDTH))
            .flex_none()
            .flex()
            .items_center()
            .gap(px(8.))
            .px(px(10.))
            .py(px(6.))
            .border_1()
            .border_color(if focused { colors.accent } else { colors.border })
            .rounded(px(6.))
            .bg(colors.panel)
            .cursor_pointer()
            // Clicking anywhere on the surface puts the caret in the line.
            .on_click(cx.listener(|this, _event, window, cx| {
                window.focus(&this.search.focus_handle(cx), cx);
                cx.notify();
            }))
            .child(div().text_size(px(11.)).text_color(colors.text_faint).child("⌕"))
            .child(div().flex_1().min_w(px(0.)).child(self.search.clone()))
            .child(div().text_size(px(10.)).text_color(colors.text_faint).child("⌘F"))
    }

    fn row(&self, row: &Row, colors: &ThemeColors, cx: &mut Context<Self>) -> Stateful<Div> {
        let failed = matches!(row.state, Probe::Failed(_));
        let id = row.saved.profile.id.clone();
        let selected = self.selected.as_deref() == Some(id.as_str());

        // Failure recolours the row's text, the way the comp does: warm
        // ink over the shared surface, no surface of its own.
        let (name_color, url_color, meta_color, dot) = match &row.state {
            Probe::Ready { .. } => {
                (colors.text, colors.text_muted, colors.text_muted, colors.ok)
            }
            Probe::Idle | Probe::Probing => {
                (colors.text, colors.text_muted, colors.text_muted, colors.idle)
            }
            Probe::Failed(_) => (
                colors.error,
                colors.error_secondary,
                colors.error_secondary,
                colors.error_mark,
            ),
        };

        // A row that is up says nothing here: the name and the URL are
        // what identify a connection, and everything else is noise until
        // something goes wrong.
        let status = match &row.state {
            Probe::Ready { .. } | Probe::Idle => String::new(),
            Probe::Probing => "connecting…".to_string(),
            Probe::Failed(error) => first_line(error),
        };

        let word = mode_word(&row.saved.profile);
        let mode = match &row.state {
            Probe::Ready { server, .. } if !server.is_empty() => {
                format!("{word} · {server}")
            }
            Probe::Ready { .. } => word.to_string(),
            Probe::Idle | Probe::Probing => row
                .saved
                .server
                .clone()
                .map(|server| format!("{word} · {server}"))
                .unwrap_or_else(|| word.to_string()),
            // The failure itself is in the status column; the mode of a
            // row that is down is not worth a word.
            Probe::Failed(_) => String::new(),
        };

        let last = match row.saved.last_opened {
            Some(at) => ago(self.now - at),
            None => "—".to_string(),
        };

        let mut card = div()
            .id(ElementId::Name(format!("connection-{id}").into()))
            .relative()
            .flex()
            .items_center()
            .gap(px(12.))
            .px(px(13.))
            .h(px(34.))
            .border_b_1()
            .border_color(colors.hairline)
            .cursor_pointer()
            // A click selects; only a double click opens. The bar over
            // the list is where a single click's actions live.
            .on_click(cx.listener(move |this, event: &gpui::ClickEvent, _window, cx| {
                if event.click_count() >= 2 {
                    this.connect(&id, cx);
                } else {
                    this.select(&id, cx);
                }
            }));

        if selected {
            card = card.bg(colors.selection).child(
                // The comp's accent rail: painted over the row's left
                // edge, so the columns keep their alignment.
                div().absolute().left_0().top_0().bottom_0().w(px(2.)).bg(colors.accent),
            );
        } else {
            card = card.hover(|s| s.bg(colors.panel));
        }

        card.child(div().w(px(9.)).flex_none().child(status_dot_of(dot)))
            .child(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .text_size(px(12.))
                    .font_weight(if selected { FontWeight::MEDIUM } else { FontWeight::NORMAL })
                    .text_color(name_color)
                    .truncate()
                    .child(row.saved.profile.name.clone()),
            )
            .child(
                div()
                    .w(px(HOST_WIDTH))
                    .flex_none()
                    .text_size(px(11.))
                    .text_color(url_color)
                    .truncate()
                    .child(profile_url(&row.saved.profile)),
            )
            .child(
                div()
                    .w(px(STATUS_WIDTH))
                    .flex_none()
                    .text_size(px(11.))
                    .text_color(meta_color)
                    .truncate()
                    .child(status),
            )
            .child(
                div()
                    .w(px(MODE_WIDTH))
                    .flex_none()
                    .text_size(px(11.))
                    // The comp marks a guarded connection in the accent and
                    // leaves an open one in plain ink, so the column can be
                    // read down for the rows that are held back.
                    .text_color(match (failed, row.saved.profile.read_only) {
                        (true, _) => colors.error_faint,
                        (false, true) => colors.accent_deep,
                        (false, false) => colors.text_secondary,
                    })
                    .truncate()
                    .child(mode),
            )
            .child(
                div()
                    .w(px(LAST_WIDTH))
                    .flex_none()
                    .text_size(px(10.))
                    .text_color(colors.text_faint)
                    .text_right()
                    .truncate()
                    .child(last),
            )
    }

    /// One environment chip in the form: a dot in the environment's own
    /// color and its name. The selected chip wears the environment's
    /// wash; clicking it again takes the tag away.
    fn env_chip(
        &self,
        env: Env,
        selected: bool,
        colors: &ThemeColors,
        cx: &mut Context<Self>,
    ) -> Stateful<Div> {
        let chip = div()
            .id(ElementId::Name(format!("env-{}", env.as_str()).into()))
            .flex_1()
            .flex()
            .items_center()
            .justify_center()
            .gap(px(7.))
            .py(px(7.))
            .border_1()
            .rounded(px(7.))
            .cursor_pointer()
            .text_size(px(11.))
            .on_click(cx.listener(move |this, _event, _window, cx| this.set_form_env(env, cx)))
            .child(status_dot(env.ring(colors)))
            .child(env.as_str());
        if selected {
            chip.border_color(env.ring(colors))
                .bg(env.surface(colors))
                .font_weight(FontWeight::MEDIUM)
                .text_color(env.text(colors))
        } else {
            chip.border_color(colors.border)
                .bg(colors.elevated)
                .text_color(colors.text_muted)
                .hover(|s| s.border_color(colors.text_faint))
        }
    }

    /// The comp's "safety & limits" block: the read-only switch.
    ///
    /// The switch is not a label on the row — it is a connection parameter,
    /// and the driver asks the server for a read-only session, so
    /// `DROP TABLE` comes back as an error from Postgres rather than from a
    /// guess about what the SQL meant.
    ///
    /// The note under it says what the flag does *not* cover, because a
    /// guardrail that is trusted further than it reaches is worse than
    /// none: the setting is the session's default, and a statement is free
    /// to turn it off for itself. Only a role without write rights closes
    /// that door.
    ///
    /// Who ends a transaction is **not** asked here. A transaction lives on
    /// one connection and a tab is one connection, so the mode is the tab's
    /// and the query toolbar is where it is switched: a connection-wide
    /// setting would open every tab of that database holding a transaction,
    /// for a user who wanted one.
    fn safety_section(&self, form: &Form, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let on = form.read_only;
        div()
            .flex()
            .flex_col()
            .gap(px(9.))
            .pt(px(12.))
            .border_t_1()
            .border_color(colors.border)
            .child(section_label("SAFETY", cx))
            .child(
                div()
                    .id("read-only-session")
                    .flex()
                    .items_center()
                    .gap(px(10.))
                    .cursor_pointer()
                    .on_click(cx.listener(|this, _event, _window, cx| {
                        this.toggle_form_read_only(cx)
                    }))
                    .child(switch(on, cx))
                    .child(
                        div()
                            .flex_1()
                            .min_w(px(0.))
                            .flex()
                            .flex_col()
                            .gap(px(3.))
                            .child(
                                div()
                                    .text_size(px(11.))
                                    .text_color(colors.text)
                                    .child("read-only session"),
                            )
                            .child(
                                div()
                                    .text_size(px(9.))
                                    .text_color(colors.text_faint)
                                    .child(if on {
                                        "Blocks insert, update, delete and DDL. The server refuses them, not the app."
                                    } else {
                                        "Every statement you type can change the database. Nothing asks twice."
                                    }),
                            ),
                    ),
            )
    }

    /// The form's own probe button, at the end of the URL field. It says
    /// what it is doing on its own face: a button whose only answer is
    /// three lines down is a button nobody watches.
    ///
    /// **Two things move, and neither of them is a pixel.** The word fades
    /// in when it changes, because a label that swaps in one frame reads as
    /// a flicker on the very element the pointer is on — the width is
    /// already held, so the fade is all that is left to say the word
    /// changed. And while the test is out the fill breathes, which is the
    /// run button's gesture for "working", at the run button's own pace.
    ///
    /// The word is a child of its own so it can be animated apart from the
    /// box: GPUI gives one animation to one element, and the box is busy
    /// breathing.
    fn test_button(&self, form: &Form, colors: &ThemeColors, cx: &mut Context<Self>) -> AnyElement {
        let testing = matches!(form.test, FormTest::Testing);
        let ink = colors.text_secondary;
        let resting = colors.elevated;
        let word = div()
            .flex_none()
            .child(if testing { "testing…" } else { "test" })
            .with_animation(
                // The id carries which word it is, so the fade replays on
                // the change and holds still through every other rebuild.
                ("test-word", testing as u64),
                Animation::new(FADE_IN).with_easing(gpui::ease_in_out),
                move |word, delta| {
                    let alpha = WORD_FADE_FLOOR + (1. - WORD_FADE_FLOOR) * delta;
                    word.text_color(ink.opacity(alpha))
                },
            );
        let button = toolbar_button_bare(cx)
            .id("test-connection")
            .flex_none()
            // Fixed room for the longer word: see `TEST_WIDTH`. The
            // horizontal padding the button carries is inside it, so the
            // word is centred rather than pushed off one end.
            .w(px(TEST_WIDTH))
            .flex()
            .justify_center()
            .text_size(px(10.))
            .child(word)
            .on_click(cx.listener(|this, _event, _window, cx| this.test_form(cx)));
        if !testing {
            return button.into_any_element();
        }
        button
            .with_animation(
                // Phase-locked to the app's clock, so a keystroke elsewhere
                // in the form does not start the breath over.
                "test-breath",
                Animation::new(TEST_BREATH)
                    .repeat_synced()
                    .with_easing(gpui::pulsating_between(0., 1.)),
                move |button, delta| {
                    button.bg(resting.blend(ink.opacity(TEST_BREATH_DEPTH * delta)))
                },
            )
            .into_any_element()
    }

    /// The form's message, under the URL and the button that tests it:
    /// what the last test found, or what went wrong on a save.
    ///
    /// The room is **always taken**, whether or not there is anything to
    /// say. A message that appeared with its answer would push every block
    /// under it down at the moment the user is reading it, which is the
    /// same shift `TEST_WIDTH` takes out of the button — and here it moves
    /// half the card rather than one field. It is two lines high, so a
    /// server's own refusal reads whole rather than cut at the pane.
    ///
    /// One place serves both, because only one of them is ever worth
    /// reading: an error is set by the very press that would have tested or
    /// saved, and a keystroke clears them together.
    ///
    /// The message **fades in** rather than appearing. The room under the
    /// URL is held whether or not anything is in it, so the fade is the
    /// whole of what says something arrived — and it arrives a round trip
    /// after the press that asked for it, which is exactly when a change
    /// nobody saw happen is a change nobody reads.
    fn form_message(&self, form: &Form, colors: &ThemeColors) -> AnyElement {
        let (color, text) = match (&form.error, &form.test) {
            (Some(error), _) => (colors.error, first_line(error)),
            (None, FormTest::Failed(error)) => (colors.error, first_line(error)),
            // A server that would not say its version still answered.
            (None, FormTest::Ok(server)) if server.is_empty() => {
                (colors.ok, "connected".to_string())
            }
            (None, FormTest::Ok(server)) => (colors.ok, format!("connected · {server}")),
            // Nothing while a test is out: the button already says so.
            (None, FormTest::Idle | FormTest::Testing) => (colors.text_faint, String::new()),
        };
        let line = div()
            .h(px(MESSAGE_HEIGHT))
            .text_size(px(10.))
            .text_color(color)
            .child(text);
        // A form that has said nothing has nothing to fade in, and the
        // first paint of every form is exactly that.
        if form.messages == 0 {
            return line.into_any_element();
        }
        line.with_animation(
            ("form-message", form.messages),
            Animation::new(FADE_IN).with_easing(gpui::ease_in_out),
            move |line, delta| line.text_color(color.opacity(delta)),
        )
        .into_any_element()
    }

    fn form_card(&self, form: &Form, colors: &ThemeColors, cx: &mut Context<Self>) -> Div {
        let label = |text: &'static str| {
            div().text_size(px(10.)).text_color(colors.text_muted).child(text)
        };
        let editing = form.editing.is_some();

        div()
            .flex()
            .flex_col()
            .gap(px(10.))
            .p(px(16.))
            .border_1()
            .border_color(colors.border_strong)
            .rounded(px(8.))
            .bg(colors.panel)
            .child(section_label(
                if editing { "EDIT CONNECTION" } else { "NEW CONNECTION" },
                cx,
            ))
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
                            .child(
                                // "test" sits at the end of the URL it
                                // tests, so the question and the button
                                // that answers it read as one line.
                                div()
                                    .flex()
                                    .items_center()
                                    .gap(px(8.))
                                    .child(div().flex_1().min_w(px(0.)).child(form.url.clone()))
                                    .child(self.test_button(form, colors, cx)),
                            ),
                    ),
            )
            .child(self.form_message(form, colors))
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
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(5.))
                    .child(label("environment tag"))
                    .child(div().flex().gap(px(8.)).children(
                        Env::ALL.map(|env| self.env_chip(env, form.env == Some(env), colors, cx)),
                    ))
                    .child(
                        div().text_size(px(10.)).text_color(colors.text_faint).child(
                            // The note names the frame the tag brings, so
                            // the frame never has to explain itself.
                            match form.env {
                                Some(env) => env.form_note(),
                                None => "An untagged connection wears no frame.",
                            },
                        ),
                    ),
            )
            .child(self.safety_section(form, colors, cx))
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
                            // Say where the password goes before it is typed,
                            // and on an edit, what leaving it empty means.
                            .child(if editing {
                                "an empty password keeps the saved one; a typed one replaces it in the OS keychain"
                            } else {
                                "the password goes to the OS keychain, not to the profiles file"
                            }),
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
                .child(line("close tab", "⌘W"))
                .child(line("run query", "⌘⏎"))
                .child(line("stop / terminate a run", "⌘."))
                .child(line("refresh", "⌘R"))
                .child(line("page back / forward", "⌘[ ⌘]")),
        )
}

/// The comp's dot, one size up: on its own row it needs the extra pixel.
fn status_dot_of(color: gpui::Hsla) -> Div {
    status_dot(color).size(px(9.))
}

/// What the row and the action bar call this connection's mode. The word
/// is the same one the shell's mark carries, so the two screens agree.
fn mode_word(profile: &Profile) -> &'static str {
    if profile.read_only { "read-only" } else { "read-write" }
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

/// Group the rows the search left by their environment tag, in the
/// form's own order — prod, staging, dev — with the untagged rows
/// closing the list: a tag was given to stand out, so an untagged row
/// must not stand first.
fn group_by_env(rows: Vec<&Row>) -> Vec<(Option<Env>, Vec<&Row>)> {
    let mut groups: Vec<(Option<Env>, Vec<&Row>)> = Vec::new();
    for row in rows {
        let env = Env::parse(row.saved.env.as_deref());
        match groups.iter_mut().find(|(key, _)| *key == env) {
            Some((_, list)) => list.push(row),
            None => groups.push((env, vec![row])),
        }
    }
    groups.sort_by_key(|(key, _)| match key {
        Some(Env::Prod) => 0,
        Some(Env::Staging) => 1,
        Some(Env::Dev) => 2,
        None => 3,
    });
    groups
}

/// Does this connection answer the search line? `query` must already be
/// lowercased, as `matching` hands it over.
///
/// The haystack is the name, the URL the row paints, the user and the
/// environment tag, so a port ("5433"), a host, a database name, the
/// name the user gave the connection or "prod" all find it. Every word
/// must hit, so a second word narrows the list rather than widening it.
fn matches_query(saved: &SavedConnection, query: &str) -> bool {
    if query.is_empty() {
        return true;
    }
    let profile = &saved.profile;
    let haystack = format!(
        "{} {} {} {}",
        profile.name,
        profile_url(profile),
        profile.user.clone().unwrap_or_default(),
        saved.env.clone().unwrap_or_default()
    )
    .to_ascii_lowercase();
    query.split_whitespace().all(|word| haystack.contains(word))
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
    fn a_row_url_carries_no_credentials() {
        let profile = Profile {
            id: "p1".into(),
            name: "prod".into(),
            engine: Engine::Postgres,
            host: Some("db.internal".into()),
            port: Some(5432),
            database: "meerkat".into(),
            user: Some("ada".into()),
            read_only: true,
        };
        assert_eq!(profile_url(&profile), "postgres://db.internal:5432/meerkat");
    }

    #[test]
    fn the_search_line_matches_the_name_the_url_and_the_user() {
        let saved = saved_connection("prod", Some("db.internal"), Some(5432), "meerkat", "ada");

        // The name, the host, the port, the database and the user.
        for query in ["prod", "db.internal", "5432", "meerkat", "ada", "postgres://"] {
            assert!(matches_query(&saved, query), "{query} should match");
        }
        // An empty line hides nothing.
        assert!(matches_query(&saved, ""));
        assert!(!matches_query(&saved, "staging"));
    }

    #[test]
    fn every_word_of_the_search_must_hit() {
        let prod = saved_connection("prod", Some("db.internal"), Some(5432), "meerkat", "ada");
        let staging = saved_connection("staging", Some("db.internal"), Some(5433), "meerkat", "ada");

        // A second word narrows: both rows are on db.internal, only one
        // answers to the port as well.
        assert!(matches_query(&prod, "db.internal 5432"));
        assert!(!matches_query(&staging, "db.internal 5432"));
    }

    fn saved_connection(
        name: &str,
        host: Option<&str>,
        port: Option<u16>,
        database: &str,
        user: &str,
    ) -> SavedConnection {
        SavedConnection {
            profile: Profile {
                id: name.into(),
                name: name.into(),
                engine: Engine::Postgres,
                host: host.map(str::to_string),
                port,
                database: database.into(),
                user: Some(user.into()),
                read_only: true,
            },
            last_opened: None,
            server: None,
            env: None,
        }
    }

    #[test]
    fn the_search_line_matches_the_environment_tag() {
        let mut saved = saved_connection("api", Some("db.internal"), Some(5432), "meerkat", "ada");
        saved.env = Some("prod".into());
        assert!(matches_query(&saved, "prod"));
        assert!(!matches_query(&saved, "staging"));
    }

    #[test]
    fn groups_follow_the_forms_env_order_and_untagged_close_the_list() {
        let rows: Vec<Row> = [
            ("scratch", None),
            ("local", Some("dev")),
            ("api", Some("Prod")),
            ("replica", Some("staging")),
            ("billing", Some("prod")),
        ]
        .into_iter()
        .map(|(name, env)| {
            let mut saved = saved_connection(name, None, None, "db", "ada");
            saved.env = env.map(str::to_string);
            Row { saved, state: Probe::Idle }
        })
        .collect();

        let groups = group_by_env(rows.iter().collect());
        let keys: Vec<Option<Env>> = groups.iter().map(|(key, _)| *key).collect();
        // The groups stand in the form's order whatever the store order
        // was, "Prod" and "prod" are one group, and untagged is last
        // even though the store listed it first.
        assert_eq!(keys, [Some(Env::Prod), Some(Env::Staging), Some(Env::Dev), None]);
        let prod: Vec<&str> =
            groups[0].1.iter().map(|row| row.saved.profile.name.as_str()).collect();
        assert_eq!(prod, ["api", "billing"]);
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
            read_only: true,
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
