//! Meerkat entry point: boot GPUI, install the theme, open the main window.

use gpui::{
    App, Application, Bounds, Context, Window, WindowBounds, WindowOptions, div, prelude::*, px,
    size,
};
use gpui_platform::application;
use theme::{Theme, theme};
use ui::{panel, placeholder, section_label};

fn main() {
    let app: Application = application();
    app.run(|cx: &mut App| {
        cx.set_global(Theme::dark());

        let bounds = Bounds::centered(None, size(px(1200.), px(760.)), cx);
        cx.open_window(
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(bounds)),
                ..Default::default()
            },
            |_, cx| cx.new(|_| Shell),
        )
        .expect("failed to open the main window");
        cx.activate(true);
    });
}

/// Root view: sidebar (schema tree) | editor + results | status bar.
/// Panes are placeholders until their crates land (Phase 1).
struct Shell;

impl Render for Shell {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();

        div()
            .flex()
            .flex_col()
            .size_full()
            .bg(colors.background)
            .text_color(colors.text)
            .child(
                div()
                    .flex()
                    .flex_1()
                    .gap_2()
                    .p_2()
                    .min_h(px(0.))
                    .child(
                        // Sidebar: connections + schema tree
                        panel(cx)
                            .w(px(260.))
                            .flex()
                            .flex_col()
                            .gap_2()
                            .p_3()
                            .child(section_label("CONNECTIONS", cx))
                            .child(placeholder("No connections yet", cx)),
                    )
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .flex_1()
                            .gap_2()
                            .min_w(px(0.))
                            .child(
                                panel(cx)
                                    .flex_1()
                                    .p_3()
                                    .child(placeholder("SQL editor", cx)),
                            )
                            .child(
                                panel(cx)
                                    .flex_1()
                                    .p_3()
                                    .child(placeholder("Results grid", cx)),
                            ),
                    ),
            )
            .child(
                // Status bar
                div()
                    .flex()
                    .items_center()
                    .h(px(28.))
                    .px_3()
                    .bg(colors.status_bar)
                    .border_t_1()
                    .border_color(colors.border)
                    .text_xs()
                    .text_color(colors.text_muted)
                    .child("meerkat 0.1.0 — not connected"),
            )
    }
}
