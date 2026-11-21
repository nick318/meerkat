package query

// Writing a name the way a statement has to spell it.
//
// Postgres folds an unquoted name to lower case and refuses a reserved
// word as a bare name, so a table called `Users`, `order` or `my table`
// is only reachable as `"Users"`, `"order"`, `"my table"`. A completion
// that wrote the name bare wrote a statement that fails — or, for
// `Users` when `users` also exists, one that reads the wrong table.

import "slices"

// QuoteIdentIfNeeded is `name` as a statement must write it: bare when
// Postgres reads it back unchanged, in double quotes (with any quote
// inside it doubled) when it would not.
func QuoteIdentIfNeeded(name string) string {
	if !NeedsQuotes(name) {
		return name
	}
	return QuoteIdent(name)
}

// QuoteIdent is `name` in double quotes, a quote inside it doubled.
func QuoteIdent(name string) string {
	out := make([]byte, 0, len(name)+2)
	out = append(out, '"')
	for i := 0; i < len(name); i++ {
		if name[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, name[i])
	}
	return string(append(out, '"'))
}

// NeedsQuotes says whether a bare `name` would read as something else:
// it is empty, starts with a digit or `$`, holds anything but lower-case
// letters, digits, `_` and `$`, or is a word Postgres reserves.
func NeedsQuotes(name string) bool {
	if name == "" {
		return true
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_', c >= 0x80:
		case (c >= '0' && c <= '9') || c == '$':
			if i == 0 {
				return true
			}
		default:
			return true
		}
	}
	_, reserved := slices.BinarySearch(reservedWords, name)
	return reserved
}

// reservedWords is every key word PostgreSQL marks reserved, or reserved
// but allowed as a function or type name — the ones a bare column or
// table name cannot be (Appendix C of the manual). Sorted, for the
// search.
var reservedWords = []string{
	"all", "analyse", "analyze", "and", "any", "array", "as", "asc",
	"asymmetric", "authorization", "binary", "both", "case", "cast",
	"check", "collate", "collation", "column", "concurrently",
	"constraint", "create", "cross", "current_catalog", "current_date",
	"current_role", "current_schema", "current_time", "current_timestamp",
	"current_user", "default", "deferrable", "desc", "distinct", "do",
	"else", "end", "except", "false", "fetch", "for", "foreign", "freeze",
	"from", "full", "grant", "group", "having", "ilike", "in", "initially",
	"inner", "intersect", "into", "is", "isnull", "join", "lateral",
	"leading", "left", "like", "limit", "localtime", "localtimestamp",
	"natural", "not", "notnull", "null", "offset", "on", "only", "or",
	"order", "outer", "overlaps", "placing", "primary", "references",
	"returning", "right", "select", "session_user", "similar", "some",
	"symmetric", "system_user", "table", "tablesample", "then", "to",
	"trailing", "true", "union", "unique", "user", "using", "variadic",
	"verbose", "when", "where", "window", "with",
}
