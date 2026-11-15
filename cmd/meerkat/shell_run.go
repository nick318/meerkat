// Sessions, runs, the connect and the catalog: the shell's half that talks
// to the server (shell.rs, agent K2's part — see docs/tasks/shell-split.md).
//
// Every reply comes back the way shell_types.go's shape decisions say: a
// goroutine does the server's work (the tokio task), and `s.update`
// hands the answer to the main thread, where the generation is checked.
// `run_statements` was a GPUI task that awaited one tokio task after
// another and updated the shell between them; in Go it is a goroutine
// that does the same through `s.call`, which waits for the main thread
// the way `this.update(cx, …)` was awaited.

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/dbpostgres"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/query"
	"github.com/nick318/meerkat/internal/resultsgrid"
	"github.com/nick318/meerkat/internal/sqleditor"
	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/uikit"
)

// call runs fn on the main thread and waits until it has: a
// `this.update(cx, …)` awaited inside a GPUI task. It answers false when
// the shell has been let go — or there is no window, in a test — which is
// the `Err` that update answered with once the entity was dropped.
//
// Go port: never call it on the main thread, which would wait for itself.
func (s *Shell) call(fn func()) bool {
	done := make(chan bool, 1)
	s.updateOr(func() {
		fn()
		done <- true
	}, func() { done <- false })
	return <-done
}

// alive is the check a GPUI task made before it spawned a tokio task:
// `this.update(cx, |_, cx| Tokio::spawn(..))`, which failed once the
// shell was gone, and with it the request was never sent.
func (s *Shell) alive() bool {
	return s.call(func() {})
}

// dropConnection closes a connection nothing will use, off the main
// thread: what dropping the last `Arc<dyn Connection>` did in the Rust.
func dropConnection(connection dbclient.Connection) {
	go func() {
		ctx := context.Background()
		switch connection := connection.(type) {
		case interface{ Close(context.Context) }:
			connection.Close(ctx)
		case interface{ Close() error }:
			_ = connection.Close()
		}
	}()
}

// dropSession closes a session nothing will use, off the main thread:
// what `impl Drop` on a session did when its last handle went. Closing a
// session twice closes it once.
func dropSession(session dbclient.Session) {
	if session == nil {
		return
	}
	go session.Close(context.Background())
}

// optional turns a Go `(T, bool)` into the `Option<T>` the shell's
// fields and the policy functions take.
func optional[T any](value T, ok bool) *T {
	if !ok {
		return nil
	}
	return &value
}

// Take a catalog as the session's own. The sidebar rows, the completion
// vocabulary and the relation count all derive from it, and every open
// query tab is handed the new words — a tab built before the catalog
// landed carries an empty vocabulary otherwise.
//
// Rust: `fn apply_catalog(&mut self, catalog: Catalog, cx)`.
func (s *Shell) applyCatalog(catalog introspect.Catalog) {
	held := &catalog
	s.vocabulary = vocabularyOf(held)
	s.catalogGroups = catalogGroups(held)
	// A group the user opened keeps its name across an introspection,
	// so the sidebar does not close under them when the real catalog
	// replaces the cached one.
	s.rebuildCatalogRows()
	total := 0
	for _, schema := range held.Schemas {
		total += len(schema.Tables)
	}
	s.relationTotal = total
	s.catalog = held
	vocabulary := s.vocabulary
	for _, tab := range s.tabs {
		if tab, ok := tab.(*QueryTab); ok {
			tab.editor.SetVocabulary(vocabulary)
		}
	}
}

// connect opens the pool, and nothing more. Reading the catalog of a large
// database takes far longer than the connect itself, so it is a
// second request: the session says "connected" and runs queries as
// soon as there is a pool, and the sidebar fills in behind it.
//
// The first attempt and every retry come through here. Each bumps
// `connect_generation`, so a reply to an attempt the user has since
// asked again about is dropped rather than painted over the newer
// one.
//
// Rust: `fn connect(&mut self, cx)`.
func (s *Shell) connect() {
	s.connectGeneration++
	generation := s.connectGeneration
	target := s.target
	s.status = StatusConnecting{To: endpoint(target)}
	s.connectStarted = time.Now()
	// Nothing repaints a window that is only waiting, so the strip
	// asks for the frame it arms on.
	time.AfterFunc(connectArm, func() { s.update(func() {}) })
	type connected struct {
		connection dbclient.Connection
		label      *dbpostgres.Label
	}
	go func() {
		value, failed := connectOutcome(joinTask(func() (connected, error) {
			ctx := context.Background()
			var connection *dbpostgres.PostgresConnection
			var err error
			switch target := target.(type) {
			case TargetUrl:
				connection, err = dbpostgres.Connect(ctx, target.Url)
			// A saved connection takes its password from the OS
			// keychain, so nothing here carries one.
			case TargetProfile:
				connection, err = dbpostgres.ConnectProfile(ctx, &target.Profile)
			default:
				err = errors.New("nothing to connect to")
			}
			if err != nil {
				return connected{}, err
			}
			return connected{connection: connection, label: connection.Label()}, nil
		}))
		dropped := func() {
			if value.connection != nil {
				dropConnection(value.connection)
			}
		}
		s.updateOr(func() {
			if s.connectGeneration != generation {
				dropped()
				return
			}
			if failed == nil {
				// A run asked for while the connect was out was
				// told to wait. The wait is over; take the line
				// down rather than leave it over a live session.
				waiting := notConnectedCopy(s.status)
				for _, tab := range s.tabs {
					if tab, ok := tab.(*QueryTab); ok && tab.error != nil && *tab.error == waiting {
						tab.error = nil
					}
				}
				s.connection = value.connection
				s.label = value.label
				s.status = StatusConnected{}
				s.resumePendingPages()
				// The tab on screen was typed into before there
				// was anywhere to send the question. Ask now.
				s.checkActive()
				s.introspect()
			} else {
				s.status = StatusFailed{Error: *failed}
				reason := notConnectedCopy(s.status)
				// Tabs opened from the cached catalog were waiting
				// for this connection. Nothing is coming.
				for _, tab := range s.tabs {
					if tab, ok := tab.(*TableTab); ok && tab.loading {
						tab.loading = false
						message := reason
						tab.error = &message
					}
				}
			}
		}, dropped)
	}()
}

