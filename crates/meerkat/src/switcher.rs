//! ⌃⇥: walk the open tabs in a popup, most recently used first.
//!
//! The shell keeps the tabs in the order the strip paints them, which is
//! the order they were opened in. That is not the order anyone switches in:
//! ⌃⇥ is "the one before this one", and holding it walks back through the
//! session. So the shell also keeps `order`, the same tabs by id with the
//! active one at the front, and this module is what walks it.
//!
//! The popup is a child of the shell, as the ⌘K palette is, so closing it
//! hands the focus straight back to the tab it settled on. It takes the
//! focus while it is open, which is what lets its own keys — ⏎, esc, and
//! ⌃⇥ itself — outrank the editor's.
//!
//! Selection **wraps**, unlike the palette's list. A switcher is a ring:
//! pressing ⌃⇥ once more past the end is how the user gets back to the tab
//! they started on, so stopping there would strand them.
//!
//! Nothing is switched until the user commits. Releasing ⌃ commits, the
//! way it does in every other tab switcher; esc leaves the active tab
//! where it was.

use gpui::{
    AnyElement, App, ElementId, FontWeight, KeyBinding, SharedString, Window, actions, div,
    prelude::*, px,
};
use std::rc::Rc;
use theme::ThemeColors;

actions!(switcher, [Next, Prev, Confirm, Cancel]);

/// The context the switcher's own keys are scoped to. It sits below the
/// shell's context, so ⏎ and esc go to the switcher while it is open and
/// to whatever was focused before once it is gone.
pub const KEY_CONTEXT: &str = "Switcher";

/// Key bindings for the switcher. ⌃⇥ is bound twice: to the shell, because
/// it has to open the popup when there is none, and to the popup, because
/// GPUI gives a keystroke to the binding that matched deepest.
pub fn key_bindings() -> Vec<KeyBinding> {
    vec![
        KeyBinding::new("ctrl-tab", Next, Some("Shell")),
        KeyBinding::new("ctrl-shift-tab", Prev, Some("Shell")),
        KeyBinding::new("ctrl-tab", Next, Some(KEY_CONTEXT)),
        KeyBinding::new("ctrl-shift-tab", Prev, Some(KEY_CONTEXT)),
        KeyBinding::new("down", Next, Some(KEY_CONTEXT)),
        KeyBinding::new("up", Prev, Some(KEY_CONTEXT)),
        KeyBinding::new("enter", Confirm, Some(KEY_CONTEXT)),
        KeyBinding::new("escape", Cancel, Some(KEY_CONTEXT)),
    ]
}

/// The dialog's geometry. It is narrower than the palette: a tab title is
/// a name, not a statement.
pub const WIDTH: f32 = 420.;
pub const TOP_MARGIN: f32 = 140.;
/// One height for every row, so the list can virtualize.
pub const ROW_HEIGHT: f32 = 30.;
/// How tall the list may grow before it scrolls instead.
pub const MAX_LIST_HEIGHT: f32 = 330.;

const GLYPH_WIDTH: f32 = 16.;
const TRAILING_WIDTH: f32 = 72.;

/// One open tab, as the popup reads it.
pub struct Entry {
    pub id: u64,
    pub title: SharedString,
    /// What kind of tab it is: `table`, `view`, `query` or `history`.
    pub detail: SharedString,
    /// Whether it is a table rather than a view, a query or the history.
    /// Tables wear the sidebar's glyph; everything else wears its ring.
    pub table: bool,
}

/// Called with the id of the tab the user picked.
pub type OnPick = Rc<dyn Fn(u64, &mut Window, &mut App)>;

/// Remember that a tab was just used: move it to the front of the order.
pub fn touch(order: &mut Vec<u64>, id: u64) {
    order.retain(|open| *open != id);
    order.insert(0, id);
}

/// Drop a closed tab from the order.
pub fn forget(order: &mut Vec<u64>, id: u64) {
    order.retain(|open| *open != id);
}

