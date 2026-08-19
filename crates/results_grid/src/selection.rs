//! What the user has picked out of a result.
//!
//! Two things get picked, and they are not the same thing. A **range** is a
//! rectangle of cells grown from the cell the user pressed on: it is what
//! the arrow keys move and what ⌘C copies. **Picks** are whole rows ticked
//! in the gutter — discontiguous by nature, which is why they are a set and
//! not a second rectangle. A row is ticked to say "this record"; a range is
//! drawn to say "these cells", and one cannot stand for the other.
//!
//! All of it is plain data over indices, with no window and no theme in
//! sight, so the policy can be argued with in a test rather than in a
//! running window. The view reads it and paints; it decides nothing.

use crate::GridData;
use std::collections::BTreeSet;

/// One cell, by row and column index into the result on screen. Both are
/// indices into what the grid holds, never absolute row numbers in the
/// table: paging replaces the rows, and a selection does not survive it.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Cell {
    pub row: usize,
    pub column: usize,
}

impl Cell {
    pub fn new(row: usize, column: usize) -> Self {
        Self { row, column }
    }
}

/// How far one keystroke moves the cursor.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Step {
    Up,
    Down,
    Left,
    Right,
    /// ⌘← / ⌘→: the ends of the row the cursor is on.
    RowStart,
    RowEnd,
    /// ⌘↑ / ⌘↓: the first and last row, in the column the cursor is on.
    First,
    Last,
}

/// How big the result is. The cursor is clamped to it, so a selection can
/// never point past the rows in hand.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Extent {
    pub rows: usize,
    pub columns: usize,
}

impl Extent {
    pub fn of(data: &GridData) -> Self {
        Self { rows: data.rows.len(), columns: data.columns.len() }
    }

    fn is_empty(&self) -> bool {
        self.rows == 0 || self.columns == 0
    }
}

/// The rectangle a range covers, both ends inclusive.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Rect {
    pub top: usize,
    pub bottom: usize,
    pub left: usize,
    pub right: usize,
}

impl Rect {
    pub fn rows(&self) -> usize {
        self.bottom - self.top + 1
    }

    pub fn columns(&self) -> usize {
        self.right - self.left + 1
    }

    pub fn cells(&self) -> usize {
        self.rows() * self.columns()
    }

    fn holds(&self, row: usize, column: usize) -> bool {
        (self.top..=self.bottom).contains(&row) && (self.left..=self.right).contains(&column)
    }
}

/// One grid's selection, held by whoever owns the tab so it survives the
/// re-render after every keystroke.
#[derive(Clone, Default, Debug, PartialEq, Eq)]
pub struct Selection {
    /// Where the range began: the cell the user pressed on, or the cell the
    /// cursor was at when ⇧ first extended it.
    anchor: Option<Cell>,
    /// Where the range now ends. This is the cell the keys move and the one
    /// the row drawer opens on. `None` is "nothing selected".
    cursor: Option<Cell>,
    /// Rows ticked in the gutter.
    picked: BTreeSet<usize>,
    /// The last row ticked, so ⇧-click can fill the gap up to it.
    lead: Option<usize>,
}

impl Selection {
    /// The focused cell, if there is one.
    pub fn cursor(&self) -> Option<Cell> {
        self.cursor
    }

    /// Nothing marked at all — no cell, no ticked row. The status strip and
    /// ⌘C both need to know.
    pub fn is_empty(&self) -> bool {
        self.cursor.is_none() && self.picked.is_empty()
    }

    /// Put the cursor on one cell and start a fresh range there.
    pub fn focus(&mut self, cell: Cell) {
        self.anchor = Some(cell);
        self.cursor = Some(cell);
    }

    /// Grow the range to `cell`, keeping the anchor where it was. With
    /// nothing selected yet this is the same as a plain click: ⇧ on a fresh
    /// result has no anchor to grow from.
    pub fn extend_to(&mut self, cell: Cell) {
        match self.anchor {
            Some(_) => self.cursor = Some(cell),
            None => self.focus(cell),
        }
    }

