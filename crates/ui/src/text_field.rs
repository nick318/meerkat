//! A single-line text field for forms: the connection name, the URL, a
//! password.
//!
//! Same shape as `sql_editor`, cut down to one line: the entity owns the
//! string and implements `EntityInputHandler`, so the platform delivers
//! typed text and IME edits, and a custom element shapes and paints the
//! line. One line means no rows to map, but it does need a horizontal
//! scroll offset, so a long URL keeps the caret in view.
//!
//! Offsets are byte offsets into the value and always sit on a character
//! boundary.

use gpui::{
    App, Bounds, ClipboardItem, Context, CursorStyle, Element, ElementId, ElementInputHandler,
    Entity, EntityInputHandler, EventEmitter, FocusHandle, Focusable, GlobalElementId, Hsla,
    IntoElement, KeyBinding, LayoutId, MouseButton, MouseDownEvent, MouseMoveEvent, MouseUpEvent,
    PaintQuad, Pixels, Point, ShapedLine, SharedString, Style, TextRun, UTF16Selection,
    UnderlineStyle, Window, actions, div, fill, point, prelude::*, px, relative, size,
};
use std::ops::Range;
use std::time::Duration;
use theme::theme;

actions!(
    text_field,
    [
        Backspace,
        Cancel,
        CopySelection,
        CutSelection,
        Delete,
        DeleteToBeginningOfLine,
        DeleteToEndOfLine,
        DeleteToNextWordEnd,
        DeleteToPreviousWordStart,
        MoveLeft,
        MoveRight,
        MoveToBeginningOfLine,
        MoveToEndOfLine,
        MoveToNextWordEnd,
        MoveToPreviousWordStart,
        NextField,
        Paste,
        SelectAll,
        SelectLeft,
        SelectRight,
        SelectToBeginningOfLine,
        SelectToEndOfLine,
        SelectToNextWordEnd,
        SelectToPreviousWordStart,
        Submit,
    ]
);

const KEY_CONTEXT: &str = "TextField";
/// 12px text on a 18px line: the design's form rhythm.
const FONT_SIZE: f32 = 12.;
const LINE_HEIGHT: f32 = 18.;
/// The same 12-on-18 rhythm, as a ratio, for a `bare` field that sets its
/// own text size.
const LINE_SPACING: f32 = LINE_HEIGHT / FONT_SIZE;
/// Keep this much room to the right of the caret, so the character being
/// typed is never flush against the field's edge.
const CARET_MARGIN: f32 = 2.;
/// What a masked field paints in place of every character.
const MASK: char = '•';
/// How long the caret stays on, and then off. macOS blinks a caret at
/// roughly this rate, and a field that matches it reads as a real one.
const BLINK_INTERVAL: Duration = Duration::from_millis(530);

/// Key bindings for every text field. Bind these once at startup; they are
/// scoped to the field's key context, so they never shadow the app's own
/// bindings while a field has focus.
pub fn text_field_key_bindings() -> Vec<KeyBinding> {
    // A closure cannot take the action: `KeyBinding::new` is generic over
    // the concrete action type, and `Box<dyn Action>` is not one.
    macro_rules! bind {
        ($keystroke:expr, $action:expr) => {
            KeyBinding::new($keystroke, $action, Some(KEY_CONTEXT))
        };
    }
    vec![
        bind!("backspace", Backspace),
        bind!("delete", Delete),
        bind!("alt-backspace", DeleteToPreviousWordStart),
        bind!("alt-delete", DeleteToNextWordEnd),
        bind!("cmd-backspace", DeleteToBeginningOfLine),
        bind!("cmd-delete", DeleteToEndOfLine),
        bind!("left", MoveLeft),
        bind!("right", MoveRight),
        bind!("alt-left", MoveToPreviousWordStart),
        bind!("alt-right", MoveToNextWordEnd),
        bind!("cmd-left", MoveToBeginningOfLine),
        bind!("cmd-right", MoveToEndOfLine),
        bind!("home", MoveToBeginningOfLine),
        bind!("end", MoveToEndOfLine),
        // The emacs pair macOS honours in every text field.
        bind!("ctrl-a", MoveToBeginningOfLine),
        bind!("ctrl-e", MoveToEndOfLine),
        bind!("shift-left", SelectLeft),
        bind!("shift-right", SelectRight),
        bind!("alt-shift-left", SelectToPreviousWordStart),
        bind!("alt-shift-right", SelectToNextWordEnd),
        bind!("cmd-shift-left", SelectToBeginningOfLine),
        bind!("cmd-shift-right", SelectToEndOfLine),
        bind!("shift-home", SelectToBeginningOfLine),
        bind!("shift-end", SelectToEndOfLine),
        bind!("cmd-a", SelectAll),
        bind!("cmd-c", CopySelection),
        bind!("cmd-x", CutSelection),
        bind!("cmd-v", Paste),
        bind!("enter", Submit),
        bind!("tab", NextField),
        bind!("escape", Cancel),
    ]
}

