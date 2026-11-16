// Meerkat entry point: boot mygo, load bundled fonts, install the
// warm-paper theme, open the first window on a database.
//
// Usage: `meerkat postgres://user@host/db`, or set `MEERKAT_DATABASE_URL`.
// With neither, the window opens on the connections screen, which lists
// the saved connections and takes new ones.
//
// **There can be more than one window, and ⌘N opens one** — on the
// connections screen, as a browser's new window opens on nothing. A window
// is a `Root` and nothing else: each one holds its own screen, its own
// session and its own tabs, so two windows can sit on two databases at
// once. Nothing is shared between them but the theme, the key bindings and
// the local SQLite file.
//
// The command line names a database for the **first** window only. A second
// window was asked for by hand, so it opens where the user can choose.

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/autoupdate"
	"github.com/nick318/meerkat/internal/dbpostgres"
	"github.com/nick318/meerkat/internal/secrets"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// appAction is the Rust `actions!(meerkat, [Quit, NewWindow])`.
type appAction int

const (
	appQuit appAction = iota
	appNewWindow
)

// windowCascade is how far a new window is offset from the one before it,
// so it does not land exactly on top of it — a window nobody can see the
// edge of reads as no new window at all.
const windowCascade float32 = 28.

// windowCascadeSteps is how many steps the cascade takes before it starts
// over. Without it the tenth window walks off the screen.
const windowCascadeSteps int = 6

// windowWidth and windowHeight are the Rust `size(px(1360.), px(880.))`.
const (
	windowWidth  = 1360
	windowHeight = 880
)

// ShellKeyBindings is the workspace's half of the table `main.rs` bound.
//
// The workspace's keys are scoped to the workspace, not bound globally, so
// the palette can take ⌘⏎ for itself while it is open: GPUI gave a
// keystroke to the binding that matched deepest in the context stack, and
// the palette sits inside the shell. mygo does the same, so the shell
// dispatches this table on the element that wears `ShellKeyContext`.
func ShellKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("cmd-enter", ShellRunQuery),
		// ⌘. stops the run the way it does in psql's siblings: once to
		// ask the statement to give up, again to close the backend.
		uikit.Bind("cmd-.", ShellStopQuery),
		// The two ends of a transaction. ⌘S is the comp's own key and
		// means here what it means everywhere: make this permanent.
		//
		// **The rollback key is not the comp's ⇧⌘Z.** That is Redo in
		// the SQL editor, whose context sits *inside* the shell's, so a
		// binding here would never fire while the user is typing — and
		// taking redo off a text editor would be the wrong trade even
		// if it did. ⇧⌘R is free, and it is the letter the word starts
		// with.
		uikit.Bind("cmd-s", ShellCommitTransaction),
		uikit.Bind("cmd-shift-r", ShellRollbackTransaction),
		uikit.Bind("cmd-t", ShellNewQuery),
		uikit.Bind("cmd-w", ShellCloseTab),
		uikit.Bind("cmd-r", ShellRefresh),
		uikit.Bind("cmd-y", ShellShowHistory),
		uikit.Bind("cmd-[", ShellPrevPage),
		uikit.Bind("cmd-]", ShellNextPage),
		// ⌃⇥ switches on the keystroke, with no popup in between, so
		// both keys are bound to the shell and nowhere else.
		uikit.Bind("ctrl-tab", ShellNextTab),
		uikit.Bind("ctrl-shift-tab", ShellPrevTab),
		// A browser's other tab keys, because a strip of tabs is read
		// as a browser's and ⌘⇥ is the system's, never the app's: the
		// app switcher takes it before any window hears it. ⌘⌥←→ walk
		// the strip like ⌃⇥, and ⌘1 … ⌘8 jump to a place in it, ⌘9 to
		// its end. Neither the editor nor the grid binds any of them.
		uikit.Bind("cmd-alt-right", ShellNextTab),
		uikit.Bind("cmd-alt-left", ShellPrevTab),
		uikit.Bind("cmd-1", ShellGoToTab1),
		uikit.Bind("cmd-2", ShellGoToTab2),
		uikit.Bind("cmd-3", ShellGoToTab3),
		uikit.Bind("cmd-4", ShellGoToTab4),
		uikit.Bind("cmd-5", ShellGoToTab5),
		uikit.Bind("cmd-6", ShellGoToTab6),
		uikit.Bind("cmd-7", ShellGoToTab7),
		uikit.Bind("cmd-8", ShellGoToTab8),
		uikit.Bind("cmd-9", ShellGoToLastTab),
	}
}