    /// The whole column, from its first row to its last.
    pub fn select_column(&mut self, column: usize, extent: Extent) {
        if extent.is_empty() || column >= extent.columns {
            return;
        }
        self.anchor = Some(Cell::new(0, column));
        self.cursor = Some(Cell::new(extent.rows - 1, column));
    }

    /// Every cell in the result.
    pub fn select_all(&mut self, extent: Extent) {
        if extent.is_empty() {
            return;
        }
        self.anchor = Some(Cell::new(0, 0));
        self.cursor = Some(Cell::new(extent.rows - 1, extent.columns - 1));
    }

    /// Move the cursor. `extend` keeps the anchor, so ⇧↓ grows the range
    /// where ↓ moves it whole. Returns the cell the cursor landed on, so
    /// the caller can scroll it into view; `None` when nothing moved.
    ///
    /// With no cursor yet the first keystroke lands on the first cell rather
    /// than doing nothing: a result that has just come back is one the user
    /// can start walking without clicking it first.
    pub fn step(&mut self, step: Step, extend: bool, extent: Extent) -> Option<Cell> {
        if extent.is_empty() {
            return None;
        }
        let last_row = extent.rows - 1;
        let last_column = extent.columns - 1;
        let Some(from) = self.cursor else {
            let cell = Cell::new(0, 0);
            self.focus(cell);
            return Some(cell);
        };

        let to = match step {
            Step::Up => Cell::new(from.row.saturating_sub(1), from.column),
            Step::Down => Cell::new((from.row + 1).min(last_row), from.column),
            Step::Left => Cell::new(from.row, from.column.saturating_sub(1)),
            Step::Right => Cell::new(from.row, (from.column + 1).min(last_column)),
            Step::RowStart => Cell::new(from.row, 0),
            Step::RowEnd => Cell::new(from.row, last_column),
            Step::First => Cell::new(0, from.column),
            Step::Last => Cell::new(last_row, from.column),
        };
        if to == from {
            return None;
        }
        if extend {
            self.extend_to(to);
        } else {
            self.focus(to);
        }
        Some(to)
    }

    /// The rectangle the range covers, if a range is drawn.
    pub fn rect(&self) -> Option<Rect> {
        let (anchor, cursor) = (self.anchor?, self.cursor?);
        Some(Rect {
            top: anchor.row.min(cursor.row),
            bottom: anchor.row.max(cursor.row),
            left: anchor.column.min(cursor.column),
            right: anchor.column.max(cursor.column),
        })
    }

    /// Is this cell inside the range? The cursor's own cell is, and the
    /// view paints it a shade stronger.
    pub fn contains(&self, row: usize, column: usize) -> bool {
        self.rect().is_some_and(|rect| rect.holds(row, column))
    }

    pub fn is_cursor(&self, row: usize, column: usize) -> bool {
        self.cursor == Some(Cell::new(row, column))
    }

    /// Tick a row, or untick it. The row becomes the lead, so a ⇧-click
    /// after it fills the gap from here.
    pub fn toggle_pick(&mut self, row: usize) {
        if !self.picked.remove(&row) {
            self.picked.insert(row);
        }
        self.lead = Some(row);
    }

    /// ⇧-click in the gutter: tick every row from the last one ticked to
    /// this one. With no lead yet it ticks the one row, which is what a
    /// plain click would have done.
    pub fn pick_through(&mut self, row: usize) {
        let from = self.lead.unwrap_or(row);
        let (low, high) = (from.min(row), from.max(row));
        for r in low..=high {
            self.picked.insert(r);
        }
        self.lead = Some(row);
    }

