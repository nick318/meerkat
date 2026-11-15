// Package query holds query execution helpers shared by the app and the
// drivers.
//
// Splitting a buffer into statements: a query buffer is a scratchpad of
// several, and a driver runs one command at a time, so the app sends
// them in turn. A semicolon separates statements only when it is plain
// text — inside a string, a quoted identifier, a comment or a
// dollar-quoted body it is just a character.
//
// Reading a statement's transaction verb: the app has to know whether the
// buffer it is about to send opens or ends a transaction itself, because
// that decides whether the app adds a `BEGIN` of its own and what the
// transaction bar says afterwards.
//
// Reading a statement's command verb: a statement that changes rows comes
// back as a count and no result set, and the count alone cannot say
// whether zero means "your `WHERE` matched nothing" or "there was never
// anything to count". The verb is what tells those apart, and it is used
// for the wording only — see [CommandVerbOf].
package query

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Range is a byte range `Start..End` into a string, the port of
// `std::ops::Range<usize>`.
type Range struct {
	Start int
	End   int
}

// IsEmpty reports whether the range holds nothing, as `Range::is_empty`.
func (r Range) IsEmpty() bool {
	return !(r.Start < r.End)
}

// Statements is every non-empty statement in `sql`, in order, ready to
// send.
func Statements(sql string) []string {
	found := StatementRanges(sql)
	out := make([]string, 0, len(found))
	for _, rng := range found {
		out = append(out, sql[rng.Start:rng.End])
	}
	return out
}

// StatementRanges is where each of those statements sits in `sql`, in the
// same order.
//
// The editor marks a run statement by statement in its gutter, so it has
// to know which lines each one covers. The ranges are trimmed and carry
// no separating semicolon, so they are exactly the text [Statements]
// hands the server.
func StatementRanges(sql string) []Range {
	return ranges(sql)
}

// TxVerb is what a statement does to the transaction around it, when that
// is all it does.
type TxVerb int

const (
	// TxVerbBegin is `BEGIN`, `BEGIN TRANSACTION`, `START TRANSACTION`.
	TxVerbBegin TxVerb = iota
	// TxVerbCommit is `COMMIT`, `END`.
	TxVerbCommit
	// TxVerbRollback is `ROLLBACK`, `ABORT` — but never
	// `ROLLBACK TO SAVEPOINT`, which leaves the transaction open and is
	// therefore not an end.
	TxVerbRollback
)

// TransactionVerb is the transaction verb `statement` is, or false for
// everything else.
//
// **What it is for**: in manual mode the app adds a `BEGIN` before a run,
// and it must not add one in front of a buffer that opens its own; and
// after a run the transaction bar says "committed" or "rolled back", which
// it can only know from the buffer when the user typed the word.
//
// Being wrong is cheap on purpose. The server's own answer —
// `Session::in_transaction` — is what the app believes about the state
// afterwards, so a verb misread here costs at most one spare `BEGIN` or a
// bar that says nothing rather than something wrong. That is why this
// reads the first word or two and does not attempt to parse SQL.
//
// Leading comments and whitespace are skipped: `-- go\nCOMMIT` is a
// commit. A `begin` inside a dollar-quoted function body is not seen at
// all, because [Statements] hands such a body over whole.
func TransactionVerb(statement string) (TxVerb, bool) {
	body := skipLeadingComments(statement)
	words := strings.FieldsFunc(body, func(c rune) bool {
		return unicode.IsSpace(c) || c == ';'
	})
	if len(words) == 0 {
		return 0, false
	}
	first := asciiLower(words[0])
	second := ""
	if len(words) > 1 {
		second = asciiLower(words[1])
	}
	switch {
	case first == "begin":
		return TxVerbBegin, true
	// `START` is only a transaction verb with `TRANSACTION` after it.
	case first == "start" && second == "transaction":
		return TxVerbBegin, true
	// `END` closes a transaction; `END` inside a PL/pgSQL body never
	// reaches here, because a dollar-quoted body is one statement.
	case first == "commit" || first == "end":
		return TxVerbCommit, true
	// `ROLLBACK TO [SAVEPOINT] name` unwinds *within* the transaction
	// and leaves it open, so it is not an end.
	case first == "rollback" && second == "to":
		return 0, false
	case first == "rollback" || first == "abort":
		return TxVerbRollback, true
	default:
		return 0, false
	}
}

// CommandVerb is what a statement did to the rows, for a statement that
// answers with a count instead of a result set.
type CommandVerb int

