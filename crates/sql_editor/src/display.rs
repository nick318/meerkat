//! What the buffer looks like on screen, where that differs from its bytes.
//!
//! Two things differ. A **tab** is one byte and several columns: the text
//! system shapes it as a glyph of no useful width, so a pasted script
//! indented with tabs painted its caret somewhere the text was not. The
//! buffer keeps the tab — rewriting it would change a string literal that
//! holds one — and the line is shaped from an expanded copy, with the two
//! sets of offsets mapped through [`Expanded`].
//!
//! And a buffer is **mostly off screen**: a pasted script of a thousand
//! lines used to be shaped whole and given a gutter row per line on every
//! frame, which is where the lag after a paste came from. [`visible_rows`]
//! is the window both the gutter and the text paint.

use std::borrow::Cow;
use std::ops::Range;

/// Columns between tab stops.
pub const TAB_SIZE: usize = 4;

/// Rows shaped past each edge of the pane, so a scroll gesture that lands
/// between two frames has text to show and a drag past the edge has a row
/// to reach for.
const OVERSCAN: usize = 4;

/// One line with its tabs expanded to spaces, and the way back.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct Expanded {
    /// Byte index of each tab in the line, and how many columns it takes.
    tabs: Vec<(usize, usize)>,
}

impl Expanded {
    /// The text to shape for `line`, and the map between its offsets and
    /// the line's own. A line without a tab is borrowed as it is.
    pub fn new(line: &str) -> (Cow<'_, str>, Self) {
        if !line.contains('\t') {
            return (Cow::Borrowed(line), Self::default());
        }
        let mut text = String::with_capacity(line.len() + TAB_SIZE);
        let mut tabs = Vec::new();
        let mut column = 0;
        for (ix, ch) in line.char_indices() {
            if ch == '\t' {
                let width = TAB_SIZE - column % TAB_SIZE;
                tabs.push((ix, width));
                text.extend(std::iter::repeat_n(' ', width));
                column += width;
            } else {
                text.push(ch);
                column += 1;
            }
        }
        (Cow::Owned(text), Self { tabs })
    }

    /// A byte offset in the line, as an offset in the shaped text.
    pub fn to_display(&self, ix: usize) -> usize {
        ix + self
            .tabs
            .iter()
            .take_while(|(at, _)| *at < ix)
            .map(|(_, width)| width - 1)
            .sum::<usize>()
    }

    /// An offset in the shaped text, as a byte offset in the line. A point
    /// inside a tab's run of spaces snaps to whichever side of the tab is
    /// nearer, as a click on one does in any editor.
    pub fn to_buffer(&self, display: usize) -> usize {
        let mut shift = 0;
        for (at, width) in &self.tabs {
            let start = at + shift;
            if display <= start {
                break;
            }
            if display < start + width {
                return if display - start < start + width - display {
                    *at
                } else {
                    at + 1
                };
            }
            shift += width - 1;
        }
        display - shift
    }
}

/// How many columns `line` takes up to byte `ix`, tabs expanded.
pub fn column_of(line: &str, ix: usize) -> usize {
    let mut column = 0;
    for (at, ch) in line.char_indices() {
        if at >= ix {
            break;
        }
        column += if ch == '\t' {
            TAB_SIZE - column % TAB_SIZE
        } else {
            1
        };
    }
    column
}

/// The byte in `line` nearest to `column`, tabs expanded: the way ↑ and ↓
/// keep the caret under where it was.
pub fn index_at_column(line: &str, column: usize) -> usize {
    let mut at_column = 0;
    for (ix, ch) in line.char_indices() {
        let width = if ch == '\t' {
            TAB_SIZE - at_column % TAB_SIZE
        } else {
            1
        };
        if at_column + width > column {
            // Past the middle of a tab belongs to its far side.
            return if column - at_column > width / 2 {
                ix + ch.len_utf8()
            } else {
                ix
            };
        }
        at_column += width;
    }
    line.len()
}

