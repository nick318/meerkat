//! Query execution helpers shared by the app and the drivers.
//!
//! Splitting a buffer into statements: a query buffer is a scratchpad of
//! several, and a driver runs one command at a time, so the app sends
//! them in turn. A semicolon separates statements only when it is plain
//! text — inside a string, a quoted identifier, a comment or a
//! dollar-quoted body it is just a character.
//!
//! Reading a statement's transaction verb: the app has to know whether the
//! buffer it is about to send opens or ends a transaction itself, because
//! that decides whether the app adds a `BEGIN` of its own and what the
//! transaction bar says afterwards.
//!
//! Reading a statement's command verb: a statement that changes rows comes
//! back as a count and no result set, and the count alone cannot say
//! whether zero means "your `WHERE` matched nothing" or "there was never
//! anything to count". The verb is what tells those apart, and it is used
//! for the wording only — see [`command_verb`].

use std::ops::Range;

/// Every non-empty statement in `sql`, in order, ready to send.
pub fn statements(sql: &str) -> Vec<String> {
    statement_ranges(sql).into_iter().map(|range| sql[range].to_string()).collect()
}

/// Where each of those statements sits in `sql`, in the same order.
///
/// The editor marks a run statement by statement in its gutter, so it has
/// to know which lines each one covers. The ranges are trimmed and carry
/// no separating semicolon, so they are exactly the text [`statements`]
/// hands the server.
pub fn statement_ranges(sql: &str) -> Vec<Range<usize>> {
    ranges(sql)
}

/// What a statement does to the transaction around it, when that is all it
/// does.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TxVerb {
    /// `BEGIN`, `BEGIN TRANSACTION`, `START TRANSACTION`.
    Begin,
    /// `COMMIT`, `END`.
    Commit,
    /// `ROLLBACK`, `ABORT` — but never `ROLLBACK TO SAVEPOINT`, which
    /// leaves the transaction open and is therefore not an end.
    Rollback,
}

/// The transaction verb `statement` is, or `None` for everything else.
///
/// **What it is for**: in manual mode the app adds a `BEGIN` before a run,
/// and it must not add one in front of a buffer that opens its own; and
/// after a run the transaction bar says "committed" or "rolled back", which
/// it can only know from the buffer when the user typed the word.
///
/// Being wrong is cheap on purpose. The server's own answer —
/// `Session::in_transaction` — is what the app believes about the state
/// afterwards, so a verb misread here costs at most one spare `BEGIN` or a
/// bar that says nothing rather than something wrong. That is why this
/// reads the first word or two and does not attempt to parse SQL.
///
/// Leading comments and whitespace are skipped: `-- go\nCOMMIT` is a
/// commit. A `begin` inside a dollar-quoted function body is not seen at
/// all, because [`statements`] hands such a body over whole.
pub fn transaction_verb(statement: &str) -> Option<TxVerb> {
    let body = skip_leading_comments(statement);
    let mut words = body.split(|c: char| c.is_whitespace() || c == ';').filter(|w| !w.is_empty());
    let first = words.next()?.to_ascii_lowercase();
    let second = words.next().unwrap_or_default().to_ascii_lowercase();
    match first.as_str() {
        "begin" => Some(TxVerb::Begin),
        // `START` is only a transaction verb with `TRANSACTION` after it.
        "start" if second == "transaction" => Some(TxVerb::Begin),
        // `END` closes a transaction; `END` inside a PL/pgSQL body never
        // reaches here, because a dollar-quoted body is one statement.
        "commit" | "end" => Some(TxVerb::Commit),
        // `ROLLBACK TO [SAVEPOINT] name` unwinds *within* the transaction
        // and leaves it open, so it is not an end.
        "rollback" if second == "to" => None,
        "rollback" | "abort" => Some(TxVerb::Rollback),
        _ => None,
    }
}

/// What a statement did to the rows, for a statement that answers with a
/// count instead of a result set.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CommandVerb {
    Insert,
    Update,
    Delete,
    /// `MERGE`, which inserts, updates and deletes in one statement — so
    /// "merged" is the only word that is true of all of it.
    Merge,
}

impl CommandVerb {
    /// The past participle, for `1 row updated`.
    pub fn past(self) -> &'static str {
        match self {
            CommandVerb::Insert => "inserted",
            CommandVerb::Update => "updated",
            CommandVerb::Delete => "deleted",
            CommandVerb::Merge => "merged",
        }
    }
}

/// The row-changing verb `statement` opens with, or `None` for everything
/// else.
///
/// **What it is for is the wording of a count, and nothing else.** The
/// server's tag carries the number of rows but the wire hands the app no
/// verb — sqlx reports `rows_affected` and drops the tag itself — so a
/// count of `0` reads either as "the `WHERE` matched nothing", which is the
/// answer the user is waiting for, or as "a `CREATE INDEX` has nothing to
/// count", which is not news. This says which sentence to write.
///
/// Being wrong is cheap, as it is in [`transaction_verb`], and cheaper: a
/// misread costs a generic `1 row affected` in place of `1 row updated`.
/// Nothing here decides what runs, what is painted, or what the count is.
/// So a data-modifying CTE — `WITH moved AS (DELETE …) INSERT …` — reads as
/// no verb and takes the generic wording, rather than being parsed for a
/// verb buried in it.
pub fn command_verb(statement: &str) -> Option<CommandVerb> {
    let body = skip_leading_comments(statement);
    let first = body
        .split(|c: char| c.is_whitespace() || c == ';' || c == '(')
        .find(|word| !word.is_empty())?
        .to_ascii_lowercase();
    match first.as_str() {
        "insert" => Some(CommandVerb::Insert),
        "update" => Some(CommandVerb::Update),
        "delete" => Some(CommandVerb::Delete),
        "merge" => Some(CommandVerb::Merge),
        _ => None,
    }
}

