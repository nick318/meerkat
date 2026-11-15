package sqleditor

import "testing"

// applied applies an edit the way the editor does, and marks the selection
// with `[` and `]` so a test reads as the screen does.
func applied(t *testing.T, text string, edit *Edit) string {
	t.Helper()
	if edit == nil {
		t.Fatalf("an edit")
	}
	out := text[:edit.Range.Start] + edit.Text + text[edit.Range.End:]
	out = out[:edit.Selection.End] + "]" + out[edit.Selection.End:]
	out = out[:edit.Selection.Start] + "[" + out[edit.Selection.Start:]
	return out
}

func expectString(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func expectNoEdit(t *testing.T, edit *Edit) {
	t.Helper()
	if edit != nil {
		t.Errorf("got %+v, want None", *edit)
	}
}

func TestASelectionEndingAtALineStartLeavesThatLineOut(t *testing.T) {
	text := "one\ntwo\nthree"
	if got := lineSpan(text, Range{0, 4}); got != (Range{0, 3}) {
		t.Errorf("got %v", got)
	}
	if got := lineSpan(text, Range{1, 5}); got != (Range{0, 7}) {
		t.Errorf("got %v", got)
	}
	// A bare caret at a line's start is on that line.
	if got := lineSpan(text, Range{4, 4}); got != (Range{4, 7}) {
		t.Errorf("got %v", got)
	}
}

func TestIndentAndOutdentMoveEveryLineAndKeepTheSelection(t *testing.T) {
	text := "select\n\n  id"
	expectString(t, applied(t, text, indent(text, Range{2, len(text)}, "  ")), "  se[lect\n\n    id]")
	text = "  select\n id\n\tname\nx"
	expectString(t, applied(t, text, outdent(text, Range{0, len(text)}, "  ")), "[select\nid\nname\nx]")
	expectNoEdit(t, outdent("x", Range{0, 0}, "  "))
}

func TestACommentGoesOnAndComesOff(t *testing.T) {
	text := "select 1\n  from t"
	on := toggleComment(text, Range{0, len(text)})
	expectString(t, applied(t, text, on), "[-- select 1\n--   from t]")
	commented := text[:on.Range.Start] + on.Text + text[on.Range.End:]
	expectString(t, applied(t, commented, toggleComment(commented, Range{0, len(commented)})), "[select 1\n  from t]")
	// One live line in a block that is not all comments: comment it all.
	mixed := "-- a\nb"
	expectString(t, applied(t, mixed, toggleComment(mixed, Range{0, len(mixed)})), "[-- -- a\n-- b]")
	expectString(t, applied(t, "", toggleComment("", Range{0, 0})), "-- []")
	expectString(t, applied(t, "--x", toggleComment("--x", Range{3, 3})), "x[]")
}

func TestLinesMoveOverTheirNeighbour(t *testing.T) {
	text := "a\nbb\nc"
	expectString(t, applied(t, text, moveLines(text, Range{3, 3}, true)), "a\nc\nb[]b")
	expectString(t, applied(t, text, moveLines(text, Range{3, 3}, false)), "b[]b\na\nc")
	expectNoEdit(t, moveLines(text, Range{0, 0}, false))
	expectNoEdit(t, moveLines(text, Range{5, 5}, true))
	// A block of two travels together.
	expectString(t, applied(t, text, moveLines(text, Range{0, 3}, true)), "c\n[a\nb]b")
}

func TestADuplicateLeavesTheSelectionWhereTheUserWasLooking(t *testing.T) {
	text := "a\nb"
	expectString(t, applied(t, text, duplicateLines(text, Range{2, 3}, true)), "a\nb\n[b]")
	expectString(t, applied(t, text, duplicateLines(text, Range{2, 3}, false)), "a\n[b]\nb")
}

func TestADeletedLineTakesItsBreakWithIt(t *testing.T) {
	text := "one\ntwo\nthree"
	expectString(t, applied(t, text, deleteLines(text, Range{5, 5})), "one\nt[]hree")
	expectString(t, applied(t, text, deleteLines(text, Range{10, 10})), "one\ntw[]o")
	expectString(t, applied(t, "only", deleteLines("only", Range{2, 2})), "[]")
	expectNoEdit(t, deleteLines("", Range{0, 0}))
}

func TestJoinedLinesMeetAtOneSpace(t *testing.T) {
	text := "select  \n    id\nfrom t"
	expectString(t, applied(t, text, joinLines(text, Range{0, 0})), "select[] id\nfrom t")
	expectString(t, applied(t, text, joinLines(text, Range{0, len(text)})), "select id[] from t")
	expectString(t, applied(t, "a\n\nb", joinLines("a\n\nb", Range{0, 0})), "a[]\nb")
	expectNoEdit(t, joinLines("a", Range{0, 0}))
}

func TestTransposeSwapsAroundTheCaret(t *testing.T) {
	expectString(t, applied(t, "abc", transpose("abc", 1)), "ba[]c")
	// At the end of the line the two before the caret trade places.
	expectString(t, applied(t, "abc\nd", transpose("abc\nd", 3)), "acb[]\nd")
	expectNoEdit(t, transpose("abc", 0))
	expectNoEdit(t, transpose("a\nb", 2))
}