// GridKeyBindings is the grid's half of the table `main.rs` bound.
//
// The grid's own keys, in the grid's own context — never the shell's. The
// SQL editor's element sits *inside* the shell's, so a key bound to the
// shell is matched before the keystroke can reach the editor's text input:
// a bare `space` bound there would eat the spaces out of the user's SQL.
// `ResultGrid` is in the stack only while the grid holds the focus, which a
// table tab does from the moment it opens and a query tab does from the
// first click in its result.
func GridKeyBindings() []uikit.KeyBinding[ShellAction] {
	return []uikit.KeyBinding[ShellAction]{
		uikit.Bind("up", ShellSelectUp),
		uikit.Bind("down", ShellSelectDown),
		uikit.Bind("left", ShellSelectLeft),
		uikit.Bind("right", ShellSelectRight),
		uikit.Bind("shift-up", ShellExtendUp),
		uikit.Bind("shift-down", ShellExtendDown),
		uikit.Bind("shift-left", ShellExtendLeft),
		uikit.Bind("shift-right", ShellExtendRight),
		// ⌘ with an arrow means "as far as it goes" on this platform,
		// and it means the same here.
		uikit.Bind("cmd-left", ShellSelectRowStart),
		uikit.Bind("cmd-right", ShellSelectRowEnd),
		uikit.Bind("cmd-up", ShellSelectFirstRow),
		uikit.Bind("cmd-down", ShellSelectLastRow),
		uikit.Bind("cmd-a", ShellSelectAll),
		uikit.Bind("cmd-c", ShellCopySelection),
		// Space ticks the row the cursor is on: ↓ then space walks a
		// result and picks out of it without the mouse.
		uikit.Bind("space", ShellTogglePick),
		uikit.Bind("escape", ShellClearSelection),
	}
}

// appKeyBindings is the part of `main.rs`'s table bound with no context:
// it fires wherever the focus is, last in line.
func appKeyBindings() []uikit.KeyBinding[appAction] {
	return []uikit.KeyBinding[appAction]{
		uikit.Bind("cmd-q", appQuit),
		// A window, like a browser's ⌘N. Bound globally on purpose:
		// it means the same thing on the connections screen and inside
		// a session, and nothing else in the app takes the key.
		uikit.Bind("cmd-n", appNewWindow),
	}
}

var appKeys = appKeyBindings()

// roots holds every open window's Root — what `cx.windows()` downcast to
// `Root` was. Main thread only.
var roots []*rootWindow

type rootWindow struct {
	window *mygo.Window
	root   *Root
}

func main() {
	var target Target
	if url, ok := databaseURL(); ok {
		target = TargetUrl{Url: url}
	}

	// The bundled fonts, before any window draws text.
	if err := uikit.RegisterFonts(); err != nil {
		panic(fmt.Sprintf("failed to load bundled fonts: %v", err))
	}

	theme.SetGlobal(theme.WarmPaper())

	// Saved profiles read their password out of the keychain.
	dbpostgres.KeychainPassword = secrets.GetPassword
	// The update toast's "restart now".
	updateRestart = restartToUpdate

	// The menu bar, before the app is ready, so mygo's default one is
	// never installed: see `appMenu`.
	mygo.App.SetMenu(mygo.NewMenu(appMenu()))

	mygo.App.WhenReady(func() {
		// The updater exists only on a distributed channel; a plain `go
		// build` is a `local` build and gets none. What `cx.observe` did
		// for each screen is one callback here: every window repaints.
		autoupdate.Init(func() {
			mygo.RunOnMain(func() {
				for _, w := range roots {
					w.window.Invalidate()
				}
			})
		})

		// ⌘Q, from the key or the app menu: the walk in `quit` decides.
		mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) {
			if e.SessionEnding {
				// The OS is ending the session and will not wait: save
				// what can be saved, synchronously.
				for _, w := range roots {
					w.root.remember()
				}
				return
			}
			if quitReady {
				return
			}
			e.PreventDefault()
			quitWalk()
		})

		openWindow(target)
	})

	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "meerkat:", err)
		os.Exit(1)
	}
}