/// The widest line of `text`, in columns.
pub fn widest_line(text: &str) -> usize {
    text.split('\n')
        .map(|line| column_of(line, line.len()))
        .max()
        .unwrap_or(0)
}

/// The rows a pane `viewport` tall shows when scrolled `scroll_top` into
/// content whose first row sits `padding` down, with a few to spare. A
/// pane that has not been measured yet shows the first screenful.
pub fn visible_rows(
    scroll_top: f32,
    viewport: f32,
    padding: f32,
    line_height: f32,
    count: usize,
) -> Range<usize> {
    if viewport <= 0. {
        return 0..count.min(120);
    }
    let first = ((scroll_top - padding) / line_height).floor().max(0.) as usize;
    let last = ((scroll_top + viewport - padding) / line_height)
        .ceil()
        .max(0.) as usize;
    let first = first.saturating_sub(OVERSCAN).min(count);
    first..(last + OVERSCAN).min(count).max(first)
}

/// Text from outside, made the buffer's own: every line break is `\n`. A
/// script copied out of a Windows tool carries `\r\n`, and the `\r` would
/// sit in the buffer as a character nothing paints and the caret steps
/// over.
pub fn normalize(text: &str) -> Cow<'_, str> {
    if !text.contains('\r') {
        return Cow::Borrowed(text);
    }
    Cow::Owned(text.replace("\r\n", "\n").replace('\r', "\n"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_tab_runs_to_the_next_stop() {
        let (text, map) = Expanded::new("a\tb\t\tc");
        assert_eq!(text, "a   b       c");
        // `b` is byte 2 of the line and column 4 of the screen.
        assert_eq!(map.to_display(2), 4);
        assert_eq!(map.to_display(5), 12);
        for ix in 0..=6 {
            assert_eq!(map.to_buffer(map.to_display(ix)), ix);
        }
    }

    #[test]
    fn a_point_inside_a_tab_snaps_to_its_nearer_side() {
        let (_, map) = Expanded::new("\tx");
        assert_eq!(map.to_buffer(1), 0);
        assert_eq!(map.to_buffer(3), 1);
        assert_eq!(map.to_buffer(4), 1);
        assert_eq!(map.to_buffer(5), 2);
    }

    #[test]
    fn a_line_without_a_tab_maps_onto_itself() {
        let (text, map) = Expanded::new("select 1");
        assert!(matches!(text, Cow::Borrowed(_)));
        assert_eq!(map.to_display(5), 5);
        assert_eq!(map.to_buffer(5), 5);
    }

    #[test]
    fn a_column_survives_a_line_with_tabs() {
        assert_eq!(column_of("\tid", 1), 4);
        assert_eq!(index_at_column("\tid", 4), 1);
        assert_eq!(index_at_column("\tid", 5), 2);
        assert_eq!(index_at_column("\tid", 1), 0);
        assert_eq!(index_at_column("ab", 9), 2);
        assert_eq!(index_at_column("héllo", 2), 3);
    }

    #[test]
    fn only_the_rows_on_screen_are_asked_for() {
        // 21px rows, 12px of padding, a 210px pane scrolled 2100px down.
        let rows = visible_rows(2100., 210., 12., 21., 5000);
        assert_eq!(rows, 95..114);
        // The top of the buffer and the end of it both clamp.
        assert_eq!(visible_rows(0., 210., 12., 21., 3), 0..3);
        assert_eq!(visible_rows(99999., 210., 12., 21., 10), 10..10);
        // Before the first paint there is no height to read.
        assert_eq!(visible_rows(0., 0., 12., 21., 5000), 0..120);
    }

    #[test]
    fn every_line_break_becomes_a_newline() {
        assert_eq!(normalize("a\r\nb\rc\n"), "a\nb\nc\n");
        assert!(matches!(normalize("a\nb"), Cow::Borrowed(_)));
    }
}
