//! The results grid: a gutter and a column header over a virtualized row
//! list. The table view and the query runner both render through it, so a
//! result looks the same wherever it came from.
//!
//! Rows are virtualized with `uniform_list`, so a 500-row page costs the
//! same as a 15-row one. Lane widths are measured once from the values
//! themselves; dragging a column to resize it comes later.
//!
//! What is marked lives in [`Selection`], which the caller owns beside the
//! scroll position: the grid paints a selection and reports where the mouse
//! landed, and decides nothing itself. Every click comes back as one
//! [`Hit`], so the whole of what a click can mean is one enum the caller
//! matches on rather than five callbacks that can disagree.

pub mod selection;

use db_client::Value;
use gpui::{
    App, Div, ElementId, FontWeight, Hsla, ScrollHandle, ScrollStrategy, SharedString, Stateful,
    UniformListScrollHandle, div, prelude::*, px, uniform_list,
};
pub use selection::{Cell, Extent, Rect, Selection, Step, clipboard_text};
use std::rc::Rc;
use theme::theme;
use ui::scrollbar::{self, DragState, Scrollbar};

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

/// The lane down the left holding the row number, and the target that ticks
/// a row. Wide enough for the comp's own gutter, which is 44px in the query
/// editor — the two read as the same lane.
const GUTTER_WIDTH: f32 = 44.;
/// The mark a ticked row wears in the gutter, in place of its number.
const TICK: &str = "✓";

/// Where a click landed, and what the modifiers said about it. The caller
/// turns one of these into a change on its [`Selection`].
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Hit {
    /// A cell. `extend` is ⇧ held, which grows the range instead of starting
    /// a new one; `detail` is the second click of a double click, which is
    /// what opens the row drawer.
    Cell { cell: Cell, extend: bool, detail: bool },
    /// The gutter beside a row: tick it, or with ⇧ tick everything back to
    /// the last row ticked.
    Pick { row: usize, through: bool },
    /// The gutter's own header: every row, or none.
    PickAll,
    /// A column header: the whole column.
    Column { column: usize },
}

/// Called with wherever the mouse landed in the grid.
pub type OnHit = Rc<dyn Fn(Hit, &mut gpui::Window, &mut App)>;

/// Scroll position for one grid, held by whoever owns the tab so it
/// survives the re-render after every keystroke and every page.
#[derive(Clone, Default)]
pub struct GridState {
    rows: UniformListScrollHandle,
    columns: ScrollHandle,
    drag: DragState,
}

impl GridState {
    pub fn new() -> Self {
        Self::default()
    }

    /// Bring a cell into view, down **and across**. A cursor that has walked
    /// off the edge of the pane is a cursor the user has lost, and a result
    /// wider than the pane is the ordinary case, not the exception.
    ///
    /// The rows go through the list's own handle. The columns are a plain
    /// scroll container, so the offset is worked out here — moving the least
    /// it can, so walking a row sideways slides the pane along by one lane
    /// rather than jumping the cell to an edge.
    pub fn reveal(&self, cell: Cell, data: &GridData) {
        self.rows.scroll_to_item(cell.row, ScrollStrategy::Nearest);

        let Some((left, width)) = data.lane_span(cell.column) else { return };
        let viewport = f32::from(self.columns.bounds().size.width);
        // Nothing is painted yet on the frame a result lands: there is no
        // pane to be inside of, and the next frame's scroll would be
        // measured against zero.
        if viewport <= 0. {
            return;
        }
        let offset = self.columns.offset();
        // Two signs, and they are not the same: the *offset* is negative as
        // the content moves left under the pane, while `max_offset` is a
        // positive distance — the content's width less the pane's. Reading
        // the second as a negative offset made the clamp `0..=0`, which is
        // to say the columns never moved at all.
        let visible_left = -f32::from(offset.x);
        let furthest = f32::from(self.columns.max_offset().x).max(0.);
        let wanted = reveal_offset(left, width, visible_left, viewport).clamp(0., furthest);
        if wanted != visible_left {
            self.columns.set_offset(gpui::point(px(-wanted), offset.y));
        }
    }

