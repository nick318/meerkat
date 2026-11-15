// What the updater looks like on screen: a toast in the corner, and a
// version line pinned in the connections screen's own bottom-right
// corner, under it.
//
// **The toast is news, so it can be closed.** It was a pill in the top
// bar, and a pill is a permanent mark: an update that is ready stays
// ready until the app restarts, so the bar carried "restart to update"
// beside the environment badge for the rest of the session, with no way
// to put it down. A restart is not something a viewer does mid-query,
// which is exactly why nothing here restarts the app by itself — and a
// notice the user cannot answer is one they learn to read past.
//
// So it arrives as a card over the bottom-right of the workspace, with
// the restart on it and a × beside it. Closing it says "not now", and
// `AutoUpdater::dismiss` keys that on the build being offered:
// the same one stays closed for the rest of the run, a *later* install
// announces itself in its turn, and a fresh run of the app says it
// again, because a restart is the one thing that update is waiting for.
// The dismissal lives in memory for that reason and is never written
// down.
//
// **"updating…" is gone from the workspace with the pill.** A download
// nobody asked for is background work with no answer to give, and a
// toast that cannot be acted on is the thing the pill was wrong for.
// The version line still narrates every state, because a person reading
// the corner went looking for it.
//
// The version line is where the updater is *read*. It is the only place
// the version and the channel are written, the only place a check can
// be asked for by hand, and the only place a failed check reports — the
// hourly check fails quietly, so an error here always answers a click
// the user made. It does **not** offer the restart: that is the toast's
// alone, or a card the user shut in the corner would go on shouting
// from the corner of the connections screen.
//
// The restart goes through `main::restart_to_update`, which is ⌘Q's own
// walk with a different last word — every window is asked about its runs
// and its transactions first, because a restart ends them exactly as a
// quit does.

package main

