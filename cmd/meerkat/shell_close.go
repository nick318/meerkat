// The close guard: every way out of a session asks before it ends work the
// server is still doing (shell.rs, agent K2's part — see
// docs/tasks/shell-split.md).

package main

import (
	"context"
	"slices"

	"github.com/nick318/meerkat/internal/dbclient"
)

// --- closing a session -----------------------------------------------

// guardClose asks before a close that would end work the server is still
// doing.
//
// `true` means go ahead now. `false` means the dialog is up, and the
// close happens — or does not — when the user answers. **Every way out
// calls this**, which is what makes the guard true rather than a
// warning the ⌘W path happens to show.
//
// A close that loses nothing never asks. Only a run in flight counts:
// an editor's buffer is already written back, and a tab with a result
// on screen loses a result the statement above it will fetch again.
//
// Rust: `pub fn guard_close(&mut self, what: Close, window, cx) -> bool`.
func (s *Shell) guardClose(what Close) bool {
	if s.quitting {
		// Asked and answered. The quit re-reads every window after a
		// dialog is agreed to, and this one has already said yes.
		return true
	}
	if s.confirm != nil {
		// One question at a time. A second ⌘W while the dialog is up is
		// the user repeating themselves, not asking something new.
		return false
	}
	running, open := s.workEndedBy(what)
	staged := s.stagedEndedBy(what)
	if len(running) == 0 && len(open) == 0 && len(staged) == 0 {
		return true
	}
	// The palette must not be left underneath: two overlays both taking
	// keys, and only one of them the one being answered. The column-find
	// popover and the value peek go for the same reason.
	if s.palette != nil {
		s.closePalette()
	}
	s.columnFind = nil
	s.peek = nil
	s.confirm = &Confirm{
		what:    what,
		running: running,
		open:    open,
		staged:  staged,
	}
	// Go port: the dialog's handle lives in the Confirm, so it is focused
	// once the Confirm is in place (Rust focused it, then stored it).
	s.confirm.focus.Focus()
	return false
}

// workEndedBy is what a close would end, by tab name: the runs still out,
// and the transactions still open. A tab can be in both lists.
//
// Rust: `fn work_ended_by(&self, what: Close) -> (Vec<SharedString>,
// Vec<SharedString>)`.
func (s *Shell) workEndedBy(what Close) (running []string, open []string) {
	running = []string{}
	open = []string{}
	for _, tab := range s.tabs {
		if what.Kind == CloseKindTab && tab.ID() != what.Tab {
			continue
		}
		tab, ok := tab.(*QueryTab)
		if !ok {
			continue
		}
		if tab.run.inFlight() {
			running = append(running, tab.title)
		}
		if tab.inTransaction {
			open = append(open, tab.title)
		}
	}
	return running, open
}

// stagedEndedBy is the tabs a close would throw staged changes away in.
// They are the third thing at stake: nothing was sent, so the server
// loses nothing, but the user loses what they typed.
func (s *Shell) stagedEndedBy(what Close) []StagedTab {
	var staged []StagedTab
	for _, tab := range s.tabs {
		if what.Kind == CloseKindTab && tab.ID() != what.Tab {
			continue
		}
		if tab, ok := tab.(*QueryTab); ok && tab.staged() > 0 {
			staged = append(staged, StagedTab{name: tab.title, changes: tab.staged()})
		}
	}
	return staged
}

// cancelRuns asks the server to give up every run this close would end,
// and hands the requests back rather than waiting for them here: quitting
// has to wait for them, and closing a tab must not.
//
// The backend id lands one round trip after a run starts, so a close
// inside that window finds no id and cannot ask. It is a race the
// stop button waits out and this cannot — the tab is going away — and
// the session work closes it for good, because a session knows its
// backend from the moment it opens.
//
// Rust: `pub(crate) fn cancel_runs(&mut self, what: Close, cx) ->
// Vec<StopTask>`. Each request is already on its way when this returns
// (see StopTask); Wait is what ⌘Q does.
func (s *Shell) cancelRuns(what Close) []*StopTask {
	connection := s.connection
	if connection == nil {
		return nil
	}
	var stops []*StopTask
	for _, tab := range s.tabs {
		if what.Kind == CloseKindTab && tab.ID() != what.Tab {
			continue
		}
		tab, ok := tab.(*QueryTab)
		if !ok {
			continue
		}
		live := tab.run.live()
		if live == nil {
			continue
		}
		if live.backend == nil {
			// The session is still opening, so nothing is out on the
			// server. Dropping the tab is all the stopping there is.
			continue
		}
		id := *live.backend
		// The tab says CANCELLING while the request is out. It matters
		// on the way to a quit, where the tabs stay on screen until the
		// cancels land — closing a tab takes the tab with it, and this
		// says nothing to nobody.
		tab.run = Run{kind: RunCancelling, flight: *live}
		stops = append(stops, newStopTask(func() (bool, error) {
			return connection.Stop(context.Background(), id, dbclient.StopCancel)
		}))
	}
	return stops
}