// retryConnect asks a failed connect again, in place. The strip's
// **Retry**.
//
// Only a failed connect is retried: one still out has its answer on
// the way, and a second beside it would open a second pool. A session
// that has failed never had a connection, so every error a tab holds
// is the "not connected" line — they are taken down, and a table tab
// goes back to waiting, which is the state `resume_pending_pages`
// picks up when the connection lands.
//
// Rust: `fn retry_connect(&mut self, cx)`.
func (s *Shell) retryConnect() {
	if _, ok := s.status.(StatusFailed); !ok {
		return
	}
	for _, tab := range s.tabs {
		switch tab := tab.(type) {
		case *TableTab:
			if tab.error != nil {
				tab.error = nil
				tab.loading = true
			}
		case *QueryTab:
			tab.error = nil
		}
	}
	s.copiedError = nil
	s.connect()
}

// introspect reads the catalog behind the connection. A session that
// opened on a cached catalog keeps painting it until this lands, and keeps
// it if this fails — a stale sidebar beats no sidebar.
//
// Rust: `fn introspect(&mut self, cx)`.
func (s *Shell) introspect() {
	connection := s.connection
	if connection == nil {
		return
	}
	s.catalogLoading = true
	s.catalogError = nil
	go func() {
		catalog, err := flatten(joinTask(func() (introspect.Catalog, error) {
			return connection.Introspect(context.Background())
		}))
		s.update(func() {
			s.catalogLoading = false
			if err == nil {
				// Keep this shape for the next session. A cache
				// that cannot be written costs nothing here.
				if s.store != nil {
					_ = s.store.CacheCatalog(s.scope, &catalog, unixNow())
				}
				s.applyCatalog(catalog)
			} else {
				message := err.Error()
				s.catalogError = &message
			}
		})
	}()
}

// resumePendingPages: a table opened from the cached catalog before the
// connection landed is still waiting for its first page — `load_page`
// left it marked loading and sent nothing. Ask for those pages now.
//
// Rust: `fn resume_pending_pages(&mut self, cx)`.
func (s *Shell) resumePendingPages() {
	type pendingPage struct {
		id   uint64
		page int
	}
	var pending []pendingPage
	for _, tab := range s.tabs {
		if tab, ok := tab.(*TableTab); ok && tab.loading {
			pending = append(pending, pendingPage{id: tab.id, page: tab.page})
		}
	}
	for _, p := range pending {
		s.loadPage(p.id, p.page)
	}
}

// loadPage asks for one page of a table tab.
//
// Rust: `fn load_page(&mut self, tab_id: u64, page: usize, cx)`.
func (s *Shell) loadPage(tabID uint64, page int) {
	connection := s.connection
	_, failed := s.status.(StatusFailed)
	reason := notConnectedCopy(s.status)
	tab, ok := s.tabMut(tabID).(*TableTab)
	if !ok {
		return
	}
	// The cached catalog can open a table before the connection is
	// there. Leave the tab loading; `resume_pending_pages` asks again
	// as soon as the connection lands. Once the connect has failed
	// there is nothing left to wait for, so say so instead.
	if connection == nil {
		tab.page = page
		tab.selection.Clear()
		tab.loading = !failed
		if failed {
			tab.error = &reason
		} else {
			tab.error = nil
		}
		s.rememberTabs()
		return
	}

	schema, table := tab.schema, tab.table
	model := s.tableModel(schema, table)
	if model == nil {
		return
	}
	sql := PageQuery(schema, model, page)

	tab, ok = s.tabMut(tabID).(*TableTab)
	if !ok {
		return
	}
	tab.page = page
	tab.loading = true
	tab.error = nil
	// A page is a different set of rows, and a selection points at rows
	// by index: what was marked cannot be carried over to them.
	tab.selection.Clear()
	tab.generation++
	generation := tab.generation

	// The page a table tab sits on is part of what the next session
	// reopens, so it is written back as it turns.
	s.rememberTabs()

	recorded := sql
	go func() {
		ran, err := flatten(joinTask(func() (sqlRun, error) { return runSQL(connection, sql) }))
		s.update(func() {
			tab, ok := s.tabMut(tabID).(*TableTab)
			if !ok {
				return
			}
			if tab.generation != generation {
				return
			}
			tab.loading = false
			var run Outcome
			if err == nil {
				rows := uint64(len(ran.result.Rows))
				elapsed := ran.elapsed
				// A table page runs on the *pool*, so there is no
				// session to ask what the server spent: the backend
				// that ran it has gone back to the pool and may be
				// running somebody else's statement by now. The
				// client's own split is all this path reports, and
				// it is enough — a page is the app's own
				// `LIMIT 500`, not a statement anyone is tuning.
				timing := newTiming(elapsed, ran.result.Wire)
				tab.timing = &timing
				tab.data = resultsgrid.NewGridData(ran.result.Columns, ran.result.Rows)
				// A page is the app's own `SELECT`: it returns rows
				// and changes none, so there is no count to keep.
				run = Outcome{
					elapsed:  &elapsed,
					rows:     &rows,
					affected: nil,
					error:    nil,
				}
			} else {
				message := err.Error()
				tab.error = &message
				tab.data = emptyGrid()
				run = Outcome{
					elapsed:  nil,
					rows:     nil,
					affected: nil,
					error:    &message,
				}
			}
			s.recordRun(recorded, storage.RunSourceApp, &run)
			s.reloadOpenHistory()
		})
	}()
}

