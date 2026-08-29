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
    statement_ranges(sql)
        .into_iter()
        .map(|range| sql[range].to_string())
        .collect()
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
    let mut words = body
        .split(|c: char| c.is_whitespace() || c == ';')
        .filter(|w| !w.is_empty());
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
/// Whether a word is one of SQL's own, whatever case it is written in.
pub fn is_keyword(word: &str) -> bool {
    KEYWORDS
        .binary_search(&word.to_ascii_lowercase().as_str())
        .is_ok()
}

/// The keywords the app knows, sorted so the lookup can bisect.
///
/// It is here rather than in the editor because three things read it: the
/// colouring, the completion panel, and the rule that decides how far back
/// a syntax error's mark reaches. A list per reader would be three lists
/// that drifted.
pub const KEYWORDS: &[&str] = &[
    "all",
    "alter",
    "and",
    "any",
    "array",
    "as",
    "asc",
    "begin",
    "between",
    "by",
    "case",
    "cast",
    "coalesce",
    "commit",
    "count",
    "create",
    "cross",
    "current_date",
    "current_timestamp",
    "delete",
    "desc",
    "distinct",
    "drop",
    "else",
    "end",
    "except",
    "exists",
    "explain",
    "false",
    "filter",
    "first",
    "from",
    "full",
    "group",
    "having",
    "ilike",
    "in",
    "index",
    "inner",
    "insert",
    "intersect",
    "into",
    "is",
    "join",
    "lateral",
    "left",
    "like",
    "limit",
    "max",
    "min",
    "not",
    "null",
    "nulls",
    "offset",
    "on",
    "or",
    "order",
    "outer",
    "over",
    "partition",
    "returning",
    "right",
    "rollback",
    "select",
    "set",
    "some",
    "sum",
    "table",
    "then",
    "true",
    "union",
    "update",
    "using",
    "values",
    "view",
    "when",
    "where",
    "window",
    "with",
];

/// The relation a statement names, for the statements the server will not
/// resolve one for.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RelationTarget {
    /// Where the name sits in the statement.
    pub name: Range<usize>,
    /// The statement says outright that a missing one is fine, so nothing
    /// is wrong with it and nothing should be marked.
    pub if_exists: bool,
}

/// The relation `DROP`, `ALTER` or `TRUNCATE` names, when that is the
/// shape of the statement.
///
/// **Postgres resolves no names for a utility statement until it runs
/// one.** `DROP TABLE nosuch` prepares perfectly happily, because the
/// grammar is all the parser is asked for; only executing it looks the
/// name up. So the check comes back with nothing to say about exactly the
/// statements where being told beforehand is worth most, and the app has
/// to name the relation itself for the server to be asked about it — see
/// `Connection::relation_exists`.
///
/// It reads a few words and stops, in [`transaction_verb`]'s spirit:
/// **anything it does not recognise answers `None`**, which costs a mark
/// rather than paints a wrong one. So `DROP FUNCTION` and `DROP SCHEMA`
/// are not read — they resolve in catalogs of their own — and
/// `DROP TABLE a, b` names only `a`, the rest going unchecked.
pub fn relation_target(statement: &str) -> Option<RelationTarget> {
    let body = skip_leading_comments(statement);
    let shift = statement.len() - body.len();
    let mut words = word_ranges(body);
    let mut next = || words.next();

    let first = next()?;
    let verb = body[first.clone()].to_ascii_lowercase();
    let mut word = match verb.as_str() {
        // `DROP TABLE`, `ALTER VIEW`, `DROP MATERIALIZED VIEW` — every
        // kind here is a *relation*, which is the one thing the server can
        // be asked about with a single call.
        "drop" | "alter" => {
            let kind = next()?;
            match body[kind.clone()].to_ascii_lowercase().as_str() {
                "table" | "view" | "index" | "sequence" => next()?,
                // Two words, and the second is not optional.
                "materialized" | "foreign" => {
                    next()?;
                    next()?
                }
                // A function, a type, a schema, a role: not a relation.
                _ => return None,
            }
        }
        "truncate" => {
            let after = next()?;
            match body[after.clone()].to_ascii_lowercase().as_str() {
                "table" | "only" => next()?,
                _ => after,
            }
        }
        _ => return None,
    };

    // `IF EXISTS` says a missing relation is the point of the statement.
    let mut if_exists = false;
    if body[word.clone()].eq_ignore_ascii_case("if") {
        let exists = next()?;
        if !body[exists].eq_ignore_ascii_case("exists") {
            return None;
        }
        if_exists = true;
        word = next()?;
    }
    // `TRUNCATE TABLE ONLY t`, and `ALTER TABLE ONLY t` as well.
    if body[word.clone()].eq_ignore_ascii_case("only") {
        word = next()?;
    }

    // Whatever is left has to look like a name. A bracket or a keyword
    // here means the statement is not the shape this reads.
    let first = body[word.clone()].as_bytes().first().copied()?;
    let name_like = first.is_ascii_alphabetic() || first == b'_' || first == b'"' || first >= 0x80;
    if !name_like {
        return None;
    }
    Some(RelationTarget {
        name: shift + word.start..shift + word.end,
        if_exists,
    })
}

