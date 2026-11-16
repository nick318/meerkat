// The window's root: the connections screen, or a workspace open on one
// database.
//
// Only one of the two is alive at a time. Leaving a workspace drops it,
// and with it the connection pool behind it, so a closed database keeps
// no sockets open.

package main

import (
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/uikit"
)

// rootAction is the Rust `actions!(root, [CloseWindow])`.
type rootAction int

const (
	rootCloseWindow rootAction = iota
)

// RootKeyContext is the context ⌘W falls back to. It sits *outside* the
// shell's, so a window with a tab strip answers ⌘W in the strip — GPUI gives
// a keystroke to the binding that matched deepest — and a window with no
// strip at all answers it here.
const RootKeyContext = "Root"

// rootKeyBindings is the Rust `root::key_bindings`. The table is
// dispatched on the root element, which is what wore `KEY_CONTEXT`.
func rootKeyBindings() []uikit.KeyBinding[rootAction] {
	return []uikit.KeyBinding[rootAction]{uikit.Bind("cmd-w", rootCloseWindow)}
}

var rootKeys = rootKeyBindings()

// Root holds one window's screen.
//
// The Rust `_subscription` was dropped with the screen it belonged to. A Go
// listener lives as long as its emitter, so dropping the screen is enough.
type Root struct {
	window *mygo.Window
	screen rootScreen
	// quitAsk says the "Quit Meerkat?" card is up on this window, and
	// quitFocus is the card's focus while it is (quit_dialog.go).
	quitAsk   bool
	quitFocus ui.Handle
}

// rootScreen is the Rust `enum Screen`: exactly one of the two is set.
type rootScreen struct {
	connections *Connections
	workspace   *Shell
}

// NewRoot opens on a database when the command line named one, and on the
// connections screen otherwise.
func NewRoot(target Target, window *mygo.Window) *Root {
	r := &Root{window: window}
	if target != nil {
		r.screen = r.workspaceScreen(target)
	} else {
		r.screen = r.connectionsScreen()
	}
	return r
}

func (r *Root) connectionsScreen() rootScreen {
	screen := NewConnections(r.window)
	screen.Subscribe(r.onConnectionsEvent)
	screen.FocusHandle().Focus()
	return rootScreen{connections: screen}
}

func (r *Root) workspaceScreen(target Target) rootScreen {
	workspace := NewShell(target, r.window)
	workspace.Subscribe(r.onShellEvent)
	workspace.FocusHandle().Focus()
	return rootScreen{workspace: workspace}
}

// remember writes an open workspace's tabs back to the local file. The
// window closing and ⌘Q both drop the shell without going through the
// "‹ connections" way out, so they call this first.
func (r *Root) remember() {
	if r.screen.workspace != nil {
		r.screen.workspace.rememberTabs()
	}
}

// guardCloseWindow asks the workspace whether this **window** may close.
// `false` means it put a question on screen instead, and closes the window
// itself if the user says so — the close button ends every tab of this
// window at once, so it may not skip the guard the tab strip goes through.
func (r *Root) guardCloseWindow() bool {
	return r.guard(Close{Kind: CloseKindWindow})
}

// guardQuit asks the workspace whether the **app** may go. ⌘Q ends every
// window, so each one is asked in turn; `false` means this one is asking
// the user, and the quit carries on from there.
func (r *Root) guardQuit() bool {
	return r.guard(Close{Kind: CloseKindQuit})
}

// onCloseWindow is ⌘W with nothing on screen to close. The connections
// screen holds no tabs, so the gesture that would take one takes the
// **window** — the same reading as ⌘W on the last tab of a strip, and the
// reading a browser gives a window with one tab left.
func (r *Root) onCloseWindow() {
	if !r.guardCloseWindow() {
		return
	}
	r.remember()
	// `window.remove_window()`: Destroy skips OnClose, which would only
	// ask the question just answered a second time. The window's OnClosed
	// (main.go) lets the shell go.
	r.window.Destroy()
}

func (r *Root) guard(what Close) bool {
	if r.screen.workspace != nil {
		return r.screen.workspace.guardClose(what)
	}
	// The connections screen holds no runs.
	return true
}

// forgetQuit forgets that this window agreed to a quit. The quit was
// refused at another window's dialog, so nothing was agreed to after all.
func (r *Root) forgetQuit() {
	if r.screen.workspace != nil {
		r.screen.workspace.forgetQuit()
	}
}

// stopRuns asks the server to give up every run in this window, on the way
// to a quit. The requests are handed back rather than detached, because
// quitting ends the process and a request that has not left yet never
// leaves.
func (r *Root) stopRuns() []*StopTask {
	if r.screen.workspace != nil {
		return r.screen.workspace.cancelRuns(Close{Kind: CloseKindQuit})
	}
	return nil
}

// release lets the workspace go, and its pools with it. GPUI dropped the
// entity; Go has no drop, so every way a window lets its shell go calls
// this.
func (r *Root) release() {
	if r.screen.workspace != nil {
		r.screen.workspace.release()
	}
	if r.screen.connections != nil {
		r.screen.connections.release()
	}
}

func (r *Root) onConnectionsEvent(_ *Connections, event ConnectionsEvent) {
	open, ok := event.(ConnectionsEventOpen)
	if !ok {
		return
	}
	r.release()
	r.screen = r.workspaceScreen(TargetProfile{Profile: open.Profile})
	r.window.Invalidate()
}

func (r *Root) onShellEvent(_ *Shell, event ShellEvent) {
	switch event {
	case ShellEventClose:
		r.release()
		r.screen = r.connectionsScreen()
		r.window.Invalidate()
	// The window the user answered has agreed; the others have not been
	// asked. So the quit starts again and walks the whole list, and this
	// window now answers yes without asking twice.
	//
	// It runs on the next tick rather than from here: the quit updates
	// every window, this one included, and this callback is already
	// inside that update. `Window.Update` posts to the main loop, which is
	// that next tick.
	case ShellEventQuit:
		// The user has answered a question about this quit, in words
		// about what it ends, so the plain "Quit Meerkat?" is not asked
		// on top of it.
		quitAgreed = true
		r.window.Update(quit)
	// Same reason for the next tick: the walk updates every window, and
	// this callback is inside one of those updates.
	case ShellEventQuitCancelled:
		r.window.Update(quitCancelled)
	}
}

// restoreFocus hands the keys back to the screen after the quit card
// goes: a workspace to its active tab, the way every other overlay of the
// shell gives them back.
func (r *Root) restoreFocus() {
	if r.screen.workspace != nil {
		r.screen.workspace.focusActiveTab()
		return
	}
	r.screen.connections.FocusHandle().Focus()
}

// FocusHandle is the Rust `Focusable for Root`.
func (r *Root) FocusHandle() *ui.Handle {
	if r.screen.workspace != nil {
		return r.screen.workspace.FocusHandle()
	}
	return r.screen.connections.FocusHandle()
}

// View is the Rust `Render for Root`.
func (r *Root) View(c *ui.Context) ui.Element {
	el := ui.Box(c).Fill().Children(func() {
		if r.screen.workspace != nil {
			r.screen.workspace.View(c)
		} else {
			r.screen.connections.View(c)
		}
		// Last, so it paints over every overlay the screen has.
		r.quitOverlay(c)
	})
	if action, ok := uikit.DispatchKeys(el, rootKeys); ok {
		switch action {
		case rootCloseWindow:
			r.onCloseWindow()
		}
	}
	return el
}