/// What a field tells the form around it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TextFieldEvent {
    /// The value changed.
    Changed,
    /// Enter: the form should do whatever its primary button does.
    Submit,
    /// Tab: the form should focus the next field.
    NextField,
    /// Escape: the form should close.
    Cancel,
}

pub struct TextField {
    focus_handle: FocusHandle,
    content: String,
    placeholder: SharedString,
    /// A password field paints dots and never copies its value out.
    masked: bool,
    /// A field with no chrome of its own, at this text size. The palette's
    /// search line sits in a header that already draws the border and the
    /// surface, so the field must add neither.
    bare: Option<f32>,
    /// What the field would finish the value with, painted faint after it
    /// and never part of the value. Whoever owns the field decides what
    /// that is and what accepts it; the field only shows it.
    ghost: SharedString,
    selected_range: Range<usize>,
    selection_reversed: bool,
    marked_range: Option<Range<usize>>,
    /// How far the line is scrolled left, so a long value keeps the caret
    /// in view. Written by the element after every paint.
    scroll: Pixels,
    last_layout: Option<ShapedLine>,
    last_bounds: Option<Bounds<Pixels>>,
    is_selecting: bool,
    /// Whether the caret is in the on half of its cycle. A caret that
    /// never goes out reads as a window that has stopped answering.
    blink_on: bool,
    /// Whether the field held the focus at the last paint. The blink is
    /// started and stopped from that edge, because focus is a property of
    /// the window and only the render pass has one.
    was_focused: bool,
    /// Counts the blink cycles, so a cycle that has been replaced stops
    /// instead of fighting the one that replaced it. Typing restarts the
    /// cycle; two of them would beat against each other.
    blink_epoch: usize,
}

impl EventEmitter<TextFieldEvent> for TextField {}

impl TextField {
    pub fn new(placeholder: impl Into<SharedString>, cx: &mut Context<Self>) -> Self {
        Self {
            focus_handle: cx.focus_handle(),
            content: String::new(),
            placeholder: placeholder.into(),
            masked: false,
            bare: None,
            ghost: SharedString::default(),
            selected_range: 0..0,
            selection_reversed: false,
            marked_range: None,
            scroll: px(0.),
            last_layout: None,
            last_bounds: None,
            is_selecting: false,
            blink_on: true,
            was_focused: false,
            blink_epoch: 0,
        }
    }

    /// Paint dots in place of the value, for a password.
    pub fn masked(mut self) -> Self {
        self.masked = true;
        self
    }

    /// Drop the border, the surface and the padding, and shape the line at
    /// `font_size`. For a field that sits inside a surface of its own —
    /// the palette's search line.
    pub fn bare(mut self, font_size: f32) -> Self {
        self.bare = Some(font_size);
        self
    }

    pub fn text(&self) -> &str {
        &self.content
    }

    pub fn trimmed(&self) -> &str {
        self.content.trim()
    }

    pub fn is_empty(&self) -> bool {
        self.trimmed().is_empty()
    }

    pub fn set_text(&mut self, text: impl Into<String>, cx: &mut Context<Self>) {
        self.content = text.into();
        let end = self.content.len();
        self.selected_range = end..end;
        self.selection_reversed = false;
        self.marked_range = None;
        cx.emit(TextFieldEvent::Changed);
        self.touched(cx);
    }

    pub fn clear(&mut self, cx: &mut Context<Self>) {
        self.set_text(String::new(), cx);
    }

    /// Show what the value would be finished with, faint and after the
    /// caret. Pass an empty string to take it away.
    ///
    /// It is never part of the value: nothing here accepts it, reads it
    /// back or lets the caret into it. A masked field never shows one —
    /// a password must not be guessed at on screen.
    pub fn set_ghost(&mut self, ghost: impl Into<SharedString>, cx: &mut Context<Self>) {
        let ghost = ghost.into();
        if self.ghost == ghost {
            return;
        }
        self.ghost = ghost;
        cx.notify();
    }

    // --- the caret's blink -----------------------------------------------

    /// Show the caret and start its cycle over. Called when the field
    /// takes the focus and after every edit or motion, so the caret is
    /// solid while the user is working and only blinks once they stop —
    /// the way a caret behaves in every platform field.
    fn restart_blink(&mut self, cx: &mut Context<Self>) {
        self.blink_on = true;
        self.blink_epoch += 1;
        self.schedule_blink(self.blink_epoch, cx);
    }

    /// Stop the cycle and leave the caret on, for a field that has lost
    /// the focus. An unfocused field paints no caret at all, so what
    /// matters here is that the timer stops waking the window.
    fn stop_blink(&mut self) {
        self.blink_on = true;
        self.blink_epoch += 1;
    }