/// The statement past whatever comments open it. A buffer's statements are
/// trimmed already, but `-- what this does` on the line above a `COMMIT` is
/// part of the statement that follows it.
fn skip_leading_comments(statement: &str) -> &str {
    let mut rest = statement.trim_start();
    loop {
        if let Some(after) = rest.strip_prefix("--") {
            rest = match after.find('\n') {
                Some(end) => after[end + 1..].trim_start(),
                // A line comment with nothing after it is the whole
                // statement, and the whole statement is then no verb.
                None => return "",
            };
            continue;
        }
        if rest.starts_with("/*") {
            let bytes = rest.as_bytes();
            let end = end_of_block_comment(bytes, 0);
            rest = rest[end..].trim_start();
            continue;
        }
        return rest;
    }
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
    fn a_range_holds_the_statement_it_names() {
        // The gutter marks a statement by where it sits in the buffer, so
        // the range and the text have to be the same statement.
        let text = "select 1;\n\nupdate t set a = 1;\n";
        let ranges = statement_ranges(text);
        assert_eq!(ranges, vec![0..8, 11..29]);
        let named: Vec<&str> = ranges.iter().map(|range| &text[range.clone()]).collect();
        assert_eq!(named, statements(text));
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
    fn the_transaction_verbs_are_read_off_the_first_word() {
        let verb = |sql| transaction_verb(sql);
        assert_eq!(verb("BEGIN"), Some(TxVerb::Begin));
        assert_eq!(verb("begin transaction"), Some(TxVerb::Begin));
        assert_eq!(verb("BEGIN ISOLATION LEVEL SERIALIZABLE"), Some(TxVerb::Begin));
        assert_eq!(verb("start transaction"), Some(TxVerb::Begin));
        assert_eq!(verb("COMMIT"), Some(TxVerb::Commit));
        assert_eq!(verb("end"), Some(TxVerb::Commit));
        assert_eq!(verb("rollback"), Some(TxVerb::Rollback));
        assert_eq!(verb("ABORT"), Some(TxVerb::Rollback));
        assert_eq!(verb("select 1"), None);
        assert_eq!(verb(""), None);
    }

    /// The two shapes that read like a verb and are not one. Getting either
    /// wrong would have the app add a `BEGIN` in front of a transaction
    /// that is already open, or drop the bar while one still is.
    #[test]
    fn a_verb_that_only_looks_like_one_is_not_read_as_one() {
        // `ROLLBACK TO SAVEPOINT` unwinds inside the transaction and leaves
        // it open, so it does not end anything.
        assert_eq!(transaction_verb("rollback to savepoint s1"), None);
        assert_eq!(transaction_verb("ROLLBACK TO s1"), None);
        // `START` on its own is not a transaction verb.
        assert_eq!(transaction_verb("start replication"), None);
    }

    /// A comment above the word is part of the statement the splitter hands
    /// over, so the verb has to be found past it.
    #[test]
    fn a_comment_in_front_of_the_verb_is_skipped() {
        assert_eq!(transaction_verb("-- land it\nCOMMIT"), Some(TxVerb::Commit));
        assert_eq!(transaction_verb("/* land it */ commit"), Some(TxVerb::Commit));
        assert_eq!(transaction_verb("-- nothing but a note"), None);
    }

    /// A `begin` inside a function body is not a statement of the buffer's,
    /// so it never reaches the verb reader at all.
    #[test]
    fn a_begin_inside_a_function_body_is_not_a_transaction() {
        let text = "create function f() returns int as $$ begin return 1; end $$";
        let statements = texts(text);
        assert_eq!(statements.len(), 1);
        assert_eq!(transaction_verb(&statements[0]), None);
    }

    /// The verb decides one sentence in the result line. A `SELECT` must
    /// not read as one, or a query returning no rows would be reported as
    /// having matched nothing.
    #[test]
    fn the_command_verbs_are_read_off_the_first_word() {
        assert_eq!(command_verb("update t set a = 1"), Some(CommandVerb::Update));
        assert_eq!(command_verb("INSERT INTO t VALUES (1)"), Some(CommandVerb::Insert));
        assert_eq!(command_verb("delete from t"), Some(CommandVerb::Delete));
        assert_eq!(command_verb("Merge into t using s on true"), Some(CommandVerb::Merge));
        assert_eq!(command_verb("select 1"), None);
        assert_eq!(command_verb("create index on t (a)"), None);
        assert_eq!(command_verb(""), None);
        assert_eq!(command_verb("-- update t set a = 1"), None);
        assert_eq!(command_verb("/* note */ delete from t"), Some(CommandVerb::Delete));
    }

    /// A statement that changes rows from inside a CTE takes the generic
    /// wording rather than a verb read out of the middle of it. Reading one
    /// there would mean parsing SQL, and the count is right either way.
    #[test]
    fn a_data_modifying_cte_reads_as_no_verb() {
        assert_eq!(
            command_verb(
                "with moved as (delete from t returning *) insert into u select * from moved"
            ),
            None
        );
    }

    #[test]
    fn an_unterminated_quote_swallows_the_rest() {
        // Better to send one broken statement and let the database report
        // it than to split in the middle of a string.
        assert_eq!(texts("select 'oops; select 2"), vec!["select 'oops; select 2"]);
    }
}
