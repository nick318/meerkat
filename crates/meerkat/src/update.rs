//! What the updater looks like on screen: a toast in the corner, and a
//! version line pinned in the connections screen's own bottom-right
//! corner, under it.
//!
//! **The toast is news, so it can be closed.** It was a pill in the top
//! bar, and a pill is a permanent mark: an update that is ready stays
//! ready until the app restarts, so the bar carried "restart to update"
//! beside the environment badge for the rest of the session, with no way
//! to put it down. A restart is not something a viewer does mid-query,
//! which is exactly why nothing here restarts the app by itself — and a
//! notice the user cannot answer is one they learn to read past.
//!
//! So it arrives as a card over the bottom-right of the workspace, with
//! the restart on it and a × beside it. Closing it says "not now", and
//! `AutoUpdater::dismiss` keys that on the build being offered:
//! the same one stays closed for the rest of the run, a *later* install
//! announces itself in its turn, and a fresh run of the app says it
//! again, because a restart is the one thing that update is waiting for.
//! The dismissal lives in memory for that reason and is never written
//! down.
//!
//! **"updating…" is gone from the workspace with the pill.** A download
//! nobody asked for is background work with no answer to give, and a
//! toast that cannot be acted on is the thing the pill was wrong for.
//! The version line still narrates every state, because a person reading
//! the corner went looking for it.
//!
//! The version line is where the updater is *read*. It is the only place
//! the version and the channel are written, the only place a check can
//! be asked for by hand, and the only place a failed check reports — the
//! hourly check fails quietly, so an error here always answers a click
//! the user made. It does **not** offer the restart: that is the toast's
//! alone, or a card the user shut in the corner would go on shouting
//! from the corner of the connections screen.
//!
//! The restart goes through `main::restart_to_update`, which is ⌘Q's own
//! walk with a different last word — every window is asked about its runs
//! and its transactions first, because a restart ends them exactly as a
//! quit does.

use auto_update::{AutoUpdater, Status, Trigger};
use gpui::{AnyElement, App, BoxShadow, FontWeight, Pixels, div, prelude::*, px};
use release_channel::Channel;
use theme::ThemeColors;

/// The toast, for the corner of the workspace. `None` is the ordinary
/// case: nothing is ready, or the user has closed this one's card, or
/// this is a local build with no updater at all.
///
/// It takes no keys — a notice that arrives on its own must not take the
/// focus off whatever the user was typing into. Both of its targets are
/// clicks.
///
/// `bottom` is the caller's, because the two screens have different
/// things along their bottom edge: the workspace has its status strip to
/// clear, and the connections screen has nothing.
pub fn toast(colors: &ThemeColors, bottom: Pixels, cx: &mut App) -> Option<AnyElement> {
    let updater = AutoUpdater::try_global(cx)?;
    let ready = updater.read(cx).announcement()?;

    let mut headline = format!("Meerkat {} is ready", ready.version);
    if let Some(sha) = &ready.sha {
        headline = format!(
            "Meerkat {} ({}) is ready",
            ready.version,
            &sha[..sha.len().min(7)]
        );
    }

    let close = {
        let updater = updater.clone();
        div()
            .id("dismiss-update-toast")
            .flex_none()
            .px(px(3.))
            .text_size(px(12.))
            .text_color(colors.text_faint)
            .cursor_pointer()
            .hover(|style| style.text_color(colors.text))
            .on_click(move |_event, _window, cx| {
                updater.update(cx, |updater, cx| updater.dismiss(cx));
            })
            .child("×")
    };

    Some(
        div()
            .absolute()
            .bottom(bottom)
            .right(px(18.))
            .w(px(288.))
            .flex()
            .flex_col()
            .gap(px(8.))
            .p(px(14.))
            .border_1()
            .border_color(colors.border_strong)
            .rounded(px(10.))
            .bg(colors.elevated)
            .shadow(vec![
                BoxShadow::new(px(0.), px(18.), colors.shadow)
                    .blur_radius(px(48.))
                    .spread_radius(px(-16.)),
            ])
            // The headline and the × share a row, and the headline is the
            // half that gives: `min_w(0.)` is what lets it wrap inside the
            // card instead of growing the row and pushing the × out over
            // the edge — a flex child's floor is its content otherwise.
            .child(
                div()
                    .flex()
                    .items_start()
                    .gap(px(8.))
                    .child(
                        div()
                            .flex_1()
                            .min_w(px(0.))
                            .text_size(px(12.))
                            .font_weight(FontWeight::SEMIBOLD)
                            .text_color(colors.text)
                            .child(headline),
                    )
                    .child(close),
            )
            // One short line, and the card's own width is what keeps it
            // short. What a restart asks about first is the close dialog's
            // to say, at the moment it asks.
            .child(
                div()
                    .text_size(px(10.))
                    .text_color(colors.text_muted)
                    .child("a restart picks it up"),
            )
            .child(
                div()
                    .id("restart-to-update")
                    .flex_none()
                    .self_start()
                    .px(px(9.))
                    .py(px(4.))
                    .rounded(px(5.))
                    .bg(colors.accent)
                    .text_size(px(10.))
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_color(colors.window)
                    .cursor_pointer()
                    .hover(|style| style.bg(colors.accent_deep))
                    .on_click(|_event, _window, cx| crate::restart_to_update(cx))
                    .child("restart now"),
            )
            .into_any_element(),
    )
}