    /// The row list scrolls through its own handle, which wraps a plain
    /// one; the scrollbar only needs the plain one.
    fn rows_handle(&self) -> ScrollHandle {
        self.rows.0.borrow().base_handle.clone()
    }
}

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

    /// Where one lane sits in the scrolling content: its left edge and its
    /// width, measured from the content's own left — so the gutter, which
    /// scrolls with the content, counts.
    ///
    /// The last lane is laid out with its width as a floor and takes the
    /// slack, so on screen it can be wider than this says. That only ever
    /// makes it *more* visible than the sum promises, which is the safe way
    /// round for scrolling something into view.
    pub fn lane_span(&self, column: usize) -> Option<(f32, f32)> {
        let width = *self.widths.get(column)?;
        Some((GUTTER_WIDTH + self.widths[..column].iter().sum::<f32>(), width))
    }
}

/// How far the content should be scrolled left so a lane is whole on
/// screen, moving the least it can.
///
/// A lane already inside the pane is left alone: without that, every step
/// along a row would drag the pane about. A lane off to the left comes to
/// the pane's left edge; one off to the right comes to its right edge. A
/// lane wider than the pane cannot be whole either way, so its left edge
/// wins — the value is read from the left.
fn reveal_offset(left: f32, width: f32, visible_left: f32, viewport: f32) -> f32 {
    let right = left + width;
    if left < visible_left || width >= viewport {
        left
    } else if right > visible_left + viewport {
        right - viewport
    } else {
        visible_left
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

/// One result set on screen. `id` must be unique among the grids alive in
/// one window, because it keys the list's scroll position.
pub struct Grid<'a> {
    id: SharedString,
    data: Rc<GridData>,
    state: &'a GridState,
    selection: &'a Selection,
    first_row: usize,
    on_hit: Option<OnHit>,
}

impl<'a> Grid<'a> {
    pub fn new(
        id: impl Into<SharedString>,
        data: Rc<GridData>,
        state: &'a GridState,
        selection: &'a Selection,
    ) -> Self {
        Self { id: id.into(), data, state, selection, first_row: 1, on_hit: None }
    }

    /// The number the gutter gives the first row on screen. A table page is
    /// a window on the table, so its gutter counts from where the page
    /// starts rather than from 1 again.
    pub fn first_row(mut self, first_row: usize) -> Self {
        self.first_row = first_row;
        self
    }

    pub fn on_hit(mut self, on_hit: OnHit) -> Self {
        self.on_hit = Some(on_hit);
        self
    }

    pub fn render(self, cx: &App) -> Div {
        let Self { id, data, state, selection, first_row, on_hit } = self;
        let colors = theme(cx).colors.clone();
        // The content can be wider than the pane; the whole grid scrolls
        // sideways as one, gutter and header included. The gutter goes with
        // it rather than pinning itself to the left edge: pinning would
        // need a second vertical scroller kept in step with the row list,
        // and `uniform_list` gives nothing to keep it in step with.
        let content_width: f32 = GUTTER_WIDTH + data.widths.iter().sum::<f32>();
        // The list closure outlives this frame's borrow, so it takes a
        // selection of its own. One clone of a small set per frame.
        let marks = Rc::new(selection.clone());
        let extent = Extent::of(&data);
        let rows = row_list(&id, data.clone(), state, marks.clone(), first_row, on_hit.clone());

        // The bars sit outside the scrolling content, or they would scroll
        // away with it.
        div()
            .relative()
            .flex_1()
            .min_h(px(0.))
            // A grid is laid out beside the row drawer, so it is a flex item
            // in a row — and a flex item may not shrink below its content by
            // default. Without this the wide content makes the *grid* wide
            // and nothing overflows the scroll container inside it, which is
            // to say the columns stop scrolling sideways at all.
            .min_w(px(0.))
            .child(
                div()
                    .id(ElementId::Name(format!("{id}-grid").into()))
                    .size_full()
                    .overflow_x_scroll()
                    .track_scroll(&state.columns)
                    // A grid has a scroll container per axis: this one for the
                    // columns, the row list for the rows. Locking each to the
                    // gesture's dominant axis keeps a diagonal swipe from
                    // moving both at once, and lets a vertical gesture pass
                    // through to the list.
                    .restrict_scroll_to_axis()
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .h_full()
                            .w(px(content_width))
                            .min_w_full()
                            // The lanes are measured against this size; leaving
                            // the default here would render the data wider than
                            // its lane.
                            .text_size(px(DATA_FONT_SIZE))
                            .child(header_row(&data, &marks, extent, on_hit, &colors))
                            .child(rows),
                    ),
            )
            .children(
                Scrollbar::new(
                    true,
                    state.rows_handle(),
                    state.drag.clone(),
                    colors.text_faint,
                    colors.text_muted,
                )
                .map(|bar| {
                    div()
                        .absolute()
                        // Start below the header, so the bar never covers it.
                        .top(px(HEADER_HEIGHT))
                        .right(px(0.))
                        .bottom(px(0.))
                        .w(px(scrollbar::THICKNESS))
                        .child(bar)
                }),
            )
            .children(
                Scrollbar::new(
                    false,
                    state.columns.clone(),
                    state.drag.clone(),
                    colors.text_faint,
                    colors.text_muted,
                )
                .map(|bar| {
                    div()
                        .absolute()
                        .left(px(0.))
                        .right(px(0.))
                        .bottom(px(0.))
                        .h(px(scrollbar::THICKNESS))
                        .child(bar)
                }),
            )
    }
}

