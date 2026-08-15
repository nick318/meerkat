//! Meerkat entry point: boot GPUI, load bundled fonts, install the
//! warm-paper theme, open the main window.

mod shell;

use gpui::{App, Bounds, TitlebarOptions, WindowBounds, WindowOptions, prelude::*, px, size};
use gpui_platform::application;
use shell::Shell;
use std::borrow::Cow;
use theme::Theme;

fn main() {
    application().run(|cx: &mut App| {
        cx.text_system()
            .add_fonts(vec![
                Cow::Borrowed(include_bytes!("../assets/fonts/JetBrainsMono-Regular.ttf")),
                Cow::Borrowed(include_bytes!("../assets/fonts/JetBrainsMono-Medium.ttf")),
                Cow::Borrowed(include_bytes!("../assets/fonts/JetBrainsMono-SemiBold.ttf")),
            ])
            .expect("failed to load bundled fonts");

        cx.set_global(Theme::warm_paper());

        let bounds = Bounds::centered(None, size(px(1360.), px(880.)), cx);
        cx.open_window(
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(bounds)),
                titlebar: Some(TitlebarOptions {
                    title: Some("Meerkat".into()),
                    ..Default::default()
                }),
                ..Default::default()
            },
            |_, cx| cx.new(|_| Shell::new()),
        )
        .expect("failed to open the main window");
        cx.activate(true);
    });
}