// markStatement says in the gutter how far one statement of a tab's run
// got.
//
// A reply from a run the tab has moved on from writes nothing, for
// the reason every other reply checks the generation: the marks
// belong to the run on screen. A buffer edited mid-run has dropped
// its marks already, and the editor answers that by writing nothing.
//
// Rust: `fn mark_statement(&mut self, tab_id, generation, ix, status,
// meta, cx)`.
func (s *Shell) markStatement(tabID, generation uint64, ix int, status sqleditor.StatementStatus, meta string) {
	tab, ok := s.tabMut(tabID).(*QueryTab)
	if !ok {
		return
	}
	if tab.generation != generation {
		return
	}
	tab.editor.SetStatementStatus(ix, status, meta)
}

// skipStatements marks every statement from `from` on as never sent, with
// `meta` saying why beside each.
//
// Rust: `fn skip_statements(&mut self, tab_id, generation, from, meta,
// cx)`.
func (s *Shell) skipStatements(tabID, generation uint64, from int, meta string) {
	tab, ok := s.tabMut(tabID).(*QueryTab)
	if !ok {
		return
	}
	if tab.generation != generation {
		return
	}
	tab.editor.SkipStatementsFrom(from, meta)
}

// runActiveQuery runs the active query tab: ⌘⏎.
//
// Rust: `fn run_active_query(&mut self, cx)`.
func (s *Shell) runActiveQuery() {
	connection := s.connection
	reason := notConnectedCopy(s.status)
	if s.active >= len(s.tabs) {
		return
	}
	tab, ok := s.tabs[s.active].(*QueryTab)
	if !ok {
		return
	}
	// ⌘⏎ while the last run is still out does nothing: the key that
	// starts a run is not the key that stops one, and a second run
	// over the top of the first would leave the first unstoppable.
	// A commit on its way out holds the same connection, so it counts
	// as a run in flight here.
	if tab.run.inFlight() || tab.txEnding {
		return
	}
	// The tab opens before the connection does, so ⌘⏎ can arrive
	// first. Say so rather than doing nothing — and say why: "not
	// connected" under a connect that failed is the one line that
	// leaves the user no better off than silence.
	if connection == nil {
		tab.error = &reason
		return
	}

	// The selection when there is one, the whole buffer otherwise —
	// and where it starts, so the gutter can mark the statements on
	// the lines they were typed on.
	sql, offset := tab.editor.RunSource()
	// A driver runs one command at a time, so send the statements in
	// turn rather than handing the server a whole scratchpad.
	ranges := query.StatementRanges(sql)
	statements := make([]string, 0, len(ranges))
	for _, r := range ranges {
		statements = append(statements, sql[r.Start:r.End])
	}
	if len(statements) == 0 {
		return
	}
	tabID := tab.id
	session := tab.session
	// **Manual mode holds one transaction across the runs of a tab**,
	// so a run opens one when none is open — and never when the buffer
	// opens its own, because a second `BEGIN` is a warning from the
	// server and a lie in the bar.
	begin := needsBegin(
		tab.txMode,
		tab.inTransaction,
		optional(query.TransactionVerb(statements[0])),
	)
	// What the buffer itself does to the transaction, which the bar has
	// to report whichever mode the tab is in: a `COMMIT` typed by hand
	// is the same event as the button. The **last** verb wins, because
	// `BEGIN; …; COMMIT` in one buffer leaves nothing open.
	var bufferVerb *query.TxVerb
	for i := len(statements) - 1; i >= 0; i-- {
		if verb, ok := query.TransactionVerb(statements[i]); ok {
			bufferVerb = &verb
			break
		}
	}
	// What the bar counts as having landed *inside* the transaction:
	// the statements that are not boundaries. A bare `BEGIN` therefore
	// opens a transaction with nothing in it, which is what it did.
	plainStatements := 0
	for _, statement := range statements {
		if _, ok := query.TransactionVerb(statement); !ok {
			plainStatements++
		}
	}
	// A tab about to open its first session may be the one too many.
	// Give back the session that has gone longest without a question;
	// with nothing spare to give, say so here rather than let the pool
	// wait out its connect timeout and answer with something about
	// connections.
	if session == nil && !s.makeRoomForSession() {
		tab, ok := s.tabs[s.active].(*QueryTab)
		if !ok {
			return
		}
		message := noSessionLeft
		tab.error = &message
		return
	}
	tab, ok = s.tabs[s.active].(*QueryTab)
	if !ok {
		return
	}
	// The backend is the *session's*, so it is known before the run
	// leaves — there is no id to wait for any more. A tab running for
	// the first time has no session yet, and `None` says so: the run
	// is alive, and nothing is out on the server to stop.
	var backend *dbclient.RunId
	if session != nil {
		backend = optional(session.Backend())
	}
	tab.run = Run{kind: RunRunning, flight: Live{
		started: time.Now(),
		backend: backend,
	}}
	tab.error = nil
	// "committed" was the answer to the last question. This is a new
	// one, so the bar stops saying it: news about a transaction that
	// has been over since before this run is not news.
	tab.txDone = nil
	tab.lastUsed = time.Now()
	tab.sessionEnded = false
	tab.generation++
	generation := tab.generation

	// A report about a change that has committed is about the last
	// run, and this is a new one. One still held in a transaction
	// stays: the change is still pending, and the bar's answer to it
	// is still the answer.
	if !(tab.schemaReport != nil && tab.schemaReport.held) {
		tab.schemaReport = nil
	}
	// Every statement queued, none of them sent. The gutter says so
	// from the moment ⌘⏎ lands, so a slow first statement is a run the
	// user can already see the shape of.
	marks := make([]sqleditor.Range, len(ranges))
	for i, r := range ranges {
		marks[i] = sqleditor.Range(r)
	}
	tab.editor.SetStatements(marks, offset)

	recorded := sql
	// The buffer has been edited since the tab was opened, and this is
	// the moment the user says it is worth something.
	s.rememberTabs()
	s.startTimer()
	go func() {
		outcome := s.runStatements(connection, session, begin, statements, tabID, generation)
		s.updateOr(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok || tab.generation != generation {
				// Nobody will read this. A session the tab does not
				// hold goes with the reply, as it did in the Rust.
				if !ok || tab.session != outcome.session {
					dropSession(outcome.session)
				}
				return
			}
			// A run the user stopped comes back as the server's own
			// refusal. The tab says CANCELLED rather than painting that
			// as a failure, because the user is the one who asked —
			// but the history still keeps what the server said.
			stopped := tab.run.kind == RunCancelling
			var waited *uint64
			if live := tab.run.live(); live != nil {
				ms := uint64(time.Since(live.started).Milliseconds())
				waited = &ms
			}
			session := outcome.session
			began := outcome.began
			shape := outcome.shape
			// A shape-changing statement landed, on either path. The
			// report opens now and fills in when the catalog has been
			// read back; the sidebar is stale until then either way.
			shaped := len(shape.ddl) != 0
			if shaped {
				tab.schemaReport = &SchemaReport{
					delta:      introspect.Delta{},
					statements: shape.ddl,
					ms:         shape.ms,
					held:       false,
					pending:    true,
					error:      nil,
				}
			}
			// The `BEGIN` went out, so a transaction is open whether or
			// not the statements after it worked: a statement that fails
			// inside a transaction leaves it open and aborted, which is
			// the state most worth having a bar for.
			if began {
				tab.inTransaction = true
			}
			// Kept whether the run worked or not. A failed statement
			// does not end a transaction, and throwing the session away
			// here would lose whatever the tab had set — and hand the
			// pool a connection with an open transaction on it.
			if session != nil {
				tab.session = session
			}
			// The sweep counts from the end of a run, not the start:
			// a statement that took nine minutes has not left its
			// session idle for nine minutes.
			tab.lastUsed = time.Now()
			// The server counts one statement at a time, and names only
			// the one a backend ran last. A buffer of several is
			// therefore unanswerable — reporting the last statement's
			// time as the run's would be a wrong number, not a partial
			// one — so the ask goes out only for a buffer of one.
			oneStatement := outcome.result != nil && outcome.result.ran == 1
			committedShape := false
			var run Outcome
			if ran := outcome.result; ran != nil {
				result := ran.result
				affected := ran.affected
				counted := ran.counted
				verb := ran.verb
				elapsedMs := ran.elapsedMs
				tab.run = Run{}
				// The run button settles on this, so it counts a
				// result rather than a reply: a failure is not
				// something to congratulate the user on.
				tab.landed++
				rows := uint64(len(result.Rows))
				timing := newTiming(elapsedMs, result.Wire)
				tab.timing = &timing
				// Columns, not success: a statement that changed
				// rows succeeded and has no result set to show.
				tab.hasResult = len(result.Columns) != 0
				// A count of zero is still a count, so this turns on
				// the *number of counted statements* rather than on
				// the number itself.
				if counted > 0 {
					tab.changed = &Changed{rows: affected, verb: verb}
				} else {
					tab.changed = nil
				}
				tab.truncated = result.Truncated
				tab.statementsRun = ran.ran
				tab.data = resultsgrid.NewGridData(result.Columns, result.Rows)
				// A run answers with a different set of rows, and a
				// selection points at rows by index. Marks from the
				// last answer can only point at these ones wrongly.
				tab.selection.Clear()
				// What the buffer did to the transaction itself. The
				// server's own answer follows in `refresh_transaction`
				// a moment later; this is what the bar says at once,
				// so a commit the user typed reads as a commit.
				if bufferVerb != nil {
					switch *bufferVerb {
					case query.TxVerbBegin:
						tab.inTransaction = true
					case query.TxVerbCommit:
						tab.inTransaction = false
						done := dbclient.TxEndCommit
						tab.txDone = &done
						// A shape change the transaction was
						// holding is permanent now, and the
						// sidebar may have it.
						if report := tab.schemaReport; report != nil && report.held {
							report.held = false
							committedShape = true
						}
					case query.TxVerbRollback:
						tab.inTransaction = false
						done := dbclient.TxEndRollback
						tab.txDone = &done
						// The change the report described never
						// happened.
						tab.schemaReport = nil
					}
				}
				if tab.inTransaction {
					tab.txTally.add(plainStatements, affected)
				} else {
					tab.txTally = TxTally{}
				}
				var affectedRun *uint64
				if counted > 0 {
					affectedRun = &affected
				}
				run = Outcome{
					elapsed: &elapsedMs,
					rows:    &rows,
					// The history's own column, kept apart from the
					// row count for the same reason the result line
					// keeps them apart: rows returned and rows
					// changed are different answers.
					affected: affectedRun,
					error:    nil,
				}
			} else {
				message := outcome.err
				if stopped {
					elapsed := uint64(0)
					if waited != nil {
						elapsed = *waited
					}
					tab.run = Run{kind: RunCancelled, elapsed: elapsed}
				} else {
					tab.run = Run{}
					tab.error = &message
				}
				tab.hasResult = false
				// Whatever the last run changed is not what this one
				// did, and this one says nothing about rows at all.
				tab.changed = nil
				tab.truncated = false
				tab.data = emptyGrid()
				tab.selection.Clear()
				run = Outcome{
					elapsed:  nil,
					rows:     nil,
					affected: nil,
					error:    &message,
				}
			}
			s.recordRun(recorded, storage.RunSourceUser, &run)
			s.reloadOpenHistory()
			s.refreshTransaction(tabID)
			if oneStatement {
				s.refreshServerTiming(tabID, generation)
			}
			if shaped {
				s.reportSchema(tabID, generation)
			} else if committedShape {
				// Typed `COMMIT` over a held change: the pool can see
				// it now, and the sidebar reads it from there.
				s.introspect()
			}
			// There is a session now, so there is something to sweep.
			s.startSessionTimer()
		}, func() { dropSession(outcome.session) })
	}()
}