/// The version-and-updates line, pinned in the connections screen's
/// bottom-right corner: `meerkat 0.1.0 · dev abc1234`, and under it
/// whatever the updater has to say. It was inline in the list's footer,
/// which put app chrome in the row that says what the *list* answers to.
///
/// **Two rows, not one.** The version and the updater's word are two
/// different things — one says what this build is, the other offers to
/// change it — and side by side they read as one sentence in two inks.
/// Stacked, each is its own line, and the corner keeps the same width
/// whatever the updater says: an error is often longer than the version,
/// and on one row it pushed the identity leftward as the status changed.
/// The rows align to the right, because that is the edge they are pinned
/// to.
pub fn foot_summary(colors: &ThemeColors, cx: &mut App) -> AnyElement {
    let channel = release_channel::channel();

    let mut identity = format!("meerkat {}", release_channel::version());
    if channel != Channel::Public {
        identity.push_str(" · ");
        identity.push_str(channel.dev_name());
    }
    if let Some(sha) = release_channel::commit_sha() {
        identity.push(' ');
        identity.push_str(sha);
    }

    let line = div().flex().flex_col().items_end().gap(px(2.)).child(
        div()
            .text_size(px(10.))
            .text_color(colors.text_faint)
            .child(identity),
    );

    let Some(updater) = AutoUpdater::try_global(cx) else {
        // A local build: the version is worth a line, an update story is
        // not — updates come from `cargo build`.
        return line.into_any_element();
    };

    let check = |label: &'static str| {
        let updater = updater.clone();
        div()
            .id("check-for-updates")
            .text_size(px(10.))
            .text_color(colors.accent)
            .cursor_pointer()
            .hover(|style| style.text_color(colors.accent_deep))
            .on_click(move |_event, _window, cx| {
                updater.update(cx, |updater, cx| updater.check(Trigger::Manual, cx));
            })
            .child(label)
    };

    let faint = |text: &'static str| {
        div()
            .text_size(px(10.))
            .text_color(colors.text_faint)
            .child(text)
    };

    match updater.read(cx).status().clone() {
        Status::Idle => line.child(check("check for updates")),
        Status::Checking => line.child(faint("checking…")),
        Status::Updating => line.child(faint("updating…")),
        // **The toast is the only place the restart is offered.** The
        // footer used to carry it too, and that made the offer as
        // un-closable as the pill was: a card the user shut in the corner
        // was still shouting from the foot of the connections screen. It
        // says nothing here instead — not even "check for updates", which
        // `check` refuses while an install is `Ready`, and a link that
        // does nothing is worse than no link.
        Status::Ready { .. } => line,
        // The failure and its retry share the second row: the retry is
        // the answer to the error, so it sits beside it.
        Status::Errored { error } => line.child(
            div()
                .flex()
                .items_center()
                .gap(px(8.))
                .child(
                    div()
                        .max_w(px(360.))
                        .truncate()
                        .text_size(px(10.))
                        .text_color(colors.error)
                        .child(format!("update failed: {error}")),
                )
                .child(check("retry")),
        ),
    }
    .into_any_element()
}
