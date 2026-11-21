package dbpostgres

// What a statement says besides its rows, and the statements that do not
// answer with rows at all.
//
// **Notices and notifications are caught per run.** pgconn hands both to
// a callback fixed when the connection's config is built, not to the
// statement that was reading when they arrived, so the callback files
// them under the connection and [collectCapped] watches its connection
// for the length of one statement. What arrives while nobody watches — a
// catalog read, the app's own probes — is dropped. A notification that
// arrived while a session sat idle is read off the wire with the next
// statement's reply, so it lands on the next run, which is the one the
// user is looking at.
//
// **`COPY` with the client at one end is not a query.** `COPY … FROM
// STDIN` waits for rows the client sends, and the run has none: the
// server would hold the statement open until something gave up. So it is
// refused before it is sent. `COPY … TO STDOUT` streams its rows in the
// copy protocol rather than as a result set, so a run that read result
// sets got none and said `COPY 2` over an empty pane; it is read with the
// copy protocol instead and each line becomes a row.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nick318/meerkat/internal/dbclient"
)

// watched is every connection a run is reading on, and what the server
// has said on it so far.
var watched sync.Map // *pgconn.PgConn -> *messageBox

type messageBox struct {
	mu       sync.Mutex
	messages []string
}

func (b *messageBox) add(message string) {
	b.mu.Lock()
	b.messages = append(b.messages, message)
	b.mu.Unlock()
}

// watch starts filing what the server says on `pg`; the function it
// returns stops, and hands back what was said.
func watch(pg *pgconn.PgConn) func() []string {
	box := &messageBox{}
	watched.Store(pg, box)
	return func() []string {
		watched.Delete(pg)
		box.mu.Lock()
		defer box.mu.Unlock()
		return box.messages
	}
}

// onNotice is the config's notice callback.
func onNotice(pg *pgconn.PgConn, notice *pgconn.Notice) {
	box, ok := watched.Load(pg)
	if !ok {
		return
	}
	message := fmt.Sprintf("%s: %s", notice.Severity, notice.Message)
	if notice.Detail != "" {
		message += " · " + notice.Detail
	}
	if notice.Hint != "" {
		message += " · hint: " + notice.Hint
	}
	box.(*messageBox).add(message)
}

// onNotification is the config's notification callback.
func onNotification(pg *pgconn.PgConn, notification *pgconn.Notification) {
	box, ok := watched.Load(pg)
	if !ok {
		return
	}
	message := fmt.Sprintf("NOTIFY %s from backend %d", notification.Channel, notification.PID)
	if notification.Payload != "" {
		message += ": " + notification.Payload
	}
	box.(*messageBox).add(message)
}

// copyEnd is which end of a `COPY` the client is at, if either.
type copyEnd int

const (
	copyNone copyEnd = iota
	copyIn
	copyOut
)

// copyStdio reads whether a statement is `COPY … FROM STDIN` or `COPY …
// TO STDOUT`: the first word is `copy`, and `stdin` follows `from` (or
// `stdout` follows `to`) outside brackets, strings, quoted names and
// comments. A table named `stdin` is `COPY stdin FROM …`, which this does
// not mistake for one, because the word has to follow `from`.
func copyStdio(statement string) copyEnd {
	words := topLevelWords(statement)
	if len(words) == 0 || words[0] != "copy" {
		return copyNone
	}
	for i := 1; i+1 < len(words); i++ {
		switch {
		case words[i] == "from" && words[i+1] == "stdin":
			return copyIn
		case words[i] == "to" && words[i+1] == "stdout":
			return copyOut
		}
	}
	return copyNone
}

// topLevelWords is the bare words of a statement, lower case, outside
// brackets, strings, quoted names, dollar-quoted bodies and comments.
func topLevelWords(sql string) []string {
	var words []string
	depth := 0
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '-' && strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				return words
			}
			i += end + 1
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return words
			}
			i += end + 4
		case c == '\'' || c == '"':
			i++
			for i < len(sql) {
				if sql[i] == c {
					if i+1 < len(sql) && sql[i+1] == c {
						i += 2
						continue
					}
					break
				}
				if c == '\'' && sql[i] == '\\' {
					i++
				}
				i++
			}
			i++
		case c == '$':
			end := strings.IndexByte(sql[i+1:], '$')
			if end < 0 {
				i++
				continue
			}
			tag := sql[i : i+end+2]
			close := strings.Index(sql[i+len(tag):], tag)
			if close < 0 {
				return words
			}
			i += len(tag) + close + len(tag)
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case isWordStart(c):
			start := i
			for i < len(sql) && (isWordStart(sql[i]) || (sql[i] >= '0' && sql[i] <= '9') || sql[i] == '$') {
				i++
			}
			if depth == 0 {
				words = append(words, strings.ToLower(sql[start:i]))
			}
		default:
			i++
		}
	}
	return words
}

func isWordStart(c byte) bool {
	return c == '_' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// errCopyFromStdin is the refusal for a `COPY … FROM STDIN`.
var errCopyFromStdin = errors.New(
	"COPY … FROM STDIN reads its rows from the client, and meerkat has none to send · " +
		"use INSERT, or COPY FROM a file the server can read")

// copyOutColumn names the one column a `COPY … TO STDOUT` is shown in.
const copyOutColumn = "copy"

// copyOutResult runs a `COPY … TO STDOUT` through the copy protocol and
// returns each line it wrote as a row of one column, held to the same
// budget as a result set.
func copyOutResult(ctx context.Context, pg *pgconn.PgConn, sql string, limits dbclient.Limits) (dbclient.QueryResult, error) {
	sink := dbclient.NewRowSink(limits)
	sink.Columns([]string{copyOutColumn})
	out := &copyLines{sink: sink}
	tag, err := pg.CopyTo(ctx, out, sql)
	if err != nil {
		return dbclient.QueryResult{}, wrapDB(err)
	}
	out.flush()
	result := sink.Finish(uint64(tag.RowsAffected()))
	result.Tag = tag.String()
	return result, nil
}

// copyLines turns the copy stream into rows, a line at a time. Past the
// budget it goes on reading and keeps nothing: a writer that failed would
// make pgconn close the connection, and on a session that is the user's
// whole state.
type copyLines struct {
	sink    *dbclient.RowSink
	partial bytes.Buffer
	full    bool
}

func (w *copyLines) Write(p []byte) (int, error) {
	if w.full {
		return len(p), nil
	}
	w.partial.Write(p)
	for {
		line, err := w.partial.ReadBytes('\n')
		if err != nil {
			// No newline yet: keep the start of the line for the next
			// write.
			w.partial.Reset()
			w.partial.Write(line)
			return len(p), nil
		}
		if !w.push(strings.TrimSuffix(string(line), "\n")) {
			return len(p), nil
		}
	}
}

func (w *copyLines) flush() {
	if !w.full && w.partial.Len() > 0 {
		w.push(w.partial.String())
	}
	w.partial.Reset()
}

func (w *copyLines) push(line string) bool {
	if !w.sink.Push([]dbclient.Value{dbclient.ValueText(fromUTF8Lossy([]byte(line)))}) {
		w.full = true
		w.partial.Reset()
		return false
	}
	return true
}
