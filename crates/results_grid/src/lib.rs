//! The results grid: a column header over a virtualized row list. The
//! table view and the query runner both render through it, so a result
//! looks the same wherever it came from.
//!
//! Rows are virtualized with `uniform_list`, so a 500-row page costs the
//! same as a 15-row one. Lane widths are measured once from the values
//! themselves; dragging a column to resize it comes later.

use db_client::Value;
use gpui::{
    App, Div, ElementId, FontWeight, Hsla, SharedString, Stateful, div, prelude::*, px,
    uniform_list,
};
use std::rc::Rc;
use theme::theme;

/// Row height from the design comp. The header is 30px, data rows 28px.
pub const ROW_HEIGHT: f32 = 28.;
pub const HEADER_HEIGHT: f32 = 30.;

/// Data text size, from the design comp.
const DATA_FONT_SIZE: f32 = 12.;
/// The app is monospaced throughout, so one character is one advance and
/// a lane can be sized from the character count alone. JetBrains Mono
/// advances 0.6em, so 7.2px at 12px; round up so a full-width value keeps
/// a hair of slack instead of tripping the ellipsis.
const CHAR_WIDTH: f32 = DATA_FONT_SIZE * 0.62;
/// The 12px of padding on each side of a cell.
const CELL_PADDING: f32 = 24.;
const MIN_COLUMN_WIDTH: f32 = 56.;
/// Past this a column steals the pane; the rest of the value truncates.
const MAX_COLUMN_WIDTH: f32 = 320.;
/// Rows sampled to size the lanes. The first screens decide the widths;
/// scanning a whole 500-row page for a few pixels is not worth it.
const WIDTH_SAMPLE: usize = 200;

/// Called with the row index when the user clicks a row.
pub type OnClickRow = Rc<dyn Fn(usize, &mut gpui::Window, &mut App)>;

/// One rendered result set, with its lanes already sized. Widths are
/// computed once here rather than on every frame.
pub struct GridData {
    pub columns: Vec<String>,
    pub rows: Vec<Vec<Value>>,
    widths: Vec<f32>,
}

impl GridData {
    pub fn new(columns: Vec<String>, rows: Vec<Vec<Value>>) -> Self {
        let widths = column_widths(&columns, &rows);
        Self { columns, rows, widths }
    }

    pub fn empty() -> Self {
        Self::new(Vec::new(), Vec::new())
    }
}

/// Size each lane to the widest value it actually holds, header included.
pub fn column_widths(columns: &[String], rows: &[Vec<Value>]) -> Vec<f32> {
    columns
        .iter()
        .enumerate()
        .map(|(ix, name)| {
            let widest = rows
                .iter()
                .take(WIDTH_SAMPLE)
                .filter_map(|row| row.get(ix))
                .map(|value| value.display().chars().count())
                .max()
                .unwrap_or(0)
                .max(name.chars().count());
            (widest as f32 * CHAR_WIDTH + CELL_PADDING)
                .clamp(MIN_COLUMN_WIDTH, MAX_COLUMN_WIDTH)
        })
        .collect()
}

/// Render a result set. `id` must be unique among the grids alive in one
/// window, because it keys the list's scroll position.
pub fn grid(
    id: impl Into<SharedString>,
    data: Rc<GridData>,
    selected: Option<usize>,
    on_click: Option<OnClickRow>,
    cx: &App,
) -> Stateful<Div> {
    let id: SharedString = id.into();
    let colors = theme(cx).colors.clone();
    // The content can be wider than the pane; the whole grid scrolls
    // sideways as one, header included.
    let content_width: f32 = data.widths.iter().sum();
    let rows = row_list(&id, data.clone(), selected, on_click);

    div()
        .id(ElementId::Name(format!("{id}-grid").into()))
        .flex_1()
        .min_h(px(0.))
        .overflow_x_scroll()
        // A grid has a scroll container per axis: this one for the
        // columns, the row list for the rows. Locking each to the
        // gesture's dominant axis keeps a diagonal swipe from moving both
        // at once, and lets a vertical gesture pass through to the list.
        .restrict_scroll_to_axis()
        .child(
            div()
                .flex()
                .flex_col()
                .h_full()
                .w(px(content_width))
                .min_w_full()
                // The lanes are measured against this size; leaving the
                // default here would render the data wider than its lane.
                .text_size(px(DATA_FONT_SIZE))
                .child(header_row(&data.columns, &data.widths, &colors))
                .child(rows),
        )
}

/// The virtualized row list. Built here rather than inline so the axis
/// lock can be set: `UniformList` carries an `Interactivity` but not
/// `StatefulInteractiveElement`, so the style flag that
/// `restrict_scroll_to_axis()` would set is set by hand.
fn row_list(
    id: &SharedString,
    data: Rc<GridData>,
    selected: Option<usize>,
    on_click: Option<OnClickRow>,
) -> impl IntoElement {
    let mut rows = uniform_list(
        ElementId::Name(format!("{id}-rows").into()),
        data.rows.len(),
        move |range, _window, cx| {
            let colors = theme(cx).colors.clone();
            range
                .map(|ix| {
                    data_row(
                        ix,
                        &data.rows[ix],
                        &data.widths,
                        selected == Some(ix),
                        on_click.clone(),
                        &colors,
                    )
                })
                .collect::<Vec<_>>()
        },
    );
    rows.style().restrict_scroll_to_axis = Some(true);
    rows.flex_1()
}

