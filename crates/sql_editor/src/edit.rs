//! Edits that work on whole lines: indent, comment, move, duplicate, join.
//!
//! Each one is a plain function over the text and the selection, answering
//! with the [`Edit`] to make — the range to replace, what to put there, and
//! where the selection ends up — so the rules are argued with in a test
//! rather than in a running window. The editor applies an `Edit` as one
//! undo step and decides nothing else about it.
//!
//! The keys are Zed's, because that is the editor these habits come from:
//! ⇥ and ⇧⇥ over a selection, ⌘/, ⌥↑ and ⌥↓, ⇧⌥↑ and ⇧⌥↓, ⇧⌘K, ⌃J, ⌃T.

use crate::motion::{line_end, line_start, next_boundary, previous_boundary};
use std::ops::Range;

/// One replacement, and the selection it leaves.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Edit {
    pub range: Range<usize>,
    pub text: String,
    pub selection: Range<usize>,
}

/// SQL's line comment, with the space every formatter puts after it.
const COMMENT: &str = "-- ";

/// The lines a selection touches, from the first one's start to the last
/// one's end. A selection that stops at the very start of a line does not
/// touch that line: ⇧↓ from a line's start selects one line, not two.
pub fn line_span(text: &str, selection: &Range<usize>) -> Range<usize> {
    let start = line_start(text, selection.start);
    let mut last = selection.end;
    if last > selection.start && text[..last].ends_with('\n') {
        last -= 1;
    }
    start..line_end(text, last)
}

/// Replacements in the buffer's own offsets, in order and never touching.
struct Patch(Vec<(Range<usize>, String)>);

impl Patch {
    /// Where `offset` lands once the patch is applied. A point at an
    /// insertion moves past it, so a caret at a line's start stays with
    /// the text when that line is indented — unless it `sticks`, which is
    /// the start of a selection: the marker put in front of a selected
    /// line belongs inside the selection.
    fn map(&self, offset: usize, sticks: bool) -> usize {
        let mut delta = 0isize;
        for (range, text) in &self.0 {
            let at_insertion = range.is_empty() && offset == range.start;
            if offset >= range.end && !(sticks && at_insertion) {
                delta += text.len() as isize - range.len() as isize;
            } else if offset > range.start {
                return (range.start as isize + delta) as usize + text.len();
            } else {
                break;
            }
        }
        (offset as isize + delta) as usize
    }

    fn into_edit(self, text: &str, selection: &Range<usize>) -> Option<Edit> {
        let first = self.0.first()?.0.start;
        let last = self.0.last()?.0.end;
        let selection =
            self.map(selection.start, !selection.is_empty())..self.map(selection.end, false);
        let mut replaced = String::new();
        let mut at = first;
        for (range, new) in &self.0 {
            replaced.push_str(&text[at..range.start]);
            replaced.push_str(new);
            at = range.end;
        }
        Some(Edit {
            range: first..last,
            text: replaced,
            selection,
        })
    }
}

/// Start offset of every line in `span`.
fn line_starts_in(text: &str, span: &Range<usize>) -> Vec<usize> {
    let mut starts = vec![span.start];
    starts.extend(
        text[span.clone()]
            .match_indices('\n')
            .map(|(ix, _)| span.start + ix + 1),
    );
    starts
}

fn is_blank(line: &str) -> bool {
    line.chars().all(char::is_whitespace)
}

/// ⇥ over a selection, and ⌘]: one `unit` in front of every line the
/// selection touches. A blank line is left blank.
pub fn indent(text: &str, selection: &Range<usize>, unit: &str) -> Option<Edit> {
    let span = line_span(text, selection);
    let patch = line_starts_in(text, &span)
        .into_iter()
        .filter(|start| !is_blank(&text[*start..line_end(text, *start)]))
        .map(|start| (start..start, unit.to_string()))
        .collect();
    Patch(patch).into_edit(text, selection)
}