// stopActiveQuery is ⌘. and the run button while a statement is out. The
// first press asks the server to give the statement up; the second closes
// the backend it is on. Nothing here touches the goroutine running the
// statements: it is waiting on the server, and the server is what has to
// let go.
//
// Rust: `fn stop_active_query(&mut self, cx)`.
func (s *Shell) stopActiveQuery() {
	connection := s.connection
	if s.active >= len(s.tabs) {
		return
	}
	tab, ok := s.tabs[s.active].(*QueryTab)
	if !ok {
		return
	}
	var how dbclient.Stop
	switch tab.run.kind {
	case RunRunning:
		how = dbclient.StopCancel
	case RunCancelling:
		how = dbclient.StopTerminate
	// Nothing is out, so there is nothing to stop. ⌘. on an idle
	// tab does nothing rather than something surprising.
	default: // RunIdle, RunCancelled
		return
	}
	if connection == nil {
		return
	}
	// `Live` is the run, not the request, so both presses keep the same
	// start time and the same backend: the timer must not restart
	// because the user asked twice. A terminate therefore leaves the
	// state where the cancel put it, and the server's reply is still
	// what ends the run.
	live := tab.run.flight
	tabID, generation := tab.id, tab.generation
	tab.run = Run{kind: RunCancelling, flight: live}

	// No backend means the tab's session is still opening and nothing
	// has been sent. Marking the run `Cancelling` **is** the stop: the
	// open lands into a run that then does not start. There is no id
	// to wait for, because there is nothing to wait for it to reach.
	if live.backend == nil {
		return
	}
	id := *live.backend

	go func() {
		if !s.alive() {
			return
		}
		// `Ok(false)` means the statement had already finished, which
		// the reply will report on its own. Only a failure to *ask*
		// is worth a word.
		_, err := flatten(joinTask(func() (bool, error) {
			return connection.Stop(context.Background(), id, how)
		}))
		if err != nil {
			s.update(func() {
				// The tab this stop was for, not whichever tab is in
				// front when the answer lands, and only while it is
				// still the same run.
				tab, ok := s.tabMut(tabID).(*QueryTab)
				if !ok {
					return
				}
				if tab.generation == generation {
					message := err.Error()
					tab.error = &message
				}
			})
		}
	}()
}

