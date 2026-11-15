package sqleditor

import (
	"strings"
	"testing"
)

const motionSQL = "select id, name\nfrom public.users"

func TestWordMotionWalksWordByWord(t *testing.T) {
	// From the very end, back over: users . public from
	ix := len(motionSQL)
	var stops []string
	for range 6 {
		ix = previousWordStart(motionSQL, ix)
		stops = append(stops, motionSQL[ix:min(ix+6, len(motionSQL))])
	}
	if stops[0] != "users" {
		t.Errorf("stops[0] = %q", stops[0])
	}
	if stops[1] != "public" {
		t.Errorf("stops[1] = %q", stops[1])
	}
	if stops[2] != "from p" {
		t.Errorf("stops[2] = %q", stops[2])
	}
}

func TestWordMotionStopsAtALineBreak(t *testing.T) {
	endOfFirstLine := strings.IndexByte(motionSQL, '\n')
	// Forward from the end of "name" lands on the break, not past it.
	if got := nextWordEnd(motionSQL, endOfFirstLine); got != endOfFirstLine+1 {
		t.Errorf("nextWordEnd = %d, want %d", got, endOfFirstLine+1)
	}
	// Backward from the start of "from" lands on the break.
	if got := previousWordStart(motionSQL, endOfFirstLine+1); got != endOfFirstLine {
		t.Errorf("previousWordStart = %d, want %d", got, endOfFirstLine)
	}
}

func TestForwardWordMotionEndsOnTheWord(t *testing.T) {
	if got := motionSQL[:nextWordEnd(motionSQL, 0)]; got != "select" {
		t.Errorf("got %q", got)
	}
	afterSelect := nextWordEnd(motionSQL, 0)
	if got := motionSQL[:nextWordEnd(motionSQL, afterSelect)]; got != "select id" {
		t.Errorf("got %q", got)
	}
}

func TestADoubleClickTakesTheWholeWord(t *testing.T) {
	slice := func(text string, r Range) string { return text[r.Start:r.End] }
	// Middle of "public".
	ix := strings.Index(motionSQL, "public") + 2
	if got := slice(motionSQL, wordAt(motionSQL, ix)); got != "public" {
		t.Errorf("got %q", got)
	}
	// Between a word and the dot, the word to the left wins, the way
	// a caret at that offset belongs to the word behind it.
	dot := strings.IndexByte(motionSQL, '.')
	if got := slice(motionSQL, wordAt(motionSQL, dot)); got != "public" {
		t.Errorf("got %q", got)
	}
	// Away from any word, take just the character clicked.
	space := strings.IndexByte(motionSQL, ' ') + 3
	if got := slice(motionSQL, wordAt(motionSQL, space)); got != "id" {
		t.Errorf("got %q", got)
	}
	if got := slice("a  b", wordAt("a  b", 2)); got != " " {
		t.Errorf("got %q", got)
	}
}

func TestMotionNeverSplitsACharacter(t *testing.T) {
	text := "select 'héllo wörld' as x"
	ix := 0
	for ix < len(text) {
		ix = nextWordEnd(text, ix)
		if !isCharBoundary(text, ix) {
			t.Fatalf("split at %d", ix)
		}
	}
	for ix > 0 {
		ix = previousWordStart(text, ix)
		if !isCharBoundary(text, ix) {
			t.Fatalf("split at %d", ix)
		}
	}
}

func TestLinesBoundAtTheBreak(t *testing.T) {
	ix := strings.Index(motionSQL, "public")
	if got := motionSQL[lineStart(motionSQL, ix):lineEnd(motionSQL, ix)]; got != "from public.users" {
		t.Errorf("got %q", got)
	}
}

func TestTheLineStartStopsAtTheIndentFirst(t *testing.T) {
	text := "select\n    id"
	end := len(text)
	indent := strings.Index(text, "id")
	if got := smartLineStart(text, end); got != indent {
		t.Errorf("got %d, want %d", got, indent)
	}
	// A second press goes on to column zero, and a third comes back.
	if got := smartLineStart(text, indent); got != indent-4 {
		t.Errorf("got %d, want %d", got, indent-4)
	}
	if got := smartLineStart(text, indent-4); got != indent {
		t.Errorf("got %d, want %d", got, indent)
	}
	// A line with no indent has one stop.
	if got := smartLineStart(text, 3); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	if got := smartLineStart(text, 0); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestASubwordEndsAtAnUnderscoreOrAHump(t *testing.T) {
	text := "master_client_reference"
	check := func(got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	check(text[:nextSubwordEnd(text, 0)], "master")
	check(text[:nextSubwordEnd(text, 6)], "master_client")
	check(text[previousSubwordStart(text, len(text)):], "reference")
	check(text[previousSubwordStart(text, 14):14], "client_")
	camel := "masterClientRef"
	check(camel[:nextSubwordEnd(camel, 0)], "master")
	check(camel[previousSubwordStart(camel, len(camel)):], "Ref")
	check(camel[previousSubwordStart(camel, 12):12], "Client")
}

func TestAParagraphIsBoundedByBlankLines(t *testing.T) {
	text := "select 1;\n\nselect 2\nfrom t;\n\nselect 3;"
	inside := strings.Index(text, "from")
	if got := startOfParagraph(text, inside); got != 10 {
		t.Errorf("got %d, want 10", got)
	}
	if got, want := endOfParagraph(text, inside), strings.Index(text, "\n\nselect 3")+1; got != want {
		t.Errorf("got %d, want %d", got, want)
	}
	if got := startOfParagraph(text, 3); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	if got := endOfParagraph(text, len(text)-2); got != len(text) {
		t.Errorf("got %d, want %d", got, len(text))
	}
}