/// ⇧⇥ and ⌘[: one `unit` of leading space off every line the selection
/// touches, or one tab, or as much as the line has.
pub fn outdent(text: &str, selection: &Range<usize>, unit: &str) -> Option<Edit> {
    let span = line_span(text, selection);
    let patch = line_starts_in(text, &span)
        .into_iter()
        .filter_map(|start| {
            let line = &text[start..];
            let taken = if line.starts_with('\t') {
                1
            } else {
                line.bytes()
                    .take(unit.len())
                    .take_while(|byte| *byte == b' ')
                    .count()
            };
            (taken > 0).then(|| (start..start + taken, String::new()))
        })
        .collect();
    Patch(patch).into_edit(text, selection)
}

/// ⌘/: comment the lines the selection touches, or uncomment them when
/// every one of them already is. The marker goes in at the shallowest
/// indent among them, so a commented block keeps its shape.
pub fn toggle_comment(text: &str, selection: &Range<usize>) -> Option<Edit> {
    let span = line_span(text, selection);
    let starts = line_starts_in(text, &span);
    let lines: Vec<(usize, &str)> = starts
        .iter()
        .map(|start| (*start, &text[*start..line_end(text, *start)]))
        .collect();
    let indent_of = |line: &str| line.len() - line.trim_start_matches([' ', '\t']).len();
    let live: Vec<&(usize, &str)> = lines.iter().filter(|(_, line)| !is_blank(line)).collect();

    let commented = !live.is_empty()
        && live
            .iter()
            .all(|(_, line)| line[indent_of(line)..].starts_with("--"));
    let patch = if commented {
        live.iter()
            .map(|(start, line)| {
                let at = start + indent_of(line);
                let marker = if text[at..].starts_with(COMMENT) {
                    COMMENT.len()
                } else {
                    2
                };
                (at..at + marker, String::new())
            })
            .collect()
    } else if live.is_empty() {
        // Nothing but blank lines: the caret's own line takes the marker,
        // so ⌘/ on an empty line starts a comment.
        vec![(selection.start..selection.start, COMMENT.to_string())]
    } else {
        let depth = live.iter().map(|(_, line)| indent_of(line)).min()?;
        live.iter()
            .map(|(start, _)| (start + depth..start + depth, COMMENT.to_string()))
            .collect()
    };
    Patch(patch).into_edit(text, selection)
}

/// ⌥↑ and ⌥↓: carry the selection's lines over the line beside them.
pub fn move_lines(text: &str, selection: &Range<usize>, down: bool) -> Option<Edit> {
    let span = line_span(text, selection);
    let block = &text[span.clone()];
    if down {
        if span.end == text.len() {
            return None;
        }
        let next = span.end + 1..line_end(text, span.end + 1);
        let shift = next.len() + 1;
        Some(Edit {
            range: span.start..next.end,
            text: format!("{}\n{block}", &text[next.clone()]),
            selection: selection.start + shift..selection.end + shift,
        })
    } else {
        if span.start == 0 {
            return None;
        }
        let previous = line_start(text, span.start - 1)..span.start - 1;
        let shift = previous.len() + 1;
        Some(Edit {
            range: previous.start..span.end,
            text: format!("{block}\n{}", &text[previous.clone()]),
            selection: selection.start - shift..selection.end - shift,
        })
    }
}

/// ⇧⌥↑ and ⇧⌥↓: a second copy of the selection's lines. Down leaves the
/// selection on the lower copy and up on the upper, which is where the
/// user was looking either way.
pub fn duplicate_lines(text: &str, selection: &Range<usize>, down: bool) -> Option<Edit> {
    let span = line_span(text, selection);
    let copy = format!("{}\n", &text[span.clone()]);
    let shift = if down { copy.len() } else { 0 };
    Some(Edit {
        range: span.start..span.start,
        text: copy,
        selection: selection.start + shift..selection.end + shift,
    })
}