// --- sessions ---------------------------------------------------------

// sessionStates is every open session, as the sweep and the cap need to
// see it.
//
// Rust: `fn session_states(&self) -> Vec<SessionState>`.
func (s *Shell) sessionStates() []SessionState {
	states := []SessionState{}
	for _, tab := range s.tabs {
		if tab, ok := tab.(*QueryTab); ok && tab.session != nil {
			states = append(states, SessionState{
				tabID:         tab.id,
				running:       tab.run.inFlight(),
				inTransaction: tab.inTransaction,
				idle:          time.Since(tab.lastUsed),
			})
		}
	}
	return states
}

// sweepIdleSessions hands back sessions nobody has used for
// `IDLE_SESSION`.
//
// Rust: `fn sweep_idle_sessions(&mut self, cx)`.
func (s *Shell) sweepIdleSessions() {
	for _, tabID := range idleSessions(s.sessionStates(), idleSession) {
		s.endSession(tabID)
	}
}

// makeRoomForSession makes room for one more session by giving back the
// one that has gone longest without a question. `false` means every
// session is busy or holding a transaction, and the caller must say so
// rather than ask for one too many: the pool would make it wait out the
// connect timeout and then answer with a message about connections, for
// something the app could see coming.
//
// Rust: `fn make_room_for_session(&mut self, cx) -> bool`.
func (s *Shell) makeRoomForSession() bool {
	states := s.sessionStates()
	if len(states) < dbclient.MaxSessions {
		return true
	}
	tabID, ok := evictable(states)
	if !ok {
		return false
	}
	s.endSession(tabID)
	return true
}

// endSession takes one tab's session back: rolled back, returned to the
// pool, and the tab told so it can say why its next run starts fresh.
//
// Rust: `fn end_session(&mut self, tab_id: u64, cx)`.
func (s *Shell) endSession(tabID uint64) {
	tab, ok := s.tabMut(tabID).(*QueryTab)
	if !ok {
		return
	}
	session := tab.session
	if session == nil {
		return
	}
	tab.session = nil
	tab.sessionEnded = true
	go session.Close(context.Background())
}

// startSessionTimer sweeps for idle sessions while there are any. One loop
// for the window, as the run timer is, and it ends itself when the last
// session has gone — a workspace sitting on no connections must not keep
// waking up to notice that.
//
// Rust: `fn start_session_timer(&mut self, cx)`.
func (s *Shell) startSessionTimer() {
	if s.sweeping {
		return
	}
	s.sweeping = true
	go func() {
		for {
			time.Sleep(idleTick)
			more := false
			ok := s.call(func() {
				s.sweepIdleSessions()
				for _, tab := range s.tabs {
					if tab, ok := tab.(*QueryTab); ok && tab.session != nil {
						more = true
						break
					}
				}
				if !more {
					s.sweeping = false
				}
			})
			if !ok || !more {
				break
			}
		}
	}()
}

