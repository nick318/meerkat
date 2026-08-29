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

pub mod columns;
pub mod selection;

pub use columns::find_columns;
use db_client::Value;
use gpui::{
    App, Bounds, Div, Element, ElementId, EntityId, FontWeight, GlobalElementId, Hsla, LayoutId,
    MouseButton, MouseMoveEvent, MouseUpEvent, Pixels, Point, ScrollHandle, ScrollStrategy,
    SharedString, Stateful, Style, UniformListScrollHandle, Window, div, point, prelude::*, px,
    uniform_list,
};
pub use selection::{Cell, Extent, Rect, Selection, Step, clipboard_text};
use std::cell::Cell as StdCell;
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
/// The most characters a cell ever shapes. A lane is `MAX_COLUMN_WIDTH`
/// wide, so about fifty characters is all that can be on screen; the rest
/// is shaped, measured and thrown away by the ellipsis. `MAX_CELL_BYTES`
/// lets a megabyte of text into one value, and shaping a megabyte of it
/// per visible cell per frame would freeze the window.
const CELL_CHARS: usize = 64;
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
    /// A cell. `extend` is ⇧ held, which grows the range instead of
    /// starting a new one. `peek` is the second click of a double click,
    /// which asks to read the whole value: a lane is capped, so a long value
    /// truncates on screen and the click is how the rest of it is asked for.
    Cell {
        cell: Cell,
        extend: bool,
        peek: bool,
    },
    /// The pointer has moved onto this cell with the button still down, so
    /// the range grows to it. It is not a fresh click: the press that
    /// started the drag has already taken the focus and set the anchor.
    Drag { cell: Cell },
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
    pointer: Pointer,
}

/// What the pointer is doing to the grid. Neither of these is a selection —
/// they are the mouse's own state between one frame and the next, so they
/// live beside the scroll position rather than in [`Selection`].
#[derive(Clone, Default)]
struct Pointer {
    /// The row whose gutter the pointer is over, if any.
    ///
    /// A hover style paints the element it is set on, and the wash for a
    /// whole row is painted by the *row*, which is the gutter's parent — no
    /// style reaches upwards. So the gutter records the row it is over and
    /// every row reads it back.
    hover_row: Rc<StdCell<Option<usize>>>,
    /// The left button went down on a cell and has not come up yet, so a
    /// move grows the range instead of doing nothing.
    dragging: Rc<StdCell<bool>>,
    /// Where the pointer was last seen, in the window's own coordinates. A
    /// drag held past an edge scrolls on every frame, and a frame is not a
    /// mouse event: it has to read the position from somewhere.
    at: Rc<StdCell<Point<Pixels>>>,
}

/// The pointer's state plus the view to repaint when it changes. Built once
/// a frame, because the view id is only known while the frame is being
/// built.
#[derive(Clone)]
struct PointerFrame {
    pointer: Pointer,
    view: EntityId,
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