/// ⇧⌘K: the selection's lines, gone, with the caret kept in its column on
/// the line that takes their place.
pub fn delete_lines(text: &str, selection: &Range<usize>) -> Option<Edit> {
    let span = line_span(text, selection);
    let column = selection.start - span.start;
    let (range, landing) = if span.end < text.len() {
        // The line below moves up into the gap.
        let below = line_end(text, span.end + 1) - (span.end + 1);
        (span.start..span.end + 1, span.start + column.min(below))
    } else if span.start > 0 {
        // The last line of the buffer: its break goes with it.
        let above = line_start(text, span.start - 1);
        let width = span.start - 1 - above;
        (span.start - 1..span.end, above + column.min(width))
    } else {
        (span.clone(), 0)
    };
    if range.is_empty() {
        return None;
    }
    let mut landing = landing;
    // The landing line is in the old text; keep it on a character.
    while !text.is_char_boundary(if landing < range.start {
        landing
    } else {
        landing + range.len()
    }) {
        landing -= 1;
    }
    Some(Edit {
        range,
        text: String::new(),
        selection: landing..landing,
    })
}

/// ⌃J: pull the next line up onto this one, with one space where the break
/// and the indent were. A selection over several lines joins all of them.
pub fn join_lines(text: &str, selection: &Range<usize>) -> Option<Edit> {
    let span = line_span(text, selection);
    let mut breaks: Vec<usize> = text[span.clone()]
        .match_indices('\n')
        .map(|(ix, _)| span.start + ix)
        .collect();
    if breaks.is_empty() {
        if span.end == text.len() {
            return None;
        }
        breaks.push(span.end);
    }
    let mut caret = selection.start;
    let patch: Vec<(Range<usize>, String)> = breaks
        .into_iter()
        .map(|at| {
            let head = &text[line_start(text, at)..at];
            let from = at - (head.len() - head.trim_end_matches([' ', '\t']).len());
            let tail = &text[at + 1..line_end(text, at + 1)];
            let to = at + 1 + (tail.len() - tail.trim_start_matches([' ', '\t']).len());
            // Nothing to keep apart when either side is empty.
            let glue = if is_blank(head) || is_blank(tail) {
                ""
            } else {
                " "
            };
            caret = from;
            (from..to, glue.to_string())
        })
        .collect();
    // The caret lands on the last join, where the two lines now meet.
    let patch = Patch(patch);
    let landing = patch.map(caret, true);
    let mut edit = patch.into_edit(text, selection)?;
    edit.selection = landing..landing;
    Some(edit)
}

