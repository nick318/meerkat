package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/uikit"
)

// ⌘E puts the keys on the sidebar's filter line from wherever they are.
func TestCmdEFocusesTheCatalogFilter(t *testing.T) {
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"shell", "editor", "grid"} {
		s := newViewTestShell(nil)
		s.newQuery()
		tab := withResult(s)
		focused := false
		tt := ui.NewTester(func(c *ui.Context) {
			s.View(c)
			focused = s.catalogFilter.FocusHandle().Focused(c)
		}, 1360, 880)
		tt.Frame()
		switch from {
		case "shell":
			s.focusHandle.Focus()
		case "editor":
			tab.editor.FocusHandle().Focus()
		case "grid":
			s.gridFocus.Focus()
		}
		tt.Frame()
		tt.Key(ui.Cmd, ui.KeyE)
		tt.Frame()
		tt.Frame()
		if !focused {
			t.Errorf("from the %s, ⌘E left the focus off the filter line", from)
		}
	}
}

// The browser's tab keys: ⌘⌥←→ walk the strip and ⌘1 … ⌘9 jump to a place
// in it, from the editor as well as from the shell.
func TestBrowserTabKeysSwitchTabs(t *testing.T) {
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	s := newViewTestShell(nil)
	s.newQuery()
	s.newQuery()
	s.newQuery()
	tt := ui.NewTester(func(c *ui.Context) { s.View(c) }, 1360, 880)
	tt.Frame()
	s.focusActiveTab()
	tt.Frame()
	press := func(mods ui.Modifiers, key ui.Key, want int) {
		t.Helper()
		tt.Key(mods, key)
		tt.Frame()
		if s.active != want {
			t.Errorf("after %v+%v the active tab is %d, want %d", mods, key, s.active, want)
		}
	}
	last := len(s.tabs) - 1
	press(ui.Cmd, ui.Key1, 0)
	press(ui.Cmd|ui.Alt, ui.KeyRight, 1)
	press(ui.Cmd|ui.Alt, ui.KeyLeft, 0)
	press(ui.Cmd|ui.Alt, ui.KeyLeft, last)
	press(ui.Cmd, ui.Key2, 1)
	press(ui.Cmd, ui.Key8, 1) // no eighth tab: nothing moves
	press(ui.Cmd, ui.Key9, last)
}

// The history screen searches its statements: ⌘F puts the keys on the
// line, and what is typed narrows the runs to the statements holding it.
func TestTheHistoryScreenSearchesItsStatements(t *testing.T) {
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().Unix()
	for i, statement := range []string{
		"select * from tenant.party limit 1000;",
		"select * from portal_be.fx_rate;",
		"SELECT * FROM portal_be.shedlock LIMIT 500;",
	} {
		elapsed, rows := uint64(5), uint64(1)
		if err := store.RecordQuery(storage.NewRun{
			Scope: "scope", Statement: statement, RanAt: now - int64(i),
			Source: storage.RunSourceUser, ElapsedMs: &elapsed, RowCount: &rows,
		}); err != nil {
			t.Fatal(err)
		}
	}
	s := newViewTestShell(nil)
	s.store, s.scope = store, "scope"
	s.newQuery()
	tt := ui.NewTester(func(c *ui.Context) { s.View(c) }, 1360, 880)
	tt.Frame()
	s.focusActiveTab()
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyY)
	tt.Frame()
	tab := s.activeTab().(*HistoryTab)
	if tab.runs != 3 {
		t.Fatalf("history shows %d runs, want 3", tab.runs)
	}
	tt.Key(ui.Cmd, ui.KeyF)
	tt.Frame()
	tt.Type("PORTAL")
	tt.Frame()
	if tab.runs != 2 {
		t.Errorf("a search for PORTAL leaves %d runs, want 2", tab.runs)
	}
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if tab.runs != 3 {
		t.Errorf("⎋ left %d runs, want all 3 back", tab.runs)
	}
}