        let Some((left, width)) = data.lane_span(cell.column) else {
            return;
        };
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
        Self {
            columns,
            rows,
            widths,
        }
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
        Some((
            GUTTER_WIDTH + self.widths[..column].iter().sum::<f32>(),
            width,
        ))
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

/// What a cell paints for a value.
///
/// **A row is 28 pixels tall, so only the first line of a value can be
/// on screen**, and a value with a newline in it is not merely too long:
/// GPUI lays a shaped text out line by line whatever `truncate` says, so
/// one pretty-printed `jsonb` would push every row under it out of line.
/// So the cut is made here rather than left to the ellipsis, and the
/// first line is cut the way a long single-line value already is.
///
/// `…` says the cut was made, in the character `Value::display` already
/// uses for a shortened `bytea`. Nothing else is dropped: ⏎ over the cell
/// or a double click opens the value whole, and ⌘C copies it whole.
pub fn cell_text(value: &Value) -> String {
    let text = value.display();
    // A value that merely ends in a newline has nothing after it worth
    // marking, and a `text` column full of them would wear an ellipsis on
    // every row for no dropped word.
    let body = text.trim_end_matches(['\n', '\r']);
    let first = body.split('\n').next().unwrap_or_default();
    // A CRLF buffer would otherwise leave the carriage return on the end
    // of every line, which shapes as a box or as nothing at all.
    let first = first.strip_suffix('\r').unwrap_or(first);
    let mut out: String = first.chars().take(CELL_CHARS).collect();
    if out.len() < body.len() {
        out.push('…');
    }
    out
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
                .map(|value| cell_text(value).chars().count())
                .max()
                .unwrap_or(0)
                .max(name.chars().count());
            (widest as f32 * CHAR_WIDTH + CELL_PADDING).clamp(MIN_COLUMN_WIDTH, MAX_COLUMN_WIDTH)
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
        Self {
            id: id.into(),
            data,
            state,
            selection,
            first_row: 1,
            on_hit: None,
        }
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
        let Self {
            id,
            data,
            state,
            selection,
            first_row,
            on_hit,
        } = self;
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
        let rows = row_list(
            &id,
            data.clone(),
            state,
            marks.clone(),
            first_row,
            on_hit.clone(),
        );

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
            // The drag lives on the window, not on a hitbox, so it goes on
            // working past the edges of the pane. It paints nothing and
            // takes no room.
            .children(on_hit.clone().map(|on_hit| {
                div().absolute().w(px(0.)).h(px(0.)).child(DragSurface {
                    data: data.clone(),
                    state: (*state).clone(),
                    on_hit,
                })
            }))
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

/// A drag past the pane's edge, in pixels of scroll per frame. The floor
/// keeps a pointer one pixel over the line moving at a readable speed; the
/// ceiling keeps a pointer flung to the far side of the screen from
/// crossing a 500-row page in three frames.
const AUTOSCROLL_MIN: f32 = 6.;
const AUTOSCROLL_MAX: f32 = 48.;

/// The drag's own mouse handlers, and the frames that carry an autoscroll.
///
/// Both live on the **window** rather than on a hitbox, which is the
/// scrollbar's pattern and is here for a sharper reason: the whole point of
/// a drag past the edge is that the pointer has left the cells, so an
/// element that only hears about its own bounds would go deaf exactly when
/// it is needed. It also means the cells themselves carry no move handler —
/// one listener a grid, instead of one for every cell on screen.
///
/// The cell under the pointer is worked out from the geometry rather than
/// asked of the elements, for the same reason: past the edge there is no
/// element to ask, and the nearest cell is what the drag is reaching for.
///
/// It paints nothing and takes no room. It is a place in the frame to hang
/// listeners from.
struct DragSurface {
    data: Rc<GridData>,
    state: GridState,
    on_hit: OnHit,
}

impl IntoElement for DragSurface {
    type Element = Self;

    fn into_element(self) -> Self::Element {
        self
    }
}

impl Element for DragSurface {
    type RequestLayoutState = ();
    type PrepaintState = ();

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, ()) {
        (window.request_layout(Style::default(), [], cx), ())
    }

    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        _: Bounds<Pixels>,
        _: &mut (),
        _: &mut Window,
        _: &mut App,
    ) {
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        _: Bounds<Pixels>,
        _: &mut (),
        _: &mut (),
        window: &mut Window,
        _: &mut App,
    ) {
        let pointer = self.state.pointer.clone();
        let rows = self.state.rows_handle();
        let columns = self.state.columns.clone();

        window.on_mouse_event({
            let (pointer, rows, columns) = (pointer.clone(), rows.clone(), columns.clone());
            let (data, on_hit) = (self.data.clone(), self.on_hit.clone());
            move |event: &MouseMoveEvent, phase, window, cx| {
                if !phase.bubble() || !pointer.dragging.get() {
                    return;
                }
                // A button that came up somewhere nothing here heard about
                // ends the drag, rather than leaving it out for ever.
                if event.pressed_button != Some(MouseButton::Left) {
                    pointer.dragging.set(false);
                    return;
                }
                pointer.at.set(event.position);
                drag_to(event.position, &data, &rows, &columns, &on_hit, window, cx);
            }
        });

        window.on_mouse_event({
            let pointer = pointer.clone();
            move |event: &MouseUpEvent, phase, _window, _cx| {
                if phase.bubble() && event.button == MouseButton::Left {
                    pointer.dragging.set(false);
                }
            }
        });

        // Past an edge with the button still down, the grid scrolls itself:
        // the cells the user is reaching for are the ones not on screen, and
        // a pointer held still out there sends no more events to act on. So
        // the scroll is per *frame*, and each frame asks for the next by
        // moving the selection, which repaints.
        if !pointer.dragging.get() {
            return;
        }
        let at = pointer.at.get();
        let down = autoscroll_step(at.y, rows.bounds().top(), rows.bounds().bottom());
        let across = autoscroll_step(at.x, columns.bounds().left(), columns.bounds().right());
        let next_rows = stepped_offset(&rows, true, down);
        let next_columns = stepped_offset(&columns, false, across);
        // Nothing left to travel on either axis ends the loop, or a drag
        // held past the last row would repaint for ever.
        if next_rows.is_none() && next_columns.is_none() {
            return;
        }
        let (data, on_hit) = (self.data.clone(), self.on_hit.clone());
        window.on_next_frame(move |window, cx| {
            if let Some(offset) = next_rows {
                rows.set_offset(offset);
            }
            if let Some(offset) = next_columns {
                columns.set_offset(offset);
            }
            drag_to(at, &data, &rows, &columns, &on_hit, window, cx);
        });
    }
}

