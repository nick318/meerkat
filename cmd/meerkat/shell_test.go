package main

// The `#[cfg(test)] mod tests` of shell.rs: one Go test per Rust test, in
// the same order, with the same inputs and the same assertions. The Rust
// helpers carry a `shell` prefix here, because the package's other tests
// share the namespace.

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/query"
	"github.com/nick318/meerkat/internal/sqleditor"
)

func shellStr(s string) *string { return &s }

func shellU64(v uint64) *uint64 { return &v }

func shellInt(v int) *int { return &v }

func TestTheEndpointIsTheHostAndThePortAndNeverThePassword(t *testing.T) {
	url := func(text string) string { return endpoint(TargetUrl{Url: text}) }
	for _, c := range []struct{ in, want string }{
		{"postgres://app:secret@db.internal:6432/shop", "db.internal:6432"},
		{"postgres://app@db.internal/shop?sslmode=require", "db.internal:5432"},
		{"postgres://app:p@ss@db.internal/shop", "db.internal:5432"},
		{"postgres://[::1]/shop", "[::1]:5432"},
		{"postgres:///shop", "localhost:5432"},
	} {
		if got := url(c.in); got != c.want {
			t.Errorf("endpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestATabWithNoServerSaysWhy(t *testing.T) {
	failed := StatusFailed{Error: dbclient.NewConnectError(
		dbclient.ConnectFailureRefused,
		"127.0.0.1:5433 · Connection refused (os error 61)",
	)}
	if got, want := notConnectedCopy(failed),
		"Not connected · The connection was refused · 127.0.0.1:5433 · Connection refused (os error 61)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	waiting := StatusConnecting{To: "db.internal:5432"}
	if got, want := notConnectedCopy(waiting),
		"Not connected yet · still connecting to db.internal:5432"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAConnectThatFailedKeepsItsClass(t *testing.T) {
	var refused error = dbclient.NewConnectError(dbclient.ConnectFailureAuth, "password authentication failed")
	_, err := connectOutcome(struct{}{}, refused, nil)
	if err == nil {
		t.Fatal("connect_outcome(Ok(Err(..))) is not an error")
	}
	if err.Kind != dbclient.ConnectFailureAuth {
		t.Errorf("kind = %v, want Auth", err.Kind)
	}
	if err.Detail != "password authentication failed" {
		t.Errorf("detail = %q", err.Detail)
	}
}

// shellNothing is a connection that knows no names at all, which is what
// a tab has before the catalog lands.
func shellNothing() *sqleditor.Vocabulary {
	return &sqleditor.Vocabulary{}
}

func shellKnows(names ...string) *sqleditor.Vocabulary {
	var all []sqleditor.Name
	for _, name := range names {
		all = append(all, sqleditor.Name{
			Name:   name,
			Detail: "table",
			Kind:   sqleditor.KindRelation,
			Owner:  nil,
		})
	}
	return sqleditor.NewVocabulary(all)
}

func shellSyntax(offset *int) *dbclient.CheckError {
	return &dbclient.CheckError{
		Refusal: dbclient.RefusalSyntax,
		Message: "syntax error at or near \"frm\"",
		Offset:  offset,
	}
}

func shellName(offset *int) *dbclient.CheckError {
	return &dbclient.CheckError{
		Refusal: dbclient.RefusalName,
		Message: "relation \"t\" does not exist",
		Offset:  offset,
	}
}

func shellMarked(sql string, mark sqleditor.Diagnostic) string {
	return sql[mark.Range.Start:mark.Range.End]
}

// The server answers about one statement and names a point inside it.
// The mark comes back as a range of the **buffer**, on the token the
// user typed.
func TestAStatementsErrorIsPlacedBackInTheBuffer(t *testing.T) {
	sql := "select 1;\nselect 2 frm t"
	ranges := query.StatementRanges(sql)
	// "select 2 frm t" starts at byte 10; `frm` is at 9 inside it.
	found := []*dbclient.CheckError{nil, shellSyntax(shellInt(9))}
	marks := diagnostics(sql, ranges, found, 0, shellNothing())
	if len(marks) != 1 {
		t.Fatalf("marks = %v, want one", marks)
	}
	if got := shellMarked(sql, marks[0]); got != "frm" {
		t.Errorf("marked %q, want %q", got, "frm")
	}
}

// No position from the server is not "no error": the statement as a
// whole is as close as anything here can point.
func TestAnErrorWithNoPositionMarksTheStatement(t *testing.T) {
	sql := "select 1 frm t"
	ranges := query.StatementRanges(sql)
	marks := diagnostics(sql, ranges, []*dbclient.CheckError{shellSyntax(nil)}, 99, shellNothing())
	if len(marks) == 0 {
		t.Fatal("no mark")
	}
	if got := shellMarked(sql, marks[0]); got != sql {
		t.Errorf("marked %q, want %q", got, sql)
	}
}

// The rule that keeps the marks from chasing the caret. `select ` is
// a syntax error and is also what every query looks like a second
// after it is started.
func TestAnUnfinishedStatementUnderTheCaretIsNotMarked(t *testing.T) {
	sql := "select 1;\nselect"
	ranges := query.StatementRanges(sql)
	// The second statement stops at the end of the input, which is
	// where the caret is while it is being typed.
	found := []*dbclient.CheckError{nil, shellSyntax(shellInt(6))}
	if marks := diagnostics(sql, ranges, found, 16, shellNothing()); len(marks) != 0 {
		t.Errorf("marks = %v, want none", marks)
	}
	// The caret moved back to the statement above: the user has
	// stopped writing this one, and an unfinished statement is then
	// worth saying. The mark goes on the token they stopped on.
	marks := diagnostics(sql, ranges, found, 2, shellNothing())
	if len(marks) == 0 {
		t.Fatal("no mark")
	}
	if got := shellMarked(sql, marks[0]); got != "select" {
		t.Errorf("marked %q, want %q", got, "select")
	}
}

// A name error is only true of the connection the check went down,
// and a buffer that makes the name first is the case it is wrong
// about. The statement that *makes* it still answers for itself.
func TestANameTheBufferMakesItselfIsNotMarked(t *testing.T) {
	sql := "create table t (id int);\nselect * from t"
	ranges := query.StatementRanges(sql)
	found := []*dbclient.CheckError{nil, shellName(shellInt(14))}
	if marks := diagnostics(sql, ranges, found, 0, shellNothing()); len(marks) != 0 {
		t.Errorf("marks = %v, want none", marks)
	}

	// Turn the first statement into one that makes nothing, and the
	// same answer is worth painting.
	sql = "select 1;\nselect * from t"
	ranges = query.StatementRanges(sql)
	marks := diagnostics(sql, ranges, found, 0, shellNothing())
	if len(marks) == 0 {
		t.Fatal("no mark")
	}
	if got := shellMarked(sql, marks[0]); got != "t" {
		t.Errorf("marked %q, want %q", got, "t")
	}
}

// A syntax error is context-free, so nothing about the buffer above
// it can make it untrue.
func TestASyntaxErrorSurvivesABufferThatMakesNames(t *testing.T) {
	sql := "create table t (id int);\nselect 1 frm t"
	ranges := query.StatementRanges(sql)
	marks := diagnostics(sql, ranges, []*dbclient.CheckError{nil, shellSyntax(shellInt(9))}, 0, shellNothing())
	if len(marks) == 0 {
		t.Fatal("no mark")
	}
	if got := shellMarked(sql, marks[0]); got != "frm" {
		t.Errorf("marked %q, want %q", got, "frm")
	}
}

// **The server is right and one token late.** `limi` parses as a
// table alias, so the refusal lands on the `100` after it — and the
// mark has to cover the word that made the parser go wrong.
func TestTheMarkReachesBackOverTheWordThatLostTheParser(t *testing.T) {
	sql := "select * from tenant.effort limi 100"
	ranges := query.StatementRanges(sql)
	// The server points at `100`, which starts at byte 33.
	marks := diagnostics(sql, ranges, []*dbclient.CheckError{shellSyntax(shellInt(33))}, 0, shellNothing())
	if len(marks) == 0 {
		t.Fatal("no mark")
	}
	if got := shellMarked(sql, marks[0]); got != "limi 100" {
		t.Errorf("marked %q, want %q", got, "limi 100")
	}
}

// The two ways a word can be meant, and each stops the reach.
func TestAKeywordOrANameTheDatabaseHasStopsTheReach(t *testing.T) {
	// `from` is SQL's own word, so the mark stays on the token the
	// server named.
	sql := "select from t"
	ranges := query.StatementRanges(sql)
	marks := diagnostics(sql, ranges, []*dbclient.CheckError{shellSyntax(shellInt(7))}, 99, shellNothing())
	if len(marks) == 0 {
		t.Fatal("no mark")
	}
	if got := shellMarked(sql, marks[0]); got != "from" {
		t.Errorf("marked %q, want %q", got, "from")
	}

	// `users` is a table this connection has, so it was reached for on
	// purpose and the `100` is what is wrong.
	sql = "select * from users 100"
	ranges = query.StatementRanges(sql)
	found := []*dbclient.CheckError{shellSyntax(shellInt(20))}
	marks = diagnostics(sql, ranges, found, 0, shellKnows("users"))
	if len(marks) == 0 {
		t.Fatal("no mark")
	}
	if got := shellMarked(sql, marks[0]); got != "100" {
		t.Errorf("marked %q, want %q", got, "100")
	}
	// The same statement against a connection that has never heard of
	// `users` reaches back, because then it is a word like any other.
	marks = diagnostics(sql, ranges, found, 0, shellNothing())
	if len(marks) == 0 {
		t.Fatal("no mark")
	}
	if got := shellMarked(sql, marks[0]); got != "users 100" {
		t.Errorf("marked %q, want %q", got, "users 100")
	}
}

// A name error already points at the name it could not find, so
// nothing reaches back from one.
func TestANameErrorMarksTheNameAndNothingBeforeIt(t *testing.T) {
	sql := "select * from tenant.effort"
	ranges := query.StatementRanges(sql)
	marks := diagnostics(sql, ranges, []*dbclient.CheckError{shellName(shellInt(14))}, 0, shellNothing())
	if len(marks) == 0 {
		t.Fatal("no mark")
	}
	if got := shellMarked(sql, marks[0]); got != "tenant.effort" {
		t.Errorf("marked %q, want %q", got, "tenant.effort")
	}
}

func TestOnlyTheEndOfTheInputIsReadAsUnfinished(t *testing.T) {
	statement := query.Range{Start: 10, End: 24}
	// The point is inside the statement, so the caret means nothing.
	if beingTyped(statement, 19, 19) {
		t.Error("an error inside the statement read as unfinished")
	}
	// At the end of it, with the caret in it.
	if !beingTyped(statement, 24, 24) {
		t.Error("caret at the end")
	}
	if !beingTyped(statement, 24, 10) {
		t.Error("caret at the start")
	}
	// At the end of it, with the caret in another statement.
	if beingTyped(statement, 24, 4) {
		t.Error("caret in another statement")
	}
}

// The strip and the mark have to be about the same error, and the
// caret is what says which.
func TestTheStripSpeaksForTheMarkTheCaretIsIn(t *testing.T) {
	marks := []sqleditor.Diagnostic{
		{
			Range:   sqleditor.Range{Start: 4, End: 8},
			Message: "first",
		},
		{
			Range:   sqleditor.Range{Start: 20, End: 24},
			Message: "second",
		},
	}
	if got := spokenFor(marks, 22); got != 1 {
		t.Errorf("spoken_for(22) = %d, want 1", got)
	}
	// The caret at the very end of a mark is still in it: that is
	// where it lands after the word is typed.
	if got := spokenFor(marks, 8); got != 0 {
		t.Errorf("spoken_for(8) = %d, want 0", got)
	}
	// Nowhere near one, so the first is spoken for.
	if got := spokenFor(marks, 40); got != 0 {
		t.Errorf("spoken_for(40) = %d, want 0", got)
	}
	if got := spokenFor(nil, 0); got != 0 {
		t.Errorf("spoken_for([], 0) = %d, want 0", got)
	}
}

func TestADoubleClickMarksTheWordItLandedIn(t *testing.T) {
	text := "select from user_id, name"
	if got := wordAt(text, 14); got != (query.Range{Start: 12, End: 19}) {
		t.Errorf("word_at(14) = %v, want 12..19", got)
	}
	if r := wordAt(text, 12); text[r.Start:r.End] != "user_id" {
		t.Errorf("word_at(12) = %q", text[r.Start:r.End])
	}
	if r := wordAt(text, 21); text[r.Start:r.End] != "name" {
		t.Errorf("word_at(21) = %q", text[r.Start:r.End])
	}
}

func TestADoubleClickOnPunctuationMarksThatCharacterAlone(t *testing.T) {
	text := "{\"a\": 1}"
	if got := wordAt(text, 0); got != (query.Range{Start: 0, End: 1}) {
		t.Errorf("word_at(0) = %v, want 0..1", got)
	}
	// A click past the end of the text marks nothing.
	if got := wordAt(text, len(text)); got != (query.Range{Start: len(text), End: len(text)}) {
		t.Errorf("word_at(len) = %v, want empty at the end", got)
	}
}

func TestATripleClickMarksTheLineWithoutItsNewline(t *testing.T) {
	text := "one\ntwo\nthree"
	for _, c := range []struct {
		at   int
		want string
	}{{0, "one"}, {5, "two"}, {12, "three"}} {
		r := lineAt(text, c.at)
		if got := text[r.Start:r.End]; got != c.want {
			t.Errorf("line_at(%d) = %q, want %q", c.at, got, c.want)
		}
	}
}

func TestAnOffsetIsPulledOntoACharacterBoundary(t *testing.T) {
	text := "héllo"
	// The layout can name an offset the text no longer has.
	if got := clampToBoundary(text, 99); got != len(text) {
		t.Errorf("clamp_to_boundary(99) = %d, want %d", got, len(text))
	}
	// 2 is inside the two-byte é.
	if got := clampToBoundary(text, 2); got != 1 {
		t.Errorf("clamp_to_boundary(2) = %d, want 1", got)
	}
}

// The arming window, which is the whole of why the button's state is a
// type of its own: for the first `RUN_ARM` the statement is out and the
// button still says "run".
func TestTheButtonOffersAStopOnlyOnceARunHasLasted(t *testing.T) {
	ago := func(ago time.Duration) Live {
		return Live{
			started: time.Now().Add(-ago),
			backend: nil,
		}
	}
	idle := Run{kind: RunIdle}
	if got := idle.phase(); got != RunPhaseReady {
		t.Errorf("Idle.phase() = %v", got)
	}
	cancelled := Run{kind: RunCancelled, elapsed: 120}
	if got := cancelled.phase(); got != RunPhaseReady {
		t.Errorf("Cancelled.phase() = %v", got)
	}
	// A statement that answers inside the window never swaps the verb,
	// which is the point: most of them answer inside it.
	quick := Run{kind: RunRunning, flight: ago(80 * time.Millisecond)}
	if got := quick.phase(); got != RunPhaseReady {
		t.Errorf("quick.phase() = %v", got)
	}
	if !quick.arming() {
		t.Error("quick is not arming")
	}
	slow := Run{kind: RunRunning, flight: ago(runArm)}
	if got := slow.phase(); got != RunPhaseStop {
		t.Errorf("slow.phase() = %v", got)
	}
	if slow.arming() {
		t.Error("slow is arming")
	}
	// A cancel is with the server, so the second press is the one that
	// closes the backend — and that state never arms.
	cancelling := Run{kind: RunCancelling, flight: ago(10 * time.Millisecond)}
	if got := cancelling.phase(); got != RunPhaseTerminate {
		t.Errorf("cancelling.phase() = %v", got)
	}
	if cancelling.arming() {
		t.Error("cancelling is arming")
	}
	// Nothing is out, so a click runs a query.
	if idle.arming() {
		t.Error("Idle is arming")
	}
}

// The divider policy: the size tracks the pointer's travel from the
// press, and stops at the rails.
func TestADividerDragMovesByThePointerAndStopsAtTheRails(t *testing.T) {
	for _, c := range []struct{ start, anchor, at, min, max, want float32 }{
		// Ten pixels right of where the press landed is ten pixels wider,
		// wherever on the handle the press landed.
		{246., 100., 110., 180., 520., 256.},
		{246., 100., 90., 180., 520., 236.},
		// The rails hold at both ends.
		{246., 100., 900., 180., 520., 520.},
		{246., 100., -900., 180., 520., 180.},
		// A window shrunk until the ceiling is under the floor answers
		// with the floor rather than panicking in `clamp`.
		{246., 100., 110., 180., 90., 180.},
	} {
		if got := draggedSize(c.start, c.anchor, c.at, c.min, c.max); got != c.want {
			t.Errorf("dragged_size(%v, %v, %v, %v, %v) = %v, want %v", c.start, c.anchor, c.at, c.min, c.max, got, c.want)
		}
	}
}

// The rails against the window in hand: the sidebar never takes more
// than half of it, the editor never more than its share, and neither
// ceiling falls below its floor on a tiny window.
func TestTheDividerRailsFollowTheWindow(t *testing.T) {
	for _, c := range []struct {
		divider        Divider
		width, height  float32
		wantMin, wantM float32
	}{
		{DividerSidebar, 1400., 900., sidebarMin, sidebarMax},
		// Half of an 800px window is under the 520 cap.
		{DividerSidebar, 800., 900., sidebarMin, 400.},
		{DividerSidebar, 200., 900., sidebarMin, sidebarMin},
		{DividerEditor, 1400., 1000., editorMin, 700.},
		{DividerEditor, 1400., 80., editorMin, editorMin},
	} {
		min, max := c.divider.rangeIn(c.width, c.height)
		if min != c.wantMin || max != c.wantM {
			t.Errorf("%v.range(%vx%v) = (%v, %v), want (%v, %v)", c.divider, c.width, c.height, min, max, c.wantMin, c.wantM)
		}
	}
}

// The sidebar cursor's ring, with the filter line itself in it: ↓
// walks into the names and ↑ walks back out to the line.
func TestTheSidebarCursorWalksOutOfTheListItWalkedInto(t *testing.T) {
	// Indices into the flattened rows, so the headers between them are
	// the gaps: a schema heading, two tables, a section heading, one
	// more table.
	stops := []int{1, 2, 4}
	check := func(stops []int, from *int, forward bool, want *int) {
		t.Helper()
		got, ok := stepStop(stops, from, forward)
		switch {
		case want == nil && ok:
			t.Errorf("step_stop(%v, %v, %v) = Some(%d), want None", stops, from, forward, got)
		case want != nil && (!ok || got != *want):
			t.Errorf("step_stop(%v, %v, %v) = (%d, %v), want Some(%d)", stops, from, forward, got, ok, *want)
		}
	}
	// The line is where a fresh filter leaves the cursor, and each key
	// enters the list from its own end.
	check(stops, nil, true, shellInt(1))
	check(stops, nil, false, shellInt(4))
	// One name at a time, over the headings.
	check(stops, shellInt(1), true, shellInt(2))
	check(stops, shellInt(2), true, shellInt(4))
	check(stops, shellInt(4), false, shellInt(2))
	// Both ends come back to the line, which is where the user is
	// still typing.
	check(stops, shellInt(4), true, nil)
	check(stops, shellInt(1), false, nil)
	// Nothing matched: there is nowhere to walk to.
	check(nil, nil, true, nil)
}

// Manual mode's whole mechanism: one `BEGIN`, and never a second.
func TestARunOpensATransactionOnlyWhenManualModeHasNone(t *testing.T) {
	verb := func(v query.TxVerb) *query.TxVerb { return &v }
	// Auto mode holds nothing open, whatever the buffer says.
	if needsBegin(dbclient.TxModeAuto, false, nil) {
		t.Error("auto, nothing open")
	}
	if needsBegin(dbclient.TxModeAuto, false, verb(query.TxVerbBegin)) {
		t.Error("auto, typed BEGIN")
	}
	// Manual mode with nothing open opens one.
	if !needsBegin(dbclient.TxModeManual, false, nil) {
		t.Error("manual, nothing open")
	}
	// ...and never a second: the transaction spans the runs of the tab.
	if needsBegin(dbclient.TxModeManual, true, nil) {
		t.Error("manual, one open")
	}
	// The buffer's own `BEGIN` is the user saying where it starts.
	if needsBegin(dbclient.TxModeManual, false, verb(query.TxVerbBegin)) {
		t.Error("manual, typed BEGIN")
	}
	// A buffer that ends a transaction still needs one to end.
	if !needsBegin(dbclient.TxModeManual, false, verb(query.TxVerbCommit)) {
		t.Error("manual, typed COMMIT")
	}
}

// The line for a statement that answered with a count. **Zero is the
// reading that matters**: it is the answer to "did my `WHERE` match
// anything", and the line it replaced — `0 rows · 0 columns` — reported
// it as an empty table instead.
func TestACountIsWordedByItsVerb(t *testing.T) {
	said := func(rows uint64, verb query.CommandVerb) string {
		return changedCopy(Changed{rows: rows, verb: &verb})
	}
	for _, c := range []struct{ got, want string }{
		{said(1, query.CommandVerbUpdate), "1 row updated"},
		{said(42, query.CommandVerbDelete), "42 rows deleted"},
		{said(0, query.CommandVerbUpdate), "no rows updated"},
		{said(0, query.CommandVerbInsert), "no rows inserted"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// With no verb read there is nothing to say happened, so the line says
// only what the server counted. A `CREATE INDEX` has no rows to have
// matched, and telling the user none were updated would be a claim
// about a statement that never updates anything.
func TestACountWithNoVerbTakesTheNeutralWord(t *testing.T) {
	said := func(rows uint64) string { return changedCopy(Changed{rows: rows, verb: nil}) }
	for _, c := range []struct{ got, want string }{
		{said(0), "no rows to show"},
		{said(1), "1 row affected"},
		{said(9), "9 rows affected"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// The bar counts rows now that the server's own count is read off the
// completion tag. It is said only where there is something to say: a
// transaction of nothing but `SELECT`s has touched nothing, and a `0`
// there would be a warning about nothing.
func TestTheTransactionBarCountsTheRowsItIsHolding(t *testing.T) {
	state := func(statements int, affected uint64) TxState {
		return TxState{
			mode:       dbclient.TxModeManual,
			open:       true,
			done:       nil,
			statements: statements,
			affected:   affected,
			running:    false,
			ending:     false,
		}
	}
	_, sub, ok := txCopy(state(1, 1))
	if !ok || !strings.HasPrefix(sub, "1 statement · 1 row touched · ") {
		t.Errorf("%s", sub)
	}
	_, sub, ok = txCopy(state(2, 30))
	if !ok || !strings.HasPrefix(sub, "2 statements · 30 rows touched · ") {
		t.Errorf("%s", sub)
	}
	// Nothing touched, nothing said about rows.
	_, sub, ok = txCopy(state(2, 0))
	if !ok || !strings.HasPrefix(sub, "2 statements · nothing visible") {
		t.Errorf("%s", sub)
	}
}

// A transaction that ends forgets what it held, rows and statements
// alike. Before, a rollback reset the statement count and kept the
// rows, so an `UPDATE` of 3 rows rolled back and run again said 6.
func TestARolledBackTransactionLeavesNoRowsBehind(t *testing.T) {
	var tally TxTally
	tally.add(1, 3)
	if tally != (TxTally{statements: 1, affected: 3}) {
		t.Errorf("tally = %+v", tally)
	}
	// What `end_transaction` and `refresh_transaction` put back.
	tally = TxTally{}
	tally.add(1, 3)
	if tally.affected != 3 {
		t.Errorf("affected = %d, want 3", tally.affected)
	}
	if tally.statements != 1 {
		t.Errorf("statements = %d, want 1", tally.statements)
	}
}

// The bar is painted for a transaction the *user* opened as well, in
// either mode — which is the whole of what "handle a typed BEGIN"
// means on screen.
func TestTheTransactionBarSpeaksForBothModes(t *testing.T) {
	state := func(mode dbclient.TxMode, open bool, done *dbclient.TxEnd, statements int) TxState {
		return TxState{
			mode:       mode,
			open:       open,
			done:       done,
			statements: statements,
			affected:   0,
			running:    false,
			ending:     false,
		}
	}
	// Nothing open and nothing just ended: no bar at all.
	if _, _, ok := txCopy(state(dbclient.TxModeAuto, false, nil, 0)); ok {
		t.Error("a bar in auto mode with nothing open")
	}
	if _, _, ok := txCopy(state(dbclient.TxModeManual, false, nil, 0)); ok {
		t.Error("a bar in manual mode with nothing open")
	}

	// A `BEGIN` typed into an auto-mode buffer gets the same bar as a
	// transaction manual mode opened.
	title, sub, ok := txCopy(state(dbclient.TxModeAuto, true, nil, 2))
	if !ok {
		t.Fatal("no bar")
	}
	if title != "transaction open" {
		t.Errorf("title = %q", title)
	}
	if !strings.HasPrefix(sub, "2 statements · ") {
		t.Errorf("%s", sub)
	}
	manualTitle, _, ok := txCopy(state(dbclient.TxModeManual, true, nil, 1))
	if !ok || manualTitle != title {
		t.Errorf("manual title = %q, want %q", manualTitle, title)
	}
	// One statement is not "1 statements".
	_, sub, ok = txCopy(state(dbclient.TxModeManual, true, nil, 1))
	if !ok || !strings.HasPrefix(sub, "1 statement · ") {
		t.Errorf("%s", sub)
	}
	// A run that failed on its first statement leaves a transaction
	// with nothing in it, and the line says so rather than "0".
	_, sub, ok = txCopy(state(dbclient.TxModeManual, true, nil, 0))
	if !ok || !strings.HasPrefix(sub, "nothing has landed in it yet") {
		t.Errorf("%s", sub)
	}
}

// What happens *next* differs by mode, so the line after a commit
// cannot be one sentence for both: in auto mode nothing opens another
// transaction.
func TestTheLineAfterACommitSaysWhatTheModeDoesNext(t *testing.T) {
	ended := func(mode dbclient.TxMode, done dbclient.TxEnd) TxState {
		return TxState{
			mode:       mode,
			open:       false,
			done:       &done,
			statements: 0,
			affected:   0,
			running:    false,
			ending:     false,
		}
	}
	title, sub, ok := txCopy(ended(dbclient.TxModeManual, dbclient.TxEndCommit))
	if !ok || title != "committed" {
		t.Errorf("title = %q", title)
	}
	if !strings.HasSuffix(sub, "a new transaction opens on your next run") {
		t.Errorf("%s", sub)
	}
	_, sub, ok = txCopy(ended(dbclient.TxModeAuto, dbclient.TxEndCommit))
	if !ok || !strings.HasSuffix(sub, "statements commit on their own again") {
		t.Errorf("%s", sub)
	}
	title, sub, ok = txCopy(ended(dbclient.TxModeAuto, dbclient.TxEndRollback))
	if !ok || title != "rolled back" {
		t.Errorf("title = %q", title)
	}
	if !strings.Contains(sub, "nothing was written") {
		t.Errorf("%s", sub)
	}
}

// A run holds the one connection the transaction is on, so the bar
// says which key clears the way rather than offering a button that
// would have to queue behind the run.
func TestARunInFlightIsNamedInTheBar(t *testing.T) {
	state := TxState{
		mode:       dbclient.TxModeManual,
		open:       true,
		done:       nil,
		statements: 3,
		affected:   0,
		running:    true,
		ending:     false,
	}
	_, sub, ok := txCopy(state)
	if !ok || !strings.Contains(sub, "⌘.") {
		t.Errorf("%s", sub)
	}
	ending := state
	ending.running = false
	ending.ending = true
	_, sub, ok = txCopy(ending)
	if !ok || !strings.Contains(sub, "waiting for the server") {
		t.Errorf("%s", sub)
	}
}

// The shape a VPN puts the app in, and the reason the split exists: a
// third of a second of waiting, of which the database did twelve
// milliseconds' work. One number could not have said that.
func TestTheServerAndTheLagAreReportedApart(t *testing.T) {
	timing := newTiming(
		340,
		dbclient.Wire{
			LinkMs:     shellU64(33),
			FirstRowMs: shellU64(46),
			FetchMs:    shellU64(294),
		},
	)
	plan := 0.6
	timing.server = &dbclient.ServerTiming{
		ExecMs: 12.4,
		PlanMs: &plan,
		Exact:  true,
	}
	if got, want := timing.summary(), "queried in 340 ms · server 13 ms · lag 327 ms"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Until the server answers — and for ever without
// `pg_stat_statements` — the estimate stands, and `~` says it is one.
func TestAnEstimatedServerTimeIsMarked(t *testing.T) {
	timing := newTiming(
		340,
		dbclient.Wire{
			LinkMs:     shellU64(33),
			FirstRowMs: shellU64(46),
			FetchMs:    shellU64(294),
		},
	)
	// The same numbers as the measured run above, and only the `~`
	// between them: the mark is the whole of what says which is which.
	if got, want := timing.summary(), "queried in 340 ms · server ~13 ms · lag 327 ms"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The estimate is time-to-first-row **less the round trip**. Leaving
// the round trip in would charge the database for the link, which is
// the mistake the whole split is here to fix.
func TestTheEstimateTakesTheRoundTripOff(t *testing.T) {
	far := newTiming(
		400,
		dbclient.Wire{
			LinkMs:     shellU64(120),
			FirstRowMs: shellU64(132),
			FetchMs:    shellU64(268),
		},
	)
	near := newTiming(
		280,
		dbclient.Wire{
			LinkMs:     shellU64(1),
			FirstRowMs: shellU64(13),
			FetchMs:    shellU64(267),
		},
	)
	// The same server work, seen down two very different links.
	if ms, _, ok := far.serverMs(); !ok || ms != 12. {
		t.Errorf("far.server_ms() = (%v, %v), want Some(12)", ms, ok)
	}
	if ms, _, ok := near.serverMs(); !ok || ms != 12. {
		t.Errorf("near.server_ms() = (%v, %v), want Some(12)", ms, ok)
	}
}

// A server figure larger than the wall clock is possible — the two are
// measured by different clocks, and the server's covers work that
// overlapped the fetch. The lag floors at zero rather than going
// negative, which would read as the link giving time back.
func TestTheLagNeverGoesNegative(t *testing.T) {
	timing := newTiming(10, dbclient.Wire{})
	timing.server = &dbclient.ServerTiming{
		ExecMs: 14.,
		PlanMs: nil,
		Exact:  true,
	}
	if lag, ok := timing.lagMs(); !ok || lag != 0. {
		t.Errorf("lag_ms() = (%v, %v), want Some(0)", lag, ok)
	}
}

// A local file has no link to separate out, so the engine offers
// neither half and the strip says only what it waited.
func TestAnEngineWithNoSplitReportsTheTotalAlone(t *testing.T) {
	timing := newTiming(7, dbclient.Wire{})
	if _, _, ok := timing.serverMs(); ok {
		t.Error("server_ms() is not None")
	}
	if got, want := timing.summary(), "queried in 7 ms"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func shellRelation(name string, kind introspect.TableKind) introspect.Table {
	return introspect.Table{
		Name:       name,
		Kind:       kind,
		Columns:    nil,
		PrimaryKey: nil,
		ApproxRows: nil,
	}
}

func shellNames(names ...string) []string {
	return append([]string{}, names...)
}

// ⌘W closes a tab while there is another one to stay on, and goes back
// to the connections screen, forgetting the tab, once there is not.
func TestTheLastTabGoesBackToConnections(t *testing.T) {
	for _, c := range []struct {
		tabs int
		want Close
	}{
		{3, Close{Kind: CloseKindTab, Tab: 7}},
		{2, Close{Kind: CloseKindTab, Tab: 7}},
		{1, Close{Kind: CloseKindShell, Tab: 7}},
		// A strip that is already empty has no × and no active tab, so
		// this is unreachable — it leaves, and forgets nothing.
		{0, Close{Kind: CloseKindShell}},
	} {
		if got := closeIntent(c.tabs, 7); got != c.want {
			t.Errorf("close_intent(%d, 7) = %+v, want %+v", c.tabs, got, c.want)
		}
	}
}

// Each way out names itself and names what it ends. The four must
// not drift into saying four unrelated things, which is the whole
// reason the copy is one function.
func TestTheConfirmationNamesTheWayOutItIsGuarding(t *testing.T) {
	one := shellNames("query 3")
	var none []string

	copy := confirmCopy(Close{Kind: CloseKindTab, Tab: 7}, one, none)
	if copy.Title != "Close “query 3”?" {
		t.Errorf("title = %q", copy.Title)
	}
	if !strings.Contains(copy.Body[0], "stop it") {
		t.Errorf("%v", copy.Body)
	}
	if copy.Keep != "Keep tab" || copy.Act != "Close it" {
		t.Errorf("(%q, %q)", copy.Keep, copy.Act)
	}

	copy = confirmCopy(Close{Kind: CloseKindShell}, one, none)
	if copy.Title != "Leave this connection?" {
		t.Errorf("title = %q", copy.Title)
	}
	if copy.Keep != "Stay" || copy.Act != "Leave anyway" {
		t.Errorf("(%q, %q)", copy.Keep, copy.Act)
	}

	// Closing one window is not quitting, now that there can be more
	// than one of them: the window's own question says so.
	copy = confirmCopy(Close{Kind: CloseKindWindow}, one, none)
	if copy.Title != "Close this window?" {
		t.Errorf("title = %q", copy.Title)
	}
	if copy.Act != "Close anyway" {
		t.Errorf("act = %q", copy.Act)
	}

	copy = confirmCopy(Close{Kind: CloseKindQuit}, one, none)
	if copy.Title != "Quit Meerkat?" {
		t.Errorf("title = %q", copy.Title)
	}
	if copy.Act != "Quit anyway" {
		t.Errorf("act = %q", copy.Act)
	}

	// A tab with only a transaction open is still named in the title:
	// the dialog must say which tab it is asking about.
	copy = confirmCopy(Close{Kind: CloseKindTab, Tab: 7}, none, one)
	if copy.Title != "Close “query 3”?" {
		t.Errorf("title = %q", copy.Title)
	}
	if want := []string{"“query 3” has a transaction open. Closing the tab rolls it back."}; !slices.Equal(copy.Body, want) {
		t.Errorf("body = %q, want %q", copy.Body, want)
	}
}

// Leaving with one run out names it; leaving with several counts
// them, and the dialog lists the names underneath.
func TestTheConfirmationCountsWhatItWouldEnd(t *testing.T) {
	var none []string

	body := confirmCopy(Close{Kind: CloseKindQuit}, shellNames("query 3"), none).Body
	if want := []string{"“query 3” is still running a statement. Quitting asks the server to stop it."}; !slices.Equal(body, want) {
		t.Errorf("body = %q, want %q", body, want)
	}

	body = confirmCopy(Close{Kind: CloseKindShell}, shellNames("query 3", "query 7"), none).Body
	if want := []string{"2 tabs are still running statements. Leaving asks the server to stop them."}; !slices.Equal(body, want) {
		t.Errorf("body = %q, want %q", body, want)
	}

	body = confirmCopy(Close{Kind: CloseKindShell}, none, shellNames("a", "b", "c")).Body
	if want := []string{"3 tabs have transactions open. Leaving rolls them back."}; !slices.Equal(body, want) {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// A tab can be both, and then the dialog says both — they are lost in
// different ways, so one line cannot stand for the other.
func TestTheConfirmationSaysBothWhenBothAreTrue(t *testing.T) {
	one := shellNames("query 3")
	copy := confirmCopy(Close{Kind: CloseKindTab, Tab: 7}, one, one)
	if len(copy.Body) != 2 {
		t.Fatalf("%q", copy.Body)
	}
	if !strings.Contains(copy.Body[0], "asks the server to stop it") {
		t.Errorf("%q", copy.Body)
	}
	if !strings.Contains(copy.Body[1], "rolls it back") {
		t.Errorf("%q", copy.Body)
	}
	if copy.Act != "Close it" {
		t.Errorf("act = %q", copy.Act)
	}
}

func shellSession(tabID uint64, idleSecs uint64) SessionState {
	return SessionState{
		tabID:         tabID,
		running:       false,
		inTransaction: false,
		idle:          time.Duration(idleSecs) * time.Second,
	}
}

func TestTheSweepTakesBackWhatHasGoneUnused(t *testing.T) {
	states := []SessionState{shellSession(1, 900), shellSession(2, 60), shellSession(3, 601)}
	if got := idleSessions(states, idleSession); !slices.Equal(got, []uint64{1, 3}) {
		t.Errorf("idle_sessions = %v, want [1 3]", got)
	}
	// Ten minutes on the nose is not yet ten minutes past.
	if got := idleSessions([]SessionState{shellSession(1, 599)}, idleSession); len(got) != 0 {
		t.Errorf("idle_sessions = %v, want []", got)
	}
}

// The two states a session is never taken in. A run is using it; a
// transaction would be rolled away with nobody asking, which is the
// thing the close dialog exists to refuse to do quietly.
func TestTheSweepNeverTakesABusyOrTransactingSession(t *testing.T) {
	running := shellSession(1, 9000)
	running.running = true
	holding := shellSession(2, 9000)
	holding.inTransaction = true
	if got := idleSessions([]SessionState{running, holding}, idleSession); len(got) != 0 {
		t.Errorf("idle_sessions = %v, want []", got)
	}
	if got, ok := evictable([]SessionState{running, holding}); ok {
		t.Errorf("evictable = Some(%d), want None", got)
	}
}

// The cap gives back the session that has gone longest without a
// question — and refuses rather than pick one that is holding
// something, because both ways out of that cost the user something
// the app may not choose for them.
func TestTheCapGivesBackTheLongestIdleSession(t *testing.T) {
	states := []SessionState{shellSession(1, 30), shellSession(2, 300), shellSession(3, 120)}
	if got, ok := evictable(states); !ok || got != 2 {
		t.Errorf("evictable = (%d, %v), want Some(2)", got, ok)
	}

	held := []SessionState{shellSession(1, 9000), shellSession(2, 9000)}
	held[0].inTransaction = true
	held[1].running = true
	if got, ok := evictable(held); ok {
		t.Errorf("evictable = Some(%d), want None", got)
	}
}

func TestWalkingTheTabsWrapsAtBothEnds(t *testing.T) {
	for _, c := range []struct {
		len, from int
		forward   bool
		want      int
	}{
		{3, 0, true, 1},
		{3, 1, true, 2},
		// Past the last tab is the first one again.
		{3, 2, true, 0},
		// ⌃⇧⇥ walks the other way, and wraps as well.
		{3, 0, false, 2},
		{3, 2, false, 1},
	} {
		if got := stepWrapping(c.len, c.from, c.forward); got != c.want {
			t.Errorf("step_wrapping(%d, %d, %v) = %d, want %d", c.len, c.from, c.forward, got, c.want)
		}
	}
}

func TestOneTabOrNoneStaysPut(t *testing.T) {
	for _, c := range []struct {
		len, from int
		forward   bool
	}{{0, 0, true}, {1, 0, true}, {1, 0, false}} {
		if got := stepWrapping(c.len, c.from, c.forward); got != 0 {
			t.Errorf("step_wrapping(%d, %d, %v) = %d, want 0", c.len, c.from, c.forward, got)
		}
	}
}

// shellRead is the rows as the sidebar draws them: a schema in `[]`, a
// section in `()`, both with their count and whether they are open, and a
// relation as its own name.
func shellRead(rows []CatalogRow) []string {
	shown := func(count int, open bool) string {
		if open {
			return strconv.Itoa(count) + " open"
		}
		return strconv.Itoa(count)
	}
	var out []string
	for _, row := range rows {
		switch row := row.(type) {
		case CatalogRowSchema:
			out = append(out, "["+row.label+" "+shown(row.count, row.open)+"]")
		case CatalogRowSection:
			out = append(out, "("+row.label+" "+shown(row.count, row.open)+")")
		case CatalogRowRelation:
			if row.open {
				out = append(out, row.name+" ▾")
			} else {
				out = append(out, row.name)
			}
		case CatalogRowLeaf:
			out = append(out, "- "+row.name+": "+row.detail)
		}
	}
	return out
}

// shellRows is `catalog_rows` with every set at its starting state.
func shellRows(groups []Group, needle string) []CatalogRow {
	return catalogRows(
		groups,
		map[string]struct{}{},
		map[string]struct{}{},
		map[string]struct{}{},
		needle,
	)
}

func shellSet(keys ...string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}

func shellSample() *introspect.Catalog {
	return &introspect.Catalog{
		Schemas: []introspect.Schema{
			{
				Name: "public",
				Tables: []introspect.Table{
					shellRelation("users", introspect.TableKindTable),
					shellRelation("active_users", introspect.TableKindView),
					shellRelation("orders", introspect.TableKindTable),
				},
			},
			// A schema of views only gets a `Views` section, not an
			// empty `Tables` one above it.
			{
				Name:   "reporting",
				Tables: []introspect.Table{shellRelation("daily", introspect.TableKindView)},
			},
		},
	}
}

func shellAssertRows(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("rows =\n  %q\nwant\n  %q", got, want)
	}
}

func TestASchemaHoldsATablesSectionAndAViewsSection(t *testing.T) {
	groups := catalogGroups(shellSample())
	var read []string
	for _, group := range groups {
		read = append(read, group.label)
	}
	shellAssertRows(t, read, "public", "reporting")

	var sections []string
	for _, section := range groups[0].sections {
		sections = append(sections, section.label)
	}
	shellAssertRows(t, sections, "Tables", "Views")
	var names []string
	for _, entry := range groups[0].sections[0].entries {
		names = append(names, entry.Name())
	}
	shellAssertRows(t, names, "users", "orders")
	// A schema of views only carries the one section.
	if len(groups[1].sections) != 1 {
		t.Errorf("reporting has %d sections, want 1", len(groups[1].sections))
	}
}

func TestEverySchemaStartsClosed(t *testing.T) {
	groups := catalogGroups(shellSample())
	// The schemas and their totals, and nothing under them.
	shellAssertRows(t, shellRead(shellRows(groups, "")), "[public 3]", "[reporting 1]")
}

func TestAnOpenSchemaShowsItsSectionsAndTheirRelations(t *testing.T) {
	groups := catalogGroups(shellSample())
	open := shellSet(groups[0].key)
	// A section is open until it is closed: opening the schema is one
	// click, not three.
	shellAssertRows(t,
		shellRead(catalogRows(groups, open, shellSet(), shellSet(), "")),
		"[public 3 open]",
		"(Tables 2 open)",
		"users",
		"orders",
		"(Views 1 open)",
		"active_users",
		"[reporting 1]",
	)

	// Closing a section leaves its header and takes its relations.
	closed := shellSet(groups[0].sections[0].key)
	shellAssertRows(t,
		shellRead(catalogRows(groups, open, closed, shellSet(), "")),
		"[public 3 open]",
		"(Tables 2)",
		"(Views 1 open)",
		"active_users",
		"[reporting 1]",
	)
}

func TestTheFilterOpensWhatItMatched(t *testing.T) {
	groups := catalogGroups(shellSample())
	// A relation name: only what holds a hit survives, open however
	// the two sets stand, and the counts are of the hits.
	shellAssertRows(t,
		shellRead(shellRows(groups, "user")),
		"[public 2 open]",
		"(Tables 1 open)",
		"users",
		"(Views 1 open)",
		"active_users",
	)
	// A schema's own name keeps everything under it.
	shellAssertRows(t,
		shellRead(shellRows(groups, "report")),
		"[reporting 1 open]", "(Views 1 open)", "daily",
	)
	// No hit anywhere is an empty list, not a list of empty headers.
	if rows := shellRows(groups, "nothing"); len(rows) != 0 {
		t.Errorf("rows = %q, want none", shellRead(rows))
	}
}

func TestTheFilterPutsItsClosestHitFirst(t *testing.T) {
	catalog := &introspect.Catalog{
		Schemas: []introspect.Schema{{
			Name: "app",
			Tables: []introspect.Table{
				shellRelation("correspondence_master", introspect.TableKindTable),
				shellRelation("master_assignment", introspect.TableKindTable),
				shellRelation("master", introspect.TableKindTable),
				shellRelation("master_rate", introspect.TableKindTable),
			},
		}},
	}
	groups := catalogGroups(catalog)
	// Alphabetical order buries the name the query says outright, so
	// the filtered section is ranked instead: the whole word first,
	// then the names it starts, shortest first, then the name that
	// only holds it further in.
	shellAssertRows(t,
		shellRead(shellRows(groups, "master")),
		"[app 4 open]",
		"(Tables 4 open)",
		"master",
		"master_rate",
		"master_assignment",
		"correspondence_master",
	)
	// With no filter the catalog's own order stands.
	open := shellSet(groups[0].key)
	shellAssertRows(t,
		shellRead(catalogRows(groups, open, shellSet(), shellSet(), ""))[2:],
		"correspondence_master",
		"master_assignment",
		"master",
		"master_rate",
	)
}

func TestADotInTheFilterNamesAPath(t *testing.T) {
	groups := catalogGroups(shellSample())
	// Both parts must hit: the schema, then the relation under it.
	shellAssertRows(t,
		shellRead(shellRows(groups, "public.orders")),
		"[public 1 open]", "(Tables 1 open)", "orders",
	)
	// Part of each is enough, as in the palette.
	shellAssertRows(t,
		shellRead(shellRows(groups, "rep.dai")),
		"[reporting 1 open]", "(Views 1 open)", "daily",
	)
	// A trailing dot asks for everything the schema holds.
	shellAssertRows(t,
		shellRead(shellRows(groups, "public.")),
		"[public 3 open]",
		"(Tables 2 open)",
		"users",
		"orders",
		"(Views 1 open)",
		"active_users",
	)
	// The schema of the hit is part of the query, so a relation of the
	// right name under the wrong schema is not an answer.
	if rows := shellRows(groups, "reporting.orders"); len(rows) != 0 {
		t.Errorf("rows = %q, want none", shellRead(rows))
	}
	// Case never matters, on either side of the dot.
	shellAssertRows(t,
		shellRead(shellRows(groups, "PUBLIC.Orders")),
		"[public 1 open]", "(Tables 1 open)", "orders",
	)
}

func TestPasswordsNeverReachTheScreen(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"postgres://ada:hunter2@db.internal:5432/app", "postgres://ada:•••@db.internal:5432/app"},
		{"postgres://ada@db.internal/app", "postgres://ada@db.internal/app"},
		{"postgres:///app", "postgres:///app"},
	} {
		if got := redact(c.in); got != c.want {
			t.Errorf("redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func shellAnswered(columns []string, rows int) *dbclient.QueryResult {
	result := &dbclient.QueryResult{Columns: columns}
	for i := 0; i < rows; i++ {
		result.Rows = append(result.Rows, []dbclient.Value{})
	}
	return result
}

func shellCounted(rowsAffected uint64) *dbclient.QueryResult {
	return &dbclient.QueryResult{RowsAffected: rowsAffected}
}

// The statistic beside a statement reads as the comp's lines do, and
// the rule is the result line's: columns say there was a result set,
// never the count.
func TestAStatementsStatisticSaysWhatItAnsweredWith(t *testing.T) {
	alter := query.DdlVerbAlter
	for _, c := range []struct{ got, want string }{
		{statementMeta(nil, "select 1", shellAnswered([]string{"a"}, 6), 34), "6 rows · 34 ms"},
		{statementMeta(nil, "select 1", shellAnswered([]string{"a"}, 1), 3), "1 row · 3 ms"},
		{statementMeta(nil, "select 1 where false", shellAnswered([]string{"a"}, 0), 3), "no rows · 3 ms"},
		{statementMeta(nil, "delete from t", shellCounted(1204), 61), "1.2k rows deleted · 61 ms"},
		{statementMeta(nil, "update t set a = 1", shellCounted(0), 2), "no rows updated · 2 ms"},
		{statementMeta(nil, "set search_path to x", shellCounted(0), 1), "done · 1 ms"},
		{statementMeta(
			nil,
			"with m as (delete from t returning *) insert into u select * from m",
			shellCounted(7),
			9,
		), "7 rows affected · 9 ms"},
		// A shape-changing statement takes its verb: the count the server
		// reports for it is nothing, and the panel says what changed.
		{statementMeta(
			&alter,
			"alter table t add c int",
			shellCounted(0),
			12,
		), "altered · 12 ms"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func shellAColumn(name string) introspect.Column {
	return introspect.Column{
		Name:     name,
		DataType: "text",
		Nullable: true,
		Default:  nil,
	}
}

func shellAltered(columns []introspect.ColumnDelta) *introspect.Delta {
	return &introspect.Delta{
		Relations: []introspect.RelationDelta{{
			Schema: "public",
			Name:   "t",
			Kind:   introspect.TableKindTable,
			Change: introspect.RelationAltered{Columns: columns},
		}},
	}
}

func shellAssertMeta(t *testing.T, got string, ok bool, want *string) {
	t.Helper()
	switch {
	case want == nil && ok:
		t.Errorf("got Some(%q), want None", got)
	case want != nil && (!ok || got != *want):
		t.Errorf("got (%q, %v), want Some(%q)", got, ok, *want)
	}
}

func TestAShapeChangingStatementsLineCountsTheColumnsOnceTheyAreKnown(t *testing.T) {
	delta := shellAltered([]introspect.ColumnDelta{
		introspect.ColumnAdded{Column: shellAColumn("a")},
		introspect.ColumnAdded{Column: shellAColumn("b")},
		introspect.ColumnDropped{Column: shellAColumn("c")},
	})
	got, ok := ddlMeta(delta, query.DdlVerbAlter, 12)
	shellAssertMeta(t, got, ok, shellStr("altered · +2 −1 columns · 12 ms"))
	got, ok = ddlMeta(
		shellAltered([]introspect.ColumnDelta{introspect.ColumnAdded{Column: shellAColumn("a")}}),
		query.DdlVerbAlter,
		5,
	)
	shellAssertMeta(t, got, ok, shellStr("altered · +1 column · 5 ms"))
	// A table that came or went is the header's news, not the line's,
	// and nothing changed is nothing to add.
	got, ok = ddlMeta(&introspect.Delta{}, query.DdlVerbCreate, 5)
	shellAssertMeta(t, got, ok, nil)
}

// The one sentence a shape change is owed: whether it can still be
// taken back.
func TestTheSchemaNoteSaysWhetherTheChangeCanBeTakenBack(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{schemaNote(true, nil), "held in the open transaction — rollback takes it back, commit makes it permanent"},
		{schemaNote(true, []string{"t.legacy_plan"}), "held in the open transaction — rollback puts t.legacy_plan back, " +
			"commit makes the drop permanent"},
		{schemaNote(false, nil), "committed on its own"},
		{schemaNote(false, []string{"t.legacy_plan"}), "committed on its own — t.legacy_plan is gone and cannot be recovered from here"},
		{schemaNote(false, []string{"a", "b"}), "committed on its own — a, b are gone and cannot be recovered from here"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestTheReportHeaderNamesOneRelationOrCountsThem(t *testing.T) {
	if got := reportVerbs([]ddlAt{{0, query.DdlVerbAlter}, {2, query.DdlVerbAlter}}); got != "altered" {
		t.Errorf("report_verbs = %q", got)
	}
	if got := reportVerbs([]ddlAt{{0, query.DdlVerbCreate}, {1, query.DdlVerbDrop}}); got != "created, dropped" {
		t.Errorf("report_verbs = %q", got)
	}
	subject, reveal := reportSubject(shellAltered(nil))
	if subject == nil || *subject != "public.t" {
		t.Errorf("subject = %v", subject)
	}
	if reveal == nil || *reveal != [2]string{"public", "t"} {
		t.Errorf("reveal = %v", reveal)
	}
	// A dropped relation is not there to reveal.
	gone := &introspect.Delta{
		Relations: []introspect.RelationDelta{{
			Schema: "public",
			Name:   "t",
			Kind:   introspect.TableKindTable,
			Change: introspect.RelationDropped{Columns: nil},
		}},
	}
	if _, reveal := reportSubject(gone); reveal != nil {
		t.Errorf("reveal = %v, want None", *reveal)
	}
	if subject, reveal := reportSubject(&introspect.Delta{}); subject != nil || reveal != nil {
		t.Errorf("report_subject(empty) = (%v, %v), want (None, None)", subject, reveal)
	}
}

// shellFurnished is a schema with a table that carries one of
// everything, and a sequence, a routine and a type beside it.
func shellFurnished() *introspect.Catalog {
	orders := shellRelation("orders", introspect.TableKindTable)
	orders.Columns = []introspect.Column{
		{
			Name:     "id",
			DataType: "bigint",
			Nullable: false,
			Default:  nil,
		},
		shellAColumn("user_id"),
	}
	orders.PrimaryKey = []string{"id"}
	orders.Indexes = []introspect.Index{{
		Name:       "orders_pkey",
		Definition: "btree (id)",
		Unique:     true,
		Primary:    true,
	}}
	orders.ForeignKeys = []introspect.ForeignKey{{
		Name:          "orders_user_id_fkey",
		Columns:       []string{"user_id"},
		TargetSchema:  "public",
		TargetTable:   "users",
		TargetColumns: []string{"id"},
	}}
	return &introspect.Catalog{
		Schemas: []introspect.Schema{{
			Name:   "public",
			Tables: []introspect.Table{orders},
			Sequences: []introspect.Sequence{{
				Name:     "ticket",
				DataType: "integer",
			}},
			Routines: []introspect.Routine{{
				Name:      "total",
				Kind:      introspect.RoutineKindFunction,
				Arguments: "integer",
				Returns:   shellStr("bigint"),
			}},
			Types: []introspect.UserType{{
				Name:       "mood",
				Kind:       introspect.TypeKindEnum,
				Definition: "sad, ok",
			}},
		}},
	}
}

func TestASchemaListsItsSequencesRoutinesAndTypesAfterItsRelations(t *testing.T) {
	groups := catalogGroups(shellFurnished())
	open := shellSet(groups[0].key)
	shellAssertRows(t,
		shellRead(catalogRows(groups, open, shellSet(), shellSet(), "")),
		"[public 4 open]",
		"(Tables 1 open)",
		"orders",
		"(Sequences 1 open)",
		"- ticket: integer",
		"(Routines 1 open)",
		"- total: (integer) → bigint",
		"(Types 1 open)",
		"- mood: enum · sad, ok",
	)
}

func TestARelationFoldsOutIntoItsColumnsAndItsObjects(t *testing.T) {
	groups := catalogGroups(shellFurnished())
	open := shellSet(groups[0].key)
	folded := shellSet(relationKey("public", "orders"))
	rows := catalogRows(groups, open, shellSet(), folded, "")
	shellAssertRows(t,
		shellRead(rows)[:8],
		"[public 4 open]",
		"(Tables 1 open)",
		"orders ▾",
		// The columns come straight away, with no folder of their
		// own: they are what a relation is opened to read.
		"- id: bigint",
		"- user_id: text",
		"(Indexes 1 open)",
		"- orders_pkey: primary · btree (id)",
		"(Foreign keys 1 open)",
	)
	// The key's own column carries the key's mark.
	if leaf, ok := rows[3].(CatalogRowLeaf); !ok || leaf.mark != MarkKey || leaf.level != 3 {
		t.Errorf("rows[3] = %#v, want a key leaf at level 3", rows[3])
	}
	if leaf, ok := rows[4].(CatalogRowLeaf); !ok || leaf.mark != MarkColumn {
		t.Errorf("rows[4] = %#v, want a column leaf", rows[4])
	}

	// A relation's folder closes like any section, by its own key.
	closed := shellSet("public\torders\tIndexes")
	rows = catalogRows(groups, open, closed, folded, "")
	shellAssertRows(t, shellRead(rows)[5:7], "(Indexes 1)", "(Foreign keys 1 open)")
}

func TestTheFilterFindsARoutineButDoesNotFoldARelationOut(t *testing.T) {
	groups := catalogGroups(shellFurnished())
	shellAssertRows(t,
		shellRead(shellRows(groups, "total")),
		"[public 1 open]",
		"(Routines 1 open)",
		"- total: (integer) → bigint",
	)
	// A relation the filter found is listed, not folded out: the hit
	// is its name.
	shellAssertRows(t,
		shellRead(shellRows(groups, "orders")),
		"[public 1 open]", "(Tables 1 open)", "orders",
	)
	// A leaf is not somewhere ⏎ can go, so the cursor never lands on
	// one: the only stop is still the relation.
	folded := shellSet(relationKey("public", "orders"))
	found := catalogRows(
		groups,
		shellSet(),
		shellSet(),
		folded,
		"public.",
	)
	if len(found) <= 10 {
		t.Errorf("the relation is folded out: %d rows", len(found))
	}
	if got := catalogStops(found); !slices.Equal(got, []int{2}) {
		t.Errorf("catalog_stops = %v, want [2]", got)
	}
}

func shellAnObject(kind introspect.ObjectKind, name string, change introspect.ObjectChange) introspect.ObjectDelta {
	return introspect.ObjectDelta{
		Schema:   "public",
		Relation: shellStr("t"),
		Kind:     kind,
		Name:     name,
		Change:   change,
	}
}

func TestAStatementThatMovedNoColumnCountsItsObjects(t *testing.T) {
	added := func() introspect.ObjectChange {
		return introspect.ObjectAdded{Detail: "btree (a)"}
	}
	one := &introspect.Delta{
		Objects: []introspect.ObjectDelta{shellAnObject(introspect.ObjectKindIndex, "t_a_idx", added())},
	}
	got, ok := ddlMeta(one, query.DdlVerbCreate, 7)
	shellAssertMeta(t, got, ok, shellStr("created · +1 index · 7 ms"))
	two := &introspect.Delta{
		Objects: []introspect.ObjectDelta{
			shellAnObject(introspect.ObjectKindIndex, "t_a_idx", added()),
			shellAnObject(introspect.ObjectKindIndex, "t_b_idx", added()),
		},
	}
	got, ok = ddlMeta(two, query.DdlVerbCreate, 7)
	shellAssertMeta(t, got, ok, shellStr("created · +2 indexes · 7 ms"))
	// A script that mixed them says objects.
	mixed := &introspect.Delta{
		Objects: []introspect.ObjectDelta{
			shellAnObject(introspect.ObjectKindIndex, "t_a_idx", added()),
			shellAnObject(
				introspect.ObjectKindTrigger,
				"audit",
				introspect.ObjectDropped{Detail: ""},
			),
		},
	}
	got, ok = ddlMeta(mixed, query.DdlVerbAlter, 7)
	shellAssertMeta(t, got, ok, shellStr("altered · +1 −1 objects · 7 ms"))
	// What came with a new table is the table's, and not counted.
	created := &introspect.Delta{
		Relations: []introspect.RelationDelta{{
			Schema: "public",
			Name:   "t",
			Kind:   introspect.TableKindTable,
			Change: introspect.RelationAdded{Columns: nil},
		}},
	}
	created.Objects = slices.Clone(one.Objects)
	got, ok = ddlMeta(created, query.DdlVerbCreate, 7)
	shellAssertMeta(t, got, ok, nil)
}

func TestTheReportHeaderNamesOneObjectAndRevealsItsRelation(t *testing.T) {
	delta := &introspect.Delta{
		Objects: []introspect.ObjectDelta{shellAnObject(
			introspect.ObjectKindIndex,
			"t_a_idx",
			introspect.ObjectAdded{Detail: "btree (a)"},
		)},
	}
	subject, reveal := reportSubject(delta)
	if subject == nil || *subject != "index t.t_a_idx" {
		t.Errorf("subject = %v", subject)
	}
	if reveal == nil || *reveal != [2]string{"public", "t"} {
		t.Errorf("reveal = %v", reveal)
	}

	// A table and its index together are two changes.
	both := shellAltered([]introspect.ColumnDelta{introspect.ColumnAdded{Column: shellAColumn("a")}})
	both.Objects = slices.Clone(delta.Objects)
	if subject, _ := reportSubject(both); subject == nil || *subject != "2 changes" {
		t.Errorf("subject = %v", subject)
	}
}

func TestTheMessagesStripListsSixAndCountsTheRest(t *testing.T) {
	shown, more := messageLines([]string{"NOTICE: a\nb", "NOTICE: c"})
	if len(shown) != 2 || shown[0] != "NOTICE: a b" || more != "" {
		t.Fatalf("shown = %q, more = %q", shown, more)
	}
	var many []string
	for i := 0; i < 9; i++ {
		many = append(many, "NOTICE: "+strconv.Itoa(i))
	}
	shown, more = messageLines(many)
	if len(shown) != messagesShown || more != "3 more" {
		t.Fatalf("shown = %q, more = %q", shown, more)
	}
}