const (
	CommandVerbInsert CommandVerb = iota
	CommandVerbUpdate
	CommandVerbDelete
	// CommandVerbMerge is `MERGE`, which inserts, updates and deletes in
	// one statement — so "merged" is the only word that is true of all of
	// it.
	CommandVerbMerge
)

// Past is the past participle, for `1 row updated`.
func (v CommandVerb) Past() string {
	switch v {
	case CommandVerbInsert:
		return "inserted"
	case CommandVerbUpdate:
		return "updated"
	case CommandVerbDelete:
		return "deleted"
	case CommandVerbMerge:
		return "merged"
	}
	return ""
}

// CommandVerbOf is the row-changing verb `statement` opens with, or false
// for everything else. (Rust: `command_verb`; the Go name `CommandVerb` is
// the type's.)
//
// **What it is for is the wording of a count, and nothing else.** The
// server's tag carries the number of rows but the wire hands the app no
// verb — sqlx reports `rows_affected` and drops the tag itself — so a
// count of `0` reads either as "the `WHERE` matched nothing", which is the
// answer the user is waiting for, or as "a `CREATE INDEX` has nothing to
// count", which is not news. This says which sentence to write.
//
// Being wrong is cheap, as it is in [TransactionVerb], and cheaper: a
// misread costs a generic `1 row affected` in place of `1 row updated`.
// Nothing here decides what runs, what is painted, or what the count is.
// So a data-modifying CTE — `WITH moved AS (DELETE …) INSERT …` — reads as
// no verb and takes the generic wording, rather than being parsed for a
// verb buried in it.
func CommandVerbOf(statement string) (CommandVerb, bool) {
	body := skipLeadingComments(statement)
	word, ok := firstWord(body, func(c rune) bool {
		return unicode.IsSpace(c) || c == ';' || c == '('
	})
	if !ok {
		return 0, false
	}
	switch asciiLower(word) {
	case "insert":
		return CommandVerbInsert, true
	case "update":
		return CommandVerbUpdate, true
	case "delete":
		return CommandVerbDelete, true
	case "merge":
		return CommandVerbMerge, true
	default:
		return 0, false
	}
}

// DdlVerb is what a statement did to the **shape** of the database, for a
// statement that changes tables rather than rows.
type DdlVerb int

const (
	DdlVerbCreate DdlVerb = iota
	DdlVerbAlter
	DdlVerbDrop
	DdlVerbTruncate
)

// Past is the past participle, for `altered · 12 ms`.
func (v DdlVerb) Past() string {
	switch v {
	case DdlVerbCreate:
		return "created"
	case DdlVerbAlter:
		return "altered"
	case DdlVerbDrop:
		return "dropped"
	case DdlVerbTruncate:
		return "truncated"
	}
	return ""
}

// DdlVerbOf is the shape-changing verb `statement` opens with, or false for
// everything else. (Rust: `ddl_verb`; the Go name `DdlVerb` is the type's.)
//
// A `CREATE`, `ALTER`, `DROP` or `TRUNCATE` is the one kind of statement
// whose answer is not on screen afterwards: a `SELECT` paints rows and an
// `UPDATE` reports a count, but a `DROP COLUMN` changes something the user
// is looking at *elsewhere* — the sidebar, the next query's columns. So
// the editor marks such a statement apart from the rest, and the shell
// reads the catalog again once one has landed. This says which statements
// those are, and nothing more: like [CommandVerbOf] it reads the first
// word past the comments and does not parse SQL. `TRUNCATE` is here
// although it changes rows and no shape, because it is the one row-change
// the server reports no count for, and it is irreversible in the same way
// a `DROP` is.
//
// Being wrong is cheap. A missed verb costs a statement painted plain and
// a sidebar refreshed one run late; a false one costs a teal mark and one
// spare read of the catalog. `SELECT … INTO` and `CREATE … AS SELECT` both
// create a table: the second opens with `create` and is read, the first
// does not and is the known gap, as it is for [ChangesNames].
func DdlVerbOf(statement string) (DdlVerb, bool) {
	body := skipLeadingComments(statement)
	word, ok := firstWord(body, func(c rune) bool {
		return unicode.IsSpace(c) || c == ';' || c == '('
	})
	if !ok {
		return 0, false
	}
	switch asciiLower(word) {
	case "create":
		return DdlVerbCreate, true
	case "alter":
		return DdlVerbAlter, true
	case "drop":
		return DdlVerbDrop, true
	case "truncate":
		return DdlVerbTruncate, true
	default:
		return 0, false
	}
}

