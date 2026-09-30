//! Finding a value in a result.
//!
//! ⌘J finds a column by its name; ⌘F finds a **cell by what it holds**. A
//! result of five hundred rows is a lot of values to read for one email
//! address, and the value is often all the user knows about the row they
//! want.
//!
//! The rule is [`fuzzy::Needle`]'s, the SQL editor's find as well: a plain
//! substring, case ignored until the needle is written in both cases.
//!
//! **What is searched is the value, not what the cell paints.** A cell cuts
//! its value to one line and to what its lane can show, so a hit past the
//! cut would be a hit the user cannot see in the grid — but it is still the
//! cell they are looking for, and ⏎ on it opens the value whole. A NULL
//! reads `NULL` on screen and is found by that word.
//!
//! The answer is cells in reading order — row by row, left to right — which
//! is the order [`Cell`] sorts in, so the walk between hits can binary
//! search it.
//!
//! **The work is bounded two ways, and neither is a promise.** The count
//! stops at a limit, so a one-letter needle does not collect a quarter of a
//! gigabyte of cells. And a needle that only grew is [`refine_cells`] over
//! the hits it already has, because every hit of `abc` is a hit of `ab` —
//! so typing a word costs one full read, on its first letter, not one per
//! letter. A needle that finds nothing still reads every value once, on
//! the GPUI thread; a result that large is the rare case, and the cap on a
//! result's bytes is what bounds it.
//!
//! The limit counts from the first row, so past it the tail of the result
//! is not walked to: a search that stopped at `10,000+` is one to narrow,
//! not one to step through.

use crate::{Cell, GridData};
use db_client::Value;

/// The cells of `data` whose value holds `needle`, in reading order, and
/// whether the search stopped at `limit` rather than at the end.
///
/// The limit is what keeps a keystroke cheap. `MAX_BYTES` lets a quarter of
/// a gigabyte into one result, and a one-letter needle over that finds
/// something in nearly every cell: past a few thousand hits the count is
/// all the user reads, and counting further buys nothing.
pub fn find_cells(data: &GridData, needle: &fuzzy::Needle, limit: usize) -> (Vec<Cell>, bool) {
    let mut found = Vec::new();
    if needle.is_empty() {
        return (found, false);
    }
    let columns = data.columns.len();
    for (row, values) in data.rows.iter().enumerate() {
        // A ragged row is painted only as far as the header reaches, so it
        // is searched only as far as well: a hit in a lane that is not
        // there could not be walked to.
        for (column, value) in values.iter().take(columns).enumerate() {
            if !holds(value, needle) {
                continue;
            }
            if found.len() == limit {
                return (found, true);
            }
            found.push(Cell::new(row, column));
        }
    }
    (found, false)
}

/// The cells of `hits` that still hold `needle`, for a needle that grew
/// from the one `hits` was found with. Only an **uncapped** answer may be
/// refined: a capped one left cells out, and the longer needle may be in
/// exactly those.
///
/// Case cannot break the rule that the new hits are a subset of the old:
/// a needle only turns strict by growing both cases, and a strict hit of
/// `abC` is still a hit of `ab` read either way.
pub fn refine_cells(data: &GridData, hits: &[Cell], needle: &fuzzy::Needle) -> Vec<Cell> {
    hits.iter()
        .copied()
        .filter(|cell| {
            data.rows
                .get(cell.row)
                .and_then(|row| row.get(cell.column))
                .is_some_and(|value| holds(value, needle))
        })
        .collect()
}

/// Whether one value holds the needle. Text is read where it lies rather
/// than copied: it is nearly every value in a result, and a copy of each
/// per keystroke is the cost the limit is there to bound.
fn holds(value: &Value, needle: &fuzzy::Needle) -> bool {
    match value {
        Value::Text(text) => needle.is_in(text),
        other => needle.is_in(&other.display()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn data() -> GridData {
        let text = |value: &str| Value::Text(value.to_string());
        GridData::new(
            vec!["id".into(), "email".into(), "note".into()],
            vec![
                vec![Value::Int(1), text("ada@example.com"), Value::Null],
                vec![
                    Value::Int(2),
                    text("grace@example.org"),
                    text("Ada's friend"),
                ],
                vec![Value::Int(12), text("linus@example.com"), text("")],
            ],
        )
    }

    fn found(needle: &str) -> Vec<Cell> {
        find_cells(&data(), &fuzzy::Needle::new(needle), 100).0
    }

    #[test]
    fn hits_come_back_in_reading_order() {
        assert_eq!(
            found("ada"),
            vec![Cell::new(0, 1), Cell::new(1, 2)],
            "row by row, and left to right within a row"
        );
    }

    /// A number is found by its digits, as it is painted.
    #[test]
    fn a_value_that_is_not_text_is_found_by_what_it_reads() {
        assert_eq!(found("2"), vec![Cell::new(1, 0), Cell::new(2, 0)]);
        assert_eq!(found("null"), vec![Cell::new(0, 2)]);
    }

    #[test]
    fn nothing_typed_finds_nothing() {
        assert!(found("").is_empty());
    }

    #[test]
    fn the_search_stops_at_its_limit_and_says_so() {
        let (hits, capped) = find_cells(&data(), &fuzzy::Needle::new("example"), 2);
        assert_eq!(hits, vec![Cell::new(0, 1), Cell::new(1, 1)]);
        assert!(capped);

        let (hits, capped) = find_cells(&data(), &fuzzy::Needle::new("example"), 3);
        assert_eq!(hits.len(), 3);
        assert!(
            !capped,
            "a search that ends exactly on the limit found everything"
        );
    }

    /// A grown needle reads only the cells the shorter one found, and
    /// answers what a full search would.
    #[test]
    fn a_grown_needle_is_refined_from_the_hits_it_had() {
        let data = data();
        let (hits, _) = find_cells(&data, &fuzzy::Needle::new("ex"), 100);
        let longer = fuzzy::Needle::new("example.com");
        assert_eq!(
            refine_cells(&data, &hits, &longer),
            find_cells(&data, &longer, 100).0
        );
        let strict = fuzzy::Needle::new("Ada's");
        assert_eq!(
            refine_cells(
                &data,
                &find_cells(&data, &fuzzy::Needle::new("ada"), 100).0,
                &strict
            ),
            vec![Cell::new(1, 2)]
        );
    }

    #[test]
    fn a_ragged_row_is_searched_only_as_far_as_the_header() {
        let data = GridData::new(
            vec!["a".into()],
            vec![vec![Value::Text("x".into()), Value::Text("needle".into())]],
        );
        let (hits, _) = find_cells(&data, &fuzzy::Needle::new("needle"), 10);
        assert!(hits.is_empty());
    }
}