/// Every token of a statement, in order, as ranges into it. A qualified
/// name is one token, as it is everywhere else here.
fn word_ranges(body: &str) -> impl Iterator<Item = Range<usize>> + '_ {
    let mut ix = 0;
    std::iter::from_fn(move || {
        let bytes = body.as_bytes();
        while ix < bytes.len() && bytes[ix].is_ascii_whitespace() {
            ix += 1;
        }
        if ix >= bytes.len() {
            return None;
        }
        let start = ix;
        ix = token_end(body, start);
        Some(start..ix)
    })
}

/// The bare name the parser had just read before `offset`, if that is what
/// is there.
///
/// **A syntax error points at the token the parser choked on, and the
/// mistake is usually the word before it.** `… limi 100` is refused at
/// `100`, because `limi` parsed perfectly well as a table alias; `select 1
/// frm users` is refused at `users`, because `frm` is an alias too. The
/// server is right both times and one token late both times, so the mark
/// reaches back over this word — see the caller for what makes it safe to.
///
/// Only a word that begins like a **name** counts. A number, a bracket or
/// an operator before the error is not a word the user misspelled, and
/// reaching back over one would widen the mark for nothing.
pub fn word_before(statement: &str, offset: usize) -> Option<Range<usize>> {
    let head = statement.get(..offset.min(statement.len()))?.trim_end();
    if head.is_empty() {
        return None;
    }
    let range = token_before(statement, head.len());
    let first = statement[range.clone()].as_bytes().first().copied()?;
    (first.is_ascii_alphabetic() || first == b'_' || first >= 0x80).then_some(range)
}

/// Whether a statement can change what the names after it resolve to.
///
/// **It is a veto, not a claim.** A check asks the server to resolve names
/// against a connection that has not run the buffer, so
/// `CREATE TABLE t (…); SELECT * FROM t;` answers that `t` does not exist —
/// which is true right now and false the moment the buffer runs. There is
/// no way to tell those apart short of running the DDL, so the app stops
/// believing name errors once the buffer holds a statement that could have
/// made one.
///
/// It reads the first word and nothing else, in [`transaction_verb`]'s
/// spirit, and it errs toward `true`: a wrong `true` costs a mark that is
/// not painted, a wrong `false` paints a mark that is not real. `SET` is in
/// the list because `SET search_path` changes what an unqualified name
/// means, which is the same problem wearing different clothes. A
/// `SELECT … INTO` creates a table without opening with a verb that says
/// so, and is the known gap.
pub fn changes_names(statement: &str) -> bool {
    let body = skip_leading_comments(statement);
    let Some(first) = body
        .split(|c: char| c.is_whitespace() || c == ';')
        .find(|w| !w.is_empty())
    else {
        return false;
    };
    matches!(
        first.to_ascii_lowercase().as_str(),
        "create" | "drop" | "alter" | "set" | "reset" | "rename" | "import"
    )
}