// forgetQuit forgets an agreement to quit, because the quit was called off
// at another window's dialog.
//
// Rust: `pub(crate) fn forget_quit(&mut self)`.
func (s *Shell) forgetQuit() {
	s.quitting = false
}

// proceedClose does the close the user agreed to.
//
// Rust: `fn proceed_close(&mut self, what: Close, window, cx)`.
func (s *Shell) proceedClose(what Close) {
	if what.Kind == CloseKindQuit {
		// The cancels are not sent from here. ⌘Q ends every window, and
		// only the app knows how many there are: it walks the list, asks
		// each one, and stops every run at once when they all agree. So
		// this window records that it has agreed and says so.
		s.quitting = true
		s.rememberTabs()
		s.emit(ShellEventQuit)
		return
	}
	stops := s.cancelRuns(what)
	switch what.Kind {
	case CloseKindTab:
		// Not waited for, because a tab closing must not wait on the
		// network. Each request runs on its own goroutine already, and
		// nothing aborts it when the handle is dropped.
		s.closeTab(what.Tab)
	case CloseKindShell:
		// ⌘W on the last tab leaves with that tab closed: it is gone
		// from the strip written back, so the connection opens on a
		// fresh query tab next time rather than on the tab the user
		// just closed. Its session dies with the shell's pools, which
		// is a rollback too, so nothing needs sending.
		if what.Tab != 0 {
			s.tabs = slices.DeleteFunc(s.tabs, func(tab Tab) bool { return tab.ID() == what.Tab })
			s.active = min(s.active, max(len(s.tabs)-1, 0))
		}
		// The shell is dropped from here, and with it every editor
		// buffer. Write the strip back while there is still
		// something to read it from.
		s.rememberTabs()
		s.emit(ShellEventClose)
	case CloseKindWindow:
		s.rememberTabs()
		// This one *is* waited for, and the window stays on screen
		// until the cancels land. Closing the last window quits the
		// app, which ends the process — so a cancel that has not left
		// yet would never leave, and the statement would outlive the
		// app that started it.
		window := s.window
		go func() {
			for _, stop := range stops {
				_, _ = stop.Wait()
			}
			// `window.remove_window()`: gone without asking the
			// window's own close guard again.
			if window != nil {
				window.Destroy()
			}
		}()
	case CloseKindQuit:
		// Handled above: it sends no cancels of its own.
	}
}

// closeConfirm puts the dialog away and keeps everything it asked about.
//
// Rust: `fn close_confirm(&mut self, window, cx)`.
func (s *Shell) closeConfirm() {
	// A quit is asked window by window, so a "stay" here calls the
	// whole quit off — including the windows that agreed to it before
	// this one was asked. Their agreement was to *this* quit, and this
	// quit is not happening; leaving them marked would let the next ⌘Q
	// end their runs without asking.
	if s.confirm != nil && s.confirm.what.Kind == CloseKindQuit {
		s.emit(ShellEventQuitCancelled)
	}
	s.confirm = nil
	// The workspace takes the focus back, or the shell's keys have
	// nowhere to dispatch once the dialog is gone.
	s.focusActiveTab()
}

// closeTab closes one tab and hands the focus to whatever takes its place.
// The active index counts tabs, not ids, so closing a tab to the left of
// the active one has to walk it back or the selection jumps.
//
// Rust: `fn close_tab(&mut self, tab_id: u64, window, cx)`.
func (s *Shell) closeTab(tabID uint64) {
	ix := slices.IndexFunc(s.tabs, func(tab Tab) bool { return tab.ID() == tabID })
	if ix < 0 {
		return
	}
	closed := s.tabs[ix]
	s.tabs = slices.Delete(s.tabs, ix, ix+1)
	// The tab's connection goes back to the pool, so it is rolled back
	// first: whatever acquires it next must not inherit a transaction
	// the user left open here. Leaving the shell needs none of this —
	// the pool dies with it, and a closing socket is itself a rollback.
	if closed, ok := closed.(*QueryTab); ok {
		if session := closed.session; session != nil {
			go session.Close(context.Background())
		}
	}
	if ix < s.active {
		s.active--
	}
	s.activate(min(s.active, max(len(s.tabs)-1, 0)))
	// Closing the last tab leaves nothing to activate, so the strip is
	// written back here rather than only from `activate`.
	s.rememberTabs()
	s.focusActiveTab()
}
