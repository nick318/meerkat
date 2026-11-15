package uikit

// An overlay scrollbar.
//
// GPUI ships scroll containers but no scrollbar, and Zed's own is far
// more than this app needs. This one paints a thumb over the content and
// drags it: the mouse handlers are registered on the window, not on a
// hitbox, so a drag keeps working after the pointer leaves the 10px bar.
//
// It takes a plain `ScrollHandle`, so it serves the results grid and the
// sidebar's `uniform_list` alike — the list's handle carries one inside.
//
// Go port: mygo draws scroll bars of its own on its scroll containers; a
// container that wears this one turns those off (`.Scrollbars(false)`).
// The handle is the container's `ui.ScrollState` (`TrackScroll`), whose X
// and Y count *up* as the content travels, where GPUI's offset ran
// negative; the thumb reads them as distance travelled either way, so the
// sign flips at the two places the Rust negated. And the window-level
// listeners are mygo's implicit pointer capture: the element that takes a
// press (`HandleInput` returning true for `InputPointerDown`) gets every
// move and the release until the button comes up, wherever the pointer
// goes (mygo ui/input.go:474-485, :580 and :759).

import (
	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/theme"
)

// Thickness of the bar, and of the thumb inside it. (Rust: `THICKNESS`.)
const Thickness float32 = 10.

const thumbThickness float32 = 6.

// minThumb: a thumb shorter than this is hard to grab, however long the
// content.
const minThumb float32 = 24.

// Drag is where a drag started: which axis, and how far into the thumb the
// pointer grabbed it, so the thumb does not jump under the cursor.
type Drag struct {
	vertical bool
	grab     float32
}

// DragState is shared between the two bars of one grid, so only one can
// be dragging. (Rust: `Rc<Cell<Option<Drag>>>`; nil `Current` is `None`.)
type DragState struct {
	Current *Drag
}

// ScrollHandle is GPUI's `ScrollHandle` over a mygo scroll container: the
// container's `ScrollState` (give it `TrackScroll(&h.State)`), and the
// viewport it showed in the last frame, which the owner writes from the
// container's `Bounds()` after building it — the bar's `bounds()`.
type ScrollHandle struct {
	State  ui.ScrollState
	Bounds ui.Rect
}

// MaxOffset is how far the content can travel along each axis.
func (h *ScrollHandle) MaxOffset() ui.Point { return ui.Point{X: h.State.MaxX, Y: h.State.MaxY} }

// Offset is how far the content has travelled along each axis, as a
// positive distance.
func (h *ScrollHandle) Offset() ui.Point { return ui.Point{X: h.State.X, Y: h.State.Y} }

// Scrollbar paints and drags one axis of a ScrollHandle.
type Scrollbar struct {
	vertical    bool
	handle      *ScrollHandle
	drag        *DragState
	thumb       theme.Hsla
	thumbActive theme.Hsla
}

// NewScrollbar returns false when the content fits, so the bar appears
// only when it can actually do something.
func NewScrollbar(vertical bool, handle *ScrollHandle, drag *DragState, thumb, thumbActive theme.Hsla) (*Scrollbar, bool) {
	max := along(handle.MaxOffset(), vertical)
	if !(max > 0.) {
		return nil, false
	}
	return &Scrollbar{
		vertical:    vertical,
		handle:      handle,
		drag:        drag,
		thumb:       thumb,
		thumbActive: thumbActive,
	}, true
}

func along(point ui.Point, vertical bool) float32 {
	if vertical {
		return point.Y
	}
	return point.X
}

func alongSize(r ui.Rect, vertical bool) float32 {
	if vertical {
		return r.H
	}
	return r.W
}

// thumbSpan is where the thumb sits inside a bar of `track` pixels.
func thumbSpan(offset, max, viewport, track float32) (float32, float32) {
	content := viewport + max
	var length float32
	if content > 0. {
		length = min(maxF(track*(viewport/content), minThumb), track)
	} else {
		length = track
	}
	progress := clampF(offset/max, 0., 1.)
	return (track - length) * progress, length
}