/// What to mark for an error the server put at `offset` in `statement`.
///
/// The server names a **point**, and a point cannot be underlined. So this
/// grows the point into the token it landed on: the word, the string, or
/// the one character of punctuation. Marking to the end of the statement
/// instead would put a squiggle under the half of the query that is
/// usually right.
///
/// **At the end of the input the point is past every token**, which is what
/// `syntax error at end of input` reports for an unfinished statement. The
/// mark then goes on the **last** token: there is nothing after it to
/// underline, and the word the user stopped on is where they will look.
///
/// The range is always non-empty for a non-empty statement, or the
/// squiggle would be invisible and the error would be reported by nothing
/// at all.
pub fn error_span(statement: &str, offset: usize) -> Range<usize> {
    // The driver converts the server's character position into a byte
    // offset, so this is already on a boundary — but the offset crossed a
    // wire, and slicing off one would panic the window rather than mark
    // the wrong token.
    let mut offset = offset.min(statement.len());
    while offset > 0 && !statement.is_char_boundary(offset) {
        offset -= 1;
    }
    // Past the last token: everything from here on is space. Walk back to
    // the token the user stopped on.
    if statement[offset..].trim().is_empty() {
        return token_before(statement, statement.trim_end().len());
    }
    // The point can land on the space in front of the offending token, so
    // step over any space first.
    let start = offset + (statement[offset..].len() - statement[offset..].trim_start().len());
    start..token_end(statement, start)
}

/// Where the name or token starting at `start` ends.
///
/// **A qualified name is one name.** The server points at the front of
/// `schema.table` and refuses the pair — `relation "schema.table" does not
/// exist` — so a mark that stopped at the first dot would underline the
/// half that is usually right and leave the wrong half bare. So the walk
/// carries on over every `.` that has another name part after it, which is
/// how `a.b.c` is marked whole and how `t.*` still stops at `t`.
fn token_end(statement: &str, start: usize) -> usize {
    let bytes = statement.as_bytes();
    let mut end = one_token_end(statement, start);
    while bytes.get(end) == Some(&b'.') {
        match bytes.get(end + 1) {
            Some(&byte) if is_word_byte(byte) || byte == b'"' => {
                end = one_token_end(statement, end + 1);
            }
            _ => break,
        }
    }
    end
}

