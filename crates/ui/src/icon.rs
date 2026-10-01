//! Line icons, drawn as stroked paths.
//!
//! The app carries no image assets, so every glyph is geometry. The older
//! marks here — the padlock, the play triangle, the magnifier — are boxes
//! and filled paths, because they predate GPUI's `PathBuilder::stroke`.
//! Those were one-offs; the sidebar, the tab strip and the rail need a
//! family, and a family has to share a pen. So every icon is drawn in a
//! 14-unit box with one stroke width, scaled to the size asked for, and a
//! 12px icon beside a 16px one reads as the same hand.

use gpui::{
    Bounds, Hsla, IntoElement, PathBuilder, Pixels, Point, Styled as _, Window, canvas, point, px,
};

/// The box every icon is drawn in, in its own units.
const GRID: f32 = 14.;

/// The pen, in grid units. At 14px that is 1.3px, the stroke the comp's
/// icons are drawn with.
const STROKE: f32 = 1.3;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Icon {
    /// A table: a frame with a header rule and a first column.
    Table,
    /// A view: the same frame, with its body left open.
    View,
    /// A query tab: a prompt and a cursor line.
    Query,
    /// The history tab and the history button: a clock face.
    History,
    /// A schema: three stacked sheets.
    Schema,
    ChevronRight,
    ChevronDown,
    Plus,
    /// A primary key column.
    Key,
    /// A statement that landed.
    Check,
    /// A saved connection.
    Database,
    /// A search line.
    Search,
}

/// An icon at `size` pixels square, stroked in `color`.
pub fn icon(kind: Icon, size: f32, color: Hsla) -> impl IntoElement {
    canvas(
        |_bounds, _window, _cx| (),
        move |bounds, _state, window, _cx| paint(kind, bounds, color, window),
    )
    .size(px(size))
    .flex_none()
}

fn paint(kind: Icon, bounds: Bounds<Pixels>, color: Hsla, window: &mut Window) {
    let scale = bounds.size.width / px(GRID);
    let at =
        |x: f32, y: f32| -> Point<Pixels> { bounds.origin + point(px(x * scale), px(y * scale)) };
    let mut pen = PathBuilder::stroke(px(STROKE * scale));
    let radius = |r: f32| point(px(r * scale), px(r * scale));
    // A closed circle as two half arcs: one arc cannot end where it began.
    let circle = |pen: &mut PathBuilder, cx: f32, cy: f32, r: f32| {
        pen.move_to(at(cx - r, cy));
        pen.arc_to(radius(r), px(0.), false, true, at(cx + r, cy));
        pen.arc_to(radius(r), px(0.), false, true, at(cx - r, cy));
        pen.close();
    };
    let rect = |pen: &mut PathBuilder, x: f32, y: f32, w: f32, h: f32| {
        pen.move_to(at(x, y));
        pen.line_to(at(x + w, y));
        pen.line_to(at(x + w, y + h));
        pen.line_to(at(x, y + h));
        pen.close();
    };
    match kind {
        Icon::Table => {
            rect(&mut pen, 1.5, 2., 11., 10.);
            pen.move_to(at(1.5, 5.5));
            pen.line_to(at(12.5, 5.5));
            pen.move_to(at(5.5, 5.5));
            pen.line_to(at(5.5, 12.));
        }
        Icon::View => {
            rect(&mut pen, 1.5, 2., 11., 10.);
            pen.move_to(at(1.5, 5.5));
            pen.line_to(at(12.5, 5.5));
            pen.move_to(at(4.5, 8.8));
            pen.line_to(at(9.5, 8.8));
        }
        Icon::Query => {
            pen.move_to(at(2.5, 4.));
            pen.line_to(at(5.5, 7.));
            pen.line_to(at(2.5, 10.));
            pen.move_to(at(7.5, 10.5));
            pen.line_to(at(11.5, 10.5));
        }
        Icon::History => {
            circle(&mut pen, 7., 7., 5.2);
            pen.move_to(at(7., 4.2));
            pen.line_to(at(7., 7.));
            pen.line_to(at(9., 8.4));
        }
        Icon::Schema => {
            pen.move_to(at(7., 1.8));
            pen.line_to(at(12.3, 4.4));
            pen.line_to(at(7., 7.));
            pen.line_to(at(1.7, 4.4));
            pen.close();
            pen.move_to(at(1.7, 7.));
            pen.line_to(at(7., 9.6));
            pen.line_to(at(12.3, 7.));
            pen.move_to(at(1.7, 9.6));
            pen.line_to(at(7., 12.2));
            pen.line_to(at(12.3, 9.6));
        }
        Icon::ChevronRight => {
            pen.move_to(at(5.3, 3.5));
            pen.line_to(at(8.8, 7.));
            pen.line_to(at(5.3, 10.5));
        }
        Icon::ChevronDown => {
            pen.move_to(at(3.5, 5.3));
            pen.line_to(at(7., 8.8));
            pen.line_to(at(10.5, 5.3));
        }
        Icon::Plus => {
            pen.move_to(at(7., 2.5));
            pen.line_to(at(7., 11.5));
            pen.move_to(at(2.5, 7.));
            pen.line_to(at(11.5, 7.));
        }
        Icon::Key => {
            circle(&mut pen, 4.6, 7., 2.6);
            pen.move_to(at(7.2, 7.));
            pen.line_to(at(12.5, 7.));
            pen.move_to(at(10.8, 7.));
            pen.line_to(at(10.8, 9.4));
        }
        Icon::Check => {
            pen.move_to(at(3., 7.3));
            pen.line_to(at(5.8, 10.));
            pen.line_to(at(11., 4.3));
        }
        Icon::Database => {
            pen.move_to(at(2., 3.8));
            pen.arc_to(
                point(px(5. * scale), px(1.9 * scale)),
                px(0.),
                false,
                true,
                at(12., 3.8),
            );
            pen.arc_to(
                point(px(5. * scale), px(1.9 * scale)),
                px(0.),
                false,
                true,
                at(2., 3.8),
            );
            pen.close();
            pen.move_to(at(2., 3.8));
            pen.line_to(at(2., 10.2));
            pen.arc_to(
                point(px(5. * scale), px(1.9 * scale)),
                px(0.),
                false,
                false,
                at(12., 10.2),
            );
            pen.line_to(at(12., 3.8));
            pen.move_to(at(2., 7.));
            pen.arc_to(
                point(px(5. * scale), px(1.9 * scale)),
                px(0.),
                false,
                false,
                at(12., 7.),
            );
        }
        Icon::Search => {
            circle(&mut pen, 6., 6., 4.);
            pen.move_to(at(9., 9.));
            pen.line_to(at(12.3, 12.3));
        }
    }
    if let Ok(path) = pen.build() {
        window.paint_path(path, color);
    }
}
