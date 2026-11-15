package uikit

// GPUI's `with_animation`, for the few places the app animates: the run
// button's breath and settle, the running statement's dot, the
// connection test's word and breath.
//
// mygo animates by values an element eases to a target (`AnimateTo`,
// `Loop`), which is a different model from GPUI's: there an animation is a
// delta from 0 to 1 handed to a closure that styles the element, keyed by
// an element id whose change restarts it. The Rust code is written in the
// second model, and the reasons in CLAUDE.md lean on it — the settle's id
// carries `landed`, the breath is `repeat_synced` — so the port keeps the
// model: `AnimationDelta` returns GPUI's delta for an element, and the
// caller styles the element with it as the closure did.
//
// Two rules come across exactly (zed crates/gpui/src/elements/animation.rs
// at the pinned revision):
//
//   - **Reduce motion.** With `Preferences().ReduceMotion` set the element
//     is drawn in its static state — the end of a oneshot, the start of a
//     repeating one — and asks for no frames.
//   - **Synced phase.** `repeat_synced` takes the phase from one clock for
//     the whole app, so an element rebuilt by a keystroke does not start
//     over. Here the clock is the frame clock (`Context.Now`) taken modulo
//     the period from the Unix epoch: every element in every window shares
//     it, and a Tester's clock drives it in tests.

import (
	"math"
	"time"

	"github.com/egoist/mygo/ui"
)

// Animation is GPUI's `Animation`: how long it runs, whether it repeats,
// whether its phase is the app's, and its easing.
type Animation struct {
	Duration time.Duration
	// Oneshot is true unless Repeat or RepeatSynced was asked for.
	Oneshot bool
	// Synced derives the phase from a clock shared by the whole app.
	Synced bool
	// Easing maps the linear delta to the eased one; Linear when nil.
	Easing func(float32) float32
}

// NewAnimation is `Animation::new`: once, linear.
func NewAnimation(d time.Duration) Animation {
	return Animation{Duration: d, Oneshot: true, Easing: Linear}
}

// Repeat loops the animation when it finishes.
func (a Animation) Repeat() Animation { a.Oneshot = false; return a }

// RepeatSynced loops it, phase-locked to the app's clock.
func (a Animation) RepeatSynced() Animation { a.Oneshot, a.Synced = false, true; return a }

// WithEasing sets the easing.
func (a Animation) WithEasing(e func(float32) float32) Animation { a.Easing = e; return a }

// animationState is what GPUI kept per element id: when it started.
type animationState struct {
	id    any
	start time.Time
	done  bool
}

// AnimationDelta returns the eased delta, 0..1, of animation a on element
// el under id, as GPUI's `with_animation(id, a, |el, delta| …)` hands it
// to its closure, and asks for the next frame while it runs. A change of
// id starts it over, as a new element id did in GPUI.
func AnimationDelta(c *ui.Context, el ui.Element, id any, a Animation) float32 {
	easing := a.Easing
	if easing == nil {
		easing = Linear
	}
	if c.Preferences().ReduceMotion {
		if a.Oneshot {
			return easing(1.0)
		}
		return easing(0.0)
	}
	st := ui.Local(el, "uikit.animation", func() *animationState { return &animationState{} })
	now := c.Now()
	if (*st).id != id || (*st).start.IsZero() {
		**st = animationState{id: id, start: now}
	}
	var elapsed time.Duration
	if a.Synced && a.Duration > 0 {
		// Reduce modulo the duration before the float conversion, as GPUI
		// does, which keeps sub-second precision at any distance from the
		// epoch.
		n := now.UnixNano() % int64(a.Duration)
		if n < 0 {
			n += int64(a.Duration)
		}
		elapsed = time.Duration(n)
	} else {
		elapsed = now.Sub((*st).start)
	}
	delta := float32(elapsed.Seconds()) / float32(a.Duration.Seconds())
	done := false
	if delta > 1.0 {
		if a.Oneshot {
			done = true
			delta = 1.0
		} else {
			delta = float32(math.Mod(float64(delta), 1.0))
		}
	}
	if !done {
		c.AnimationFrame()
	}
	return easing(delta)
}

// Linear is GPUI's `linear`: the delta itself.
func Linear(delta float32) float32 { return delta }

// Quadratic is GPUI's `quadratic`.
func Quadratic(delta float32) float32 { return delta * delta }

// EaseInOut is GPUI's `ease_in_out`, a quadratic that starts and ends
// slowly.
func EaseInOut(delta float32) float32 {
	if delta < 0.5 {
		return 2.0 * delta * delta
	}
	x := -2.0*delta + 2.0
	return 1.0 - x*x/2.0
}

// EaseOutQuint is GPUI's `ease_out_quint()`.
func EaseOutQuint() func(float32) float32 {
	return func(delta float32) float32 {
		return 1.0 - float32(math.Pow(float64(1.0-delta), 5))
	}
}

// Bounce is GPUI's `bounce`: easing forward, then back.
func Bounce(easing func(float32) float32) func(float32) float32 {
	return func(delta float32) float32 {
		if delta < 0.5 {
			return easing(delta * 2.0)
		}
		return easing((1.0 - delta) * 2.0)
	}
}

// PulsatingBetween is GPUI's `pulsating_between`: a breath between min and
// max, a sine with a cubic in it for a more natural rhythm.
func PulsatingBetween(lo, hi float32) func(float32) float32 {
	span := hi - lo
	return func(delta float32) float32 {
		t := float32(math.Sin(float64(delta * 2.0 * math.Pi)))
		breath := (t*t*t + t) / 2.0
		normalized := (breath + 1.0) / 2.0
		return lo + normalized*span
	}
}
