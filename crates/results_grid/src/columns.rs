//! Finding a column of a result by name.
//!
//! A wide result is one the pane cannot show whole: a hundred columns is a
//! hundred lanes to scroll past, and the name is usually the only thing the
//! user knows about the one they want. So the search is over the column
//! *names* the result came back with, and the answer is where each one sits
//! — an index into the same lanes [`crate::Selection`] and the grid count
//! in, so the caller can put the cursor there and scroll to it.
//!
//! The rule is [`fuzzy`]'s, which is the ⌘K palette's and the SQL editor's
//! as well: a query names the **starts of a name's words**, so `mast_cl`
//! finds `master_client_reference` — the case this replaced a plain
//! substring for. A substring could only be typed by somebody who already
//! knew where in the name to start reading, which is not what a search line
//! over a hundred columns is for.
//!
//! **The answer is ranked, not in result order.** A search that offers the
//! right column fourth is one the user reads before they can use it, and
//! the match itself says which one is closest to what was typed; lane 34
//! coming before lane 4 is what a ranked answer looks like. Ties go to the
//! shorter name and then to the earlier lane, so the order is settled by
//! the result rather than by how the matching happened to run.
//!
//! A column's **type** is not part of it. A result carries its column names
//! and its values; the driver reports no type per column, so a search over
//! type names would only work on the tabs that came from the catalog, and a
//! search that answers differently depending on where the tab came from is
//! worse than one that does not offer it.

/// The columns whose names answer `needle`, best first.
///
/// An empty needle matches every column, in the result's own order, so a
/// search line that has just opened lists the result from its first lane
/// rather than showing nothing until a character is typed.
pub fn find_columns(columns: &[String], needle: &str) -> Vec<usize> {
    let pattern = fuzzy::Pattern::new(needle);
    // Nothing typed, nothing to rank: the result's own order is the only
    // order there is, and sorting an unranked list by name length would
    // shuffle the lanes for no reason.
    if pattern.is_empty() {
        return (0..columns.len()).collect();
    }
    let mut found: Vec<(usize, i32, usize)> = columns
        .iter()
        .enumerate()
        .filter_map(|(index, name)| {
            let hit = pattern.score(name)?;
            Some((index, hit.score, name.chars().count()))
        })
        .collect();
    // Best score first; then the shorter name, which is more of what was
    // typed; then the lane the result puts first.
    found.sort_by_key(|&(index, score, length)| (-score, length, index));
    found.into_iter().map(|(index, _, _)| index).collect()
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

    /// The case this crate's matcher exists for: two words of a name, each
    /// named by its start.
    #[test]
    fn a_query_may_name_the_words_of_a_name() {
        let columns: Vec<String> = ["master_state_type_code", "master_client_reference"]
            .iter()
            .map(|name| name.to_string())
            .collect();
        assert_eq!(find_columns(&columns, "mast_cl"), vec![1]);
        assert_eq!(find_columns(&columns, "mcr"), vec![1]);
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

    /// The closest match comes first, whatever lane it sits in — a search
    /// that answers with the right column fourth is one the user has to
    /// read before they can use it.
    #[test]
    fn matches_come_back_ranked() {
        let columns: Vec<String> = ["invoice_master_name", "master_state_type_code"]
            .iter()
            .map(|name| name.to_string())
            .collect();
        assert_eq!(find_columns(&columns, "mast"), vec![1, 0]);
    }

    /// Equal matches keep the result's own order, so the list never depends
    /// on how the matching happened to run.
    #[test]
    fn an_equal_match_keeps_the_lane_order() {
        let columns = vec!["zed".to_string(), "zip".to_string()];
        assert_eq!(find_columns(&columns, "z"), vec![0, 1]);
    }
}
