//! The results grid: a fixed-width column header over a virtualized row
//! list. The table view and the query runner both render through it, so a
//! result looks the same wherever it came from.
//!
//! Rows are virtualized with `uniform_list`, so a 500-row page costs the
//! same as a 15-row one. Column widths are fixed for now; measuring and
//! resizing come later.

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

const DEFAULT_COLUMN_WIDTH: f32 = 140.;
/// Narrow lane for key columns, which hold short values.
const KEY_COLUMN_WIDTH: f32 = 64.;

/// Called with the row index when the user clicks a row.
pub type OnClickRow = Rc<dyn Fn(usize, &mut gpui::Window, &mut App)>;

/// One rendered result set.
pub struct GridData {
    pub columns: Vec<String>,
    pub rows: Vec<Vec<Value>>,
}

/// Fixed lane widths, one per column. Key-looking columns get the narrow
/// lane; everything else gets the default.
pub fn column_widths(columns: &[String]) -> Vec<f32> {
    columns
        .iter()
        .map(|name| {
            if is_key_column(name) {
                KEY_COLUMN_WIDTH
            } else {
                DEFAULT_COLUMN_WIDTH
            }
        })
        .collect()
}

fn is_key_column(name: &str) -> bool {
    let name = name.to_ascii_lowercase();
    name == "id" || name.ends_with("_id")
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
    let widths = Rc::new(column_widths(&data.columns));
    // The lanes are fixed, so the content can be wider than the pane; the
    // whole grid scrolls sideways as one, header included.
    let content_width: f32 = widths.iter().sum();
    let row_count = data.rows.len();

    let list_data = data.clone();
    let list_widths = widths.clone();

    div()
        .id(ElementId::Name(format!("{id}-grid").into()))
        .flex_1()
        .min_h(px(0.))
        .overflow_x_scroll()
        .child(
            div()
                .flex()
                .flex_col()
                .h_full()
                .w(px(content_width))
                .min_w_full()
                .child(header_row(&data.columns, &widths, &colors))
                .child(
                    uniform_list(
                        ElementId::Name(format!("{id}-rows").into()),
                        row_count,
                        move |range, _window, cx| {
                            let colors = theme(cx).colors.clone();
                            range
                                .map(|ix| {
                                    data_row(
                                        ix,
                                        &list_data.rows[ix],
                                        &list_widths,
                                        selected == Some(ix),
                                        on_click.clone(),
                                        &colors,
                                    )
                                })
                                .collect::<Vec<_>>()
                        },
                    )
                    .flex_1(),
                ),
        )
}

fn header_row(columns: &[String], widths: &[f32], colors: &theme::ThemeColors) -> Div {
    let mut row = div()
        .h(px(HEADER_HEIGHT))
        .flex_none()
        .flex()
        .items_center()
        .border_b_1()
        .border_color(colors.border_strong)
        .bg(colors.panel)
        .text_size(px(10.))
        .font_weight(FontWeight::SEMIBOLD)
        .text_color(colors.text_muted);
    for (ix, name) in columns.iter().enumerate() {
        let mut cell = div()
            .w(px(widths[ix]))
            .flex_none()
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
        .flex()
        .items_center()
        .border_b_1()
        .border_color(colors.hairline)
        .cursor_pointer();

    for (column, value) in values.iter().enumerate() {
        // A row can be shorter than the header when a driver returns
        // ragged rows; lay out only what the header has room for.
        let Some(width) = widths.get(column) else { break };
        row = row.child(
            div()
                .w(px(*width))
                .flex_none()
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