// The statement past whatever comments open it. A buffer's statements are
// trimmed already, but `-- what this does` on the line above a `COMMIT` is
// part of the statement that follows it.
// IsKeyword reports whether a word is one of SQL's own, whatever case it is
// written in.
func IsKeyword(word string) bool {
	_, found := slices.BinarySearch(Keywords, asciiLower(word))
	return found
}

// Keywords is the keywords the app knows, sorted so the lookup can bisect.
//
// It is here rather than in the editor because three things read it: the
// colouring, the completion panel, and the rule that decides how far back
// a syntax error's mark reaches. A list per reader would be three lists
// that drifted.
var Keywords = []string{
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
}

// RelationTarget is the relation a statement names, for the statements the
// server will not resolve one for.
type RelationTarget struct {
	// Where the name sits in the statement.
	Name Range
	// The statement says outright that a missing one is fine, so nothing
	// is wrong with it and nothing should be marked.
	IfExists bool
}

// RelationTargetOf is the relation `DROP`, `ALTER` or `TRUNCATE` names,
// when that is the shape of the statement, or nil. (Rust:
// `relation_target`; the Go name `RelationTarget` is the type's.)
//
// **Postgres resolves no names for a utility statement until it runs
// one.** `DROP TABLE nosuch` prepares perfectly happily, because the
// grammar is all the parser is asked for; only executing it looks the
// name up. So the check comes back with nothing to say about exactly the
// statements where being told beforehand is worth most, and the app has
// to name the relation itself for the server to be asked about it — see
// `Connection::relation_exists`.
//
// It reads a few words and stops, in [TransactionVerb]'s spirit:
// **anything it does not recognise answers nil**, which costs a mark
// rather than paints a wrong one. So `DROP FUNCTION` and `DROP SCHEMA`
// are not read — they resolve in catalogs of their own — and
// `DROP TABLE a, b` names only `a`, the rest going unchecked.
func RelationTargetOf(statement string) *RelationTarget {
	body := skipLeadingComments(statement)
	shift := len(statement) - len(body)
	next := wordRanges(body)
	text := func(rng Range) string { return body[rng.Start:rng.End] }

	first, ok := next()
	if !ok {
		return nil
	}
	var word Range
	switch asciiLower(text(first)) {
	// `DROP TABLE`, `ALTER VIEW`, `DROP MATERIALIZED VIEW` — every
	// kind here is a *relation*, which is the one thing the server can
	// be asked about with a single call.
	case "drop", "alter":
		kind, ok := next()
		if !ok {
			return nil
		}
		switch asciiLower(text(kind)) {
		case "table", "view", "index", "sequence":
			if word, ok = next(); !ok {
				return nil
			}
		// Two words, and the second is not optional.
		case "materialized", "foreign":
			if _, ok = next(); !ok {
				return nil
			}
			if word, ok = next(); !ok {
				return nil
			}
		// A function, a type, a schema, a role: not a relation.
		default:
			return nil
		}
	case "truncate":
		after, ok := next()
		if !ok {
			return nil
		}
		switch asciiLower(text(after)) {
		case "table", "only":
			if word, ok = next(); !ok {
				return nil
			}
		default:
			word = after
		}
	default:
		return nil
	}

	// `IF EXISTS` says a missing relation is the point of the statement.
	ifExists := false
	if eqIgnoreASCIICase(text(word), "if") {
		exists, ok := next()
		if !ok {
			return nil
		}
		if !eqIgnoreASCIICase(text(exists), "exists") {
			return nil
		}
		ifExists = true
		if word, ok = next(); !ok {
			return nil
		}
	}
	// `TRUNCATE TABLE ONLY t`, and `ALTER TABLE ONLY t` as well.
	if eqIgnoreASCIICase(text(word), "only") {
		if word, ok = next(); !ok {
			return nil
		}
	}

	// Whatever is left has to look like a name. A bracket or a keyword
	// here means the statement is not the shape this reads.
	if word.Start >= word.End {
		return nil
	}
	lead := body[word.Start]
	nameLike := isASCIIAlpha(lead) || lead == '_' || lead == '"' || lead >= 0x80
	if !nameLike {
		return nil
	}
	return &RelationTarget{
		Name:     Range{Start: shift + word.Start, End: shift + word.End},
		IfExists: ifExists,
	}
}

