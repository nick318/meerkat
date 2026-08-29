//! Meerkat entry point: boot GPUI and tokio, load bundled fonts, install
//! the warm-paper theme, open the first window on a database.
//!
//! Usage: `meerkat postgres://user@host/db`, or set `MEERKAT_DATABASE_URL`.
//! With neither, the window opens on the connections screen, which lists
//! the saved connections and takes new ones.
//!
//! **There can be more than one window, and ⌘N opens one** — on the
//! connections screen, as a browser's new window opens on nothing. A window
//! is a `Root` and nothing else: each one holds its own screen, its own
//! session and its own tabs, so two windows can sit on two databases at
//! once. Nothing is shared between them but the theme, the key bindings and
//! the local SQLite file.
//!
//! The command line names a database for the **first** window only. A second
//! window was asked for by hand, so it opens where the user can choose.

mod connections;
mod env;
mod history;
mod palette;
mod root;
mod shell;
mod sql;
mod update;

use gpui::{
    App, Bounds, Focusable as _, Global, KeyBinding, TitlebarOptions, WindowBounds, WindowOptions,
    actions, prelude::*, px, size,
};
use gpui_platform::application;
use root::Root;
use shell::Target;
use std::borrow::Cow;
use theme::Theme;

actions!(meerkat, [Quit, NewWindow]);

/// How far a new window is offset from the one before it, so it does not
/// land exactly on top of it — a window nobody can see the edge of reads as
/// no new window at all.
const WINDOW_CASCADE: f32 = 28.;

/// How many steps the cascade takes before it starts over. Without it the
/// tenth window walks off the screen.
const WINDOW_CASCADE_STEPS: usize = 6;

