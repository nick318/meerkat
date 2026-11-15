package uikit

// Not a port of a Rust test: the blink's phase read from a clock, and
// the frames a field asks for while nothing is typed.

import (
	"bytes"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/theme"
)

type blinker struct{ b Blink }

func (t *blinker) BlinkState() *Blink { return &t.b }

func TestTheBlinkTurnsOverEveryInterval(t *testing.T) {
	owner := &blinker{b: NewBlink()}
	owner.b.focused = true
	RestartBlink(owner)
	start := time.Unix(1_000, 0)
	if !owner.b.OnAt(start) {
		t.Fatal("a restarted caret starts on")
	}
	if got := owner.b.NextFlip(start); !got.Equal(start.Add(blinkInterval)) {
		t.Errorf("first flip at %v, want %v", got.Sub(start), blinkInterval)
	}
	mid := start.Add(blinkInterval + blinkInterval/2)
	if owner.b.OnAt(mid) {
		t.Error("the caret is off in the second half cycle")
	}
	if got := owner.b.NextFlip(mid); !got.Equal(start.Add(2 * blinkInterval)) {
		t.Errorf("second flip at %v, want %v", got.Sub(start), 2*blinkInterval)
	}
	if !owner.b.OnAt(start.Add(2 * blinkInterval)) {
		t.Error("the caret is back on in the third half cycle")
	}
	// An edit puts it back on and starts the cycle from the next read.
	RestartBlink(owner)
	later := start.Add(5*blinkInterval + time.Millisecond)
	if !owner.b.OnAt(later) {
		t.Error("an edit leaves the caret on")
	}
	if got := owner.b.NextFlip(later); !got.Equal(later.Add(blinkInterval)) {
		t.Errorf("after an edit the flip is %v away, want %v", got.Sub(later), blinkInterval)
	}
}

func TestAStoppedOrUnfocusedBlinkNeverTurns(t *testing.T) {
	owner := &blinker{b: NewBlink()}
	// A programmatic edit of an unfocused owner arms nothing.
	RestartBlink(owner)
	now := time.Unix(1_000, 0)
	if !owner.b.OnAt(now) || !owner.b.NextFlip(now).IsZero() {
		t.Error("an unfocused owner's blink must stay on and never flip")
	}
	owner.b.focused = true
	RestartBlink(owner)
	StopBlink(owner)
	if !owner.b.OnAt(now.Add(blinkInterval)) || !owner.b.NextFlip(now).IsZero() {
		t.Error("a stopped blink must stay on and never flip")
	}
}

func blinkFieldRig(t *testing.T, f *TextField) (*ui.Tester, *int) {
	t.Helper()
	if err := RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	builds := 0
	tt := ui.NewTester(func(c *ui.Context) {
		builds++
		c.Root().Background(theme.Current().Colors.Panel.Color()).Font(theme.UiFontFamily)
		ui.Column(c).Padding(20).Width(400).Children(func() { f.View(c) })
	}, 600, 120)
	tt.Frame()
	return tt, &builds
}

func TestAFocusedFieldBlinksByRepaintsAlone(t *testing.T) {
	f := NewTextField("filter")
	f.SetText("orders")
	tt, builds := blinkFieldRig(t, f)
	f.FocusHandle().Focus()
	tt.Frame()
	snap := func() []byte { return append([]byte(nil), tt.Image().Pix...) }
	on := snap()
	*builds = 0
	tt.Advance(blinkInterval + blinkInterval/2)
	off := snap()
	if bytes.Equal(on, off) {
		t.Error("the caret did not go out in the second half cycle")
	}
	tt.Advance(blinkInterval)
	if !bytes.Equal(on, snap()) {
		t.Error("the caret did not come back in the third half cycle")
	}
	if *builds != 0 {
		t.Errorf("the blink built the view %d times, want 0", *builds)
	}
}

func TestAnUnfocusedFieldSetFromCodeAsksForNoFrames(t *testing.T) {
	f := NewTextField("filter")
	tt, builds := blinkFieldRig(t, f)
	f.SetText("ord")
	f.Clear()
	tt.Frame()
	*builds = 0
	tt.Advance(10 * time.Second)
	if *builds != 0 {
		t.Errorf("an unfocused field built the view %d times in ten seconds, want 0", *builds)
	}
}
