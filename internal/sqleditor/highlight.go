package sqleditor

// A one-pass SQL tokenizer, just enough to colour the editor the way the
// design comp does: keywords in the deep accent, literals in green,
// comments muted, everything else in the body colour.
//
// It also colours the names of the connected database. A word that the
// catalog knows — a schema, a table, a view or a column — gets the
// identifier colour; a word it does not know stays plain, so a typo is
// visible before the query runs.
//
// It works a line at a time, so a string literal that spans lines loses
// its colour after the first newline. That is the price of not carrying
// a parser; nothing else depends on this being exact.
//
// The keywords themselves live in `query`, with the rest of what the app
// knows about SQL text: the editor is not the only thing that has to tell
// a keyword from a name.

import (
	"unicode/utf8"

	"github.com/nick318/meerkat/internal/query"
)

type Token int

const (
	TokenKeyword Token = iota
	TokenLiteral
	TokenComment
	// TokenIdentifier is a name the connected database actually has.
	TokenIdentifier
	TokenPlain
)

// String names the token as Rust's Debug did.
func (t Token) String() string {
	switch t {
	case TokenKeyword:
		return "Keyword"
	case TokenLiteral:
		return "Literal"
	case TokenComment:
		return "Comment"
	case TokenIdentifier:
		return "Identifier"
	case TokenPlain:
		return "Plain"
	}
	return "Token(?)"
}

// span is one `(Range<usize>, Token)` of a line.
type span struct {
	Range Range
	Token Token
}

// spans splits one line into coloured spans. The spans are in order, they
// never overlap, and their lengths add up to the length of the line, so
// they can be handed straight to the text system as runs.
func spans(line string, vocabulary *Vocabulary) []span {
	bytes := line
	var out []span
	ix := 0

	for ix < len(bytes) {
		start := ix
		var token Token
		switch c := bytes[ix]; {
		case c == '-' && ix+1 < len(bytes) && bytes[ix+1] == '-':
			ix = len(bytes)
			token = TokenComment
		case c == '\'':
			ix = endOfQuoted(bytes, ix, '\'')
			token = TokenLiteral
		// A quoted identifier is a name, not a literal.
		case c == '"':
			ix = endOfQuoted(bytes, ix, '"')
			token = TokenPlain
		case c >= '0' && c <= '9':
			for ix < len(bytes) && (isASCIIDigit(bytes[ix]) || bytes[ix] == '.') {
				ix++
			}
			token = TokenLiteral
		case isWordByte(c):
			for ix < len(bytes) && isWordByte(bytes[ix]) {
				ix++
			}
			word := line[start:ix]
			// Keywords win: a column called `order` still reads as a
			// keyword, which is what the SQL parser will do with it.
			if query.IsKeyword(word) {
				token = TokenKeyword
			} else if vocabulary.Contains(word) {
				token = TokenIdentifier
			} else {
				token = TokenPlain
			}
		default:
			// Punctuation, spaces and any multi-byte character: step to
			// the next character boundary so slicing stays valid.
			ix++
			for ix < len(bytes) && !utf8.RuneStart(bytes[ix]) {
				ix++
			}
			token = TokenPlain
		}

		if n := len(out); n > 0 && out[n-1].Token == token {
			out[n-1].Range.End = ix
		} else {
			out = append(out, span{Range{start, ix}, token})
		}
	}
	return out
}

// endOfQuoted is the index just past the closing quote, treating a doubled
// quote as an escape. An unterminated quote runs to the end of the line.
func endOfQuoted(bytes string, start int, quote byte) int {
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

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || isASCIIDigit(b) || b == '_' || b == '$' || b >= 0x80
}