// refreshServerTiming asks the server what it actually spent on the
// statement this tab just ran, and fills it into the timing already on
// screen.
//
// **It is asked after the result, never before it.** The number is
// worth having and not worth waiting for: a round trip in front of the
// grid would add lag to the very measurement the user opened this to
// understand. So the result paints on the client's own reckoning, and
// the server's figure replaces the estimate a moment later — the strip
// drops its `~` and nothing else moves.
//
// The generation is checked twice over, as every reply to this tab is:
// a slow answer must not land on the run after the one it describes.
//
// Rust: `fn refresh_server_timing(&mut self, tab_id, generation, cx)`.
func (s *Shell) refreshServerTiming(tabID, generation uint64) {
	tab, ok := s.tabMut(tabID).(*QueryTab)
	if !ok {
		return
	}
	session := tab.session
	if session == nil {
		return
	}
	go func() {
		if !s.alive() {
			return
		}
		// Nothing to say is the ordinary answer here — no extension, or
		// a statement the server has not counted — and it is not worth
		// a word on screen. The client's own split stands.
		server, err := flatten(joinTask(func() (*dbclient.ServerTiming, error) {
			return session.ServerTiming(context.Background())
		}))
		if err != nil || server == nil {
			return
		}
		s.update(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok {
				return
			}
			if tab.generation != generation {
				return
			}
			if tab.timing != nil {
				tab.timing.server = server
			}
		})
	}()
}

// startTimer repaints while any run is out, so the toolbar's timer moves.
// One timer for the window, not one per run: `timing` is what stops a
// second run from starting a second loop, and the loop ends itself when
// the last run comes back.
//
// Rust: `fn start_timer(&mut self, cx)`.
func (s *Shell) startTimer() {
	if s.timing {
		return
	}
	s.timing = true
	go func() {
		for {
			time.Sleep(timerTick)
			more := false
			ok := s.call(func() {
				more = s.anyRunInFlight()
				if !more {
					s.timing = false
				}
			})
			if !ok || !more {
				break
			}
		}
	}()
}

func (s *Shell) anyRunInFlight() bool {
	for _, tab := range s.tabs {
		if tab, ok := tab.(*QueryTab); ok && tab.run.inFlight() {
			return true
		}
	}
	return false
}

// --- query history ---------------------------------------------------

// recordRun remembers a run in the local file. A history that cannot be
// written is not worth interrupting a session over, so the error is
// dropped.
//
// Rust: `fn record_run(&self, statement: &str, source: RunSource,
// outcome: &Outcome)`.
func (s *Shell) recordRun(statement string, source storage.RunSource, outcome *Outcome) {
	if s.store == nil {
		return
	}
	// A blank buffer is not a run worth keeping.
	trimmed := strings.TrimSpace(statement)
	if trimmed == "" {
		return
	}
	_ = s.store.RecordQuery(storage.NewRun{
		Scope:     s.scope,
		Statement: trimmed,
		RanAt:     unixNow(),
		Source:    source,
		ElapsedMs: outcome.elapsed,
		RowCount:  outcome.rows,
		Affected:  outcome.affected,
		Error:     outcome.error,
	})
}

// openHistory focuses the history tab, opening it if this session has
// none. One history tab is enough: it is a view of one file, not of a
// query.
//
// Rust: `fn open_history(&mut self, cx)`.
func (s *Shell) openHistory() {
	for ix, tab := range s.tabs {
		if _, ok := tab.(*HistoryTab); ok {
			s.activate(ix)
			id := s.tabs[ix].ID()
			s.loadHistory(id)
			return
		}
	}
	id := s.takeID()
	s.tabs = append(s.tabs, &HistoryTab{
		id:         id,
		rows:       nil,
		runs:       0,
		userOnly:   false,
		errorsOnly: false,
		error:      nil,
	})
	s.activate(len(s.tabs) - 1)
	s.loadHistory(id)
}

// loadHistory reads the runs back and flattens them. Reading is a local
// SQLite call, so it stays on this thread; the goroutine bridge is for the
// database, not for a file next to the profiles.
//
// Rust: `fn load_history(&mut self, tab_id: u64, cx)`.
func (s *Shell) loadHistory(tabID uint64) {
	scope := s.scope
	tab, ok := s.tabMut(tabID).(*HistoryTab)
	if !ok {
		return
	}
	since := unixNow() - historyDays*24*60*60
	filter := storage.DefaultHistoryFilter()
	filter.UserOnly = tab.userOnly
	filter.ErrorsOnly = tab.errorsOnly
	filter.Since = &since

	if s.store == nil {
		reason := "no history file"
		if s.storeError != nil {
			reason = *s.storeError
		}
		tab.error = &reason
		return
	}
	runs, err := s.store.ListHistory(scope, filter)
	if err == nil {
		rows := HistoryFlatten(runs, HistoryToday())
		tab.runs = len(runs)
		tab.rows = rows
		tab.error = nil
	} else {
		tab.rows = nil
		tab.runs = 0
		message := err.Error()
		tab.error = &message
	}
}

// reloadOpenHistory keeps an open history tab current when a run finishes
// behind it.
//
// Rust: `fn reload_open_history(&mut self, cx)`.
func (s *Shell) reloadOpenHistory() {
	for _, tab := range s.tabs {
		if _, ok := tab.(*HistoryTab); ok {
			s.loadHistory(tab.ID())
			return
		}
	}
}

// toggleHistoryFilter flips one of the history screen's two chips: the
// errors chip when `errors`, the "mine only" chip otherwise.
//
// Rust: `fn toggle_history_filter(&mut self, tab_id, errors: bool, cx)`.
func (s *Shell) toggleHistoryFilter(tabID uint64, errors bool) {
	tab, ok := s.tabMut(tabID).(*HistoryTab)
	if !ok {
		return
	}
	if errors {
		tab.errorsOnly = !tab.errorsOnly
	} else {
		tab.userOnly = !tab.userOnly
	}
	s.loadHistory(tabID)
}

