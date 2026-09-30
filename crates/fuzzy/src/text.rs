//! Finding a piece of text inside a longer one: the ⌘F line over the SQL
//! editor's buffer and over the values of a result.
//!
//! **This is not the name matcher, on purpose.** [`crate::Pattern`] answers
//! "which name did the user mean", and reads a query as the starts of a
//! name's words. A find line answers "where is this text", over prose, SQL
//! and values the user did not name — the word-start rule over a statement
//! full of word starts would light up half the buffer. So a needle here is
//! a plain substring, the way every editor's find bar reads one, and the
//! palette's statement search draws the line in the same place.
//!
//! **Case follows the crate's own rule**, so one habit serves every search
//! line in the app: case is ignored until the needle is written in both
//! cases. `select` finds `SELECT`, `EMAIL` finds `email`, and `userId` asks
//! for exactly that spelling.
//!
//! Every range handed back is a byte range on character boundaries of the
//! haystack, because the walk is over `char_indices`: the editor paints the
//! ranges and selects them, and a range inside a character would panic on
//! the slice.

use std::ops::Range;

/// A find line's text, read once so a long haystack is not charged for
/// reading it again per position.
#[derive(Debug, Clone)]
pub struct Needle {
    chars: Vec<char>,
    /// Whether the case of the needle is a demand. See the module docs.
    strict_case: bool,
}

impl Needle {
    pub fn new(text: &str) -> Self {
        let strict_case =
            text.chars().any(char::is_uppercase) && text.chars().any(char::is_lowercase);
        let chars = if strict_case {
            text.chars().collect()
        } else {
            text.chars().map(fold).collect()
        };
        Self { chars, strict_case }
    }

    /// An empty needle finds nothing. A find line that has just opened has
    /// not asked a question yet, and lighting every position of the buffer
    /// would answer one nobody asked.
    pub fn is_empty(&self) -> bool {
        self.chars.is_empty()
    }

    /// Every place the needle sits in `haystack`, left to right and never
    /// overlapping: `aa` in `aaaa` is two hits, not three, so stepping from
    /// one hit to the next always moves past the text it selected.
    pub fn find_all(&self, haystack: &str) -> Vec<Range<usize>> {
        self.find_capped(haystack, usize::MAX).0
    }

    /// The first `limit` hits, and whether there were more. A one-letter
    /// needle over a long buffer hits nearly everywhere, and past a few
    /// thousand hits the count is all anybody reads.
    pub fn find_capped(&self, haystack: &str, limit: usize) -> (Vec<Range<usize>>, bool) {
        let mut found = Vec::new();
        if self.is_empty() {
            return (found, false);
        }
        let mut from = 0;
        while let Some(hit) = self.find_from(haystack, from) {
            if found.len() == limit {
                return (found, true);
            }
            from = hit.end;
            found.push(hit);
        }
        (found, false)
    }

    /// Whether the needle is anywhere in `haystack`.
    pub fn is_in(&self, haystack: &str) -> bool {
        !self.is_empty() && self.find_from(haystack, 0).is_some()
    }

    /// The first hit that starts at or after byte `from`.
    fn find_from(&self, haystack: &str, from: usize) -> Option<Range<usize>> {
        let rest = haystack.get(from..)?;
        for (start, _) in rest.char_indices() {
            let mut end = start;
            let mut wanted = self.chars.iter();
            let mut tail = rest[start..].chars();
            let landed = loop {
                let Some(want) = wanted.next() else {
                    break true;
                };
                let Some(ch) = tail.next() else { break false };
                if !self.same(*want, ch) {
                    break false;
                }
                end += ch.len_utf8();
            };
            if landed {
                return Some(from + start..from + end);
            }
        }
        None
    }

    fn same(&self, want: char, ch: char) -> bool {
        if self.strict_case {
            want == ch
        } else {
            want == fold(ch)
        }
    }
}

/// One character folded to one character, so a fold never changes how many
/// characters a hit covers and its range in the original stays right. The
/// name matcher folds the same way.
fn fold(ch: char) -> char {
    ch.to_lowercase().next().unwrap_or(ch)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn found(haystack: &str, needle: &str) -> Vec<Range<usize>> {
        Needle::new(needle).find_all(haystack)
    }

    #[test]
    fn every_place_the_text_sits_is_found() {
        assert_eq!(
            found("select id from users where id = 1", "id"),
            vec![7..9, 27..29]
        );
    }

    /// A substring, not a word start: the find line is over text, and the
    /// user looking for `ser` in `users` means it.
    #[test]
    fn a_hit_may_sit_inside_a_word() {
        assert_eq!(found("users", "ser"), vec![1..4]);
    }

    #[test]
    fn hits_never_overlap() {
        assert_eq!(found("aaaa", "aa"), vec![0..2, 2..4]);
    }

    #[test]
    fn case_is_ignored_until_the_needle_means_it() {
        assert_eq!(found("SELECT 1", "select"), vec![0..6]);
        assert_eq!(found("select 1", "SELECT"), vec![0..6]);
        assert_eq!(found("userId userid", "userId"), vec![0..6]);
    }

    #[test]
    fn nothing_typed_finds_nothing() {
        assert!(found("select", "").is_empty());
        assert!(!Needle::new("").is_in("select"));
    }

    /// The ranges slice the haystack, so they must fall on character
    /// boundaries however wide the characters before them are.
    #[test]
    fn a_range_is_measured_in_the_haystack_s_own_bytes() {
        let haystack = "Łódź — łódź";
        let hits = found(haystack, "łódź");
        assert_eq!(hits.len(), 2);
        for hit in hits {
            assert_eq!(haystack[hit].to_lowercase(), "łódź");
        }
    }

    #[test]
    fn a_capped_search_says_there_was_more() {
        let needle = Needle::new("a");
        assert_eq!(needle.find_capped("aaa", 2), (vec![0..1, 1..2], true));
        assert_eq!(needle.find_capped("aa", 2), (vec![0..1, 1..2], false));
    }

    #[test]
    fn a_needle_longer_than_what_is_left_is_not_found() {
        assert!(found("sel", "select").is_empty());
        assert!(Needle::new("lec").is_in("select"));
    }
}
