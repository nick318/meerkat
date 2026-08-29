//! The caret's blink, for every buffer that draws one.
//!
//! Two things paint a caret — the form's `TextField` and the multi-line
//! `sql_editor` — and a caret that blinks at one rate in the connection
//! form and holds still in the SQL pane reads as two different apps. So
//! the cycle lives here once and both own a `Blink`.
//!
//! **The blink is a timer, not an animation.** GPUI's `with_animation`
//! asks for a frame every frame it runs, and a caret is on screen for as
//! long as a window is open; a timer that fires twice a second wakes the
//! window twice a second and no more. Nothing here paints: the owner
//! reads `Blink::on` while it builds its quads, and a caret it decides
//! not to paint at all — an unfocused field — costs nothing either way.
//!
//! **The caret is solid while the user is working.** Every edit and
//! every motion calls `restart_blink`, so the caret goes on and its
//! cycle starts over. A caret blinking under a held-down arrow key is a
//! caret the eye cannot follow to where it has moved to.

use gpui::Context;
use std::time::Duration;

/// How long the caret stays on, and then off. macOS blinks a caret at
/// roughly this rate, and a field that matches it reads as a real one.
const INTERVAL: Duration = Duration::from_millis(530);

/// Where a caret is in its cycle.
pub struct Blink {
    /// Whether the caret is in the on half of the cycle. A caret that
    /// never goes out reads as a window that has stopped answering.
    on: bool,
    /// Counts the cycles, so a cycle that has been replaced stops
    /// instead of fighting the one that replaced it. Typing restarts the
    /// cycle; two of them would beat against each other.
    epoch: usize,
    /// Whether the owner held the focus at the last paint. The cycle is
    /// started and stopped from that edge, because focus is a property
    /// of the window and only the render pass has one.
    focused: bool,
}

impl Default for Blink {
    fn default() -> Self {
        Self {
            on: true,
            epoch: 0,
            focused: false,
        }
    }
}

impl Blink {
    /// Whether to paint the caret this frame.
    pub fn on(&self) -> bool {
        self.on
    }
}

/// What a caret's owner has to say for `Blink` to drive it: where the
/// state lives. Everything else comes with the trait.
pub trait Blinking: Sized + 'static {
    fn blink(&self) -> &Blink;
    fn blink_mut(&mut self) -> &mut Blink;

    /// Show the caret and start its cycle over. Called when the buffer
    /// takes the focus and after every edit or motion, so the caret is
    /// solid while the user is working and only blinks once they stop —
    /// the way a caret behaves in every platform field.
    fn restart_blink(&mut self, cx: &mut Context<Self>) {
        let blink = self.blink_mut();
        blink.on = true;
        blink.epoch += 1;
        let epoch = blink.epoch;
        schedule(epoch, cx);
    }

    /// Stop the cycle and leave the caret on, for a buffer that has lost
    /// the focus. An unfocused buffer paints no caret at all, so what
    /// matters here is that the timer stops waking the window.
    fn stop_blink(&mut self) {
        let blink = self.blink_mut();
        blink.on = true;
        blink.epoch += 1;
    }

    /// Note whether the buffer holds the focus this frame, and start or
    /// stop the cycle on the edge. Call it from `render`, which is the
    /// one place with a window to ask.
    fn track_blink_focus(&mut self, focused: bool, cx: &mut Context<Self>) {
        if focused == self.blink().focused {
            return;
        }
        self.blink_mut().focused = focused;
        if focused {
            self.restart_blink(cx);
        } else {
            self.stop_blink();
        }
    }
}

/// Turn the caret over once, then queue the next turn. The epoch is what
/// ends the chain: a cycle whose epoch has moved on returns without
/// queueing again, so the owner is never left with a timer it does not
/// want.
fn schedule<T: Blinking>(epoch: usize, cx: &mut Context<T>) {
    cx.spawn(async move |this, cx| {
        cx.background_executor().timer(INTERVAL).await;
        this.update(cx, |this, cx| {
            let blink = this.blink_mut();
            if blink.epoch != epoch {
                return;
            }
            blink.on = !blink.on;
            cx.notify();
            schedule(epoch, cx);
        })
        .ok();
    })
    .detach();
}