    /// The gutter's header tick: all of them, or none. Ticking all of a
    /// ticked result clears it, so one target does both.
    pub fn toggle_all_picks(&mut self, extent: Extent) {
        if self.all_picked(extent) {
            self.picked.clear();
            self.lead = None;
        } else {
            self.picked = (0..extent.rows).collect();
            self.lead = extent.rows.checked_sub(1);
        }
    }

    pub fn is_picked(&self, row: usize) -> bool {
        self.picked.contains(&row)
    }

    pub fn all_picked(&self, extent: Extent) -> bool {
        extent.rows > 0 && self.picked.len() == extent.rows
    }

    pub fn picks(&self) -> usize {
        self.picked.len()
    }

    pub fn picked_rows(&self) -> impl Iterator<Item = usize> + '_ {
        self.picked.iter().copied()
    }

    /// ⎋ and every new result: nothing marked.
    pub fn clear(&mut self) {
        *self = Self::default();
    }

    /// What the status strip says about the selection, or nothing when
    /// there is nothing worth saying — a single focused cell is not news.
    pub fn summary(&self) -> Option<String> {
        let mut parts = Vec::new();
        match self.picked.len() {
            0 => {}
            1 => parts.push("1 row picked".to_string()),
            n => parts.push(format!("{n} rows picked")),
        }
        if let Some(rect) = self.rect().filter(|rect| rect.cells() > 1) {
            parts.push(format!("{} × {} cells", rect.rows(), rect.columns()));
        }
        (!parts.is_empty()).then(|| parts.join(" · "))
    }
}

/// What ⌘C and "copy row" put on the clipboard: **CSV**, one line per row.
///
/// **Ticked rows win over the range, and they are copied whole.** Ticking
/// rows says "these records", so cutting them down to whatever rectangle
/// happened to be drawn as well would answer a question nobody asked.
///
/// No header line either way. A copy pastes back exactly what was marked,
/// and a heading nobody marked is a surprise wherever it lands.
///
/// A NULL copies as an empty field. It is an absence, and pasting the word
/// `NULL` into a sheet would make it data.
pub fn clipboard_text(data: &GridData, selection: &Selection) -> Option<String> {
    if selection.picks() > 0 {
        let lines: Vec<String> = selection
            .picked_rows()
            .filter_map(|row| data.rows.get(row))
            .map(|values| values.iter().map(field).collect::<Vec<_>>().join(SEPARATOR))
            .collect();
        return (!lines.is_empty()).then(|| lines.join("\n"));
    }

    let rect = selection.rect()?;
    let lines: Vec<String> = (rect.top..=rect.bottom)
        .filter_map(|row| data.rows.get(row))
        .map(|values| {
            (rect.left..=rect.right)
                .map(|column| values.get(column).map(field).unwrap_or_default())
                .collect::<Vec<_>>()
                .join(SEPARATOR)
        })
        .collect();
    (!lines.is_empty()).then(|| lines.join("\n"))
}

/// What separates one field from the next. A comma, so what lands on the
/// clipboard is a CSV row: the thing that can be pasted into a file, a
/// `COPY … FROM stdin`, or a ticket, and still read as the row it came from.
const SEPARATOR: &str = ",";