/// ⌃T: swap the characters either side of the caret and step past them.
/// At the end of a line it swaps the two before it, as macOS does.
pub fn transpose(text: &str, caret: usize) -> Option<Edit> {
    let at_line_end = caret == text.len() || text[caret..].starts_with('\n');
    let (left, middle, right) = if at_line_end {
        let middle = previous_boundary(text, caret);
        (previous_boundary(text, middle), middle, caret)
    } else {
        (
            previous_boundary(text, caret),
            caret,
            next_boundary(text, caret),
        )
    };
    if left == middle || middle == right || text[left..right].contains('\n') {
        return None;
    }
    Some(Edit {
        range: left..right,
        text: format!("{}{}", &text[middle..right], &text[left..middle]),
        selection: right..right,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Apply an edit the way the editor does, and mark the selection with
    /// `[` and `]` so a test reads as the screen does.
    fn applied(text: &str, edit: Option<Edit>) -> String {
        let edit = edit.expect("an edit");
        let mut out = text.to_string();
        out.replace_range(edit.range.clone(), &edit.text);
        out.insert(edit.selection.end, ']');
        out.insert(edit.selection.start, '[');
        out
    }

    #[test]
    fn a_selection_ending_at_a_line_start_leaves_that_line_out() {
        let text = "one\ntwo\nthree";
        assert_eq!(line_span(text, &(0..4)), 0..3);
        assert_eq!(line_span(text, &(1..5)), 0..7);
        // A bare caret at a line's start is on that line.
        assert_eq!(line_span(text, &(4..4)), 4..7);
    }

    #[test]
    fn indent_and_outdent_move_every_line_and_keep_the_selection() {
        let text = "select\n\n  id";
        assert_eq!(
            applied(text, indent(text, &(2..text.len()), "  ")),
            "  se[lect\n\n    id]"
        );
        let text = "  select\n id\n\tname\nx";
        assert_eq!(
            applied(text, outdent(text, &(0..text.len()), "  ")),
            "[select\nid\nname\nx]"
        );
        assert_eq!(outdent("x", &(0..0), "  "), None);
    }

    #[test]
    fn a_comment_goes_on_and_comes_off() {
        let text = "select 1\n  from t";
        let on = toggle_comment(text, &(0..text.len()));
        assert_eq!(applied(text, on.clone()), "[-- select 1\n--   from t]");
        let mut commented = text.to_string();
        let on = on.unwrap();
        commented.replace_range(on.range, &on.text);
        assert_eq!(
            applied(
                &commented,
                toggle_comment(&commented, &(0..commented.len()))
            ),
            "[select 1\n  from t]"
        );
        // One live line in a block that is not all comments: comment it all.
        let mixed = "-- a\nb";
        assert_eq!(
            applied(mixed, toggle_comment(mixed, &(0..mixed.len()))),
            "[-- -- a\n-- b]"
        );
        assert_eq!(applied("", toggle_comment("", &(0..0))), "-- []");
        assert_eq!(applied("--x", toggle_comment("--x", &(3..3))), "x[]");
    }

    #[test]
    fn lines_move_over_their_neighbour() {
        let text = "a\nbb\nc";
        assert_eq!(applied(text, move_lines(text, &(3..3), true)), "a\nc\nb[]b");
        assert_eq!(
            applied(text, move_lines(text, &(3..3), false)),
            "b[]b\na\nc"
        );
        assert_eq!(move_lines(text, &(0..0), false), None);
        assert_eq!(move_lines(text, &(5..5), true), None);
        // A block of two travels together.
        assert_eq!(applied(text, move_lines(text, &(0..3), true)), "c\n[a\nb]b");
    }

    #[test]
    fn a_duplicate_leaves_the_selection_where_the_user_was_looking() {
        let text = "a\nb";
        assert_eq!(
            applied(text, duplicate_lines(text, &(2..3), true)),
            "a\nb\n[b]"
        );
        assert_eq!(
            applied(text, duplicate_lines(text, &(2..3), false)),
            "a\n[b]\nb"
        );
    }

    #[test]
    fn a_deleted_line_takes_its_break_with_it() {
        let text = "one\ntwo\nthree";
        assert_eq!(applied(text, delete_lines(text, &(5..5))), "one\nt[]hree");
        assert_eq!(applied(text, delete_lines(text, &(10..10))), "one\ntw[]o");
        assert_eq!(applied("only", delete_lines("only", &(2..2))), "[]");
        assert_eq!(delete_lines("", &(0..0)), None);
    }

    #[test]
    fn joined_lines_meet_at_one_space() {
        let text = "select  \n    id\nfrom t";
        assert_eq!(
            applied(text, join_lines(text, &(0..0))),
            "select[] id\nfrom t"
        );
        assert_eq!(
            applied(text, join_lines(text, &(0..text.len()))),
            "select id[] from t"
        );
        assert_eq!(applied("a\n\nb", join_lines("a\n\nb", &(0..0))), "a[]\nb");
        assert_eq!(join_lines("a", &(0..0)), None);
    }

    #[test]
    fn transpose_swaps_around_the_caret() {
        assert_eq!(applied("abc", transpose("abc", 1)), "ba[]c");
        // At the end of the line the two before the caret trade places.
        assert_eq!(applied("abc\nd", transpose("abc\nd", 3)), "acb[]\nd");
        assert_eq!(transpose("abc", 0), None);
        assert_eq!(transpose("a\nb", 2), None);
    }
}