// openWindow opens one window on a target, or on the connections screen
// with none.
//
// Every window is built here — the first one and every ⌘N after it — so the
// guard on the close button and the cascade cannot be true of one window
// and forgotten on the next.
func openWindow(target Target) {
	step := len(roots) % windowCascadeSteps
	x, y := 0, 0 // centered, as `Bounds::centered(None, ..)`
	if step > 0 {
		x, y = centeredOrigin()
		offset := int(windowCascade * float32(step))
		x += offset
		y += offset
	}

	entry := &rootWindow{}
	view := func(c *ui.Context) {
		c.Root().Background(theme.Current().Colors.Window.Color()).Font(theme.UiFontFamily)
		if action, ok := uikit.DispatchWindowKeys(c, appKeys); ok {
			switch action {
			case appQuit:
				quit()
			case appNewWindow:
				// On the connections screen, whatever this window is on:
				// ⌘N asks for a window, not for a second view of this
				// database.
				openWindow(nil)
			}
		}
		if entry.root != nil {
			entry.root.View(c)
		}
	}
	// The screens draw their own title bar, so the tab strip can sit where
	// the system's empty one was: three bars of chrome became one.
	// `uikit.TitleBar` moves the window.
	window := mygo.NewWindow(mygo.WindowOptions{
		Title:                "Meerkat",
		Width:                windowWidth,
		Height:               windowHeight,
		X:                    x,
		Y:                    y,
		TitleBarStyle:        mygo.TitleBarHidden,
		TrafficLightPosition: &mygo.Point{X: 14, Y: 15},
		Content:              ui.View(view),
	})
	entry.window = window
	// The screen takes the focus from the first frame (NewRoot focuses
	// it), or the key bindings have nowhere to dispatch.
	entry.root = NewRoot(target, window)
	roots = append(roots, entry)

	// The platform wants a yes or no on the spot, and the question takes a
	// person to answer. So a close with runs out answers "no" and puts the
	// dialog up; agreeing to it closes the window from there.
	//
	// A session's tabs are written back as they change, but the last
	// keystrokes in an editor are not, so the way out saves first.
	window.OnClose(func(e *mygo.CloseEvent) {
		if !quitReady && !entry.root.guardCloseWindow() {
			e.PreventDefault()
			return
		}
		entry.root.remember()
	})
	// Every way a window goes ends here — the close button after its
	// guard, and the `Destroy` a dialog's "end and close" or ⌘W on the
	// connections screen calls, which skips `OnClose`. GPUI dropped the
	// window's entities; Go lets the shell go by hand.
	window.OnClosed(func() {
		entry.root.release()
		forgetWindow(entry)
	})
	window.Invalidate()
}

// forgetWindow takes a closed window out of `roots`. With no window the
// app has nothing left to be; mygo quits on its own when the last window
// goes, which is what `cx.on_window_closed` did.
func forgetWindow(entry *rootWindow) {
	for i, w := range roots {
		if w == entry {
			roots = append(roots[:i], roots[i+1:]...)
			return
		}
	}
}

// centeredOrigin is the top-left corner of a window of the default size
// centered on the primary display's work area.
func centeredOrigin() (int, int) {
	wa := mygo.Screen.PrimaryDisplay().WorkArea
	return wa.X + (wa.Width-windowWidth)/2, wa.Y + (wa.Height-windowHeight)/2
}

// appMenu is the menu bar. The Rust app had none: GPUI installs nothing
// unless asked, so every key reached the window's own bindings.
//
// mygo installs a default one, and on macOS a menu item's key equivalent
// is matched **before** the window sees the key. Its File menu carried
// "Close Window ⌘W", so ⌘W closed the window — and with it, on the last
// window, the app — instead of the tab the strip answers ⌘W with. Its View
// menu took ⇧⌘R, which rolls a transaction back here. So the bar is ours,
// and it carries no item whose key the window binds:
//
//   - the app menu, whose Quit goes through `OnBeforeQuit` and so through
//     `quitWalk`, the same as ⌘Q;
//   - the Edit menu, whose items AppKit routes to the focused element as
//     edit commands — copy and paste from the menu, and dictation;
//   - full screen, and the Window menu.
func appMenu() []*mygo.MenuItem {
	return []*mygo.MenuItem{
		{Role: mygo.RoleAppMenu},
		{Role: mygo.RoleEditMenu},
		{Label: "View", Submenu: []*mygo.MenuItem{{Role: mygo.RoleToggleFullScreen}}},
		{Role: mygo.RoleWindowMenu},
	}
}

// quitAgreed says the user has already answered a question about this
// quit — a window's own dialog about its runs and transactions — so the
// plain "Quit Meerkat?" is not asked on top of it.
var quitAgreed bool