/// Where one part of a name — or one token that is not a name — ends.
fn one_token_end(statement: &str, start: usize) -> usize {
    let bytes = statement.as_bytes();
    match bytes.get(start) {
        None => start,
        // A string or a quoted name is one token however long it runs. An
        // unterminated one — which is an error in its own right — runs to
        // the end of the statement, and marking all of it is right: the
        // whole tail is inside the quote.
        Some(&quote @ (b'\'' | b'"')) => {
            let mut ix = start + 1;
            while ix < bytes.len() {
                if bytes[ix] == quote {
                    // A doubled quote is an escape, not the end.
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
        Some(&byte) if is_word_byte(byte) => {
            let mut ix = start;
            while ix < bytes.len() && is_word_byte(bytes[ix]) {
                ix += 1;
            }
            ix
        }
        // Punctuation, or any character the scanner has no word for: one
        // character, on its own boundary so the slice stays valid.
        _ => {
            let mut ix = start + 1;
            while ix < bytes.len() && !statement.is_char_boundary(ix) {
                ix += 1;
            }
            ix
        }
    }
}

/// The token that ends at `end`, walking backwards.
///
/// A name and its dots are one run here too, so an unfinished
/// `select * from schema.` marks the schema it stopped after rather than
/// the lone dot.
fn token_before(statement: &str, end: usize) -> Range<usize> {
    let bytes = statement.as_bytes();
    if end == 0 {
        return 0..0;
    }
    let part = |byte: u8| is_word_byte(byte) || byte == b'.';
    if part(bytes[end - 1]) {
        let mut start = end;
        while start > 0 && part(bytes[start - 1]) {
            start -= 1;
        }
        return start..end;
    }
    // One character back, on its own boundary.
    let mut start = end - 1;
    while start > 0 && !statement.is_char_boundary(start) {
        start -= 1;
    }
    start..end
}

/// A byte the scanner reads as part of a name or a number. `$` is in, for
/// `$1` and for the `$tag$` a dollar-quoted body opens with; anything
/// above ASCII is in, because a name may be any of it.
fn is_word_byte(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || byte == b'_' || byte == b'$' || byte >= 0x80
}

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
                ix = memchr(bytes, b'\n', ix)
                    .map(|ix| ix + 1)
                    .unwrap_or(bytes.len());
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
    bytes[from..]
        .iter()
        .position(|byte| *byte == needle)
        .map(|ix| from + ix)
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

    fn marked(statement: &str, offset: usize) -> &str {
        &statement[error_span(statement, offset)]
    }

    #[test]
    fn the_keyword_list_is_sorted_for_bisection() {
        let mut sorted = KEYWORDS.to_vec();
        sorted.sort_unstable();
        assert_eq!(KEYWORDS, sorted.as_slice());
        assert!(is_keyword("SELECT") && is_keyword("select"));
        assert!(!is_keyword("selected"));
    }

    fn target(statement: &str) -> Option<(&str, bool)> {
        relation_target(statement).map(|found| (&statement[found.name], found.if_exists))
    }

    /// The statements Postgres will not resolve a name for until it runs
    /// them, which is why the app has to name the relation itself.
    #[test]
    fn a_utility_statement_names_its_relation() {
        let dropped = target("drop table tenant_dev_tenant.effort1");
        assert_eq!(dropped, Some(("tenant_dev_tenant.effort1", false)));
        assert_eq!(target("DROP TABLE IF EXISTS t"), Some(("t", true)));
        assert_eq!(target("drop materialized view mv"), Some(("mv", false)));
        assert_eq!(target("alter table only t"), Some(("t", false)));
        assert_eq!(target("truncate t"), Some(("t", false)));
        assert_eq!(target("truncate table only t"), Some(("t", false)));
        assert_eq!(target("-- go\n  drop view v"), Some(("v", false)));
        assert_eq!(
            target("drop table \"My Table\""),
            Some(("\"My Table\"", false))
        );
        // Only the first of a list; the rest go unchecked rather than
        // wrongly checked.
        assert_eq!(target("drop table a, b"), Some(("a", false)));
    }

    /// Anything the reader does not recognise answers `None`, which costs
    /// a mark rather than painting a wrong one.
    #[test]
    fn everything_else_names_nothing() {
        for statement in [
            // Not relations: they live in catalogs of their own.
            "drop function f(int)",
            "drop schema s",
            "drop type t",
            "drop role r",
            // Not the shape at all.
            "select * from t",
            "create table t (id int)",
            "alter",
            "",
            // `IF` without `EXISTS` is not this statement.
            "drop table if t",
        ] {
            assert_eq!(relation_target(statement), None, "{statement}");
        }
    }

    /// The word the parser had just read, which is where the mistake
    /// usually is.
    #[test]
    fn the_word_before_an_error_is_the_one_that_was_read() {
        let statement = "select * from t limi 100";
        let range = word_before(statement, 21).unwrap();
        assert_eq!(&statement[range], "limi");
        // A qualified name comes back whole, as it is marked whole.
        let statement = "select * from a.b 100";
        let range = word_before(statement, 18).unwrap();
        assert_eq!(&statement[range], "a.b");
        // Nothing that is not a name: a number, an operator, the front of
        // the statement.
        assert_eq!(word_before("select 100 200", 11), None);
        assert_eq!(word_before("select x = = 1", 11), None);
        assert_eq!(word_before("select 1", 0), None);
        assert_eq!(word_before("  select 1", 2), None);
    }

    #[test]
    fn a_statement_that_could_make_a_name_is_read_as_one() {
        for statement in [
            "create table t (id int)",
            "CREATE TEMP TABLE t AS SELECT 1",
            "drop table t",
            "alter table t add column c int",
            "set search_path to app",
            "-- first\n  create schema s",
        ] {
            assert!(changes_names(statement), "{statement}");
        }
        for statement in [
            "select * from t",
            "insert into t values (1)",
            "begin",
            "",
            "   ",
        ] {
            assert!(!changes_names(statement), "{statement}");
        }
    }

    /// **The case a viewer meets every day.** The server points at the
    /// front of `schema.table` and refuses the pair, so the mark has to
    /// carry the dots: `tenant_dev_tenant` alone is the half that is
    /// right.
    #[test]
    fn a_qualified_name_is_marked_whole() {
        let statement = "select * from tenant_dev_tenant.effor limit 100";
        assert_eq!(marked(statement, 14), "tenant_dev_tenant.effor");
        assert_eq!(marked("select a.b.c from t", 7), "a.b.c");
        assert_eq!(
            marked("select \"my schema\".t from x", 7),
            "\"my schema\".t"
        );
        // A dot with no name after it is not part of the name, and `*` is
        // not a name part either.
        assert_eq!(marked("select * from schema. limit 1", 14), "schema");
        assert_eq!(marked("select t.* from t", 7), "t");
    }

    /// An unfinished qualified name marks the part that is there.
    #[test]
    fn an_unfinished_qualified_name_marks_what_was_typed() {
        assert_eq!(marked("select * from schema.", 21), "schema.");
    }

    /// The server names a point; the mark is the token it points at.
    #[test]
    fn the_error_marks_the_token_the_cursor_landed_on() {
        let statement = "select 1 frm users";
        assert_eq!(marked(statement, 13), "users");
        assert_eq!(marked(statement, 9), "frm");
    }

    /// A statement the user has not finished: the point is past the last
    /// token, so the mark goes on the token itself.
    #[test]
    fn the_end_of_the_input_marks_the_last_token() {
        assert_eq!(marked("select", 6), "select");
        // Trailing space is not the answer to "where did I stop".
        assert_eq!(marked("select from  ", 13), "from");
        assert_eq!(marked("select *", 8), "*");
    }

    /// A string is one token however long it runs, and an unterminated one
    /// swallows the rest of the statement — which is exactly what is wrong
    /// with it.
    #[test]
    fn a_string_is_marked_whole() {
        assert_eq!(marked("select 'a b c' from t", 7), "'a b c'");
        assert_eq!(marked("select 'a''b' from t", 7), "'a''b'");
        assert_eq!(marked("select 'oops from t", 7), "'oops from t");
        assert_eq!(marked("select \"a b\" from t", 7), "\"a b\"");
    }

    /// Punctuation is one character, and a multi-byte character is one
    /// character rather than one byte — a range that split one would not
    /// slice.
    #[test]
    fn punctuation_marks_one_character() {
        assert_eq!(marked("select ) from t", 7), ")");
        assert_eq!(marked("select § from t", 7), "§");
        assert_eq!(marked("select §", 9), "§");
        // An offset that landed inside a character marks the character.
        assert_eq!(marked("select §", 8), "§");
    }

    /// A point on the space in front of a token still marks the token: an
    /// empty range paints nothing, and an error painted by nothing is an
    /// error the user never sees.
    #[test]
    fn the_mark_is_never_empty() {
        assert_eq!(marked("select  frm", 6), "frm");
        for offset in 0..=12 {
            assert!(
                !error_span("select 1 frm", offset).is_empty(),
                "offset {offset}"
            );
        }
        // Nothing to mark in nothing.
        assert_eq!(error_span("", 0), 0..0);
        assert_eq!(error_span("   ", 3), 0..0);
    }

    /// The offset is the server's, and a server that names one past the
    /// end must not panic the app.
    #[test]
    fn an_offset_past_the_end_is_clamped() {
        assert_eq!(marked("select", 99), "select");
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
        assert_eq!(
            texts(text),
            vec!["select /* ; nested /* ; */ still */ 1", "select 2"]
        );
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
        assert_eq!(
            texts("select 1 $ 2; select 3"),
            vec!["select 1 $ 2", "select 3"]
        );
    }

    #[test]
    fn the_transaction_verbs_are_read_off_the_first_word() {
        let verb = |sql| transaction_verb(sql);
        assert_eq!(verb("BEGIN"), Some(TxVerb::Begin));
        assert_eq!(verb("begin transaction"), Some(TxVerb::Begin));
        assert_eq!(
            verb("BEGIN ISOLATION LEVEL SERIALIZABLE"),
            Some(TxVerb::Begin)
        );
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
        assert_eq!(
            transaction_verb("/* land it */ commit"),
            Some(TxVerb::Commit)
        );
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
        assert_eq!(
            command_verb("update t set a = 1"),
            Some(CommandVerb::Update)
        );
        assert_eq!(
            command_verb("INSERT INTO t VALUES (1)"),
            Some(CommandVerb::Insert)
        );
        assert_eq!(command_verb("delete from t"), Some(CommandVerb::Delete));
        assert_eq!(
            command_verb("Merge into t using s on true"),
            Some(CommandVerb::Merge)
        );
        assert_eq!(command_verb("select 1"), None);
        assert_eq!(command_verb("create index on t (a)"), None);
        assert_eq!(command_verb(""), None);
        assert_eq!(command_verb("-- update t set a = 1"), None);
        assert_eq!(
            command_verb("/* note */ delete from t"),
            Some(CommandVerb::Delete)
        );
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
        assert_eq!(
            texts("select 'oops; select 2"),
            vec!["select 'oops; select 2"]
        );
    }
}