// Every token of a statement, in order, as ranges into it. A qualified
// name is one token, as it is everywhere else here.
//
// The Rust returns an iterator; this returns its `next`.
func wordRanges(body string) func() (Range, bool) {
	ix := 0
	return func() (Range, bool) {
		for ix < len(body) && isASCIIWhitespace(body[ix]) {
			ix++
		}
		if ix >= len(body) {
			return Range{}, false
		}
		start := ix
		ix = tokenEnd(body, start)
		return Range{Start: start, End: ix}, true
	}
}

// WordBefore is the bare name the parser had just read before `offset`, if
// that is what is there.
//
// **A syntax error points at the token the parser choked on, and the
// mistake is usually the word before it.** `… limi 100` is refused at
// `100`, because `limi` parsed perfectly well as a table alias; `select 1
// frm users` is refused at `users`, because `frm` is an alias too. The
// server is right both times and one token late both times, so the mark
// reaches back over this word — see the caller for what makes it safe to.
//
// Only a word that begins like a **name** counts. A number, a bracket or
// an operator before the error is not a word the user misspelled, and
// reaching back over one would widen the mark for nothing.
func WordBefore(statement string, offset int) (Range, bool) {
	cut := min(offset, len(statement))
	if !isCharBoundary(statement, cut) {
		return Range{}, false
	}
	head := strings.TrimRightFunc(statement[:cut], unicode.IsSpace)
	if head == "" {
		return Range{}, false
	}
	rng := tokenBefore(statement, len(head))
	if rng.Start >= rng.End {
		return Range{}, false
	}
	lead := statement[rng.Start]
	if isASCIIAlpha(lead) || lead == '_' || lead >= 0x80 {
		return rng, true
	}
	return Range{}, false
}

// ChangesNames reports whether a statement can change what the names after
// it resolve to.
//
// **It is a veto, not a claim.** A check asks the server to resolve names
// against a connection that has not run the buffer, so
// `CREATE TABLE t (…); SELECT * FROM t;` answers that `t` does not exist —
// which is true right now and false the moment the buffer runs. There is
// no way to tell those apart short of running the DDL, so the app stops
// believing name errors once the buffer holds a statement that could have
// made one.
//
// It reads the first word and nothing else, in [TransactionVerb]'s
// spirit, and it errs toward `true`: a wrong `true` costs a mark that is
// not painted, a wrong `false` paints a mark that is not real. `SET` is in
// the list because `SET search_path` changes what an unqualified name
// means, which is the same problem wearing different clothes. A
// `SELECT … INTO` creates a table without opening with a verb that says
// so, and is the known gap.
func ChangesNames(statement string) bool {
	body := skipLeadingComments(statement)
	first, ok := firstWord(body, func(c rune) bool {
		return unicode.IsSpace(c) || c == ';'
	})
	if !ok {
		return false
	}
	switch asciiLower(first) {
	case "create", "drop", "alter", "set", "reset", "rename", "import":
		return true
	}
	return false
}

// ErrorSpan is what to mark for an error the server put at `offset` in
// `statement`.
//
// The server names a **point**, and a point cannot be underlined. So this
// grows the point into the token it landed on: the word, the string, or
// the one character of punctuation. Marking to the end of the statement
// instead would put a squiggle under the half of the query that is
// usually right.
//
// **At the end of the input the point is past every token**, which is what
// `syntax error at end of input` reports for an unfinished statement. The
// mark then goes on the **last** token: there is nothing after it to
// underline, and the word the user stopped on is where they will look.
//
// The range is always non-empty for a non-empty statement, or the
// squiggle would be invisible and the error would be reported by nothing
// at all.
func ErrorSpan(statement string, offset int) Range {
	// The driver converts the server's character position into a byte
	// offset, so this is already on a boundary — but the offset crossed a
	// wire, and slicing off one would panic the window rather than mark
	// the wrong token.
	offset = min(offset, len(statement))
	for offset > 0 && !isCharBoundary(statement, offset) {
		offset--
	}
	// Past the last token: everything from here on is space. Walk back to
	// the token the user stopped on.
	if strings.TrimSpace(statement[offset:]) == "" {
		return tokenBefore(statement, len(strings.TrimRightFunc(statement, unicode.IsSpace)))
	}
	// The point can land on the space in front of the offending token, so
	// step over any space first.
	tail := statement[offset:]
	start := offset + (len(tail) - len(strings.TrimLeftFunc(tail, unicode.IsSpace)))
	return Range{Start: start, End: tokenEnd(statement, start)}
}

