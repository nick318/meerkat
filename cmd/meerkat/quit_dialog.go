// The "Quit Meerkat?" question, drawn by the app rather than by AppKit.
//
// It was an NSAlert: a system sheet in the system's look, which takes a
// moment to slide in and sits apart from everything else on screen. This
// is the shell's close confirmation in its own paper and ink — a scrim and
// a card, painted in the next frame — and it is drawn by the **window's
// root**, over whichever screen the window holds, because ⌘Q is asked on
// the connections screen as well as in a workspace.
//
// Answering is one key: ⏎ quits and ⎋ stays, and a second ⌘Q while the
// card is up quits too, the way holding to the gesture means it. A click
// on the scrim stays: dismissing loses nothing here, unlike the close
// confirmation, whose question is about ending server work.

package main

import (
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// quitDialogAction is what a key on the card asks for.
type quitDialogAction int

const (
	quitDialogQuit quitDialogAction = iota
	quitDialogStay
)

// quitDialogKeys is the card's own table, dispatched on the card, which
// holds the focus while it is up — so ⏎ answers it rather than reaching
// the SQL editor behind it.
var quitDialogKeys = []uikit.KeyBinding[quitDialogAction]{
	uikit.Bind("enter", quitDialogQuit),
	uikit.Bind("cmd-enter", quitDialogQuit),
	uikit.Bind("escape", quitDialogStay),
}

// quitDialogTitle and quitDialogBody are the card's words, one place so a
// test can read them.
const (
	quitDialogTitle = "Quit Meerkat?"
	quitDialogBody  = "Every window closes, and its connection with it. The tabs come back the next time you open the connection."
)

// confirmQuit puts the question up on the window the user is looking at
// and leaves the quit waiting for the answer. Asked while it is already
// up — a second ⌘Q — it is the answer: quit.
func confirmQuit() {
	if quitAsking {
		answerQuit(true)
		return
	}
	target := quitDialogWindow()
	if target == nil {
		// No window to ask on: nothing is open, so nothing is lost.
		quitAgreed = true
		quit()
		return
	}
	quitAsking = true
	target.root.quitAsk = true
	target.root.quitFocus.Focus()
	if target.window != nil {
		if !target.window.IsFocused() {
			target.window.Focus()
		}
		target.window.Invalidate()
	}
}

// focusedWindow is mygo's, a variable because it waits for a running app
// and a test has none.
var focusedWindow = mygo.FocusedWindow

// quitDialogWindow is the window the question goes on: the focused one,
// or the first window when none has the focus (⌘Q from the Dock).
func quitDialogWindow() *rootWindow {
	if len(roots) == 0 {
		return nil
	}
	if focused := focusedWindow(); focused != nil {
		for _, w := range roots {
			if w.window == focused {
				return w
			}
		}
	}
	return roots[0]
}

// answerQuit takes the card down from every window and carries the quit
// on, or calls it off.
func answerQuit(quitting bool) {
	if !quitAsking {
		return
	}
	quitAsking = false
	for _, w := range roots {
		if w.root.quitAsk {
			w.root.quitAsk = false
			if !quitting {
				// The keys go back to where the user left them, or the
				// window's bindings have nowhere to dispatch.
				w.root.restoreFocus()
			}
			if w.window != nil {
				w.window.Invalidate()
			}
		}
	}
	if !quitting {
		quitCancelled()
		return
	}
	quitAgreed = true
	quit()
}

// quitOverlay paints the card while the root has it up. The confirmation's
// own shapes — the scrim, the elevated card, the overlay shadow — so the
// two questions the app asks read as one family.
func (r *Root) quitOverlay(c *ui.Context) {
	if !r.quitAsk {
		return
	}
	colors := &theme.Current().Colors
	scrim := ui.Row(c.Key("quit-scrim")).
		Absolute().
		Top(0).
		Left(0).
		Fill().
		Justify(ui.Center).
		AlignItems(ui.Start).
		Padding(confirmTopMargin, 0, 0, 0).
		Background(colors.Overlay.Color())
	scrim.OnClick(func() { answerQuit(false) })
	scrim.Children(func() {
		card := ui.Column(c.Key("quit-card")).
			Focusable().
			TabStop(false).
			Bind(&r.quitFocus).
			Width(confirmWidth).
			Gap(10).
			Padding(18).
			Border(1, colors.BorderStrong.Color()).
			Radius(10).
			Background(colors.Elevated.Color()).
			Font(theme.UiFontFamily)
		// A click on the card is not a click on the scrim behind it.
		card.OnClick(func() {})
		overlayShadow(card, colors)
		if action, ok := uikit.DispatchKeys(card, quitDialogKeys); ok {
			answerQuit(action == quitDialogQuit)
		}
		card.Children(func() {
			ui.Text(c, quitDialogTitle).
				FontSize(theme.SizeEmphasis).
				FontWeight(600).
				TextColor(colors.Text.Color())
			ui.Text(c, quitDialogBody).
				FontSize(theme.SizeBody).
				TextColor(colors.TextSecondary.Color())
			ui.Row(c).
				AlignItems(ui.Center).
				Gap(8).
				Padding(4, 0, 0, 0).
				Children(func() {
					ui.Box(c).
						Grow(1).Basis(0).
						FontSize(theme.SizeMeta).
						TextColor(colors.TextFaint.Color()).
						Children(func() { ui.Text(c, "⏎ or ⌘Q quits · ⎋ stays") })
					stay := ui.Box(c.Key("quit-stay")).
						Padding(5, 12).
						Border(1, colors.BorderStrong.Color()).
						Radius(6).
						FontSize(theme.SizeBody).
						TextColor(colors.Text.Color()).
						Cursor(ui.CursorPointer)
					if stay.Hovered() {
						stay.Background(colors.Panel.Color())
					}
					stay.OnClick(func() { answerQuit(false) })
					stay.Children(func() { ui.Text(c, "Stay") })
					// The accent, filled: nothing is at stake, so this is
					// the ordinary answer and ⏎ gives it — not the clay a
					// button wears when it ends server work.
					act := ui.Box(c.Key("quit-act")).
						Padding(5, 12).
						Border(1, colors.AccentFill.Color()).
						Radius(6).
						Background(colors.AccentFill.Color()).
						FontSize(theme.SizeBody).
						FontWeight(500).
						TextColor(colors.OnAccent.Color()).
						Cursor(ui.CursorPointer)
					act.OnClick(func() { answerQuit(true) })
					act.Children(func() { ui.Text(c, "Quit") })
				})
		})
	})
}
