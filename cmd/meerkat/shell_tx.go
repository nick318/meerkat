// Transactions: who ends one, and what the tab knows about the one it is
// in (shell.rs, agent K2's part — see docs/tasks/shell-split.md).

package main

import (
	"context"
	"time"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/storage"
)

// refreshTransaction asks what the tab's session is now in, after a run
// has changed it.
//
// It runs **after** the result is on screen and never waits on the
// session's connection, so it costs the run nothing. The answer is worth
// having early because the moment it is wanted — a close — cannot
// wait for a round trip to decide what to ask the user.
//
// A session that cannot answer is left as it was rather than reported
// as clean: the safe reading of a missing answer is the careful one.
//
// Rust: `fn refresh_transaction(&mut self, tab_id: u64, cx)`.
func (s *Shell) refreshTransaction(tabID uint64) {
	tab, ok := s.tabMut(tabID).(*QueryTab)
	if !ok {
		return
	}
	session := tab.session
	if session == nil {
		return
	}
	// The answer is about the session as this run left it. A run started
	// since may have opened or ended a transaction of its own, and an
	// answer read while it was out says nothing about where it left
	// things — so a reply from before the latest run is dropped, and that
	// run asks again when it lands. (The Rust applied every reply.)
	generation := tab.generation
	go func() {
		if !s.alive() {
			return
		}
		open, err := flatten(joinTask(func() (bool, error) {
			return session.InTransaction(context.Background())
		}))
		if err != nil {
			return
		}
		s.update(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok {
				return
			}
			if tab.generation != generation || tab.session != session {
				return
			}
			if tab.inTransaction != open {
				tab.inTransaction = open
				// Nothing is open, so there is nothing for the count to
				// be a count of. It is reset here as well as on the
				// buttons, because the server is what has the last word:
				// a statement the app did not recognise may have ended
				// the transaction, and a stale count would outlive it.
				if !open {
					tab.txTally = TxTally{}
				}
			}
		})
	}()
}

// --- transactions -----------------------------------------------------

// commitRolledBack is what the tab says when a `COMMIT` comes back as a
// rollback. Postgres does that, with no error, to a transaction one of
// whose statements had failed: everything in it is gone.
const commitRolledBack = "the transaction had failed, so the server rolled it back instead of committing: nothing was saved"

// rolledBack reads a boundary's command tag: true when the server
// answered with `ROLLBACK`, whatever was sent.
func rolledBack(tag string) bool {
	return tag == "ROLLBACK"
}

// setTxMode switches which way this tab commits.
//
// **Auto is refused while a transaction is open.** The switch would
// otherwise leave the transaction standing with nothing on screen
// offering to end it, and the statements after it would land inside a
// transaction the tab says it is not in. Commit or roll back first;
// the bar is right there.
//
// Rust: `fn set_tx_mode(&mut self, mode: TxMode, cx)`.
func (s *Shell) setTxMode(mode dbclient.TxMode) {
	if s.active >= len(s.tabs) {
		return
	}
	tab, ok := s.tabs[s.active].(*QueryTab)
	if !ok {
		return
	}
	if tab.txMode == mode {
		return
	}
	if mode == dbclient.TxModeAuto && tab.inTransaction {
		return
	}
	tab.txMode = mode
	// The mode is not news about the last transaction, and the bar's
	// "committed" line is: switching clears it rather than leaving an
	// answer to an older question under a control that just moved.
	tab.txDone = nil
	// A tab remembers its mode across sessions, so a switch is worth
	// writing back at once.
	s.rememberTabs()
}

// endTransaction commits or rolls back the transaction the active tab is
// in.
//
// It goes down the tab's **own** session, because that is where the
// transaction is: a `COMMIT` on a pooled connection would commit
// nothing and report success. So it waits for the connection the same
// way a run does — and a run in flight is holding it, which is why a
// boundary is refused rather than queued behind one. The user has ⌘.
// for that, and the bar says so.
//
// Rust: `fn end_transaction(&mut self, how: TxEnd, cx)`.
func (s *Shell) endTransaction(how dbclient.TxEnd) {
	if s.active >= len(s.tabs) {
		return
	}
	tab, ok := s.tabs[s.active].(*QueryTab)
	if !ok {
		return
	}
	if !tab.inTransaction || tab.txEnding {
		return
	}
	if tab.run.inFlight() {
		message := runHoldsTheSession
		tab.error = &message
		return
	}
	session := tab.session
	if session == nil {
		// No session and yet in a transaction is a state that cannot
		// happen: the flag is only ever set from a session's answer.
		return
	}
	tabID, statement := tab.id, how.SQL()
	tab.txEnding = true
	tab.error = nil
	tab.lastUsed = time.Now()

	started := time.Now()
	go func() {
		if !s.alive() {
			return
		}
		// What the server actually did, which is not always what was
		// asked: the `COMMIT` of a failed transaction rolls it back.
		did, err := flatten(joinTask(func() (dbclient.TxEnd, error) {
			return session.EndTransaction(context.Background(), how)
		}))
		s.update(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok {
				return
			}
			tab.txEnding = false
			tab.lastUsed = time.Now()
			committedShape := false
			if err == nil {
				tab.inTransaction = false
				tab.txTally = TxTally{}
				done := did
				tab.txDone = &done
				if how == dbclient.TxEndCommit && did == dbclient.TxEndRollback {
					// Said out loud: the bar alone would read as the
					// user's own rollback, and the work they asked to
					// keep is gone.
					message := commitRolledBack
					tab.error = &message
				}
				// A shape change the transaction held is decided
				// with it: permanent, and the sidebar's to show, or
				// never happened.
				switch did {
				case dbclient.TxEndCommit:
					if report := tab.schemaReport; report != nil && report.held {
						report.held = false
						committedShape = true
					}
				case dbclient.TxEndRollback:
					tab.schemaReport = nil
				}
			} else {
				// The transaction is still whatever the server says it
				// is, so the bar stays up and `refresh_transaction`
				// below is what corrects it.
				message := err.Error()
				tab.error = &message
				// A boundary that failed because the connection is gone
				// leaves nothing to end: the transaction died with the
				// backend. The session goes, as a run's does.
				if tab.session != nil && tab.session.IsClosed() {
					tab.inTransaction = false
					tab.txTally = TxTally{}
					s.endSession(tabID)
				}
			}
			// A boundary is a statement the user ran on this
			// connection, so the history keeps it: "why did my work
			// disappear" is answered by a `ROLLBACK` in the list.
			elapsed := uint64(time.Since(started).Milliseconds())
			var rows *uint64
			var failed *string
			if err == nil {
				zero := uint64(0)
				rows = &zero
			} else {
				message := err.Error()
				failed = &message
			}
			s.recordRun(
				statement,
				storage.RunSourceUser,
				&Outcome{
					elapsed: &elapsed,
					rows:    rows,
					// A boundary changes no rows of its own. What it did
					// to the rows before it is not a number the server
					// reports, and inventing one here would say a commit
					// wrote nothing.
					affected: nil,
					error:    failed,
				},
			)
			s.reloadOpenHistory()
			s.refreshTransaction(tabID)
			if committedShape {
				s.introspect()
			}
		})
	}()
}
