//! Meerkat entry point: boot GPUI and tokio, load bundled fonts, install
//! the warm-paper theme, open the main window on a database.
//!
//! Usage: `meerkat postgres://user@host/db`, or set `MEERKAT_DATABASE_URL`.
//! A saved-connection screen comes later; one URL is enough to browse.

mod shell;
mod sql;

use gpui::{
    App, Bounds, Focusable as _, KeyBinding, TitlebarOptions, WindowBounds, WindowOptions, actions,
    prelude::*, px, size,
};
use gpui_platform::application;
use shell::Shell;
use std::borrow::Cow;
use theme::Theme;

actions!(meerkat, [Quit]);

const USAGE: &str = "usage: meerkat postgres://user@host:5432/database\n\
                     (or set MEERKAT_DATABASE_URL)";

fn main() {
    let Some(url) = database_url() else {
        eprintln!("{USAGE}");
        std::process::exit(2);
    };

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
        cx.bind_keys([
            KeyBinding::new("cmd-enter", shell::RunQuery, None),
            KeyBinding::new("cmd-t", shell::NewQuery, None),
            KeyBinding::new("cmd-r", shell::Refresh, None),
            KeyBinding::new("cmd-[", shell::PrevPage, None),
            KeyBinding::new("cmd-]", shell::NextPage, None),
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
                |_, cx| cx.new(|cx| Shell::new(url.clone(), cx)),
            )
            .expect("failed to open the main window");

        // The shell must hold focus from the first frame, or the key
        // bindings above have nowhere to dispatch.
        window
            .update(cx, |shell, window, cx| {
                window.focus(&shell.focus_handle(cx), cx);
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