/// Where the selection lands after one step. It wraps at both ends,
/// because a switcher is a ring: one press past the last tab is how the
/// user comes back to the one they started on.
pub fn step(len: usize, selected: usize, forward: bool) -> usize {
    if len == 0 {
        return 0;
    }
    if forward {
        (selected + 1) % len
    } else {
        (selected + len - 1) % len
    }
}

/// One row of the list. Free-standing for the same reason the palette's
/// rows are: the list's render closure outlives the borrow of the view it
/// came from.
pub fn switcher_row(
    ix: usize,
    entry: &Entry,
    selected: bool,
    on_pick: &OnPick,
    colors: &ThemeColors,
    cx: &App,
) -> AnyElement {
    let on_pick = on_pick.clone();
    let id = entry.id;

    let mut row = div()
        .id(ElementId::NamedInteger("switcher-row".into(), ix as u64))
        .h(px(ROW_HEIGHT))
        .w_full()
        .flex()
        .items_center()
        .gap(px(10.))
        .px(px(9.))
        .rounded(px(6.))
        .cursor_pointer()
        .on_click(move |_event, window, cx| on_pick(id, window, cx))
        .child(
            div()
                .w(px(GLYPH_WIDTH))
                .flex_none()
                .child(if entry.table {
                    ui::table_glyph(selected, cx)
                } else {
                    div()
                        .size(px(5.))
                        .rounded_full()
                        .border_1()
                        .border_color(if selected { colors.accent } else { colors.text_faint })
                }),
        )
        .child(
            div()
                .flex_1()
                .min_w(px(0.))
                .text_size(px(12.))
                .font_weight(if selected { FontWeight::MEDIUM } else { FontWeight::NORMAL })
                .text_color(if selected { colors.text } else { colors.text_body })
                .truncate()
                .child(entry.title.clone()),
        )
        .child(
            div()
                .w(px(TRAILING_WIDTH))
                .flex_none()
                .flex()
                .justify_end()
                .text_size(px(10.))
                .text_color(if selected { colors.accent } else { colors.text_faint })
                .truncate()
                // The selected row says what releasing ⌃ would do; the
                // first row says where the user is standing now.
                .child(match (selected, ix) {
                    (true, _) => SharedString::from("switch ⏎"),
                    (false, 0) => SharedString::from("current"),
                    (false, _) => entry.detail.clone(),
                }),
        );

    row = if selected {
        row.bg(colors.selection)
    } else {
        let hover = colors.hairline;
        row.hover(move |s| s.bg(hover))
    };
    row.into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_order_puts_the_tab_just_used_at_the_front() {
        let mut order = Vec::new();
        touch(&mut order, 1);
        touch(&mut order, 2);
        touch(&mut order, 3);
        assert_eq!(order, [3, 2, 1]);

        // Going back to a tab that is already open moves it, it does not
        // add it twice.
        touch(&mut order, 1);
        assert_eq!(order, [1, 3, 2]);

        forget(&mut order, 3);
        assert_eq!(order, [1, 2]);
        // Forgetting a tab that is not open is not an error.
        forget(&mut order, 9);
        assert_eq!(order, [1, 2]);
    }

    /// The switcher opens on the active tab and one step lands on the one
    /// used before it, which is what ⌃⇥ means.
    #[test]
    fn one_step_lands_on_the_previous_tab() {
        assert_eq!(step(3, 0, true), 1);
        assert_eq!(step(3, 1, true), 2);
        // A ring: one more press comes back to where the user started.
        assert_eq!(step(3, 2, true), 0);
        // ⌃⇧⇥ walks the other way, and wraps as well.
        assert_eq!(step(3, 0, false), 2);
        assert_eq!(step(3, 2, false), 1);
    }

    #[test]
    fn an_empty_list_stays_put() {
        assert_eq!(step(0, 0, true), 0);
        assert_eq!(step(1, 0, true), 0);
        assert_eq!(step(1, 0, false), 0);
    }
}