// joinTask runs a task the way a tokio task ran it, and reports what
// `Tokio::spawn`'s handle reported: the task's own result, and — when it
// panicked — what it panicked with, which is Rust's `JoinError`. Call it
// on the goroutine that does the work, and hand its three values to
// `flatten` or `connectOutcome`.
func joinTask[T any](task func() (T, error)) (value T, err error, join any) {
	defer func() {
		if caught := recover(); caught != nil {
			var zero T
			value, err, join = zero, nil, caught
		}
	}()
	value, err = task()
	return value, err, nil
}

// flatten collapses "the tokio task died" and "the query failed" into one
// message. Either way the user needs a sentence, not a nested Result.
//
// Go port: Rust's `Result<anyhow::Result<T>, JoinError>` is the three
// values of joinTask; the `String` error is an `error` whose text is the
// sentence.
func flatten[T any](value T, err error, join any) (T, error) {
	var zero T
	switch {
	case join != nil:
		return zero, fmt.Errorf("the query was interrupted: %v", join)
	case err != nil:
		return zero, errors.New(err.Error())
	default:
		return value, nil
	}
}

// connectOutcome is the same collapse for the connect, which keeps its
// class: the strip paints the headline, the detail and the hint apart, so
// a sentence is not enough. A task that died is `Other`, with what the
// runtime said.
//
// Go port: the error is nil on success.
func connectOutcome[T any](value T, err error, join any) (T, *dbclient.ConnectError) {
	var zero T
	switch {
	case join != nil:
		failed := dbclient.NewConnectError(
			dbclient.ConnectFailureOther,
			fmt.Sprintf("the connect was interrupted: %v", join),
		)
		return zero, &failed
	case err != nil:
		failed := dbclient.ConnectErrorOf(err)
		return zero, &failed
	default:
		return value, nil
	}
}

// sqlRun is what `run_sql` answered with: the result, and the
// milliseconds it took — Rust's `(QueryResult, u128)`.
type sqlRun struct {
	result  dbclient.QueryResult
	elapsed uint64
}

// runSQL runs one statement and times it. Timing wraps the call itself,
// so it measures the database, not the paint that follows.
//
// Go port: Rust spawned it onto tokio and handed back the task; this is
// the task's body, run on the caller's goroutine through `joinTask`.
func runSQL(connection dbclient.Connection, sql string) (sqlRun, error) {
	started := time.Now()
	result, err := connection.Execute(context.Background(), sql)
	if err != nil {
		return sqlRun{}, err
	}
	return sqlRun{result: result, elapsed: uint64(time.Since(started).Milliseconds())}, nil
}

// clone is the derived `Clone` of Shape: its own slice of statements.
func (sh Shape) clone() Shape {
	return Shape{
		ddl: append([]ddlAt(nil), sh.ddl...),
		ms:  sh.ms,
	}
}

