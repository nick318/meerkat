package main

import (
	"math"
	"testing"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// ⌘+ and ⌘- walk the browser's steps, hold at both ends, and a zoom
// between two steps goes to the nearest one in the direction asked.
func TestTheZoomWalksItsSteps(t *testing.T) {
	cases := []struct {
		name string
		cur  float32
		dir  int
		want float32
	}{
		{"in from 100%", 1, 1, 1.1},
		{"out from 100%", 1, -1, 0.9},
		{"in from 125%", 1.25, 1, 1.5},
		{"out from 67%", 0.67, -1, 0.5},
		{"held at the top", 3, 1, 3},
		{"held at the bottom", 0.5, -1, 0.5},
		{"in from between two steps", 1.2, 1, 1.25},
		{"out from between two steps", 1.2, -1, 1.1},
		{"in from under the bottom", 0.3, 1, 0.5},
		{"out from over the top", 4, -1, 3},
	}
	for _, c := range cases {
		if got := stepZoom(c.cur, c.dir); got != c.want {
			t.Errorf("%s: stepZoom(%v, %d) = %v, want %v", c.name, c.cur, c.dir, got, c.want)
		}
	}
}

// Every step in is undone by a step out, so ⌘+ ⌘- comes back to where it
// started.
func TestAStepInAndOutComesBack(t *testing.T) {
	for _, s := range zoomSteps[:len(zoomSteps)-1] {
		if got := stepZoom(stepZoom(s, 1), -1); got != s {
			t.Errorf("in and out from %v lands on %v", s, got)
		}
	}
}

// A stored zoom that does not read as one starts the app at 100% rather
// than failing it, and one past the ends is held at them.
func TestAStoredZoomIsReadSafely(t *testing.T) {
	cases := map[string]float32{
		"1.25":  1.25,
		"2":     2,
		"":      1,
		"big":   1,
		"0":     1,
		"-2":    1,
		"NaN":   1,
		"+Inf":  1,
		"0.1":   0.5,
		"12":    3,
		"1.333": 1.333,
	}
	for value, want := range cases {
		if got := parseZoom(value); math.Abs(float64(got-want)) > 1e-6 {
			t.Errorf("parseZoom(%q) = %v, want %v", value, got, want)
		}
	}
}

// zoomRig is a window's own view with the zoom saved nowhere, at 100%.
func zoomRig(t *testing.T) (*ui.Tester, *float32, *[]float32) {
	t.Helper()
	savedZoom, savedSave, savedRoots := uiZoom, saveZoom, roots
	t.Cleanup(func() { uiZoom, saveZoom, roots = savedZoom, savedSave, savedRoots })
	uiZoom = 1
	roots = nil
	var saved []float32
	saveZoom = func(z float32) { saved = append(saved, z) }
	var width float32
	view := windowView(&rootWindow{})
	tt := ui.NewTester(func(c *ui.Context) {
		view(c)
		width, _ = c.Size()
	}, 1200, 800)
	tt.Frame()
	return tt, &width, &saved
}

// ⌘= and ⇧⌘= zoom in, ⌘- zooms out and ⌘0 puts it back, and what the
// view lays out in shrinks and grows with it, which is what makes it a
// zoom rather than a number nobody reads.
func TestTheZoomKeysZoomTheWindow(t *testing.T) {
	tt, width, saved := zoomRig(t)
	if *width != 1200 {
		t.Fatalf("unzoomed, the view is %v wide, want 1200", *width)
	}
	press := func(mods ui.Modifiers, key ui.Key, want float32) {
		t.Helper()
		tt.Key(mods, key)
		tt.Frame()
		if uiZoom != want {
			t.Fatalf("after %v+%v the zoom is %v, want %v", mods, key, uiZoom, want)
		}
		if got := 1200 / want; math.Abs(float64(*width-got)) > 0.5 {
			t.Errorf("at %v the view is %v wide, want %v", want, *width, got)
		}
	}
	press(ui.Cmd, ui.KeyEqual, 1.1)
	press(ui.Cmd|ui.Shift, ui.KeyEqual, 1.25)
	press(ui.Cmd, ui.KeyMinus, 1.1)
	press(ui.Cmd|ui.Shift, ui.KeyMinus, 1)
	press(ui.Cmd, ui.KeyMinus, 0.9)
	press(ui.Cmd, ui.Key0, 1)

	want := []float32{1.1, 1.25, 1.1, 1, 0.9, 1}
	if len(*saved) != len(want) {
		t.Fatalf("saved %v, want %v", *saved, want)
	}
	for i := range want {
		if (*saved)[i] != want[i] {
			t.Fatalf("saved %v, want %v", *saved, want)
		}
	}

	// ⌘0 at 100% changes nothing, so it writes nothing.
	tt.Key(ui.Cmd, ui.Key0)
	tt.Frame()
	if len(*saved) != len(want) {
		t.Errorf("⌘0 at 100%% wrote the zoom again: %v", *saved)
	}
}

// The View menu's three items make the same calls the keys do.
func TestTheViewMenuZooms(t *testing.T) {
	zoomRig(t)
	items := map[string]func(*mygo.MenuItem, *mygo.Window){}
	for _, menu := range appMenu() {
		for _, item := range menu.Submenu {
			if item.Accelerator != "" {
				items[item.Accelerator] = item.Click
			}
		}
	}
	for _, accel := range []string{zoomMenuIn, zoomMenuOut, zoomMenuReset} {
		if items[accel] == nil {
			t.Fatalf("the menu bar has no item on %s", accel)
		}
	}
	items[zoomMenuIn](nil, nil)
	items[zoomMenuIn](nil, nil)
	if uiZoom != 1.25 {
		t.Fatalf("two Zoom Ins land on %v, want 1.25", uiZoom)
	}
	items[zoomMenuOut](nil, nil)
	if uiZoom != 1.1 {
		t.Fatalf("Zoom Out lands on %v, want 1.1", uiZoom)
	}
	items[zoomMenuReset](nil, nil)
	if uiZoom != 1 {
		t.Fatalf("Actual Size lands on %v, want 1", uiZoom)
	}
}

// The keys reach the zoom with the focus in the SQL editor, where every
// other key is the buffer's: the window's table is last in line, and
// nothing nearer the focus binds them.
func TestTheZoomKeysWorkFromTheEditor(t *testing.T) {
	savedZoom, savedSave := uiZoom, saveZoom
	t.Cleanup(func() { uiZoom, saveZoom = savedZoom, savedSave })
	uiZoom = 1
	saveZoom = func(float32) {}
	s := newViewTestShell(nil)
	s.newQuery()
	view := windowView(&rootWindow{root: &Root{screen: rootScreen{workspace: s}}})
	tt := ui.NewTester(view, 1360, 880)
	tt.Frame()
	tab, ok := s.activeTab().(*QueryTab)
	if !ok {
		t.Fatalf("the active tab is %T, want a query tab", s.activeTab())
	}
	tab.editor.FocusHandle().Focus()
	tt.Frame()
	before := tab.editor.Text()
	tt.Key(ui.Cmd|ui.Shift, ui.KeyEqual)
	tt.Frame()
	if uiZoom != 1.1 {
		t.Fatalf("⇧⌘= in the editor left the zoom at %v, want 1.1", uiZoom)
	}
	if tab.editor.Text() != before {
		t.Errorf("⇧⌘= typed into the editor: %q", tab.editor.Text())
	}
}
