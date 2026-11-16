package main

import (
	"testing"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// The menu bar must carry no item whose key the window binds itself: on
// macOS a menu's key equivalent wins over the window, so mygo's default
// "Close Window ⌘W" closed the window instead of the tab, and its "Force
// Reload ⇧⌘R" took the rollback key.
func TestTheMenuBarTakesNoKeyTheWindowBinds(t *testing.T) {
	var walk func(items []*mygo.MenuItem)
	walk = func(items []*mygo.MenuItem) {
		for _, item := range items {
			switch item.Role {
			case mygo.RoleClose, mygo.RoleFileMenu, mygo.RoleViewMenu,
				mygo.RoleReload, mygo.RoleForceReload, mygo.RoleNew:
				t.Errorf("the menu bar carries %q, whose key the window binds", item.Role)
			}
			if item.Accelerator != "" {
				t.Errorf("the menu item %q takes %s from the window", item.Label, item.Accelerator)
			}
			walk(item.Submenu)
		}
	}
	walk(appMenu())
	// Quit has to be on it, or ⌘Q from the menu bar would have no home.
	found := false
	for _, item := range appMenu() {
		if item.Role == mygo.RoleAppMenu {
			found = true
		}
	}
	if !found {
		t.Error("the menu bar has no app menu, so nothing on it quits")
	}
}

func TestEveryQuitAsksUnlessItWasAnswered(t *testing.T) {
	cases := []struct {
		name       string
		restarting bool
		agreed     bool
		windows    int
		want       bool
	}{
		{"a plain ⌘Q asks", false, false, 1, true},
		{"⌘Q over two windows asks once", false, false, 2, true},
		{"a window's own dialog already asked", false, true, 1, false},
		{"restart now was the question", true, false, 1, false},
		{"the last window closed", false, false, 0, false},
	}
	for _, c := range cases {
		if got := needsQuitConfirm(c.restarting, c.agreed, c.windows); got != c.want {
			t.Errorf("%s: needsQuitConfirm = %v, want %v", c.name, got, c.want)
		}
	}
}

// ⌘W in a strip of two closes the active tab and leaves the window: the
// key reaches the shell's own binding, not the window's close.
func TestCmdWClosesTheActiveTab(t *testing.T) {
	s, _, tt, _ := framesRig(t)
	s.newQuery()
	tt.Frame()
	s.focusHandle.Focus()
	tt.Frame()
	if len(s.tabs) != 2 {
		t.Fatalf("rig has %d tabs, want 2", len(s.tabs))
	}
	tt.Key(ui.Cmd, ui.KeyW)
	tt.Frame()
	if len(s.tabs) != 1 {
		t.Fatalf("after ⌘W the strip has %d tabs, want 1", len(s.tabs))
	}
}

// ⌘W on the last tab goes back to the connections screen: the shell asks
// to be closed, and the tab is gone from the strip it writes back.
func TestCmdWOnTheLastTabGoesBackToConnections(t *testing.T) {
	s, _, tt, _ := framesRig(t)
	var events []ShellEvent
	s.Subscribe(func(_ *Shell, e ShellEvent) { events = append(events, e) })
	if len(s.tabs) != 1 {
		t.Fatalf("rig has %d tabs, want 1", len(s.tabs))
	}
	tt.Key(ui.Cmd, ui.KeyW)
	tt.Frame()
	if len(events) != 1 || events[0] != ShellEventClose {
		t.Fatalf("after ⌘W on the last tab the shell emitted %v, want [ShellEventClose]", events)
	}
	if len(s.tabs) != 0 {
		t.Errorf("the closed tab is still in the strip: %d tabs", len(s.tabs))
	}
}