// Where the name or token starting at `start` ends.
//
// **A qualified name is one name.** The server points at the front of
// `schema.table` and refuses the pair — `relation "schema.table" does not
// exist` — so a mark that stopped at the first dot would underline the
// half that is usually right and leave the wrong half bare. So the walk
// carries on over every `.` that has another name part after it, which is
// how `a.b.c` is marked whole and how `t.*` still stops at `t`.
func tokenEnd(statement string, start int) int {
	end := oneTokenEnd(statement, start)
	for end < len(statement) && statement[end] == '.' {
		if end+1 < len(statement) && (isWordByte(statement[end+1]) || statement[end+1] == '"') {
			end = oneTokenEnd(statement, end+1)
		} else {
			break
		}
	}
	return end
}

// Where one part of a name — or one token that is not a name — ends.
func oneTokenEnd(statement string, start int) int {
	if start >= len(statement) {
		return start
	}
	switch b := statement[start]; {
	// A string or a quoted name is one token however long it runs. An
	// unterminated one — which is an error in its own right — runs to
	// the end of the statement, and marking all of it is right: the
	// whole tail is inside the quote.
	case b == '\'' || b == '"':
		quote := b
		ix := start + 1
		for ix < len(statement) {
			if statement[ix] == quote {
				// A doubled quote is an escape, not the end.
				if ix+1 < len(statement) && statement[ix+1] == quote {
					ix += 2
					continue
				}
				return ix + 1
			}
			ix++
		}
		return len(statement)
	case isWordByte(b):
		ix := start
		for ix < len(statement) && isWordByte(statement[ix]) {
			ix++
		}
		return ix
	// Punctuation, or any character the scanner has no word for: one
	// character, on its own boundary so the slice stays valid.
	default:
		ix := start + 1
		for ix < len(statement) && !isCharBoundary(statement, ix) {
			ix++
		}
		return ix
	}
}

// The token that ends at `end`, walking backwards.
//
// A name and its dots are one run here too, so an unfinished
// `select * from schema.` marks the schema it stopped after rather than
// the lone dot.
func tokenBefore(statement string, end int) Range {
	if end == 0 {
		return Range{Start: 0, End: 0}
	}
	part := func(b byte) bool { return isWordByte(b) || b == '.' }
	if part(statement[end-1]) {
		start := end
		for start > 0 && part(statement[start-1]) {
			start--
		}
		return Range{Start: start, End: end}
	}
	// One character back, on its own boundary.
	start := end - 1
	for start > 0 && !isCharBoundary(statement, start) {
		start--
	}
	return Range{Start: start, End: end}
}

// A byte the scanner reads as part of a name or a number. `$` is in, for
// `$1` and for the `$tag$` a dollar-quoted body opens with; anything
// above ASCII is in, because a name may be any of it.
func isWordByte(b byte) bool {
	return isASCIIAlphanumeric(b) || b == '_' || b == '$' || b >= 0x80
}

func skipLeadingComments(statement string) string {
	rest := strings.TrimLeftFunc(statement, unicode.IsSpace)
	for {
		if after, ok := strings.CutPrefix(rest, "--"); ok {
			end := strings.IndexByte(after, '\n')
			if end < 0 {
				// A line comment with nothing after it is the whole
				// statement, and the whole statement is then no verb.
				return ""
			}
			rest = strings.TrimLeftFunc(after[end+1:], unicode.IsSpace)
			continue
		}
		if strings.HasPrefix(rest, "/*") {
			end := endOfBlockComment([]byte(rest), 0)
			rest = strings.TrimLeftFunc(rest[end:], unicode.IsSpace)
			continue
		}
		return rest
	}
}

// Byte ranges of the statements in `text`, whitespace trimmed and the
// separating semicolons left out.
func ranges(text string) []Range {
	bytes := []byte(text)
	var statements []Range
	start := 0
	ix := 0

	for ix < len(bytes) {
		switch {
		case bytes[ix] == '\'':
			ix = endOfQuoted(bytes, ix, '\'')
		case bytes[ix] == '"':
			ix = endOfQuoted(bytes, ix, '"')
		case bytes[ix] == '-' && ix+1 < len(bytes) && bytes[ix+1] == '-':
			if found, ok := memchr(bytes, '\n', ix); ok {
				ix = found + 1
			} else {
				ix = len(bytes)
			}
		case bytes[ix] == '/' && ix+1 < len(bytes) && bytes[ix+1] == '*':
			ix = endOfBlockComment(bytes, ix)
		case bytes[ix] == '$':
			if end, ok := endOfDollarQuoted(bytes, ix); ok {
				ix = end
			} else {
				ix++
			}
		case bytes[ix] == ';':
			push(&statements, text, Range{Start: start, End: ix})
			ix++
			start = ix
		default:
			ix++
		}
	}
	push(&statements, text, Range{Start: start, End: len(bytes)})
	return statements
}

