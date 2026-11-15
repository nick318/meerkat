// Marking what will not parse: the server's check, debounced, and the
// relation a utility statement names (shell.rs, agent K2's part — see
// docs/tasks/shell-split.md).

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/query"
)

// scheduleCheck asks the server whether the buffer parses, once the typing
// stops.
//
// **The check is the server's, because the server is the only thing
// that knows Postgres.** A grammar of the app's own would be a second
// dialect to keep in step with the one the statement is actually sent
// to, and every gap between the two would read as the app being wrong
// about valid SQL. So the statement goes out to be *prepared* and
// thrown away: nothing runs, and the answer is the same refusal the
// run would have met.
//
// Only the **active** tab is checked. A restored strip of a hundred
// tabs sets a hundred buffers as it opens, and a check apiece would
// be a hundred round trips about text nobody is looking at.
//
// Rust: `fn schedule_check(&mut self, tab_id: u64, cx)`.
func (s *Shell) scheduleCheck(tabID uint64) {
	connection := s.connection
	active := false
	if s.active < len(s.tabs) {
		if tab, ok := s.tabs[s.active].(*QueryTab); ok && tab.id == tabID {
			active = true
		}
	}
	tab, ok := s.tabMut(tabID).(*QueryTab)
	if !ok {
		return
	}
	// The edit has already dropped the marks the last check left, so
	// there is nothing to clear here: a buffer with no connection
	// simply goes unmarked.
	tab.checked++
	checked := tab.checked
	if !active {
		return
	}
	if connection == nil {
		return
	}
	go func() {
		time.Sleep(checkDebounce)
		// Every keystroke schedules one of these, and all but the last
		// are on their way to finding the text has moved on.
		var sql string
		var session dbclient.Session
		current := false
		ok := s.call(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok {
				return
			}
			if tab.checked != checked {
				return
			}
			// **Down the tab's own session when it has one**, because
			// that is where a temp table it made and a `search_path`
			// it set actually are — the pool is a different connection
			// and would call both of them missing. A run in flight is
			// holding that connection, so the pool answers instead
			// rather than queue the check behind the statement the
			// user is waiting for.
			if !tab.run.inFlight() {
				session = tab.session
			}
			sql = tab.editor.Text()
			current = true
		})
		if !ok || !current {
			return
		}
		ranges := query.StatementRanges(sql)
		if len(ranges) > checkStatements {
			ranges = ranges[:checkStatements]
		}
		statements := make([]string, 0, len(ranges))
		for _, r := range ranges {
			statements = append(statements, sql[r.Start:r.End])
		}
		if !s.alive() {
			return
		}
		found, err := flatten(joinTask(func() ([]*dbclient.CheckError, error) {
			ctx := context.Background()
			found := make([]*dbclient.CheckError, 0, len(statements))
			for _, statement := range statements {
				var answer *dbclient.CheckError
				var err error
				if session != nil {
					answer, err = session.Check(ctx, statement)
				} else {
					answer, err = connection.Check(ctx, statement)
				}
				// A check that could not be made says nothing. It
				// is a question nobody asked for, so it must never
				// raise an error over a query that has not been
				// run.
				if err != nil {
					answer = nil
				}
				if answer == nil {
					answer = missingRelation(ctx, statement, session, connection)
				}
				found = append(found, answer)
			}
			return found, nil
		}))
		if err != nil {
			return
		}
		s.update(func() {
			vocabulary := s.vocabulary
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok {
				return
			}
			if tab.checked != checked {
				return
			}
			marks := diagnostics(sql, ranges, found, tab.editor.Cursor(), vocabulary)
			tab.editor.SetDiagnostics(marks)
		})
	}()
}

// checkActive checks the tab on screen with no edit to prompt it. A buffer
// restored before the connect landed had nowhere to send its question, and
// an unmarked query that will not parse is the state this whole path
// exists to end.
//
// Rust: `fn check_active(&mut self, cx)`.
func (s *Shell) checkActive() {
	if s.active >= len(s.tabs) {
		return
	}
	tab, ok := s.tabs[s.active].(*QueryTab)
	if !ok {
		return
	}
	id := tab.id
	s.scheduleCheck(id)
}

// missingRelation is the relation a `DROP`, `ALTER` or `TRUNCATE` names,
// when the server turns out not to have one.
//
// **This is the hole [`Connection::check`] cannot fill on its own.**
// Postgres resolves no names for a utility statement until it runs one,
// so `DROP TABLE nosuch` prepares happily and the check comes back with
// nothing — on exactly the statements where being told beforehand is
// worth most. So the app reads the name out of the statement and the
// **server** is still the one that answers, through `to_regclass`: it
// resolves the name the parser's own way, and on a session it sees that
// tab's temp tables and `search_path`.
//
// It costs one round trip, and only for a statement that names a
// relation and does not say `IF EXISTS` — which says outright that a
// missing one is the point.
//
// Go port: Rust's `async fn`; session is nil for `None`. It blocks, so
// it is called on the check's goroutine.
func missingRelation(
	ctx context.Context,
	statement string,
	session dbclient.Session,
	connection dbclient.Connection,
) *dbclient.CheckError {
	target := query.RelationTargetOf(statement)
	if target == nil {
		return nil
	}
	if target.IfExists {
		return nil
	}
	name := statement[target.Name.Start:target.Name.End]
	var exists, known bool
	var err error
	if session != nil {
		exists, known, err = session.RelationExists(ctx, name)
	} else {
		exists, known, err = connection.RelationExists(ctx, name)
	}
	// "Cannot say" is not "it is not there", and neither is a failed ask.
	if err != nil || !known || exists {
		return nil
	}
	offset := target.Name.Start
	return &dbclient.CheckError{
		Refusal: dbclient.RefusalName,
		// The server's own words for the same thing, so the strip reads
		// the same whichever way the answer was reached.
		Message: fmt.Sprintf("relation \"%s\" does not exist", name),
		Offset:  &offset,
	}
}
