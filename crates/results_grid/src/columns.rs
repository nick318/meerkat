//! Finding a column of a result by name.
//!
//! A wide result is one the pane cannot show whole: a hundred columns is a
//! hundred lanes to scroll past, and the name is usually the only thing the
//! user knows about the one they want. So the search is over the column
//! *names* the result came back with, and the answer is where each one sits
//! — an index into the same lanes [`crate::Selection`] and the grid count
//! in, so the caller can put the cursor there and scroll to it.
//!
//! The rule is the ⌘K palette's: a case-insensitive **substring**, not a
//! fuzzy score. A column name is typed, not guessed. Unlike the palette
//! this lowercases the whole of Unicode rather than ASCII alone, because
//! nothing here underlines the hit — no byte range has to stay valid in the
//! original name.
//!
//! A column's **type** is not part of it. A result carries its column names
//! and its values; the driver reports no type per column, so a search over
//! type names would only work on the tabs that came from the catalog, and a
//! search that answers differently depending on where the tab came from is
//! worse than one that does not offer it.

/// The columns whose names hold `needle`, in the order they sit in the
/// result.
///
/// An empty needle matches every column, so a search line that has just
/// opened lists the result from its first lane rather than showing nothing
/// until a character is typed.
pub fn find_columns(columns: &[String], needle: &str) -> Vec<usize> {
    let needle = needle.trim().to_lowercase();
    columns
        .iter()
        .enumerate()
        .filter(|(_, name)| needle.is_empty() || name.to_lowercase().contains(&needle))
        .map(|(index, _)| index)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn columns() -> Vec<String> {
        ["id", "email", "created_at", "LAST_SEEN", "deleted_at"]
            .iter()
            .map(|name| name.to_string())
            .collect()
    }

    #[test]
    fn a_hit_anywhere_in_the_name_counts() {
        assert_eq!(find_columns(&columns(), "at"), vec![2, 4]);
        assert_eq!(find_columns(&columns(), "mail"), vec![1]);
    }

    /// Case is not part of what the user is asking: a column the database
    /// spells in capitals is found by the name typed in lower case.
    #[test]
    fn case_is_ignored_both_ways() {
        assert_eq!(find_columns(&columns(), "last"), vec![3]);
        assert_eq!(find_columns(&columns(), "EMAIL"), vec![1]);
    }

    /// An open search line lists the result rather than nothing, and the
    /// space a user leaves while typing is not part of the name.
    #[test]
    fn an_empty_line_matches_every_column() {
        assert_eq!(find_columns(&columns(), ""), vec![0, 1, 2, 3, 4]);
        assert_eq!(find_columns(&columns(), "  "), vec![0, 1, 2, 3, 4]);
        assert_eq!(find_columns(&columns(), " email "), vec![1]);
    }

    #[test]
    fn nothing_matches_nothing() {
        assert!(find_columns(&columns(), "sample").is_empty());
        assert!(find_columns(&[], "id").is_empty());
    }

    /// The indices are the result's own lane numbers, in result order —
    /// never the order the matches were found in.
    #[test]
    fn matches_come_back_in_result_order() {
        let columns = vec!["zed".to_string(), "azure".to_string(), "z".to_string()];
        assert_eq!(find_columns(&columns, "z"), vec![0, 1, 2]);
    }
}
