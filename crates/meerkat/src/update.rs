//! What the updater looks like on screen: a pill in the shell's top bar,
//! and a line in the connections screen's footer.
//!
//! The pill appears only when there is something to say — "updating…"
//! while the download lays the new bundle down, "restart to update" once
//! it is ready. An idle updater is the ordinary case and paints nothing:
//! a mark that is always there saying nothing teaches the eye to skip it.
//!
//! The footer line is the updater's home. It is the only place the
//! version and the channel are written, the only place a check can be
//! asked for by hand, and the only place a failed check reports — the
//! hourly check fails quietly, so an error here always answers a click
//! the user made.
//!
//! The restart goes through `main::restart_to_update`, which is ⌘Q's own
//! walk with a different last word — every window is asked about its runs
//! and its transactions first, because a restart ends them exactly as a
//! quit does.

use auto_update::{AutoUpdater, Status, Trigger};
use gpui::{AnyElement, App, FontWeight, div, prelude::*, px};
use release_channel::Channel;
use theme::ThemeColors;

/// The top bar's pill. `None` is the ordinary case: idle and checking
/// paint nothing there, and a local build has no updater at all.
pub fn top_bar_pill(colors: &ThemeColors, cx: &mut App) -> Option<AnyElement> {
    let updater = AutoUpdater::try_global(cx)?;
    match updater.read(cx).status() {
        Status::Updating => Some(
            div()
                .flex_none()
                .text_size(px(10.))
                .text_color(colors.text_faint)
                .child("updating…")
                .into_any_element(),
        ),
        Status::Ready { .. } => Some(
            div()
                .id("restart-to-update")
                .flex_none()
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
                .child("restart to update")
                .into_any_element(),
        ),
        Status::Idle | Status::Checking | Status::Errored { .. } => None,
    }
}

/// The footer's version-and-updates line, for the connections screen:
/// `meerkat 0.1.0 · dev abc1234`, then whatever the updater has to say.
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

    let line = div()
        .flex()
        .items_center()
        .gap(px(8.))
        .child(div().text_size(px(10.)).text_color(colors.text_faint).child(identity));

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
        div().text_size(px(10.)).text_color(colors.text_faint).child(text)
    };

    match updater.read(cx).status().clone() {
        Status::Idle => line.child(check("check for updates")),
        Status::Checking => line.child(faint("checking…")),
        Status::Updating => line.child(faint("updating…")),
        Status::Ready { version, sha } => {
            let mut label = format!("restart to update to {version}");
            if let Some(sha) = sha {
                label.push(' ');
                label.push_str(&sha[..sha.len().min(7)]);
            }
            line.child(
                div()
                    .id("restart-to-update-foot")
                    .text_size(px(10.))
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_color(colors.accent)
                    .cursor_pointer()
                    .hover(|style| style.text_color(colors.accent_deep))
                    .on_click(|_event, _window, cx| crate::restart_to_update(cx))
                    .child(label),
            )
        }
        Status::Errored { error } => line
            .child(
                div()
                    .max_w(px(360.))
                    .truncate()
                    .text_size(px(10.))
                    .text_color(colors.error)
                    .child(format!("update failed: {error}")),
            )
            .child(check("retry")),
    }
    .into_any_element()
}
