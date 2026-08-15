//! Small component kit on top of GPUI, modeled on Zed's `ui` crate.
//! Implements the warm-paper design language: hairline rules, bordered
//! pills, one ochre accent. Grows as Meerkat needs more controls.

mod text_field;

pub use text_field::{TextField, TextFieldEvent, text_field_key_bindings};

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

/// Compact row counts for the sidebar: `640`, `18.4k`, `1.2m`.
/// Estimates, so three significant figures are plenty.
pub fn format_count(count: u64) -> String {
    match count {
        0..=999 => count.to_string(),
        1_000..=999_999 => trim_zero(count as f64 / 1_000., "k"),
        1_000_000..=999_999_999 => trim_zero(count as f64 / 1_000_000., "m"),
        _ => trim_zero(count as f64 / 1_000_000_000., "b"),
    }
}

fn trim_zero(value: f64, suffix: &str) -> String {
    if value >= 100. || (value.fract() * 10.).round() == 0. {
        format!("{}{suffix}", value.round() as u64)
    } else {
        format!("{value:.1}{suffix}")
    }
}

/// Query timings: `34 ms` up to a second, then `1.9 s`.
pub fn format_millis(millis: u128) -> String {
    if millis < 1_000 {
        format!("{millis} ms")
    } else {
        format!("{:.1} s", millis as f64 / 1_000.)
    }
}

/// The meerkat itself: two ears, a head, two eyes and a muzzle, drawn as
/// plain rectangles so the app carries no image assets. The comp draws it
/// at 42px; every part scales from that.
pub fn meerkat_mark(size: f32, cx: &App) -> Div {
    let colors = &theme(cx).colors;
    // Every offset below is the comp's 42px geometry, in units of the
    // requested size.
    let unit = |value: f32| px(value / 42. * size);
    let ear = |left: bool| {
        let mut ear = div()
            .absolute()
            .top(px(0.))
            .size(unit(13.))
            .rounded_full()
            .bg(colors.mark_ears);
        ear = if left { ear.left(unit(2.)) } else { ear.right(unit(2.)) };
        ear
    };
    let eye = |offset: f32| {
        div()
            .absolute()
            .left(unit(offset))
            .top(unit(18.))
            .size(unit(5.))
            .rounded_full()
            .bg(colors.mark_ink)
    };

    div()
        .relative()
        .flex_none()
        .w(px(size))
        .h(px(size))
        .child(ear(true))
        .child(ear(false))
        .child(
            div()
                .absolute()
                .left(unit(4.))
                .top(unit(7.))
                .size(unit(34.))
                .rounded_full()
                .bg(colors.mark_face),
        )
        .child(eye(12.))
        .child(eye(25.))
        .child(
            div()
                .absolute()
                .left(unit(18.))
                .top(unit(27.))
                .w(unit(7.))
                .h(unit(5.))
                .rounded_full()
                .bg(colors.mark_muzzle),
        )
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn counts_read_compactly() {
        assert_eq!(format_count(0), "0");
        assert_eq!(format_count(640), "640");
        assert_eq!(format_count(18_412), "18.4k");
        assert_eq!(format_count(311_000), "311k");
        // A round thousand reads better without the trailing zero.
        assert_eq!(format_count(5_000), "5k");
        assert_eq!(format_count(20_000), "20k");
        assert_eq!(format_count(1_200_000), "1.2m");
        assert_eq!(format_count(2_400_000_000), "2.4b");
    }

    #[test]
    fn timings_switch_to_seconds() {
        assert_eq!(format_millis(34), "34 ms");
        assert_eq!(format_millis(999), "999 ms");
        assert_eq!(format_millis(1_900), "1.9 s");
    }
}
