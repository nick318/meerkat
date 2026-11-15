package sqleditor

import (
	"reflect"
	"testing"

	"github.com/nick318/meerkat/internal/fuzzy"
	"github.com/nick318/meerkat/internal/query"
)

func diagnostic(r Range) Diagnostic {
	return Diagnostic{
		Range:   r,
		Message: "syntax error",
	}
}

// The squiggle is cut out of the colour, not painted over it: the
// pieces still cover the line exactly once, in order.
func TestAMarkSplitsTheSpanItLandsIn(t *testing.T) {
	line := "select 1 frm users"
	list := spans(line, &Vocabulary{})
	pieces := splitSpans(list, []Range{{9, 12}})
	var marked []string
	for _, p := range pieces {
		if p.Marked {
			marked = append(marked, line[p.Range.Start:p.Range.End])
		}
	}
	if !reflect.DeepEqual(marked, []string{"frm"}) {
		t.Errorf("marked = %v", marked)
	}
	total := 0
	for _, p := range pieces {
		total += p.Range.End - p.Range.Start
	}
	if total != len(line) {
		t.Errorf("total %d, want %d", total, len(line))
	}
	for i := 0; i+1 < len(pieces); i++ {
		if pieces[i].Range.End != pieces[i+1].Range.Start {
			t.Errorf("pieces %d and %d do not meet: %v", i, i+1, pieces)
		}
	}
}

// The colour survives the cut. A keyword half inside a mark is still
// a keyword on both sides of it.
func TestASplitPieceKeepsItsColour(t *testing.T) {
	pieces := splitSpans(spans("select", &Vocabulary{}), []Range{{0, 3}})
	want := []piece{{Range{0, 3}, TokenKeyword, true}, {Range{3, 6}, TokenKeyword, false}}
	if !reflect.DeepEqual(pieces, want) {
		t.Errorf("got %v, want %v", pieces, want)
	}
}

func TestNothingFoundMarksNothing(t *testing.T) {
	list := spans("select 1", &Vocabulary{})
	for _, p := range splitSpans(list, nil) {
		if p.Marked {
			t.Errorf("marked %v", p)
		}
	}
}

// A statement runs over several lines, and each shaped line knows
// only its own bytes.
func TestAMarkIsCutToTheLineItFallsOn(t *testing.T) {
	// "select 1\nfrm users": line two starts at byte 9.
	diagnostics := []Diagnostic{diagnostic(Range{4, 12})}
	if got := marksOnLine(diagnostics, 0, 8); !reflect.DeepEqual(got, []Range{{4, 8}}) {
		t.Errorf("got %v", got)
	}
	if got := marksOnLine(diagnostics, 9, 9); !reflect.DeepEqual(got, []Range{{0, 3}}) {
		t.Errorf("got %v", got)
	}
	// A line the mark does not reach carries nothing.
	if got := marksOnLine(diagnostics, 19, 5); !reflect.DeepEqual(got, []Range{}) {
		t.Errorf("got %v", got)
	}
}

func mark(r Range, status StatementStatus) StatementMark {
	return StatementMark{
		Range:  r,
		Status: status,
	}
}

func on(ix int, status StatementStatus, first bool) *lineMark {
	return &lineMark{ix: ix, status: status, first: first}
}

// The teal is read off the text, before any run: a pasted script
// shows which of its statements change the shape of the database the
// moment it lands.
func TestShapeChangingStatementsAreFoundInTheBufferBeforeARun(t *testing.T) {
	text := "select 1;\nalter table t\n  add c int;\ndrop table u;\n"
	var editorShape []Range
	for _, r := range query.StatementRanges(text) {
		if _, ok := query.DdlVerbOf(text[r.Start:r.End]); ok {
			editorShape = append(editorShape, Range(r))
		}
	}
	if len(editorShape) != 2 {
		t.Fatalf("shape-changing statements = %d, want 2", len(editorShape))
	}
	want := []shapeMark{shapeNone, shapeFirst, shapeRest, shapeFirst, shapeNone}
	if got := lineShape(text, editorShape); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := kindAt(lineShape(text, editorShape), 0); got != StatementPlain {
		t.Errorf("row 0 = %v", got)
	}
	if got := kindAt(lineShape(text, editorShape), 2); got != StatementDdl {
		t.Errorf("row 2 = %v", got)
	}
}