fn header_row(columns: &[String], widths: &[f32], colors: &theme::ThemeColors) -> Div {
    let mut row = div()
        .h(px(HEADER_HEIGHT))
        .flex_none()
        // Rows are laid out inside the list, which does not stretch them;
        // without this the hairlines stop where the values do.
        .w_full()
        .flex()
        .items_center()
        .border_b_1()
        .border_color(colors.border_strong)
        .bg(colors.panel)
        .text_size(px(10.))
        .font_weight(FontWeight::SEMIBOLD)
        .text_color(colors.text_muted);
    let last = columns.len().saturating_sub(1);
    for (ix, name) in columns.iter().enumerate() {
        let mut cell = lane(div(), widths[ix], ix == last)
            .px(px(12.))
            .h_full()
            .flex()
            .items_center()
            .overflow_hidden()
            .truncate()
            .child(name.to_ascii_uppercase());
        if ix > 0 {
            cell = cell.border_l_1().border_color(colors.hairline);
        }
        row = row.child(cell);
    }
    row
}

/// Size one cell. The last lane absorbs the slack, so short results still
/// rule the full pane width instead of stopping mid-way; it keeps its
/// fixed width as a floor, so a wide result still scrolls sideways.
fn lane(cell: Div, width: f32, last: bool) -> Div {
    if last {
        cell.min_w(px(width)).flex_1()
    } else {
        cell.w(px(width)).flex_none()
    }
}

fn data_row(
    ix: usize,
    values: &[Value],
    widths: &[f32],
    selected: bool,
    on_click: Option<OnClickRow>,
    colors: &theme::ThemeColors,
) -> Stateful<Div> {
    let mut row = div()
        .id(ix)
        .h(px(ROW_HEIGHT))
        .flex_none()
        .w_full()
        .flex()
        .items_center()
        .border_b_1()
        .border_color(colors.hairline)
        .cursor_pointer();

    let last = widths.len().saturating_sub(1);
    for (column, value) in values.iter().enumerate() {
        // A row can be shorter than the header when a driver returns
        // ragged rows; lay out only what the header has room for.
        let Some(width) = widths.get(column) else { break };
        row = row.child(
            lane(div(), *width, column == last)
                .px(px(12.))
                .overflow_hidden()
                .truncate()
                .text_color(value_color(value, colors))
                .child(value.display()),
        );
    }

    if let Some(on_click) = on_click {
        row = row.on_click(move |_event, window, cx| on_click(ix, window, cx));
    }
    if selected {
        row.bg(colors.selection)
    } else {
        let hover_bg = colors.panel;
        row.hover(move |s| s.bg(hover_bg))
    }
}

fn value_color(value: &Value, colors: &theme::ThemeColors) -> Hsla {
    match value {
        // NULL must read as absence, not as data.
        Value::Null => colors.text_faint,
        Value::Bytes(_) => colors.text_muted,
        _ => colors.text_body,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn text(value: &str) -> Value {
        Value::Text(value.to_string())
    }

    #[test]
    fn lanes_fit_the_widest_value() {
        let data = GridData::new(
            vec!["id".to_string(), "email".to_string()],
            vec![
                vec![Value::Int(1), text("ada@example.com")],
                vec![Value::Int(1041), text("a@b.co")],
            ],
        );
        // "1041" is 4 characters, but the floor keeps the lane usable.
        assert_eq!(data.widths[0], MIN_COLUMN_WIDTH);
        // 15 characters plus the padding.
        assert_eq!(data.widths[1], 15. * CHAR_WIDTH + CELL_PADDING);
        // A lane must hold its own text: 15 characters at the data size.
        assert!(data.widths[1] >= 15. * DATA_FONT_SIZE * 0.6 + CELL_PADDING);
    }

    #[test]
    fn a_header_wider_than_its_values_still_fits() {
        let data = GridData::new(
            vec!["reactivated_at_utc".to_string()],
            vec![vec![text("x")]],
        );
        assert_eq!(data.widths[0], 18. * CHAR_WIDTH + CELL_PADDING);
    }

    #[test]
    fn one_huge_value_cannot_steal_the_pane() {
        let data = GridData::new(
            vec!["blob".to_string()],
            vec![vec![text(&"x".repeat(4_000))]],
        );
        assert_eq!(data.widths[0], MAX_COLUMN_WIDTH);
    }

    #[test]
    fn an_empty_result_has_no_lanes() {
        assert!(GridData::empty().widths.is_empty());
    }
}
