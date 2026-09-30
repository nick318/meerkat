//! The ⌘F strip: one search line over whatever sits under it, the count of
//! what it found, and the two steps between the hits.
//!
//! Two things wear it — the SQL editor over its buffer and the query tab
//! over its result — and they are one control in the user's hands, so they
//! are one control here: the same keys, the same count, the same words. What
//! a hit *is* stays with the owner. The editor's hits are byte ranges and
//! the grid's are cells, so the bar is handed a [`FindCount`] and two callbacks
//! and paints; it decides nothing.
//!
//! **It is a strip, not a popover.** A popover over the editor covers the
//! very lines being searched, and one over the grid covers its header — the
//! row that says which column a hit is in. A strip pushes the pane down by
//! one row and hides nothing.
//!
//! **The bar never sits inside its owner's key context.** The grid binds a
//! bare `space` to ticking a row and the editor binds `enter` to a line
//! break; either one as an ancestor of the search line would take those
//! keys from it. Each owner paints the bar beside its own context, never
//! under it, and gives the bar a context of its own.
//!
//! The walk itself — [`step`] and [`first_from`] — is plain functions over
//! sorted hits, so it is argued with in a test rather than in a window.

use crate::{TextField, search_glyph};
use gpui::{
    App, ClickEvent, Div, Entity, FontWeight, SharedString, Stateful, Window, div, prelude::*, px,
};
use std::rc::Rc;
use theme::theme;

/// The strip's search box: wide enough for a statement fragment, never so
/// wide that the count drifts away from it.
const FIELD_WIDTH: f32 = 260.;
/// The search line's text, the size of the column find's.
pub const FONT_SIZE: f32 = 12.;
/// Past this many hits the count stops counting. A needle of one letter
/// over a large result would otherwise walk every value in it on each
/// keystroke, and "10,000+" is as much of an answer as the exact number.
pub const MAX_HITS: usize = 10_000;

/// What the search found, as far as the bar is concerned.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct FindCount {
    /// Whether anything is typed. An empty line is a question not yet asked
    /// and says nothing, rather than "no matches".
    pub typed: bool,
    /// How many hits there are.
    pub total: usize,
    /// Which of them the owner is on, when it is on one. The caret can sit
    /// between hits after the user clicks away, and "3 of 12" would then be
    /// a claim about a hit nobody is looking at.
    pub current: Option<usize>,
    /// The count stopped at [`MAX_HITS`], so there are more than it says.
    pub capped: bool,
}

impl FindCount {
    /// The count beside the line. Pure, so the wording is testable without
    /// a window.
    pub fn label(&self) -> String {
        if !self.typed {
            return String::new();
        }
        if self.total == 0 {
            return "no matches".to_string();
        }
        let plus = if self.capped { "+" } else { "" };
        let total = grouped(self.total);
        match self.current {
            Some(ix) => format!("{} of {total}{plus}", grouped(ix + 1)),
            None => format!("{total}{plus} found"),
        }
    }

    /// Nothing answers what was typed. The count then takes the error's ink,
    /// so a needle that has gone wrong reads as such from the corner of the
    /// eye.
    pub fn missed(&self) -> bool {
        self.typed && self.total == 0
    }
}

/// `12,345`: a count long enough to need reading is grouped in threes.
fn grouped(count: usize) -> String {
    let digits = count.to_string();
    let mut out = String::new();
    for (ix, digit) in digits.chars().enumerate() {
        if ix > 0 && (digits.len() - ix).is_multiple_of(3) {
            out.push(',');
        }
        out.push(digit);
    }
    out
}

/// Which hit a step from `at` lands on: the first one past it going
/// forward, the last one before it going back. Hits are sorted.
///
/// **The walk wraps**, as every editor's find does: one press past the last
/// hit comes round to the first, and the count says where the walk is. With
/// nothing to stand on, forward starts at the first hit and back at the
/// last.
pub fn step<T: Ord>(hits: &[T], at: Option<&T>, forward: bool) -> Option<usize> {
    if hits.is_empty() {
        return None;
    }
    let Some(at) = at else {
        return Some(if forward { 0 } else { hits.len() - 1 });
    };
    Some(if forward {
        let past = hits.partition_point(|hit| hit <= at);
        if past == hits.len() { 0 } else { past }
    } else {
        let before = hits.partition_point(|hit| hit < at);
        if before == 0 {
            hits.len() - 1
        } else {
            before - 1
        }
    })
}

/// The first hit at or after `at`, wrapping to the first of all. This is
/// where a keystroke in the line lands: the search moves on from where the
/// user was when they opened it, not from the top of the buffer, and a hit
/// already under them stays put.
pub fn first_from<T: Ord>(hits: &[T], at: &T) -> Option<usize> {
    if hits.is_empty() {
        return None;
    }
    let from = hits.partition_point(|hit| hit < at);
    Some(if from == hits.len() { 0 } else { from })
}

/// Called with `true` for the next hit and `false` for the one before.
pub type OnStep = Rc<dyn Fn(bool, &mut Window, &mut App)>;
/// Called when the strip's close button is pressed.
pub type OnClose = Rc<dyn Fn(&mut Window, &mut App)>;