func push(statements *[]Range, text string, rng Range) {
	trimmed := trim(text, rng)
	if !trimmed.IsEmpty() {
		*statements = append(*statements, trimmed)
	}
}

func trim(text string, rng Range) Range {
	slice := text[rng.Start:rng.End]
	start := rng.Start + (len(slice) - len(strings.TrimLeftFunc(slice, unicode.IsSpace)))
	end := rng.End - (len(slice) - len(strings.TrimRightFunc(slice, unicode.IsSpace)))
	return Range{Start: start, End: max(end, start)}
}

func memchr(bytes []byte, needle byte, from int) (int, bool) {
	for ix := from; ix < len(bytes); ix++ {
		if bytes[ix] == needle {
			return ix, true
		}
	}
	return 0, false
}

// Index just past the closing quote, treating a doubled quote as an
// escape. An unterminated quote runs to the end of the buffer.
func endOfQuoted(bytes []byte, start int, quote byte) int {
	ix := start + 1
	for ix < len(bytes) {
		if bytes[ix] == quote {
			if ix+1 < len(bytes) && bytes[ix+1] == quote {
				ix += 2
				continue
			}
			return ix + 1
		}
		ix++
	}
	return len(bytes)
}

func endOfBlockComment(bytes []byte, start int) int {
	ix := start + 2
	// Postgres nests block comments, unlike C.
	depth := 1
	for ix+1 < len(bytes) {
		if bytes[ix] == '/' && bytes[ix+1] == '*' {
			depth++
			ix += 2
		} else if bytes[ix] == '*' && bytes[ix+1] == '/' {
			depth--
			ix += 2
			if depth == 0 {
				return ix
			}
		} else {
			ix++
		}
	}
	return len(bytes)
}

// Index just past a `$tag$ ... $tag$` body, or false when the `$` does
// not open one.
func endOfDollarQuoted(bytes []byte, start int) (int, bool) {
	ix := start + 1
	for ix < len(bytes) && (isASCIIAlphanumeric(bytes[ix]) || bytes[ix] == '_') {
		ix++
	}
	if ix >= len(bytes) || bytes[ix] != '$' {
		return 0, false
	}
	tag := bytes[start : ix+1]
	ix = ix + 1
	for ix+len(tag) <= len(bytes) {
		if string(bytes[ix:ix+len(tag)]) == string(tag) {
			return ix + len(tag), true
		}
		ix++
	}
	return len(bytes), true
}

// The Rust standard library's ASCII helpers and `str` methods, spelled out
// because Go's nearest equivalents read Unicode where Rust reads ASCII.

// firstWord is `split(pred).find(|w| !w.is_empty())`.
func firstWord(body string, pred func(rune) bool) (string, bool) {
	words := strings.FieldsFunc(body, pred)
	if len(words) == 0 {
		return "", false
	}
	return words[0], true
}

// asciiLower is `str::to_ascii_lowercase`: only `A`–`Z` change.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// eqIgnoreASCIICase is `str::eq_ignore_ascii_case`.
func eqIgnoreASCIICase(a, b string) bool {
	return len(a) == len(b) && asciiLower(a) == asciiLower(b)
}

// isASCIIAlpha is `u8::is_ascii_alphabetic`.
func isASCIIAlpha(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// isASCIIAlphanumeric is `u8::is_ascii_alphanumeric`.
func isASCIIAlphanumeric(b byte) bool {
	return isASCIIAlpha(b) || ('0' <= b && b <= '9')
}

// isASCIIWhitespace is `u8::is_ascii_whitespace`: space, tab, line feed,
// form feed and carriage return — not the vertical tab.
func isASCIIWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\x0C' || b == '\r'
}

// isCharBoundary is `str::is_char_boundary`.
func isCharBoundary(s string, ix int) bool {
	if ix == 0 || ix == len(s) {
		return true
	}
	if ix > len(s) {
		return false
	}
	return utf8.RuneStart(s[ix])
}
