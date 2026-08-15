//! A one-pass SQL tokenizer, just enough to colour the editor the way the
//! design comp does: keywords in the deep accent, literals in green,
//! comments muted, everything else in the body colour.
//!
//! It also colours the names of the connected database. A word that the
//! catalog knows — a schema, a table, a view or a column — gets the
//! identifier colour; a word it does not know stays plain, so a typo is
//! visible before the query runs.
//!
//! It works a line at a time, so a string literal that spans lines loses
//! its colour after the first newline. That is the price of not carrying
//! a parser; nothing else depends on this being exact.

use std::collections::HashSet;
use std::ops::Range;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Token {
    Keyword,
    Literal,
    Comment,
    /// A name the connected database actually has.
    Identifier,
    Plain,
}

/// The names in the connected database, folded to lower case because
/// Postgres folds unquoted identifiers the same way.
#[derive(Debug, Default)]
pub struct Vocabulary {
    names: HashSet<String>,
}

impl Vocabulary {
    pub fn new(names: impl IntoIterator<Item = String>) -> Self {
        Self {
            names: names.into_iter().map(|name| name.to_ascii_lowercase()).collect(),
        }
    }

    pub fn contains(&self, word: &str) -> bool {
        // A one-character name would light up half the buffer for no
        // information; `id` and longer is where this starts to help.
        word.len() > 1 && self.names.contains(&word.to_ascii_lowercase())
    }

    pub fn is_empty(&self) -> bool {
        self.names.is_empty()
    }
}

/// Split one line into coloured spans. The spans are in order, they never
/// overlap, and their lengths add up to the length of the line, so they
/// can be handed straight to the text system as runs.
pub fn spans(line: &str, vocabulary: &Vocabulary) -> Vec<(Range<usize>, Token)> {
    let bytes = line.as_bytes();
    let mut spans: Vec<(Range<usize>, Token)> = Vec::new();
    let mut ix = 0;

    while ix < bytes.len() {
        let start = ix;
        let token = match bytes[ix] {
            b'-' if bytes.get(ix + 1) == Some(&b'-') => {
                ix = bytes.len();
                Token::Comment
            }
            b'\'' => {
                ix = end_of_quoted(bytes, ix, b'\'');
                Token::Literal
            }
            // A quoted identifier is a name, not a literal.
            b'"' => {
                ix = end_of_quoted(bytes, ix, b'"');
                Token::Plain
            }
            b'0'..=b'9' => {
                while ix < bytes.len() && (bytes[ix].is_ascii_digit() || bytes[ix] == b'.') {
                    ix += 1;
                }
                Token::Literal
            }
            c if is_word_byte(c) => {
                while ix < bytes.len() && is_word_byte(bytes[ix]) {
                    ix += 1;
                }
                let word = &line[start..ix];
                // Keywords win: a column called `order` still reads as a
                // keyword, which is what the SQL parser will do with it.
                if is_keyword(word) {
                    Token::Keyword
                } else if vocabulary.contains(word) {
                    Token::Identifier
                } else {
                    Token::Plain
                }
            }
            _ => {
                // Punctuation, spaces and any multi-byte character: step to
                // the next character boundary so slicing stays valid.
                ix += 1;
                while ix < bytes.len() && !line.is_char_boundary(ix) {
                    ix += 1;
                }
                Token::Plain
            }
        };

        match spans.last_mut() {
            Some((range, last)) if *last == token => range.end = ix,
            _ => spans.push((start..ix, token)),
        }
    }
    spans
}

/// Index just past the closing quote, treating a doubled quote as an
/// escape. An unterminated quote runs to the end of the line.
fn end_of_quoted(bytes: &[u8], start: usize, quote: u8) -> usize {
    let mut ix = start + 1;
    while ix < bytes.len() {
        if bytes[ix] == quote {
            if bytes.get(ix + 1) == Some(&quote) {
                ix += 2;
                continue;
            }
            return ix + 1;
        }
        ix += 1;
    }
    bytes.len()
}

fn is_word_byte(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || byte == b'_' || byte == b'$' || byte >= 0x80
}

