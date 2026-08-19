//! Meerkat entry point: boot GPUI and tokio, load bundled fonts, install
//! the warm-paper theme, open the main window on a database.
//!
//! Usage: `meerkat postgres://user@host/db`, or set `MEERKAT_DATABASE_URL`.
//! With neither, the window opens on the connections screen, which lists
//! the saved connections and takes new ones.

mod connections;
mod env;
mod history;
mod palette;
mod root;
mod shell;
mod sql;

use gpui::{
    App, Bounds, Focusable as _, KeyBinding, TitlebarOptions, WindowBounds, WindowOptions, actions,
    prelude::*, px, size,
};
use gpui_platform::application;
use root::Root;
use shell::Target;
use std::borrow::Cow;
use theme::Theme;

actions!(meerkat, [Quit]);

fn main() {
    let target = database_url().map(Target::Url);

    application().run(move |cx: &mut App| {
        // Before anything else: the drivers run on tokio, and every view
        // reaches the database through this runtime.
        gpui_tokio::init(cx);

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
        cx.bind_keys(connections::key_bindings());
        cx.bind_keys(palette::key_bindings());
        cx.bind_keys(shell::confirm_key_bindings());
        // The workspace's keys are scoped to the workspace, not bound
        // globally, so the palette can take ⌘⏎ for itself while it is
        // open: GPUI gives a keystroke to the binding that matched deepest
        // in the context stack, and the palette sits inside the shell.
        cx.bind_keys([
            KeyBinding::new("cmd-enter", shell::RunQuery, Some("Shell")),
            // ⌘. stops the run the way it does in psql's siblings: once to
            // ask the statement to give up, again to close the backend.
            KeyBinding::new("cmd-.", shell::StopQuery, Some("Shell")),
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
            KeyBinding::new("shift-down", shell::ExtendDown, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("shift-left", shell::ExtendLeft, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("shift-right", shell::ExtendRight, Some(shell::GRID_KEY_CONTEXT)),
            // ⌘ with an arrow means "as far as it goes" on this platform,
            // and it means the same here.
            KeyBinding::new("cmd-left", shell::SelectRowStart, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("cmd-right", shell::SelectRowEnd, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("cmd-up", shell::SelectFirstRow, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("cmd-down", shell::SelectLastRow, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("cmd-a", shell::SelectAll, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("cmd-c", shell::CopySelection, Some(shell::GRID_KEY_CONTEXT)),
            // Space ticks the row the cursor is on: ↓ then space walks a
            // result and picks out of it without the mouse.
            KeyBinding::new("space", shell::TogglePick, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("escape", shell::ClearSelection, Some(shell::GRID_KEY_CONTEXT)),
            KeyBinding::new("cmd-q", Quit, None),
        ]);

        let bounds = Bounds::centered(None, size(px(1360.), px(880.)), cx);
        let window = cx
            .open_window(
                WindowOptions {
                    window_bounds: Some(WindowBounds::Windowed(bounds)),
                    titlebar: Some(TitlebarOptions {
                        title: Some("Meerkat".into()),
                        ..Default::default()
                    }),
                    ..Default::default()
                },
                |window, cx| cx.new(|cx| Root::new(target.clone(), window, cx)),
            )
            .expect("failed to open the main window");
        let window_handle = window;

        // A session's tabs are written back as they change, but the last
        // keystrokes in an editor are not: ⌘Q and the window's close
        // button both drop the shell, so each one saves on the way out.
        //
        // Both also end every run at once, so both ask first. `guard_quit`
        // answering `false` means the workspace has put the question on
        // screen and will quit itself if the user agrees — so this returns
        // and does not.
        cx.on_action(move |_: &Quit, cx: &mut App| {
            let go = window
                .update(cx, |root, window, cx| root.guard_quit(window, cx))
                .unwrap_or(true);
            if !go {
                return;
            }
            window.update(cx, |root, _window, cx| root.remember(cx)).ok();
            cx.quit();
        });

        // The screen must hold focus from the first frame, or the key
        // bindings above have nowhere to dispatch.
        window
            .update(cx, |root, window, cx| {
                window.focus(&root.focus_handle(cx), cx);
                // The platform wants a yes or no on the spot, and the
                // question takes a person to answer. So a close with runs
                // out answers "no" and puts the dialog up; agreeing to it
                // quits from there.
                window.on_window_should_close(cx, move |_window, cx| {
                    let go = window_handle
                        .update(cx, |root, window, cx| root.guard_quit(window, cx))
                        .unwrap_or(true);
                    if !go {
                        return false;
                    }
                    window_handle.update(cx, |root, _window, cx| root.remember(cx)).ok();
                    true
                });
            })
            .expect("failed to focus the main window");
        cx.activate(true);
    });
}

/// The first non-flag argument wins, then the environment.
fn database_url() -> Option<String> {
    std::env::args()
        .skip(1)
        .find(|arg| !arg.starts_with('-'))
        .or_else(|| std::env::var("MEERKAT_DATABASE_URL").ok())
        .filter(|url| !url.is_empty())
}