/// The virtualized row list. Built here rather than inline so the axis
/// lock can be set: `UniformList` carries an `Interactivity` but not
/// `StatefulInteractiveElement`, so the style flag that
/// `restrict_scroll_to_axis()` would set is set by hand.
fn row_list(
    id: &SharedString,
    data: Rc<GridData>,
    state: &GridState,
    marks: Rc<Selection>,
    first_row: usize,
    on_hit: Option<OnHit>,
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
                        &marks,
                        first_row,
                        on_hit.clone(),
                        &colors,
                    )
                })
                .collect::<Vec<_>>()
        },
    );
    rows.style().restrict_scroll_to_axis = Some(true);
    rows.track_scroll(&state.rows).flex_1()
}

fn header_row(
    data: &GridData,
    marks: &Selection,
    extent: Extent,
    on_hit: Option<OnHit>,
    colors: &theme::ThemeColors,
) -> Div {
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

    // The gutter's own head is the "all of them, or none" tick. It says
    // which state it is in rather than what a click would do, as every
    // other tick in the app does.
    let all = marks.all_picked(extent);
    let hover_bg = colors.hairline;
    let mut head = div()
        .id("gutter-head")
        .w(px(GUTTER_WIDTH))
        .flex_none()
        .h_full()
        .flex()
        .items_center()
        .justify_center()
        .border_r_1()
        .border_color(colors.hairline)
        .text_color(if all { colors.accent_deep } else { colors.text_faint })
        .child(if all { TICK } else { "#" });
    if let Some(on_hit) = on_hit.clone() {
        head = head
            .cursor_pointer()
            .hover(move |s| s.bg(hover_bg))
            .on_click(move |_event, window, cx| on_hit(Hit::PickAll, window, cx));
    }
    row = row.child(head);

    let last = data.columns.len().saturating_sub(1);
    let selected_columns = marks.rect().filter(|rect| rect.rows() == extent.rows.max(1));
    for (ix, name) in data.columns.iter().enumerate() {
        // A column reads as selected only when the range covers all of it,
        // which is what clicking its header does.
        let whole = selected_columns.is_some_and(|rect| (rect.left..=rect.right).contains(&ix));
        let mut cell = lane(div().id(ix), data.widths[ix], ix == last)
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
        if whole {
            cell = cell.bg(colors.selection).text_color(colors.accent_deep);
        }
        if let Some(on_hit) = on_hit.clone() {
            cell = cell
                .cursor_pointer()
                .on_click(move |_event, window, cx| on_hit(Hit::Column { column: ix }, window, cx));
        }
        row = row.child(cell);
    }
    row
}