/// Grow the range to whatever cell the pointer is over. Outside the pane
/// that is the nearest cell, which is the one the drag is reaching for.
fn drag_to(
    at: Point<Pixels>,
    data: &GridData,
    rows: &ScrollHandle,
    columns: &ScrollHandle,
    on_hit: &OnHit,
    window: &mut Window,
    cx: &mut App,
) {
    let row = row_at(
        f32::from(at.y),
        f32::from(rows.bounds().top()),
        f32::from(rows.offset().y),
    );
    let column = column_at(
        f32::from(at.x),
        f32::from(columns.bounds().left()),
        f32::from(columns.offset().x),
        &data.widths,
    );
    let (Some(row), Some(column)) = (row.filter(|_| !data.rows.is_empty()), column) else {
        return;
    };
    let cell = Cell::new(row.min(data.rows.len() - 1), column);
    on_hit(Hit::Drag { cell }, window, cx);
}

/// Which row a pointer at `y` is over, counting from the pane's top. The
/// scroll offset runs negative as the content moves up, so it is taken away
/// rather than added. Above the first row answers the first row: a drag has
/// to reach the top of the page, and there is nothing else up there.
fn row_at(y: f32, top: f32, offset: f32) -> Option<usize> {
    let row = ((y - top - offset) / ROW_HEIGHT).floor();
    (row.is_finite()).then(|| row.max(0.) as usize)
}

/// Which lane a pointer at `x` is over. The gutter counts, because it
/// scrolls with the content; a pointer over it is over the first lane, for
/// the same reason a pointer above the first row is over the first row.
fn column_at(x: f32, left: f32, offset: f32, widths: &[f32]) -> Option<usize> {
    if widths.is_empty() {
        return None;
    }
    let mut position = x - left - offset - GUTTER_WIDTH;
    for (ix, width) in widths.iter().enumerate() {
        if position < *width {
            return Some(ix);
        }
        position -= width;
    }
    // Past the last lane's own width: the last lane takes the slack, so
    // that is still the last lane.
    Some(widths.len() - 1)
}

