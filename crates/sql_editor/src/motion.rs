//! Cursor motion over a text buffer: character, word and line boundaries.
//!
//! All offsets are byte offsets into the buffer and always land on a
//! character boundary. A newline stops word motion, the way Zed's editor
//! does with `ignore_newlines: false` — in a query buffer, crossing a line
//! break by accident is worse than one extra keypress.

/// What counts as part of a word. `_` and `$` belong to SQL identifiers;
/// `.` does not, so `public.users` is two words.
pub fn is_word_char(ch: char) -> bool {
    ch.is_alphanumeric() || ch == '_' || ch == '$'
}

pub fn previous_boundary(text: &str, offset: usize) -> usize {
    text[..offset]
        .char_indices()
        .next_back()
        .map(|(ix, _)| ix)
        .unwrap_or(0)
}

pub fn next_boundary(text: &str, offset: usize) -> usize {
    text[offset..]
        .char_indices()
        .nth(1)
        .map(|(ix, _)| offset + ix)
        .unwrap_or(text.len())
}

fn char_before(text: &str, offset: usize) -> Option<char> {
    text[..offset].chars().next_back()
}

fn char_at(text: &str, offset: usize) -> Option<char> {
    text[offset..].chars().next()
}

/// Start of the word before `offset`, as ⌥← gives on macOS.
pub fn previous_word_start(text: &str, offset: usize) -> usize {
    let mut ix = offset;
    if ix == 0 {
        return 0;
    }
    // Sitting right after a line break: step over it and stop, so ⌥←
    // lands at the end of the line above rather than skipping a word.
    if char_before(text, ix) == Some('\n') {
        return previous_boundary(text, ix);
    }
    while let Some(ch) = char_before(text, ix) {
        if ch == '\n' || is_word_char(ch) {
            break;
        }
        ix = previous_boundary(text, ix);
    }
    while let Some(ch) = char_before(text, ix) {
        if !is_word_char(ch) {
            break;
        }
        ix = previous_boundary(text, ix);
    }
    ix
}

/// End of the word after `offset`, as ⌥→ gives on macOS.
pub fn next_word_end(text: &str, offset: usize) -> usize {
    let mut ix = offset;
    if ix >= text.len() {
        return text.len();
    }
    if char_at(text, ix) == Some('\n') {
        return next_boundary(text, ix);
    }
    while let Some(ch) = char_at(text, ix) {
        if ch == '\n' || is_word_char(ch) {
            break;
        }
        ix = next_boundary(text, ix);
    }
    while let Some(ch) = char_at(text, ix) {
        if !is_word_char(ch) {
            break;
        }
        ix = next_boundary(text, ix);
    }
    ix
}

/// The word around `offset`, for a double click. Falls back to the single
/// character under the cursor when that character is not word-like.
pub fn word_at(text: &str, offset: usize) -> std::ops::Range<usize> {
    let inside = char_at(text, offset).is_some_and(is_word_char)
        || char_before(text, offset).is_some_and(is_word_char);
    if !inside {
        return offset..next_boundary(text, offset).min(text.len());
    }
    let mut start = offset;
    while let Some(ch) = char_before(text, start) {
        if !is_word_char(ch) {
            break;
        }
        start = previous_boundary(text, start);
    }
    let mut end = offset;
    while let Some(ch) = char_at(text, end) {
        if !is_word_char(ch) {
            break;
        }
        end = next_boundary(text, end);
    }
    start..end
}

pub fn line_start(text: &str, offset: usize) -> usize {
    text[..offset].rfind('\n').map(|ix| ix + 1).unwrap_or(0)
}

pub fn line_end(text: &str, offset: usize) -> usize {
    text[offset..]
        .find('\n')
        .map(|ix| offset + ix)
        .unwrap_or(text.len())
}

#[cfg(test)]
mod tests {
    use super::*;

    const SQL: &str = "select id, name\nfrom public.users";

    #[test]
    fn word_motion_walks_word_by_word() {
        // From the very end, back over: users . public from
        let mut ix = SQL.len();
        let mut stops = Vec::new();
        for _ in 0..6 {
            ix = previous_word_start(SQL, ix);
            stops.push(&SQL[ix..(ix + 6).min(SQL.len())]);
        }
        assert_eq!(stops[0], "users");
        assert_eq!(stops[1], "public");
        assert_eq!(stops[2], "from p");
    }

    #[test]
    fn word_motion_stops_at_a_line_break() {
        let end_of_first_line = SQL.find('\n').unwrap();
        // Forward from the end of "name" lands on the break, not past it.
        assert_eq!(next_word_end(SQL, end_of_first_line), end_of_first_line + 1);
        // Backward from the start of "from" lands on the break.
        assert_eq!(
            previous_word_start(SQL, end_of_first_line + 1),
            end_of_first_line
        );
    }

    #[test]
    fn forward_word_motion_ends_on_the_word() {
        assert_eq!(&SQL[..next_word_end(SQL, 0)], "select");
        let after_select = next_word_end(SQL, 0);
        assert_eq!(&SQL[..next_word_end(SQL, after_select)], "select id");
    }

    #[test]
    fn a_double_click_takes_the_whole_word() {
        // Middle of "public".
        let ix = SQL.find("public").unwrap() + 2;
        assert_eq!(&SQL[word_at(SQL, ix)], "public");
        // Between a word and the dot, the word to the left wins, the way
        // a caret at that offset belongs to the word behind it.
        let dot = SQL.find('.').unwrap();
        assert_eq!(&SQL[word_at(SQL, dot)], "public");
        // Away from any word, take just the character clicked.
        let space = SQL.find(' ').unwrap() + 3;
        assert_eq!(&SQL[word_at(SQL, space)], "id");
        assert_eq!(&"a  b"[word_at("a  b", 2)], " ");
    }

    #[test]
    fn motion_never_splits_a_character() {
        let text = "select 'héllo wörld' as x";
        let mut ix = 0;
        while ix < text.len() {
            ix = next_word_end(text, ix);
            assert!(text.is_char_boundary(ix), "split at {ix}");
        }
        while ix > 0 {
            ix = previous_word_start(text, ix);
            assert!(text.is_char_boundary(ix), "split at {ix}");
        }
    }

    #[test]
    fn lines_bound_at_the_break() {
        let ix = SQL.find("public").unwrap();
        assert_eq!(
            &SQL[line_start(SQL, ix)..line_end(SQL, ix)],
            "from public.users"
        );
    }
}