func TestAStatementMarksEveryLineItCovers(t *testing.T) {
	text := "select 1;\nupdate t\n   set a = 1;\n"
	marks := []StatementMark{
		mark(Range{0, 8}, StatementDone),
		mark(Range{10, 31}, StatementRunning),
	}
	want := []*lineMark{
		// The mark itself sits on the first line of each.
		on(0, StatementDone, true),
		on(1, StatementRunning, true),
		on(1, StatementRunning, false),
		// The line past the last semicolon belongs to no statement.
		nil,
	}
	if got := lineMarks(text, marks); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAStatementEndingAtALineBreakDoesNotOpenTheNextLine(t *testing.T) {
	// `select 1` runs to offset 8, and offset 9 opens line two. A range
	// whose end is one past its last character must not reach there.
	marks := []StatementMark{mark(Range{0, 8}, StatementDone)}
	want := []*lineMark{on(0, StatementDone, true), nil}
	if got := lineMarks("select 1;\nselect 2;", marks); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNothingRunMarksNothing(t *testing.T) {
	want := []*lineMark{nil, nil}
	if got := lineMarks("select 1;\nselect 2;", nil); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAStaleRangeIsPulledBackIntoTheBuffer(t *testing.T) {
	text := "select 1"
	// The platform's copy of the buffer can be an edit ahead of ours.
	if got := clampRange(text, Range{40, 50}); got != (Range{8, 8}) {
		t.Errorf("got %v", got)
	}
	if got := clampRange(text, Range{2, 50}); got != (Range{2, 8}) {
		t.Errorf("got %v", got)
	}
	// Out of order comes back ordered rather than panicking on slice.
	if got := clampRange(text, Range{6, 2}); got != (Range{6, 6}) {
		t.Errorf("got %v", got)
	}
}

// The panel marks what the word actually matched, wherever in the name
// that landed — not the label's first characters, which is what a
// prefix match used to make the same thing.
func TestALabelIsCutIntoMatchedAndPlain(t *testing.T) {
	hit := fuzzy.Score("master_client_reference", "mast_cl")
	if hit == nil {
		t.Fatalf("matches")
	}
	want := []matchedPiece{
		{"mast", true},
		{"er_", false},
		{"cl", true},
		{"ient_reference", false},
	}
	if got := matchedSpans("master_client_reference", fromFuzzy(hit.Ranges)); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Nothing typed marks nothing, and the whole label stays one piece.
	if got := matchedSpans("users", nil); !reflect.DeepEqual(got, []matchedPiece{{"users", false}}) {
		t.Errorf("got %v", got)
	}
}

func TestClampingNeverSplitsACharacter(t *testing.T) {
	text := "héllo"
	// Byte 2 is inside the two-byte 'é'.
	if got := clampOffset(text, 2); got != 1 {
		t.Errorf("clampOffset(2) = %d, want 1", got)
	}
	if !isCharBoundary(text, clampOffset(text, 2)) {
		t.Errorf("clampOffset(2) splits a character")
	}
	for offset := 0; offset <= len(text)+4; offset++ {
		if !isCharBoundary(text, clampOffset(text, offset)) {
			t.Errorf("clampOffset(%d) splits a character", offset)
		}
	}
}

func TestCompositionSelectionIsMeasuredInsideTheInsertedText(t *testing.T) {
	// Two UTF-16 units into "héllo" is one code point plus one byte
	// of the two-byte one, so byte 3.
	if got := utf16ToByte("héllo", 2); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
	if got := utf16ToByte("abc", 0); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	// Past the end clamps rather than running off the buffer.
	if got := utf16ToByte("abc", 99); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}