/// Size one cell. The last lane absorbs the slack, so short results still
/// rule the full pane width instead of stopping mid-way; it keeps its
/// fixed width as a floor, so a wide result still scrolls sideways.
fn lane<T: Styled>(cell: T, width: f32, last: bool) -> T {
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
    marks: &Selection,
    first_row: usize,
    on_hit: Option<OnHit>,
    colors: &theme::ThemeColors,
) -> Stateful<Div> {
    let picked = marks.is_picked(ix);
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
    if picked {
        // A ticked row is washed whole: the tick is a statement about the
        // record, not about a cell in it.
        row = row.bg(colors.selection);
    } else if marks.cursor().is_none_or(|cursor| cursor.row != ix) {
        // Hovering says "clickable". A row the cursor is already on says
        // something truer, so it keeps its own marks instead.
        let hover_bg = colors.panel;
        row = row.hover(move |s| s.bg(hover_bg));
    }

    // The gutter: the row's number, or the tick when it is picked.
    let mut gutter = div()
        .id("gutter")
        .w(px(GUTTER_WIDTH))
        .flex_none()
        .h_full()
        .flex()
        .items_center()
        .justify_center()
        .border_r_1()
        .border_color(colors.hairline)
        .text_size(px(11.))
        .text_color(match (picked, marks.cursor()) {
            (true, _) => colors.accent_deep,
            // The number marks where the cursor is, so the eye can find the
            // focused row after scrolling sideways off its cell.
            (false, Some(cursor)) if cursor.row == ix => colors.text_secondary,
            _ => colors.line_number,
        })
        .child(if picked { TICK.to_string() } else { (first_row + ix).to_string() });
    if let Some(on_hit) = on_hit.clone() {
        let gutter_hover = colors.hairline;
        gutter = gutter.hover(move |s| s.bg(gutter_hover)).on_click(move |event, window, cx| {
            on_hit(Hit::Pick { row: ix, through: event.modifiers().shift }, window, cx);
        });
    }
    row = row.child(gutter);

    let last = widths.len().saturating_sub(1);
    for (column, value) in values.iter().enumerate() {
        // A row can be shorter than the header when a driver returns
        // ragged rows; lay out only what the header has room for.
        let Some(width) = widths.get(column) else { break };
        let mut cell = lane(div().id(column), *width, column == last)
            .px(px(12.))
            .h_full()
            .flex()
            .items_center()
            .overflow_hidden()
            .truncate()
            .text_color(value_color(value, colors))
            .child(value.display());
        // The cursor's cell is the strongest mark on screen, the rest of
        // the range a wash under it. Neither carries a border: a border
        // would take a pixel out of the cell's content box and shift the
        // value inside it every time the cursor moved.
        if marks.is_cursor(ix, column) {
            cell = cell.bg(colors.match_strong).text_color(colors.text);
        } else if marks.contains(ix, column) {
            cell = cell.bg(colors.range_surface);
        }
        if let Some(on_hit) = on_hit.clone() {
            cell = cell.on_click(move |event, window, cx| {
                let hit = Hit::Cell {
                    cell: Cell::new(ix, column),
                    extend: event.modifiers().shift,
                    detail: event.click_count() >= 2,
                };
                on_hit(hit, window, cx);
            });
        }
        row = row.child(cell);
    }

    row
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

    /// A lane already on screen must not move the pane: the cursor walks a
    /// row a lane at a time, and a pane that re-centred on every step would
    /// slide about under the reader.
    #[test]
    fn a_lane_already_in_view_scrolls_nothing() {
        assert_eq!(reveal_offset(200., 100., 150., 400.), 150.);
        // Flush against each edge still counts as whole on screen.
        assert_eq!(reveal_offset(150., 100., 150., 400.), 150.);
        assert_eq!(reveal_offset(450., 100., 150., 400.), 150.);
    }

    #[test]
    fn a_lane_off_the_edge_comes_the_shortest_way_back() {
        // Off to the left: its left edge to the pane's left.
        assert_eq!(reveal_offset(100., 80., 150., 400.), 100.);
        // Off to the right: its right edge to the pane's right, which is
        // one lane's worth of movement, not a jump to the left edge.
        assert_eq!(reveal_offset(500., 100., 150., 400.), 200.);
    }

    /// A lane wider than the pane cannot be whole on screen. Its left edge
    /// wins, because that is the end the value is read from.
    #[test]
    fn a_lane_wider_than_the_pane_shows_its_start() {
        assert_eq!(reveal_offset(600., 500., 0., 400.), 600.);
    }

    /// The gutter scrolls with the content, so it counts in every lane's
    /// position — the first lane does not start at zero.
    #[test]
    fn lane_positions_count_the_gutter() {
        let data = GridData::new(
            vec!["id".to_string(), "email".to_string()],
            vec![vec![Value::Int(1), text("ada@example.com")]],
        );
        let (left, width) = data.lane_span(0).unwrap();
        assert_eq!((left, width), (GUTTER_WIDTH, data.widths[0]));
        let (left, width) = data.lane_span(1).unwrap();
        assert_eq!((left, width), (GUTTER_WIDTH + data.widths[0], data.widths[1]));
        assert_eq!(data.lane_span(2), None);
    }
}