/// How far one frame of a drag scrolls, from how far the pointer is past
/// the pane's edge. Nothing at all while the pointer is inside it — a drag
/// that scrolled from the middle of the pane could never be made to stop.
fn autoscroll_step(position: Pixels, low: Pixels, high: Pixels) -> f32 {
    let (position, low, high) = (f32::from(position), f32::from(low), f32::from(high));
    let past = if position < low {
        position - low
    } else if position > high {
        position - high
    } else {
        return 0.;
    };
    past.signum() * past.abs().clamp(AUTOSCROLL_MIN, AUTOSCROLL_MAX)
}

/// Where a scroll handle lands after one frame of autoscroll, or `None`
/// when the content cannot travel that way any further.
fn stepped_offset(handle: &ScrollHandle, vertical: bool, step: f32) -> Option<Point<Pixels>> {
    if step == 0. {
        return None;
    }
    let offset = handle.offset();
    let max = handle.max_offset();
    let (travelled, furthest) = if vertical {
        (-f32::from(offset.y), f32::from(max.y).max(0.))
    } else {
        (-f32::from(offset.x), f32::from(max.x).max(0.))
    };
    let wanted = (travelled + step).clamp(0., furthest);
    if wanted == travelled {
        return None;
    }
    Some(if vertical {
        point(offset.x, px(-wanted))
    } else {
        point(px(-wanted), offset.y)
    })
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
    let pointer = state.pointer.clone();
    let mut rows = uniform_list(
        ElementId::Name(format!("{id}-rows").into()),
        data.rows.len(),
        move |range, window, cx| {
            let colors = theme(cx).colors.clone();
            // The view id is only knowable while a frame is being built,
            // and the hover listeners need one to ask for a repaint.
            let frame = PointerFrame {
                pointer: pointer.clone(),
                view: window.current_view(),
            };
            range
                .map(|ix| {
                    data_row(
                        ix,
                        &data.rows[ix],
                        &data.widths,
                        &marks,
                        first_row,
                        &frame,
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
        .text_color(if all {
            colors.accent_deep
        } else {
            colors.text_faint
        })
        .child(if all { TICK } else { "#" });
    if let Some(on_hit) = on_hit.clone() {
        head = head
            .cursor_pointer()
            .hover(move |s| s.bg(hover_bg))
            .on_click(move |_event, window, cx| on_hit(Hit::PickAll, window, cx));
    }
    row = row.child(head);

    let last = data.columns.len().saturating_sub(1);
    let selected_columns = marks
        .rect()
        .filter(|rect| rect.rows() == extent.rows.max(1));
    let cursor_column = marks.cursor().map(|cursor| cursor.column);
    for (ix, name) in data.columns.iter().enumerate() {
        // A column reads as selected only when the range covers all of it,
        // which is what clicking its header does.
        let whole = selected_columns.is_some_and(|rect| (rect.left..=rect.right).contains(&ix));
        // The header also says which column the cursor is in. It is the mark
        // that survives scrolling: the cursor's own cell can be a hundred
        // rows down the page, and after a jump to a column it is the only
        // thing that says the jump landed.
        let current = cursor_column == Some(ix);
        let mut cell = lane(div().id(ix), data.widths[ix], ix == last)
            .relative()
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
        if current {
            // Drawn as a child rather than as a bottom border, because a
            // border carries one colour for all four sides and the left
            // hairline has already claimed it — and because an absolutely
            // positioned rule takes no room, so the name above it does not
            // move as the cursor changes lane.
            cell = cell.text_color(colors.accent_deep).child(
                div()
                    .absolute()
                    .left(px(0.))
                    .right(px(0.))
                    .bottom(px(0.))
                    .h(px(2.))
                    .bg(colors.accent),
            );
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
    frame: &PointerFrame,
    on_hit: Option<OnHit>,
    colors: &theme::ThemeColors,
) -> Stateful<Div> {
    let picked = marks.is_picked(ix);
    // The gutter is the one target that speaks for the whole record, so it
    // is the one target that washes the whole row. A cell washes itself:
    // a click there marks that cell, and a hover must say what a click
    // would do rather than promise the row.
    let row_hovered = frame.pointer.hover_row.get() == Some(ix);
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
    } else if row_hovered {
        row = row.bg(colors.panel);
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
        .child(if picked {
            TICK.to_string()
        } else {
            (first_row + ix).to_string()
        });
    if let Some(on_hit) = on_hit.clone() {
        let gutter_hover = colors.hairline;
        let hover_row = frame.pointer.hover_row.clone();
        let view = frame.view;
        gutter = gutter
            .hover(move |s| s.bg(gutter_hover))
            // The row wash is painted by the row, and no hover style
            // reaches up to a parent — so the row is told which gutter the
            // pointer is over, and repaints on the change.
            .on_hover(move |hovered, _window, cx| {
                let now = (*hovered).then_some(ix);
                // A leave for a row that is not the marked one is the tail
                // of a move onto another row, which has already said so.
                if hover_row.get() == now || (!*hovered && hover_row.get() != Some(ix)) {
                    return;
                }
                hover_row.set(now);
                cx.notify(view);
            })
            .on_click(move |event, window, cx| {
                on_hit(
                    Hit::Pick {
                        row: ix,
                        through: event.modifiers().shift,
                    },
                    window,
                    cx,
                );
            });
    }
    row = row.child(gutter);

    let last = widths.len().saturating_sub(1);
    for (column, value) in values.iter().enumerate() {
        // A row can be shorter than the header when a driver returns
        // ragged rows; lay out only what the header has room for.
        let Some(width) = widths.get(column) else {
            break;
        };
        let cursor = marks.is_cursor(ix, column);
        let mut cell = lane(div().id(column), *width, column == last)
            .px(px(12.))
            .h_full()
            .flex()
            .items_center()
            .overflow_hidden()
            .truncate()
            .text_color(value_color(value, colors))
            .child(cell_text(value));
        // The cursor's cell is the strongest mark on screen, the rest of
        // the range a wash under it. Neither carries a border: a border
        // would take a pixel out of the cell's content box and shift the
        // value inside it every time the cursor moved.
        if cursor {
            cell = cell.bg(colors.match_strong).text_color(colors.text);
        } else if marks.contains(ix, column) {
            cell = cell.bg(colors.range_surface);
        } else if !picked && !row_hovered {
            // Hovering says "clickable", and here it says it of one cell,
            // because one cell is what a click marks. A cell already
            // carrying a mark says something truer, so it keeps it.
            let hover_bg = colors.panel;
            cell = cell.hover(move |s| s.bg(hover_bg));
        }
        if let Some(on_hit) = on_hit.clone() {
            let at = Cell::new(ix, column);
            // On the press, not the release: a drag has to start from the
            // cell the button went down on, and the release may be three
            // cells away — or off the pane entirely. Where it goes from
            // here is `DragSurface`'s, on the window.
            let pressed = frame.pointer.dragging.clone();
            let position = frame.pointer.at.clone();
            cell = cell.on_mouse_down(MouseButton::Left, move |event, window, cx| {
                pressed.set(true);
                position.set(event.position);
                let hit = Hit::Cell {
                    cell: at,
                    extend: event.modifiers.shift,
                    peek: event.click_count >= 2,
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
    fn a_cell_paints_the_first_line_of_a_value() {
        assert_eq!(cell_text(&text("one\ntwo\nthree")), "one…");
        assert_eq!(cell_text(&text("one\r\ntwo")), "one…");
    }

    #[test]
    fn a_value_short_enough_to_fit_is_painted_whole() {
        assert_eq!(cell_text(&text("ada@example.com")), "ada@example.com");
        assert_eq!(cell_text(&Value::Null), "NULL");
        // A trailing newline drops nothing anybody can read.
        assert_eq!(cell_text(&text("one\n")), "one");
    }

    #[test]
    fn a_long_line_is_cut_to_what_a_lane_can_hold() {
        let cut = cell_text(&text(&"x".repeat(CELL_CHARS * 4)));
        assert_eq!(cut.chars().count(), CELL_CHARS + 1);
        assert!(cut.ends_with('…'));
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

    /// A drag reads the cell out of the geometry rather than out of the
    /// elements, because past the pane's edge there is no element to ask.
    #[test]
    fn a_drag_finds_the_row_under_the_pointer() {
        // A pane whose top edge is 100px down the window, unscrolled.
        assert_eq!(row_at(100., 100., 0.), Some(0));
        assert_eq!(row_at(100. + ROW_HEIGHT - 0.1, 100., 0.), Some(0));
        assert_eq!(row_at(100. + ROW_HEIGHT, 100., 0.), Some(1));
        // Ten rows scrolled away: the offset runs negative.
        assert_eq!(row_at(100., 100., -10. * ROW_HEIGHT), Some(10));
        // Above the pane there is nothing but the first row to reach for.
        assert_eq!(row_at(20., 100., 0.), Some(0));
    }

    #[test]
    fn a_drag_finds_the_lane_under_the_pointer() {
        let widths = [56., 100.];
        // The gutter scrolls with the content, so it is part of the sums —
        // and a pointer over it is over the first lane.
        assert_eq!(column_at(10., 0., 0., &widths), Some(0));
        assert_eq!(column_at(GUTTER_WIDTH, 0., 0., &widths), Some(0));
        assert_eq!(column_at(GUTTER_WIDTH + 55.9, 0., 0., &widths), Some(0));
        assert_eq!(column_at(GUTTER_WIDTH + 56., 0., 0., &widths), Some(1));
        // The last lane takes the slack, so past its measured width is
        // still the last lane.
        assert_eq!(column_at(5_000., 0., 0., &widths), Some(1));
        // Scrolled one lane across.
        assert_eq!(column_at(GUTTER_WIDTH, 0., -56., &widths), Some(1));
        assert_eq!(column_at(10., 0., 0., &[]), None);
    }

    /// Inside the pane a drag scrolls nothing: a drag that crept while the
    /// pointer sat in the middle of the result could never be made to stop.
    #[test]
    fn a_drag_scrolls_only_once_it_is_past_the_edge() {
        assert_eq!(autoscroll_step(px(300.), px(100.), px(500.)), 0.);
        assert_eq!(autoscroll_step(px(100.), px(100.), px(500.)), 0.);
        assert_eq!(autoscroll_step(px(500.), px(100.), px(500.)), 0.);
    }

    #[test]
    fn a_drag_past_the_edge_scrolls_by_how_far_past_it_is() {
        // A hair over the line still moves, and moves readably.
        assert_eq!(
            autoscroll_step(px(501.), px(100.), px(500.)),
            AUTOSCROLL_MIN
        );
        assert_eq!(
            autoscroll_step(px(99.), px(100.), px(500.)),
            -AUTOSCROLL_MIN
        );
        // In between, the overshoot is the speed.
        assert_eq!(autoscroll_step(px(520.), px(100.), px(500.)), 20.);
        assert_eq!(autoscroll_step(px(80.), px(100.), px(500.)), -20.);
        // Flung to the far side of the screen, it is still bounded.
        assert_eq!(
            autoscroll_step(px(2_000.), px(100.), px(500.)),
            AUTOSCROLL_MAX
        );
        assert_eq!(
            autoscroll_step(px(-900.), px(100.), px(500.)),
            -AUTOSCROLL_MAX
        );
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
        assert_eq!(
            (left, width),
            (GUTTER_WIDTH + data.widths[0], data.widths[1])
        );
        assert_eq!(data.lane_span(2), None);
    }
}
