package query

// Reading which names a statement mentions, for the schema report.
//
// **The report compares the catalog after a run with the one the shell
// already had, and that one can be hours old.** On a shared database a
// migration, a colleague or another app changes the shape in between, and
// a diff of the two catalogs lays every one of those changes at the door
// of the statement the user just ran — a `CREATE INDEX` reported as
// dropping a view in another schema. Reading the catalog again right
// before the run would cost one more full introspection per DDL. So the
// report keeps only what the run's statements could have touched, and
// this is how it tells: the names the statements mention.
//
// It reads tokens, not grammar, in [DdlVerbOf]'s spirit. A name anywhere
// in the statement counts — a key's target, a check's column, a word in a
// comment — so it errs toward keeping a change rather than hiding one.

// Names is every name a set of statements mentions.
type Names struct {
	// Every part of every unqualified name, as the server would read
	// it: folded to lower case unless it was quoted.
	bare map[string]struct{}
	// Every `schema.name`, by its last two parts.
	qualified map[[2]string]struct{}
	// A schema a `CREATE`, `ALTER` or `DROP SCHEMA` names, where
	// everything in it is the statement's doing — a `DROP SCHEMA …
	// CASCADE` takes every table with it and names none of them.
	schemas map[string]struct{}
}

// NamesIn reads the names `statements` mention.
func NamesIn(statements []string) Names {
	names := Names{
		bare:      map[string]struct{}{},
		qualified: map[[2]string]struct{}{},
		schemas:   map[string]struct{}{},
	}
	for _, statement := range statements {
		names.read(statement)
	}
	return names
}

func (n *Names) read(statement string) {
	body := skipLeadingComments(statement)
	next := wordRanges(body)
	var words []string
	for {
		rng, ok := next()
		if !ok {
			break
		}
		token := body[rng.Start:rng.End]
		words = append(words, token)
		parts := nameParts(token)
		switch len(parts) {
		case 0:
		case 1:
			n.bare[parts[0]] = struct{}{}
		default:
			last := len(parts) - 1
			n.qualified[[2]string{parts[last-1], parts[last]}] = struct{}{}
		}
	}
	if schema, ok := schemaTarget(words); ok {
		n.schemas[schema] = struct{}{}
	}
}

// Mentions says whether a statement named `name` in `schema`, bare or
// qualified, or named the schema itself in a schema statement.
func (n Names) Mentions(schema, name string) bool {
	if _, ok := n.schemas[schema]; ok {
		return true
	}
	if _, ok := n.bare[name]; ok {
		return true
	}
	_, ok := n.qualified[[2]string{schema, name}]
	return ok
}

// The schema `CREATE SCHEMA`, `ALTER SCHEMA` or `DROP SCHEMA` names, past
// `IF [NOT] EXISTS`.
func schemaTarget(words []string) (string, bool) {
	if len(words) < 3 {
		return "", false
	}
	switch asciiLower(words[0]) {
	case "create", "alter", "drop":
	default:
		return "", false
	}
	if !eqIgnoreASCIICase(words[1], "schema") {
		return "", false
	}
	rest := words[2:]
	for len(rest) > 0 {
		switch asciiLower(rest[0]) {
		case "if", "not", "exists":
			rest = rest[1:]
			continue
		}
		break
	}
	if len(rest) == 0 {
		return "", false
	}
	parts := nameParts(rest[0])
	if len(parts) != 1 {
		return "", false
	}
	return parts[0], true
}

// nameParts splits a token into the parts of the name it is, as the
// server reads them, or nil when the token is not a name: a string, a
// number, a parameter, punctuation.
func nameParts(token string) []string {
	var parts []string
	ix := 0
	for ix < len(token) {
		end := oneTokenEnd(token, ix)
		part := token[ix:end]
		switch lead := part[0]; {
		case lead == '"':
			inner := part[1:]
			if len(inner) > 0 && inner[len(inner)-1] == '"' {
				inner = inner[:len(inner)-1]
			}
			parts = append(parts, unescapeQuotes(inner))
		case isASCIIAlpha(lead) || lead == '_' || lead >= 0x80:
			parts = append(parts, asciiLower(part))
		default:
			return nil
		}
		ix = end
		if ix < len(token) {
			if token[ix] != '.' {
				return nil
			}
			ix++
		}
	}
	return parts
}

// A doubled quote inside a quoted name is one quote.
func unescapeQuotes(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, s[i])
		if s[i] == '"' && i+1 < len(s) && s[i+1] == '"' {
			i++
		}
	}
	return string(out)
}
