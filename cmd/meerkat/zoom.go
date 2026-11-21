package main

// ⌘+, ⌘- and ⌘0: the size of everything in the window, the way a browser,
// Mail or Xcode answers those keys.
//
// It is a **zoom**, not a font size. The type scale in `theme` is six
// tokens and the layout is built around them — the grid and the SQL editor
// measure columns by a fixed advance, rows are a fixed height, icons sit on
// a 14-unit grid — so scaling the fonts alone would put 15px text in a
// 22px row and cut every value in half. mygo's `Context.SetZoom` lays the
// view out in a window 1/z its size and draws it z times larger, at the
// screen's resolution times z, so text, icons and hairlines all grow
// together and stay as sharp as they are at that size natively.
//
// The zoom is one value for the whole app, like the pane sizes: it is the
// reader's eyesight and the screen they are on, not a property of a
// window or a database. So a step in one window steps every window, and
// it is kept in the store's `ui_state` bag and read back at the next
// start.

import (
	"math"
	"strconv"

	"github.com/egoist/mygo"
	"github.com/nick318/meerkat/internal/storage"
)

// zoomSteps are the zooms ⌘+ and ⌘- walk, a browser's: close together
// near 100%, where a step is a matter of taste, and wider apart above it.
// The ends are where the app stops being usable — under 50% the type
// scale's caption is a 5px smear, and over 300% a laptop's window holds
// no more than the title bar and the editor.
var zoomSteps = []float32{0.5, 0.67, 0.75, 0.8, 0.9, 1, 1.1, 1.25, 1.5, 1.75, 2, 2.5, 3}

// zoomStoreKey is the zoom's row in the store's `ui_state` bag.
const zoomStoreKey = "ui_zoom"

// uiZoom is the app's zoom, 1 for none. Every window's view sets it at the
// top of every frame. Main thread only.
var uiZoom float32 = 1

// The accelerators of the View menu's zoom items, which ⌘+, ⌘- and ⌘0 in
// the app's key table answer the same way (see appMenu).
const (
	zoomMenuReset = "CmdOrCtrl+0"
	zoomMenuIn    = "CmdOrCtrl+Plus"
	zoomMenuOut   = "CmdOrCtrl+-"
)

// stepZoom is the zoom one step from cur: the next of zoomSteps up for a
// positive dir, down for a negative one, and cur itself held at the end.
// A cur between two steps — a value written by hand, or by an older build
// with other steps — goes to the nearest step in the direction asked.
func stepZoom(cur float32, dir int) float32 {
	const slack = 0.001
	if dir > 0 {
		for _, s := range zoomSteps {
			if s > cur+slack {
				return s
			}
		}
		return zoomSteps[len(zoomSteps)-1]
	}
	for i := len(zoomSteps) - 1; i >= 0; i-- {
		if zoomSteps[i] < cur-slack {
			return zoomSteps[i]
		}
	}
	return zoomSteps[0]
}

// clampZoom holds a zoom between the two ends of zoomSteps, and reads
// anything that is not a zoom at all as none.
func clampZoom(z float32) float32 {
	if math.IsNaN(float64(z)) || math.IsInf(float64(z), 0) || z <= 0 {
		return 1
	}
	return min(max(z, zoomSteps[0]), zoomSteps[len(zoomSteps)-1])
}

// parseZoom reads the stored zoom, and answers 1 for a row that does not
// parse rather than failing the start.
func parseZoom(value string) float32 {
	parsed, err := strconv.ParseFloat(value, 32)
	if err != nil {
		return 1
	}
	return clampZoom(float32(parsed))
}

// loadZoom reads the saved zoom into uiZoom, before the first window. A
// store that will not open leaves the app at 100%: a zoom is not worth
// refusing to start over.
func loadZoom() {
	store, err := storage.OpenDefault()
	if err != nil {
		return
	}
	defer store.Close()
	if value, ok := store.UiValue(zoomStoreKey); ok {
		uiZoom = parseZoom(value)
	}
}

// saveZoom writes the zoom back. It runs once a key press, never per
// frame, so a store opened for the one write costs nothing worth keeping
// a handle for.
var saveZoom = func(z float32) {
	store, err := storage.OpenDefault()
	if err != nil {
		return
	}
	defer store.Close()
	_ = store.SetUiValue(zoomStoreKey, formatF32(z))
}

// zoomTo sets the app's zoom, writes it back and repaints every window,
// since every window wears it. A zoom that does not change is not saved.
func zoomTo(z float32) {
	z = clampZoom(z)
	if z == uiZoom {
		return
	}
	uiZoom = z
	saveZoom(z)
	for _, w := range roots {
		if w.window != nil {
			w.window.Invalidate()
		}
	}
}

// zoomIn, zoomOut and zoomReset are the three gestures, from a key or from
// the View menu.
func zoomIn()    { zoomTo(stepZoom(uiZoom, 1)) }
func zoomOut()   { zoomTo(stepZoom(uiZoom, -1)) }
func zoomReset() { zoomTo(1) }

// zoomMenu is the View menu's three zoom items, the labels and keys every
// Mac app gives them. The window binds the same keys to the same calls;
// on macOS the menu hears them first, which changes nothing.
func zoomMenu() []*mygo.MenuItem {
	return []*mygo.MenuItem{
		{
			Label:       "Actual Size",
			Accelerator: zoomMenuReset,
			Click:       func(*mygo.MenuItem, *mygo.Window) { zoomReset() },
		},
		{
			Label:       "Zoom In",
			Accelerator: zoomMenuIn,
			Click:       func(*mygo.MenuItem, *mygo.Window) { zoomIn() },
		},
		{
			Label:       "Zoom Out",
			Accelerator: zoomMenuOut,
			Click:       func(*mygo.MenuItem, *mygo.Window) { zoomOut() },
		},
	}
}
