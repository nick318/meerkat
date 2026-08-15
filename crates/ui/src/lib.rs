//! Small component kit on top of GPUI, modeled on Zed's `ui` crate.
//! Implements the warm-paper design language: hairline rules, bordered
//! pills, one ochre accent. Grows as Meerkat needs more controls.

use gpui::{
    App, Div, FontWeight, InteractiveElement as _, ParentElement as _, SharedString, Styled as _,
    div, px,
};
use theme::theme;

/// A bordered card surface sitting on a panel (connection card, inputs).
pub fn card(cx: &App) -> Div {
    let colors = &theme(cx).colors;
    div()
        .bg(colors.elevated)
        .border_1()
        .border_color(colors.border_strong)
        .rounded(px(7.))
}

/// A small bordered toolbar button ("filter", "export", "format").
pub fn toolbar_button(text: impl Into<SharedString>, cx: &App) -> Div {
    let colors = &theme(cx).colors;
    div()
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
        .child(text.into())
}

/// The single filled accent button ("run", "commit").
pub fn accent_button(text: impl Into<SharedString>, cx: &App) -> Div {
    let colors = &theme(cx).colors;
    div()
        .px(px(10.))
        .py(px(5.))
        .rounded(px(6.))
        .bg(colors.accent)
        .text_size(px(11.))
        .font_weight(FontWeight::MEDIUM)
        .text_color(colors.window)
        .cursor_pointer()
        .hover(|s| s.bg(colors.accent_deep))
        .child(text.into())
}

/// An uppercase micro section label ("SCHEMA · PUBLIC", "CONNECTIONS").
pub fn section_label(text: impl Into<SharedString>, cx: &App) -> Div {
    let colors = &theme(cx).colors;
    div()
        .text_size(px(9.))
        .font_weight(FontWeight::SEMIBOLD)
        .text_color(colors.text_faint)
        .child(text.into())
}

/// A small status dot (green = connected, sand = idle).
pub fn status_dot(color: gpui::Hsla) -> Div {
    div().size(px(6.)).rounded_full().bg(color)
}

/// A tiny square glyph marking a table row in lists (accent when active).
pub fn table_glyph(active: bool, cx: &App) -> Div {
    let colors = &theme(cx).colors;
    div().size(px(5.)).bg(if active {
        colors.accent
    } else {
        colors.text_faint
    })
}
