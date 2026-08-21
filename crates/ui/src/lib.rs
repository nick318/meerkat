//! Small component kit on top of GPUI, modeled on Zed's `ui` crate.
//! Implements the warm-paper design language: hairline rules, bordered
//! pills, one ochre accent. Grows as Meerkat needs more controls.

pub mod scrollbar;
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
    toolbar_button_bare(cx).child(text.into())
}

/// The same button with no word in it, for a button whose label is an
/// element of its own: a word that fades as it changes has to be animated
/// apart from the box around it, and GPUI gives one animation to one
/// element.
pub fn toolbar_button_bare(cx: &App) -> Div {
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

/// A timing that came back as a fraction of a millisecond, which is what
/// the server reports its own work as.
///
/// Under ten milliseconds the fraction is most of the number — `0.4 ms` and
/// `4.4 ms` both round to the same useless answer — so one decimal is kept
/// there and dropped above it, where it says nothing.
pub fn format_millis_frac(millis: f64) -> String {
    if millis < 10. {
        format!("{millis:.1} ms")
    } else {
        format_millis(millis.round() as u128)
    }
}

/// A clock that is still running: always seconds with one decimal, from
/// `0.1 s` up. Unlike `format_millis` it never switches units, because a
/// timer that changed shape as it passed a second would read as a glitch.
pub fn format_seconds(millis: u128) -> String {
    format!("{:.1} s", millis as f64 / 1_000.)
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

/// The padlock on the session's read-only mark: a body with a shackle
/// over it, drawn as rectangles like the meerkat, because the app carries
/// no image assets. The comp draws it 8 wide by 9 tall beside a 10px
/// label.
///
/// The shackle is a bordered box whose bottom edge is covered by the body
/// that overlaps it, which is why nothing here has to paint three sides of
/// a border: GPUI's border widths come in whole pixels, and a 1px ring on
/// a 5px box is the whole drawing.
pub fn lock_glyph(color: gpui::Hsla) -> Div {
    div()
        .relative()
        .flex_none()
        .w(px(8.))
        .h(px(9.))
        .child(
            div()
                .absolute()
                .left(px(1.5))
                .top(px(0.))
                .w(px(5.))
                .h(px(5.))
                .border_1()
                .border_color(color)
                .rounded_t(px(3.)),
        )
        .child(
            div()
                .absolute()
                .left(px(0.))
                .bottom(px(0.))
                .w(px(8.))
                .h(px(5.5))
                .rounded(px(1.5))
                .bg(color),
        )
}

/// The play triangle on the run button. GPUI's borders come in whole
/// pixels on all four sides, so the CSS trick of a zero-sized box with one
/// coloured border does not translate; this fills a real three-point path
/// instead, which is also the only shape in the app that needs one.
pub fn play_glyph(color: gpui::Hsla) -> impl gpui::IntoElement {
    gpui::canvas(
        |_bounds, _window, _cx| (),
        move |bounds, _state, window, _cx| {
            let mut path = gpui::Path::new(bounds.origin);
            path.line_to(bounds.origin + gpui::point(px(0.), bounds.size.height));
            path.line_to(
                bounds.origin + gpui::point(bounds.size.width, bounds.size.height / 2.),
            );
            window.paint_path(path, color);
        },
    )
    .w(px(8.))
    .h(px(10.))
    .flex_none()
}

/// The magnifier: on the "column" button, and in the search line the
/// button opens.
///
/// The lens is a bordered circle, the way every other glyph here is drawn
/// from boxes. The handle cannot be: GPUI's borders come in whole pixels on
/// all four sides and a box cannot be turned, so the one diagonal in the
/// app is a filled path, as the play triangle is.
pub fn search_glyph(color: gpui::Hsla) -> Div {
    div()
        .relative()
        .flex_none()
        .w(px(11.))
        .h(px(11.))
        .child(
            div()
                .absolute()
                .left(px(0.))
                .top(px(0.))
                .size(px(8.))
                .rounded_full()
                .border_1()
                .border_color(color),
        )
        .child(div().absolute().left(px(6.)).top(px(6.)).child(lens_handle(color)))
}

/// The magnifier's handle: a stroke from one corner of a small box to the
/// other, as a four-point path, because a line has to have a width.
fn lens_handle(color: gpui::Hsla) -> impl gpui::IntoElement {
    const SIZE: f32 = 5.;
    const WIDTH: f32 = 1.3;
    gpui::canvas(
        |_bounds, _window, _cx| (),
        move |bounds, _state, window, _cx| {
            let corner = |x: f32, y: f32| bounds.origin + gpui::point(px(x), px(y));
            let mut path = gpui::Path::new(corner(0., WIDTH));
            path.line_to(corner(WIDTH, 0.));
            path.line_to(corner(SIZE, SIZE - WIDTH));
            path.line_to(corner(SIZE - WIDTH, SIZE));
            window.paint_path(path, color);
        },
    )
    .w(px(SIZE))
    .h(px(SIZE))
    .flex_none()
}

/// The stop square, which the run button wears while a statement is out.
pub fn stop_glyph(color: gpui::Hsla) -> Div {
    div().size(px(9.)).flex_none().rounded(px(1.5)).bg(color)
}

/// The comp's switch: a track with the knob at whichever end the state is.
/// `on` fills the track with the accent, `off` leaves it the colour of a
/// strong border — the same reading as a checkbox, in the space of a word.
pub fn switch(on: bool, cx: &App) -> Div {
    let colors = &theme(cx).colors;
    let mut track = div()
        .w(px(30.))
        .h(px(17.))
        .flex_none()
        .flex()
        .items_center()
        .rounded_full()
        .p(px(2.))
        .bg(if on { colors.accent } else { colors.border_strong })
        .child(div().size(px(13.)).rounded_full().bg(colors.elevated));
    if on {
        track = track.justify_end();
    }
    track
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

    #[test]
    fn a_running_clock_never_changes_units() {
        // What `format_millis` would call "34 ms", the timer calls "0.0 s":
        // the unit has to hold still while the number climbs.
        assert_eq!(format_seconds(34), "0.0 s");
        assert_eq!(format_seconds(900), "0.9 s");
        assert_eq!(format_seconds(1_900), "1.9 s");
        assert_eq!(format_seconds(64_000), "64.0 s");
    }
}