// quitAsking says the "Quit Meerkat?" question is on screen, so a second
// ⌘Q under it does not put up a second one.
var quitAsking bool

// needsQuitConfirm says whether a quit every window has agreed to still
// asks the user. **Every quit asks**, because ⌘Q sits one key away from ⌘W
// and ends every window at once; a window with something at stake has
// asked already, in words about what is at stake. A restart into an update
// was asked for by a click on "restart now", so it does not ask again. And
// with no window left the quit is the last window's close, which the user
// already chose — mygo quits when the last window goes, as GPUI's
// `on_window_closed` did — so there is nothing left to ask about.
func needsQuitConfirm(restarting, agreed bool, windows int) bool {
	return windows > 0 && !restarting && !agreed
}

// confirmQuit puts the question up and carries the quit on from the
// answer. The dialog blocks until it is answered, so it is shown from a
// goroutine and the answer comes back on the main thread.
func confirmQuit() {
	if quitAsking {
		return
	}
	quitAsking = true
	go func() {
		result, err := mygo.Dialog.Message(mygo.MessageOptions{
			Type:          mygo.MessageQuestion,
			Message:       "Quit Meerkat?",
			Detail:        "Every window closes, and its connection with it. The tabs come back the next time you open the connection.",
			Buttons:       []string{"Quit", "Cancel"},
			DefaultButton: 0,
			CancelButton:  1,
		})
		mygo.RunOnMain(func() {
			quitAsking = false
			if err != nil || result.Button != 0 {
				quitCancelled()
				return
			}
			quitAgreed = true
			quit()
		})
	}()
}

// quitCancelled calls a quit off, at every window that had already agreed
// to it.
func quitCancelled() {
	// The windows that agreed before the "stay" agreed to *this* ending,
	// and this ending is not happening — a restart asked for later must
	// not inherit it either.
	restarting = false
	quitAgreed = false
	for _, w := range roots {
		w.root.forgetQuit()
	}
}

// quit quits, if every window agrees. It goes through mygo's own quit, so
// the app menu's Quit and ⌘Q take the same walk: `OnBeforeQuit` calls
// `quitWalk`.
func quit() {
	mygo.App.Quit()
}

// quitReady says the walk has finished and every cancel has been waited
// for; the quit that follows goes through without being asked again.
var quitReady bool

// quitWalk is the Rust `quit`.
//
// ⌘Q ends every window's work at once, so **every** window is asked, and
// the first one with something at stake puts the question on screen and
// stops the walk. Agreeing to it starts the walk again — that window then
// answers yes — so the user is asked once per window and the app goes only
// when the last of them has said so.
//
// The cancels are waited for. Quitting ends the process, so a
// `pg_cancel_backend` that has not left yet never leaves, and the statement
// outlives the app that started it.
func quitWalk() {
	windows := append([]*rootWindow(nil), roots...)
	for _, w := range windows {
		if !w.root.guardQuit() {
			return
		}
	}
	if needsQuitConfirm(restarting, quitAgreed, len(windows)) {
		confirmQuit()
		return
	}
	var stops []*StopTask
	for _, w := range windows {
		w.root.remember()
		stops = append(stops, w.root.stopRuns()...)
	}
	go func() {
		for _, stop := range stops {
			_, _ = stop.Wait()
		}
		mygo.RunOnMain(func() {
			quitReady = true
			// A restart is ⌘Q's walk with a different last word: the same
			// questions were asked, the same cancels were waited for, and
			// only what happens after the last window agrees differs.
			if restarting {
				mygo.App.Relaunch()
			} else {
				mygo.App.Quit()
			}
		})
	}()
}

// restarting says the quit under way is really a restart into an installed
// update. Read once, at the end of the quit walk; a cancelled quit clears
// it, so the next plain ⌘Q does not relaunch the app.
var restarting bool

// restartToUpdate restarts into the update the updater has already laid
// over the bundle. It is the quit walk end to end — every window is asked
// about its runs and its transactions first — so nothing ends without the
// user saying so twice being needed.
func restartToUpdate() {
	restarting = true
	quit()
}

// databaseURL: the first non-flag argument wins, then the environment.
func databaseURL() (string, bool) {
	url := ""
	found := false
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-") {
			url, found = arg, true
			break
		}
	}
	if !found {
		url, found = os.LookupEnv("MEERKAT_DATABASE_URL")
	}
	if !found || url == "" {
		return "", false
	}
	return url, true
}
