//! Small component kit on top of GPUI, modeled on Zed's `ui` crate.
//! Grows as Meerkat needs more controls.

use gpui::{App, Div, ParentElement as _, Styled as _, div, px};
use theme::theme;

/// A bordered panel surface (sidebar, editor pane, results pane).
pub fn panel(cx: &App) -> Div {
    let colors = &theme(cx).colors;
    div()
        .bg(colors.surface)
        .border_1()
        .border_color(colors.border)
        .rounded_md()
}

/// An uppercase section label, like the schema-tree headers.
pub fn section_label(text: impl Into<gpui::SharedString>, cx: &App) -> Div {
    let colors = &theme(cx).colors;
    div()
        .text_xs()
        .text_color(colors.text_muted)
        .child(text.into())
}

/// A muted placeholder line for panes with no content yet.
pub fn placeholder(text: impl Into<gpui::SharedString>, cx: &App) -> Div {
    let colors = &theme(cx).colors;
    div()
        .flex()
        .items_center()
        .justify_center()
        .size_full()
        .text_sm()
        .text_color(colors.text_muted)
        .min_h(px(0.))
        .child(text.into())
}
