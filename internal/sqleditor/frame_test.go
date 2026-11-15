package sqleditor

import (
	"image"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// A key the editor takes shows everywhere in the frame it arrives in —
// the strip's count above the editor as well as the caret in it — with no
// second frame asked for: mygo builds the view again in the same frame
// after a key is taken.
func TestAKeyShowsInTheFrameItArrivesIn(t *testing.T) {
	e := New("select a;\nselect b;\nselect c;", nil)
	builds := 0
	view := editorView(e)
	tt := ui.NewTester(func(c *ui.Context) { builds++; view(c) }, 700, 240)
	e.FocusHandle().Focus()
	tt.Frame()
	e.Find()
	tt.Frame()
	tt.Type("select")
	tt.Frame()
	// Back to the buffer, with the strip still open over it.
	e.FocusHandle().Focus()
	tt.Frame()
	if !tt.HasText("1 of 3") {
		t.Fatalf("count before the key; texts: %v", tt.Texts())
	}

	builds = 0
	tt.Key(ui.Cmd, ui.KeyG)
	if e.selectedRange != (Range{10, 16}) {
		t.Fatalf("selection after ⌘G = %v", e.selectedRange)
	}
	if !tt.HasText("2 of 3") {
		t.Errorf("the strip did not show the step in its frame; texts: %v", tt.Texts())
	}
	if builds > 3 {
		t.Errorf("the key cost %d builds, more than one frame's", builds)
	}
	if tt.FramePending() {
		t.Errorf("the key left a frame asked for")
	}
	// The caret was painted where the step left it.
	line := e.lastLayout.line(1)
	if line == nil || e.lastLayout.firstRow > 1 {
		t.Fatalf("row 1 is not laid out")
	}

	// An edit in the buffer is searched again on the next build, and the
	// strip counts what is there now.
	tt.Key(0, ui.KeyRight)
	tt.Type(" select")
	if !tt.HasText("4 found") && !tt.HasText("of 4") {
		t.Errorf("the count did not follow the edit; texts: %v", tt.Texts())
	}
	tt.Advance(10 * time.Millisecond)
}

// framesRig is an editor in a Tester that counts the view's builds.
func framesRig(t *testing.T, text string) (*SqlEditor, *ui.Tester, *int) {
	t.Helper()
	e := New(text, nil)
	builds := 0
	view := editorView(e)
	tt := ui.NewTester(func(c *ui.Context) { builds++; view(c) }, 700, 240)
	e.FocusHandle().Focus()
	tt.Frame()
	tt.Frame()
	return e, tt, &builds
}

// pixelsDiffer reports whether two frames differ inside `r`.
func pixelsDiffer(a, b *image.RGBA, r image.Rectangle) bool {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if a.RGBAAt(x, y) != b.RGBAAt(x, y) {
				return true
			}
		}
	}
	return false
}

// The caret blinks by repainting the editor, never by building the view:
// a turn of a 2px bar twice a second must not rebuild the window. An edit
// still puts the caret back on and starts the cycle over.
func TestTheCaretBlinksWithoutBuildingTheView(t *testing.T) {
	e, tt, builds := framesRig(t, "select 1;")
	tt.Advance(10 * time.Millisecond)
	on := image.NewRGBA(tt.Image().Bounds())
	copy(on.Pix, tt.Image().Pix)
	if !e.blink.On() {
		t.Fatalf("the caret starts off")
	}
	*builds = 0
	tt.Advance(530 * time.Millisecond)
	if e.blink.On() {
		t.Errorf("the caret did not turn off")
	}
	if !pixelsDiffer(on, tt.Image(), tt.Image().Bounds()) {
		t.Errorf("the off half painted the same frame as the on half")
	}
	tt.Advance(5 * time.Second)
	if *builds != 0 {
		t.Errorf("the blink built the view %d times", *builds)
	}

	// Typing in the off half puts the caret on, and the cycle starts over
	// from the keystroke.
	for e.blink.On() {
		tt.Advance(100 * time.Millisecond)
	}
	tt.Type("x")
	if !e.blink.On() {
		t.Errorf("an edit left the caret off")
	}
	tt.Advance(400 * time.Millisecond)
	if !e.blink.On() {
		t.Errorf("the cycle did not start over at the edit")
	}
}

// A running statement's dot breathes by repainting, not by building the
// view at the display's rate.
func TestTheRunningDotBreathesWithoutBuildingTheView(t *testing.T) {
	e, tt, builds := framesRig(t, "select pg_sleep(10);\nselect 2;")
	e.SetStatements(queryRanges(e.Text()), 0)
	e.SetStatementStatus(0, StatementRunning, "running")
	// The window out of focus, so no caret blinks in the frames counted.
	tt.SetFocused(false)
	tt.Frame()
	tt.Advance(10 * time.Millisecond)
	before := image.NewRGBA(tt.Image().Bounds())
	copy(before.Pix, tt.Image().Pix)
	*builds = 0
	tt.Advance(350 * time.Millisecond)
	if *builds != 0 {
		t.Errorf("the breath built the view %d times in 350 ms", *builds)
	}
	// The dot sits in the gutter's mark lane on the first line.
	lane := image.Rect(0, 0, int(markWidth)+4, int(textPaddingY+lineHeight)+2)
	if !pixelsDiffer(before, tt.Image(), lane) {
		t.Errorf("the dot did not change between two moments of its breath")
	}
}
