package main

// Not a port of a Rust test: how often the shell builds its view while
// nothing but a tone or a caret moves. mygo paints a frame again without
// building the view when only a drawing asked for it, and these tests
// hold the shell to that.

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/uikit"
)

// framesRig is a shell on a result, with the focus on the shell itself, so
// no caret is on screen to wake the window.
func framesRig(t *testing.T) (*Shell, *QueryTab, *ui.Tester, *int) {
	t.Helper()
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	s := newViewTestShell(nil)
	s.newQuery()
	tab := withResult(s)
	builds := 0
	tt := ui.NewTester(func(c *ui.Context) { builds++; s.View(c) }, 1360, 880)
	tt.Frame()
	s.focusHandle.Focus()
	tt.Frame()
	// Let the run button's settle end.
	tt.Advance(2 * time.Second)
	return s, tab, tt, &builds
}

// buildsPerSecond counts the builds over five seconds of nothing touched.
func buildsPerSecond(tt *ui.Tester, builds *int) float64 {
	tt.Advance(time.Second)
	*builds = 0
	tt.Advance(5 * time.Second)
	return float64(*builds) / 5
}

func TestARunInFlightBreathesWithoutRebuildingTheView(t *testing.T) {
	_, tab, tt, builds := framesRig(t)
	if got := buildsPerSecond(tt, builds); got != 0 {
		t.Fatalf("idle shell builds %.1f times a second, want 0", got)
	}
	tab.run = Run{kind: RunRunning, flight: Live{started: time.Now().Add(-2 * time.Second)}}
	tt.Frame()
	if got := buildsPerSecond(tt, builds); got != 0 {
		t.Errorf("a run in flight builds the view %.1f times a second, want 0", got)
	}
	// The breath still moves: the fill beside the verb changes tone
	// between two repaints half a breath apart.
	label, ok := tt.Find("Stop")
	if !ok {
		t.Fatalf("no stop button; texts: %q", tt.Texts())
	}
	x, y := int(label.X-4), int(label.Y+label.H/2)
	scale := float32(tt.Image().Bounds().Dx()) / 1360
	px := func() [4]uint8 {
		c := tt.Image().RGBAAt(int(float32(x)*scale), int(float32(y)*scale))
		return [4]uint8{c.R, c.G, c.B, c.A}
	}
	before := px()
	tt.Advance(runBreath / 4)
	if after := px(); after == before {
		t.Errorf("the breath did not move the fill: %v both times", before)
	}
	// The settle, when the result lands, is a repaint as well.
	tab.run = Run{}
	tab.landed++
	tt.Frame()
	*builds = 0
	tt.Advance(time.Second)
	if *builds != 0 {
		t.Errorf("the settle built the view %d times, want 0", *builds)
	}
}

func TestACaretInTheSidebarFilterBlinksWithoutRebuildingTheView(t *testing.T) {
	s, _, tt, builds := framesRig(t)
	s.catalogFilter.FocusHandle().Focus()
	tt.Frame()
	if got := buildsPerSecond(tt, builds); got != 0 {
		t.Errorf("a blinking caret builds the view %.1f times a second, want 0", got)
	}
}

func TestAnUnfocusedFieldEditedFromCodeArmsNoBlink(t *testing.T) {
	s, _, tt, builds := framesRig(t)
	s.catalogFilter.SetText("ord")
	s.catalogFilter.Clear()
	tt.Frame()
	if got := buildsPerSecond(tt, builds); got != 0 {
		t.Errorf("after an edit of an unfocused filter the view builds %.1f times a second, want 0", got)
	}
}

// A step in the result find strip moves the count in the frame of the key:
// the claimed shortcut is a consumed input, and mygo builds again for it.
func TestAResultFindStepMovesTheCountInTheSameFrame(t *testing.T) {
	s, _, tt, _ := framesRig(t)
	s.openResultFind()
	tt.Frame()
	tt.Type("a")
	mustFind(t, tt, "1 of 5")
	tt.Key(ui.Cmd, ui.KeyG)
	mustFind(t, tt, "2 of 5")
	tt.Key(ui.Shift, ui.KeyEnter)
	mustFind(t, tt, "1 of 5")
}