import (
	"fmt"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/autoupdate"
	"github.com/nick318/meerkat/internal/releasechannel"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// updateRestart is `crate::restart_to_update`: ⌘Q's walk with a restart
// as its last word. The Rust toast called it straight; main.go is not this
// file's to write, so main.go sets this when it starts, and until it does
// the button does nothing.
var updateRestart func()

// toast is the toast, for the corner of the workspace (Rust:
// `update::toast`). False is the ordinary case: nothing is ready, or the
// user has closed this one's card, or this is a local build with no
// updater at all — and then nothing is built.
//
// It takes no keys — a notice that arrives on its own must not take the
// focus off whatever the user was typing into. Both of its targets are
// clicks.
//
// `bottom` is the caller's, because the two screens have different
// things along their bottom edge: the workspace has its status strip to
// clear, and the connections screen has nothing.
//
// The restart button calls updateRestart, which main.go sets to its
// `restart_to_update`.
func toast(c *ui.Context, colors *theme.ThemeColors, bottom float32) (ui.Element, bool) {
	updater := autoupdate.TryGlobal()
	if updater == nil {
		return ui.Element{}, false
	}
	ready := updater.Announcement()
	if ready == nil {
		return ui.Element{}, false
	}

	headline := fmt.Sprintf("Meerkat %s is ready", ready.Version)
	if ready.Sha != nil {
		sha := *ready.Sha
		headline = fmt.Sprintf(
			"Meerkat %s (%s) is ready",
			ready.Version,
			sha[:min(len(sha), 7)],
		)
	}

	close := func() {
		el := ui.Box(c.Key("dismiss-update-toast")).
			Shrink(0).
			Padding(0, 3).
			FontSize(12).
			TextColor(colors.TextFaint.Color()).
			Cursor(ui.CursorPointer).
			Children(func() { ui.Text(c, "×") })
		if el.Hovered() {
			el.TextColor(colors.Text.Color())
		}
		el.OnClick(func() { updater.Dismiss() })
	}

	card := ui.Column(c).
		Absolute().
		Bottom(bottom).
		Right(18).
		Width(288).
		Gap(8).
		Padding(14).
		Border(1, colors.BorderStrong.Color()).
		Radius(10).
		Background(colors.Elevated.Color())
	uikit.BoxShadow{OffsetX: 0, OffsetY: 18, BlurRadius: 48, SpreadRadius: -16, Color: colors.Shadow}.Apply(card)
	card.Children(func() {
		// The headline and the × share a row, and the headline is the
		// half that gives: `min_w(0.)` is what lets it wrap inside the
		// card instead of growing the row and pushing the × out over
		// the edge — a flex child's floor is its content otherwise.
		ui.Row(c).
			AlignItems(ui.Start).
			Gap(8).
			Children(func() {
				ui.Box(c).
					Grow(1).Basis(0).
					MinWidth(0).
					FontSize(12).
					FontWeight(600).
					TextColor(colors.Text.Color()).
					Children(func() { ui.Text(c, headline) })
				close()
			})
		// One short line, and the card's own width is what keeps it
		// short. What a restart asks about first is the close dialog's
		// to say, at the moment it asks.
		ui.Box(c).
			FontSize(10).
			TextColor(colors.TextMuted.Color()).
			Children(func() { ui.Text(c, "a restart picks it up") })
		button := ui.Box(c.Key("restart-to-update")).
			Shrink(0).
			AlignSelf(ui.Start).
			Padding(4, 9).
			Radius(5).
			Background(colors.Accent.Color()).
			FontSize(10).
			FontWeight(600).
			TextColor(colors.Window.Color()).
			Cursor(ui.CursorPointer).
			Children(func() { ui.Text(c, "restart now") })
		if button.Hovered() {
			button.Background(colors.AccentDeep.Color())
		}
		button.OnClick(func() {
			if updateRestart != nil {
				updateRestart()
			}
		})
	})
	return card, true
}

// footSummary is the version-and-updates line, pinned in the
// connections screen's bottom-right corner: `meerkat 0.1.0 · dev abc1234`,
// and under it whatever the updater has to say (Rust:
// `update::foot_summary`). It was inline in the list's footer, which put
// app chrome in the row that says what the *list* answers to.
//
// **Two rows, not one.** The version and the updater's word are two
// different things — one says what this build is, the other offers to
// change it — and side by side they read as one sentence in two inks.
// Stacked, each is its own line, and the corner keeps the same width
// whatever the updater says: an error is often longer than the version,
// and on one row it pushed the identity leftward as the status changed.
// The rows align to the right, because that is the edge they are pinned
// to.
func footSummary(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	channel := releasechannel.Current()

	identity := fmt.Sprintf("meerkat %s", releasechannel.Version())
	if channel != releasechannel.Public {
		identity += " · "
		identity += channel.DevName()
	}
	if sha, ok := releasechannel.CommitSHA(); ok {
		identity += " "
		identity += sha
	}

	updater := autoupdate.TryGlobal()

	check := func(label string) {
		el := ui.Box(c.Key("check-for-updates")).
			FontSize(10).
			TextColor(colors.Accent.Color()).
			Cursor(ui.CursorPointer).
			Children(func() { ui.Text(c, label) })
		if el.Hovered() {
			el.TextColor(colors.AccentDeep.Color())
		}
		el.OnClick(func() { updater.Check(autoupdate.TriggerManual) })
	}

	faint := func(text string) {
		ui.Box(c).
			FontSize(10).
			TextColor(colors.TextFaint.Color()).
			Children(func() { ui.Text(c, text) })
	}

	return ui.Column(c).
		AlignItems(ui.End).
		Gap(2).
		Children(func() {
			ui.Box(c).
				FontSize(10).
				TextColor(colors.TextFaint.Color()).
				Children(func() { ui.Text(c, identity) })

			if updater == nil {
				// A local build: the version is worth a line, an update
				// story is not — updates come from `cargo build`.
				return
			}

			switch status := updater.Status().(type) {
			case autoupdate.StatusIdle:
				check("check for updates")
			case autoupdate.StatusChecking:
				faint("checking…")
			case autoupdate.StatusUpdating:
				faint("updating…")
			// **The toast is the only place the restart is offered.** The
			// footer used to carry it too, and that made the offer as
			// un-closable as the pill was: a card the user shut in the
			// corner was still shouting from the foot of the connections
			// screen. It says nothing here instead — not even "check for
			// updates", which `check` refuses while an install is `Ready`,
			// and a link that does nothing is worse than no link.
			case autoupdate.StatusReady:
			// The failure and its retry share the second row: the retry
			// is the answer to the error, so it sits beside it.
			case autoupdate.StatusErrored:
				ui.Row(c).
					AlignItems(ui.Center).
					Gap(8).
					Children(func() {
						ui.Box(c).
							MaxWidth(360).
							FontSize(10).
							TextColor(colors.Error.Color()).
							Children(func() {
								ui.Text(c, fmt.Sprintf("update failed: %s", status.Error)).SingleLine()
							})
						check("retry")
					})
			}
		})
}
