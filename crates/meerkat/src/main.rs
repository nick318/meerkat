//! Meerkat entry point: boot GPUI and tokio, load bundled fonts, install
//! the warm-paper theme, open the main window on a database.
//!
//! Usage: `meerkat postgres://user@host/db`, or set `MEERKAT_DATABASE_URL`.
//! With neither, the window opens on the connections screen, which lists
//! the saved connections and takes new ones.

mod connections;
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
        // The workspace's keys are scoped to the workspace, not bound
        // globally, so the palette can take ⌘⏎ for itself while it is
        // open: GPUI gives a keystroke to the binding that matched deepest
        // in the context stack, and the palette sits inside the shell.
        cx.bind_keys([
            KeyBinding::new("cmd-enter", shell::RunQuery, Some("Shell")),
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
            KeyBinding::new("cmd-q", Quit, None),
        ]);
        cx.on_action(|_: &Quit, cx: &mut App| cx.quit());

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

        // The screen must hold focus from the first frame, or the key
        // bindings above have nowhere to dispatch.
        window
            .update(cx, |root, window, cx| {
                window.focus(&root.focus_handle(cx), cx);
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