    /// Turn the caret over once, then queue the next turn. The epoch is
    /// what ends the chain: a cycle whose epoch has moved on returns
    /// without queueing again, so the field is never left with a timer
    /// it does not want.
    fn schedule_blink(&mut self, epoch: usize, cx: &mut Context<Self>) {
        cx.spawn(async move |this, cx| {
            cx.background_executor().timer(BLINK_INTERVAL).await;
            this.update(cx, |this, cx| {
                if this.blink_epoch != epoch {
                    return;
                }
                this.blink_on = !this.blink_on;
                cx.notify();
                this.schedule_blink(epoch, cx);
            })
            .ok();
        })
        .detach();
    }

    /// Redraw, and put the caret back on show. Every edit and every
    /// motion goes through here rather than calling `cx.notify()` itself.
    fn touched(&mut self, cx: &mut Context<Self>) {
        self.restart_blink(cx);
        cx.notify();
    }

    // --- selection -------------------------------------------------------

    fn cursor_offset(&self) -> usize {
        if self.selection_reversed {
            self.selected_range.start
        } else {
            self.selected_range.end
        }
    }

    fn move_to(&mut self, offset: usize, cx: &mut Context<Self>) {
        let offset = clamp_offset(&self.content, offset);
        self.selected_range = offset..offset;
        self.selection_reversed = false;
        self.touched(cx);
    }

    fn select_to(&mut self, offset: usize, cx: &mut Context<Self>) {
        let offset = clamp_offset(&self.content, offset);
        if self.selection_reversed {
            self.selected_range.start = offset;
        } else {
            self.selected_range.end = offset;
        }
        if self.selected_range.end < self.selected_range.start {
            self.selection_reversed = !self.selection_reversed;
            self.selected_range = self.selected_range.end..self.selected_range.start;
        }
        self.touched(cx);
    }

