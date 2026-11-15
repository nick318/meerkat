package uikit

// The caret's blink, for every buffer that draws one.
//
// Two things paint a caret — the form's `TextField` and the multi-line
// `sqleditor` — and a caret that blinks at one rate in the connection
// form and holds still in the SQL pane reads as two different apps. So
// the cycle lives here once and both own a `Blink`.
//
// **The blink is a timer, not an animation.** GPUI's `with_animation`
// asks for a frame every frame it runs, and a caret is on screen for as
// long as a window is open; a timer that fires twice a second wakes the
// window twice a second and no more. Nothing here paints: the owner
// reads `Blink.On` while it builds its quads, and a caret it decides
// not to paint at all — an unfocused field — costs nothing either way.
//
// **The caret is solid while the user is working.** Every edit and
// every motion calls `RestartBlink`, so the caret goes on and its
// cycle starts over. A caret blinking under a held-down arrow key is a
// caret the eye cannot follow to where it has moved to.
//
// Go port: the Rust cycle was a chain of `cx.spawn` timers, each one
// queueing the next and stopped by the epoch. mygo builds frames from the
// view, so the timer is `Context.After`: `TrackBlinkFocus`, which the
// owner calls from its view in every frame, turns the caret over from the
// time since the cycle started and asks for a frame at the next turn —
// one frame per half cycle, and none at all while the owner is unfocused.
// The epoch still counts the cycles, as the Rust's did.

import (
	"time"

	"github.com/egoist/mygo/ui"
)

// blinkInterval is how long the caret stays on, and then off. macOS
// blinks a caret at roughly this rate, and a field that matches it reads
// as a real one. (Rust: `INTERVAL`.)
const blinkInterval = 530 * time.Millisecond

// Blink is where a caret is in its cycle.
type Blink struct {
	// Whether the caret is in the on half of the cycle. A caret that
	// never goes out reads as a window that has stopped answering.
	on bool
	// Counts the cycles, so a cycle that has been replaced stops
	// instead of fighting the one that replaced it. Typing restarts the
	// cycle; two of them would beat against each other.
	epoch int
	// Whether the owner held the focus at the last paint. The cycle is
	// started and stopped from that edge, because focus is a property
	// of the window and only the render pass has one.
	focused bool
	// When the current cycle started, by the frame clock; zero until the
	// first frame after a restart reads the clock.
	started time.Time
	// Whether a cycle is running: started by a restart, ended by a stop.
	running bool
}

// NewBlink is the Rust `Blink::default()`: the caret on, no cycle.
func NewBlink() Blink { return Blink{on: true} }

// On reports whether to paint the caret this frame.
func (b *Blink) On() bool { return b.on }

// Blinking is what a caret's owner has to say for `Blink` to drive it:
// where the state lives. Everything else comes with the functions below
// (the Rust trait's default methods).
type Blinking interface {
	BlinkState() *Blink
}

// RestartBlink shows the caret and starts its cycle over. Called when the
// buffer takes the focus and after every edit or motion, so the caret is
// solid while the user is working and only blinks once they stop — the
// way a caret behaves in every platform field.
func RestartBlink(t Blinking) {
	blink := t.BlinkState()
	blink.on = true
	blink.epoch++
	// The next frame reads the clock: an edit from a text-input callback
	// has no Context to ask.
	blink.started = time.Time{}
	blink.running = true
}

// StopBlink stops the cycle and leaves the caret on, for a buffer that
// has lost the focus. An unfocused buffer paints no caret at all, so what
// matters here is that the timer stops waking the window.
func StopBlink(t Blinking) {
	blink := t.BlinkState()
	blink.on = true
	blink.epoch++
	blink.running = false
}

// TrackBlinkFocus notes whether the buffer holds the focus this frame, and
// starts or stops the cycle on the edge. Call it from the view, in every
// frame, which is the one place with a window to ask; it also turns the
// caret over when the cycle says so and asks for the frame of the next
// turn (Rust: the `schedule` chain).
func TrackBlinkFocus(t Blinking, focused bool, c *ui.Context) {
	if focused != t.BlinkState().focused {
		t.BlinkState().focused = focused
		if focused {
			RestartBlink(t)
		} else {
			StopBlink(t)
		}
	}
	schedule(t.BlinkState(), c)
}

// schedule turns the caret over by the time the cycle has run, then asks
// for the frame of the next turn. A stopped cycle asks for nothing, so the
// owner is never left with a timer it does not want.
func schedule(blink *Blink, c *ui.Context) {
	if !blink.running {
		return
	}
	now := c.Now()
	if blink.started.IsZero() {
		blink.started = now
	}
	elapsed := now.Sub(blink.started)
	turns := elapsed / blinkInterval
	blink.on = turns%2 == 0
	c.After(blinkInterval - elapsed%blinkInterval)
}