fn main() {
    let target = database_url().map(Target::Url);

    application().run(move |cx: &mut App| {
        // Before anything else: the drivers run on tokio, and every view
        // reaches the database through this runtime.
        gpui_tokio::init(cx);

        // The updater exists only on a distributed channel; a `cargo
        // build` is a `local` build and gets none.
        auto_update::init(cx);

        cx.text_system()
            .add_fonts(vec![
                Cow::Borrowed(include_bytes!("../assets/fonts/JetBrainsMono-Regular.ttf")),
                Cow::Borrowed(include_bytes!("../assets/fonts/JetBrainsMono-Medium.ttf")),
                Cow::Borrowed(include_bytes!("../assets/fonts/JetBrainsMono-SemiBold.ttf")),
            ])
            .expect("failed to load bundled fonts");

        cx.set_global(Theme::warm_paper());

        cx.bind_keys(sql_editor::key_bindings());
        cx.bind_keys(ui::text_field_key_bindings());
        cx.bind_keys(root::key_bindings());
        cx.bind_keys(connections::key_bindings());
        cx.bind_keys(palette::key_bindings());
        cx.bind_keys(shell::confirm_key_bindings());
        cx.bind_keys(shell::column_find_key_bindings());
        cx.bind_keys(shell::catalog_filter_key_bindings());
        cx.bind_keys(shell::peek_key_bindings());
        // The workspace's keys are scoped to the workspace, not bound
        // globally, so the palette can take ⌘⏎ for itself while it is
        // open: GPUI gives a keystroke to the binding that matched deepest
        // in the context stack, and the palette sits inside the shell.
        cx.bind_keys([
            KeyBinding::new("cmd-enter", shell::RunQuery, Some("Shell")),
            // ⌘. stops the run the way it does in psql's siblings: once to
            // ask the statement to give up, again to close the backend.
            KeyBinding::new("cmd-.", shell::StopQuery, Some("Shell")),
            // The two ends of a transaction. ⌘S is the comp's own key and
            // means here what it means everywhere: make this permanent.
            //
            // **The rollback key is not the comp's ⇧⌘Z.** That is Redo in
            // the SQL editor, whose context sits *inside* the shell's, so a
            // binding here would never fire while the user is typing — and
            // taking redo off a text editor would be the wrong trade even
            // if it did. ⇧⌘R is free, and it is the letter the word starts
            // with.
            KeyBinding::new("cmd-s", shell::CommitTransaction, Some("Shell")),
            KeyBinding::new("cmd-shift-r", shell::RollbackTransaction, Some("Shell")),
            KeyBinding::new("cmd-t", shell::NewQuery, Some("Shell")),
            KeyBinding::new("cmd-w", shell::CloseTab, Some("Shell")),
            KeyBinding::new("cmd-r", shell::Refresh, Some("Shell")),
            KeyBinding::new("cmd-y", shell::ShowHistory, Some("Shell")),
            KeyBinding::new("cmd-[", shell::PrevPage, Some("Shell")),
            KeyBinding::new("cmd-]", shell::NextPage, Some("Shell")),
            // ⌃⇥ switches on the keystroke, with no popup in between, so
            // both keys are bound to the shell and nowhere else.
            KeyBinding::new("ctrl-tab", shell::NextTab, Some("Shell")),
            KeyBinding::new("ctrl-shift-tab", shell::PrevTab, Some("Shell")),
            // The grid's own keys, in the grid's own context — never the
            // shell's. The SQL editor's element sits *inside* the shell's,
            // so a key bound to the shell is matched before the keystroke
            // can reach the editor's text input: a bare `space` bound there
            // would eat the spaces out of the user's SQL. `ResultGrid` is
            // in the stack only while the grid holds the focus, which a
            // table tab does from the moment it opens and a query tab does
            // from the first click in its result.
            KeyBinding::new("up", shell::SelectUp, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("down", shell::SelectDown, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("left", shell::SelectLeft, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("right", shell::SelectRight, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("shift-up", shell::ExtendUp, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new(
                "shift-down",
                shell::ExtendDown,
                Some(shell::GRID_KEY_CONTEXT),
            ),
            KeyBinding::new(
                "shift-left",
                shell::ExtendLeft,
                Some(shell::GRID_KEY_CONTEXT),
            ),
            KeyBinding::new(
                "shift-right",
                shell::ExtendRight,
                Some(shell::GRID_KEY_CONTEXT),
            ),
            // ⌘ with an arrow means "as far as it goes" on this platform,
            // and it means the same here.
            KeyBinding::new(
                "cmd-left",
                shell::SelectRowStart,
                Some(shell::GRID_KEY_CONTEXT),
            ),
            KeyBinding::new(
                "cmd-right",
                shell::SelectRowEnd,
                Some(shell::GRID_KEY_CONTEXT),
            ),
            KeyBinding::new(
                "cmd-up",
                shell::SelectFirstRow,
                Some(shell::GRID_KEY_CONTEXT),
            ),
            KeyBinding::new(
                "cmd-down",
                shell::SelectLastRow,
                Some(shell::GRID_KEY_CONTEXT),
            ),
            KeyBinding::new("cmd-a", shell::SelectAll, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("cmd-c", shell::CopySelection, Some(shell::GRID_KEY_CONTEXT)),
            // Space ticks the row the cursor is on: ↓ then space walks a
            // result and picks out of it without the mouse.
            KeyBinding::new("space", shell::TogglePick, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new(
                "escape",
                shell::ClearSelection,
                Some(shell::GRID_KEY_CONTEXT),
            ),
            KeyBinding::new("cmd-q", Quit, None),
            // A window, like a browser's ⌘N. Bound globally on purpose:
            // it means the same thing on the connections screen and inside
            // a session, and nothing else in the app takes the key.
            KeyBinding::new("cmd-n", NewWindow, None),
        ]);

        cx.on_action(|_: &Quit, cx: &mut App| quit(cx));
        cx.on_action(|_: &NewWindow, cx: &mut App| {
            // On the connections screen, whatever this window is on: ⌘N
            // asks for a window, not for a second view of this database.
            open_window(None, cx);
        });

        // With no window there is no menu bar and no way back in, so the
        // app has nothing left to be. The handle is called after the window
        // has gone, so an empty list means this was the last one.
        cx.on_window_closed(|cx, _id| {
            if cx.windows().is_empty() {
                cx.quit();
            }
        })
        .detach();

        open_window(target.clone(), cx);
        cx.activate(true);
    });
}

/// Open one window on a target, or on the connections screen with none.
///
/// Every window is built here — the first one and every ⌘N after it — so
/// the guard on the close button and the cascade cannot be true of one
/// window and forgotten on the next.
fn open_window(target: Option<Target>, cx: &mut App) {
    let step = cx.windows().len() % WINDOW_CASCADE_STEPS;
    let mut bounds = Bounds::centered(None, size(px(1360.), px(880.)), cx);
    let offset = px(WINDOW_CASCADE * step as f32);
    bounds.origin.x += offset;
    bounds.origin.y += offset;

    let handle = cx
        .open_window(
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(bounds)),
                titlebar: Some(TitlebarOptions {
                    title: Some("Meerkat".into()),
                    ..Default::default()
                }),
                ..Default::default()
            },
            |window, cx| cx.new(|cx| Root::new(target, window, cx)),
        )
        .expect("failed to open a window");

    // The screen must hold focus from the first frame, or the key bindings
    // have nowhere to dispatch.
    handle
        .update(cx, |root, window, cx| {
            window.focus(&root.focus_handle(cx), cx);
            // The platform wants a yes or no on the spot, and the question
            // takes a person to answer. So a close with runs out answers
            // "no" and puts the dialog up; agreeing to it closes the window
            // from there.
            //
            // A session's tabs are written back as they change, but the last
            // keystrokes in an editor are not, so the way out saves first.
            window.on_window_should_close(cx, move |_window, cx| {
                let go = handle
                    .update(cx, |root, window, cx| root.guard_close_window(window, cx))
                    .unwrap_or(true);
                if !go {
                    return false;
                }
                handle
                    .update(cx, |root, _window, cx| root.remember(cx))
                    .ok();
                true
            });
        })
        .expect("failed to focus a new window");
}

/// Call a quit off, at every window that had already agreed to it.
pub fn quit_cancelled(cx: &mut App) {
    // The windows that agreed before the "stay" agreed to *this* ending,
    // and this ending is not happening — a restart asked for later must
    // not inherit it either.
    cx.set_global(Restarting(false));
    for window in cx.windows() {
        let Some(window) = window.downcast::<Root>() else {
            continue;
        };
        window
            .update(cx, |root, _window, cx| root.forget_quit(cx))
            .ok();
    }
}

/// Quit, if every window agrees.
///
/// ⌘Q ends every window's work at once, so **every** window is asked, and
/// the first one with something at stake puts the question on screen and
/// stops the walk. Agreeing to it starts the walk again — that window then
/// answers yes — so the user is asked once per window and the app goes only
/// when the last of them has said so.
///
/// The cancels are waited for. Quitting drops the tokio runtime, so a
/// `pg_cancel_backend` that has not left yet never leaves, and the statement
/// outlives the app that started it.
pub fn quit(cx: &mut App) {
    let windows: Vec<_> = cx
        .windows()
        .into_iter()
        .filter_map(|window| window.downcast::<Root>())
        .collect();
    for window in &windows {
        let go = window
            .update(cx, |root, window, cx| root.guard_quit(window, cx))
            .unwrap_or(true);
        if !go {
            return;
        }
    }
    let mut stops = Vec::new();
    for window in &windows {
        window
            .update(cx, |root, _window, cx| {
                root.remember(cx);
                stops.extend(root.stop_runs(cx));
            })
            .ok();
    }
    cx.spawn(async move |cx| {
        for stop in stops {
            stop.await.ok();
        }
        cx.update(|cx| {
            // A restart is ⌘Q's walk with a different last word: the same
            // questions were asked, the same cancels were waited for, and
            // only what happens after the last window agrees differs.
            if cx.default_global::<Restarting>().0 {
                cx.restart();
            } else {
                cx.quit();
            }
        });
    })
    .detach();
}

/// Whether the quit under way is really a restart into an installed
/// update. Read once, at the end of the quit walk; a cancelled quit
/// clears it, so the next plain ⌘Q does not relaunch the app.
#[derive(Default)]
struct Restarting(bool);

impl Global for Restarting {}

/// Restart into the update the updater has already laid over the bundle.
/// It is the quit walk end to end — every window is asked about its runs
/// and its transactions first — so nothing ends without the user saying
/// so twice being needed.
pub fn restart_to_update(cx: &mut App) {
    cx.set_global(Restarting(true));
    quit(cx);
}

/// The first non-flag argument wins, then the environment.
fn database_url() -> Option<String> {
    std::env::args()
        .skip(1)
        .find(|arg| !arg.starts_with('-'))
        .or_else(|| std::env::var("MEERKAT_DATABASE_URL").ok())
        .filter(|url| !url.is_empty())
}