    fn delete_to(&mut self, offset: usize, window: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            if offset == self.cursor_offset() {
                return;
            }
            self.select_to(offset, cx);
        }
        self.replace_text_in_range(None, "", window, cx);
    }

    fn selected(&self) -> String {
        let range = clamp_range(&self.content, self.selected_range.clone());
        self.content[range].to_string()
    }

    // --- actions ---------------------------------------------------------

    fn backspace(&mut self, _: &Backspace, window: &mut Window, cx: &mut Context<Self>) {
        let target = motion::previous_boundary(&self.content, self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn delete(&mut self, _: &Delete, window: &mut Window, cx: &mut Context<Self>) {
        let target = motion::next_boundary(&self.content, self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn delete_to_previous_word_start(
        &mut self,
        _: &DeleteToPreviousWordStart,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::previous_word_start(&self.content, self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn delete_to_next_word_end(
        &mut self,
        _: &DeleteToNextWordEnd,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::next_word_end(&self.content, self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn delete_to_beginning_of_line(
        &mut self,
        _: &DeleteToBeginningOfLine,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.delete_to(0, window, cx);
    }

    fn delete_to_end_of_line(
        &mut self,
        _: &DeleteToEndOfLine,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.delete_to(self.content.len(), window, cx);
    }

    fn move_left(&mut self, _: &MoveLeft, _: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            let target = motion::previous_boundary(&self.content, self.cursor_offset());
            self.move_to(target, cx);
        } else {
            self.move_to(self.selected_range.start, cx);
        }
    }

    fn move_right(&mut self, _: &MoveRight, _: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            let target = motion::next_boundary(&self.content, self.cursor_offset());
            self.move_to(target, cx);
        } else {
            self.move_to(self.selected_range.end, cx);
        }
    }

    fn move_to_previous_word_start(
        &mut self,
        _: &MoveToPreviousWordStart,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::previous_word_start(&self.content, self.cursor_offset());
        self.move_to(target, cx);
    }

    fn move_to_next_word_end(
        &mut self,
        _: &MoveToNextWordEnd,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::next_word_end(&self.content, self.cursor_offset());
        self.move_to(target, cx);
    }

    fn move_to_beginning_of_line(
        &mut self,
        _: &MoveToBeginningOfLine,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.move_to(0, cx);
    }

    fn move_to_end_of_line(&mut self, _: &MoveToEndOfLine, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(self.content.len(), cx);
    }

    fn select_left(&mut self, _: &SelectLeft, _: &mut Window, cx: &mut Context<Self>) {
        let target = motion::previous_boundary(&self.content, self.cursor_offset());
        self.select_to(target, cx);
    }

    fn select_right(&mut self, _: &SelectRight, _: &mut Window, cx: &mut Context<Self>) {
        let target = motion::next_boundary(&self.content, self.cursor_offset());
        self.select_to(target, cx);
    }

    fn select_to_previous_word_start(
        &mut self,
        _: &SelectToPreviousWordStart,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::previous_word_start(&self.content, self.cursor_offset());
        self.select_to(target, cx);
    }

    fn select_to_next_word_end(
        &mut self,
        _: &SelectToNextWordEnd,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::next_word_end(&self.content, self.cursor_offset());
        self.select_to(target, cx);
    }

    fn select_to_beginning_of_line(
        &mut self,
        _: &SelectToBeginningOfLine,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.select_to(0, cx);
    }

    fn select_to_end_of_line(
        &mut self,
        _: &SelectToEndOfLine,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.select_to(self.content.len(), cx);
    }

    fn select_all(&mut self, _: &SelectAll, _: &mut Window, cx: &mut Context<Self>) {
        self.select_everything(cx);
    }

    /// Mark the whole value, as ⌘A does. A key that puts the focus on a
    /// field the user may have typed into before wants this: the next
    /// character replaces what is there rather than being appended to it.
    pub fn select_everything(&mut self, cx: &mut Context<Self>) {
        self.move_to(0, cx);
        self.select_to(self.content.len(), cx);
    }

    fn copy(&mut self, _: &CopySelection, _: &mut Window, cx: &mut Context<Self>) {
        // A password never leaves the field.
        if self.masked {
            return;
        }
        let selected = self.selected();
        if !selected.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(selected));
        }
    }

    fn cut(&mut self, _: &CutSelection, window: &mut Window, cx: &mut Context<Self>) {
        if self.masked {
            return;
        }
        let selected = self.selected();
        if !selected.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(selected));
            self.replace_text_in_range(None, "", window, cx);
        }
    }

    fn paste(&mut self, _: &Paste, window: &mut Window, cx: &mut Context<Self>) {
        let Some(text) = cx.read_from_clipboard().and_then(|item| item.text()) else {
            return;
        };
        // One line: a pasted URL with a stray newline lands as one value.
        let text = text.replace(['\n', '\r'], "");
        self.replace_text_in_range(None, &text, window, cx);
    }

    fn submit(&mut self, _: &Submit, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(TextFieldEvent::Submit);
    }

    fn next_field(&mut self, _: &NextField, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(TextFieldEvent::NextField);
    }

    fn cancel(&mut self, _: &Cancel, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(TextFieldEvent::Cancel);
    }

    // --- mouse -----------------------------------------------------------

    fn on_mouse_down(&mut self, event: &MouseDownEvent, _: &mut Window, cx: &mut Context<Self>) {
        let offset = self.offset_for_position(event.position);
        match event.click_count {
            1 if event.modifiers.shift => self.select_to(offset, cx),
            1 => {
                self.is_selecting = true;
                self.move_to(offset, cx);
            }
            2 => {
                let word = motion::word_at(&self.content, offset);
                self.move_to(word.start, cx);
                self.select_to(word.end, cx);
            }
            _ => {
                self.move_to(0, cx);
                self.select_to(self.content.len(), cx);
            }
        }
    }

    fn on_mouse_up(&mut self, _: &MouseUpEvent, _: &mut Window, _: &mut Context<Self>) {
        self.is_selecting = false;
    }

    fn on_mouse_move(&mut self, event: &MouseMoveEvent, _: &mut Window, cx: &mut Context<Self>) {
        if self.is_selecting {
            self.select_to(self.offset_for_position(event.position), cx);
        }
    }

    // --- offsets ---------------------------------------------------------

    /// What the element paints: the value, or one dot per character.
    fn display_text(&self) -> String {
        if self.masked {
            MASK.to_string().repeat(self.content.chars().count())
        } else {
            self.content.clone()
        }
    }

    /// A value offset, in the painted string. The two agree character by
    /// character, so count characters across.
    fn to_display(&self, offset: usize) -> usize {
        if !self.masked {
            return offset;
        }
        let offset = clamp_offset(&self.content, offset);
        self.content[..offset].chars().count() * MASK.len_utf8()
    }

    /// The inverse: a painted offset, back in the value.
    fn from_display(&self, offset: usize) -> usize {
        if !self.masked {
            return clamp_offset(&self.content, offset);
        }
        let characters = offset / MASK.len_utf8();
        self.content
            .char_indices()
            .nth(characters)
            .map(|(ix, _)| ix)
            .unwrap_or(self.content.len())
    }

    fn offset_for_position(&self, position: Point<Pixels>) -> usize {
        let (Some(bounds), Some(line)) = (self.last_bounds.as_ref(), self.last_layout.as_ref())
        else {
            return self.cursor_offset();
        };
        let x = position.x - bounds.left() + self.scroll;
        self.from_display(line.closest_index_for_x(x))
    }

    fn offset_from_utf16(&self, target: usize) -> usize {
        let mut utf8 = 0;
        let mut utf16 = 0;
        for ch in self.content.chars() {
            if utf16 >= target {
                break;
            }
            utf16 += ch.len_utf16();
            utf8 += ch.len_utf8();
        }
        utf8
    }

    fn offset_to_utf16(&self, target: usize) -> usize {
        let mut utf8 = 0;
        let mut utf16 = 0;
        for ch in self.content.chars() {
            if utf8 >= target {
                break;
            }
            utf8 += ch.len_utf8();
            utf16 += ch.len_utf16();
        }
        utf16
    }

    fn range_to_utf16(&self, range: &Range<usize>) -> Range<usize> {
        self.offset_to_utf16(range.start)..self.offset_to_utf16(range.end)
    }

    fn range_from_utf16(&self, range: &Range<usize>) -> Range<usize> {
        self.offset_from_utf16(range.start)..self.offset_from_utf16(range.end)
    }
}

impl EntityInputHandler for TextField {
    fn text_for_range(
        &mut self,
        range_utf16: Range<usize>,
        actual_range: &mut Option<Range<usize>>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<String> {
        let range = clamp_range(&self.content, self.range_from_utf16(&range_utf16));
        actual_range.replace(self.range_to_utf16(&range));
        Some(self.content[range].to_string())
    }

    fn selected_text_range(
        &mut self,
        _ignore_disabled_input: bool,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<UTF16Selection> {
        Some(UTF16Selection {
            range: self.range_to_utf16(&self.selected_range),
            reversed: self.selection_reversed,
        })
    }

    fn marked_text_range(&self, _: &mut Window, _: &mut Context<Self>) -> Option<Range<usize>> {
        self.marked_range.as_ref().map(|range| self.range_to_utf16(range))
    }

    fn unmark_text(&mut self, _: &mut Window, _: &mut Context<Self>) {
        self.marked_range = None;
    }

    fn replace_text_in_range(
        &mut self,
        range_utf16: Option<Range<usize>>,
        new_text: &str,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let range = clamp_range(
            &self.content,
            range_utf16
                .as_ref()
                .map(|range| self.range_from_utf16(range))
                .or(self.marked_range.clone())
                .unwrap_or(self.selected_range.clone()),
        );

        self.content =
            self.content[..range.start].to_owned() + new_text + &self.content[range.end..];
        let cursor = range.start + new_text.len();
        self.selected_range = cursor..cursor;
        self.selection_reversed = false;
        self.marked_range.take();
        cx.emit(TextFieldEvent::Changed);
        self.touched(cx);
    }

    fn replace_and_mark_text_in_range(
        &mut self,
        range_utf16: Option<Range<usize>>,
        new_text: &str,
        new_selected_range_utf16: Option<Range<usize>>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let range = clamp_range(
            &self.content,
            range_utf16
                .as_ref()
                .map(|range| self.range_from_utf16(range))
                .or(self.marked_range.clone())
                .unwrap_or(self.selected_range.clone()),
        );

        self.content =
            self.content[..range.start].to_owned() + new_text + &self.content[range.end..];
        self.marked_range =
            (!new_text.is_empty()).then(|| range.start..range.start + new_text.len());
        // The platform reports this selection relative to `new_text`, so
        // convert it inside that string and then shift it into the value.
        self.selected_range = match new_selected_range_utf16.as_ref() {
            Some(selected) => {
                let start = range.start + utf16_to_byte(new_text, selected.start);
                let end = range.start + utf16_to_byte(new_text, selected.end);
                clamp_range(&self.content, start..end)
            }
            None => {
                let cursor = range.start + new_text.len();
                cursor..cursor
            }
        };
        cx.emit(TextFieldEvent::Changed);
        self.touched(cx);
    }

    fn bounds_for_range(
        &mut self,
        range_utf16: Range<usize>,
        bounds: Bounds<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<Bounds<Pixels>> {
        let line = self.last_layout.as_ref()?;
        let range = clamp_range(&self.content, self.range_from_utf16(&range_utf16));
        let from = self.to_display(range.start).min(line.len());
        let to = self.to_display(range.end).min(line.len());
        Some(Bounds::from_corners(
            point(bounds.left() + line.x_for_index(from) - self.scroll, bounds.top()),
            point(
                bounds.left() + line.x_for_index(to) - self.scroll,
                bounds.bottom(),
            ),
        ))
    }

    fn character_index_for_point(
        &mut self,
        point: Point<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<usize> {
        Some(self.offset_to_utf16(self.offset_for_position(point)))
    }
}

impl Focusable for TextField {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}

impl Render for TextField {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();
        let focused = self.focus_handle.is_focused(window);
        // Focus belongs to the window, and this is the one place that has
        // one, so the blink is started and stopped from the edge here
        // rather than from a focus listener the constructor cannot install.
        if focused != self.was_focused {
            self.was_focused = focused;
            if focused {
                self.restart_blink(cx);
            } else {
                self.stop_blink();
            }
        }

        div()
            .key_context(KEY_CONTEXT)
            .track_focus(&self.focus_handle(cx))
            .cursor(CursorStyle::IBeam)
            .on_action(cx.listener(Self::backspace))
            .on_action(cx.listener(Self::delete))
            .on_action(cx.listener(Self::delete_to_previous_word_start))
            .on_action(cx.listener(Self::delete_to_next_word_end))
            .on_action(cx.listener(Self::delete_to_beginning_of_line))
            .on_action(cx.listener(Self::delete_to_end_of_line))
            .on_action(cx.listener(Self::move_left))
            .on_action(cx.listener(Self::move_right))
            .on_action(cx.listener(Self::move_to_previous_word_start))
            .on_action(cx.listener(Self::move_to_next_word_end))
            .on_action(cx.listener(Self::move_to_beginning_of_line))
            .on_action(cx.listener(Self::move_to_end_of_line))
            .on_action(cx.listener(Self::select_left))
            .on_action(cx.listener(Self::select_right))
            .on_action(cx.listener(Self::select_to_previous_word_start))
            .on_action(cx.listener(Self::select_to_next_word_end))
            .on_action(cx.listener(Self::select_to_beginning_of_line))
            .on_action(cx.listener(Self::select_to_end_of_line))
            .on_action(cx.listener(Self::select_all))
            .on_action(cx.listener(Self::copy))
            .on_action(cx.listener(Self::cut))
            .on_action(cx.listener(Self::paste))
            .on_action(cx.listener(Self::submit))
            .on_action(cx.listener(Self::next_field))
            .on_action(cx.listener(Self::cancel))
            .on_mouse_down(MouseButton::Left, cx.listener(Self::on_mouse_down))
            .on_mouse_up(MouseButton::Left, cx.listener(Self::on_mouse_up))
            .on_mouse_up_out(MouseButton::Left, cx.listener(Self::on_mouse_up))
            .on_mouse_move(cx.listener(Self::on_mouse_move))
            .w_full()
            .map(|field| match self.bare {
                // A bare field draws nothing of its own: the surface
                // around it already carries the border and the padding.
                Some(font_size) => field
                    .text_size(px(font_size))
                    .line_height(px(font_size * LINE_SPACING)),
                None => field
                    .px(px(10.))
                    .py(px(7.))
                    .border_1()
                    // The focused field is the one wearing the accent;
                    // every other border on the screen stays a hairline.
                    .border_color(if focused { colors.accent } else { colors.border_strong })
                    .rounded(px(6.))
                    .bg(colors.elevated)
                    .text_size(px(FONT_SIZE))
                    .line_height(px(LINE_HEIGHT)),
            })
            .text_color(colors.text_body)
            .child(FieldElement { field: cx.entity() })
    }
}

/// Paints the one line: selection, text, caret.
struct FieldElement {
    field: Entity<TextField>,
}

struct PrepaintState {
    line: ShapedLine,
    scroll: Pixels,
    selection: Option<PaintQuad>,
    cursor: Option<PaintQuad>,
    /// The faint suggestion, and how far along the line it starts.
    ghost: Option<(ShapedLine, Pixels)>,
}

impl IntoElement for FieldElement {
    type Element = Self;

    fn into_element(self) -> Self::Element {
        self
    }
}

impl Element for FieldElement {
    type RequestLayoutState = ();
    type PrepaintState = PrepaintState;

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, Self::RequestLayoutState) {
        let mut style = Style::default();
        style.size.width = relative(1.).into();
        style.size.height = window.line_height().into();
        (window.request_layout(style, [], cx), ())
    }

    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut Self::RequestLayoutState,
        window: &mut Window,
        cx: &mut App,
    ) -> Self::PrepaintState {
        let colors = theme(cx).colors.clone();
        let field = self.field.read(cx);
        let style = window.text_style();
        let font_size = style.font_size.to_pixels(window.rem_size());

        let placeholder = field.content.is_empty();
        let text: SharedString = if placeholder {
            field.placeholder.clone()
        } else {
            field.display_text().into()
        };
        let color = if placeholder { colors.text_faint } else { style.color };
        let runs = single_run(&text, style.font(), color, underline_for(field));
        let line = window.text_system().shape_line(text, font_size, &runs, None);

        // Keep the caret inside the field: scroll only as far as it must.
        let cursor_x = if placeholder {
            px(0.)
        } else {
            line.x_for_index(field.to_display(field.cursor_offset()).min(line.len()))
        };
        let width = bounds.size.width;
        let mut scroll = field.scroll;
        if cursor_x - scroll > width - px(CARET_MARGIN) {
            scroll = cursor_x - width + px(CARET_MARGIN);
        }
        if cursor_x < scroll {
            scroll = cursor_x;
        }
        let overflow = (line.width() + px(CARET_MARGIN) - width).max(px(0.));
        scroll = scroll.clamp(px(0.), overflow);

        let origin = point(bounds.left() - scroll, bounds.top());
        let selection = (!placeholder)
            .then(|| selection_quad(field, &line, origin, bounds, colors.selection))
            .flatten();
        // The off half of the blink simply has no caret to paint.
        let cursor = field
            .blink_on
            .then(|| cursor_quad(cursor_x + origin.x, bounds, colors.accent))
            .flatten();

        // The ghost hangs off the end of the value, not off the caret, so
        // it stays put while the caret walks back through the text. It is
        // shaped after the scroll is settled and never widens it: the
        // value is what has to stay in view, and a long suggestion must
        // not push it out.
        let ghost = (!placeholder && !field.masked && !field.ghost.is_empty()).then(|| {
            let runs = single_run(&field.ghost, style.font(), colors.text_faint, None);
            (
                window.text_system().shape_line(field.ghost.clone(), font_size, &runs, None),
                line.width(),
            )
        });

        PrepaintState { line, scroll, selection, cursor, ghost }
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut Self::RequestLayoutState,
        prepaint: &mut Self::PrepaintState,
        window: &mut Window,
        cx: &mut App,
    ) {
        let focus_handle = self.field.read(cx).focus_handle.clone();
        window.handle_input(
            &focus_handle,
            ElementInputHandler::new(bounds, self.field.clone()),
            cx,
        );

        if let Some(selection) = prepaint.selection.take() {
            window.paint_quad(selection);
        }

        let line = prepaint.line.clone();
        let origin = point(bounds.left() - prepaint.scroll, bounds.top());
        line.paint(origin, bounds.size.height, gpui::TextAlign::Left, None, window, cx)
            .ok();

        if let Some((ghost, at)) = prepaint.ghost.take() {
            ghost
                .paint(
                    point(origin.x + at, origin.y),
                    bounds.size.height,
                    gpui::TextAlign::Left,
                    None,
                    window,
                    cx,
                )
                .ok();
        }

        if focus_handle.is_focused(window)
            && let Some(cursor) = prepaint.cursor.take()
        {
            window.paint_quad(cursor);
        }

        let scroll = prepaint.scroll;
        self.field.update(cx, |field, _| {
            field.last_layout = Some(line);
            field.last_bounds = Some(bounds);
            field.scroll = scroll;
        });
    }
}

fn single_run(
    text: &str,
    font: gpui::Font,
    color: Hsla,
    underline: Option<UnderlineStyle>,
) -> Vec<TextRun> {
    if text.is_empty() {
        return Vec::new();
    }
    vec![TextRun {
        len: text.len(),
        font,
        color,
        background_color: None,
        underline,
        strikethrough: None,
    }]
}

/// Underline the text the IME is still composing.
fn underline_for(field: &TextField) -> Option<UnderlineStyle> {
    field.marked_range.as_ref().map(|_| UnderlineStyle {
        color: None,
        thickness: px(1.),
        wavy: false,
    })
}

fn selection_quad(
    field: &TextField,
    line: &ShapedLine,
    origin: Point<Pixels>,
    bounds: Bounds<Pixels>,
    color: Hsla,
) -> Option<PaintQuad> {
    if field.selected_range.is_empty() {
        return None;
    }
    let from = field.to_display(field.selected_range.start).min(line.len());
    let to = field.to_display(field.selected_range.end).min(line.len());
    Some(fill(
        Bounds::from_corners(
            point(origin.x + line.x_for_index(from), bounds.top()),
            point(origin.x + line.x_for_index(to), bounds.bottom()),
        ),
        color,
    ))
}

fn cursor_quad(x: Pixels, bounds: Bounds<Pixels>, color: Hsla) -> Option<PaintQuad> {
    Some(fill(
        Bounds::new(point(x, bounds.top()), size(px(1.5), bounds.size.height)),
        color,
    ))
}

/// Pull an offset onto a character boundary inside the value. The platform
/// can hand back a range from one edit ago.
fn clamp_offset(text: &str, offset: usize) -> usize {
    let mut offset = offset.min(text.len());
    while !text.is_char_boundary(offset) {
        offset -= 1;
    }
    offset
}

fn clamp_range(text: &str, range: Range<usize>) -> Range<usize> {
    let start = clamp_offset(text, range.start);
    let end = clamp_offset(text, range.end.max(range.start));
    start..end
}

fn utf16_to_byte(text: &str, utf16_offset: usize) -> usize {
    let mut utf8 = 0;
    let mut utf16 = 0;
    for ch in text.chars() {
        if utf16 >= utf16_offset {
            break;
        }
        utf16 += ch.len_utf16();
        utf8 += ch.len_utf8();
    }
    utf8.min(text.len())
}

/// Caret motion over one line: character and word boundaries. The query
/// editor has its own copy that also knows about line breaks; a field has
/// none, so this one stays short.
mod motion {
    use std::ops::Range;

    /// What counts as part of a word. A URL is one word up to each
    /// punctuation mark, so ⌥← walks host, port and database in turn.
    fn is_word_char(ch: char) -> bool {
        ch.is_alphanumeric() || ch == '_' || ch == '$'
    }

    pub fn previous_boundary(text: &str, offset: usize) -> usize {
        text[..offset].char_indices().next_back().map(|(ix, _)| ix).unwrap_or(0)
    }

    pub fn next_boundary(text: &str, offset: usize) -> usize {
        text[offset..].char_indices().nth(1).map(|(ix, _)| offset + ix).unwrap_or(text.len())
    }

    fn char_before(text: &str, offset: usize) -> Option<char> {
        text[..offset].chars().next_back()
    }

    fn char_at(text: &str, offset: usize) -> Option<char> {
        text[offset..].chars().next()
    }

    /// Start of the word before `offset`, as ⌥← gives on macOS.
    pub fn previous_word_start(text: &str, offset: usize) -> usize {
        let mut ix = offset;
        while let Some(ch) = char_before(text, ix) {
            if is_word_char(ch) {
                break;
            }
            ix = previous_boundary(text, ix);
        }
        while let Some(ch) = char_before(text, ix) {
            if !is_word_char(ch) {
                break;
            }
            ix = previous_boundary(text, ix);
        }
        ix
    }

    /// End of the word after `offset`, as ⌥→ gives on macOS.
    pub fn next_word_end(text: &str, offset: usize) -> usize {
        let mut ix = offset;
        while let Some(ch) = char_at(text, ix) {
            if is_word_char(ch) {
                break;
            }
            ix = next_boundary(text, ix);
        }
        while let Some(ch) = char_at(text, ix) {
            if !is_word_char(ch) {
                break;
            }
            ix = next_boundary(text, ix);
        }
        ix
    }

    /// The word around `offset`, for a double click.
    pub fn word_at(text: &str, offset: usize) -> Range<usize> {
        let inside = char_at(text, offset).is_some_and(is_word_char)
            || char_before(text, offset).is_some_and(is_word_char);
        if !inside {
            return offset..next_boundary(text, offset).min(text.len());
        }
        let mut start = offset;
        while let Some(ch) = char_before(text, start) {
            if !is_word_char(ch) {
                break;
            }
            start = previous_boundary(text, start);
        }
        let mut end = offset;
        while let Some(ch) = char_at(text, end) {
            if !is_word_char(ch) {
                break;
            }
            end = next_boundary(text, end);
        }
        start..end
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const URL: &str = "postgres://ada@db.internal:5432/app";

    #[test]
    fn word_motion_walks_a_url_part_by_part() {
        assert_eq!(&URL[..motion::next_word_end(URL, 0)], "postgres");
        let after_scheme = motion::next_word_end(URL, 0);
        assert_eq!(&URL[..motion::next_word_end(URL, after_scheme)], "postgres://ada");
        // Backwards from the end: the database name, then the port.
        let back = motion::previous_word_start(URL, URL.len());
        assert_eq!(&URL[back..], "app");
        assert_eq!(&URL[motion::previous_word_start(URL, back)..], "5432/app");
    }

    #[test]
    fn a_double_click_takes_the_whole_word() {
        let ix = URL.find("internal").unwrap() + 2;
        assert_eq!(&URL[motion::word_at(URL, ix)], "internal");
    }

    #[test]
    fn motion_never_splits_a_character() {
        let text = "héllo wörld";
        let mut ix = 0;
        while ix < text.len() {
            ix = motion::next_word_end(text, ix);
            assert!(text.is_char_boundary(ix), "split at {ix}");
        }
        while ix > 0 {
            ix = motion::previous_word_start(text, ix);
            assert!(text.is_char_boundary(ix), "split at {ix}");
        }
    }

    #[test]
    fn a_stale_range_is_pulled_back_into_the_value() {
        let text = "postgres://localhost";
        assert_eq!(clamp_range(text, 40..50), 20..20);
        assert_eq!(clamp_range(text, 2..50), 2..20);
        assert_eq!(clamp_range(text, 6..2), 6..6);
    }

    #[test]
    fn clamping_never_splits_a_character() {
        let text = "héllo";
        assert_eq!(clamp_offset(text, 2), 1);
        for offset in 0..=text.len() + 4 {
            assert!(text.is_char_boundary(clamp_offset(text, offset)));
        }
    }

    #[test]
    fn composition_selection_is_measured_inside_the_inserted_text() {
        assert_eq!(utf16_to_byte("héllo", 2), 3);
        assert_eq!(utf16_to_byte("abc", 99), 3);
    }
}
