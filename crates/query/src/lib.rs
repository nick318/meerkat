//! Query execution helpers shared by the app and the drivers.
//!
//! Splitting a buffer into statements: a query buffer is a scratchpad of
//! several, and a driver runs one command at a time, so the app sends
//! them in turn. A semicolon separates statements only when it is plain
//! text — inside a string, a quoted identifier, a comment or a
//! dollar-quoted body it is just a character.

use std::ops::Range;

/// Every non-empty statement in `sql`, in order, ready to send.
pub fn statements(sql: &str) -> Vec<String> {
    ranges(sql).into_iter().map(|range| sql[range].to_string()).collect()
}

/// Byte ranges of the statements in `text`, whitespace trimmed and the
/// separating semicolons left out.
fn ranges(text: &str) -> Vec<Range<usize>> {
    let bytes = text.as_bytes();
    let mut statements: Vec<Range<usize>> = Vec::new();
    let mut start = 0;
    let mut ix = 0;

    while ix < bytes.len() {
        match bytes[ix] {
            b'\'' => ix = end_of_quoted(bytes, ix, b'\''),
            b'"' => ix = end_of_quoted(bytes, ix, b'"'),
            b'-' if bytes.get(ix + 1) == Some(&b'-') => {
                ix = memchr(bytes, b'\n', ix).map(|ix| ix + 1).unwrap_or(bytes.len());
            }
            b'/' if bytes.get(ix + 1) == Some(&b'*') => ix = end_of_block_comment(bytes, ix),
            b'$' => match end_of_dollar_quoted(bytes, ix) {
                Some(end) => ix = end,
                None => ix += 1,
            },
            b';' => {
                push(&mut statements, text, start..ix);
                ix += 1;
                start = ix;
            }
            _ => ix += 1,
        }
    }
    push(&mut statements, text, start..bytes.len());
    statements
}

fn push(statements: &mut Vec<Range<usize>>, text: &str, range: Range<usize>) {
    let trimmed = trim(text, range);
    if !trimmed.is_empty() {
        statements.push(trimmed);
    }
}

fn trim(text: &str, range: Range<usize>) -> Range<usize> {
    let slice = &text[range.clone()];
    let start = range.start + (slice.len() - slice.trim_start().len());
    let end = range.end - (slice.len() - slice.trim_end().len());
    start..end.max(start)
}

fn memchr(bytes: &[u8], needle: u8, from: usize) -> Option<usize> {
    bytes[from..].iter().position(|byte| *byte == needle).map(|ix| from + ix)
}

/// Index just past the closing quote, treating a doubled quote as an
/// escape. An unterminated quote runs to the end of the buffer.
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

fn end_of_block_comment(bytes: &[u8], start: usize) -> usize {
    let mut ix = start + 2;
    // Postgres nests block comments, unlike C.
    let mut depth = 1;
    while ix + 1 < bytes.len() {
        if bytes[ix] == b'/' && bytes[ix + 1] == b'*' {
            depth += 1;
            ix += 2;
        } else if bytes[ix] == b'*' && bytes[ix + 1] == b'/' {
            depth -= 1;
            ix += 2;
            if depth == 0 {
                return ix;
            }
        } else {
            ix += 1;
        }
    }
    bytes.len()
}

/// Index just past a `$tag$ ... $tag$` body, or `None` when the `$` does
/// not open one.
fn end_of_dollar_quoted(bytes: &[u8], start: usize) -> Option<usize> {
    let mut ix = start + 1;
    while ix < bytes.len() && (bytes[ix].is_ascii_alphanumeric() || bytes[ix] == b'_') {
        ix += 1;
    }
    if bytes.get(ix) != Some(&b'$') {
        return None;
    }
    let tag = &bytes[start..=ix];
    let mut ix = ix + 1;
    while ix + tag.len() <= bytes.len() {
        if &bytes[ix..ix + tag.len()] == tag {
            return Some(ix + tag.len());
        }
        ix += 1;
    }
    Some(bytes.len())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn texts(text: &str) -> Vec<String> {
        statements(text)
    }

    #[test]
    fn a_semicolon_separates_statements() {
        let text = "select 1;\n\nselect 2;";
        assert_eq!(texts(text), vec!["select 1", "select 2"]);
    }

    #[test]
    fn a_buffer_without_semicolons_is_one_statement() {
        assert_eq!(texts("select 1"), vec!["select 1"]);
        assert!(texts("   \n\n  ").is_empty());
    }

    #[test]
    fn a_semicolon_inside_a_string_does_not_split() {
        let text = "select ';' as a; select 2";
        assert_eq!(texts(text), vec!["select ';' as a", "select 2"]);
        assert_eq!(texts("select 'it''s; fine'"), vec!["select 'it''s; fine'"]);
    }

    #[test]
    fn a_semicolon_inside_a_quoted_identifier_does_not_split() {
        let text = "select * from \"we;ird\"; select 2";
        assert_eq!(texts(text), vec!["select * from \"we;ird\"", "select 2"]);
    }

    #[test]
    fn a_semicolon_inside_a_comment_does_not_split() {
        let text = "select 1 -- ; not a split\n; select 2";
        assert_eq!(texts(text), vec!["select 1 -- ; not a split", "select 2"]);

        let text = "select /* ; nested /* ; */ still */ 1; select 2";
        assert_eq!(texts(text), vec!["select /* ; nested /* ; */ still */ 1", "select 2"]);
    }

    #[test]
    fn a_semicolon_inside_a_dollar_quoted_body_does_not_split() {
        let text = "create function f() returns int as $$ begin; return 1; end $$; select 2";
        assert_eq!(
            texts(text),
            vec![
                "create function f() returns int as $$ begin; return 1; end $$",
                "select 2",
            ]
        );
        // A lone `$` is not a quote opener.
        assert_eq!(texts("select 1 $ 2; select 3"), vec!["select 1 $ 2", "select 3"]);
    }

    #[test]
    fn an_unterminated_quote_swallows_the_rest() {
        // Better to send one broken statement and let the database report
        // it than to split in the middle of a string.
        assert_eq!(texts("select 'oops; select 2"), vec!["select 'oops; select 2"]);
    }
}
