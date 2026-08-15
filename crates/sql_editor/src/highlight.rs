//! A one-pass SQL tokenizer, just enough to colour the editor the way the
//! design comp does: keywords in the deep accent, literals in green,
//! comments muted, everything else in the body colour.
//!
//! It works a line at a time, so a string literal or a block comment that
//! spans lines loses its colour after the first newline. That is the price
//! of not carrying a parser; nothing else depends on this being exact.

use std::ops::Range;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Token {
    Keyword,
    Literal,
    Comment,
    Plain,
}

/// Split one line into coloured spans. The spans are in order, they never
/// overlap, and their lengths add up to the length of the line, so they
/// can be handed straight to the text system as runs.
pub fn spans(line: &str) -> Vec<(Range<usize>, Token)> {
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
                if is_keyword(&line[start..ix]) { Token::Keyword } else { Token::Plain }
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
        spans(line)
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
            let total: usize = spans(line).iter().map(|(range, _)| range.len()).sum();
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
    fn an_unterminated_quote_stops_at_the_line_end() {
        assert_eq!(kinds("'oops"), vec![("'oops", Token::Literal)]);
    }
}