/// The strip. `id` must be unique among the strips on screen.
pub fn find_bar(
    id: impl Into<SharedString>,
    query: Entity<TextField>,
    count: FindCount,
    on_step: OnStep,
    on_close: OnClose,
    cx: &App,
) -> Stateful<Div> {
    let colors = &theme(cx).colors;
    let id: SharedString = id.into();
    let count_ink = if count.missed() {
        colors.error
    } else {
        colors.text_muted
    };
    let back = on_step.clone();

    div()
        .id(id)
        .flex_none()
        .flex()
        .items_center()
        .gap(px(8.))
        .px(px(14.))
        .py(px(6.))
        .border_b_1()
        .border_color(colors.hairline)
        .bg(colors.panel)
        // A click on the strip is not a click on the text or the rows
        // under it.
        .occlude()
        .child(
            div()
                .flex_none()
                .w(px(FIELD_WIDTH))
                .flex()
                .items_center()
                .gap(px(8.))
                .px(px(9.))
                .py(px(4.))
                .border_1()
                .border_color(colors.border_strong)
                .rounded(px(6.))
                .bg(colors.elevated)
                .child(search_glyph(colors.text_faint))
                .child(div().flex_1().min_w(px(0.)).child(query)),
        )
        .child(
            div()
                .flex_none()
                .min_w(px(72.))
                .text_size(px(10.))
                .text_color(count_ink)
                .child(count.label()),
        )
        .child(
            step_button("find-previous", "↑", count.total > 0, cx)
                .on_click(move |_: &ClickEvent, window, cx| back(false, window, cx)),
        )
        .child(
            step_button("find-next", "↓", count.total > 0, cx)
                .on_click(move |_: &ClickEvent, window, cx| on_step(true, window, cx)),
        )
        .child(div().flex_1())
        // The keys, written down, because a gesture nothing on screen names
        // is a gesture nobody finds.
        .child(
            div()
                .flex_none()
                .flex()
                .gap(px(10.))
                .text_size(px(9.))
                .text_color(colors.text_muted)
                .child("⏎ next")
                .child("⇧⏎ previous")
                .child("esc close"),
        )
        .child(
            div()
                .id("find-close")
                .flex_none()
                .px(px(6.))
                .py(px(2.))
                .rounded(px(4.))
                .text_size(px(12.))
                .text_color(colors.text_muted)
                .cursor_pointer()
                .hover(|s| s.bg(colors.hairline).text_color(colors.text))
                .on_click(move |_, window, cx| on_close(window, cx))
                .child("×"),
        )
}

/// One of the two steps. It goes faint rather than away with nothing to
/// step to, so the strip does not reflow as the count reaches zero.
fn step_button(id: &'static str, glyph: &'static str, live: bool, cx: &App) -> Stateful<Div> {
    let colors = &theme(cx).colors;
    let button = div()
        .id(id)
        .flex_none()
        .w(px(22.))
        .h(px(20.))
        .flex()
        .items_center()
        .justify_center()
        .border_1()
        .border_color(colors.border_strong)
        .rounded(px(5.))
        .bg(colors.elevated)
        .text_size(px(11.))
        .font_weight(FontWeight::MEDIUM)
        .child(glyph);
    if live {
        button
            .text_color(colors.text_secondary)
            .cursor_pointer()
            .hover(|s| s.border_color(colors.text_faint))
    } else {
        button.text_color(colors.text_faint)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_step_forward_lands_past_where_the_walk_stands() {
        let hits = [2, 5, 9];
        assert_eq!(step(&hits, Some(&2), true), Some(1));
        assert_eq!(step(&hits, Some(&3), true), Some(1));
        assert_eq!(step(&hits, Some(&0), true), Some(0));
    }

    #[test]
    fn a_step_back_lands_before_where_the_walk_stands() {
        let hits = [2, 5, 9];
        assert_eq!(step(&hits, Some(&5), false), Some(0));
        assert_eq!(step(&hits, Some(&7), false), Some(1));
    }

    /// One press past the last hit comes round to the first, and the other
    /// way round.
    #[test]
    fn the_walk_wraps_at_both_ends() {
        let hits = [2, 5, 9];
        assert_eq!(step(&hits, Some(&9), true), Some(0));
        assert_eq!(step(&hits, Some(&12), true), Some(0));
        assert_eq!(step(&hits, Some(&2), false), Some(2));
        assert_eq!(step(&hits, Some(&0), false), Some(2));
    }

    #[test]
    fn with_nothing_to_stand_on_the_walk_starts_at_an_end() {
        let hits = [2, 5, 9];
        assert_eq!(step(&hits, None, true), Some(0));
        assert_eq!(step(&hits, None, false), Some(2));
        assert_eq!(step::<i32>(&[], None, true), None);
    }

    /// Typing keeps a hit the user is already on, and otherwise moves on
    /// from where they were rather than from the top.
    #[test]
    fn a_keystroke_lands_on_the_first_hit_from_where_the_user_was() {
        let hits = [2, 5, 9];
        assert_eq!(first_from(&hits, &5), Some(1));
        assert_eq!(first_from(&hits, &6), Some(2));
        assert_eq!(first_from(&hits, &10), Some(0));
        assert_eq!(first_from::<i32>(&[], &1), None);
    }

    #[test]
    fn the_count_says_where_the_walk_is() {
        let count = |typed, total, current, capped| FindCount {
            typed,
            total,
            current,
            capped,
        };
        assert_eq!(count(false, 0, None, false).label(), "");
        assert_eq!(count(true, 0, None, false).label(), "no matches");
        assert_eq!(count(true, 12, Some(2), false).label(), "3 of 12");
        assert_eq!(count(true, 12, None, false).label(), "12 found");
        assert_eq!(count(true, 10_000, Some(0), true).label(), "1 of 10,000+");
        assert!(count(true, 0, None, false).missed());
        assert!(!count(false, 0, None, false).missed());
    }
}
