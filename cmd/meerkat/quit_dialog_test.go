package main

import (
	"testing"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/uikit"
)

// quitRig is one window on a workspace, registered the way openWindow
// registers it, with mygo's quit replaced by a counter.
func quitRig(t *testing.T) (*Root, *ui.Tester, *int) {
	t.Helper()
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	s := newViewTestShell(nil)
	s.newQuery()
	r := &Root{screen: rootScreen{workspace: s}}
	saved, savedRoots, savedFocused := quitNow, roots, focusedWindow
	quits := 0
	quitNow = func() { quits++ }
	focusedWindow = func() *mygo.Window { return nil }
	roots = []*rootWindow{{root: r}}
	t.Cleanup(func() {
		quitNow, roots, focusedWindow = saved, savedRoots, savedFocused
		quitAsking, quitAgreed, restarting = false, false, false
	})
	tt := ui.NewTester(func(c *ui.Context) { r.View(c) }, 1360, 880)
	tt.Frame()
	s.focusActiveTab()
	tt.Frame()
	return r, tt, &quits
}

// ⌘Q puts the app's own card up, and ⏎ on it quits.
func TestTheQuitCardQuitsOnEnter(t *testing.T) {
	r, tt, quits := quitRig(t)
	quitWalk()
	tt.Frame()
	if !r.quitAsk || !tt.HasText(quitDialogTitle) {
		t.Fatalf("⌘Q put no quit card up; texts: %q", tt.Texts())
	}
	writeShellPNG(t, tt, "09-quit")
	if *quits != 0 {
		t.Fatalf("the card quit before it was answered")
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if *quits != 1 {
		t.Errorf("⏎ on the card quit %d times, want 1", *quits)
	}
	if r.quitAsk {
		t.Error("the card is still up after ⏎")
	}
}

// ⎋ stays, takes the card down, and gives the keys back to the tab.
func TestTheQuitCardStaysOnEscape(t *testing.T) {
	r, tt, quits := quitRig(t)
	quitWalk()
	tt.Frame()
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if *quits != 0 || r.quitAsk || quitAsking || quitAgreed {
		t.Fatalf("⎋: quits=%d card=%v asking=%v agreed=%v", *quits, r.quitAsk, quitAsking, quitAgreed)
	}
	if tt.HasText(quitDialogTitle) {
		t.Error("the card is still painted after ⎋")
	}
	// The next ⌘Q asks again rather than quitting on the stale answer.
	quitWalk()
	tt.Frame()
	if !r.quitAsk || *quits != 0 {
		t.Errorf("the next ⌘Q did not ask again: card=%v quits=%d", r.quitAsk, *quits)
	}
}

// A second ⌘Q while the card is up is the answer.
func TestASecondCmdQQuits(t *testing.T) {
	_, tt, quits := quitRig(t)
	quitWalk()
	tt.Frame()
	quitWalk()
	if *quits != 1 {
		t.Errorf("a second ⌘Q quit %d times, want 1", *quits)
	}
}