// runStatements runs a buffer's statements in order and keeps the last
// result that has columns, so a trailing `create table` does not blank a
// grid the SELECT before it filled. The first statement to fail stops the
// run and its error is what the user sees.
//
// **All of them go down one connection, in order**, so `BEGIN; SELECT …;`
// in one buffer means what it reads as. Before sessions each statement
// took whichever pooled connection was free, and the second one could not
// see what the first had done.
//
// Go port: Rust's `run_statements` returned a GPUI task; this is its body,
// and it blocks — call it on a goroutine, never on the main thread. Each
// `this.update` it awaited is `s.call`; the ones whose answer it dropped
// (`.ok()`) are `s.update`, which keeps their order on the main thread.
func (s *Shell) runStatements(
	connection dbclient.Connection,
	session dbclient.Session,
	begin bool,
	statements []string,
	tabID uint64,
	generation uint64,
) RunOutcome {
	ctx := context.Background()
	// The tab's session, opening one if this is its first run. The
	// open is its own step rather than the first thing inside the run,
	// so the tab knows its backend before a statement is ever sent.
	if session == nil {
		if !s.alive() {
			return abandonedRunOutcome()
		}
		opened, err := flatten(joinTask(func() (dbclient.Session, error) {
			return connection.OpenSession(ctx)
		}))
		if err != nil {
			// No connection, so no statement left: the gutter
			// must not leave a buffer queued for ever.
			s.update(func() {
				s.skipStatements(tabID, generation, 0, metaNoSession)
			})
			return RunOutcome{
				session: nil,
				result:  nil,
				err:     err.Error(),
				began:   false,
				shape:   Shape{},
			}
		}
		session = opened
	}

	// The tab learns where its session is the moment there is one, so
	// every stop and every close from here on has a backend to name.
	//
	// It is also where a run called off while the session was opening
	// ends. Nothing was sent, so calling it off costs the server
	// nothing — which is why a stop in that window needs no waiting.
	goOn := false
	ok := s.call(func() {
		tab, ok := s.tabMut(tabID).(*QueryTab)
		if !ok {
			return
		}
		if tab.generation != generation {
			return
		}
		tab.session = session
		if tab.run.kind == RunRunning {
			tab.run.flight.backend = optional(session.Backend())
		}
		goOn = tab.run.kind != RunCancelling
	})
	if !ok || !goOn {
		// Nothing was sent, so nothing was begun: a run called off in
		// this window costs the server nothing at all.
		s.update(func() {
			s.skipStatements(tabID, generation, 0, metaStopped)
		})
		return RunOutcome{
			session: session,
			result:  nil,
			err:     calledOff,
			began:   false,
			shape:   Shape{},
		}
	}

	// The clock starts before the `BEGIN`, because the round trip is
	// part of what the user waited — but the `BEGIN` is not one of the
	// statements, so it is neither marked in the gutter nor counted.
	started := time.Now()
	began := false
	if begin {
		if !s.alive() {
			return abandonedRunOutcome()
		}
		_, err := flatten(joinTask(func() (struct{}, error) {
			return struct{}{}, session.Begin(ctx)
		}))
		if err != nil {
			// The transaction never opened, so none of the statements
			// behind it were sent either.
			s.update(func() {
				s.skipStatements(tabID, generation, 0, metaNoBegin)
			})
			return RunOutcome{
				session: session,
				result:  nil,
				err:     err.Error(),
				began:   false,
				shape:   Shape{},
			}
		}
		began = true
	}

	// **The loop is here, and one goroutine hop carries one statement.**
	// It used to be one task carrying the whole buffer, which could say
	// nothing until the last statement landed — and the gutter needs
	// each answer as it arrives. It also puts the stop *between* two
	// statements: before, a ⌘. that reached the backend while it sat
	// idle between them cancelled nothing, and the rest of the buffer
	// went out anyway.
	var tally Tally
	for ix, statement := range statements {
		goOn := false
		ok := s.call(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok {
				return
			}
			if tab.generation != generation {
				return
			}
			goOn = tab.run.kind != RunCancelling
			if goOn {
				tab.editor.SetStatementStatus(ix, sqleditor.StatementRunning, metaRunning)
			} else {
				// Never sent, and the gutter says so rather than
				// leaving them queued for ever.
				tab.editor.SkipStatementsFrom(ix, metaStopped)
			}
		})
		if !ok || !goOn {
			return RunOutcome{
				session: session,
				result:  nil,
				err:     stoppedBetween,
				began:   began,
				shape:   tally.shape.clone(),
			}
		}

		// The statement's own clock, for the statistic beside it. The
		// run's clock is the whole wait; this is one statement's share
		// of it, which is the number that says *which* of five was the
		// slow one.
		sent := time.Now()
		if !s.alive() {
			return abandonedRunOutcome()
		}
		result, err := flatten(joinTask(func() (dbclient.QueryResult, error) {
			return session.Execute(ctx, statement)
		}))
		ms := uint64(time.Since(sent).Milliseconds())
		if err == nil {
			verb := optional(query.DdlVerbOf(statement))
			meta := statementMeta(verb, statement, &result, ms)
			tally.add(ix, statement, verb, result, ms)
			s.update(func() {
				s.markStatement(tabID, generation, ix, sqleditor.StatementDone, meta)
			})
			continue
		}
		// The first failure ends the run, so everything behind
		// it never left: this line takes the failure and the
		// lines below it wear the dash.
		//
		// **A statement the user stopped is not a failure.**
		// The server refuses a cancelled statement in the same
		// words it refuses a broken one, and the tab already
		// paints CANCELLED rather than an error strip for it —
		// so the gutter says the same thing, and this line
		// takes the dash with the rest.
		s.update(func() {
			stopped := false
			if tab, ok := s.tabMut(tabID).(*QueryTab); ok && tab.run.kind == RunCancelling {
				stopped = true
			}
			if stopped {
				s.markStatement(
					tabID,
					generation,
					ix,
					sqleditor.StatementSkipped,
					fmt.Sprintf("stopped · %s", uikit.FormatMillis(ms)),
				)
				s.skipStatements(tabID, generation, ix+1, metaStopped)
			} else {
				s.markStatement(
					tabID,
					generation,
					ix,
					sqleditor.StatementFailed,
					fmt.Sprintf("failed · %s", uikit.FormatMillis(ms)),
				)
				s.skipStatements(
					tabID,
					generation,
					ix+1,
					fmt.Sprintf("not run · statement %d failed", ix+1),
				)
			}
		})
		return RunOutcome{
			session: session,
			result:  nil,
			err:     err.Error(),
			began:   began,
			shape:   tally.shape.clone(),
		}
	}

	shape := tally.shape
	tally.shape = Shape{}
	ran := tally.finish(started)
	return RunOutcome{
		session: session,
		result:  &ran,
		err:     "",
		began:   began,
		shape:   shape,
	}
}

// add counts one statement that landed.
func (t *Tally) add(
	ix int,
	statement string,
	ddl *query.DdlVerb,
	result dbclient.QueryResult,
	ms uint64,
) {
	t.ran++
	if ddl != nil {
		t.shape.ddl = append(t.shape.ddl, ddlAt{ix: ix, verb: *ddl})
		t.shape.ms += ms
	}
	if len(result.Columns) == 0 {
		// No result set, so the count is the whole of what this
		// statement said — including a zero, which is the answer to
		// "did my `WHERE` match anything".
		t.affected += result.RowsAffected
		t.counted++
		read := optional(query.CommandVerbOf(statement))
		switch {
		case t.verb == nil:
			t.verb = &tallyVerb{verb: read}
		// Two statements that changed rows in different ways have
		// no shared word, so the line takes the generic one rather
		// than the last statement's.
		case sameCommandVerb(t.verb.verb, read):
			t.verb = &tallyVerb{verb: read}
		default:
			t.verb = &tallyVerb{verb: nil}
		}
	} else {
		t.last = result
	}
}

// sameCommandVerb is `Option<CommandVerb>`'s `==`.
func sameCommandVerb(a, b *query.CommandVerb) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (t *Tally) finish(started time.Time) Ran {
	var verb *query.CommandVerb
	if t.verb != nil {
		verb = t.verb.verb
	}
	return Ran{
		result:    t.last,
		affected:  t.affected,
		counted:   t.counted,
		verb:      verb,
		elapsedMs: uint64(time.Since(started).Milliseconds()),
		ran:       t.ran,
	}
}
