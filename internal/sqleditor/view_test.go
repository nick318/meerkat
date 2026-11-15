package sqleditor

// Tester checks of the painted editor. They are extra to the Rust's tests
// (which are all pure), never a stand-in for one.

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/query"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

func TestMain(m *testing.M) {
	if err := uikit.RegisterFonts(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

const viewSQL = "select id, email\n  from public.users\n where id = 1;\nalter table users\n  add column seen_on date;\nselect 1 frm users;\nselect\tcount(*) from sessions;\n"

func viewEditor() *SqlEditor {
	vocabulary := NewVocabulary([]Name{
		{Name: "public", Detail: "schema", Kind: KindSchema},
		{Name: "users", Detail: "table", Kind: KindRelation, Owner: ptr("public")},
		{Name: "sessions", Detail: "table", Kind: KindRelation, Owner: ptr("public")},
		{Name: "email", Detail: "text", Kind: KindColumn, Owner: ptr("users")},
	})
	return New(viewSQL, vocabulary)
}

func editorView(e *SqlEditor) func(c *ui.Context) {
	return func(c *ui.Context) {
		c.Root().Background(theme.Current().Colors.Panel.Color()).Font(theme.UiFontFamily).TextColor(theme.Current().Colors.TextBody.Color())
		ui.Box(c).Fill().Children(func() { e.View(c) })
	}
}

func savePNG(t *testing.T, tt *ui.Tester, name string) string {
	t.Helper()
	dir := os.Getenv("SQLEDITOR_PNG_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, tt.Image()); err != nil {
		t.Fatal(err)
	}
	return path
}

// The editor paints a buffer of several statements with a run's marks on
// them, a statement that changes the shape, and a squiggle.
func TestTheEditorPaintsMarksAShapeChangeAndASquiggle(t *testing.T) {
	e := viewEditor()
	ranges := queryRanges(viewSQL)
	e.SetStatements(ranges, 0)
	e.SetStatementStatus(0, StatementDone, "1 row · 3 ms")
	e.SetStatementStatus(1, StatementDone, "altered · 12 ms")
	e.SetStatementStatus(2, StatementFailed, "failed · 2 ms")
	e.SkipStatementsFrom(3, "not run · statement 3 failed")
	frm := strings.Index(viewSQL, "frm")
	e.SetDiagnostics([]Diagnostic{{Range: Range{frm, frm + 3}, Message: "syntax error at or near \"users\""}})

	tt := ui.NewTester(editorView(e), 760, 300)
	e.FocusHandle().Focus()
	tt.Frame()
	tt.Advance(50 * time.Millisecond)
	path := savePNG(t, tt, "sqleditor-marks.png")
	t.Logf("wrote %s", path)

	for _, want := range []string{"1 row · 3 ms", "altered · 12 ms", "failed · 2 ms", "DDL", "7"} {
		if !tt.HasText(want) {
			t.Errorf("%q not on screen; texts: %v", want, tt.Texts())
		}
	}
	if e.lastLayout == nil || len(e.lastLayout.lines) == 0 {
		t.Fatalf("nothing was laid out")
	}
}

// Typing goes in through the TextInputClient, keys through the table, and
// every edit is told to the owner once the text is the new one.
func TestTypingAndKeysEditTheBuffer(t *testing.T) {
	e := New("select 1", nil)
	var seen []string
	e.Subscribe(func(ed *SqlEditor, ev SqlEditorEvent) {
		if ev == Changed {
			seen = append(seen, ed.Text())
		}
	})
	tt := ui.NewTester(editorView(e), 600, 200)
	e.FocusHandle().Focus()
	tt.Frame()
	tt.Type(";")
	tt.Frame()
	if e.Text() != "select 1;" {
		t.Fatalf("text = %q", e.Text())
	}
	tt.Key(ui.Cmd, ui.KeyLeft)
	tt.Frame()
	if e.Cursor() != 0 {
		t.Errorf("cursor after ⌘← = %d", e.Cursor())
	}
	tt.Key(ui.Shift|ui.Alt, ui.KeyRight)
	tt.Frame()
	if got, ok := e.SelectedText(); !ok || got != "select" {
		t.Errorf("selected = %q, %v", got, ok)
	}
	tt.Key(ui.Cmd, ui.KeySlash)
	tt.Frame()
	if e.Text() != "-- select 1;" {
		t.Errorf("text after ⌘/ = %q", e.Text())
	}
	tt.Key(ui.Cmd, ui.KeyZ)
	tt.Frame()
	if e.Text() != "select 1;" {
		t.Errorf("text after ⌘Z = %q", e.Text())
	}
	if len(seen) == 0 || seen[0] != "select 1;" {
		t.Errorf("Changed saw %q", seen)
	}
}

// ⌘F opens the strip on the selection, and the walk selects the hits.
func TestFindWalksTheHits(t *testing.T) {
	e := New("select a;\nselect b;\nselect c;", nil)
	tt := ui.NewTester(editorView(e), 700, 240)
	e.FocusHandle().Focus()
	tt.Frame()
	e.Find()
	tt.Frame()
	if e.find == nil {
		t.Fatalf("no strip")
	}
	tt.Type("select")
	tt.Frame()
	tt.Frame()
	if !tt.HasText("1 of 3") && !tt.HasText("3 found") {
		t.Errorf("count missing; texts: %v", tt.Texts())
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Frame()
	if e.selectedRange != (Range{10, 16}) {
		t.Errorf("selection after ⏎ = %v", e.selectedRange)
	}
	// A hit the walk selected is not a selection to run.
	if _, ok := e.SelectedText(); ok {
		t.Errorf("a found hit counted as a selection")
	}
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if e.find != nil {
		t.Errorf("the strip is still open")
	}
}

// queryRanges is the statements of a buffer, as the shell hands them to
// SetStatements.
func queryRanges(text string) []Range {
	var out []Range
	for _, r := range query.StatementRanges(text) {
		out = append(out, Range(r))
	}
	return out
}

// A long buffer follows the caret down and across, keeps the gutter in
// place, and shows both bars; typing a word opens the completion panel.
func TestALongBufferScrollsToTheCaretAndCompletes(t *testing.T) {
	var b strings.Builder
	for i := range 60 {
		b.WriteString("select id from public.users where id = ")
		b.WriteString(strings.Repeat("1", i))
		b.WriteString(";\n")
	}
	e := New(b.String(), viewEditor().vocabulary)
	tt := ui.NewTester(editorView(e), 700, 300)
	e.FocusHandle().Focus()
	tt.Frame()
	tt.Frame()
	tt.Type("select ema")
	tt.Frame()
	tt.Frame()
	path := savePNG(t, tt, "sqleditor-scrolled.png")
	t.Logf("wrote %s", path)
	if e.scrollHandle.State.Y <= 0 {
		t.Errorf("the buffer did not scroll down: %v", e.scrollHandle.State)
	}
	if !tt.HasText("61") {
		t.Errorf("the caret's line number is not on screen")
	}
	if len(e.completions) == 0 || e.completions[0].Label != "email" {
		t.Errorf("completions = %v", labels(e.completions))
	}
	if !tt.HasText("↩ or ⇥ to insert · esc to dismiss") {
		t.Errorf("the panel is not on screen")
	}
	tt.Key(0, ui.KeyTab)
	tt.Frame()
	if !strings.HasSuffix(e.Text(), "select email") {
		t.Errorf("text ends %q", e.Text()[len(e.Text())-20:])
	}
	// Up a long way, then to the end of a long line: the view follows
	// across as well.
	tt.Key(ui.Cmd, ui.KeyUp)
	tt.Frame()
	tt.Frame()
	if e.scrollHandle.State.Y != 0 {
		t.Errorf("⌘↑ left the view at %v", e.scrollHandle.State.Y)
	}
	for range 59 {
		tt.Key(0, ui.KeyDown)
		tt.Frame()
	}
	tt.Key(ui.Cmd, ui.KeyRight)
	tt.Frame()
	tt.Frame()
	if e.hScrollHandle.State.X <= 0 {
		t.Errorf("the view did not follow across: %v", e.hScrollHandle.State)
	}
	path = savePNG(t, tt, "sqleditor-across.png")
	t.Logf("wrote %s", path)
}

// A gesture over the text goes to the container that scrolls its way:
// down to the outer one, across to the inner one, as GPUI handed each
// container the axis it has.
func TestAGestureScrollsTheContainerOfItsAxis(t *testing.T) {
	var b strings.Builder
	for range 60 {
		b.WriteString(strings.Repeat("select 1 ", 20))
		b.WriteString(";\n")
	}
	e := New(b.String(), nil)
	e.selectedRange = Range{0, 0}
	tt := ui.NewTester(editorView(e), 600, 300)
	tt.Frame()
	tt.Frame()
	tt.Scroll(300, 150, 0, 120)
	tt.Frame()
	if e.scrollHandle.State.Y <= 0 || e.hScrollHandle.State.X != 0 {
		t.Errorf("down: v %v, h %v", e.scrollHandle.State, e.hScrollHandle.State)
	}
	tt.Scroll(300, 150, 90, 0)
	tt.Frame()
	if e.hScrollHandle.State.X <= 0 {
		t.Errorf("across: h %v", e.hScrollHandle.State)
	}
	// A press and a drag in the text select by the pointer, wherever the
	// release lands.
	tt.Press(200, 100)
	tt.Move(260, 140)
	tt.Release(900, 140)
	tt.Frame()
	if e.selectedRange.IsEmpty() {
		t.Errorf("the drag selected nothing")
	}
	if e.isSelecting {
		t.Errorf("the release left the buffer selecting")
	}
}