// offsetForThumb is the scroll offset that puts the thumb's start at
// `position`.
func offsetForThumb(start, max, track, length float32) float32 {
	travel := track - length
	if travel <= 0. {
		return 0.
	}
	return max * clampF(start/travel, 0., 1.)
}

// maxF and clampF are Rust's f32::max and f32::clamp: max ignores a NaN
// operand, clamp passes a NaN through.
func maxF(a, b float32) float32 {
	if a != a {
		return b
	}
	if b != b {
		return a
	}
	return max(a, b)
}

func clampF(v, lo, hi float32) float32 {
	if v != v {
		return v
	}
	return min(max(v, lo), hi)
}

// prepaintState is what the Rust worked out in `prepaint`, kept on the
// element from the paint that worked it out to the input that reads it.
type prepaintState struct {
	thumb ui.Rect // relative to the bar's box
	// Pixels the content can still travel along this axis.
	max    float32
	track  float32
	length float32
}

// View builds the bar. The parent positions it absolutely; it fills
// whatever it is given.
func (s *Scrollbar) View(c *ui.Context) ui.Element {
	vertical := s.vertical
	el := ui.Box(c).Fill()
	pre := ui.Local(el, "uikit.scrollbar", func() prepaintState { return prepaintState{} })
	hovered := el.Hovered()

	el.Draw(func(p *ui.Painter, bounds ui.Rect) {
		viewport := alongSize(s.handle.Bounds, vertical)
		max := along(s.handle.MaxOffset(), vertical)
		// GPUI's offset ran negative as the content moved up or left;
		// mygo's counts the distance travelled, which is what the thumb
		// reads.
		offset := along(s.handle.Offset(), vertical)
		track := alongSize(bounds, vertical)
		start, length := thumbSpan(offset, max, viewport, track)

		inset := (Thickness - thumbThickness) / 2.
		var thumb ui.Rect
		if vertical {
			thumb = ui.Rect{X: inset, Y: start, W: thumbThickness, H: length}
		} else {
			thumb = ui.Rect{X: start, Y: inset, W: length, H: thumbThickness}
		}
		*pre = prepaintState{thumb: thumb, max: max, track: track, length: length}

		dragging := s.drag.Current != nil && s.drag.Current.vertical == vertical
		active := dragging || hovered
		color := s.thumb
		if active {
			color = s.thumbActive
		}
		p.Fill(ui.Rect{X: bounds.X + thumb.X, Y: bounds.Y + thumb.Y, W: thumb.W, H: thumb.H}, color.Color(), thumbThickness/2.)
	})

	el.HandleInput(func(ev ui.InputEvent) bool {
		st := *pre
		switch ev.Kind {
		case ui.InputPointerDown:
			if ev.Button != 0 {
				return false
			}
			position := along(ui.Point{X: ev.X, Y: ev.Y}, vertical)
			if contains(st.thumb, ev.X, ev.Y) {
				s.drag.Current = &Drag{vertical: vertical, grab: position - along(ui.Point{X: st.thumb.X, Y: st.thumb.Y}, vertical)}
			} else {
				// Clicking the track jumps the thumb to the pointer,
				// centred, and starts a drag from there.
				start := position - st.length/2.
				setOffset(s.handle, vertical, offsetForThumb(start, st.max, st.track, st.length))
				s.drag.Current = &Drag{vertical: vertical, grab: st.length / 2.}
			}
			return true
		case ui.InputPointerMove:
			current := s.drag.Current
			if current == nil || current.vertical != vertical {
				return false
			}
			start := along(ui.Point{X: ev.X, Y: ev.Y}, vertical) - current.grab
			setOffset(s.handle, vertical, offsetForThumb(start, st.max, st.track, st.length))
			return true
		case ui.InputPointerUp:
			if s.drag.Current != nil && s.drag.Current.vertical == vertical {
				s.drag.Current = nil
				return true
			}
		}
		return false
	})
	return el
}

func contains(r ui.Rect, x, y float32) bool {
	return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H
}

func setOffset(handle *ScrollHandle, vertical bool, travelled float32) {
	if vertical {
		handle.State.Y = travelled
	} else {
		handle.State.X = travelled
	}
}