fn is_keyword(word: &str) -> bool {
    KEYWORDS.binary_search(&word.to_ascii_lowercase().as_str()).is_ok()
}

/// Sorted, so the lookup can bisect.
const KEYWORDS: &[&str] = &[
    "all", "alter", "and", "any", "array", "as", "asc", "begin", "between", "by", "case", "cast",
    "coalesce", "commit", "count", "create", "cross", "current_date", "current_timestamp",
    "delete", "desc", "distinct", "drop", "else", "end", "except", "exists", "explain", "false",
    "filter", "first", "from", "full", "group", "having", "ilike", "in", "index", "inner",
    "insert", "intersect", "into", "is", "join", "lateral", "left", "like", "limit", "max", "min",
    "not", "null", "nulls", "offset", "on", "or", "order", "outer", "over", "partition",
    "returning", "right", "rollback", "select", "set", "some", "sum", "table", "then", "true",
    "union", "update", "using", "values", "view", "when", "where", "window", "with",
];

#[cfg(test)]
mod tests {
    use super::*;

    fn kinds(line: &str) -> Vec<(&str, Token)> {
        spans(line, &Vocabulary::default())
            .into_iter()
            .map(|(range, token)| (&line[range], token))
            .collect()
    }

    #[test]
    fn the_keyword_list_is_sorted_for_bisection() {
        let mut sorted = KEYWORDS.to_vec();
        sorted.sort_unstable();
        assert_eq!(KEYWORDS, sorted.as_slice());
    }

    #[test]
    fn spans_cover_the_whole_line() {
        for line in [
            "select u.id, count(o.id) as n",
            "  on o.user_id = u.id and o.created_at > '2026-05-15'",
            "-- a comment",
            "",
            "select 'héllo wörld' as greeting",
        ] {
            let total: usize =
                spans(line, &Vocabulary::default()).iter().map(|(range, _)| range.len()).sum();
            assert_eq!(total, line.len(), "{line}");
        }
    }

    #[test]
    fn keywords_are_case_insensitive() {
        assert_eq!(kinds("SELECT"), vec![("SELECT", Token::Keyword)]);
        assert_eq!(kinds("Select"), vec![("Select", Token::Keyword)]);
        // A column that merely starts like a keyword is not a keyword.
        assert_eq!(kinds("selected"), vec![("selected", Token::Plain)]);
    }

    #[test]
    fn literals_and_comments_take_their_own_colour() {
        assert_eq!(
            kinds("where x = 'a''b' -- why"),
            vec![
                ("where", Token::Keyword),
                (" x = ", Token::Plain),
                ("'a''b'", Token::Literal),
                (" ", Token::Plain),
                ("-- why", Token::Comment),
            ]
        );
    }

    #[test]
    fn a_quoted_identifier_is_not_a_literal() {
        assert_eq!(
            kinds("from \"order\""),
            vec![("from", Token::Keyword), (" \"order\"", Token::Plain)]
        );
    }

    #[test]
    fn catalog_names_are_marked_and_typos_are_not() {
        let vocabulary = Vocabulary::new(["public".into(), "users".into(), "email".into()]);
        let line = "select email from public.userz";
        let marked: Vec<&str> = spans(line, &vocabulary)
            .into_iter()
            .filter(|(_, token)| *token == Token::Identifier)
            .map(|(range, _)| &line[range])
            .collect();
        assert_eq!(marked, vec!["email", "public"]);
    }

    #[test]
    fn catalog_names_are_matched_case_insensitively() {
        let vocabulary = Vocabulary::new(["Users".into()]);
        assert!(vocabulary.contains("USERS"));
        assert!(vocabulary.contains("users"));
        // One character is noise, not information.
        assert!(!Vocabulary::new(["x".into()]).contains("x"));
    }

    #[test]
    fn a_keyword_that_is_also_a_column_stays_a_keyword() {
        let vocabulary = Vocabulary::new(["order".into()]);
        assert_eq!(
            spans("order", &vocabulary),
            vec![(0..5, Token::Keyword)]
        );
    }

    #[test]
    fn an_unterminated_quote_stops_at_the_line_end() {
        assert_eq!(kinds("'oops"), vec![("'oops", Token::Literal)]);
    }
}