/// One value as one CSV field, by RFC 4180's rules.
///
/// A value holding the separator, a quote or a line break is wrapped in
/// quotes with its own quotes doubled. Without that, one cell with a comma
/// in it would paste as two fields, and one with a newline as two rows,
/// shearing everything below it out of line.
fn field(value: &db_client::Value) -> String {
    let text = match value {
        db_client::Value::Null => return String::new(),
        other => other.display(),
    };
    if text.contains([',', '\n', '\r', '"']) {
        format!("\"{}\"", text.replace('"', "\"\""))
    } else {
        text
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use db_client::Value;

    fn data() -> GridData {
        GridData::new(
            vec!["id".to_string(), "email".to_string(), "plan".to_string()],
            vec![
                vec![Value::Int(1041), Value::Text("ida@northwind.io".into()), Value::Text("scale".into())],
                vec![Value::Int(1040), Value::Text("m.okafor@lumen.dev".into()), Value::Null],
                vec![
                    Value::Int(1039),
                    Value::Text("Reviewed report, prepared comments".into()),
                    Value::Text("pro".into()),
                ],
            ],
        )
    }

    fn extent() -> Extent {
        Extent { rows: 3, columns: 3 }
    }

    #[test]
    fn a_range_grows_from_the_anchor_both_ways() {
        let mut selection = Selection::default();
        selection.focus(Cell::new(1, 1));
        selection.extend_to(Cell::new(0, 2));
        assert_eq!(selection.rect(), Some(Rect { top: 0, bottom: 1, left: 1, right: 2 }));
        // Dragging back past the anchor turns the rectangle over rather
        // than emptying it.
        selection.extend_to(Cell::new(2, 0));
        assert_eq!(selection.rect(), Some(Rect { top: 1, bottom: 2, left: 0, right: 1 }));
        assert!(selection.contains(2, 0));
        assert!(!selection.contains(0, 0));
        assert!(selection.is_cursor(2, 0));
    }

    /// ⇧ with nothing selected has no anchor to grow from, so it lands as
    /// a plain click instead of doing nothing.
    #[test]
    fn extending_from_nothing_selects_the_one_cell() {
        let mut selection = Selection::default();
        selection.extend_to(Cell::new(1, 2));
        assert_eq!(selection.rect(), Some(Rect { top: 1, bottom: 1, left: 2, right: 2 }));
    }

    #[test]
    fn the_cursor_stops_at_the_edges() {
        let mut selection = Selection::default();
        selection.focus(Cell::new(0, 0));
        assert_eq!(selection.step(Step::Up, false, extent()), None);
        assert_eq!(selection.step(Step::Left, false, extent()), None);
        assert_eq!(selection.step(Step::Last, false, extent()), Some(Cell::new(2, 0)));
        assert_eq!(selection.step(Step::Down, false, extent()), None);
        assert_eq!(selection.step(Step::RowEnd, false, extent()), Some(Cell::new(2, 2)));
        assert_eq!(selection.step(Step::Right, false, extent()), None);
    }

    /// Walking without ⇧ moves the range whole; walking with it leaves the
    /// anchor where it was.
    #[test]
    fn shift_keeps_the_anchor_and_a_plain_step_does_not() {
        let mut selection = Selection::default();
        selection.focus(Cell::new(0, 0));
        selection.step(Step::Down, true, extent());
        selection.step(Step::Right, true, extent());
        assert_eq!(selection.rect(), Some(Rect { top: 0, bottom: 1, left: 0, right: 1 }));

        selection.step(Step::Down, false, extent());
        assert_eq!(selection.rect(), Some(Rect { top: 2, bottom: 2, left: 1, right: 1 }));
    }

    /// The first keystroke on a result nobody has clicked lands on the
    /// first cell.
    #[test]
    fn the_first_keystroke_needs_no_click_before_it() {
        let mut selection = Selection::default();
        assert_eq!(selection.step(Step::Down, false, extent()), Some(Cell::new(0, 0)));
        assert!(selection.is_cursor(0, 0));
    }

    #[test]
    fn an_empty_result_takes_no_selection() {
        let empty = Extent { rows: 0, columns: 0 };
        let mut selection = Selection::default();
        assert_eq!(selection.step(Step::Down, false, empty), None);
        selection.select_all(empty);
        selection.select_column(0, empty);
        assert!(selection.is_empty());
    }

    #[test]
    fn a_column_and_the_whole_result_are_ranges_too() {
        let mut selection = Selection::default();
        selection.select_column(1, extent());
        assert_eq!(selection.rect(), Some(Rect { top: 0, bottom: 2, left: 1, right: 1 }));
        selection.select_all(extent());
        assert_eq!(selection.rect(), Some(Rect { top: 0, bottom: 2, left: 0, right: 2 }));
    }

    #[test]
    fn ticking_a_row_twice_unticks_it() {
        let mut selection = Selection::default();
        selection.toggle_pick(1);
        assert!(selection.is_picked(1));
        selection.toggle_pick(1);
        assert!(!selection.is_picked(1));
        assert_eq!(selection.picks(), 0);
    }

    #[test]
    fn shift_clicking_the_gutter_fills_the_gap() {
        let mut selection = Selection::default();
        selection.toggle_pick(0);
        selection.pick_through(2);
        assert_eq!(selection.picked_rows().collect::<Vec<_>>(), vec![0, 1, 2]);
        assert!(selection.all_picked(extent()));

        // With no lead, ⇧ ticks the one row it landed on.
        let mut fresh = Selection::default();
        fresh.pick_through(2);
        assert_eq!(fresh.picked_rows().collect::<Vec<_>>(), vec![2]);
    }

    /// One target does both, so a fully ticked result clears on the same
    /// click that ticked it.
    #[test]
    fn the_header_tick_takes_all_of_them_and_gives_them_back() {
        let mut selection = Selection::default();
        selection.toggle_all_picks(extent());
        assert!(selection.all_picked(extent()));
        selection.toggle_all_picks(extent());
        assert_eq!(selection.picks(), 0);
    }

    #[test]
    fn copying_a_range_takes_the_cells_it_covers() {
        let data = data();
        let mut selection = Selection::default();
        selection.focus(Cell::new(0, 0));
        selection.extend_to(Cell::new(1, 1));
        assert_eq!(
            clipboard_text(&data, &selection).unwrap(),
            "1041,ida@northwind.io\n1040,m.okafor@lumen.dev"
        );
    }

    /// Ticked rows are copied whole, whatever rectangle is also drawn: the
    /// tick says "this record".
    #[test]
    fn copying_ticked_rows_takes_them_whole() {
        let data = data();
        let mut selection = Selection::default();
        selection.focus(Cell::new(0, 0));
        selection.toggle_pick(1);
        assert_eq!(
            clipboard_text(&data, &selection).unwrap(),
            // The NULL is an absence, so the field is empty.
            "1040,m.okafor@lumen.dev,"
        );
    }

    /// A value holding the separator would read as two fields, so it is
    /// quoted — and a quote inside it is doubled, as RFC 4180 has it.
    #[test]
    fn a_value_holding_a_comma_is_quoted() {
        let data = data();
        let mut selection = Selection::default();
        selection.focus(Cell::new(2, 1));
        assert_eq!(
            clipboard_text(&data, &selection).unwrap(),
            "\"Reviewed report, prepared comments\""
        );

        let quoted = GridData::new(
            vec!["note".to_string()],
            vec![vec![Value::Text("she said \"yes\"".into())]],
        );
        let mut one = Selection::default();
        one.focus(Cell::new(0, 0));
        assert_eq!(clipboard_text(&quoted, &one).unwrap(), "\"she said \"\"yes\"\"\"");

        // A line break inside a value would paste as two rows.
        let wrapped = GridData::new(
            vec!["note".to_string()],
            vec![vec![Value::Text("first\nsecond".into())]],
        );
        assert_eq!(clipboard_text(&wrapped, &one).unwrap(), "\"first\nsecond\"");
    }

    #[test]
    fn nothing_selected_copies_nothing() {
        assert_eq!(clipboard_text(&data(), &Selection::default()), None);
    }

    #[test]
    fn the_status_strip_says_nothing_about_one_cell() {
        let mut selection = Selection::default();
        selection.focus(Cell::new(0, 0));
        assert_eq!(selection.summary(), None);

        selection.extend_to(Cell::new(1, 2));
        assert_eq!(selection.summary().unwrap(), "2 × 3 cells");

        selection.toggle_pick(0);
        assert_eq!(selection.summary().unwrap(), "1 row picked · 2 × 3 cells");
    }
}
